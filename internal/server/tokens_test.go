package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jimutt/spov/internal/store"
)

// issueToken runs a whole device flow and returns the token.
func (f *deviceFixture) issueToken(t *testing.T, name string) string {
	t.Helper()
	cfg := f.oauthConfig()
	ctx := context.Background()
	opts := []oauth2.AuthCodeOption{}
	if name != "" {
		opts = append(opts, oauth2.SetAuthURLParam("name", name))
	}
	da, err := cfg.DeviceAuth(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	f.approve(da.UserCode, "approve")
	// Exchanged directly rather than through x/oauth2's polling loop,
	// which waits a full second before its first request.
	//
	// @decision(DL-006)
	return f.accessToken(da.DeviceCode)
}

// TestListTokensNeverReturnsTheToken: only the hash is stored, and the
// listing must not resurrect anything usable.
//
// @decision(DL-006)
func TestListTokensNeverReturnsTheToken(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	token := f.issueToken(t, "laptop")

	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/v1/me/tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp := f.sendNoCookies(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing = %d", resp.StatusCode)
	}
	body := string(resp.Body)
	if strings.Contains(body, token) {
		t.Errorf("the listing returned the token: %s", body)
	}
	// Nor the hash, which is equally not for showing.
	if strings.Contains(body, "hash") {
		t.Errorf("the listing exposes the hash: %s", body)
	}
}

// TestRevokingCurrentEndsThisSession, which is what `spov logout` does.
//
// @decision(DL-006)
func TestRevokingCurrentEndsThisSession(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	token := f.issueToken(t, "laptop")

	if code := f.deleteWithToken("/api/v1/me/tokens/"+CurrentToken, token); code != http.StatusNoContent {
		t.Fatalf("revoking current = %d", code)
	}
	// The token no longer authenticates.
	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if resp := f.sendNoCookies(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked token = %d, want 401", resp.StatusCode)
	}
}

// TestRevokingAnotherTokenLeavesThisOne.
//
// @decision(DL-006)
func TestRevokingAnotherTokenLeavesThisOne(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	first := f.issueToken(t, "laptop")
	// A second flow, from another machine.
	f.now = f.now.Add(time.Minute)
	second := f.issueToken(t, "desktop")

	tokens := f.listTokensWith(first)
	if len(tokens) != 2 {
		t.Fatalf("got %d tokens, want 2", len(tokens))
	}
	var other string
	for _, tok := range tokens {
		if !tok.Current {
			other = tok.ID
		}
	}
	if other == "" {
		t.Fatal("no other token found")
	}
	if code := f.deleteWithToken("/api/v1/me/tokens/"+other, first); code != http.StatusNoContent {
		t.Fatalf("revoking = %d", code)
	}

	// The revoking token still works; the revoked one does not.
	if got := f.listTokensWith(first); len(got) != 1 {
		t.Errorf("got %d tokens after revoking one", len(got))
	}
	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+second)
	if resp := f.sendNoCookies(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the revoked token = %d, want 401", resp.StatusCode)
	}
}

// TestRevokingAnotherUsersTokenIsNotFound: the id is unguessable, and
// "not yours" would confirm it exists.
//
// @decision(DL-006)
func TestRevokingAnotherUsersTokenIsNotFound(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\nbob@example.com\n")
	f.doLogin("")
	alice := f.issueToken(t, "alice laptop")
	aliceTokens := f.listTokensWith(alice)
	if len(aliceTokens) != 1 {
		t.Fatalf("got %d tokens", len(aliceTokens))
	}

	// Bob, in his own browser, with his own token.
	bob := f.secondBrowser()
	bobDevice := &deviceFixture{loginFixture: bob, device: f.device, now: f.now}
	bob.github.emails = []map[string]any{{"email": "bob@example.com", "primary": true, "verified": true}}
	bobDevice.doLogin("")
	f.now = f.now.Add(time.Minute)
	bobToken := bobDevice.issueToken(t, "bob laptop")

	if code := f.deleteWithToken("/api/v1/me/tokens/"+aliceTokens[0].ID, bobToken); code != http.StatusNotFound {
		t.Errorf("bob revoking alice's token = %d, want 404", code)
	}
	// Alice's token still works.
	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+alice)
	if resp := f.sendNoCookies(req); resp.StatusCode != http.StatusOK {
		t.Errorf("alice's token = %d after bob's attempt", resp.StatusCode)
	}
}

