//go:build integration

package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"

	"github.com/jimutt/spov/internal/credproxy"
)

// These are spike S-I3, the measurements DL-034 list before
// the credential proxy ships. Each creates billable sandboxes and
// terminates what it creates.

// buildProxy cross-compiles cmd/spov-proxy for the sidecar.
func buildProxy(t *testing.T) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "spov-proxy")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "github.com/jimutt/spov/cmd/spov-proxy")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build spov-proxy: %v\n%s", err, b)
	}
	bin, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

// runIn runs a script in a sidecar, with stdin if given.
func runIn(ctx context.Context, t *testing.T, c *modal.SidecarContainer, script string, stdin []byte) (int, string, string) {
	t.Helper()
	proc, err := c.Exec(ctx, []string{"bash", "-c", script}, nil)
	if err != nil {
		t.Fatalf("sidecar exec: %v", err)
	}
	if stdin != nil {
		if _, err := proc.Stdin.Write(stdin); err != nil {
			t.Fatalf("sidecar stdin: %v", err)
		}
	}
	_ = proc.Stdin.Close()
	code, err := proc.Wait(ctx, nil)
	if err != nil {
		t.Fatalf("sidecar wait: %v", err)
	}
	stdout, _ := io.ReadAll(proc.Stdout)
	stderr, _ := io.ReadAll(proc.Stderr)
	return code, string(stdout), string(stderr)
}

