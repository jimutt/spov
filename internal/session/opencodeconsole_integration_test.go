//go:build integration

package session

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/console"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/sandbox"
)

// TestIntegrationOpenCodeConsoleThroughTheProxy is the OpenCode Console
// spike: a real sign-in, approved by a person at the URL it logs, then a
// real launch whose sidecar holds the token while the main container
// holds a placeholder. It checks that OpenCode loads the console config
// through the proxy and answers on a Go model, that a console path off
// the list gets no credential, and that a refreshed token written in with
// set-key is used by the next call. The grant is revoked at the end.
//
// Run it with SPOV_CONSOLE_SPIKE=1 and -v, and watch for the URL. It uses
// a little of the account's Go allowance. With SPOV_CONSOLE_KEY_FILE
// naming a file that holds a console API key, it launches with that key
// instead of a sign-in, and skips the refresh.
//
// @decision(DL-015)
func TestIntegrationOpenCodeConsoleThroughTheProxy(t *testing.T) {
	if os.Getenv("SPOV_CONSOLE_SPIKE") != "1" {
		t.Skip("set SPOV_CONSOLE_SPIKE=1: it needs a person to approve a sign-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	c := &console.Client{UserAgent: "spov/integration-test"}
	var grant console.Grant
	var replaced string
	keyed := os.Getenv("SPOV_CONSOLE_KEY_FILE") != ""
	if keyed {
		b, err := os.ReadFile(os.Getenv("SPOV_CONSOLE_KEY_FILE"))
		if err != nil {
			t.Fatal(err)
		}
		grant.Access = strings.TrimSpace(string(b))
		t.Log("RESULT launching with a pasted console API key")
	} else {
		grant = consoleSignIn(ctx, t, c)
		// Revoked at the end by sending a replaced refresh token again.
		t.Cleanup(func() {
			if replaced == "" {
				if g, err := c.Refresh(context.Background(), grant.Refresh, time.Now()); err == nil {
					replaced, grant = grant.Refresh, g
				}
			}
			if _, err := c.Refresh(context.Background(), replaced, time.Now()); err == nil {
				t.Error("a replaced refresh token was accepted; the grant may still be live")
			}
			t.Log("RESULT the spike's grant was revoked")
		})
	}

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
	image, err := provider.EnsureProxyImage(ctx, binary)
	if err != nil {
		t.Fatalf("EnsureProxyImage: %v", err)
	}
	launcher := &Launcher{
		Sandboxes:  provider,
		Progress:   testWriter{t},
		ProxyImage: func(context.Context) (string, error) { return image, nil },
	}
	start := time.Now()
	sess, err := launcher.Launch(ctx, Options{
		Name:  "spov-console-" + time.Now().UTC().Format("20060102-150405"),
		User:  "integration-test",
		Image: integrationImage(),
		Selections: []git.Selection{{Platform: publicPlatform{}, Repos: []git.Repo{{
			Owner: "octocat", Name: "Hello-World", CloneURL: "https://github.com/octocat/Hello-World.git",
		}}}},
		Port:         opencode.DefaultPort,
		CPU:          1,
		MemoryMiB:    2048,
		Timeout:      15 * time.Minute,
		IdleTimeout:  5 * time.Minute,
		Egress:       sandbox.EgressOpen(),
		ProviderKeys: map[string]string{prep.OpenCodeKeyEnv: grant.Access},
		ProviderAPIs: prep.ProviderAPIs(),
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Logf("RESULT launched in %s", time.Since(start).Round(time.Second))
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
		return strings.TrimSpace(res.Stdout + res.Stderr)
	}
	// The token goes in on stdin, so the search itself does not put it in
	// a process list.
	found, err := sess.Handle.ExecInput(ctx, []string{"bash", "-c",
		"grep -rlsF -f - /root /etc /workspace /tmp /proc/[0-9]*/environ 2>/dev/null | grep -v '^/proc/self/' | wc -l"}, []byte(grant.Access+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.TrimSpace(found.Stdout); n != "0" {
		t.Errorf("the real token is in %s places in the main container", n)
	}
	t.Logf("RESULT the main container holds OPENCODE_API_KEY=%s…", truncate(sh("printenv "+prep.OpenCodeKeyEnv), 18))

	// OpenCode loaded the console config through the proxy if its Go
	// models point at the /inference gateway.
	api := "curl -sS -u opencode:\"$OPENCODE_PASSWORD\" http://127.0.0.1:" + "4096"
	var bases string
	for i := 0; i < 20; i++ {
		bases = goBaseURLs(t, sh(api+"/api/model"))
		if strings.Contains(bases, "/inference/") {
			break
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("RESULT opencode-go base URLs: %s", bases)
	if !strings.Contains(bases, "https://opencode.ai/inference/go/") {
		t.Errorf("OpenCode did not load the console config through the proxy")
	}

	// Off the list, the proxy adds nothing; on it, the token.
	for _, c := range []struct{ path, want string }{
		{"/console/api/user", "401"},
		{"/console/api/v2/config", "200"},
	} {
		got := sh("curl -s -o /dev/null -w '%{http_code}' -H \"Authorization: Bearer $" + prep.OpenCodeKeyEnv + "\" https://opencode.ai" + c.path)
		t.Logf("RESULT %s from the main container answered %s", c.path, got)
		if got != c.want {
			t.Errorf("%s answered %s, want %s", c.path, got, c.want)
		}
	}

	ask := func(label string) {
		t.Helper()
		start := time.Now()
		out := sh("cd /workspace && OPENCODE_SERVER_PASSWORD=\"$OPENCODE_PASSWORD\" timeout 180 /root/.opencode/bin/opencode run " +
			"--server http://127.0.0.1:4096 -m opencode-go/glm-5.3-flash 'What is 6 times 7? Reply with the number only, and use no tools.' 2>&1 | tail -5")
		t.Logf("RESULT %s: %q in %s", label, truncate(out, 300), time.Since(start).Round(time.Second))
		if !strings.Contains(out, "42") {
			t.Errorf("%s: no answer from the Go model", label)
		}
	}
	ask("first prompt on opencode-go/glm-5.3-flash")
	if keyed {
		return
	}

	// A refresh ends the token the sidecar holds; set-key gives it the
	// new one, and the next call works.
	next, err := c.Refresh(ctx, grant.Refresh, time.Now())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	replaced, grant = grant.Refresh, next
	res, err := sess.Handle.SidecarExecInput(ctx, credproxy.SidecarName, credproxy.SetKeyCommand(prep.OpenCodeKeyEnv), []byte(next.Access+"\n"))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("set-key: %v %+v", err, res)
	}
	ask("after the refresh and set-key")
}

// goBaseURLs lists the distinct base URLs of OpenCode's opencode-go
// models in an /api/model answer.
func goBaseURLs(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Data []struct {
			ProviderID string `json:"providerID"`
			Settings   struct {
				BaseURL string `json:"baseURL"`
			} `json:"settings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return "unreadable: " + truncate(body, 120)
	}
	var urls []string
	for _, m := range out.Data {
		if m.ProviderID == "opencode-go" && !slices.Contains(urls, m.Settings.BaseURL) {
			urls = append(urls, m.Settings.BaseURL)
		}
	}
	return strings.Join(urls, " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// consoleSignIn runs a real device sign-in, approved by a person at the
// URL it logs.
func consoleSignIn(ctx context.Context, t *testing.T, c *console.Client) (grant console.Grant) {
	t.Helper()
	d, err := c.Start(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("APPROVE: %s (code %s)", d.VerificationURL, d.UserCode)
	for {
		if time.Now().After(d.ExpiresAt) {
			t.Fatal("the sign-in was not approved in time")
		}
		time.Sleep(d.Interval)
		res, err := c.Poll(ctx, d.Code, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if res.State == console.PollFailed {
			t.Fatalf("the sign-in failed: %s", res.Reason)
		}
		if res.State == console.PollApproved {
			grant = res.Grant
			break
		}
	}
	t.Logf("RESULT signed in; the access token lasts until %s", grant.ExpiresAt.Format(time.RFC3339))
	return grant
}
