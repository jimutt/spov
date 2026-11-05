//go:build integration

package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// TestIntegrationScenarioForgejo runs a real Forgejo in a Modal sandbox,
// behind Modal's TLS tunnel, so scenario 4's sandboxes can clone from it
// and push to it as they would to Codefloe (DL-041). It writes an env file
// as scripts/forge-test-server.sh does, with the tunnel's address, then
// keeps the forge up until the file named by SPOV_SCENARIO_FORGEJO_STOP
// appears or its time runs out, and terminates it. This is the scenario's
// tool, not a test of spov. Opt in with the directory for the env file:
//
//	SPOV_SCENARIO_FORGEJO=<dir> MODAL_ENVIRONMENT=spov-scenarios \
//	  go test -tags integration ./internal/sandbox/ -run ScenarioForgejo -v -timeout 3h
//
// @decision(DL-041)
func TestIntegrationScenarioForgejo(t *testing.T) {
	dir := os.Getenv("SPOV_SCENARIO_FORGEJO")
	if dir == "" {
		t.Skip("set SPOV_SCENARIO_FORGEJO to a directory to run a Forgejo for scenario 4")
	}
	env := os.Getenv("MODAL_ENVIRONMENT")
	if env == "" {
		t.Fatal("set MODAL_ENVIRONMENT to the scenario's environment, so nothing is touched elsewhere")
	}
	const version = "17.0.1" // Codefloe's, as in scripts/forge-test-server.sh
	life := 2 * time.Hour
	ctx := context.Background()
	p, err := NewModalProvider(ctx, ModalConfig{AppName: "spov", Environment: env})
	if err != nil {
		t.Fatalf("connect to Modal: %v", err)
	}
	image := p.client.Images.FromRegistry("codeberg.org/forgejo/forgejo:"+version, nil)
	// The root URL is the tunnel's, known only once the sandbox exists:
	// the command waits for it, then starts the image's own entrypoint.
	sb, err := p.client.Sandboxes.Create(ctx, p.app, image, &modal.SandboxCreateParams{
		Command: []string{"sh", "-c", `until [ -s /tmp/root_url ]; do sleep 1; done; ` +
			`export FORGEJO__server__ROOT_URL="$(cat /tmp/root_url)"; exec /usr/bin/entrypoint /usr/bin/s6-svscan /etc/s6`},
		Env: map[string]string{
			"FORGEJO__security__INSTALL_LOCK":        "true",
			"FORGEJO__database__DB_TYPE":             "sqlite3",
			"FORGEJO__server__DISABLE_SSH":           "true",
			"FORGEJO__service__DISABLE_REGISTRATION": "true",
		},
		EncryptedPorts: []int{3000},
		CPU:            1, MemoryMiB: 1024,
		Timeout: life,
		Tags:    map[string]string{"spov-scenario": "forgejo"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		if _, err := sb.Terminate(context.WithoutCancel(ctx), nil); err != nil {
			t.Logf("terminate %s: %v", sb.SandboxID, err)
		}
	})
	tunnels, err := sb.Tunnels(ctx, tunnelTimeout, nil)
	if err != nil {
		t.Fatalf("tunnels: %v", err)
	}
	url := strings.TrimSuffix(tunnels[3000].URL(), "/")
	if _, err := run(ctx, sb, "sh", "-c", `printf %s "$0/" > /tmp/root_url`, url); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		resp, err := http.Get(url + "/api/v1/version")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Forgejo did not answer at %s: %v", url, err)
		}
		time.Sleep(3 * time.Second)
	}
	pass := fmt.Sprintf("Pw-%d", time.Now().UnixNano())
	out, err := run(ctx, sb, "su-exec", "git", "forgejo", "admin", "user", "create", "--username", "scn-admin",
		"--password", pass, "--email", "scn-admin@example.test", "--admin", "--must-change-password=false", "--access-token")
	token := regexp.MustCompile(`[0-9a-f]{40}`).FindString(out)
	if err != nil || token == "" {
		t.Fatalf("create the admin user: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("FORGE_KIND=forgejo\nFORGE_URL=%s\nFORGE_ADMIN=scn-admin\nFORGE_ADMIN_PASSWORD=%s\nFORGE_ADMIN_TOKEN=%s\nFORGE_SANDBOX=%s\n",
		url, pass, token, sb.SandboxID)
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Forgejo %s at %s in %s, env in %s", version, url, sb.SandboxID, filepath.Join(dir, "env"))

	stop := os.Getenv("SPOV_SCENARIO_FORGEJO_STOP")
	if stop == "" {
		stop = filepath.Join(dir, "stop")
	}
	until := time.Now().Add(life - 5*time.Minute)
	for time.Now().Before(until) {
		if _, err := os.Stat(stop); err == nil {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// run execs a command in sb and returns its stdout, failing on a nonzero
// exit.
func run(ctx context.Context, sb *modal.Sandbox, command ...string) (string, error) {
	proc, err := sb.Exec(ctx, command, nil)
	if err != nil {
		return "", err
	}
	_ = proc.Stdin.Close()
	code, err := proc.Wait(ctx, nil)
	if err != nil {
		return "", err
	}
	stdout, _ := io.ReadAll(proc.Stdout)
	stderr, _ := io.ReadAll(proc.Stderr)
	if code != 0 {
		return string(stdout), fmt.Errorf("%s: exit %d: %s", command[0], code, strings.TrimSpace(string(stderr)))
	}
	return string(stdout), nil
}
