package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// allowlistFixture is an admin caller over an org whose allowlist holds
// the given entries. The caller's own address is me@example.com, which
// several of the guards below turn on.
func allowlistFixture(t *testing.T, admin bool, entries map[string]bool) *apiFixture {
	t.Helper()
	return allowlistFixtureWith(t, admin, entries, nil)
}

// allowlistFixtureWith is allowlistFixture letting a test adjust Deps.
func allowlistFixtureWith(t *testing.T, admin bool, entries map[string]bool, adjust func(*Deps)) *apiFixture {
	t.Helper()
	f := newAPIFixtureWith(t, func(d *Deps) {
		if adjust != nil {
			adjust(d)
		}
		// A fake, so no test asks the real GitHub.
		//
		// @decision(DL-128)
		if d.GitHubAccounts == nil {
			d.GitHubAccounts = newFakeGitHubAccounts().lookup
		}
		ctx := context.Background()
		for email, isAdmin := range entries {
			if err := d.Store.Allowlist().Put(ctx, store.AllowlistEntry{
				OrgID: auth.LocalID, Email: email, Admin: isAdmin,
			}); err != nil {
				t.Fatal(err)
			}
		}
		list, err := auth.NewAllowlist(ctx,
			auth.StoreAllowlist(d.Store.Allowlist(), auth.LocalID), auth.AllowlistRefresh, nil)
		if err != nil {
			t.Fatal(err)
		}
		// A Login is what makes this a server with an allowlist. The
		// session manager is real, because Handler wraps every request
		// in it — and because a delete has to be able to end sessions.
		sessions := scs.New()
		sessions.Store = memstore.New()
		sessions.HashTokenInStore = true
		d.Login = &Login{
			Sessions: sessions, Allowlist: list, Users: d.Store.Users(), Entries: d.Store.Allowlist(), OrgID: auth.LocalID,
		}
	})
	f.auth.inner = auth.StaticAuthenticator{
		Principal: auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID, Email: "me@example.com", Admin: admin},
	}
	return f
}

