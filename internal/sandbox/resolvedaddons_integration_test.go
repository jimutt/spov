//go:build integration

package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/addonindex"
	"github.com/jimutt/spov/internal/toolchain"
)

// TestIntegrationResolvedAddons resolves partial add-on versions against
// the real indexes, builds the exact variant and checks that the sandbox
// runs exactly those releases; then checks that a resolved version which
// does not exist fails the build, through mise and through apt. Set
// SPOV_RESOLVED_ADDONS=1; it builds one variant and attempts two.
//
// @decision(DL-033)
func TestIntegrationResolvedAddons(t *testing.T) {
	if os.Getenv("SPOV_RESOLVED_ADDONS") != "1" {
		t.Skip("set SPOV_RESOLVED_ADDONS=1: it builds a variant image")
	}
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	sel, err := toolchain.Parse([]string{"postgres@14", "php@8.1", "redis@7.4", "node@22", "java@21"})
	if err != nil {
		t.Fatal(err)
	}
	resolved := (&addonindex.Resolver{Warn: func(m string) { t.Error(m) }}).Resolve(ctx, sel)
	t.Logf("RESULT resolved %s", strings.Join(toolchain.Installed(resolved), ", "))
	spec := ImageSpec{OpenCodeVersion: testOpenCodeVersion(), Addons: resolved}
	requested := spec.Requested()
	if spec.Name() == requested.Name() {
		t.Fatal("the exact build has the request's name")
	}
	start := time.Now()
	name, err := p.EnsureImage(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	t.Logf("RESULT %s ready in %s", name, time.Since(start).Round(time.Second))

	exact := map[string]string{}
	for _, s := range resolved {
		exact[s.Name] = s.Exact()
	}
	h := bootWithAddons(ctx, t, p, name, resolved)
	for _, c := range []struct{ cmd, want string }{
		{"for i in $(seq 40); do pg_isready -q && break; sleep 0.5; done; psql -tAc 'show server_version'", exact["postgres"]},
		{"php --version | head -1", "PHP " + exact["php"] + " "},
		{"for i in $(seq 40); do redis-cli ping 2>/dev/null | grep -q PONG && break; sleep 0.5; done; redis-cli info server | grep redis_version",
			"redis_version:" + exact["redis"]},
		{"node --version", "v" + exact["node"]},
		{"java -version 2>&1 | head -1", strings.SplitN(exact["java"], "+", 2)[0]},
	} {
		res, err := h.Exec(ctx, []string{"bash", "-c", c.cmd})
		if err != nil {
			t.Fatal(err)
		}
		out := strings.TrimSpace(res.Stdout + res.Stderr)
		if res.ExitCode != 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, %q; want %q", c.cmd, res.ExitCode, out, c.want)
			continue
		}
		t.Logf("RESULT %s → %s", c.cmd, firstLine(out))
	}

	for _, bogus := range []struct{ entry, version string }{{"node@22", "22.99.0"}, {"postgres@14", "14.99"}} {
		sel, _ := toolchain.Parse([]string{bogus.entry})
		r, err := sel[0].Resolve(bogus.version)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = p.EnsureImage(ctx, ImageSpec{OpenCodeVersion: testOpenCodeVersion(), Addons: []toolchain.Selection{r}})
		if err == nil {
			t.Errorf("%s at %s built", bogus.entry, bogus.version)
			continue
		}
		t.Logf("RESULT %s at %s failed the build in %s: %s", bogus.entry, bogus.version,
			time.Since(start).Round(time.Second), firstLine(err.Error()))
	}
}
