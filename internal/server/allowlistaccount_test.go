package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// fakeGitHubAccounts is GitHub's GET /users/{login} for the tests: the
// accounts it knows, the logins it refuses and how often it was asked.
type fakeGitHubAccounts struct {
	mu       sync.Mutex
	accounts map[string]auth.GitHubAccount
	refuse   map[string]error
	err      error
	calls    []string
}

func newFakeGitHubAccounts() *fakeGitHubAccounts {
	return &fakeGitHubAccounts{
		accounts: map[string]auth.GitHubAccount{
			"octo":    {ID: 77, Login: "Octo", Name: "Octo Cat"},
			"bob":     {ID: 42, Login: "bob", Name: "Bob B"},
			"carol":   {ID: 7, Login: "carol", Name: "Carol C"},
			"them-gh": {ID: 4242, Login: "them-gh", Name: "Them"},
			// The login fixture's stub GitHub signs in as this account.
			"octocat": {ID: 583231, Login: "octocat", Name: "The Octocat"},
		},
		refuse: map[string]error{"acme": auth.ErrNotAPersonAccount},
	}
}

func (g *fakeGitHubAccounts) lookup(_ context.Context, login string) (auth.GitHubAccount, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, login)
	if g.err != nil {
		return auth.GitHubAccount{}, g.err
	}
	key := strings.ToLower(login)
	if err, ok := g.refuse[key]; ok {
		return auth.GitHubAccount{}, err
	}
	a, ok := g.accounts[key]
	if !ok {
		return auth.GitHubAccount{}, auth.ErrNoSuchAccount
	}
	return a, nil
}

func (g *fakeGitHubAccounts) asked() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// accountFixture is an allowlist fixture whose GitHub is gh.
func accountFixture(t *testing.T, entries map[string]bool) (*apiFixture, *fakeGitHubAccounts) {
	t.Helper()
	gh := newFakeGitHubAccounts()
	f := allowlistFixtureWith(t, true, entries, func(d *Deps) { d.GitHubAccounts = gh.lookup })
	return f, gh
}

func entryOf(t *testing.T, out []byte) AllowlistEntryView {
	t.Helper()
	var v AllowlistEntryView
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// An "@login" entry is resolved with GitHub once, stored by the numeric
// ID, and answered with the name GitHub gave; a later PUT of the same
// login is matched against the stored one, in any case, without asking.
// A percent-encoded "@" is the same path.
//
// @decision(DL-128)
func TestAnAccountEntryIsResolvedOnceAndStoredByItsID(t *testing.T) {
	f, gh := accountFixture(t, map[string]bool{"me@example.com": true})
	resp, out := f.do(http.MethodPut, "/api/v1/allowlist/@octo", map[string]any{"admin": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, out)
	}
	got := entryOf(t, out)
	if got.Kind != EntryKindGitHub || got.GithubID != 77 || got.GithubLogin != "Octo" || got.Name != "Octo Cat" ||
		got.Email != "" || got.Admin || got.You || got.LastLoginAt != nil {
		t.Errorf("entry = %+v", got)
	}
	if e, err := f.store.Allowlist().GetAccount(context.Background(), auth.LocalID, 77); err != nil || e.GithubLogin != "Octo" {
		t.Errorf("stored = %+v, %v", e, err)
	}
	if gh.asked() != 1 {
		t.Fatalf("GitHub was asked %d times", gh.asked())
	}
	for _, path := range []string{"/api/v1/allowlist/@OCTO", "/api/v1/allowlist/%40octo"} {
		resp, out := f.do(http.MethodPut, path, map[string]any{"admin": true})
		if resp.StatusCode != http.StatusOK || !entryOf(t, out).Admin || entryOf(t, out).GithubID != 77 {
			t.Errorf("PUT %s = %d: %s", path, resp.StatusCode, out)
		}
	}
	if gh.asked() != 1 {
		t.Errorf("an existing entry asked GitHub again (%d calls)", gh.asked())
	}
	entries := allowlistOf(t, f)
	if len(entries) != 2 || entries[0].GithubID != 77 || !entries[0].Admin {
		t.Errorf("entries = %+v", entries)
	}
	// And it admits its account, whatever the address.
	if _, ok := f.srv.deps.Login.Allowlist.Lookup(77, "whatever@example.com"); !ok {
		t.Error("the cache does not admit the account")
	}
	if resp, out := f.do(http.MethodDelete, "/api/v1/allowlist/%40Octo", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE with an encoded @ = %d: %s", resp.StatusCode, out)
	}
	if _, ok := f.srv.deps.Login.Allowlist.Lookup(77, ""); ok {
		t.Error("a deleted account entry still admits")
	}
}

// What GitHub refuses is refused, in words that say which refusal it
// was, and nothing is stored.
//
// @decision(DL-128)
func TestAnAccountGitHubCannotResolveIsNotStored(t *testing.T) {
	for _, tc := range []struct {
		name, login string
		err         error
		status      int
		says        string
	}{
		{"unknown", "nobody", nil, http.StatusUnprocessableEntity, "GitHub knows no account named @nobody"},
		{"an organisation", "acme", nil, http.StatusUnprocessableEntity, "organisation"},
		{"rate limited", "bob", auth.ErrGitHubRateLimited, http.StatusBadGateway, "try again later"},
		{"unreachable", "bob", auth.ErrGitHubUnavailable, http.StatusBadGateway, "try again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, gh := accountFixture(t, map[string]bool{"me@example.com": true})
			gh.err = tc.err
			resp, out := f.do(http.MethodPut, "/api/v1/allowlist/@"+tc.login, map[string]any{"admin": true})
			if resp.StatusCode != tc.status || !strings.Contains(string(out), tc.says) {
				t.Errorf("PUT = %d, want %d saying %q: %s", resp.StatusCode, tc.status, tc.says, out)
			}
			if entries := allowlistOf(t, f); len(entries) != 1 {
				t.Errorf("a refused account was stored: %+v", entries)
			}
		})
	}
	// A server with no lookup refuses rather than storing half an entry.
	f := allowlistFixtureWith(t, true, map[string]bool{"me@example.com": true}, func(d *Deps) {
		d.GitHubAccounts = nil
	})
	f.srv.deps.GitHubAccounts = nil
	if resp, out := f.do(http.MethodPut, "/api/v1/allowlist/@bob", map[string]any{"admin": false}); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("PUT with no lookup = %d: %s", resp.StatusCode, out)
	}
}