// TestIntegrationCredentialProxyThroughASidecar runs the real spov-proxy
// in a sidecar, with a sandbox CA, and measures from the main container:
// how long the sidecar takes to come up; whether curl, git and gh use the
// proxy and trust the CA for a decrypted host; whether a header the
// client sent is replaced; and whether OpenCode's own model call goes
// through the proxy and trusts the CA, with a placeholder key.
//
// github.com is decrypted for a stored token whose only repository is
// not the one fetched, so git and gh go out anonymously through the
// decrypted path. postman-echo.com is a scoped platform, which gets its
// token on any path, and echoes it back scrubbed.
func TestIntegrationCredentialProxyThroughASidecar(t *testing.T) {
	bin := buildProxy(t)
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	cfg := credproxy.SidecarConfig{
		Platforms: []credproxy.Platform{
			{Name: "github", Kind: credproxy.KindGitHub, Host: "github.com", User: "x-access-token",
				Token: "ghp_spike-not-a-token", Repos: []string{"octocat/Hello-World"}},
			{Name: "echo", Kind: credproxy.KindGitHub, Host: "postman-echo.com", User: "spov",
				Token: "swapped-by-the-proxy", Scoped: true},
		},
		Providers: []credproxy.Provider{{
			Env:         "ANTHROPIC_API_KEY",
			ProviderAPI: credproxy.ProviderAPI{Host: "api.anthropic.com", Header: credproxy.HeaderAPIKey},
			Key:         "sk-ant-spike-not-a-key",
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(cfg)
	caCert, caKey, err := credproxy.NewCA(cfg.CAHosts(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	const caPath = "/usr/local/share/spov/ca.crt"
	sb, creds, password := startOpenCodeSandboxEnv(t, ctx, p, "spov-credproxy", 8*time.Minute,
		map[string]string{caPath: string(caCert)},
		map[string]string{
			"HTTPS_PROXY": "http://proxy:3128", "HTTP_PROXY": "http://proxy:3128",
			"https_proxy": "http://proxy:3128", "http_proxy": "http://proxy:3128",
			"NO_PROXY": "localhost,127.0.0.1,::1,main", "no_proxy": "localhost,127.0.0.1,::1,main",
			"NODE_EXTRA_CA_CERTS": caPath,
			"ANTHROPIC_API_KEY":   "sk-ant-api03-spovplaceholder",
		})

	image, err := p.image(ctx, testImage())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	proxy, err := sb.ExperimentalSidecars.Create(ctx, "proxy", image, &modal.SidecarCreateParams{
		Command: []string{"sleep", "infinity"},
		Env: map[string]string{
			credproxy.EnvCACert: string(caCert), credproxy.EnvCAKey: string(caKey), credproxy.EnvConfig: string(config),
		},
	})
	if err != nil {
		t.Fatalf("sidecar create: %v", err)
	}
	created := time.Since(start)
	if code, out, errOut := runIn(ctx, t, proxy, "cat > /usr/local/bin/spov-proxy && chmod 755 /usr/local/bin/spov-proxy", bin); code != 0 {
		t.Fatalf("upload: exit %d %s%s", code, out, errOut)
	}
	uploaded := time.Since(start)
	if code, out, errOut := runIn(ctx, t, proxy, "setsid /usr/local/bin/spov-proxy >/tmp/proxy.log 2>&1 < /dev/null &", nil); code != 0 {
		t.Fatalf("start: exit %d %s%s", code, out, errOut)
	}
	inMain := func(script string) (int, string, string) {
		t.Helper()
		return execIn(ctx, t, sb, "bash", "-c", script)
	}
	if code, _, errOut := inMain("for i in $(seq 100); do (exec 3<>/dev/tcp/proxy/3128) 2>/dev/null && exit 0; sleep 0.1; done; exit 1"); code != 0 {
		t.Fatalf("the proxy never answered: %s", errOut)
	}
	t.Logf("RESULT sidecar: created in %s, binary uploaded by %s, answering by %s",
		created.Round(time.Millisecond), uploaded.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	// The main container's own tools read the system bundle.
	if code, _, errOut := inMain("cat " + caPath + " >> /etc/ssl/certs/ca-certificates.crt"); code != 0 {
		t.Fatalf("trust the CA: %s", errOut)
	}
	// With main's own egress closed, anything that works below went
	// through the proxy.
	if err := sb.UpdateNetworkPolicy(ctx, &modal.SandboxUpdateNetworkPolicyParams{
		OutboundCIDRAllowlist: &modal.Allowlist{}, OutboundDomainAllowlist: &modal.Allowlist{},
	}); err != nil {
		t.Fatalf("close main's egress: %v", err)
	}
	time.Sleep(3 * time.Second)
	_, envs, _ := inMain("env | grep -ci '_proxy='")
	t.Logf("RESULT proxy variables an exec inherits: %s", strings.TrimSpace(envs))

	for _, c := range []struct{ name, script, want string }{
		{"curl, decrypted", "curl -sS -v https://api.github.com/zen 2>&1 | grep -E 'issuer:|^HTTP/' | head -3", "spov sandbox proxy CA"},
		{"curl, tunneled", "curl -sS -o /dev/null -w '%{http_code}' https://example.com", "200"},
		{"git, decrypted", "git ls-remote https://github.com/git/git HEAD | cut -c1-12", ""},
		{"gh, decrypted", "GH_TOKEN=ghs_placeholder gh api /zen", ""},
		// The proxy's credential arrives, and its echo is scrubbed.
		{"header replaced", "curl -sS -H 'Authorization: Bearer placeholder' https://postman-echo.com/headers", "Basic spov-redacted"},
	} {
		code, out, errOut := inMain(c.script)
		t.Logf("RESULT %-17s exit %d %s %s", c.name+":", code, strings.Join(strings.Fields(truncate(out, 300)), " "), strings.TrimSpace(truncate(errOut, 300)))
		if code != 0 || (c.want != "" && !strings.Contains(out, c.want)) {
			t.Errorf("%s did not work through the proxy", c.name)
		}
		if strings.Contains(out, "Bearer placeholder") {
			t.Errorf("%s: the client's own header reached the upstream", c.name)
		}
	}

	// OpenCode's model call, with a placeholder key: the proxy decrypts
	// api.anthropic.com and sets a key that is not valid either, so a
	// 401 in the audit log shows the route and the trust, and no key is
	// needed.
	client := &http.Client{Timeout: 60 * time.Second}
	keyed := createSession(ctx, t, client, creds.URL, creds.Token, password)
	post(ctx, t, client, creds.URL+"/api/session/"+keyed+"/model", creds.Token, password,
		`{"model":{"id":"claude-haiku-4-5-20251001","providerID":"anthropic"}}`)
	prompt(ctx, t, client, creds.URL, creds.Token, password, keyed, "Say hi.")
	var log string
	for range 30 {
		_, log, _ = runIn(ctx, t, proxy, "cat /tmp/proxy.log", nil)
		if strings.Contains(log, "api.anthropic.com") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(log, "\n") {
		var e credproxy.Entry
		if json.Unmarshal([]byte(line), &e) == nil && (e.Host == "api.anthropic.com") {
			seen[e.Host] = true
			t.Logf("RESULT OpenCode through the proxy: %s %s%s -> %d", e.Method, e.Host, e.Path, e.Status)
		} else if strings.HasPrefix(line, "spov-proxy:") {
			t.Logf("RESULT proxy said: %s", line)
		}
	}
	if !seen["api.anthropic.com"] {
		t.Error("OpenCode's model call never reached the proxy decrypted: it ignored HTTPS_PROXY, or did not trust the CA")
	}
}

// TestIntegrationSidecarAndIdleTimeout measures whether a sidecar keeps a
// sandbox from idling out: a running sidecar alone, an exec into it every
// 60 s, and a Poll of it every 60 s. Sandboxes with IdleTimeout 2m are
// watched for 4½ minutes, as in S-R1.
func TestIntegrationSidecarAndIdleTimeout(t *testing.T) {
	const (
		idle    = 2 * time.Minute
		observe = idle*2 + 30*time.Second
	)
	p := newIntegrationProvider(t)
	for _, mode := range []string{"sidecar only", "sidecar exec every 60s", "sidecar poll every 60s"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), observe+6*time.Minute)
			defer cancel()
			sb, _, _ := startOpenCodeSandbox(t, ctx, p, "spov-sidecaridle", idle)
			image, err := p.image(ctx, testImage())
			if err != nil {
				t.Fatal(err)
			}
			side, err := sb.ExperimentalSidecars.Create(ctx, "proxy", image, &modal.SidecarCreateParams{Command: []string{"sleep", "infinity"}})
			if err != nil {
				t.Fatalf("sidecar create: %v", err)
			}
			trafficCtx, stop := context.WithCancel(ctx)
			defer stop()
			if mode != "sidecar only" {
				go func() {
					tick := time.NewTicker(60 * time.Second)
					defer tick.Stop()
					for {
						select {
						case <-trafficCtx.Done():
							return
						case <-tick.C:
							if mode == "sidecar exec every 60s" {
								if proc, err := side.Exec(trafficCtx, []string{"true"}, nil); err == nil {
									_, _ = proc.Wait(trafficCtx, nil)
								}
							} else {
								_, _ = side.Poll(trafficCtx, nil)
							}
						}
					}
				}()
			}
			begin := time.Now()
			died := time.Duration(0)
			for time.Since(begin) < observe {
				time.Sleep(15 * time.Second)
				code, err := sb.Poll(ctx, nil)
				if err != nil {
					t.Fatalf("poll: %v", err)
				}
				if code != nil {
					died = time.Since(begin).Round(time.Second)
					break
				}
			}
			stop()
			if died != 0 {
				t.Logf("RESULT %-24s died after %s: this does NOT keep the sandbox alive", mode, died)
			} else {
				t.Logf("RESULT %-24s alive after %s: this DOES keep the sandbox alive", mode, observe)
			}
		})
	}
}