// TestRevokingAnUnknownTokenIsNotFound.
//
// @decision(DL-006)
func TestRevokingAnUnknownTokenIsNotFound(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	token := f.issueToken(t, "laptop")
	if code := f.deleteWithToken("/api/v1/me/tokens/nosuchtoken", token); code != http.StatusNotFound {
		t.Errorf("revoking an unknown token = %d, want 404", code)
	}
}

// TestRevokingCurrentFromABrowserIsNotFound: a session cookie is not a
// CLI token, so there is no "current" one to revoke.
//
// @decision(DL-006)
func TestRevokingCurrentFromABrowserIsNotFound(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	if code := f.delete("/api/v1/me/tokens/" + CurrentToken); code != http.StatusNotFound {
		t.Errorf("revoking current from a browser = %d, want 404", code)
	}
}

// TestABrowserSeesItsUsersTokens, so the web UI can list and revoke
// them without holding one.
//
// @decision(DL-006)
func TestABrowserSeesItsUsersTokens(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	f.issueToken(t, "laptop")

	resp := f.get("/api/v1/me/tokens")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing from a browser = %d", resp.StatusCode)
	}
	var out struct {
		Tokens []TokenView `json:"tokens"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("a browser saw %d tokens", len(out.Tokens))
	}
	// None is current: a cookie is not a token.
	if out.Tokens[0].Current {
		t.Error("a browser's request marked a token current")
	}
}

// TestARevokedTokenLeavesTheListing.
//
// @decision(DL-006)
func TestARevokedTokenLeavesTheListing(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	token := f.issueToken(t, "laptop")
	tokens := f.listTokensWith(token)
	if len(tokens) != 1 {
		t.Fatalf("got %d tokens", len(tokens))
	}
	// Revoke it in the store directly, so the browser can still look.
	if err := f.store.Tokens().Revoke(context.Background(), DefaultOrgID, tokens[0].ID, f.now); err != nil {
		t.Fatal(err)
	}
	resp := f.get("/api/v1/me/tokens")
	var out struct {
		Tokens []TokenView `json:"tokens"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tokens) != 0 {
		t.Errorf("a revoked token is still listed: %+v", out.Tokens)
	}
}

// TestTokenListingShowsWhatIdentifiesAToken, since revoking one means
// telling them apart.
//
// @decision(DL-006)
func TestTokenListingShowsWhatIdentifiesAToken(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	token := f.issueToken(t, "my laptop")
	tokens := f.listTokensWith(token)
	if len(tokens) != 1 {
		t.Fatalf("got %d tokens", len(tokens))
	}
	tok := tokens[0]
	if tok.Name != "my laptop" {
		t.Errorf("name = %q", tok.Name)
	}
	if tok.CreatedAt.IsZero() {
		t.Error("no creation time")
	}
	if tok.ExpiresAt == nil {
		t.Error("no expiry, so a token would live forever")
	} else if !tok.ExpiresAt.After(f.now) {
		t.Errorf("expiry %v is not in the future", tok.ExpiresAt)
	}
}

// deleteWithToken issues a DELETE authenticated by a bearer token.
func (f *deviceFixture) deleteWithToken(path, token string) int {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodDelete, f.http.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return f.sendNoCookies(req).StatusCode
}

// failingTokenStore injects failures into the token store.
type failingTokenStore struct {
	store.TokenStore
	listErr   error
	byIDErr   error
	revokeErr error
}

func (f failingTokenStore) ListByUser(ctx context.Context, orgID, userID string) ([]store.APIToken, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.TokenStore.ListByUser(ctx, orgID, userID)
}

func (f failingTokenStore) ByID(ctx context.Context, orgID, id string) (store.APIToken, error) {
	if f.byIDErr != nil {
		return store.APIToken{}, f.byIDErr
	}
	return f.TokenStore.ByID(ctx, orgID, id)
}

func (f failingTokenStore) Revoke(ctx context.Context, orgID, id string, at time.Time) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	return f.TokenStore.Revoke(ctx, orgID, id, at)
}