// A PUT that creates an entry always asks GitHub and stores the ID
// GitHub returns, even when a user row carries the login: a removed
// former member who renamed away leaves the old login there, and the
// person who registered it since is the one being added. With GitHub
// down nothing is stored, however sure the row looks.
//
// @decision(DL-128)
func TestANewEntryIsAlwaysResolvedWithGitHub(t *testing.T) {
	f, gh := accountFixture(t, map[string]bool{"me@example.com": true})
	if err := f.store.Users().Create(context.Background(), store.User{
		ID: store.NewID(time.Now()), OrgID: auth.LocalID, Email: "former@example.com", GithubID: 1,
		GithubLogin: "jsmith", Name: "Former",
	}); err != nil {
		t.Fatal(err)
	}
	gh.accounts["jsmith"] = auth.GitHubAccount{ID: 2, Login: "jsmith", Name: "New Person"}
	gh.err = auth.ErrGitHubUnavailable
	if resp, out := f.do(http.MethodPut, "/api/v1/allowlist/@jsmith", map[string]any{"admin": false}); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("PUT with GitHub down = %d: %s", resp.StatusCode, out)
	}
	if entries := allowlistOf(t, f); len(entries) != 1 {
		t.Fatalf("a stale user row's login was stored as an entry: %+v", entries)
	}
	gh.err = nil
	resp, out := f.do(http.MethodPut, "/api/v1/allowlist/@jsmith", map[string]any{"admin": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, out)
	}
	if got := entryOf(t, out); got.GithubID != 2 || got.Name != "New Person" {
		t.Errorf("entry = %+v, want the account GitHub names now", got)
	}
	if _, err := f.store.Allowlist().GetAccount(context.Background(), auth.LocalID, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the former member's account got an entry: %v", err)
	}
	// An existing entry is matched by its stored login without a lookup.
	asked := gh.asked()
	if resp, out := f.do(http.MethodPut, "/api/v1/allowlist/@JSMITH", map[string]any{"admin": true}); resp.StatusCode != http.StatusOK ||
		gh.asked() != asked {
		t.Errorf("PUT of an existing entry = %d after %d lookups: %s", resp.StatusCode, gh.asked()-asked, out)
	}
}

