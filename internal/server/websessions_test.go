package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/jimutt/spov/internal/auth"
)

// listWebSessions reads the caller's browser logins.
func (f *loginFixture) listWebSessions() []WebSessionView {
	f.t.Helper()
	resp := f.get("/api/v1/me/sessions")
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("listing web sessions = %d", resp.StatusCode)
	}
	var out struct {
		Sessions []WebSessionView `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Sessions
}

// secondBrowser returns a fixture view with its own cookie jar, so a
// test can hold two logins for the same user at once.
func (f *loginFixture) secondBrowser() *loginFixture {
	f.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		f.t.Fatal(err)
	}
	other := *f
	other.client = &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &other
}

// TestListWebSessionsShowsEveryLoginAndMarksTheCurrentOne.
//
// @decision(DL-007)
func TestListWebSessionsShowsEveryLoginAndMarksTheCurrentOne(t *testing.T) {
	first := newLoginFixture(t, "alice@example.com\n")
	first.doLogin("")

	second := first.secondBrowser()
	second.doLogin("")

	got := first.listWebSessions()
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want both logins: %+v", len(got), got)
	}
	current := 0
	for _, s := range got {
		if s.Current {
			current++
		}
		if s.ID == "" {
			t.Error("a session has no id")
		}
		if s.Expires.IsZero() {
			t.Errorf("session %s has no expiry", s.ID)
		}
	}
	if current != 1 {
		t.Errorf("%d sessions marked current, want exactly one", current)
	}
	// The two browsers see the same pair, from opposite sides.
	if other := second.listWebSessions(); len(other) != 2 {
		t.Errorf("the second browser sees %d sessions", len(other))
	}
}

// TestListWebSessionsNeverReturnsTheCookie: the handle identifies a row
// and must not be usable as the credential it names.
//
// @decision(DL-007)
func TestListWebSessionsNeverReturnsTheCookie(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.doLogin("")
	var token string
	for _, c := range resp.Cookies {
		if c.Name == "spov_session" {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no session cookie")
	}
	for _, s := range f.listWebSessions() {
		if s.ID == token {
			t.Error("the listing returned the session token itself")
		}
		if len(s.ID) >= len(token) {
			t.Errorf("the handle %q is as long as the token, so it may be one", s.ID)
		}
	}
}

// TestRevokeWebSessionEndsThatLoginAndNotTheOthers.
//
// @decision(DL-007)
func TestRevokeWebSessionEndsThatLoginAndNotTheOthers(t *testing.T) {
	first := newLoginFixture(t, "alice@example.com\n")
	first.doLogin("")
	second := first.secondBrowser()
	second.doLogin("")

	// The first browser revokes the other one.
	var target string
	for _, s := range first.listWebSessions() {
		if !s.Current {
			target = s.ID
		}
	}
	if target == "" {
		t.Fatal("no other session to revoke")
	}
	if code := first.delete("/api/v1/me/sessions/" + target); code != http.StatusNoContent {
		t.Fatalf("revoking = %d", code)
	}

	// The revoked browser is out...
	if resp := second.get("/api/v1/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the revoked browser = %d, want 401", resp.StatusCode)
	}
	// ...and the one that did the revoking is not.
	if resp := first.get("/api/v1/me"); resp.StatusCode != http.StatusOK {
		t.Errorf("the revoking browser = %d, want 200", resp.StatusCode)
	}
	if got := first.listWebSessions(); len(got) != 1 {
		t.Errorf("got %d sessions after revoking one", len(got))
	}
}

// TestRevokingTheCurrentSessionLogsYouOut.
//
// @decision(DL-007)
func TestRevokingTheCurrentSessionLogsYouOut(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")
	sessions := f.listWebSessions()
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	if code := f.delete("/api/v1/me/sessions/" + sessions[0].ID); code != http.StatusNoContent {
		t.Fatalf("revoking = %d", code)
	}
	if resp := f.get("/api/v1/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after revoking the current session = %d, want 401", resp.StatusCode)
	}
}

// TestRevokingAnotherUsersSessionIsNotFound rather than forbidden: the
// handle is unguessable, and "not yours" would confirm it exists.
//
// @decision(DL-007)
func TestRevokingAnotherUsersSessionIsNotFound(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\nbob@example.com\n")
	f.doLogin("")
	alice := f.listWebSessions()
	if len(alice) != 1 {
		t.Fatalf("got %d sessions", len(alice))
	}

	// Bob, in his own browser.
	bob := f.secondBrowser()
	bob.github.emails = []map[string]any{{"email": "bob@example.com", "primary": true, "verified": true}}
	// Another person is another GitHub account (DL-128).
	bob.github.id, bob.github.login = 2002, "bob"
	bob.doLogin("")

	if code := bob.delete("/api/v1/me/sessions/" + alice[0].ID); code != http.StatusNotFound {
		t.Errorf("bob revoking alice's session = %d, want 404", code)
	}
	// Alice is unaffected.
	if resp := f.get("/api/v1/me"); resp.StatusCode != http.StatusOK {
		t.Errorf("alice = %d after bob's attempt", resp.StatusCode)
	}
	// And bob's listing shows only his own.
	if got := bob.listWebSessions(); len(got) != 1 || got[0].ID == alice[0].ID {
		t.Errorf("bob sees %+v", got)
	}
}

// TestRevokingAnUnknownSessionIsNotFound.
//
// @decision(DL-007)
func TestRevokingAnUnknownSessionIsNotFound(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")
	if code := f.delete("/api/v1/me/sessions/nosuchhandle"); code != http.StatusNotFound {
		t.Errorf("revoking an unknown session = %d, want 404", code)
	}
}

// TestWebSessionRoutesNeedACredential.
//
// @decision(DL-007)
func TestWebSessionRoutesNeedACredential(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	if resp := f.get("/api/v1/me/sessions"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("listing without a session = %d, want 401", resp.StatusCode)
	}
	if code := f.delete("/api/v1/me/sessions/anything"); code != http.StatusUnauthorized {
		t.Errorf("revoking without a session = %d, want 401", code)
	}
}

// TestTokenID keeps the handle short enough not to be the token and
// stable for a given token.
//
// @decision(DL-007)
func TestTokenID(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyz012345"
	got := tokenID(token)
	if got != "abcdefghijkl" {
		t.Errorf("tokenID = %q", got)
	}
	if got != tokenID(token) {
		t.Error("tokenID is not stable")
	}
	// A short token is returned whole rather than panicking on the
	// slice; scs tokens are long, but nothing here should depend on it.
	if got := tokenID("abc"); got != "abc" {
		t.Errorf("tokenID(short) = %q", got)
	}
	if got := tokenID(""); got != "" {
		t.Errorf("tokenID(empty) = %q", got)
	}
}

// TestTheStoreNeverHoldsACookiesValue: the session store's key is a hash
// of the token, storeKey agrees with scs on it, and revoking by the
// listed handle still deletes the row.
//
// @decision(DL-007)
func TestTheStoreNeverHoldsACookiesValue(t *testing.T) {
	sessions := scs.New()
	store := memstore.New()
	sessions.Store = store
	sessions.HashTokenInStore = true
	l := &Login{Sessions: sessions}

	ctx, err := sessions.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	sessions.Put(ctx, auth.SessionKeyUserID, "u1")
	token, _, err := sessions.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, found, _ := store.Find(token); found {
		t.Fatal("the store holds the row under the cookie's value")
	}
	if _, found, _ := store.Find(l.storeKey(token)); !found {
		t.Fatal("storeKey is not the key scs stored the session under")
	}

	found, err := l.sessionsOf(context.Background(), "u1")
	if err != nil || len(found) != 1 {
		t.Fatalf("sessionsOf = %v, %v", found, err)
	}
	if found[0].id != tokenID(l.storeKey(token)) || strings.HasPrefix(token, found[0].id) {
		t.Errorf("handle %q is not a prefix of the store key alone", found[0].id)
	}
	ok, err := l.revoke(context.Background(), "u1", found[0].id)
	if err != nil || !ok {
		t.Fatalf("revoke = %v, %v", ok, err)
	}
	if _, found, _ := store.Find(l.storeKey(token)); found {
		t.Error("a revoked session is still in the store")
	}
}