func allowlistOf(t *testing.T, f *apiFixture) []AllowlistEntryView {
	t.Helper()
	resp, out := f.do(http.MethodGet, "/api/v1/allowlist", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET allowlist = %d: %s", resp.StatusCode, out)
	}
	var body struct {
		Entries []AllowlistEntryView `json:"entries"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	return body.Entries
}

// The allowlist is the deployment's access control, so reading or
// changing it needs an administrator — the same rule DL-009 put on the
// users endpoints.
//
// @decision(DL-007) @decision(DL-009)
func TestTheAllowlistNeedsAnAdmin(t *testing.T) {
	f := allowlistFixture(t, false, map[string]bool{"me@example.com": true})
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/allowlist", nil},
		{http.MethodPut, "/api/v1/allowlist/new@example.com", map[string]any{"admin": true}},
		{http.MethodDelete, "/api/v1/allowlist/me@example.com", nil},
	} {
		resp, out := f.do(tc.method, tc.path, tc.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s as a non-admin = %d, want 403: %s", tc.method, tc.path, resp.StatusCode, out)
		}
	}
}

// A local server has one fixed user and no login, so an allowlist would
// govern nothing. Saying so beats accepting entries nothing consults.
//
// @decision(DL-007)
func TestAServerWithNoLoginHasNoAllowlist(t *testing.T) {
	f := newAPIFixture(t)
	f.auth.inner = auth.StaticAuthenticator{
		Principal: auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID, Admin: true},
	}
	resp, out := f.do(http.MethodGet, "/api/v1/allowlist", nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("GET allowlist on a local server = %d, want 501: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "no browser login") {
		t.Errorf("the answer does not say why: %s", out)
	}
}

// @decision(DL-007)
func TestAddingAndListingEntries(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true})

	resp, out := f.do(http.MethodPut, "/api/v1/allowlist/New@Example.COM", map[string]any{"admin": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, out)
	}
	var got AllowlistEntryView
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Email != "new@example.com" {
		t.Errorf("stored as %q, want it lower-cased", got.Email)
	}
	if got.Admin {
		t.Error("a plain entry came back as an admin")
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at is missing")
	}

	entries := allowlistOf(t, f)
	if len(entries) != 2 || entries[0].Email != "me@example.com" || entries[1].Email != "new@example.com" {
		t.Fatalf("entries = %+v", entries)
	}

	// A second PUT edits rather than duplicating, and keeps the date.
	resp, out = f.do(http.MethodPut, "/api/v1/allowlist/new@example.com", map[string]any{"admin": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second PUT = %d: %s", resp.StatusCode, out)
	}
	var second AllowlistEntryView
	if err := json.Unmarshal(out, &second); err != nil {
		t.Fatal(err)
	}
	if !second.Admin {
		t.Error("the second PUT did not grant admin")
	}
	if !second.CreatedAt.Equal(got.CreatedAt) {
		t.Errorf("created_at moved from %v to %v", got.CreatedAt, second.CreatedAt)
	}
	if entries := allowlistOf(t, f); len(entries) != 2 {
		t.Errorf("a second PUT made %d entries", len(entries))
	}
}

// @decision(DL-007)
func TestAnAddressThatIsNotOneIsRefused(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true})
	for _, bad := range []string{"alice", "a@b@c", "Alice%20%3Calice@example.com%3E"} {
		resp, out := f.do(http.MethodPut, "/api/v1/allowlist/"+bad, map[string]any{"admin": false})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("PUT %q = %d, want 422: %s", bad, resp.StatusCode, out)
		}
		if resp, _ := f.do(http.MethodDelete, "/api/v1/allowlist/"+bad, nil); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("DELETE %q = %d, want 422", bad, resp.StatusCode)
		}
	}
	if entries := allowlistOf(t, f); len(entries) != 1 {
		t.Errorf("a refused address was stored anyway: %+v", entries)
	}
}

// @decision(DL-007)
func TestRemovingAnEntry(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true, "them@example.com": false})
	resp, out := f.do(http.MethodDelete, "/api/v1/allowlist/Them@Example.com", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	if entries := allowlistOf(t, f); len(entries) != 1 {
		t.Errorf("entries = %+v, want only the caller", entries)
	}
	// Removing it twice is a 404, not a silent success: an admin who
	// mistypes an address should be told, not reassured.
	if resp, _ := f.do(http.MethodDelete, "/api/v1/allowlist/them@example.com", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", resp.StatusCode)
	}
}

// TestTheOrgCannotBeLeftWithoutAnAdmin pins that no request can leave
// the org with zero admin entries, which keeps the re-seed unreachable.
//
// @decision(DL-007)
func TestTheOrgCannotBeLeftWithoutAnAdmin(t *testing.T) {
	// The caller is the only admin, so every route to zero runs
	// through them.
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true, "them@example.com": false})

	resp, out := f.do(http.MethodPut, "/api/v1/allowlist/me@example.com", map[string]any{"admin": false})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("self-demotion = %d, want 422: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "another admin") {
		t.Errorf("the refusal does not say what to do: %s", out)
	}
	resp, out = f.do(http.MethodDelete, "/api/v1/allowlist/me@example.com", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("self-deletion = %d, want 422: %s", resp.StatusCode, out)
	}

	// Nothing changed, so the caller is still an admin.
	entries := allowlistOf(t, f)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	for _, e := range entries {
		if e.Email == "me@example.com" && !e.Admin {
			t.Error("the caller was demoted by a refused request")
		}
	}
}

// Self-demotion is refused even when the org would keep an admin, since
// an admin who removes their own rights cannot restore them.
//
// @decision(DL-007) @decision(DL-009)
func TestSelfDemotionIsRefusedEvenWithAnotherAdmin(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true, "other@example.com": true})
	resp, out := f.do(http.MethodPut, "/api/v1/allowlist/me@example.com", map[string]any{"admin": false})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("self-demotion with a spare admin = %d, want 422: %s", resp.StatusCode, out)
	}
	// But demoting the *other* admin is allowed, since one remains.
	if resp, out := f.do(http.MethodPut, "/api/v1/allowlist/other@example.com",
		map[string]any{"admin": false}); resp.StatusCode != http.StatusOK {
		t.Fatalf("demoting another admin = %d, want 200: %s", resp.StatusCode, out)
	}
	// And now that only the caller is left, they cannot go either.
	if resp, _ := f.do(http.MethodDelete, "/api/v1/allowlist/other@example.com", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("deleting a non-admin = %d", resp.StatusCode)
	}
}

// TestAStaleAdminCannotWrite pins that an admin removed from the store
// but still an admin in the cache can read but cannot write, and in
// particular cannot re-instate themselves.
//
// @decision(DL-007)
func TestAStaleAdminCannotWrite(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true, "other@example.com": true})
	ctx := context.Background()
	// Removed in the store, still an admin in this replica's cache —
	// which is what the principal reads.
	if err := f.store.Allowlist().Delete(ctx, auth.LocalID, "me@example.com"); err != nil {
		t.Fatal(err)
	}
	if admin, ok := f.srv.deps.Login.Allowlist.Lookup(0, "me@example.com"); !ok || !admin {
		t.Fatal("the cache already caught up, so this proves nothing")
	}

	// Reading is still allowed for the length of the interval: that is
	// the bound DL-007 chose.
	if resp, out := f.do(http.MethodGet, "/api/v1/allowlist", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET as a stale admin = %d, want 200: %s", resp.StatusCode, out)
	}
	// Writing is not.
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/api/v1/allowlist/me@example.com", map[string]any{"admin": true}},
		{http.MethodDelete, "/api/v1/allowlist/other@example.com", nil},
	} {
		resp, out := f.do(tc.method, tc.path, tc.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s as a stale admin = %d, want 403: %s", tc.method, resp.StatusCode, out)
		}
	}
	// And in particular they did not put themselves back.
	if _, err := f.store.Allowlist().Get(ctx, auth.LocalID, "me@example.com"); err == nil {
		t.Error("a removed admin re-instated themselves inside the refresh window")
	}
}

// TestGuardLastAdmin calls the guard directly: requireLiveAdmin and the
// self rule mean no request can present it with a count of one.
//
// @decision(DL-007) @decision(DL-128)
func TestGuardLastAdmin(t *testing.T) {
	addr := func(email string, admin bool) store.AllowlistEntry {
		return store.AllowlistEntry{OrgID: auth.LocalID, Email: email, Admin: admin}
	}
	acct := func(id int64, admin bool) store.AllowlistEntry {
		return store.AllowlistEntry{OrgID: auth.LocalID, GithubID: id, Admin: admin}
	}
	onlyMe := []store.AllowlistEntry{addr("me@example.com", true)}
	for _, tc := range []struct {
		name   string
		target store.AllowlistEntry
		mine   []store.AllowlistEntry
		admins int
		refuse string
	}{
		{"the last administrator", addr("last@example.com", true), onlyMe, 1, "last administrator"},
		{"no administrator at all", addr("last@example.com", true), onlyMe, 0, "last administrator"},
		{"oneself, even with others", addr("me@example.com", true), onlyMe, 5, "ask another admin"},
		{"oneself in another case", addr("Me@Example.COM", true), onlyMe, 5, "ask another admin"},
		{"somebody else, with others", addr("them@example.com", true), onlyMe, 2, ""},
		// Covered by two admin entries, one of them may go.
		{"one of two of one's own admin entries", acct(7, true),
			[]store.AllowlistEntry{addr("me@example.com", true), acct(7, true)}, 2, ""},
		// A plain entry beside it does not keep one an admin.
		{"one's admin entry beside a plain one", addr("me@example.com", true),
			[]store.AllowlistEntry{addr("me@example.com", true), acct(7, false)}, 3, "ask another admin"},
		{"one's own account entry, alone", acct(7, true), []store.AllowlistEntry{acct(7, true)}, 4, "ask another admin"},
		{"somebody else's account entry", acct(8, true), []store.AllowlistEntry{acct(7, true)}, 2, ""},
		{"an account entry is not an address entry", acct(7, true), onlyMe, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := guardLastAdmin(losesOwnAdmin(tc.target, tc.mine), tc.admins, "remove yourself")
			if tc.refuse == "" {
				if err != nil {
					t.Fatalf("refused a safe change: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the change was allowed")
			}
			if !strings.Contains(err.Error(), tc.refuse) {
				t.Errorf("error %q does not say %q", err, tc.refuse)
			}
		})
	}
}

// An admin of another org, as the shared bearer token (OrgID "local")
// is on a hosted server, is refused rather than scoped to its own org.
//
// @decision(DL-007)
func TestACredentialFromAnotherOrgCannotEditTheAllowlist(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true})
	// The allowlist governs the fixture's org; this caller is an admin
	// of a different one, exactly as the shared bearer is on a hosted
	// server.
	f.auth.inner = auth.StaticAuthenticator{
		Principal: auth.Principal{UserID: "op", OrgID: "another-org", Email: "op@example.com", Admin: true},
	}
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/allowlist", nil},
		{http.MethodPut, "/api/v1/allowlist/new@example.com", map[string]any{"admin": true}},
		{http.MethodDelete, "/api/v1/allowlist/me@example.com", nil},
	} {
		resp, out := f.do(tc.method, tc.path, tc.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s from another org = %d, want 403: %s", tc.method, resp.StatusCode, out)
		}
		if !strings.Contains(string(out), "spov login") {
			t.Errorf("%s does not name a credential that works: %s", tc.method, out)
		}
	}
	// Nothing was written into the caller's own org either.
	if entries, err := f.store.Allowlist().List(context.Background(), "another-org"); err != nil || len(entries) != 0 {
		t.Errorf("entries in the caller's org = %+v, %v", entries, err)
	}
}

// TestAChangeIsVisibleToThisReplicaImmediately. Other replicas catch up
// on their own timer, which is the bound DL-007 is about; the admin who
// made the change should not have to wait for their own.
//
// @decision(DL-007)
func TestAChangeIsVisibleToThisReplicaImmediately(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true})
	list := f.srv.deps.Login.Allowlist
	if _, ok := list.Lookup(0, "new@example.com"); ok {
		t.Fatal("the cache already had the address")
	}
	if resp, out := f.do(http.MethodPut, "/api/v1/allowlist/new@example.com",
		map[string]any{"admin": false}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, out)
	}
	if _, ok := list.Lookup(0, "new@example.com"); !ok {
		t.Error("the cache was not refreshed after a change")
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/allowlist/new@example.com", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatal("DELETE failed")
	}
	if _, ok := list.Lookup(0, "new@example.com"); ok {
		t.Error("the cache still allows a deleted address")
	}
}

// TestDeletingAnEntryEndsThatPersonsSessions pins that a re-added
// address does not resume a session from before it was removed.
//
// @decision(DL-007)
func TestDeletingAnEntryEndsThatPersonsSessions(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true, "them@example.com": false})
	ctx := context.Background()
	if err := f.store.Users().Create(ctx, store.User{
		ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", OrgID: auth.LocalID, Email: "them@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	revoked := make(chan string, 4)
	real := f.srv.revokeSessions
	f.srv.revokeSessions = func(ctx context.Context, users []store.User) error {
		for _, u := range users {
			revoked <- u.Email
		}
		// Still the real one, so this test also proves the wiring
		// survives a Login whose session manager is absent.
		return real(ctx, users)
	}

	if resp, out := f.do(http.MethodDelete, "/api/v1/allowlist/them@example.com", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	select {
	case got := <-revoked:
		if got != "them@example.com" {
			t.Errorf("revoked sessions for %q", got)
		}
	default:
		t.Error("deleting an entry did not end that person's sessions")
	}
}

// A failure to end sessions must not fail the delete: the entry is
// already gone, which is what was asked for, and the session is inert
// within one refresh interval. It is still reported.
//
// @decision(DL-007)
func TestAFailedRevocationWarnsRatherThanFailingTheDelete(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true, "them@example.com": false})
	// Sessions are ended for a person who signed in, so there is a row.
	if err := f.store.Users().Create(context.Background(), store.User{
		ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", OrgID: auth.LocalID, Email: "them@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	f.srv.revokeSessions = func(context.Context, []store.User) error {
		return errors.New("the session store is unreachable")
	}
	var warnings []string
	f.srv.deps.Warn = func(m string) { warnings = append(warnings, m) }

	if resp, out := f.do(http.MethodDelete, "/api/v1/allowlist/them@example.com", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	if entries := allowlistOf(t, f); len(entries) != 1 {
		t.Errorf("the entry survived a failed revocation: %+v", entries)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "them@example.com") {
			found = true
		}
	}
	if !found {
		t.Errorf("a failed revocation was silent: %v", warnings)
	}
}

// An e-mail is one path segment, and a caller who sends something that
// is not must not reach a different endpoint.
//
// @decision(DL-007)
func TestAnEmailWithASlashDoesNotEscapeItsSegment(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true})
	resp, _ := f.do(http.MethodDelete, "/api/v1/allowlist/"+url.PathEscape("../users"), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusNotFound {
		t.Errorf("a path segment that is not an address = %d", resp.StatusCode)
	}
	if entries := allowlistOf(t, f); len(entries) != 1 {
		t.Errorf("entries = %+v", entries)
	}
}

// orderedAllowlist records the order of the calls a handler makes.
type orderedAllowlist struct {
	store.AllowlistStore
	mu    *sync.Mutex
	calls *[]string
}

func (o orderedAllowlist) record(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	*o.calls = append(*o.calls, name)
}

func (o orderedAllowlist) Get(ctx context.Context, orgID, email string) (store.AllowlistEntry, error) {
	o.record("Get")
	return o.AllowlistStore.Get(ctx, orgID, email)
}

func (o orderedAllowlist) GetAccount(ctx context.Context, orgID string, id int64) (store.AllowlistEntry, error) {
	o.record("Get")
	return o.AllowlistStore.GetAccount(ctx, orgID, id)
}

func (o orderedAllowlist) ByLogin(ctx context.Context, orgID, login string) ([]store.AllowlistEntry, error) {
	o.record("Get")
	return o.AllowlistStore.ByLogin(ctx, orgID, login)
}

func (o orderedAllowlist) CountAdmins(ctx context.Context, orgID string) (int, error) {
	o.record("CountAdmins")
	return o.AllowlistStore.CountAdmins(ctx, orgID)
}

// orderedStore hands out an orderedAllowlist, inside a Tx as well.
type orderedStore struct {
	store.Store
	mu    *sync.Mutex
	calls *[]string
	// inTx is the view a transaction hands out. Only reads made inside
	// one are recorded: an account entry is resolved before the
	// transaction, which takes no lock and decides nothing.
	inTx bool
}

func (o orderedStore) Allowlist() store.AllowlistStore {
	if !o.inTx {
		return o.Store.Allowlist()
	}
	return orderedAllowlist{AllowlistStore: o.Store.Allowlist(), mu: o.mu, calls: o.calls}
}

func (o orderedStore) Tx(ctx context.Context, fn func(store.Store) error) error {
	return o.Store.Tx(ctx, func(tx store.Store) error {
		return fn(orderedStore{Store: tx, mu: o.mu, calls: o.calls, inTx: true})
	})
}

// TestTheAdminCountIsTakenBeforeTheEntryIsRead pins that a mutating
// handler calls CountAdmins, which takes the advisory lock, before it
// reads the entry. The memory store serialises on a mutex and cannot
// show the race, so the order is asserted.
//
// @decision(DL-007) @decision(DL-128)
func TestTheAdminCountIsTakenBeforeTheEntryIsRead(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"put", http.MethodPut, "/api/v1/allowlist/them@example.com", map[string]any{"admin": true}},
		{"delete", http.MethodDelete, "/api/v1/allowlist/them@example.com", nil},
		{"put an account", http.MethodPut, "/api/v1/allowlist/@octo", map[string]any{"admin": true}},
		{"delete an account", http.MethodDelete, "/api/v1/allowlist/@octo", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := allowlistFixture(t, true, map[string]bool{"me@example.com": true, "them@example.com": false})
			if err := f.store.Allowlist().Put(context.Background(), store.AllowlistEntry{
				OrgID: auth.LocalID, GithubID: 77, GithubLogin: "octo",
			}); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var calls []string
			f.srv.deps.Store = orderedStore{Store: f.srv.deps.Store, mu: &mu, calls: &calls}

			if resp, out := f.do(tc.method, tc.path, tc.body); resp.StatusCode >= 400 {
				t.Fatalf("%s = %d: %s", tc.method, resp.StatusCode, out)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) < 2 || calls[0] != "CountAdmins" || calls[1] != "Get" {
				t.Errorf("calls = %v, want CountAdmins before Get: the entry must be read under the lock", calls)
			}
		})
	}
}