// A login's text never reaches GitHub or another route unless it has a
// login's shape, and a bare word is not taken for one.
//
// @decision(DL-128)
func TestAMalformedLoginIsRefusedBeforeGitHubIsAsked(t *testing.T) {
	f, gh := accountFixture(t, map[string]bool{"me@example.com": true})
	for _, bad := range []string{"bob", "@-bob", "@bo--b", "@bob.smith", "@" + strings.Repeat("x", 40),
		"%40bob%2F..%2Fusers", "@bob%3Fx=1", "@bob%23x", "@%2e%2e"} {
		resp, out := f.do(http.MethodPut, "/api/v1/allowlist/"+bad, map[string]any{"admin": true})
		if resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusNotFound {
			t.Errorf("PUT %s = %d: %s", bad, resp.StatusCode, out)
		}
		if resp, _ := f.do(http.MethodDelete, "/api/v1/allowlist/"+bad, nil); resp.StatusCode != http.StatusUnprocessableEntity &&
			resp.StatusCode != http.StatusNotFound {
			t.Errorf("DELETE %s = %d", bad, resp.StatusCode)
		}
	}
	if gh.asked() != 0 {
		t.Errorf("GitHub was asked about %v", gh.calls)
	}
	if entries := allowlistOf(t, f); len(entries) != 1 {
		t.Errorf("entries = %+v", entries)
	}
	resp, out := f.do(http.MethodPut, "/api/v1/allowlist/bob", map[string]any{"admin": true})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(out), "@login") {
		t.Errorf("a bare word = %d, want a 422 that says how to write an account: %s", resp.StatusCode, out)
	}
}

// The listing says whom each entry covers: the newest sign-in among
// them, and whether it covers the caller.
//
// @decision(DL-128)
func TestTheListSaysWhenAnEntrysPeopleLastSignedIn(t *testing.T) {
	f, _ := accountFixture(t, map[string]bool{"me@example.com": true, "never@example.com": false})
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := f.store.Users().Create(ctx, store.User{ID: store.NewID(at), OrgID: auth.LocalID, Email: "me@example.com",
		Name: "Me", LastLoginAt: &at}); err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Hour)
	if err := f.store.Users().Create(ctx, store.User{ID: store.NewID(later), OrgID: auth.LocalID, Email: "b@example.com",
		GithubID: 42, GithubLogin: "bob", LastLoginAt: &later}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: auth.LocalID, GithubID: 42, GithubLogin: "bob",
		GithubName: "Bob B"}); err != nil {
		t.Fatal(err)
	}
	byRef := map[string]AllowlistEntryView{}
	for _, e := range allowlistOf(t, f) {
		byRef[e.Email+e.GithubLogin] = e
	}
	if me := byRef["me@example.com"]; !me.You || me.LastLoginAt == nil || !me.LastLoginAt.Equal(at) || me.Name != "Me" ||
		me.Kind != EntryKindEmail {
		t.Errorf("my entry = %+v", me)
	}
	if bob := byRef["bob"]; bob.You || bob.LastLoginAt == nil || !bob.LastLoginAt.Equal(later) || bob.Name != "Bob B" {
		t.Errorf("bob's entry = %+v", bob)
	}
	if never := byRef["never@example.com"]; never.LastLoginAt != nil || never.You {
		t.Errorf("an entry nobody used = %+v", never)
	}
}

