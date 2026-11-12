package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// fakeAccounts resolves the logins it was given and refuses the rest.
type fakeAccounts struct {
	accounts map[string]GitHubAccount
	err      error
	calls    []string
}

func (f *fakeAccounts) lookup(_ context.Context, login string) (GitHubAccount, error) {
	f.calls = append(f.calls, login)
	if f.err != nil {
		return GitHubAccount{}, f.err
	}
	a, ok := f.accounts[strings.ToLower(login)]
	if !ok {
		return GitHubAccount{}, ErrNoSuchAccount
	}
	return a, nil
}

// An "@login" line is resolved at the import and stored as the account
// it names, keyed on its ID; the same account twice takes the widest
// privilege.
//
// @decision(DL-128)
func TestTheSeedResolvesAccountLines(t *testing.T) {
	ctx := context.Background()
	gh := &fakeAccounts{accounts: map[string]GitHubAccount{
		"bob":   {ID: 42, Login: "Bob", Name: "Bob B"},
		"carol": {ID: 7, Login: "carol"},
	}}
	st := store.NewMemory()
	path := writeAllowlist(t, "@bob\nalice@example.com admin\n@Carol admin\n@carol\n@BOB\n")
	seeded, err := SeedAllowlist(ctx, st, "org", path, gh.lookup, nil)
	if err != nil || !seeded {
		t.Fatalf("seeded = %v, err = %v", seeded, err)
	}
	bob, err := st.Allowlist().GetAccount(ctx, "org", 42)
	if err != nil || bob.GithubLogin != "Bob" || bob.GithubName != "Bob B" || bob.Admin {
		t.Errorf("bob = %+v, %v", bob, err)
	}
	if carol, err := st.Allowlist().GetAccount(ctx, "org", 7); err != nil || !carol.Admin {
		t.Errorf("carol = %+v, %v; want the admin line kept", carol, err)
	}
	if len(gh.calls) != 2 {
		t.Errorf("GitHub was asked %v, want each login once", gh.calls)
	}
	list, err := NewAllowlist(ctx, StoreAllowlist(st.Allowlist(), "org"), AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := list.Lookup(42, "anything@example.com"); !ok {
		t.Error("the account line does not admit its account")
	}
	if _, ok := list.Lookup(0, "bob@example.com"); ok {
		t.Error("an account line admitted an address")
	}
}

// An account line that cannot be resolved makes the file unusable, on
// DL-007's terms: fatal on a first boot, a warning on a re-seed, and
// nothing written either way.
//
// @decision(DL-128) @decision(DL-007)
func TestAnUnresolvableAccountLineMakesTheFileUnusable(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		gh   *fakeAccounts
		want error
	}{
		{"unknown", &fakeAccounts{}, ErrNoSuchAccount},
		{"GitHub down", &fakeAccounts{err: ErrGitHubUnavailable}, ErrGitHubUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeAllowlist(t, "alice@example.com admin\n@nobody admin\n")
			st := store.NewMemory()
			_, err := SeedAllowlist(ctx, st, "org", path, tc.gh.lookup, nil)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "line 2") {
				t.Fatalf("first boot = %v, want %v naming line 2", err, tc.want)
			}
			if list, _ := st.Allowlist().List(ctx, "org"); len(list) != 0 {
				t.Errorf("a half-resolved file wrote %+v", list)
			}

			// A re-seed starts anyway, says why, and keeps what is there.
			if err := st.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "bob@example.com"}); err != nil {
				t.Fatal(err)
			}
			var warnings []string
			seeded, err := SeedAllowlist(ctx, st, "org", path, tc.gh.lookup, func(m string) { warnings = append(warnings, m) })
			if err != nil || seeded {
				t.Fatalf("re-seed = %v, %v", seeded, err)
			}
			if !hasWarning(warnings, "cannot seed one") {
				t.Errorf("the re-seed was silent: %v", warnings)
			}
			if list, _ := st.Allowlist().List(ctx, "org"); len(list) != 1 {
				t.Errorf("entries after the re-seed = %+v", list)
			}
		})
	}
}

