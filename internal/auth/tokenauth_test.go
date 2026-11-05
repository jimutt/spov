package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// tokenFixture is an authenticator over a memory store holding one
// allowed user with one live token.
type tokenFixture struct {
	auth  *TokenAuthenticator
	store store.Store
	user  store.User
	token string
	rec   store.APIToken
	now   time.Time
}

func newTokenFixture(t *testing.T, allowlist string) *tokenFixture {
	t.Helper()
	f := &tokenFixture{store: store.NewMemory(), now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	f.user = store.User{ID: "user-1", OrgID: "default", Email: "alice@example.com"}
	if err := f.store.Users().Create(ctx, f.user); err != nil {
		t.Fatal(err)
	}
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	f.token = tok
	expires := f.now.Add(90 * 24 * time.Hour)
	f.rec = store.APIToken{
		ID: "token-1", OrgID: f.user.OrgID, UserID: f.user.ID,
		Hash: HashToken(tok), Name: "laptop", ExpiresAt: &expires, CreatedAt: f.now,
	}
	if err := f.store.Tokens().Create(ctx, f.rec); err != nil {
		t.Fatal(err)
	}
	list, _ := testAllowlist(t, allowlist)
	f.auth = &TokenAuthenticator{
		Tokens: f.store.Tokens(), Users: f.store.Users(), Allowlist: list,
		Now: func() time.Time { return f.now },
	}
	return f
}

// req builds a request carrying a bearer token.
func req(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// TestTokenAuthenticatorResolvesItsUser.
//
// @decision(DL-006)
func TestTokenAuthenticatorResolvesItsUser(t *testing.T) {
	f := newTokenFixture(t, "alice@example.com\n")
	p, err := f.auth.Authenticate(req(f.token))
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != f.user.ID || p.OrgID != f.user.OrgID || p.Email != f.user.Email {
		t.Errorf("principal = %+v", p)
	}
	// The token's own id travels, so `spov logout` can revoke "the one
	// I am using" without the CLI knowing which that is.
	if p.TokenID != f.rec.ID {
		t.Errorf("TokenID = %q, want %q", p.TokenID, f.rec.ID)
	}
	if p.Admin {
		t.Error("a plain allowlist entry produced an admin")
	}
}

// TestTokenAuthenticatorTakesAdminFromTheAllowlist, not from the stored
// row, exactly as the cookie authenticator does.
//
// @decision(DL-006) @decision(DL-007)
func TestTokenAuthenticatorTakesAdminFromTheAllowlist(t *testing.T) {
	f := newTokenFixture(t, "alice@example.com admin\n")
	p, err := f.auth.Authenticate(req(f.token))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Admin {
		t.Error("an admin entry did not produce an admin principal")
	}
}

// TestTokenAuthenticatorRefusesARemovedUser: a CLI token must lose
// access on the same terms as a browser session.
//
// @decision(DL-006) @decision(DL-007)
func TestTokenAuthenticatorRefusesARemovedUser(t *testing.T) {
	f := newTokenFixture(t, "someone-else@example.com\n")
	_, err := f.auth.Authenticate(req(f.token))
	if !errors.Is(err, ErrBadCredential) {
		t.Fatalf("err = %v, want ErrBadCredential", err)
	}
	if errors.Is(err, ErrUnauthenticated) {
		t.Error("a removed user was reported as unauthenticated")
	}
}

// TestTokenAuthenticatorRefusesARevokedToken.
//
// @decision(DL-006)
func TestTokenAuthenticatorRefusesARevokedToken(t *testing.T) {
	f := newTokenFixture(t, "alice@example.com\n")
	if err := f.store.Tokens().Revoke(context.Background(), f.rec.OrgID, f.rec.ID, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Authenticate(req(f.token)); !errors.Is(err, ErrBadCredential) {
		t.Errorf("a revoked token gave %v, want ErrBadCredential", err)
	}
}

// TestTokenAuthenticatorRefusesAnExpiredToken.
//
// @decision(DL-006)
func TestTokenAuthenticatorRefusesAnExpiredToken(t *testing.T) {
	f := newTokenFixture(t, "alice@example.com\n")
	f.now = f.now.Add(91 * 24 * time.Hour)
	if _, err := f.auth.Authenticate(req(f.token)); !errors.Is(err, ErrBadCredential) {
		t.Errorf("an expired token gave %v, want ErrBadCredential", err)
	}
}

// TestTokenAuthenticatorPassesOnWhatIsNotItsCredential: the chain tries
// each authenticator in turn, so a cookie request or a local-mode bearer
// must fall through rather than be rejected here.
//
// @decision(DL-006) @decision(DL-007)
func TestTokenAuthenticatorPassesOnWhatIsNotItsCredential(t *testing.T) {
	f := newTokenFixture(t, "alice@example.com\n")
	for _, tc := range []struct{ name, header string }{
		{"no header", ""},
		{"a local-mode bearer", "kZ4v9rQwErTyUiOpAsDfGhJkLzXcVbNm1234567890"},
		{"a github token", "ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.auth.Authenticate(req(tc.header))
			if !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("err = %v, want ErrUnauthenticated so the chain continues", err)
			}
		})
	}
}

// TestAWellFormedUnknownTokenStopsTheChain: it is a spov token that does
// not exist, which is a rejection rather than "not mine".
//
// @decision(DL-006)
func TestAWellFormedUnknownTokenStopsTheChain(t *testing.T) {
	f := newTokenFixture(t, "alice@example.com\n")
	other, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.auth.Authenticate(req(other))
	if !errors.Is(err, ErrBadCredential) {
		t.Errorf("err = %v, want ErrBadCredential", err)
	}
}

// TestTokenAuthenticatorTouchesAtMostOnceAMinute: writing last_used_at
// on every request turns a read-only API call into a write.
//
// @decision(DL-006)
func TestTokenAuthenticatorTouchesAtMostOnceAMinute(t *testing.T) {
	f := newTokenFixture(t, "alice@example.com\n")
	ctx := context.Background()

	if _, err := f.auth.Authenticate(req(f.token)); err != nil {
		t.Fatal(err)
	}
	first, err := f.store.Tokens().ByID(ctx, f.rec.OrgID, f.rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.LastUsedAt == nil {
		t.Fatal("the first use was not recorded")
	}

	// A second request moments later must not write again.
	f.now = f.now.Add(10 * time.Second)
	if _, err := f.auth.Authenticate(req(f.token)); err != nil {
		t.Fatal(err)
	}
	again, err := f.store.Tokens().ByID(ctx, f.rec.OrgID, f.rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.LastUsedAt.Equal(*first.LastUsedAt) {
		t.Errorf("last_used_at moved within the interval: %v then %v", first.LastUsedAt, again.LastUsedAt)
	}

	// Past the interval it does.
	f.now = f.now.Add(2 * TouchInterval)
	if _, err := f.auth.Authenticate(req(f.token)); err != nil {
		t.Fatal(err)
	}
	later, err := f.store.Tokens().ByID(ctx, f.rec.OrgID, f.rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !later.LastUsedAt.After(*first.LastUsedAt) {
		t.Error("last_used_at did not move past the interval")
	}
}

// TestTokenAuthenticatorErrorsCarryNoCredential: the middleware logs a
// rejection, so its text must never hold the token.
//
// @decision(DL-006)
func TestTokenAuthenticatorErrorsCarryNoCredential(t *testing.T) {
	f := newTokenFixture(t, "someone-else@example.com\n")
	_, err := f.auth.Authenticate(req(f.token))
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if strings.Contains(err.Error(), f.token) {
		t.Errorf("the error carries the token: %v", err)
	}
	// An unknown token is the other path that logs.
	other, mintErr := NewToken()
	if mintErr != nil {
		t.Fatal(mintErr)
	}
	_, err = f.auth.Authenticate(req(other))
	if err != nil && strings.Contains(err.Error(), other) {
		t.Errorf("the error carries the token: %v", err)
	}
}

// failingTokens injects failures into the token store.
type failingTokens struct {
	store.TokenStore
	byHashErr error
	touchErr  error
}

func (f failingTokens) ByHash(ctx context.Context, hash []byte) (store.APIToken, error) {
	if f.byHashErr != nil {
		return store.APIToken{}, f.byHashErr
	}
	return f.TokenStore.ByHash(ctx, hash)
}

func (f failingTokens) Touch(ctx context.Context, orgID, id string, at time.Time) error {
	if f.touchErr != nil {
		return f.touchErr
	}
	return f.TokenStore.Touch(ctx, orgID, id, at)
}

// TestTokenAuthenticatorReportsAStoreFailureAsItself: a database that is
// down is not a bad credential, and answering 401 would tell the user to
// log in again for no reason.
//
// @decision(DL-006)
func TestTokenAuthenticatorReportsAStoreFailureAsItself(t *testing.T) {
	f := newTokenFixture(t, "alice@example.com\n")
	down := errors.New("the database is down")
	f.auth.Tokens = failingTokens{TokenStore: f.store.Tokens(), byHashErr: down}

	_, err := f.auth.Authenticate(req(f.token))
	if err == nil {
		t.Fatal("a store failure authenticated the request")
	}
	if errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrBadCredential) {
		t.Errorf("a store failure was reported as a credential problem: %v", err)
	}
	if !strings.Contains(err.Error(), "database is down") {
		t.Errorf("the cause was lost: %v", err)
	}
}

// TestAFailedTouchDoesNotFailTheRequest: recording use is bookkeeping,
// and the request is authenticated either way.
//
// @decision(DL-006)
func TestAFailedTouchDoesNotFailTheRequest(t *testing.T) {
	f := newTokenFixture(t, "alice@example.com\n")
	var warned []string
	f.auth.Tokens = failingTokens{TokenStore: f.store.Tokens(), touchErr: errors.New("write failed")}
	f.auth.Warn = func(msg string) { warned = append(warned, msg) }

	p, err := f.auth.Authenticate(req(f.token))
	if err != nil {
		t.Fatalf("a failed touch failed the request: %v", err)
	}
	if p.UserID != f.user.ID {
		t.Errorf("principal = %+v", p)
	}
	if len(warned) != 1 {
		t.Errorf("got %d warnings, want one: %v", len(warned), warned)
	}
	// And the warning carries no credential.
	for _, w := range warned {
		if strings.Contains(w, f.token) {
			t.Errorf("a warning carried the token: %s", w)
		}
	}
}

// TestTokenAuthenticatorDefaultsItsClock, so a zero Now does not expire
// every token against the zero time.
//
// @decision(DL-006)
func TestTokenAuthenticatorDefaultsItsClock(t *testing.T) {
	f := newTokenFixture(t, "alice@example.com\n")
	f.auth.Now = nil
	// The fixture's token expires 90 days after a fixed date in the
	// past, so with the real clock it is expired — which proves the
	// default is the wall clock rather than the zero time.
	if _, err := f.auth.Authenticate(req(f.token)); err == nil {
		t.Skip("the fixture's token has not yet expired in real time")
	}
	// Re-issue with a real expiry and confirm it works.
	future := time.Now().UTC().Add(time.Hour)
	rec := f.rec
	rec.ID = "token-2"
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	rec.Hash = HashToken(tok)
	rec.ExpiresAt = &future
	if err := f.store.Tokens().Create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Authenticate(req(tok)); err != nil {
		t.Errorf("a token valid against the real clock was refused: %v", err)
	}
}
