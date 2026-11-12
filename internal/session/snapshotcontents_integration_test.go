//go:build integration

package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// TestIntegrationAnExitSnapshotHoldsNoSecret is spike S-SR2: a launch
// under the proxy with a forge token and a provider key nobody else
// knows, searched for from a root shell in the running main container
// and again in a plain sandbox booted from its exit snapshot. Re-run it
// on a Modal SDK bump or a change to what the launch writes.
//
//	SPOV_SNAPSHOT_SPIKE=1 go test -tags integration ./internal/session/ -run AnExitSnapshotHoldsNoSecret -v -timeout 30m
//
// @decision(DL-084) @decision(DL-034)
func TestIntegrationAnExitSnapshotHoldsNoSecret(t *testing.T) {
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
	secret := func(prefix string) string {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return prefix + hex.EncodeToString(b)
	}
	token, key := secret("spovprobe-forge-"), secret("spovprobe-key-")

	launcher := &Launcher{
		Sandboxes:  provider,
		Progress:   testWriter{t},
		ProxyImage: func(context.Context) (string, error) { return image, nil },
	}
	name := "spov-snapsecrets-" + time.Now().UTC().Format("20060102-150405")
	first, err := launcher.Launch(ctx, Options{
		Name: name, User: "integration-test", Image: integrationImage(),
		Selections: []git.Selection{{Platform: echoPlatform{token: token}, Repos: []git.Repo{{
			Owner: "octocat", Name: "Hello-World", CloneURL: "https://github.com/octocat/Hello-World.git",
		}}}},
		Port: opencode.DefaultPort, CPU: 0.5, MemoryMiB: 2048, Timeout: 10 * time.Minute, IdleTimeout: 3 * time.Minute,
		Egress: sandbox.EgressOpen(), Run: 1, ExitSnapshot: true,
		ProviderKeys: map[string]string{"SPOV_ECHO_API_KEY": key},
		ProviderAPIs: map[string]credproxy.ProviderAPI{"SPOV_ECHO_API_KEY": {Host: "httpbin.org", Header: credproxy.HeaderAPIKey}},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = first.Close(context.WithoutCancel(ctx))
		}
	})

	// search looks for each secret without holding it whole on its own
	// command line: in every file, every process's environment and
	// arguments. It prints one line per secret and the paths that hold
	// a private key.
	search := func(secrets map[string]string) string {
		var b strings.Builder
		for label, s := range secrets {
			half := len(s) / 2
			b.WriteString("a='" + s[:half] + "'; b='" + s[half:] + "'; s=\"$a$b\"; " +
				"printf '" + label + ": files %s, environments %s, arguments %s\\n' " +
				"\"$(grep -rlF --exclude-dir=proc --exclude-dir=sys -f <(printf %s \"$s\") / 2>/dev/null | head -5 | tr '\\n' ' ')\" " +
				"\"$(grep -alF -f <(printf %s \"$s\") /proc/[0-9]*/environ 2>/dev/null | wc -l)\" " +
				"\"$(grep -alF -f <(printf %s \"$s\") /proc/[0-9]*/cmdline 2>/dev/null | wc -l)\"; ")
		}
		b.WriteString("printf 'private keys: %s\\n' \"$(grep -rlI --exclude-dir=proc --exclude-dir=sys --exclude-dir=__modal --exclude-dir=mise --exclude-dir=node-compile-cache --exclude-dir=node_modules -e 'PRIVATE KEY-----' / 2>/dev/null | head -10 | tr '\\n' ' ')\"; " +
			"printf 'proxy files: %s\\n' \"$(ls -d /run/spov-proxy* /usr/local/bin/spov-proxy 2>/dev/null | tr '\\n' ' ')\"")
		return b.String()
	}
	run := func(h sandbox.Handle, script string) string {
		t.Helper()
		c, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		res, err := h.Exec(c, []string{"bash", "-c", script})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		return strings.TrimSpace(res.Stdout + res.Stderr)
	}
	clean := func(where, out string) {
		t.Helper()
		t.Logf("RESULT %s:\n%s", where, out)
		for _, line := range strings.Split(out, "\n") {
			for _, label := range []string{"forge token", "provider key"} {
				if strings.HasPrefix(line, label+":") && line != label+": files , environments 0, arguments 0" {
					t.Errorf("%s: the %s is readable: %s", where, label, line)
				}
			}
		}
	}

	// The control: the request through the proxy carries the real token,
	// so the launch did hold it.
	if got := run(first.Handle, "curl -sS -o /dev/null -w '%{http_code}' https://postman-echo.com/headers"); got != "200" {
		t.Fatalf("the proxied request answered %q", got)
	}
	secrets := map[string]string{"forge token": token, "provider key": key}
	clean("the running main container", run(first.Handle, search(secrets)))
	t.Logf("RESULT the main container's password: %s",
		run(first.Handle, search(map[string]string{"opencode password": first.Password})))

	sandboxID := first.Info.ID
	if err := first.Close(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	closed = true
	img, err := provider.ExitSnapshot(ctx, sandboxID, 5*time.Minute)
	if err != nil {
		t.Fatalf("the exit snapshot: %v", err)
	}
	t.Cleanup(func() {
		if err := provider.DeleteImage(context.WithoutCancel(ctx), img); err != nil {
			t.Logf("delete %s: %v", img, err)
		}
	})

	// A plain sandbox from the snapshot: no proxy, no entrypoint, so
	// what is found is what the snapshot kept.
	plain, err := provider.Create(ctx, sandbox.Spec{
		Image: integrationImage(), ImageID: img,
		Command: []string{"sleep", "infinity"}, Ports: []int{opencode.DefaultPort},
		CPU: 0.5, MemoryMiB: 2048, Timeout: 10 * time.Minute, IdleTimeout: 3 * time.Minute,
		Tags:   sandbox.BuildTags(sandbox.TagSet{User: "integration-test", CreatedAt: time.Now()}),
		Egress: sandbox.EgressOpen(),
	})
	if err != nil {
		t.Fatalf("boot the snapshot: %v", err)
	}
	t.Cleanup(func() { _ = plain.Terminate(context.WithoutCancel(ctx)) })
	clean("the exit snapshot", run(plain, search(secrets)))
	t.Logf("RESULT the snapshot's password: %s", run(plain, search(map[string]string{"opencode password": first.Password})))
	t.Logf("RESULT the snapshot's environment files and git credentials:\n%s",
		run(plain, "ls -la /root/.git-credentials /root/.config/gh/hosts.yml /root/.config/gh/hosts.d 2>&1 | head; "+
			"cat /root/.git-credentials 2>/dev/null | sed -E 's#://[^@]*@#://<redacted>@#' | head -5; ls /tmp | head -20"))
}
