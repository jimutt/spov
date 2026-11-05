package toolchain

import (
	"slices"
	"strings"
	"testing"
)

func parse(t *testing.T, entries ...string) []Selection {
	t.Helper()
	sel, err := Parse(entries)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

// TestResolveAcceptsOnlyAnExactReleaseOfTheRequest is the one check an
// index's answer passes before it reaches a build command.
//
// @decision(DL-033)
func TestResolveAcceptsOnlyAnExactReleaseOfTheRequest(t *testing.T) {
	for _, c := range []struct {
		entry, answer string
		ok            bool
	}{
		{"node@22", "22.23.3", true},
		{"node@22", "23.0.0", false},       // not a release of 22
		{"node@22", "22.23.3-rc.1", false}, // a pre-release
		{"node@22", "22.23.3; curl x", false},
		{"node@22", "22.23", false}, // not exact
		{"go@1", "1.27.1", true},
		{"go@1", "12.0.1", false},
		{"rust@stable", "1.99.0", true},
		{"rust@stable", "nightly", false},
		{"java@21", "21.0.12+101.0.LTS", true},
		{"java@21", "jre-21.0.12+101.0.LTS", false},
		{"java@21.0.12", "21.0.12+101.0.LTS", true},
		{"postgres@14", "14.24", true},
		{"postgres@14", "14.24.1", false},
		{"postgres@14", "15.1", false},
		{"php@8.1", "8.1.34", true},
		{"php@8.1", "8.10.1", false},
		{"redis@7.4", "7.4.11", true},
	} {
		s := parse(t, c.entry)[0]
		got, err := s.Resolve(c.answer)
		if (err == nil) != c.ok {
			t.Errorf("%s resolving to %q: err %v, want ok %v", c.entry, c.answer, err, c.ok)
			continue
		}
		if c.ok && (got.Resolved != c.answer || got.Exact() != c.answer || got.Version != s.Version) {
			t.Errorf("%s resolving to %q: %+v", c.entry, c.answer, got)
		}
	}
	// The requested version itself, or nothing, leaves it unresolved.
	s := parse(t, "redis@7.4")[0]
	for _, v := range []string{"", "7.4"} {
		if got, err := s.Resolve(v); err != nil || got.Resolved != "" {
			t.Errorf("Resolve(%q) = %+v, %v", v, got, err)
		}
	}
}

// TestOnlyAPartialVersionHasAnIndex.
//
// @decision(DL-033)
func TestOnlyAPartialVersionHasAnIndex(t *testing.T) {
	for entry, want := range map[string]Index{
		"node@22":     {Mise: "node"},
		"java@21":     {Mise: "java", Prefix: "temurin-"},
		"postgres@14": {Packages: "https://apt.postgresql.org/pub/repos/apt/dists/trixie-pgdg/main/binary-amd64/Packages.gz", Package: "postgresql-14"},
		"php@8.1":     {Packages: "https://packages.sury.org/php/dists/trixie/main/binary-amd64/Packages.gz", Package: "php8.1-cli"},
		"redis@7.4":   {Packages: "https://packages.redis.io/deb/dists/trixie/main/binary-amd64/Packages.gz", Package: "redis-server"},
	} {
		got, ok := parse(t, entry)[0].Index()
		if !ok || got != want {
			t.Errorf("%s: %+v, %v", entry, got, ok)
		}
	}
	for _, entry := range []string{"node@22.23.3", "redis@7.4.6", "playwright", "dotnet", "java@21.0.12+101.0.LTS"} {
		if _, ok := parse(t, entry)[0].Index(); ok {
			t.Errorf("%s needs no resolving", entry)
		}
	}
}

// TestAResolvedSelectionPinsItsReleaseAndAnUnresolvedOneRendersAsBefore.
//
// @decision(DL-033)
func TestAResolvedSelectionPinsItsReleaseAndAnUnresolvedOneRendersAsBefore(t *testing.T) {
	sel := parse(t, "node@22", "php@8.1", "postgres@14", "redis@7.4")
	unresolved := strings.Join(slices.Concat(layersOf(sel)...), "\n")
	for _, want := range []string{"mise use -g node@22", "php8.1-cli ", "grep -qF 'PHP 8.1.'", "postgresql-14;", "'redis-server=6:7.4.*'"} {
		if !strings.Contains(unresolved, want) {
			t.Errorf("unresolved layers lack %q", want)
		}
	}
	resolved, err := ResolveTo(sel, []string{"node@22.23.3", "php@8.1.34", "postgres@14.24", "redis@7.4.11"})
	if err != nil {
		t.Fatal(err)
	}
	pinned := strings.Join(slices.Concat(layersOf(resolved)...), "\n")
	for _, want := range []string{
		"mise use -g node@22.23.3", "'php8.1-cli=8.1.34-*'", "'php8.1-pgsql=8.1.34-*'", "grep -qF 'PHP 8.1.34 '",
		"'postgresql-14=14.24-*'", "/etc/postgresql/14/main/pg_hba.conf", "'redis-server=6:7.4.11-*'",
	} {
		if !strings.Contains(pinned, want) {
			t.Errorf("resolved layers lack %q", want)
		}
	}
	if got := Installed(resolved); !slices.Equal(got, []string{"node@22.23.3", "php@8.1.34", "postgres@14.24", "redis@7.4.11"}) {
		t.Errorf("Installed = %v", got)
	}
	if got := Strings(resolved); !slices.Equal(got, Strings(sel)) {
		t.Errorf("the request is unchanged: %v", got)
	}
	if !slices.Equal(Unresolved(resolved), sel) {
		t.Error("Unresolved does not give the request back")
	}
	if !strings.Contains(Guide(resolved), "PostgreSQL 14.24") || !strings.Contains(Guide(resolved), "Node 22.23.3") {
		t.Error("the guide does not name the resolved versions")
	}
	if _, err := ResolveTo(sel, []string{"node@23.0.0"}); err == nil {
		t.Error("ResolveTo took a version that is not a release of the request")
	}
}

func layersOf(sel []Selection) [][]string {
	var out [][]string
	for _, s := range sel {
		out = append(out, s.Layer())
	}
	return out
}
