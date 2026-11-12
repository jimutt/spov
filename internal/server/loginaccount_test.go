package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// user is the one row the login fixture's org holds.
func (f *loginFixture) onlyUser() store.User {
	f.t.Helper()
	users, err := f.store.Users().List(context.Background(), DefaultOrgID)
	if err != nil || len(users) != 1 {
		f.t.Fatalf("users = %+v, %v; want one", users, err)
	}
	return users[0]
}

func (f *loginFixture) primary(email string) {
	f.github.emails = []map[string]any{{"email": email, "primary": true, "verified": true}}
}

// An account entry admits its account whatever its address, and the row
// is found by the account: an address that changed at GitHub keeps the
// same user, whose address follows.
//
// @decision(DL-128)
func TestAnAccountEntryAdmitsItsAccountWhateverItsAddress(t *testing.T) {
	f := newLoginFixture(t, "@octocat admin\n")
	if resp := f.doLogin(""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the callback = %d: %s", resp.StatusCode, resp.Body)
	}
	first := f.onlyUser()
	if first.GithubID != 583231 || first.Email != "alice@example.com" {
		t.Fatalf("row = %+v", first)
	}
	f.logout()
	f.primary("alice@new.example")
	if resp := f.doLogin(""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a sign-in with a new address = %d: %s", resp.StatusCode, resp.Body)
	}
	again := f.onlyUser()
	if again.ID != first.ID || again.Email != "alice@new.example" {
		t.Errorf("row = %+v, want the same row with the new address", again)
	}
	var me struct {
		Admin bool `json:"admin"`
	}
	resp := f.get("/api/v1/me")
	if err := json.Unmarshal(resp.Body, &me); err != nil || resp.StatusCode != http.StatusOK || !me.Admin {
		t.Errorf("me = %d %s, want an admin by the account entry", resp.StatusCode, resp.Body)
	}
}

// A row from before accounts were recorded is bound to the account that
// next proves its address, and no second account can then claim it.
//
// @decision(DL-128)
func TestAnOldRowIsBoundOnceToTheAccountThatProvesItsAddress(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	ctx := context.Background()
	old := store.User{ID: store.NewID(time.Now()), OrgID: DefaultOrgID, Email: "alice@example.com", Name: "Old"}
	if err := f.store.Users().Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	if resp := f.doLogin(""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the callback = %d: %s", resp.StatusCode, resp.Body)
	}
	bound := f.onlyUser()
	if bound.ID != old.ID || bound.GithubID != 583231 {
		t.Fatalf("row = %+v, want the old row bound to the account", bound)
	}
	f.logout()

	// Another account now holds the same verified primary address.
	f.github.id, f.github.login = 999, "mallory"
	resp := f.doLogin("")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(resp.Body), "belongs to another account") {
		t.Fatalf("a second account = %d: %s", resp.StatusCode, resp.Body)
	}
	if after := f.onlyUser(); after.ID != old.ID || after.GithubID != 583231 {
		t.Errorf("the row moved: %+v", after)
	}
	if got := f.get("/api/v1/me"); got.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after the refusal = %d", got.StatusCode)
	}
}

// An address recorded on a row of another account never moves to the
// account signing in, even one an account entry admits.
//
// @decision(DL-128)
func TestAnAddressOnAnotherAccountsRowNeverMoves(t *testing.T) {
	f := newLoginFixture(t, "@octocat\n")
	ctx := context.Background()
	other := store.User{ID: store.NewID(time.Now()), OrgID: DefaultOrgID, Email: "shared@example.com", GithubID: 5}
	if err := f.store.Users().Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	f.doLogin("")
	mine := f.onlyUserExcept(other.ID)
	f.logout()

	f.primary("shared@example.com")
	resp := f.doLogin("")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(resp.Body), "belongs to another account") {
		t.Fatalf("a sign-in needing another account's address = %d: %s", resp.StatusCode, resp.Body)
	}
	if got, _ := f.store.Users().Get(ctx, DefaultOrgID, mine.ID); got.Email != "alice@example.com" {
		t.Errorf("the signing-in row took the address: %+v", got)
	}
	if got, _ := f.store.Users().Get(ctx, DefaultOrgID, other.ID); got.GithubID != 5 || got.Email != "shared@example.com" {
		t.Errorf("the other row changed: %+v", got)
	}
}

