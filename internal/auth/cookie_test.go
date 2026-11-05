package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// fakeSessions is a SessionReader whose value does not depend on a real
// session manager.
type fakeSessions struct{ userID string }

func (f fakeSessions) GetString(context.Context, string) string { return f.userID }

// cookieFixture builds an authenticator over a memory store holding one
// allowed user.
func cookieFixture(t *testing.T, allowlist string) (*CookieAuthenticator, store.User, func(*testing.T, string)) {
	t.Helper()
	st := store.NewMemory()
	user := store.User{ID: "user-1", OrgID: "default", Email: "alice@example.com", Name: "Alice"}
	if err := st.Users().Create(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	list, setAllowlist := testAllowlist(t, allowlist)
	return &CookieAuthenticator{
		Sessions:  fakeSessions{userID: user.ID},
		Users:     st.Users(),
		Allowlist: list,
		OrgID:     "default",
	}, user, setAllowlist
}

// TestCookieAuthenticatorResolvesTheSessionToAPrincipal.
//
// @decision(DL-007)
func TestCookieAuthenticatorResolvesTheSessionToAPrincipal(t *testing.T) {
	a, user, _ := cookieFixture(t, "alice@example.com\n")
	p, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != user.ID || p.OrgID != "default" || p.Email != user.Email {
		t.Errorf("principal = %+v", p)
	}
	if p.Admin {
		t.Error("a plain allowlist entry produced an admin")
	}
}

// TestCookieAuthenticatorTakesAdminFromTheFile, not from the stored row:
// an operator edits the file, and the change must not wait for a login.
//
// @decision(DL-007)
func TestCookieAuthenticatorTakesAdminFromTheFile(t *testing.T) {
	a, _, setAllowlist := cookieFixture(t, "alice@example.com admin\n")
	p, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Admin {
		t.Error("an admin entry did not produce an admin principal")
	}

	// And revoking the flag takes effect on the next request, without a
	// new login.
	setAllowlist(t, "alice@example.com\n")
	if err := a.Allowlist.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err = a.Authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.Admin {
		t.Error("the principal is still an admin after the flag was removed")
	}
}

// TestCookieAuthenticatorRefusesARemovedUser: removing a line locks the
// user out on their next request, not at session expiry. This is the
// whole reason the allowlist is checked per request.
//
// @decision(DL-007)
func TestCookieAuthenticatorRefusesARemovedUser(t *testing.T) {
	a, _, setAllowlist := cookieFixture(t, "alice@example.com\nbob@example.com\n")
	if _, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)); err != nil {
		t.Fatal(err)
	}

	setAllowlist(t, "bob@example.com\n")
	if err := a.Allowlist.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if !errors.Is(err, ErrBadCredential) {
		t.Fatalf("err = %v, want ErrBadCredential", err)
	}
	// It is a rejection, not an absent credential: the middleware logs
	// the first and not the second.
	if errors.Is(err, ErrUnauthenticated) {
		t.Error("a removed user was reported as unauthenticated")
	}
}

// TestCookieAuthenticatorWithoutASessionIsUnauthenticated, so the chain
// moves on to the bearer token rather than rejecting the request.
//
// @decision(DL-007)
func TestCookieAuthenticatorWithoutASessionIsUnauthenticated(t *testing.T) {
	a, _, _ := cookieFixture(t, "alice@example.com\n")
	a.Sessions = fakeSessions{userID: ""}
	_, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

// TestCookieAuthenticatorRefusesASessionWhoseUserIsGone.
//
// @decision(DL-007)
func TestCookieAuthenticatorRefusesASessionWhoseUserIsGone(t *testing.T) {
	a, _, _ := cookieFixture(t, "alice@example.com\n")
	a.Sessions = fakeSessions{userID: "user-does-not-exist"}
	_, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if !errors.Is(err, ErrBadCredential) {
		t.Errorf("err = %v, want ErrBadCredential", err)
	}
}

// TestChainTriesEachAuthenticatorInOrder: only "no credential of this
// kind" moves on; a definite rejection stops the chain, so a bad cookie
// is not retried as a bearer token.
//
// @decision(DL-007)
func TestChainTriesEachAuthenticatorInOrder(t *testing.T) {
	cookie := StaticAuthenticator{Err: ErrUnauthenticated}
	token := StaticAuthenticator{Principal: Principal{UserID: "from-token"}}

	p, err := Chain{cookie, token}.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != "from-token" {
		t.Errorf("principal = %+v, want the token's", p)
	}

	// A rejection stops the chain: the second authenticator must not
	// get a chance to accept what the first refused.
	rejecting := StaticAuthenticator{Err: ErrBadCredential}
	if _, err := (Chain{rejecting, token}).Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrBadCredential) {
		t.Errorf("err = %v, want the first rejection", err)
	}

	// An empty chain, and one where nobody has a credential, are both
	// unauthenticated rather than a panic or a zero principal.
	if _, err := (Chain{}).Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("an empty chain gave %v", err)
	}
	if _, err := (Chain{cookie, cookie}).Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a chain with no credential gave %v", err)
	}
}

// TestChainPrefersTheCookieOverTheToken, which is the order the hosted
// server composes them in.
//
// @decision(DL-007)
func TestChainPrefersTheCookieOverTheToken(t *testing.T) {
	cookie := StaticAuthenticator{Principal: Principal{UserID: "from-cookie"}}
	token := StaticAuthenticator{Principal: Principal{UserID: "from-token"}}
	p, err := Chain{cookie, token}.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != "from-cookie" {
		t.Errorf("principal = %+v", p)
	}
}

// TestCookieRejectionNamesTheAddressAndNotTheSession: the middleware
// logs a rejection, so it must say who was refused without carrying the
// session token that would let a reader of the log become them.
//
// @decision(DL-007) @decision(DL-006)
func TestCookieRejectionNamesTheAddressAndNotTheSession(t *testing.T) {
	a, _, _ := cookieFixture(t, "bob@example.com\n")
	const token = "a-secret-session-token"
	a.Sessions = tokenSessions{userID: "user-1", token: token}

	_, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if err == nil {
		t.Fatal("a user off the allowlist was accepted")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the rejection carries the session token: %v", err)
	}
	// It still says which address to add, which is the whole point of
	// logging it.
	if !strings.Contains(err.Error(), "alice@example.com") {
		t.Errorf("the rejection does not name the address: %v", err)
	}
}

// tokenSessions is a SessionReader that would leak its token if the
// authenticator ever read one into an error.
type tokenSessions struct{ userID, token string }

func (f tokenSessions) GetString(_ context.Context, key string) string {
	if key == SessionKeyUserID {
		return f.userID
	}
	return f.token
}
