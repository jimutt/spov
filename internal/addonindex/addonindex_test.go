package addonindex

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/toolchain"
)

// An excerpt of mise's real lists, with the entries that must be skipped.
var miseLists = map[string]string{
	"node": "20.19.5\n22.9.0\n22.23.3\n22.23.10-rc.1\n24.21.0\n",
	"java": "temurin-21.0.9+10.0.LTS\ntemurin-21.0.12+8.0.LTS\ntemurin-21.0.12+101.0.LTS\n" +
		"temurin-jre-21.0.13+5.0.LTS\ntemurin-jre-musl-21.0.13+5.0.LTS\ntemurin-22.0.2+9\nzulu-21.99.0\n",
	"rust":   "1.98.0\n1.99.0\nnightly\nbeta\nstable\n1.0.0-alpha\n",
	"python": "3.13-dev\n3.13.0rc1\n3.13.7\n3.13.11\n",
}

const packages = `Package: postgresql-14
Version: 14.23-1.pgdg13+1

Package: postgresql-14
Version: 14.24-1.pgdg13+2

Package: postgresql-14-dbgsym
Version: 14.99-1.pgdg13+1

Package: postgresql-15
Version: 15.14-1.pgdg13+1

Package: redis-server
Version: 6:7.4.6-1rl1~trixie1

Package: redis-server
Version: 6:7.4.10-1rl1~trixie1

Package: redis-server
Version: 6:8.0.2-1rl1~trixie1

Package: php8.1-cli
Version: 8.1.34-8+0~20260703.81+debian13~1.gbp86d822
`

type fakeIndex struct {
	*httptest.Server
	hits atomic.Int32
	fail atomic.Bool

	mu   sync.Mutex
	mise map[string]string
}

func (f *fakeIndex) add(tool, line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mise[tool] += line + "\n"
}

func newFakeIndex(t *testing.T) *fakeIndex {
	f := &fakeIndex{mise: map[string]string{}}
	for k, v := range miseLists {
		f.mise[k] = v
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(packages))
	_ = w.Close()
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.URL.Path == "/dists/Packages.gz" {
			_, _ = w.Write(gz.Bytes())
			return
		}
		f.mu.Lock()
		list, ok := f.mise[strings.TrimPrefix(r.URL.Path, "/mise/")]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(list))
	}))
	t.Cleanup(f.Close)
	return f
}

