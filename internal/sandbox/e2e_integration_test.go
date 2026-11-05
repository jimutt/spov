//go:build integration

package sandbox

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/proxy"
)

// TestIntegrationEndToEnd is the whole slice-3 contract against real
// Modal: build the image, render the DL-036 entrypoint, boot it, clone a
// repo, and reach a healthy server of the pinned version through the
// tunnel with the generated password.
func TestIntegrationEndToEnd(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	image, err := p.EnsureImage(ctx, ImageSpec{OpenCodeVersion: testOpenCodeVersion()})
	if err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	password, err := opencode.GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	// Landing is set explicitly; the cwd assertion below is this
	// tree's only end-to-end proof that the override works.
	//
	// @decision(DL-037)
	entrypoint := opencode.Entrypoint{
		Clones:  []opencode.Clone{{URL: "https://github.com/modal-labs/libmodal.git", Dir: "libmodal"}},
		Landing: "libmodal",
		Port:    opencode.DefaultPort,
	}
	command, err := entrypoint.Command()
	if err != nil {
		t.Fatal(err)
	}
	envConfig := opencode.EnvConfig{Password: password}
	env, err := envConfig.Env()
	if err != nil {
		t.Fatal(err)
	}
	// The password travels in Secrets, not Env: passing only Env starts
	// the server with authentication disabled, and every authenticated
	// request is then rejected.
	secrets, err := envConfig.Secrets()
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	h, err := p.Create(ctx, Spec{
		Image:   image,
		Command: command,
		Env:     env,
		Secrets: secrets,
		// No exposed ports: the sandbox is reached only through a
		// connect token and the loopback proxy.
		//
		// @decision(DL-030)
		ReadyPort:   opencode.DefaultPort,
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     10 * time.Minute,
		IdleTimeout: 2 * time.Minute,
		Tags:        BuildTags(TagSet{Name: "e2e", User: "integration-test", Repos: []string{"modal-labs/libmodal"}, CreatedAt: time.Now()}),
		Egress:      EgressOpen(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = h.Terminate(context.WithoutCancel(ctx)) })
	t.Logf("ready after %s", time.Since(start).Round(100*time.Millisecond))

	// The proxy is what every client uses, so the whole rest of this test
	// runs through it: an assertion made against a direct tunnel would
	// not be testing the path spov actually takes.
	//
	// @decision(DL-030)
	if tunnels, err := h.Tunnels(ctx); err != nil {
		t.Fatalf("tunnels: %v", err)
	} else if len(tunnels) != 0 {
		t.Errorf("the sandbox has a public address: %v", tunnels)
	}

	prox, err := proxy.Start(ctx, proxy.Options{
		Mint: func(ctx context.Context) (proxy.Credential, error) {
			conn, err := h.ConnectToken(ctx, opencode.DefaultPort, "")
			if err != nil {
				return proxy.Credential{}, err
			}
			return proxy.Credential{
				URL:      conn.URL,
				Token:    conn.Token,
				User:     opencode.BasicAuthUser,
				Password: password,
			}, nil
		},
		Warn: func(msg string) { t.Log(msg) },
	})
	if err != nil {
		t.Fatalf("start the proxy: %v", err)
	}
	t.Cleanup(func() { _ = prox.Close() })

	url := prox.URL
	// The client is given no password: the proxy supplies it, which is
	// exactly what the TUI relies on.
	client := opencode.NewClient(url, "")

	healthCtx, healthCancel := context.WithTimeout(ctx, 60*time.Second)
	defer healthCancel()

	health, err := client.WaitHealthy(healthCtx, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("server never became healthy: %v", err)
	}
	t.Logf("healthy after %s: version=%s pid=%d", time.Since(start).Round(100*time.Millisecond), health.Version, health.PID)

	t.Run("server runs the pinned version", func(t *testing.T) {
		if err := opencode.CheckVersion(testOpenCodeVersion(), "this test pins", health.Version); err != nil {
			t.Error(err)
		}
	})

	t.Run("the repo was cloned to the landing directory", func(t *testing.T) {
		res, err := h.Exec(ctx, []string{"bash", "-c", "ls /workspace/libmodal/README.md"})
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 0 {
			t.Errorf("clone missing: exit=%d stderr=%q", res.ExitCode, res.Stderr)
		}
	})

	t.Run("the server's cwd is the landing repo", func(t *testing.T) {
		// The TUI sends its local cwd, which does not exist here, so the
		// server's own cwd is what decides the landing directory.
		// procps is not in the image, so scan /proc directly. Match the
		// binary as argv[0] exactly: PID 1 is dumb-init running the whole
		// entrypoint script, so its cmdline also contains "opencode
		// serve" and would otherwise match first.
		res, err := h.Exec(ctx, []string{"bash", "-c", `
for d in /proc/[0-9]*; do
  argv0=$(tr '\0' '\n' < "$d/cmdline" 2>/dev/null | head -1)
  if [ "$argv0" = "/root/.opencode/bin/opencode" ]; then
    readlink -f "$d/cwd"
    exit 0
  fi
done
echo "server process not found" >&2
exit 1`})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(res.Stdout); got != "/workspace/libmodal" {
			t.Errorf("server cwd = %q (exit %d, stderr %q), want /workspace/libmodal", got, res.ExitCode, res.Stderr)
		}
	})

	t.Run("the sandbox is unreachable without the proxy", func(t *testing.T) {
		// The edge rejects a request carrying no connect token, before
		// OpenCode sees it. This is the barrier a public tunnel did not
		// have.
		//
		// @decision(DL-030)
		conn, err := h.ConnectToken(ctx, opencode.DefaultPort, "")
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, conn.URL+"/api/info", nil)
		req.SetBasicAuth(opencode.BasicAuthUser, password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a request with the right password but no token returned %d, want 401", resp.StatusCode)
		}
	})

	t.Run("a wrong password is rejected by the server, not retried", func(t *testing.T) {
		// Aimed past the proxy at the edge, with the token itself,
		// since the proxy always substitutes its own Authorization.
		//
		// @decision(DL-030)
		conn, err := h.ConnectToken(ctx, opencode.DefaultPort, "")
		if err != nil {
			t.Fatal(err)
		}
		wrong := opencode.NewClient(conn.URL, "definitely-not-the-password")
		wrong.HTTP = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &tokenTransport{
				token: conn.Token,
				base:  http.DefaultTransport,
			},
		}
		if _, err := wrong.Health(ctx); err == nil {
			t.Error("a wrong password was accepted")
		} else if !errors.Is(err, opencode.ErrUnauthorized) {
			t.Errorf("err = %v, want ErrUnauthorized", err)
		}
	})
}

// tokenTransport attaches a connect token so a request can reach the
// sandbox without going through spov's proxy.
type tokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.AddCookie(&http.Cookie{Name: proxy.TokenCookie, Value: t.token})
	return t.base.RoundTrip(clone)
}