// Two entries carrying one stale login are not guessed between: the
// 409 names each by its id: form and display name, and that form
// reaches each of them for a change or a removal, without GitHub.
//
// @decision(DL-128)
func TestTwoEntriesWithOneLoginAreNotGuessedBetween(t *testing.T) {
	f, gh := accountFixture(t, map[string]bool{"me@example.com": true})
	ctx := context.Background()
	for _, e := range []store.AllowlistEntry{
		{OrgID: auth.LocalID, GithubID: 1, GithubLogin: "carol", GithubName: "Carol Old"},
		{OrgID: auth.LocalID, GithubID: 2, GithubLogin: "carol", GithubName: "Carol New"},
	} {
		if err := f.store.Allowlist().Put(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		resp, out := f.do(method, "/api/v1/allowlist/@carol", map[string]any{"admin": true})
		body := string(out)
		if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "id:1 (Carol Old)") ||
			!strings.Contains(body, "id:2 (Carol New)") || !strings.Contains(body, "id: form") {
			t.Errorf("%s = %d, want a 409 naming both by id: %s", method, resp.StatusCode, out)
		}
	}
	if entries := allowlistOf(t, f); len(entries) != 3 || entries[0].Admin || entries[1].Admin {
		t.Errorf("entries = %+v", entries)
	}
	// The id: form reaches each one, keeping its labels.
	resp, out := f.do(http.MethodPut, "/api/v1/allowlist/id:2", map[string]any{"admin": true})
	if got := entryOf(t, out); resp.StatusCode != http.StatusOK || got.GithubID != 2 || !got.Admin || got.Name != "Carol New" {
		t.Errorf("PUT id:2 = %d: %s", resp.StatusCode, out)
	}
	if resp, out := f.do(http.MethodDelete, "/api/v1/allowlist/id%3A1", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE id:1 = %d: %s", resp.StatusCode, out)
	}
	if _, err := f.store.Allowlist().GetAccount(ctx, auth.LocalID, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("id:1 is still there: %v", err)
	}
	if gh.asked() != 0 {
		t.Errorf("GitHub was asked %d times", gh.asked())
	}
}

// The id: form names only an existing account entry, by digits alone:
// a PUT by ID never creates one, and anything else after id: is a 422.
//
// @decision(DL-128)
func TestTheIDFormNamesOnlyAnExistingAccountEntry(t *testing.T) {
	f, gh := accountFixture(t, map[string]bool{"me@example.com": true})
	resp, out := f.do(http.MethodPut, "/api/v1/allowlist/id:77", map[string]any{"admin": false})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(out), "@login") {
		t.Errorf("PUT of a new id: = %d: %s", resp.StatusCode, out)
	}
	for _, bad := range []string{"id:", "id:0", "id:-1", "id:1x", "id:%2B5", "id:1%2F..", "id:99999999999999999999"} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			if resp, out := f.do(method, "/api/v1/allowlist/"+bad, map[string]any{"admin": true}); resp.StatusCode != http.StatusUnprocessableEntity &&
				resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s = %d: %s", method, bad, resp.StatusCode, out)
			}
		}
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/allowlist/id:77", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE of an unknown id: = %d", resp.StatusCode)
	}
	if entries := allowlistOf(t, f); len(entries) != 1 || gh.asked() != 0 {
		t.Errorf("entries = %+v after %d lookups", entries, gh.asked())
	}
}

// The self rules hold for a caller covered by several entries: one of
// their admin entries may go while another covers them, the last may
// not, and an account entry alone makes an admin.
//
// @decision(DL-128) @decision(DL-007)
func TestTheSelfRulesHoldForSeveralEntries(t *testing.T) {
	f, _ := accountFixture(t, map[string]bool{"me@example.com": true, "other@example.com": true})
	ctx := context.Background()
	if err := f.store.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: auth.LocalID, GithubID: 500, GithubLogin: "me-gh",
		Admin: true}); err != nil {
		t.Fatal(err)
	}
	f.auth.inner = auth.StaticAuthenticator{Principal: auth.Principal{
		UserID: auth.LocalID, OrgID: auth.LocalID, Email: "me@example.com", GitHubID: 500, Admin: true,
	}}
	for _, e := range allowlistOf(t, f) {
		if e.You != (e.Email == "me@example.com" || e.GithubID == 500) {
			t.Errorf("entry %+v marks you wrongly", e)
		}
	}
	// The address entry may go, since the account entry still covers.
	if resp, out := f.do(http.MethodPut, "/api/v1/allowlist/me@example.com", map[string]any{"admin": false}); resp.StatusCode != http.StatusOK {
		t.Fatalf("demoting one of two own admin entries = %d: %s", resp.StatusCode, out)
	}
	// The account entry is now the last that makes the caller an admin.
	for _, tc := range []struct {
		method string
		body   any
	}{{http.MethodPut, map[string]any{"admin": false}}, {http.MethodDelete, nil}} {
		resp, out := f.do(tc.method, "/api/v1/allowlist/@me-gh", tc.body)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(out), "ask another admin") {
			t.Errorf("%s of the caller's last admin entry = %d: %s", tc.method, resp.StatusCode, out)
		}
	}
	// The caller is an admin through the account entry alone.
	if err := f.store.Allowlist().Delete(ctx, auth.LocalID, "me@example.com"); err != nil {
		t.Fatal(err)
	}
	if resp, out := f.do(http.MethodPut, "/api/v1/allowlist/new@example.com", map[string]any{"admin": false}); resp.StatusCode != http.StatusOK {
		t.Errorf("an admin by account = %d: %s", resp.StatusCode, out)
	}
	// And a stale one through it cannot write: the store, not the cache,
	// decides.
	if err := f.store.Allowlist().DeleteAccount(ctx, auth.LocalID, 500); err != nil {
		t.Fatal(err)
	}
	if resp, out := f.do(http.MethodPut, "/api/v1/allowlist/@me-gh", map[string]any{"admin": true}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a stale admin put themselves back: %d: %s", resp.StatusCode, out)
	}
	if _, err := f.store.Allowlist().GetAccount(ctx, auth.LocalID, 500); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the removed account entry is back: %v", err)
	}
}

