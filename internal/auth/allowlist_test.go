package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// fakeSource is an allowlist a test can change under a running cache.
type fakeSource struct {
	mu      sync.Mutex
	entries map[string]bool
	err     error
	calls   int
}

func (f *fakeSource) load(context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]bool, len(f.entries))
	for k, v := range f.entries {
		out[k] = v
	}
	return out, nil
}

func (f *fakeSource) set(entries map[string]bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries, f.err = entries, err
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestRefreshReplacesTheList reloads the list on a timer, and
// therefore on every replica: an added entry works and a removed one
// stops working.
//
// @decision(DL-007)
func TestRefreshReplacesTheList(t *testing.T) {
	src := &fakeSource{entries: map[string]bool{"alice@example.com": false, "bob@example.com": true}}
	list, err := NewAllowlist(context.Background(), src.load, AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := list.Lookup("bob@example.com"); !ok {
		t.Fatal("bob was not allowed to begin with")
	}

	src.set(map[string]bool{"alice@example.com": true, "carol@example.com": false}, nil)
	if err := list.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := list.Lookup("bob@example.com"); ok {
		t.Error("bob is still allowed after being removed")
	}
	if _, ok := list.Lookup("carol@example.com"); !ok {
		t.Error("carol was not allowed after being added")
	}
	// A changed privilege takes effect too.
	if admin, _ := list.Lookup("alice@example.com"); !admin {
		t.Error("alice did not become an admin")
	}
}

// TestRefreshKeepsThePreviousListWhenTheReadFails: a database blip must
// not log everyone out of a running server.
//
// @decision(DL-007)
func TestRefreshKeepsThePreviousListWhenTheReadFails(t *testing.T) {
	src := &fakeSource{entries: map[string]bool{"alice@example.com": true}}
	var warnings []string
	list, err := NewAllowlist(context.Background(), src.load, AllowlistRefresh,
		func(m string) { warnings = append(warnings, m) })
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("connection reset")
	src.set(nil, boom)
	for i := 0; i < 3; i++ {
		if err := list.Refresh(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("refresh %d: %v", i, err)
		}
		if admin, ok := list.Lookup("alice@example.com"); !ok || !admin {
			t.Fatalf("alice was lost after failed refresh %d", i)
		}
	}
	// One line for the outage, not one per tick: Run refreshes every
	// AllowlistRefresh, and a database that is down for an hour would
	// otherwise bury the log.
	if len(warnings) != 1 {
		t.Errorf("got %d warnings for one outage, want 1: %v", len(warnings), warnings)
	}
	// And recovery is announced, or the last thing in the log is a
	// failure that has since been fixed.
	src.set(map[string]bool{"alice@example.com": true}, nil)
	if err := list.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[1], "again") {
		t.Errorf("recovery was not announced: %v", warnings)
	}
}

// TestAnEmptyReadIsApplied pins that an empty query result is applied
// rather than treated as a failed read.
//
// @decision(DL-007)
func TestAnEmptyReadIsApplied(t *testing.T) {
	src := &fakeSource{entries: map[string]bool{"alice@example.com": true}}
	list, err := NewAllowlist(context.Background(), src.load, AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(map[string]bool{}, nil)
	if err := list.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := list.Lookup("alice@example.com"); ok {
		t.Error("an emptied allowlist still allows someone: the last entry cannot be removed")
	}
}

// A first read that fails leaves no list at all, and the zero value
// allows nobody — so the server must not start rather than refuse every
// login and look like an OAuth problem.
//
// @decision(DL-007)
func TestAFailedFirstReadRefusesToBuildTheList(t *testing.T) {
	boom := errors.New("no database")
	src := &fakeSource{err: boom}
	if _, err := NewAllowlist(context.Background(), src.load, AllowlistRefresh, nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the read error", err)
	}
}

// An empty store is not fatal, unlike DL-007's empty file: it is one API
// call away from being fixed, where an unreadable file needed a
// redeploy. It is still said out loud.
//
// @decision(DL-007)
func TestAnEmptyStoreWarnsRatherThanRefusing(t *testing.T) {
	var warnings []string
	src := &fakeSource{entries: map[string]bool{}}
	list, err := NewAllowlist(context.Background(), src.load, AllowlistRefresh,
		func(m string) { warnings = append(warnings, m) })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := list.Lookup("alice@example.com"); ok {
		t.Error("an empty allowlist allowed someone")
	}
	if !hasWarning(warnings, "nobody is on the allowlist") {
		t.Errorf("warnings = %v", warnings)
	}
}

// TestRunRefreshesUntilTheContextEnds is the mechanism that gives the
// staleness its bound. Without it a replica reads the store once at
// startup and never again, which is DL-007's gap with extra steps.
//
// @decision(DL-007)
func TestRunRefreshesUntilTheContextEnds(t *testing.T) {
	src := &fakeSource{entries: map[string]bool{"alice@example.com": true}}
	list, err := NewAllowlist(context.Background(), src.load, time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); list.Run(ctx) }()

	src.set(map[string]bool{}, nil)
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := list.Lookup("alice@example.com"); !ok {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("the list was never refreshed")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	// And it stops: a refresher that outlived its context would keep
	// querying a closed pool on the way out.
	after := src.count()
	time.Sleep(20 * time.Millisecond)
	if got := src.count(); got != after {
		t.Errorf("the source was read %d more times after Run returned", got-after)
	}
}

// TestLiveReadsThroughTheCache: an admin who has just added an address
// should not have to tell the person to wait out a refresh interval.
//
// @decision(DL-007)
func TestLiveReadsThroughTheCache(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{entries: map[string]bool{}}
	// A long interval, so nothing but Live can explain a fresh answer.
	list, err := NewAllowlist(ctx, src.load, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(map[string]bool{"alice@example.com": true}, nil)
	if _, ok := list.Lookup("alice@example.com"); ok {
		t.Fatal("the cache already had alice, so this proves nothing")
	}
	admin, ok, err := list.Live(ctx, "Alice@Example.com")
	if err != nil || !ok || !admin {
		t.Fatalf("live: admin=%v ok=%v err=%v", admin, ok, err)
	}
	// What it read is kept, so the next request does not read again.
	if _, ok := list.Lookup("alice@example.com"); !ok {
		t.Error("Live did not update the cache")
	}

	boom := errors.New("no database")
	src.set(nil, boom)
	if _, _, err := list.Live(ctx, "alice@example.com"); !errors.Is(err, boom) {
		t.Errorf("a failed live read gave %v, want the error: a login must not silently fall back", err)
	}
}

// TestZeroAllowlistAllowsNobody: the zero value must be closed, not open.
//
// @decision(DL-007)
func TestZeroAllowlistAllowsNobody(t *testing.T) {
	var list Allowlist
	if _, ok := list.Lookup("alice@example.com"); ok {
		t.Error("the zero allowlist allowed someone")
	}
}

// The source is what the cache is built from, so it must project the
// store faithfully — including the case-folding the store applies.
//
// @decision(DL-007)
func TestTheStoreSourceProjectsTheOrgsEntries(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	for _, e := range []store.AllowlistEntry{
		{OrgID: "org", Email: "Alice@Example.com", Admin: true},
		{OrgID: "org", Email: "bob@example.com"},
		{OrgID: "other", Email: "mallory@example.com", Admin: true},
	} {
		if err := st.Allowlist().Put(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := StoreAllowlist(st.Allowlist(), "org")(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want only this org's", entries)
	}
	if !entries["alice@example.com"] {
		t.Error("alice is not an admin, or was not lower-cased")
	}
	if entries["bob@example.com"] {
		t.Error("bob became an admin")
	}
}

// A parse error must reach the caller rather than becoming an empty
// list, which would be indistinguishable from "nobody is allowed".
//
// @decision(DL-007)
func TestParseAllowlistEmailRejectsWhatIsNotAnAddress(t *testing.T) {
	for _, bad := range []string{"alice", "@", "a@b@c", "", "Alice <alice@example.com>"} {
		if got, err := ParseAllowlistEmail(bad); err == nil {
			t.Errorf("%q was accepted as %q", bad, got)
		}
	}
	got, err := ParseAllowlistEmail("  Alice@Example.COM ")
	if err != nil || got != "alice@example.com" {
		t.Errorf("got %q, %v", got, err)
	}
	// Past the store's key limit the row is refused with an index-size
	// error, which reaches the caller as a 502 rather than "that is not
	// an address".
	long := strings.Repeat("a", MaxEmailLen) + "@example.com"
	if _, err := ParseAllowlistEmail(long); err == nil {
		t.Errorf("an address of %d characters was accepted", len(long))
	}
}

// testAllowlist builds a list over entries written in the seed file's
// syntax, and returns the function that changes them. Tests that used
// to rewrite a file and call Reload now rewrite this and call Refresh.
func testAllowlist(t *testing.T, content string) (*Allowlist, func(*testing.T, string)) {
	t.Helper()
	src := &fakeSource{}
	set := func(t *testing.T, content string) {
		t.Helper()
		entries, err := parseAllowlist(strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		src.set(entries, nil)
	}
	set(t, content)
	list, err := NewAllowlist(context.Background(), src.load, AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	return list, set
}

// failingAllowlist fails the read the cache is built from.
type failingAllowlist struct {
	store.AllowlistStore
	err error
}

func (f failingAllowlist) List(context.Context, string) ([]store.AllowlistEntry, error) {
	return nil, f.err
}

// A store that cannot be read must produce an error, never an empty
// list.
//
// @decision(DL-007)
func TestAFailedReadIsNotAnEmptyAllowlist(t *testing.T) {
	boom := errors.New("connection refused")
	st := store.NewMemory()
	src := StoreAllowlist(failingAllowlist{AllowlistStore: st.Allowlist(), err: boom}, "org")
	if entries, err := src(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("entries = %v, err = %v; want the read error", entries, err)
	}
}