type faultyTokenStore struct {
	store.Store
	tokens store.TokenStore
}

func (s faultyTokenStore) Tokens() store.TokenStore { return s.tokens }

// TestTokenEndpointsReportAStoreFailure rather than reporting an empty
// list or a successful revoke that did not happen.
//
// @decision(DL-006)
func TestTokenEndpointsReportAStoreFailure(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	token := f.issueToken(t, "laptop")
	tokens := f.listTokensWith(token)
	if len(tokens) != 1 {
		t.Fatalf("got %d tokens", len(tokens))
	}
	down := errors.New("the database is down")

	t.Run("listing", func(t *testing.T) {
		f.swapTokenStore(failingTokenStore{TokenStore: f.store.Tokens(), listErr: down})
		defer f.restoreStore()
		req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/v1/me/tokens", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if resp := f.sendNoCookies(req); resp.StatusCode < 500 {
			t.Errorf("a failed listing = %d, want a server error", resp.StatusCode)
		}
	})

	t.Run("revoking", func(t *testing.T) {
		f.swapTokenStore(failingTokenStore{TokenStore: f.store.Tokens(), revokeErr: down})
		defer f.restoreStore()
		if code := f.deleteWithToken("/api/v1/me/tokens/"+tokens[0].ID, token); code < 500 {
			t.Errorf("a failed revoke = %d, want a server error", code)
		}
	})

	t.Run("resolving", func(t *testing.T) {
		f.swapTokenStore(failingTokenStore{TokenStore: f.store.Tokens(), byIDErr: down})
		defer f.restoreStore()
		if code := f.deleteWithToken("/api/v1/me/tokens/"+tokens[0].ID, token); code < 500 {
			t.Errorf("a failed lookup = %d, want a server error", code)
		}
	})
}

// swapTokenStore points the server's Deps at a failing token store.
// Only the handlers read it, so the authenticator keeps working.
func (f *deviceFixture) swapTokenStore(ts store.TokenStore) {
	f.server.deps.Store = faultyTokenStore{Store: f.store, tokens: ts}
}

func (f *deviceFixture) restoreStore() {
	f.server.deps.Store = f.store
}

// TestTokensAreServedWithoutTheDeviceFlow: the handlers only ever read
// the token store, so a server with no device flow answers honestly, an
// empty list and 404 for a revoke.
//
// @decision(DL-055) @decision(DL-006)
func TestTokensAreServedWithoutTheDeviceFlow(t *testing.T) {
	f := newAPIFixture(t)
	if f.srv.deps.Device != nil {
		t.Fatal("this fixture was meant to have no device flow")
	}

	res, body := f.do(http.MethodGet, "/api/v1/me/tokens", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("listing = %d %s, want 200", res.StatusCode, body)
	}
	var out struct {
		Tokens []TokenView `json:"tokens"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	// Not null: the page renders a list, and `null` is not one.
	if out.Tokens == nil {
		t.Errorf("tokens = null, want an empty array: %s", body)
	}
	if len(out.Tokens) != 0 {
		t.Errorf("a server that issues no tokens listed %d", len(out.Tokens))
	}

	// And a revoke is a 404 rather than a route that does not exist,
	// which is the difference between "no such token" and "no such API".
	if res, body := f.do(http.MethodDelete, "/api/v1/me/tokens/whatever", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("revoke = %d %s, want 404", res.StatusCode, body)
	}
	// `current` too: a bearer token from local auth carries no TokenID.
	if res, body := f.do(http.MethodDelete, "/api/v1/me/tokens/"+CurrentToken, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("revoke current = %d %s, want 404", res.StatusCode, body)
	}
}

// TestTheDocumentCarriesTheTokenOperations, since the web client is
// generated from it: an operation missing here is one the tokens page
// cannot call at all.
//
// @decision(DL-055) @decision(DL-005)
func TestTheDocumentCarriesTheTokenOperations(t *testing.T) {
	f := newAPIFixture(t)
	doc := f.srv.api.OpenAPI().Paths
	for _, path := range []string{"/api/v1/me/tokens", "/api/v1/me/tokens/{id}"} {
		if doc[path] == nil {
			t.Errorf("the document has no %s", path)
		}
	}
}
