package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"
	"golang.org/x/oauth2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// loginFixture is a hosted server with the browser login wired up, a
// stub GitHub behind it, and a client that keeps cookies.
type loginFixture struct {
	t             *testing.T
	server        *Server
	http          *httptest.Server
	client        *http.Client
	store         store.Store
	login         *Login
	allowlistPath string
	github        *githubStub
	warnings      []string
}

// githubStub is a stub GitHub that completes a login.
type githubStub struct {
	*httptest.Server
	emails      []map[string]any
	login, name string
}

func newGitHubStub(t *testing.T) *githubStub {
	t.Helper()
	s := &githubStub{login: "octocat", name: "The Octocat"}
	s.emails = []map[string]any{{"email": "alice@example.com", "primary": true, "verified": true}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_stub","token_type":"bearer"}`))
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": s.login, "name": s.name})
	})
	mux.HandleFunc("GET /user/emails", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(s.emails)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// setAllowlist replaces the org's entries and refreshes the cache,
// which is what an admin API call does.
//
// @decision(DL-007)
func (f *loginFixture) setAllowlist(entries map[string]bool) {
	f.t.Helper()
	ctx := context.Background()
	al := f.store.Allowlist()
	cur, err := al.List(ctx, DefaultOrgID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range cur {
		if err := al.Delete(ctx, DefaultOrgID, e.Email); err != nil {
			f.t.Fatal(err)
		}
	}
	for email, admin := range entries {
		if err := al.Put(ctx, store.AllowlistEntry{OrgID: DefaultOrgID, Email: email, Admin: admin}); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.login.Allowlist.Refresh(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func newLoginFixture(t *testing.T, allowlist string) *loginFixture {
	t.Helper()
	return newLoginFixtureWith(t, allowlist, nil)
}

// newLoginFixtureWith builds the fixture and lets a test adjust Deps —
// and the Login they hang off — before the server is constructed.
func newLoginFixtureWith(t *testing.T, allowlist string, adjust func(*Deps, *Login)) *loginFixture {
	t.Helper()
	f := &loginFixture{t: t, store: store.NewMemory(), github: newGitHubStub(t)}

	f.allowlistPath = filepath.Join(t.TempDir(), "allowlist")
	if err := os.WriteFile(f.allowlistPath, []byte(allowlist), 0o600); err != nil {
		t.Fatal(err)
	}
	warn := func(s string) { f.warnings = append(f.warnings, s) }
	// The file seeds the store, and the store is what enforcement
	// reads — the same two steps the real wiring takes.
	//
	// @decision(DL-007)
	ctx := context.Background()
	if _, err := auth.SeedAllowlist(ctx, f.store, DefaultOrgID, f.allowlistPath, warn); err != nil {
		t.Fatal(err)
	}
	list, err := auth.NewAllowlist(ctx,
		auth.StoreAllowlist(f.store.Allowlist(), DefaultOrgID), auth.AllowlistRefresh, warn)
	if err != nil {
		t.Fatal(err)
	}

	sessions := scs.New()
	sessions.Store = memstore.New()
	sessions.Lifetime = 7 * 24 * time.Hour
	sessions.Cookie.Name = "spov_session"
	sessions.Cookie.HttpOnly = true
	sessions.Cookie.SameSite = http.SameSiteLaxMode
	// The test server is http, so a Secure cookie would never be sent
	// back; the production wiring sets it from the public URL's scheme.
	sessions.Cookie.Secure = false

	oauthApp := auth.NewGitHubOAuth("client-id", "client-secret", "http://spov.test/oauth/github/callback")
	oauthApp.Config.Endpoint = oauth2.Endpoint{
		AuthURL:   f.github.URL + "/login/oauth/authorize",
		TokenURL:  f.github.URL + "/login/oauth/access_token",
		AuthStyle: oauth2.AuthStyleInParams,
	}
	oauthApp.APIURL = f.github.URL
	oauthApp.HTTP = f.github.Client()

	f.login = &Login{
		OAuth: oauthApp, Sessions: sessions, Users: f.store.Users(),
		Allowlist: list, OrgID: DefaultOrgID,
		Warn: func(s string) { f.warnings = append(f.warnings, s); t.Log(s) },
	}

	cookies := &auth.CookieAuthenticator{
		Sessions: sessions, Users: f.store.Users(), Allowlist: list, OrgID: DefaultOrgID,
	}
	deps := Deps{
		Store: f.store,
		Auth:  auth.Chain{cookies},
		// The approval page authenticates with the cookie alone, as the
		// real wiring does: a CLI token must not approve a device flow.
		//
		// @decision(DL-006)
		BrowserAuth: cookies,
		Login:       f.login,
		Warn:        func(s string) { t.Log(s) },
	}
	if adjust != nil {
		adjust(&deps, f.login)
	}
	// The guard compares the Host against the address the server was
	// built for, so the listener comes first.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(deps, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	f.server = srv
	f.http = httptest.NewUnstartedServer(srv.Handler())
	f.http.Listener = ln
	f.http.Start()
	t.Cleanup(func() { f.http.Close(); srv.Close() })

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.client = &http.Client{
		Jar: jar,
		// Redirects are the flow's control signal, so they are
		// inspected rather than followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return f
}

// response is what a test needs from a finished response: the body is
// already read and closed.
type response struct {
	StatusCode int
	Header     http.Header
	Cookies    []*http.Cookie
	Body       []byte
}

// do issues a request with the fixture's cookie jar and drains the
// response.
func (f *loginFixture) do(method, path string) response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.http.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.send(req)
}

// send performs req and drains the response.
func (f *loginFixture) send(req *http.Request) response {
	f.t.Helper()
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return response{StatusCode: resp.StatusCode, Header: resp.Header, Cookies: resp.Cookies(), Body: body}
}

// sendNoCookies performs req without the fixture's cookie jar, so a
// bearer token is the only credential in play.
func (f *loginFixture) sendNoCookies(req *http.Request) response {
	f.t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return response{StatusCode: resp.StatusCode, Header: resp.Header, Cookies: resp.Cookies(), Body: body}
}

// get issues a GET with the fixture's cookie jar and the Host the
// browser guard expects.
func (f *loginFixture) get(path string) response {
	f.t.Helper()
	return f.do(http.MethodGet, path)
}

// login walks the whole flow and returns the callback's response.
func (f *loginFixture) doLogin(next string) response {
	f.t.Helper()
	path := LoginPath
	if next != "" {
		path += "?next=" + url.QueryEscape(next)
	}
	resp := f.get(path)
	if resp.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("%s = %d, want a redirect to GitHub", path, resp.StatusCode)
	}
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		f.t.Fatal(err)
	}
	state := to.Query().Get("state")
	if state == "" {
		f.t.Fatal("the redirect to GitHub carried no state")
	}
	return f.get(CallbackPath + "?code=the-code&state=" + url.QueryEscape(state))
}

// delete issues a DELETE with the fixture's cookie jar and returns the
// status.
func (f *loginFixture) delete(path string) int {
	f.t.Helper()
	return f.do(http.MethodDelete, path).StatusCode
}

// logout ends the fixture's session.
func (f *loginFixture) logout() {
	f.t.Helper()
	if code := f.do(http.MethodPost, LogoutPath).StatusCode; code != http.StatusSeeOther {
		f.t.Fatalf("logout = %d", code)
	}
}

// TestLoginCreatesTheUserAndTheSession is the whole happy path: a first
// login creates the user, sets a cookie, and that cookie authenticates
// the API.
//
// @decision(DL-007)
func TestLoginCreatesTheUserAndTheSession(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")

	resp := f.doLogin("")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the callback = %d, want a redirect", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/" {
		t.Errorf("landed at %q, want /", got)
	}

	// The user exists, with what GitHub said.
	user, err := f.store.Users().ByEmail(context.Background(), DefaultOrgID, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if user.GithubLogin != "octocat" || user.Name != "The Octocat" {
		t.Errorf("user = %+v", user)
	}
	// The git identity is initialised from the profile, so commits made
	// in this user's sessions are attributed.
	if user.GitEmail != "alice@example.com" || user.GitName != "The Octocat" {
		t.Errorf("git identity = %q <%s>", user.GitName, user.GitEmail)
	}
	if user.LastLoginAt == nil {
		t.Error("last_login_at was not recorded")
	}

	// And the cookie authenticates the API.
	apiResp := f.get("/api/v1/me")
	if apiResp.StatusCode != http.StatusOK {
		t.Fatalf("/api/v1/me with the session cookie = %d", apiResp.StatusCode)
	}
	var me struct {
		Email  string `json:"email"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(apiResp.Body, &me); err != nil {
		t.Fatal(err)
	}
	if me.Email != "alice@example.com" || me.UserID != user.ID {
		t.Errorf("me = %+v", me)
	}
}

// TestTheSessionCookieIsHardened: the cookie is the credential, so its
// attributes are what stop a page on another site from using it.
//
// @decision(DL-007)
func TestTheSessionCookieIsHardened(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.doLogin("")

	var cookie *http.Cookie
	for _, c := range resp.Cookies {
		if c.Name == "spov_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("the login set no session cookie")
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie is not HttpOnly, so script can read it")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q", cookie.Path)
	}
	// The token is opaque: nothing about the user is encoded in it.
	if strings.Contains(cookie.Value, "alice") {
		t.Errorf("the cookie value carries the identity: %q", cookie.Value)
	}
}

// TestLoginRefusesAnAddressOffTheAllowlist, and says which one, so the
// person can tell an operator what to add.
//
// @decision(DL-007)
func TestLoginRefusesAnAddressOffTheAllowlist(t *testing.T) {
	f := newLoginFixture(t, "someone-else@example.com\n")
	resp := f.doLogin("")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the callback = %d, want 403", resp.StatusCode)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "alice@example.com") {
		t.Errorf("the page does not name the address: %s", body)
	}
	// No user was created for someone who may not log in.
	if _, err := f.store.Users().ByEmail(context.Background(), DefaultOrgID, "alice@example.com"); err == nil {
		t.Error("a refused login created a user")
	}
	// A cookie may still be present — the flow itself used a session —
	// but it must not authenticate anything.
	if got := f.get("/api/v1/me"); got.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after a refused login = %d, want 401", got.StatusCode)
	}
}

// TestLoginRefusesAnUnverifiedEmail: GitHub lets anyone add an
// unverified address, so trusting one would turn the allowlist into an
// open door.
//
// @decision(DL-007)
func TestLoginRefusesAnUnverifiedEmail(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.github.emails = []map[string]any{
		{"email": "alice@example.com", "primary": true, "verified": false},
	}
	resp := f.doLogin("")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the callback = %d, want 403", resp.StatusCode)
	}
	if body := string(resp.Body); !strings.Contains(body, "verified") {
		t.Errorf("the page does not explain why: %s", body)
	}
}

// TestCallbackRefusesAMismatchedState, which is the CSRF defence on the
// callback itself.
//
// @decision(DL-007)
func TestCallbackRefusesAMismatchedState(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	// Start a real flow, so a session with a state exists...
	if resp := f.get(LoginPath); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	// ...then return with the wrong one.
	resp := f.get(CallbackPath + "?code=the-code&state=not-the-state")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a mismatched state = %d, want 400", resp.StatusCode)
	}
	if _, err := f.store.Users().ByEmail(context.Background(), DefaultOrgID, "alice@example.com"); err == nil {
		t.Error("a login with a bad state created a user")
	}
}

// TestCallbackWithoutAFlowInProgress: a bookmarked callback, or a cookie
// dropped between the redirect and the return.
//
// @decision(DL-007)
func TestCallbackWithoutAFlowInProgress(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.get(CallbackPath + "?code=the-code&state=anything")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a callback with no flow = %d, want 400", resp.StatusCode)
	}
	if body := string(resp.Body); !strings.Contains(body, "expired") {
		t.Errorf("the page does not explain: %s", body)
	}
}

// TestStateIsSingleUse: replaying a callback must not produce a second
// session.
//
// @decision(DL-007)
func TestStateIsSingleUse(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.get(LoginPath)
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := to.Query().Get("state")

	if got := f.get(CallbackPath + "?code=c&state=" + url.QueryEscape(state)); got.StatusCode != http.StatusSeeOther {
		t.Fatalf("the first callback = %d", got.StatusCode)
	}
	// The same state again: the flow keys were popped, so there is no
	// flow in progress any more.
	replay := f.get(CallbackPath + "?code=c&state=" + url.QueryEscape(state))
	if replay.StatusCode == http.StatusSeeOther {
		t.Error("a replayed callback logged in again")
	}
}

// TestLogoutRevokesTheSession: the cookie stops working immediately,
// because the row is gone rather than the cookie merely cleared.
//
// @decision(DL-007)
func TestLogoutRevokesTheSession(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")
	if resp := f.get("/api/v1/me"); resp.StatusCode != http.StatusOK {
		t.Fatalf("me before logout = %d", resp.StatusCode)
	}

	f.logout()
	if got := f.get("/api/v1/me"); got.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after logout = %d, want 401", got.StatusCode)
	}
}

// TestRemovingAnAllowlistEntryLocksTheUserOutImmediately, without
// waiting for the session to expire. This is why the allowlist is
// consulted per request rather than only at login.
//
// @decision(DL-007)
func TestRemovingAnAllowlistEntryLocksTheUserOutImmediately(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\nbob@example.com\n")
	f.doLogin("")
	if resp := f.get("/api/v1/me"); resp.StatusCode != http.StatusOK {
		t.Fatalf("me = %d", resp.StatusCode)
	}

	f.setAllowlist(map[string]bool{"bob@example.com": false})
	if resp := f.get("/api/v1/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after removal = %d, want 401", resp.StatusCode)
	}
}

// TestRevokeSessionsDeletesTheirSessions, so a re-added address does not
// resume a session from before it was removed.
//
// @decision(DL-007)
func TestRevokeSessionsDeletesTheirSessions(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")

	f.setAllowlist(map[string]bool{"bob@example.com": false})
	if err := f.login.RevokeSessions(context.Background(), DefaultOrgID, "alice@example.com"); err != nil {
		t.Fatal(err)
	}

	// Put alice back: the old session must not work, because its row is
	// gone.
	f.setAllowlist(map[string]bool{"alice@example.com": false})
	if resp := f.get("/api/v1/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after revoke and re-add = %d, want 401", resp.StatusCode)
	}
}

// An address may be allowed before anyone has ever logged in under it,
// so removing it must not be an error.
//
// @decision(DL-007)
func TestRevokeSessionsForSomeoneWhoNeverLoggedIn(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")
	if err := f.login.RevokeSessions(context.Background(), DefaultOrgID, "nobody@example.com"); err != nil {
		t.Fatalf("revoking an address with no user: %v", err)
	}
	// And it revoked nothing else on the way past.
	if got := f.get("/api/v1/me"); got.StatusCode != http.StatusOK {
		t.Errorf("me = %d, want the untouched session to still work", got.StatusCode)
	}
}

// TestASecondLoginReusesTheUser rather than creating a duplicate, and
// refreshes what GitHub owns.
//
// @decision(DL-007)
func TestASecondLoginReusesTheUser(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")
	first, err := f.store.Users().ByEmail(context.Background(), DefaultOrgID, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}

	// A renamed GitHub account, logging in again from a fresh browser.
	// The logout is required: /login short-circuits for a browser that
	// already has a session, which TestLoginIsANoOpWhenAlreadySignedIn
	// covers.
	f.logout()
	f.github.name = "Alice Example"
	f.github.login = "alice-example"
	f.doLogin("")

	users, err := f.store.Users().List(context.Background(), DefaultOrgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d users, want the first one reused", len(users))
	}
	second := users[0]
	if second.ID != first.ID {
		t.Errorf("the user id changed from %s to %s", first.ID, second.ID)
	}
	if second.Name != "Alice Example" || second.GithubLogin != "alice-example" {
		t.Errorf("the profile was not refreshed: %+v", second)
	}
}

// TestLoginIsANoOpWhenAlreadySignedIn: a browser that already has a
// session is sent on rather than round-tripped through GitHub, and the
// session it has is kept.
//
// @decision(DL-007)
func TestLoginIsANoOpWhenAlreadySignedIn(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")

	resp := f.get(LoginPath + "?next=" + url.QueryEscape("/sessions"))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login while signed in = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/sessions" {
		t.Errorf("landed at %q, want the next path rather than GitHub", got)
	}
	// The existing session still works.
	if got := f.get("/api/v1/me"); got.StatusCode != http.StatusOK {
		t.Errorf("me = %d after a redundant login", got.StatusCode)
	}
}

// TestAdminComesFromTheAllowlist.
//
// @decision(DL-007)
func TestAdminComesFromTheAllowlist(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com admin\n")
	f.doLogin("")
	resp := f.get("/api/v1/me")
	var me struct {
		Admin bool `json:"admin"`
	}
	if err := json.Unmarshal(resp.Body, &me); err != nil {
		t.Fatal(err)
	}
	if !me.Admin {
		t.Error("an admin entry did not produce an admin principal")
	}
}

// TestSafeNext keeps a post-login redirect on this origin: an open
// redirect on the login route is how a phishing page borrows a trusted
// hostname.
//
// @decision(DL-007)
func TestSafeNext(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "/"},
		{"/sessions", "/sessions"},
		{"/sessions?tab=live", "/sessions?tab=live"},
		{"https://attacker.example.com/", "/"},
		{"//attacker.example.com/", "/"},
		{"http://attacker.example.com", "/"},
		{"javascript:alert(1)", "/"},
		{"sessions", "/"},
	} {
		if got := safeNext(tc.in); got != tc.want {
			t.Errorf("safeNext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLoginHonoursASafeNext end to end.
//
// @decision(DL-007)
func TestLoginHonoursASafeNext(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.doLogin("/sessions?tab=live")
	if got := resp.Header.Get("Location"); got != "/sessions?tab=live" {
		t.Errorf("landed at %q", got)
	}
}

// TestLoginRefusesAnOffOriginNext even when it travelled through the
// session rather than the query string.
//
// @decision(DL-007)
func TestLoginRefusesAnOffOriginNext(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.doLogin("https://attacker.example.com/")
	if got := resp.Header.Get("Location"); got != "/" {
		t.Errorf("landed at %q, want /", got)
	}
}

// TestLoginRoutesNeedNoCredential: they are how a browser gets one.
//
// @decision(DL-007)
func TestLoginRoutesNeedNoCredential(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	if resp := f.get(LoginPath); resp.StatusCode == http.StatusUnauthorized {
		t.Error("the login route demanded a credential")
	}
	// The API, by contrast, still does.
	if resp := f.get("/api/v1/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the API without a session = %d, want 401", resp.StatusCode)
	}
}

// TestLoginRoutesAreBehindTheBrowserGuard: they are the most
// browser-facing surface there is, so a request from another site must
// not reach them.
//
// @decision(DL-007) @decision(DL-006)
func TestLoginRoutesAreBehindTheBrowserGuard(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	req, err := http.NewRequest(http.MethodGet, f.http.URL+LoginPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "attacker.example.com"
	resp := f.send(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a login request for another host = %d, want 403", resp.StatusCode)
	}
}

// TestHealthStaysOutsideTheSessionMiddleware: a probe must not touch the
// session table, or a database blip would fail liveness.
//
// @decision(DL-007) @decision(DL-010)
func TestHealthStaysOutsideTheSessionMiddleware(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.get("/livez")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("livez = %d", resp.StatusCode)
	}
	// scs adds "Vary: Cookie" to everything it wraps; its absence is
	// how we know the probe did not go through it.
	if got := resp.Header.Get("Vary"); strings.Contains(got, "Cookie") {
		t.Errorf("livez went through the session middleware (Vary: %q)", got)
	}
}

// failingUsers fails the write half of the user store, so a login that
// cannot be recorded is exercised.
type failingUsers struct {
	store.UserStore
	createErr  error
	updateErr  error
	listErr    error
	byEmailErr error
}

func (f failingUsers) Create(ctx context.Context, u store.User) error {
	if f.createErr != nil {
		return f.createErr
	}
	return f.UserStore.Create(ctx, u)
}

func (f failingUsers) Update(ctx context.Context, u store.User) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	return f.UserStore.Update(ctx, u)
}

func (f failingUsers) List(ctx context.Context, orgID string) ([]store.User, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.UserStore.List(ctx, orgID)
}

func (f failingUsers) ByEmail(ctx context.Context, orgID, email string) (store.User, error) {
	if f.byEmailErr != nil {
		return store.User{}, f.byEmailErr
	}
	return f.UserStore.ByEmail(ctx, orgID, email)
}

// TestCallbackReportsGitHubsOwnError: a user who refused consent comes
// back with error= rather than a code.
//
// @decision(DL-007)
func TestCallbackReportsGitHubsOwnError(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.get(LoginPath)
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := to.Query().Get("state")

	got := f.get(CallbackPath + "?error=access_denied&state=" + url.QueryEscape(state))
	if got.StatusCode != http.StatusForbidden {
		t.Errorf("a refused consent = %d, want 403", got.StatusCode)
	}
	if _, err := f.store.Users().ByEmail(context.Background(), DefaultOrgID, "alice@example.com"); err == nil {
		t.Error("a refused consent created a user")
	}
}

// TestCallbackWithoutACode.
//
// @decision(DL-007)
func TestCallbackWithoutACode(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.get(LoginPath)
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	got := f.get(CallbackPath + "?state=" + url.QueryEscape(to.Query().Get("state")))
	if got.StatusCode != http.StatusBadRequest {
		t.Errorf("a callback with no code = %d, want 400", got.StatusCode)
	}
}

// TestCallbackReportsAFailedExchange without logging anyone in.
//
// @decision(DL-007)
func TestCallbackReportsAFailedExchange(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	// A GitHub that refuses the exchange.
	f.login.OAuth.Config.Endpoint.TokenURL = f.github.URL + "/nope"

	resp := f.get(LoginPath)
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	got := f.get(CallbackPath + "?code=c&state=" + url.QueryEscape(to.Query().Get("state")))
	if got.StatusCode != http.StatusBadGateway {
		t.Errorf("a failed exchange = %d, want 502", got.StatusCode)
	}
	// The client secret must not reach the page.
	if strings.Contains(string(got.Body), "client-secret") {
		t.Error("the error page carries the client secret")
	}
}

// TestLoginReportsAStoreFailureRatherThanSigningIn: a user that could
// not be written must not get a session, or the next request would 401
// with no explanation.
//
// @decision(DL-007)
func TestLoginReportsAStoreFailureRatherThanSigningIn(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.login.Users = failingUsers{UserStore: f.store.Users(), createErr: errors.New("the database is down")}

	resp := f.doLogin("")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a failed create = %d, want 500", resp.StatusCode)
	}
	if strings.Contains(string(resp.Body), "the database is down") {
		t.Error("the page leaked the store's error")
	}
	if got := f.get("/api/v1/me"); got.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after a failed login = %d, want 401", got.StatusCode)
	}
}

// TestSecondLoginReportsAFailedUpdate, which is the other half of
// upsertUser.
//
// @decision(DL-007)
func TestSecondLoginReportsAFailedUpdate(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")
	f.logout()

	f.login.Users = failingUsers{UserStore: f.store.Users(), updateErr: errors.New("the database is down")}
	resp := f.doLogin("")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("a failed update = %d, want 500", resp.StatusCode)
	}
}

// TestAUserWithNoGitHubNameFallsBackToTheLogin, so the git identity is
// never empty.
//
// @decision(DL-007)
func TestAUserWithNoGitHubNameFallsBackToTheLogin(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.github.name = ""
	f.doLogin("")

	user, err := f.store.Users().ByEmail(context.Background(), DefaultOrgID, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if user.GitName != "octocat" {
		t.Errorf("git name = %q, want the GitHub login", user.GitName)
	}
}

// TestRevokeSessionsReportsAFailedLookup rather than panicking or
// revoking everything. The entry is already gone by then, so the caller
// warns rather than failing the delete — but it has to be told.
//
// @decision(DL-007)
func TestRevokeSessionsReportsAFailedLookup(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")
	f.login.Users = failingUsers{UserStore: f.store.Users(), byEmailErr: errors.New("the database is down")}

	if err := f.login.RevokeSessions(context.Background(), DefaultOrgID, "alice@example.com"); err == nil {
		t.Error("a failed lookup was reported as success")
	}

	// The session is untouched: a lookup that failed says nothing about
	// whose sessions to end.
	f.login.Users = f.store.Users()
	if got := f.get("/api/v1/me"); got.StatusCode != http.StatusOK {
		t.Errorf("me = %d after a failed lookup, want it left alone", got.StatusCode)
	}
}

// TestRevokeSessionsWithoutAStoreDoesNothing: the SIGHUP watcher may
// hold a Login that was never fully built.
//
// @decision(DL-007)
func TestRevokeSessionsWithoutAStoreDoesNothing(t *testing.T) {
	if err := (&Login{}).RevokeSessions(context.Background(), DefaultOrgID, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
}

// TestTheCallbackSurvivesACrossSiteNavigation: the callback always
// arrives as a top-level cross-site navigation, since GitHub redirects
// the browser back, so the guard must not refuse it on Sec-Fetch-Site.
//
// @decision(DL-007) @decision(DL-006)
func TestTheCallbackSurvivesACrossSiteNavigation(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.get(LoginPath)
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := to.Query().Get("state")

	req, err := http.NewRequest(http.MethodGet,
		f.http.URL+CallbackPath+"?code=the-code&state="+url.QueryEscape(state), nil)
	if err != nil {
		t.Fatal(err)
	}
	// What a browser actually sends returning from github.com.
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	got := f.send(req)
	if got.StatusCode == http.StatusForbidden {
		t.Fatalf("the guard refused a real browser's callback: %s", got.Body)
	}
	if got.StatusCode != http.StatusSeeOther {
		t.Fatalf("the callback = %d, want a redirect", got.StatusCode)
	}
	// It really logged in, rather than merely not being refused.
	if _, err := f.store.Users().ByEmail(context.Background(), DefaultOrgID, "alice@example.com"); err != nil {
		t.Errorf("the cross-site callback did not create the user: %v", err)
	}
}

// TestOnlyTheCallbackIsExemptFromTheCrossSiteCheck: the exemption must
// be one path and one method, not a hole in the guard.
//
// @decision(DL-007) @decision(DL-006)
func TestOnlyTheCallbackIsExemptFromTheCrossSiteCheck(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	crossSite := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/me"},
		{http.MethodGet, "/api/v1/sessions"},
		{http.MethodGet, LoginPath},
		{http.MethodPost, LogoutPath},
		// The callback path itself, by a method a redirect never uses.
		{http.MethodPost, CallbackPath},
	} {
		req, err := http.NewRequest(tc.method, f.http.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		crossSite(req)
		if got := f.send(req); got.StatusCode != http.StatusForbidden {
			t.Errorf("cross-site %s %s = %d, want 403", tc.method, tc.path, got.StatusCode)
		}
	}
}

// TestTheCallbackStillChecksTheHost: the cross-site exemption relaxes
// Sec-Fetch-Site and Origin, and nothing else. A DNS-rebinding attempt
// still arrives with the wrong Host.
//
// @decision(DL-007) @decision(DL-006)
func TestTheCallbackStillChecksTheHost(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	req, err := http.NewRequest(http.MethodGet, f.http.URL+CallbackPath+"?code=c&state=s", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "attacker.example.com"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if got := f.send(req); got.StatusCode != http.StatusForbidden {
		t.Errorf("a callback for another host = %d, want 403", got.StatusCode)
	}
}

// TestAnAbandonedLoginExpiresQuickly: an abandoned flow's session row
// expires at FlowLifetime, not the full session lifetime.
//
// @decision(DL-007)
func TestAnAbandonedLoginExpiresQuickly(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.get(LoginPath)

	deadlines := f.sessionDeadlines()
	if len(deadlines) != 1 {
		t.Fatalf("an anonymous /login wrote %d rows, want 1", len(deadlines))
	}
	// Minutes, not the seven-day session lifetime.
	if until := time.Until(deadlines[0]); until > FlowLifetime+time.Minute {
		t.Errorf("an abandoned login lives for %s, want about %s", until, FlowLifetime)
	}
}

// TestACompletedLoginGetsTheFullLifetime: the short flow deadline must
// not follow the session past the login, or everyone would be logged
// out a quarter of an hour after signing in.
//
// @decision(DL-007)
func TestACompletedLoginGetsTheFullLifetime(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	f.doLogin("")

	deadlines := f.sessionDeadlines()
	if len(deadlines) != 1 {
		t.Fatalf("got %d sessions after a login", len(deadlines))
	}
	if until := time.Until(deadlines[0]); until < 6*24*time.Hour {
		t.Errorf("a completed login lives for %s, want the full lifetime", until)
	}
}

// sessionDeadlines is every live session's expiry.
func (f *loginFixture) sessionDeadlines() []time.Time {
	f.t.Helper()
	var out []time.Time
	err := f.login.Sessions.Iterate(context.Background(), func(ctx context.Context) error {
		out = append(out, f.login.Sessions.Deadline(ctx))
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// TestConcurrentFirstLoginsYieldOneUser: two first logins for the same
// address race, and the unique index lets one through; the loser must
// read the winner's row rather than answer 500 to a valid login.
//
// @decision(DL-007)
func TestConcurrentFirstLoginsYieldOneUser(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	// The store the login writes through reports a conflict, as the
	// unique index does when the other request won the race.
	f.login.Users = failingUsers{UserStore: f.store.Users(), createErr: store.ErrConflict}
	// The winner's row, already there.
	winner := store.User{ID: "the-winner", OrgID: DefaultOrgID, Email: "alice@example.com"}
	if err := f.store.Users().Create(context.Background(), winner); err != nil {
		t.Fatal(err)
	}

	if resp := f.doLogin(""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the losing login = %d, want it to succeed on the winner's row", resp.StatusCode)
	}
	// And it is signed in as the winner, not as a second user.
	apiResp := f.get("/api/v1/me")
	var me struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(apiResp.Body, &me); err != nil {
		t.Fatal(err)
	}
	if me.UserID != winner.ID {
		t.Errorf("signed in as %q, want the winner %q", me.UserID, winner.ID)
	}
}

// TestGitHubsRefusalIsLogged: redirect_uri_mismatch arrives this way,
// and it is the only clue an operator gets.
//
// @decision(DL-007)
func TestGitHubsRefusalIsLogged(t *testing.T) {
	f := newLoginFixture(t, "alice@example.com\n")
	resp := f.get(LoginPath)
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	f.get(CallbackPath + "?error=redirect_uri_mismatch&state=" + url.QueryEscape(to.Query().Get("state")))

	found := false
	for _, w := range f.warnings {
		if strings.Contains(w, "redirect_uri_mismatch") {
			found = true
		}
	}
	if !found {
		t.Errorf("GitHub's error code was not logged: %v", f.warnings)
	}
}
