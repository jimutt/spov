//go:build integration

package sandbox

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/toolchain"
)

// TestIntegrationAddonVersions builds variants that take the versioned
// add-ons: postgres, php and redis from their upstream repositories at
// their defaults and at other versions, and pinned default languages. Each is
// built on the cached base layers, timed over three fresh sandboxes, and
// booted with the real entrypoint, which starts its services. Set
// SPOV_ADDON_VERSIONS=1; it builds six images.
//
// @decision(DL-033)
func TestIntegrationAddonVersions(t *testing.T) {
	if os.Getenv("SPOV_ADDON_VERSIONS") != "1" {
		t.Skip("set SPOV_ADDON_VERSIONS=1: it builds six variant images")
	}
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	base := ImageSpec{OpenCodeVersion: testOpenCodeVersion()}
	start := time.Now()
	baseName, err := p.EnsureImage(ctx, base)
	if err != nil {
		t.Fatalf("EnsureImage(base): %v", err)
	}
	t.Logf("RESULT base %s ready in %s", baseName, time.Since(start).Round(time.Second))

	type check struct{ cmd, want string }
	waitPostgres := check{"for i in $(seq 40); do pg_isready -q && break; sleep 0.5; done; pg_isready", "accepting connections"}
	waitRedis := check{"for i in $(seq 40); do redis-cli ping 2>/dev/null | grep -q PONG && break; sleep 0.5; done; redis-cli ping", "PONG"}
	variants := []struct {
		addons []string
		checks []check
	}{
		{[]string{"postgres", "php", "redis"}, []check{
			waitPostgres, {"psql -tAc 'show server_version'", "17."},
			{"php --version | head -1", "PHP 8.4."}, {"composer --version 2>&1", "Composer version 2.10.3"},
			waitRedis, {"redis-cli info server | grep redis_version", "redis_version:8.0."},
		}},
		{[]string{"postgres@14"}, []check{
			waitPostgres, {"psql -tAc 'show server_version'", "14."}, {"psql -tAc 'show listen_addresses'", "localhost"},
		}},
		{[]string{"php@8.1"}, []check{
			{"php --version | head -1", "PHP 8.1."},
			{"php -m | grep -cE '^(curl|mbstring|xml|zip|pdo_sqlite|pdo_pgsql)$'", "6"},
			{"composer --version 2>&1", "Composer version 2.10.3"},
		}},
		{[]string{"php@7.1"}, []check{
			{"php --version | head -1", "PHP 7.1."}, {"composer --version 2>&1", "Composer version 2.2.30"},
		}},
		{[]string{"redis@7.2"}, []check{
			waitRedis, {"redis-cli info server | grep redis_version", "redis_version:7.2."},
			{"redis-cli config get bind | tail -1", "127.0.0.1"},
		}},
		{[]string{"node@22", "python@3.12", "go@1.24"}, []check{
			{"node --version", "v22."}, {"python --version", "Python 3.12."}, {"go version", "go1.24."},
			{"corepack --version", toolchain.CorepackVersion},
		}},
	}

	for _, v := range variants {
		label := strings.Join(v.addons, " ")
		t.Run(label, func(t *testing.T) {
			sel, err := toolchain.Parse(v.addons)
			if err != nil {
				t.Fatal(err)
			}
			spec := ImageSpec{OpenCodeVersion: base.OpenCodeVersion, Addons: sel}
			start := time.Now()
			name, err := p.EnsureImage(ctx, spec)
			if err != nil {
				t.Fatalf("EnsureImage: %v", err)
			}
			t.Logf("RESULT %s: %s ready in %s", label, name, time.Since(start).Round(time.Second))

			var ready []time.Duration
			for i := range 3 {
				ready = append(ready, timeReadiness(ctx, t, p, name, i))
			}
			slices.Sort(ready)
			t.Logf("RESULT %s: create to serving over 3 sandboxes: median %s (min %s, max %s)", label,
				ready[1].Round(10*time.Millisecond), ready[0].Round(10*time.Millisecond), ready[2].Round(10*time.Millisecond))

			h := bootWithAddons(ctx, t, p, name, sel)
			for _, c := range v.checks {
				cmd, want := c.cmd, c.want
				res, err := h.Exec(ctx, []string{"bash", "-c", cmd})
				if err != nil {
					t.Fatal(err)
				}
				out := strings.TrimSpace(res.Stdout + res.Stderr)
				if res.ExitCode != 0 || !strings.Contains(out, want) {
					t.Errorf("%s: exit %d, %q; want it to contain %q", cmd, res.ExitCode, out, want)
					continue
				}
				t.Logf("RESULT %s: %s → %s", label, cmd, firstLine(out))
			}
		})
	}

	var ready []time.Duration
	for i := range 3 {
		ready = append(ready, timeReadiness(ctx, t, p, baseName, i))
	}
	slices.Sort(ready)
	t.Logf("RESULT base: create to serving over 3 sandboxes: median %s (min %s, max %s)",
		ready[1].Round(10*time.Millisecond), ready[0].Round(10*time.Millisecond), ready[2].Round(10*time.Millisecond))
}

// bootWithAddons starts a sandbox from image with the real entrypoint
// and the add-ons' services and environment, and waits for its server.
func bootWithAddons(ctx context.Context, t *testing.T, p *ModalProvider, image string, sel []toolchain.Selection) Handle {
	t.Helper()
	password, err := opencode.GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	command, err := opencode.Entrypoint{Port: opencode.DefaultPort, Addons: sel}.Command()
	if err != nil {
		t.Fatal(err)
	}
	envConfig := opencode.EnvConfig{Password: password, Addons: sel}
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
		CPU:       1, MemoryMiB: 2048,
		Timeout: 20 * time.Minute, IdleTimeout: 5 * time.Minute,
		Tags:   BuildTags(TagSet{Name: "addon-versions", User: "integration-test", CreatedAt: time.Now()}),
		Egress: EgressOpen(),
	})
	if err != nil {
		t.Fatalf("create from %s: %v", image, err)
	}
	t.Cleanup(func() { _ = h.Terminate(context.WithoutCancel(ctx)) })
	return h
}
