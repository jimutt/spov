//go:build integration

package session

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/toolchain"
)

// TestIntegrationAnAddonVariantIsBuiltOnFirstLaunch launches a session
// whose image is a variant carrying every add-on, letting the launch
// build it as a server would, then checks each add-on, its services and
// the version files it honours, and the workspace guide. The base image
// is resolved first so the variant build is measured on cached base
// layers. Re-run it on a toolchain, add-on or image bump.
//
// @decision(DL-033) @decision(DL-016)
func TestIntegrationAnAddonVariantIsBuiltOnFirstLaunch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	provider, err := sandbox.NewModalProvider(ctx, sandbox.ModalConfig{AppName: integrationApp})
	if err != nil {
		t.Fatalf("connect to Modal: %v", err)
	}
	base := sandbox.ImageSpec{OpenCodeVersion: integrationVersion}
	start := time.Now()
	if _, err := provider.EnsureImage(ctx, base); err != nil {
		t.Fatalf("EnsureImage(base): %v", err)
	}
	t.Logf("RESULT base image ready in %s", time.Since(start).Round(time.Second))

	sel, err := toolchain.Parse(toolchain.Names())
	if err != nil {
		t.Fatal(err)
	}
	spec := sandbox.ImageSpec{OpenCodeVersion: integrationVersion, Addons: sel}
	already, err := provider.HasImage(ctx, spec.Name())
	if err != nil {
		t.Fatal(err)
	}
	launcher := &Launcher{
		Sandboxes: provider,
		Progress:  testWriter{t},
		Variants:  &VariantBuilder{Builder: provider},
	}
	start = time.Now()
	sess, err := launcher.Launch(ctx, Options{
		Name:         "spov-variant-" + time.Now().UTC().Format("20060102-150405"),
		User:         "integration-test",
		Image:        spec.Name(),
		Variant:      &spec,
		BuildVariant: true,
		Addons:       sel,
		Selections: []git.Selection{{Platform: publicPlatform{}, Repos: []git.Repo{{
			Owner: "octocat", Name: "Hello-World", CloneURL: "https://github.com/octocat/Hello-World.git",
		}}}},
		Port:        opencode.DefaultPort,
		CPU:         2,
		MemoryMiB:   4096,
		Timeout:     20 * time.Minute,
		IdleTimeout: 5 * time.Minute,
		Egress:      sandbox.EgressOpen(),
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Logf("RESULT first launch of %s (already published: %v) took %s", spec.Name(), already, time.Since(start).Round(time.Second))
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := sess.Close(c); err != nil {
			t.Errorf("terminate: %v", err)
		}
	})

	sh := func(script string) (int, string) {
		t.Helper()
		res, err := sess.Handle.Exec(ctx, []string{"bash", "-c", script})
		if err != nil {
			t.Fatal(err)
		}
		return res.ExitCode, strings.TrimSpace(res.Stdout + res.Stderr)
	}

	t.Run("every add-on is installed", func(t *testing.T) {
		for _, cmd := range []string{
			"rustc --version", "cargo --version", "java -version 2>&1 | head -1", "dotnet --version",
			"ruby --version", "php -r 'echo PHP_VERSION;'", "composer --version 2>/dev/null | head -1",
			"ls -d $PLAYWRIGHT_BROWSERS_PATH/chromium-*", "psql --version", "redis-server --version",
			"printenv DOTNET_NOLOGO PLAYWRIGHT_BROWSERS_PATH",
		} {
			code, out := sh(cmd)
			if code != 0 {
				t.Errorf("%s failed (exit %d): %s", cmd, code, out)
				continue
			}
			t.Logf("RESULT %s: %s", cmd, strings.ReplaceAll(out, "\n", " | "))
		}
	})

	t.Run("the services are running", func(t *testing.T) {
		for _, c := range []struct{ name, cmd string }{
			{"postgres", "psql -tAc 'select 1'"},
			{"redis", "redis-cli ping"},
		} {
			waited := ""
			code, out := 0, ""
			for i := range 30 {
				if code, out = sh(c.cmd); code == 0 {
					waited = fmt.Sprintf("%ds after the exec started polling", i)
					break
				}
				time.Sleep(time.Second)
			}
			if code != 0 {
				t.Errorf("%s never answered: %s", c.name, out)
				continue
			}
			t.Logf("RESULT %s answers %q, %s", c.name, out, waited)
		}
	})

	t.Run("an add-on's version files win inside a repository", func(t *testing.T) {
		for _, c := range []struct{ dir, file, content, cmd, want string }{
			{"rt", "rust-toolchain.toml", "[toolchain]\nchannel = \"1.98.0\"\n", "rustc --version", "1.98.0"},
			{"dn", "global.json", `{"sdk":{"version":"9.0.100","rollForward":"latestFeature"}}`, "dotnet --version", "9.0."},
			{"jv", ".java-version", "17", "java -version 2>&1", "Temurin-17."},
		} {
			dir := opencode.WorkspaceRoot + "/" + c.dir
			code, out := sh(fmt.Sprintf("mkdir -p %s && cd %s && printf '%%b' %q > %s && TIMEFORMAT='took %%Rs' && time %s",
				dir, dir, c.content, c.file, c.cmd))
			if code != 0 || !strings.Contains(out, c.want) {
				t.Errorf("%s with %s: exit %d, %s; want %q", c.cmd, c.file, code, out, c.want)
				continue
			}
			lines := strings.Split(out, "\n")
			t.Logf("RESULT %s from %s: %s", c.cmd, c.file, strings.Join(lines[max(0, len(lines)-2):], " | "))
		}
	})

	t.Run("the workspace guide names the add-ons", func(t *testing.T) {
		_, out := sh("cat " + opencode.AgentsFile)
		for _, want := range append([]string{"## Toolchain", "Docker is not available"}, "Rust 1.99.0", "PostgreSQL 17", "Redis 8.0", ".NET SDK") {
			if !strings.Contains(out, want) {
				t.Errorf("AGENTS.md does not say %q:\n%s", want, out)
			}
		}
	})

	t.Run("sizes", func(t *testing.T) {
		_, out := sh("du -sh /root/.local/share/mise /root/.rustup /usr /ms-playwright 2>/dev/null")
		t.Logf("RESULT sizes:\n%s", out)
	})
}