// Another organisation's admin cannot edit this one's list by account
// either, and makes the server ask GitHub nothing.
//
// @decision(DL-128) @decision(DL-007)
func TestAnotherOrgsAdminCannotAddAnAccount(t *testing.T) {
	f, gh := accountFixture(t, map[string]bool{"me@example.com": true})
	f.auth.inner = auth.StaticAuthenticator{
		Principal: auth.Principal{UserID: "op", OrgID: "another-org", Email: "op@example.com", GitHubID: 77, Admin: true},
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if resp, out := f.do(method, "/api/v1/allowlist/@octo", map[string]any{"admin": true}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s from another org = %d: %s", method, resp.StatusCode, out)
		}
	}
	if gh.asked() != 0 {
		t.Errorf("another org's admin made the server ask GitHub %d times", gh.asked())
	}
	// A non-admin is refused before GitHub is asked as well.
	f.auth.inner = auth.StaticAuthenticator{
		Principal: auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID, Email: "me@example.com"},
	}
	if resp, _ := f.do(http.MethodPut, "/api/v1/allowlist/@octo", map[string]any{"admin": true}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a non-admin = %d", resp.StatusCode)
	}
	if gh.asked() != 0 {
		t.Errorf("a non-admin made the server ask GitHub %d times", gh.asked())
	}
}

// A stale admin, whose entry is gone from the store but not from the
// cache, cannot make the server ask GitHub.
//
// @decision(DL-128)
func TestAStaleAdminCannotMakeTheServerAskGitHub(t *testing.T) {
	f, gh := accountFixture(t, map[string]bool{"me@example.com": true, "other@example.com": true})
	if err := f.store.Allowlist().Delete(context.Background(), auth.LocalID, "me@example.com"); err != nil {
		t.Fatal(err)
	}
	if resp, out := f.do(http.MethodPut, "/api/v1/allowlist/@octo", map[string]any{"admin": true}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("PUT as a stale admin = %d: %s", resp.StatusCode, out)
	}
	if gh.asked() != 0 {
		t.Errorf("GitHub was asked %d times", gh.asked())
	}
}

// bindThem binds the off-boarding fixture's person to an account and
// gives them an account entry beside their address entry.
func bindThem(t *testing.T, o *offboarded) {
	t.Helper()
	ctx := context.Background()
	if err := o.f.store.Users().BindGithub(ctx, auth.LocalID, theirID, 4242); err != nil {
		t.Fatal(err)
	}
	u, _ := o.f.store.Users().Get(ctx, auth.LocalID, theirID)
	u.GithubLogin = "them-gh"
	if err := o.f.store.Users().Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := o.f.store.Allowlist().Put(ctx, store.AllowlistEntry{
		OrgID: auth.LocalID, GithubID: 4242, GithubLogin: "them-gh", GithubName: "Them",
	}); err != nil {
		t.Fatal(err)
	}
	o.f.srv.allowlistChanged(ctx)
}