// The lookups share one deadline, so a GitHub that never answers cannot
// hold the server at boot.
//
// @decision(DL-128)
func TestTheSeedsLookupsShareOneDeadline(t *testing.T) {
	var deadlines []time.Time
	hang := func(ctx context.Context, _ string) (GitHubAccount, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("a lookup ran with no deadline")
		}
		deadlines = append(deadlines, d)
		return GitHubAccount{}, ctx.Err()
	}
	path := writeAllowlist(t, "@a\n@b\n")
	before := time.Now()
	if _, err := SeedAllowlist(context.Background(), store.NewMemory(), "org", path, hang, nil); err == nil {
		t.Fatal("the seed succeeded")
	}
	if len(deadlines) == 0 {
		t.Fatal("nothing was looked up")
	}
	if d := deadlines[0]; d.Sub(before) > SeedLookupTimeout+time.Second || d.Sub(before) < SeedLookupTimeout-time.Second {
		t.Errorf("the deadline is %v away, want %v", d.Sub(before), SeedLookupTimeout)
	}
	for _, d := range deadlines[1:] {
		if !d.Equal(deadlines[0]) {
			t.Error("each lookup got its own deadline")
		}
	}
}

// A deployment with an administrator reads no file and asks GitHub
// nothing at boot.
//
// @decision(DL-128)
func TestASeededDeploymentAsksGitHubNothing(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "root@example.com", Admin: true}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeAccounts{}
	seeded, err := SeedAllowlist(ctx, st, "org", writeAllowlist(t, "@bob admin\n"), gh.lookup, nil)
	if err != nil || seeded || len(gh.calls) != 0 {
		t.Errorf("seeded = %v, err = %v, lookups = %v", seeded, err, gh.calls)
	}
}

// A seeder that resolved its lines but lost the race to another replica
// writes nothing.
//
// @decision(DL-128) @decision(DL-007)
func TestASeedThatLostTheRaceWritesNothing(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	gh := &fakeAccounts{accounts: map[string]GitHubAccount{"bob": {ID: 42, Login: "bob"}}}
	racing := func(ctx context.Context, login string) (GitHubAccount, error) {
		// Another replica seeds while this one asks GitHub.
		if err := st.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: "org", Email: "root@example.com", Admin: true}); err != nil {
			t.Fatal(err)
		}
		return gh.lookup(ctx, login)
	}
	seeded, err := SeedAllowlist(ctx, st, "org", writeAllowlist(t, "@bob admin\n"), racing, nil)
	if err != nil || seeded {
		t.Fatalf("seeded = %v, %v", seeded, err)
	}
	if _, err := st.Allowlist().GetAccount(ctx, "org", 42); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the losing seeder wrote its entry: %v", err)
	}
}

// A re-seed takes a line whose login an existing entry carries as that
// entry, without GitHub; a line that would create an entry asks GitHub,
// whatever login a user row carries.
//
// @decision(DL-128)
func TestAReSeedAsksGitHubForEveryEntryItWouldCreate(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: "org", GithubID: 5, GithubLogin: "kept"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().Create(ctx, store.User{ID: store.NewID(time.Now()), OrgID: "org", Email: "f@example.com",
		GithubID: 1, GithubLogin: "jsmith"}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeAccounts{accounts: map[string]GitHubAccount{"jsmith": {ID: 2, Login: "jsmith"}}}
	seeded, err := SeedAllowlist(ctx, st, "org", writeAllowlist(t, "@kept admin\n@jsmith\n"), gh.lookup, nil)
	if err != nil || !seeded {
		t.Fatalf("seeded = %v, %v", seeded, err)
	}
	if len(gh.calls) != 1 || gh.calls[0] != "jsmith" {
		t.Errorf("GitHub was asked %v, want only the new line", gh.calls)
	}
	if e, err := st.Allowlist().GetAccount(ctx, "org", 5); err != nil || !e.Admin {
		t.Errorf("the kept entry = %+v, %v", e, err)
	}
	if _, err := st.Allowlist().GetAccount(ctx, "org", 2); err != nil {
		t.Errorf("the new line is not GitHub's account: %v", err)
	}
	if _, err := st.Allowlist().GetAccount(ctx, "org", 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a user row's stale login made an entry: %v", err)
	}
}
