package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const token = "s3cret-token-value"

func localAuth(t *testing.T) *LocalAuthenticator {
	t.Helper()
	a, err := NewLocal(token)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestLocalAuthenticatorAcceptsExactlyItsToken.
//
// @decision(DL-006)
func TestLocalAuthenticatorAcceptsExactlyItsToken(t *testing.T) {
	a := localAuth(t)
	cases := []struct {
		name   string
		header string
		want   error
	}{
		{"exact token", "Bearer " + token, nil},
		{"scheme is case-insensitive", "bearer " + token, nil},
		{"wrong token", "Bearer " + token + "x", ErrBadCredential},
		{"prefix of the token", "Bearer " + token[:5], ErrBadCredential},
		{"no header", "", ErrUnauthenticated},
		{"empty bearer", "Bearer ", ErrUnauthenticated},
		{"basic is not a credential we know", "Basic " + token, ErrUnauthenticated},
		{"trailing garbage", "Bearer " + token + " extra", ErrUnauthenticated},
		{"tab separator", "Bearer\t" + token, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			p, err := a.Authenticate(r)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if err == nil && p != Local() {
				t.Errorf("principal = %+v, want the local principal", p)
			}
		})
	}
}

// TestLocalAuthenticatorRefusesTwoAuthorizationHeaders: a front proxy
// and this server could read different ones.
//
// @decision(DL-006)
func TestLocalAuthenticatorRefusesTwoAuthorizationHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Add("Authorization", "Bearer "+token)
	r.Header.Add("Authorization", "Bearer evil")
	if _, err := localAuth(t).Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

// TestNewLocalRefusesAnEmptyToken: an empty token would match an empty
// header.
//
// @decision(DL-006)
func TestNewLocalRefusesAnEmptyToken(t *testing.T) {
	if _, err := NewLocal(""); err == nil {
		t.Fatal("an empty token was accepted")
	}
}

// TestPrincipalCanAccess.
//
// @decision(DL-006)
func TestPrincipalCanAccess(t *testing.T) {
	owner := Principal{UserID: "u1", OrgID: "o1"}
	other := Principal{UserID: "u2", OrgID: "o1"}
	admin := Principal{UserID: "u3", OrgID: "o1", Admin: true}
	if !owner.CanAccess("u1") {
		t.Error("the owner was refused")
	}
	if other.CanAccess("u1") {
		t.Error("another user was allowed")
	}
	if !admin.CanAccess("u1") {
		t.Error("an admin was refused")
	}
	if (Principal{}).CanAccess("") {
		t.Error("an empty principal matched an empty owner")
	}
}

// TestMiddlewareStoresThePrincipalAndRejectsTheRest.
//
// @decision(DL-006)
func TestMiddlewareStoresThePrincipalAndRejectsTheRest(t *testing.T) {
	var warned []string
	var seen Principal
	h := Middleware(localAuth(t), func(s string) { warned = append(warned, s) })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := From(r.Context())
			if !ok {
				t.Error("no principal in context")
			}
			seen = p
			w.WriteHeader(http.StatusNoContent)
		}))

	do := func(header string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "127.0.0.1:5555"
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	if w := do("Bearer " + token); w.Code != http.StatusNoContent || seen != Local() {
		t.Fatalf("valid token: status %d, principal %+v", w.Code, seen)
	}

	w := do("")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: status %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body problem
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil || body.Status != 401 {
		t.Errorf("body = %+v, err = %v", body, err)
	}
	if len(warned) != 0 {
		t.Errorf("an absent credential was logged as a rejection: %v", warned)
	}

	w = do("Bearer wrong")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad credential: status %d", w.Code)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "127.0.0.1:5555") {
		t.Errorf("a bad credential was not logged with its address: %v", warned)
	}
	for _, line := range warned {
		if strings.Contains(line, "wrong") || strings.Contains(line, token) {
			t.Errorf("a credential reached the log: %q", line)
		}
	}
}

// TestStaticAuthenticator.
//
// @decision(DL-006)
func TestStaticAuthenticator(t *testing.T) {
	want := Principal{UserID: "u", OrgID: "o"}
	p, err := StaticAuthenticator{Principal: want}.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil || p != want {
		t.Errorf("got %+v, %v", p, err)
	}
	if _, err := (StaticAuthenticator{Err: ErrUnauthenticated}).Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v", err)
	}
}