func (f *loginFixture) onlyUserExcept(id string) store.User {
	f.t.Helper()
	users, _ := f.store.Users().List(context.Background(), DefaultOrgID)
	var out []store.User
	for _, u := range users {
		if u.ID != id {
			out = append(out, u)
		}
	}
	if len(out) != 1 {
		f.t.Fatalf("users = %+v", out)
	}
	return out[0]
}

// The entry is the numeric ID: a renamed account keeps its entry, whose
// label follows, and an account that registers the released login is
// not admitted by it.
//
// @decision(DL-128)
func TestARenamedOrReregisteredLoginNeverMovesAnEntry(t *testing.T) {
	f := newLoginFixture(t, "@octocat\n")
	f.github.login = "octo-renamed"
	if resp := f.doLogin(""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the renamed account = %d: %s", resp.StatusCode, resp.Body)
	}
	entry, err := f.store.Allowlist().GetAccount(context.Background(), DefaultOrgID, 583231)
	if err != nil || entry.GithubLogin != "octo-renamed" {
		t.Errorf("entry = %+v, %v; want its label refreshed", entry, err)
	}
	f.logout()

	f.github.id, f.github.login = 31337, "octocat"
	f.primary("newcomer@example.com")
	resp := f.doLogin("")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the new holder of the login = %d: %s", resp.StatusCode, resp.Body)
	}
	if body := string(resp.Body); !strings.Contains(body, "@octocat (newcomer@example.com)") {
		t.Errorf("the refusal does not name the account and the address: %s", body)
	}
}

// An address entry admits a row not yet bound to an account, and its
// sign-in binds it; an account entry cannot admit an unbound row.
//
// @decision(DL-128)
func TestAnAccountEntryAdmitsOnlyABoundRow(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")
	u := f.onlyUser()
	if u.GithubID != 583231 {
		t.Fatalf("the row was not bound: %+v", u)
	}
	// The same account, by its entry alone, is admitted on its next
	// request.
	f.setAllowlist(map[string]bool{"#583231": false})
	if got := f.get("/api/v1/me"); got.StatusCode != http.StatusOK {
		t.Errorf("me with only the account entry = %d", got.StatusCode)
	}
	// Another account's entry does not admit it.
	f.setAllowlist(map[string]bool{"#1": true})
	if got := f.get("/api/v1/me"); got.StatusCode != http.StatusUnauthorized {
		t.Errorf("me with another account's entry = %d", got.StatusCode)
	}
}

// The refusal page names both the account and the address, escaped.
//
// @decision(DL-128)
func TestTheRefusalPageNamesTheAccountAndTheAddress(t *testing.T) {
	f := newLoginFixture(t, "someone-else@example.com\n")
	f.github.name = "<script>alert(1)</script>"
	resp := f.doLogin("")
	body := string(resp.Body)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "@octocat (alice@example.com)") {
		t.Fatalf("the refusal = %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "<script>") {
		t.Errorf("the page carries markup from GitHub: %s", body)
	}
}

// A profile whose login is not a login's shape is not signed in: the
// login is printed on pages and in the log.
//
// @decision(DL-128)
func TestAProfileWithAMalformedLoginIsRefused(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.github.login = "<b>x</b>"
	if resp := f.doLogin(""); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("the callback = %d, want 502: %s", resp.StatusCode, resp.Body)
	}
	if users, _ := f.store.Users().List(context.Background(), DefaultOrgID); len(users) != 0 {
		t.Errorf("a refused profile created %+v", users)
	}
}
