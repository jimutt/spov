//go:build integration

package session

import (
	"context"
	"io"
	"net/http"
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

// TestIntegrationASnapshotResumeThroughTheLauncher is DL-088 on Modal:
// a launch under the proxy with an exit snapshot, a dirty file and a
// conversation, the sandbox terminated, then a second launch booted
// from the snapshot in the entrypoint's snapshot mode. The resumed
// sandbox must keep the file and the conversation, clone nothing, and
// reach a decrypted host through its new sidecar with its new CA.
//
//	SPOV_SNAPSHOT_SPIKE=1 go test -tags integration ./internal/session/ -run SnapshotResumeThroughTheLauncher -v -timeout 30m
//
// @decision(DL-088)
func TestIntegrationASnapshotResumeThroughTheLauncher(t *testing.T) {
	if os.Getenv("SPOV_SNAPSHOT_SPIKE") == "" {
		t.Skip("set SPOV_SNAPSHOT_SPIKE=1 to create billable sandboxes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
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
	image, err := provider.EnsureProxyImage(ctx, binary)
	if err != nil {
		t.Fatalf("EnsureProxyImage: %v", err)
	}
	launcher := &Launcher{
		Sandboxes:  provider,
		Progress:   testWriter{t},
		ProxyImage: func(context.Context) (string, error) { return image, nil },
	}
	name := "spov-snapresume-" + time.Now().UTC().Format("20060102-150405")
	opts := Options{
		Name: name, User: "integration-test", Image: integrationImage(),
		Selections: []git.Selection{{Platform: echoPlatform{token: "real-token"}, Repos: []git.Repo{{
			Owner: "octocat", Name: "Hello-World", CloneURL: "https://github.com/octocat/Hello-World.git",
		}}}},
		Port: opencode.DefaultPort, CPU: 0.5, MemoryMiB: 2048, Timeout: 10 * time.Minute, IdleTimeout: 3 * time.Minute,
		Egress: sandbox.EgressOpen(), Run: 1, ExitSnapshot: true,
		ProviderKeys: map[string]string{"SPOV_ECHO_API_KEY": "sk-ant-api03-echo"},
		ProviderAPIs: map[string]credproxy.ProviderAPI{"SPOV_ECHO_API_KEY": {Host: "httpbin.org", Header: credproxy.HeaderAPIKey}},
	}
	first, err := launcher.Launch(ctx, opts)
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if !first.ExitSnapshot {
		t.Fatal("the first launch was not given an exit snapshot")
	}
	sh := func(s *Session, script string) string {
		t.Helper()
		res, err := s.Handle.Exec(ctx, []string{"bash", "-c", script})
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(res.Stdout + res.Stderr)
	}
	call := func(s *Session, method, path, body string) string {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, s.URL+path, rd)
		req.SetBasicAuth(opencode.BasicAuthUser, s.Password)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.Status + " " + string(out)
	}
	dir := first.Landing
	sh(first, "echo uncommitted > "+dir+"/dirty.txt")
	created := call(first, http.MethodPost, "/api/session", `{"location":{"directory":"`+dir+`"}}`)
	t.Logf("a conversation before the end: %s", truncate(created, 200))
	sandboxID := first.Info.ID
	if err := first.Close(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("terminate the first run: %v", err)
	}
	img, err := provider.ExitSnapshot(ctx, sandboxID, 5*time.Minute)
	if err != nil {
		t.Fatalf("the exit snapshot: %v", err)
	}
	t.Cleanup(func() {
		if err := provider.DeleteImage(context.WithoutCancel(ctx), img); err != nil {
			t.Logf("delete %s: %v", img, err)
		}
	})

	opts.Run, opts.SandboxName, opts.SnapshotImage, opts.Resume = 2, name+"-run2", img, opencode.ResumeSnapshot
	start := time.Now()
	second, err := launcher.Launch(ctx, opts)
	if err != nil {
		t.Fatalf("the resumed launch: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := second.Close(c); err != nil {
			t.Errorf("terminate the second run: %v", err)
		}
	})
	t.Logf("RESULT the resumed launch took %s", time.Since(start).Round(time.Second))
	if got := sh(second, "cat "+second.Landing+"/dirty.txt"); got != "uncommitted" {
		t.Errorf("the uncommitted file came back as %q", got)
	}
	listing := call(second, http.MethodGet, "/api/session", "")
	if !strings.Contains(created, `"id":"ses`) || !strings.Contains(listing, between(created, `"id":"`, `"`)) {
		t.Errorf("the conversation did not come back: %s", truncate(listing, 400))
	}
	if got := sh(second, "curl -sS -o /dev/null -w '%{http_code}' https://postman-echo.com/headers"); got != "200" {
		t.Errorf("a decrypted host through the new sidecar answered %q", got)
	}
	t.Logf("RESULT the bundle holds %s spov CA certificate(s)", sh(second, "grep -c 'BEGIN CERTIFICATE' "+credproxy.MainCAFile))
}

func between(s, from, to string) string {
	_, rest, ok := strings.Cut(s, from)
	if !ok {
		return "\x00"
	}
	got, _, _ := strings.Cut(rest, to)
	return got
}