// TestFromWithoutMiddleware.
func TestFromWithoutMiddleware(t *testing.T) {
	if _, ok := From(context.Background()); ok {
		t.Error("a bare context reported a principal")
	}
}

func guarded(t *testing.T) (http.Handler, *[]string) {
	t.Helper()
	var warned []string
	g, err := NewBrowserGuard("127.0.0.1:4321", func(s string) { warned = append(warned, s) })
	if err != nil {
		t.Fatal(err)
	}
	return g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})), &warned
}

// TestBrowserGuardRefusesForeignHosts: DNS rebinding arrives with the
// attacker's hostname.
//
// @decision(DL-006)
func TestBrowserGuardRefusesForeignHosts(t *testing.T) {
	h, _ := guarded(t)
	cases := map[string]int{
		"127.0.0.1:4321":    http.StatusNoContent,
		"localhost:4321":    http.StatusNoContent,
		"[::1]:4321":        http.StatusNoContent,
		"evil.example:4321": http.StatusForbidden,
		"127.0.0.1:9999":    http.StatusForbidden,
		"":                  http.StatusForbidden,
	}
	for host, want := range cases {
		t.Run("host "+host, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/x", nil)
			r.Host = host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != want {
				t.Errorf("status %d, want %d", w.Code, want)
			}
		})
	}
}

// TestBrowserGuardRefusesCrossSiteRequests: a page in another tab can
// cause a request to a loopback port; it must not reach a handler.
//
// @decision(DL-006)
func TestBrowserGuardRefusesCrossSiteRequests(t *testing.T) {
	h, warned := guarded(t)
	cases := []struct {
		name   string
		method string
		hdr    map[string]string
		want   int
	}{
		{"go client, no fetch metadata", http.MethodDelete, nil, http.StatusNoContent},
		{"same-origin page", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusNoContent},
		{"user-typed navigation", http.MethodGet, map[string]string{"Sec-Fetch-Site": "none"}, http.StatusNoContent},
		{"cross-site POST", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"cross-site DELETE without Origin", http.MethodDelete, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"cross-site GET, as an <img> sends it", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"}, http.StatusForbidden},
		{"same-site but cross-origin", http.MethodGet, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"foreign Origin, no Sec-Fetch-Site", http.MethodPost, map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"foreign Origin on a GET, no Sec-Fetch-Site", http.MethodGet, map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"null Origin on a GET", http.MethodGet, map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"own origin on a GET", http.MethodGet, map[string]string{"Origin": "http://127.0.0.1:4321"}, http.StatusNoContent},
		{"own origin over https", http.MethodPost, map[string]string{"Origin": "https://localhost:4321", "Sec-Fetch-Site": "same-origin"}, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/api/v1/sessions", nil)
			r.Host = "127.0.0.1:4321"
			for k, v := range tc.hdr {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("status %d, want %d", w.Code, tc.want)
			}
		})
	}
	if len(*warned) == 0 {
		t.Error("refusals were not logged")
	}
}

// TestNewBrowserGuardNeedsAPort.
func TestNewBrowserGuardNeedsAPort(t *testing.T) {
	if _, err := NewBrowserGuard("127.0.0.1", nil); err == nil {
		t.Error("an address without a port was accepted")
	}
}