// assertUntouched checks that nothing of theirs was ended.
func (o *offboarded) assertUntouched(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, sess := range []store.Session{o.interactive, o.jobSession, o.heldSession} {
		if got := o.session(t, sess); !got.State.Live() {
			t.Errorf("their session %s was stopped", sess.Name)
		}
	}
	if got := o.job(t, o.running); got.State != store.JobRunning {
		t.Errorf("their job is %s", got.State)
	}
	if creds, _ := o.f.store.Credentials().ListByUser(ctx, auth.LocalID, theirID); len(creds) != 3 {
		t.Errorf("their credentials = %d, want all three", len(creds))
	}
	tokens, _ := o.f.store.Tokens().ListByUser(ctx, auth.LocalID, theirID)
	live := 0
	for _, tok := range tokens {
		if tok.RevokedAt == nil {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d live tokens, want their one", live)
	}
	if evs := o.offboardEvents(t); len(evs) != 0 {
		t.Errorf("audit rows = %+v, want none", evs)
	}
}

// Removing an account entry off-boards the person it covered, as
// removing an address does.
//
// @decision(DL-128) @decision(DL-132)
func TestRemovingAnAccountEntryOffboardsItsPerson(t *testing.T) {
	o := newOffboarded(t, nil)
	bindThem(t, o)
	if err := o.f.store.Allowlist().Delete(context.Background(), auth.LocalID, theirEmail); err != nil {
		t.Fatal(err)
	}
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/@Them-GH", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	o.assertEnded(t)
	evs := o.offboardEvents(t)
	if len(evs) != 1 || !strings.HasPrefix(evs[0].Detail, "@them-gh (them@example.com): 2 jobs cancelled") {
		t.Errorf("audit rows = %+v", evs)
	}
}

// A person covered by an address entry and an account entry loses
// nothing when one goes, and everything when the second does.
//
// @decision(DL-128) @decision(DL-132)
func TestAPersonCoveredTwiceLosesNothingUntilTheSecondEntryGoes(t *testing.T) {
	var revoked atomic.Int32
	o := newOffboarded(t, nil)
	real := o.f.srv.revokeSessions
	o.f.srv.revokeSessions = func(ctx context.Context, users []store.User) error {
		revoked.Add(int32(len(users)))
		return real(ctx, users)
	}
	bindThem(t, o)
	// The list says so, which is what the page's confirmation reads.
	also := map[string][]string{}
	for _, e := range allowlistOf(t, o.f) {
		also[e.Email+e.GithubLogin] = e.AlsoCoveredBy
	}
	if got := also[theirEmail]; len(got) != 1 || got[0] != "@them-gh" {
		t.Errorf("the address entry is also covered by %v", got)
	}
	if got := also["them-gh"]; len(got) != 1 || got[0] != theirEmail {
		t.Errorf("the account entry is also covered by %v", got)
	}
	if got := also["me@example.com"]; len(got) != 0 {
		t.Errorf("my entry is also covered by %v", got)
	}
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE of the address = %d: %s", resp.StatusCode, out)
	}
	o.assertUntouched(t)
	if revoked.Load() != 0 {
		t.Error("the sessions of a person still covered were ended")
	}
	if _, ok := o.f.srv.deps.Login.Allowlist.Lookup(4242, theirEmail); !ok {
		t.Fatal("the person is no longer admitted")
	}
	// The retry path does not reach them either: an entry covers them.
	if resp, _ := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a repeated delete of the address = %d, want 404 while an entry covers them", resp.StatusCode)
	}
	o.assertUntouched(t)

	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/@them-gh", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE of the account = %d: %s", resp.StatusCode, out)
	}
	o.assertEnded(t)
	if revoked.Load() != 1 {
		t.Errorf("sessions were ended for %d users, want 1", revoked.Load())
	}
}

// A partial removal of an account entry is retried by the same delete
// of "@login": the entry is gone, the user row still carries the login
// and no entry covers them. A login no row carries stays a 404.
//
// @decision(DL-128) @decision(DL-132)
func TestARetryByLoginOffboardsAgain(t *testing.T) {
	var fail atomic.Bool
	o := newOffboarded(t, func(d *Deps) { d.Store = failingStore{Store: d.Store, fail: &fail} })
	bindThem(t, o)
	if err := o.f.store.Allowlist().Delete(context.Background(), auth.LocalID, theirEmail); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/@them-gh", nil)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(out), "send the same delete again") {
		t.Fatalf("a failed step = %d: %s", resp.StatusCode, out)
	}
	if _, err := o.f.store.Allowlist().GetAccount(context.Background(), auth.LocalID, 4242); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the entry is still there: %v", err)
	}
	fail.Store(false)
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/@THEM-gh", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the retry = %d: %s", resp.StatusCode, out)
	}
	o.assertEnded(t)
	if evs := o.offboardEvents(t); len(evs) != 2 {
		t.Errorf("audit rows = %d, want one per attempt", len(evs))
	}
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/@nobody-here", nil); resp.StatusCode != http.StatusNotFound ||
		!strings.Contains(string(out), "@nobody-here is not on the allowlist") {
		t.Errorf("a login nobody carries = %d: %s", resp.StatusCode, out)
	}
}

