//go:build integration

package sandbox

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/toolchain"
)

// TestIntegrationToolchainImage builds the default image, measures how
// long a sandbox from it takes to serve, and checks the toolchain inside:
// every pinned default, and a repository's own version files served by
// mise and Go. Re-run it on a toolchain, mise or image bump.
//
// SPOV_READINESS_BASELINE names another published image to time the same
// way, so a change to the image can be compared with what it replaces.
//
// @decision(DL-032)
func TestIntegrationToolchainImage(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	spec := ImageSpec{OpenCodeVersion: testOpenCodeVersion()}
	start := time.Now()
	image, err := p.EnsureImage(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	t.Logf("RESULT image %s ready in %s", image, time.Since(start).Round(time.Second))

	const runs = 5
	images := []string{image}
	if base := os.Getenv("SPOV_READINESS_BASELINE"); base != "" {
		images = append(images, base)
	}
	for _, img := range images {
		var ready []time.Duration
		for i := range runs {
			d := timeReadiness(ctx, t, p, img, i)
			ready = append(ready, d)
		}
		slices.Sort(ready)
		t.Logf("RESULT %s: create to serving over %d fresh sandboxes: median %s, min %s, max %s",
			img, runs, ready[runs/2].Round(10*time.Millisecond), ready[0].Round(10*time.Millisecond), ready[runs-1].Round(10*time.Millisecond))
	}

	h := bootToolchainSandbox(ctx, t, p, image, "toolchain-check")
	sh := func(script string) (int, string) {
		t.Helper()
		res, err := h.Exec(ctx, []string{"bash", "-c", script})
		if err != nil {
			t.Fatal(err)
		}
		return res.ExitCode, strings.TrimSpace(res.Stdout + res.Stderr)
	}

	t.Run("every default reports its pinned version", func(t *testing.T) {
		for _, tool := range toolchain.Defaults {
			code, out := sh("mise current " + tool.Name)
			if code != 0 || out != tool.Version {
				t.Errorf("%s: mise current says %q (exit %d), want %s", tool.Name, out, code, tool.Version)
			}
		}
		for _, cmd := range []string{"python --version", "node --version", "go version", "bun --version", "uv --version",
			"corepack --version", "pnpm --version", "jq --version", "sqlite3 --version", "fd --version", "gcc --version | head -1",
			"make --version | head -1", "pkg-config --version", "ssh -V", "zip -v | head -2 | tail -1", "rg --version | head -1"} {
			code, out := sh(cmd)
			if code != 0 {
				t.Errorf("%s failed (exit %d): %s", cmd, code, out)
				continue
			}
			t.Logf("RESULT %s: %s", cmd, firstLine(out))
		}
	})

	t.Run("a repository's version files win inside it", func(t *testing.T) {
		cases := []struct{ dir, file, content, cmd, want string }{
			{"nvm", ".nvmrc", "22", "node --version", "v22."},
			{"pyv", ".python-version", "3.13", "python --version", "Python 3.13."},
			{"misetoml", "mise.toml", "[tools]\nnode = \"20\"\n", "node --version", "v20."},
			// go.mod's toolchain line is a minimum: an older one runs on the
			// installed Go. Naming a version fetches it, which is the path
			// a newer minimum takes.
			{"gomod", "go.mod", "module x\n\ngo 1.26.0\n", "go version && GOTOOLCHAIN=go1.26.4 go version", "go1.26.4"},
		}
		for _, c := range cases {
			dir := opencode.WorkspaceRoot + "/" + c.dir
			script := fmt.Sprintf("mkdir -p %s && cd %s && printf '%%b' %q > %s && TIMEFORMAT='took %%Rs' && time %s",
				dir, dir, c.content, c.file, c.cmd)
			code, out := sh(script)
			if code != 0 || !strings.Contains(out, c.want) {
				t.Errorf("%s with %s: exit %d, %s; want %q", c.cmd, c.file, code, out, c.want)
				continue
			}
			t.Logf("RESULT %s from %s: %s", c.cmd, c.file, lastLines(out, 2))
		}
		// Outside any repository the defaults hold.
		if _, out := sh("cd /tmp && node --version"); !strings.Contains(out, "v"+version(toolchain.Defaults, "node")) {
			t.Errorf("outside a repository node is %s", out)
		}
	})

	t.Run("sizes", func(t *testing.T) {
		_, out := sh("du -sh /root/.local/share/mise /usr /root/.opencode 2>/dev/null")
		t.Logf("RESULT sizes:\n%s", out)
	})
}

// timeReadiness boots a sandbox with the real entrypoint and returns how
// long after create its server answered. Create returns once the port's
// TCP probe passes, which is when the server is listening.
func timeReadiness(ctx context.Context, t *testing.T, p *ModalProvider, image string, i int) time.Duration {
	t.Helper()
	start := time.Now()
	h := bootToolchainSandbox(ctx, t, p, image, fmt.Sprintf("readiness-%d", i))
	d := time.Since(start)
	_ = h.Terminate(context.WithoutCancel(ctx))
	return d
}

// bootToolchainSandbox starts a sandbox from image running the real
// entrypoint with no repositories, and waits for its server's port.
func bootToolchainSandbox(ctx context.Context, t *testing.T, p *ModalProvider, image, name string) Handle {
	t.Helper()
	password, err := opencode.GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	command, err := opencode.Entrypoint{Port: opencode.DefaultPort}.Command()
	if err != nil {
		t.Fatal(err)
	}
	envConfig := opencode.EnvConfig{Password: password}
	env, err := envConfig.Env()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := envConfig.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	h, err := p.Create(ctx, Spec{
		Image: image, Command: command, Env: env, Secrets: secrets,
		ReadyPort: opencode.DefaultPort,
		CPU:       0.5, MemoryMiB: 2048,
		Timeout: 20 * time.Minute, IdleTimeout: 5 * time.Minute,
		Tags:   BuildTags(TagSet{Name: name, User: "integration-test", CreatedAt: time.Now()}),
		Egress: EgressOpen(),
	})
	if err != nil {
		t.Fatalf("create from %s: %v", image, err)
	}
	t.Cleanup(func() { _ = h.Terminate(context.WithoutCancel(ctx)) })
	return h
}

func version(tools []toolchain.Tool, name string) string {
	for _, tool := range tools {
		if tool.Name == name {
			return tool.Version
		}
	}
	return ""
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
