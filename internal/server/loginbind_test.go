package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// oldRowWithToken is a user row from before accounts were recorded, for
// alice@example.com, holding a CLI token and a stored credential, which
// no entry covers any longer.
func oldRowWithToken(t *testing.T, f *loginFixture) (store.User, string) {
	t.Helper()
	ctx := context.Background()
	old := store.User{ID: store.NewID(time.Now()), OrgID: DefaultOrgID, Email: "alice@example.com", Name: "Alice"}
	if err := f.store.Users().Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if err := f.store.Tokens().Create(ctx, store.APIToken{ID: store.NewID(time.Now()), OrgID: DefaultOrgID,
		UserID: old.ID, Hash: auth.HashToken(tok), Name: "laptop", ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Credentials().Put(ctx, store.UserCredential{ID: store.NewID(time.Now()), OrgID: DefaultOrgID,
		UserID: old.ID, Platform: "github", Ciphertext: []byte("sealed"), Nonce: []byte("n")}); err != nil {
		t.Fatal(err)
	}
	return old, tok
}

// tokenWorks reports whether a CLI token authenticates against the
// fixture's allowlist as it stands.
func tokenWorks(t *testing.T, f *loginFixture, tok string) bool {
	t.Helper()
	a := &auth.TokenAuthenticator{Tokens: f.store.Tokens(), Users: f.store.Users(), Allowlist: f.login.Allowlist}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	_, err := a.Authenticate(r)
	if err != nil && !errors.Is(err, auth.ErrBadCredential) {
		t.Fatal(err)
	}
	return err == nil
}

// A person admitted only by an account entry does not take over an
// earlier user's row by address: the row is bound only when an address
// entry allows that address, and no second row is made for it either.
// The earlier user's inert CLI token stays inert.
//
// @decision(DL-128)
func TestAnAccountEntryDoesNotTakeOverAnEarlierUsersRowByAddress(t *testing.T) {
	f := newLoginFixture(t, "@octocat admin\n")
	old, tok := oldRowWithToken(t, f)
	if tokenWorks(t, f, tok) {
		t.Fatal("the earlier user's token works before anything happened")
	}
	resp := f.doLogin("")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the sign-in = %d: %s", resp.StatusCode, resp.Body)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "recorded for an earlier user") || !strings.Contains(body, "adding the address") {
		t.Errorf("the page does not say why or what to do: %s", body)
	}
	users, _ := f.store.Users().List(context.Background(), DefaultOrgID)
	if len(users) != 1 || users[0].ID != old.ID || users[0].GithubID != 0 {
		t.Errorf("users = %+v, want only the earlier row, unbound", users)
	}
	if tokenWorks(t, f, tok) {
		t.Error("the earlier user's token authenticates again")
	}
	if got := f.get("/api/v1/me"); got.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after the refusal = %d", got.StatusCode)
	}

	// Allowing the address is what connects the account to the row,
	// as the address entry would have admitted it all along.
	f.setAllowlist(map[string]bool{"#583231": true, "alice@example.com": false})
	if resp := f.doLogin(""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the sign-in with the address allowed = %d: %s", resp.StatusCode, resp.Body)
	}
	if bound := f.onlyUser(); bound.ID != old.ID || bound.GithubID != 583231 {
		t.Errorf("row = %+v, want the earlier row bound", bound)
	}
	if !tokenWorks(t, f, tok) {
		t.Error("the bound row's token does not work under the address entry")
	}
}

// The address entry that allows a bind is read from the store at the
// bind: one the cache still holds but the store no longer does binds
// nothing.
//
// @decision(DL-128)
func TestABindNeedsTheAddressEntryAtThatMoment(t *testing.T) {
	f := newLoginFixture(t, "@octocat admin\nalice@example.com\n")
	old, _ := oldRowWithToken(t, f)
	if _, ok := f.login.Allowlist.Lookup(0, "alice@example.com"); !ok {
		t.Fatal("the cache does not hold the address entry, so this proves nothing")
	}
	if err := f.store.Allowlist().Delete(context.Background(), DefaultOrgID, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if resp := f.doLogin(""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a bind with no address entry in the store = %d: %s", resp.StatusCode, resp.Body)
	}
	if got, _ := f.store.Users().Get(context.Background(), DefaultOrgID, old.ID); got.GithubID != 0 {
		t.Errorf("the row was bound: %+v", got)
	}
}