// A half-finished removal of an account entry is retried by its id:
// form, which the retry matches against the user's account: the entry's
// login and the user row's can differ after a rename, and a retry by the
// entry's login would find no one.
//
// @decision(DL-128) @decision(DL-132)
func TestARetryByIDFindsARenamedPerson(t *testing.T) {
	var fail atomic.Bool
	o := newOffboarded(t, func(d *Deps) { d.Store = failingStore{Store: d.Store, fail: &fail} })
	bindThem(t, o)
	ctx := context.Background()
	if err := o.f.store.Allowlist().Delete(ctx, auth.LocalID, theirEmail); err != nil {
		t.Fatal(err)
	}
	u, _ := o.f.store.Users().Get(ctx, auth.LocalID, theirID)
	u.GithubLogin = "them-renamed"
	if err := o.f.store.Users().Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/id:4242", nil); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a failed step = %d: %s", resp.StatusCode, out)
	}
	fail.Store(false)
	if resp, _ := o.f.do(http.MethodDelete, "/api/v1/allowlist/@them-gh", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a retry by the entry's old login = %d, want 404", resp.StatusCode)
	}
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/id:4242", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the retry by id = %d: %s", resp.StatusCode, out)
	}
	o.assertEnded(t)
}

// An address entry that covers a row not yet bound to an account says
// so, since removing it off-boards that person even when their account
// has an entry: the account entry cannot admit an unbound row.
//
// @decision(DL-128)
func TestAnAddressEntryOverAnUnboundRowSaysSo(t *testing.T) {
	o := newOffboarded(t, nil)
	ctx := context.Background()
	if err := o.f.store.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: auth.LocalID, GithubID: 4242,
		GithubLogin: "them-gh"}); err != nil {
		t.Fatal(err)
	}
	unbound := map[string]bool{}
	for _, e := range allowlistOf(t, o.f) {
		unbound[e.Email+e.GithubLogin] = e.Unbound
	}
	if !unbound[theirEmail] || unbound["them-gh"] || unbound["me@example.com"] {
		t.Errorf("unbound = %v, want only their address entry", unbound)
	}
	// The behaviour it warns of: removing the address ends everything.
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	o.assertEnded(t)
	// Once bound, the address entry no longer says so.
	bindThem(t, o)
	for _, e := range allowlistOf(t, o.f) {
		if e.Unbound {
			t.Errorf("%+v is unbound after the bind", e)
		}
	}
}

// A delete never reaches outside the caller's organisation: a user of
// another org with the same address and login is neither matched nor
// ended.
//
// @decision(DL-128) @decision(DL-132)
func TestADeleteNeverActsOutsideTheCallersOrg(t *testing.T) {
	f, _ := accountFixture(t, map[string]bool{"me@example.com": true})
	ctx := context.Background()
	stranger := store.User{ID: store.NewID(time.Now()), OrgID: "other-org", Email: "x@example.com", GithubID: 31,
		GithubLogin: "x-gh"}
	if err := f.store.Users().Create(ctx, stranger); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Tokens().Create(ctx, store.APIToken{ID: store.NewID(time.Now()), OrgID: "other-org",
		UserID: stranger.ID, Hash: []byte{9}, Name: "cli"}); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"x@example.com", "@x-gh"} {
		if resp, out := f.do(http.MethodDelete, "/api/v1/allowlist/"+ref, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("DELETE %s = %d: %s", ref, resp.StatusCode, out)
		}
	}
	tokens, _ := f.store.Tokens().ListByUser(ctx, "other-org", stranger.ID)
	if len(tokens) != 1 || tokens[0].RevokedAt != nil {
		t.Errorf("another org's token = %+v", tokens)
	}
}