// TestAllowCrossSiteNavigation exempts one path and one method from the
// Sec-Fetch-Site and Origin checks, for a route an identity provider
// redirects the browser to. Without it every OAuth callback is a 403.
//
// @decision(DL-007)
func TestAllowCrossSiteNavigation(t *testing.T) {
	const callback = "/oauth/github/callback"
	g, err := NewBrowserGuard("127.0.0.1:8080", nil)
	if err != nil {
		t.Fatal(err)
	}
	g.AllowCrossSiteNavigation(callback)
	served := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	do := func(method, path string, mod func(*http.Request)) int {
		r := httptest.NewRequest(method, path, nil)
		r.Host = "127.0.0.1:8080"
		if mod != nil {
			mod(r)
		}
		w := httptest.NewRecorder()
		served.ServeHTTP(w, r)
		return w.Code
	}
	crossSite := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }

	if got := do(http.MethodGet, callback, crossSite); got != http.StatusOK {
		t.Errorf("a cross-site GET of the callback = %d, want 200", got)
	}
	// A provider's redirect carries no Origin, but if one appears it
	// must not resurrect the 403 the exemption exists to prevent.
	if got := do(http.MethodGet, callback, func(r *http.Request) {
		crossSite(r)
		r.Header.Set("Origin", "https://github.com")
	}); got != http.StatusOK {
		t.Errorf("a cross-site GET with an Origin = %d, want 200", got)
	}

	// The exemption is that path and GET only.
	if got := do(http.MethodPost, callback, crossSite); got != http.StatusForbidden {
		t.Errorf("a cross-site POST of the callback = %d, want 403", got)
	}
	if got := do(http.MethodGet, "/api/v1/me", crossSite); got != http.StatusForbidden {
		t.Errorf("a cross-site GET elsewhere = %d, want 403", got)
	}
	// And the Host check is untouched.
	if got := do(http.MethodGet, callback, func(r *http.Request) {
		crossSite(r)
		r.Host = "attacker.example.com"
	}); got != http.StatusForbidden {
		t.Errorf("a callback for another host = %d, want 403", got)
	}

	// A guard with no exemption refuses it, which is the state that
	// broke login.
	plain, err := NewBrowserGuard("127.0.0.1:8080", nil)
	if err != nil {
		t.Fatal(err)
	}
	served = plain.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if got := do(http.MethodGet, callback, crossSite); got != http.StatusForbidden {
		t.Errorf("an unexempted callback = %d, want 403", got)
	}
}

// TestOptionalPassesAnonymousRequestsThrough: a browser page must be
// able to redirect an unauthenticated visitor to a login rather than
// answer 401 as the API middleware does.
//
// @decision(DL-006)
func TestOptionalPassesAnonymousRequestsThrough(t *testing.T) {
	var sawPrincipal bool
	var seen Principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, sawPrincipal = From(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	for _, tc := range []struct {
		name    string
		auth    Authenticator
		wantHas bool
		wantID  string
	}{
		{"a credential", StaticAuthenticator{Principal: Principal{UserID: "user-1"}}, true, "user-1"},
		// No credential: the handler runs anonymously and decides.
		{"none", StaticAuthenticator{Err: ErrUnauthenticated}, false, ""},
		// A wrong credential is the same outcome, since a page offers a
		// login rather than arguing about the cookie.
		{"a bad credential", StaticAuthenticator{Err: ErrBadCredential}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sawPrincipal, seen = false, Principal{}
			w := httptest.NewRecorder()
			Optional(tc.auth)(next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/device", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("the handler did not run: %d", w.Code)
			}
			if sawPrincipal != tc.wantHas {
				t.Errorf("principal present = %v, want %v", sawPrincipal, tc.wantHas)
			}
			if seen.UserID != tc.wantID {
				t.Errorf("principal = %+v", seen)
			}
		})
	}
}

// TestAllowPublicHostAcceptsTheProxiedOrigin: behind a proxy the Host
// names the public origin rather than the listener, and the guard runs
// before authentication.
//
// @decision(DL-010)
func TestAllowPublicHostAcceptsTheProxiedOrigin(t *testing.T) {
	g, err := NewBrowserGuard("127.0.0.1:8080", nil)
	if err != nil {
		t.Fatal(err)
	}
	served := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	do := func(host string) int {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		r.Host = host
		w := httptest.NewRecorder()
		served.ServeHTTP(w, r)
		return w.Code
	}

	if got := do("spov.example.com"); got != http.StatusForbidden {
		t.Errorf("an unconfigured public host = %d, want 403", got)
	}
	g.AllowPublicHost("spov.example.com")
	if got := do("spov.example.com"); got != http.StatusOK {
		t.Errorf("the public host = %d, want 200", got)
	}
	// Case-insensitively, since a Host header may arrive either way.
	if got := do("SPOV.example.COM"); got != http.StatusOK {
		t.Errorf("the public host in mixed case = %d, want 200", got)
	}
	// And nothing else became allowed.
	if got := do("attacker.example.com"); got != http.StatusForbidden {
		t.Errorf("another host = %d, want 403", got)
	}
	// The listen address still works.
	if got := do("127.0.0.1:8080"); got != http.StatusOK {
		t.Errorf("the listen address = %d, want 200", got)
	}
}
