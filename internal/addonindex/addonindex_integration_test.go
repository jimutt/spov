//go:build integration

package addonindex

import (
	"context"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/toolchain"
)

// TestIntegrationResolverAgainstTheRealIndexes resolves a partial version
// of every add-on that takes one against mise's index and the apt
// repositories, as a server would. Re-run on a mise bump or when an
// index changes format. No Modal, no cost.
//
// @decision(DL-033)
func TestIntegrationResolverAgainstTheRealIndexes(t *testing.T) {
	entries := []string{
		"dotnet@10.0", "java@21", "php@8.1", "postgres@14", "redis@7.4", "ruby@3.4", "rust@stable",
		"python@3.13", "uv@0.12", "node@22", "bun@1", "go@1.26",
	}
	sel, err := toolchain.Parse(entries)
	if err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Warn: func(m string) { t.Error(m) }}
	start := time.Now()
	got := r.Resolve(context.Background(), sel)
	t.Logf("RESULT resolved %d add-ons cold in %s", len(got), time.Since(start).Round(time.Millisecond))
	for _, s := range got {
		if s.Resolved == "" {
			t.Errorf("%s did not resolve", s)
			continue
		}
		t.Logf("RESULT %s -> %s", s, s.Resolved)
	}
	start = time.Now()
	r.Resolve(context.Background(), sel)
	t.Logf("RESULT warm in %s", time.Since(start).Round(time.Microsecond))
}
