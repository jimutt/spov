//go:build integration

package session

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
)

// echoPlatform is a GitHub-kind platform on postman-echo.com, which
// answers with the headers it was sent, so a test can see what the proxy
// added. Its repositories are cloned from github.com without a credential.
// It renews its tokens, as a GitHub App does, so the proxy adds its token
// on any path.
type echoPlatform struct{ token string }

func (echoPlatform) RenewsTokens() bool { return true }

func (echoPlatform) Name() string { return "echo" }
func (echoPlatform) Host() string { return "postman-echo.com" }
func (echoPlatform) Kind() string { return "github" }
func (echoPlatform) ListRepos(context.Context) ([]git.Repo, error) {
	return nil, nil
}
func (p echoPlatform) IssueSandboxToken(context.Context, []git.Repo, time.Duration) (git.Token, error) {
	return git.Token{Username: "spov", Secret: p.token}, nil
}
func (echoPlatform) TokenScope() git.Scope { return git.Scope{Description: "a test token"} }
func (echoPlatform) FindPullRequest(context.Context, git.RepoRef, string) (*git.PullRequest, error) {
	return nil, git.ErrUnsupported
}

// TestIntegrationALaunchThroughTheCredentialProxy runs the real launch
// path with the proxy: the image is baked (or found), the sidecar starts
// inside Create, the entrypoint waits for it, and the main container
// holds a placeholder. It then checks, from inside the main container,
// that a request to the platform's host arrives with the real credential,
// that the real token is nowhere in the main container, and that a
// re-mint through the sidecar takes effect on the next request. A
// provider key set on httpbin.org stands in for a model provider's: it
// arrives in its header, and the main container holds a placeholder.
//
// @decision(DL-034) @decision(DL-021)
func TestIntegrationALaunchThroughTheCredentialProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	provider, err := sandbox.NewModalProvider(ctx, sandbox.ModalConfig{AppName: integrationApp})
	if err != nil {
		t.Fatalf("connect to Modal: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "spov-proxy")
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", bin, "github.com/jimutt/spov/cmd/spov-proxy")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build spov-proxy: %v\n%s", err, out)
	}
	binary, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	bakeStart := time.Now()
	image, err := provider.EnsureProxyImage(ctx, binary)
	if err != nil {
		t.Fatalf("EnsureProxyImage: %v", err)
	}
	t.Logf("RESULT proxy image %s ready in %s", image, time.Since(bakeStart).Round(time.Second))

	const (
		real    = "real-token-one"
		realKey = "sk-ant-api03-real-key-one"
	)
	launcher := &Launcher{
		Sandboxes:  provider,
		Progress:   testWriter{t},
		ProxyImage: func(context.Context) (string, error) { return image, nil },
	}
	launchStart := time.Now()
	sess, err := launcher.Launch(ctx, Options{
		Name:  "spov-credproxy-" + time.Now().UTC().Format("20060102-150405"),
		User:  "integration-test",
		Image: integrationImage(),
		Selections: []git.Selection{{Platform: echoPlatform{token: real}, Repos: []git.Repo{{
			Owner: "octocat", Name: "Hello-World", CloneURL: "https://github.com/octocat/Hello-World.git",
		}}}},
		Port:         opencode.DefaultPort,
		CPU:          0.5,
		MemoryMiB:    2048,
		Timeout:      10 * time.Minute,
		IdleTimeout:  2 * time.Minute,
		Egress:       sandbox.EgressOpen(),
		ProviderKeys: map[string]string{"SPOV_ECHO_API_KEY": realKey},
		ProviderAPIs: map[string]credproxy.ProviderAPI{
			"SPOV_ECHO_API_KEY": {Host: "httpbin.org", Header: credproxy.HeaderAPIKey},
		},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Logf("RESULT launched with the proxy in %s", time.Since(launchStart).Round(time.Second))
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := sess.Close(c); err != nil {
			t.Errorf("terminate: %v", err)
		}
	})

	sh := func(script string) string {
		t.Helper()
		res, err := sess.Handle.Exec(ctx, []string{"bash", "-c", script})
		if err != nil {
			t.Fatal(err)
		}
		return res.Stdout + res.Stderr
	}
	basic := func(tok string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte("spov:"+tok))
	}

	// The proxy scrubs an echoed credential from the response, so what
	// arrived shows as redacted; a placeholder or an old token would not.
	redacted := `"Basic spov-redacted"`
	headers := sh("curl -sS https://postman-echo.com/headers")
	t.Logf("RESULT the echo host saw: %s", strings.TrimSpace(headers))
	if !strings.Contains(headers, redacted) {
		t.Errorf("the request did not arrive with the real credential")
	}
	if n := strings.TrimSpace(sh("grep -rls '" + real + "' /root /etc /workspace /tmp /proc/[0-9]*/environ 2>/dev/null | wc -l")); n != "0" {
		t.Errorf("the real token is in %s places in the main container", n)
	}
	if env := sh("printenv SPOV_GIT_TOKEN_ECHO"); strings.Contains(env, real) || strings.TrimSpace(env) == "" {
		t.Errorf("the main container's token variable is %q", env)
	}
	keyed := sh("curl -sS -H 'x-api-key: from-the-client' https://httpbin.org/headers | tr -d '\\n '")
	t.Logf("RESULT the key's host saw: %s", keyed)
	if !strings.Contains(keyed, `"X-Api-Key":"spov-redacted"`) || strings.Contains(keyed, "from-the-client") {
		t.Errorf("the request did not arrive with the real key alone")
	}
	if n := strings.TrimSpace(sh("grep -rls '" + realKey + "' /root /etc /workspace /tmp /proc/[0-9]*/environ 2>/dev/null | wc -l")); n != "0" {
		t.Errorf("the real key is in %s places in the main container", n)
	}
	if env := sh("printenv SPOV_ECHO_API_KEY"); !strings.HasPrefix(env, "sk-ant-api03-spovplaceholder") {
		t.Errorf("the main container's key variable is %q", env)
	}
	if len(sess.ProxiedKeys) != 1 {
		t.Errorf("proxied keys = %v", sess.ProxiedKeys)
	}
	if out := sh("ls /workspace/Hello-World/README"); strings.Contains(out, "No such file") {
		t.Errorf("the clone did not happen: %s", out)
	}

	// Runtime installs go through the sidecar too, tunnelled for hosts
	// it does not decrypt; a client on the system trust store also
	// trusts the sidecar's CA for the hosts it does.
	//
	// @decision(DL-032)
	for _, c := range []struct{ name, script, want string }{
		{"node from .nvmrc", "mkdir -p /workspace/nv && cd /workspace/nv && echo 22 > .nvmrc && node --version", "v22."},
		{"python from .python-version", "mkdir -p /workspace/pv && cd /workspace/pv && echo 3.13 > .python-version && python --version", "Python 3.13."},
		{"a go toolchain", "cd /tmp && GOTOOLCHAIN=go1.26.4 go version", "go1.26.4"},
		{"uv pip", "cd /tmp && uv venv -q v && . v/bin/activate && uv pip install -q requests && echo installed", "installed"},
		{"python through a decrypted host", `python -c "import urllib.request; print(urllib.request.urlopen(urllib.request.Request('https://postman-echo.com/get', headers={'User-Agent': 'curl/8'})).status)"`, "200"},
	} {
		out := sh(c.script)
		if !strings.Contains(out, c.want) {
			t.Errorf("%s through the sidecar: %s", c.name, out)
			continue
		}
		t.Logf("RESULT %s through the sidecar: %s", c.name, strings.TrimSpace(out[strings.LastIndex(strings.TrimSpace(out), "\n")+1:]))
	}

	res, err := sess.Handle.SidecarExecInput(ctx, credproxy.SidecarName, credproxy.SetTokenCommand("echo"), []byte("real-token-two\n"))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("set-token: %v %+v", err, res)
	}
	if headers := sh("curl -sS https://postman-echo.com/headers"); !strings.Contains(headers, redacted) || strings.Contains(headers, basic(real)) {
		t.Errorf("after the re-mint the echo host saw: %s", headers)
	}
}