func sel(t *testing.T, entries ...string) []toolchain.Selection {
	t.Helper()
	s, err := toolchain.Parse(entries)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// resolver points the add-ons' indexes at f. The apt add-ons name their
// real repositories, so the client rewrites every request to f.
func resolver(f *fakeIndex, now *time.Time) *Resolver {
	return &Resolver{
		Client:  &http.Client{Transport: rewrite{f.URL}},
		MiseURL: "http://mise.invalid/mise/",
		Now:     func() time.Time { return *now },
	}
}

type rewrite struct{ base string }

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	path := r.URL.Path
	if strings.HasSuffix(path, "/Packages.gz") {
		path = "/dists/Packages.gz"
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, rw.base+path, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestAPartialVersionResolvesToTheNewestReleaseItCovers(t *testing.T) {
	f := newFakeIndex(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := resolver(f, &now)
	got := toolchain.Installed(r.Resolve(context.Background(),
		sel(t, "node@22", "java@21", "rust", "python@3.13", "postgres@14", "redis@7.4", "php@8.1", "playwright", "dotnet")))
	want := []string{
		"dotnet@10.0.401", // exact already
		"java@21.0.12+101.0.LTS",
		"php@8.1.34",
		"playwright@1.63.0", // exact versions only
		"postgres@14.24",
		"python@3.13.11",
		"redis@7.4.10",
		"rust@1.99.0",
		"node@22.23.3",
	}
	if strings.Join(got, " ") != strings.Join(sortLike(want, got), " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// sortLike returns want in got's order, so the comparison ignores the
// selection order Parse chose.
func sortLike(want, got []string) []string {
	out := make([]string, 0, len(want))
	for _, g := range got {
		for _, w := range want {
			if w == g {
				out = append(out, w)
			}
		}
	}
	if len(out) != len(want) {
		return want
	}
	return out
}

func TestAnAnswerIsCachedForAnHourThenRefreshedInTheBackground(t *testing.T) {
	f := newFakeIndex(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := resolver(f, &now)
	node := sel(t, "node@22")
	ctx := context.Background()

	r.Resolve(ctx, node)
	r.Resolve(ctx, node)
	if n := f.hits.Load(); n != 1 {
		t.Fatalf("a fresh answer was looked up again: %d lookups", n)
	}

	f.add("node", "22.24.0")
	now = now.Add(Fresh + time.Minute)
	if got := r.Resolve(ctx, node)[0].Exact(); got != "22.23.3" {
		t.Fatalf("a stale answer should be used at once, got %s", got)
	}
	r.background.Wait()
	if got := r.Resolve(ctx, node)[0].Exact(); got != "22.24.0" {
		t.Fatalf("the background refresh should have moved it on, got %s", got)
	}
}

func TestAFailedLookupFallsBackOrKeepsTheLastAnswer(t *testing.T) {
	f := newFakeIndex(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := resolver(f, &now)
	var warnings []string
	var mu sync.Mutex
	r.Warn = func(m string) { mu.Lock(); warnings = append(warnings, m); mu.Unlock() }
	ctx := context.Background()

	f.fail.Store(true)
	got := r.Resolve(ctx, sel(t, "node@22"))[0]
	if got.Resolved != "" || got.Version != "22" {
		t.Fatalf("with no answer, the version as written: got %+v", got)
	}
	// A failure is not retried within the backoff, then is.
	hits := f.hits.Load()
	r.Resolve(ctx, sel(t, "node@22"))
	if f.hits.Load() != hits {
		t.Fatal("a failed lookup was retried within the backoff")
	}
	f.fail.Store(false)
	now = now.Add(FailureBackoff)
	if got := r.Resolve(ctx, sel(t, "node@22"))[0]; got.Resolved != "22.23.3" {
		t.Fatalf("after the backoff: %+v", got)
	}
	f.fail.Store(true)

	f.fail.Store(false)
	r.Resolve(ctx, sel(t, "postgres@14"))
	f.fail.Store(true)
	now = now.Add(2 * Fresh)
	if got := r.Resolve(ctx, sel(t, "postgres@14"))[0].Exact(); got != "14.24" {
		t.Fatalf("a failed refresh must keep the last answer, got %s", got)
	}
	r.background.Wait()
	if got := r.Resolve(ctx, sel(t, "postgres@14"))[0].Exact(); got != "14.24" {
		t.Fatalf("got %s after the failed refresh", got)
	}
	// That last Resolve can start another refresh, which warns too.
	r.background.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) < 2 {
		t.Fatalf("each failure should warn: %q", warnings)
	}
}

func TestAnIndexWithNoUsableReleaseFallsBack(t *testing.T) {
	f := newFakeIndex(t)
	now := time.Now()
	r := resolver(f, &now)
	// The fake lists no postgres 16 and no ruby at all.
	for _, e := range []string{"postgres@16", "ruby@3.4"} {
		if got := r.Resolve(context.Background(), sel(t, e))[0]; got.Resolved != "" {
			t.Fatalf("%s resolved to %s", e, got.Resolved)
		}
	}
}

type memPersist struct {
	saved map[string]Answer
	err   error
}

func (m *memPersist) LoadAnswers() (map[string]Answer, error) { return m.saved, m.err }
func (m *memPersist) SaveAnswers(a map[string]Answer) error {
	m.saved = map[string]Answer{}
	for k, v := range a {
		m.saved[k] = v
	}
	return nil
}

func TestAnswersOutliveTheProcessThroughPersist(t *testing.T) {
	f := newFakeIndex(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	p := &memPersist{}
	first := resolver(f, &now)
	first.Persist = p
	first.Resolve(context.Background(), sel(t, "redis@7.4"))

	second := resolver(f, &now)
	second.Persist = p
	f.fail.Store(true)
	if got := second.Resolve(context.Background(), sel(t, "redis@7.4"))[0].Exact(); got != "7.4.10" {
		t.Fatalf("a new process should start from the saved answer, got %s", got)
	}
	if n := f.hits.Load(); n != 1 {
		t.Fatalf("the saved answer was looked up again: %d lookups", n)
	}

	broken := resolver(f, &now)
	broken.Persist = &memPersist{err: errors.New("unreadable")}
	f.fail.Store(false)
	if got := broken.Resolve(context.Background(), sel(t, "redis@7.4"))[0].Exact(); got != "7.4.10" {
		t.Fatalf("an unreadable store starts empty and looks up, got %s", got)
	}
}

func TestATamperedAnswerIsRefused(t *testing.T) {
	f := newFakeIndex(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := resolver(f, &now)
	r.Persist = &memPersist{saved: map[string]Answer{"node@22": {Version: "22.1.0; curl evil", At: now}}}
	f.fail.Store(true)
	if got := r.Resolve(context.Background(), sel(t, "node@22"))[0]; got.Resolved != "" {
		t.Fatalf("a cached answer that is not an exact release must not be used, got %q", got.Resolved)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"22.23.10", "22.23.9", true},
		{"21.0.12+101.0.LTS", "21.0.12+8.0.LTS", true},
		{"21.0.9+10", "21.0.12+8", false},
		{"14.24", "14.24", false},
		{"1.99.0", "1.99", true},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestPackageVersionsReadsUpstreamVersions(t *testing.T) {
	got, err := PackageVersions(strings.NewReader(packages), "redis-server")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "7.4.6 7.4.10 8.0.2" {
		t.Fatalf("got %v", got)
	}
}
