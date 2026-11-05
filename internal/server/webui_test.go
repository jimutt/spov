package server

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/alexedwards/scs/v2"
)

// stubWeb is a built web app, small enough to recognise in a response.
func stubWeb() fs.FS {
	return fstest.MapFS{
		"index.html":            {Data: []byte("<h1>THE WEB APP</h1>")},
		"404.html":              {Data: []byte("<h1>WEB 404</h1>")},
		"_astro/app.Bh4Xk1.css": {Data: []byte("body{}")},
		"sessions/index.html":   {Data: []byte("<h1>WEB SESSIONS</h1>")},
	}
}

// TestTheWebAppIsServedWithoutACredential: a logged-out browser must
// reach / and the page that offers to log it in, not a 401 problem JSON
// from the authenticated mux.
//
// @decision(DL-055) @decision(DL-010)
func TestTheWebAppIsServedWithoutACredential(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) { d.WebUI = stubWeb() })

	for _, tc := range []struct {
		path string
		want string
	}{
		{"/", "THE WEB APP"},
		{"/sessions", "WEB SESSIONS"},
		{"/nope", "WEB 404"},
	} {
		resp, body := f.do(http.MethodGet, tc.path, nil, func(r *http.Request) {
			r.Header.Del("Authorization")
		})
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("GET %s (%d) = %q, want the web app", tc.path, resp.StatusCode, body)
		}
	}
}

// TestTheWebAppSurvivesACrossSiteNavigation: a completed GitHub login
// lands on / as a top-level cross-site navigation, and the browser guard
// runs before authentication — so a guarded / answers 403 and the whole
// login flow dead-ends on its last hop. DL-007 recorded exactly that
// consequence.
//
// @decision(DL-055) @decision(DL-007) @decision(DL-006)
func TestTheWebAppSurvivesACrossSiteNavigation(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) { d.WebUI = stubWeb() })
	resp, body := f.do(http.MethodGet, "/", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		// What a browser sends arriving from github.com.
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Origin", "https://github.com")
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the guard refused the post-login landing page: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "THE WEB APP") {
		t.Errorf("body = %q", body)
	}
}

// TestTheWebAppNeverSwallowsTheServersOwnPaths: the API, its
// documentation and its schemas stay behind the guard and the
// authenticator. An unregistered path under /api must answer as the API
// too — otherwise a mistyped API call returns HTML with a 200 and the
// caller decodes a web page as a response.
//
// @decision(DL-055) @decision(DL-006)
func TestTheWebAppNeverSwallowsTheServersOwnPaths(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) { d.WebUI = stubWeb() })

	for _, path := range []string{
		"/api/v1/me",
		"/api/v1/no-such-operation",
		"/api",
		"/docs",
		"/openapi.yaml",
		"/openapi.json",
		"/openapi-3.0.yaml",
		"/schemas/SessionView.json",
		"/login",
		"/logout",
		"/oauth/github/callback",
		"/device",
		"/device/code",
	} {
		resp, body := f.do(http.MethodGet, path, nil, func(r *http.Request) {
			r.Header.Del("Authorization")
		})
		if strings.Contains(string(body), "THE WEB APP") || strings.Contains(string(body), "WEB 404") {
			t.Errorf("GET %s (%d) was answered by the web app: %q", path, resp.StatusCode, body)
		}
	}
}

// TestOwnsPath is the reserved surface written out: every path here is
// the server's, and everything else is the web app's. The predicate is
// exhaustive on purpose — a path that changes hands silently changes
// whether it is authenticated.
//
// @decision(DL-055)
func TestOwnsPath(t *testing.T) {
	f := newAPIFixture(t)

	owned := []string{
		"/api", "/api/", "/api/v1/me", "/api/v1/no-such-operation", "/api/v2/anything",
		"/docs",
		"/openapi.json", "/openapi.yaml", "/openapi-3.0.json", "/openapi-3.0.yaml",
		"/schemas", "/schemas/SessionView.json",
		"/livez", "/readyz", "/startupz",
		LoginPath, LogoutPath, CallbackPath, "/oauth/anything",
		DeviceVerifyPath, DeviceCodePath, deviceTokenPath, "/device/anything",
	}
	for _, p := range owned {
		if !f.srv.ownsPath(p) {
			t.Errorf("ownsPath(%q) = false, want true", p)
		}
	}

	web := []string{
		"/", "/sessions", "/sessions/abc", "/usage", "/tokens",
		// Settings, and /credentials, which is not a page and 404s from
		// the web branch. It is kept here deliberately: a route the
		// server might later claim is exactly what this test exists to
		// catch.
		//
		// @decision(DL-062)
		"/settings", "/settings/", "/credentials",
		// The job-creation form. `/api/v1/jobs` is the server's and
		// this is not; a later `/jobs` API surface would take the page
		// away and the form would answer as the API.
		//
		// @decision(DL-061)
		"/jobs/new", "/jobs/new/", "/jobs",
		"/_astro/app.css", "/favicon.svg", "/index.html",
		// Near misses on the reserved prefixes: these are page names,
		// not the surfaces above.
		"/apiary", "/documentation", "/devices", "/logins",
	}
	for _, p := range web {
		if f.srv.ownsPath(p) {
			t.Errorf("ownsPath(%q) = true, want false", p)
		}
	}
}

// TestABlankHumaPathDoesNotClaimEverything: huma disables one of its
// own routes by blanking its path, and HasPrefix(p, "") matches every
// string. Without a guard that would make the server claim every path
// and take the whole dashboard down.
//
// @decision(DL-055)
func TestABlankHumaPathDoesNotClaimEverything(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.reserved = reservedPaths{docs: "", openAPI: "", schemas: ""}

	for _, p := range []string{"/", "/sessions", "/_astro/app.css"} {
		if f.srv.ownsPath(p) {
			t.Errorf("ownsPath(%q) = true with the huma paths disabled", p)
		}
	}
	// What the server owns regardless still is its own.
	for _, p := range []string{"/api/v1/me", "/livez", LoginPath} {
		if !f.srv.ownsPath(p) {
			t.Errorf("ownsPath(%q) = false", p)
		}
	}
}

// TestHashedAssetsAreImmutable: the names carry a content hash, so they
// can be cached forever; the HTML that references them cannot.
//
// @decision(DL-055)
func TestHashedAssetsAreImmutable(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) { d.WebUI = stubWeb() })

	resp, _ := f.do(http.MethodGet, "/_astro/app.Bh4Xk1.css", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
	})
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q", got)
	}
	resp, _ = f.do(http.MethodGet, "/", nil, func(r *http.Request) { r.Header.Del("Authorization") })
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("html Cache-Control = %q", got)
	}
}

// TestTheWebAppDoesNotTouchTheSession: a logged-in browser sends its
// cookie with every asset request. Loading and saving a session for each
// one would put the session store on the critical path of every static
// byte the page pulls.
//
// @decision(DL-055) @decision(DL-007)
func TestTheWebAppDoesNotTouchTheSession(t *testing.T) {
	f := newLoginFixtureWith(t, "alice@example.com\n", func(d *Deps, _ *Login) {
		d.WebUI = stubWeb()
	})
	f.doLogin("/")

	counted := &countingSessionStore{inner: f.login.Sessions.Store}
	f.login.Sessions.Store = counted

	if resp := f.get("/_astro/app.Bh4Xk1.css"); resp.StatusCode != http.StatusOK {
		t.Fatalf("asset = %d", resp.StatusCode)
	}
	if n := counted.finds; n != 0 {
		t.Errorf("an asset request hit the session store %d times", n)
	}
	// The API, by contrast, must still resolve the cookie.
	if resp := f.get("/api/v1/me"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/v1/me = %d: %s", resp.StatusCode, resp.Body)
	}
	if counted.finds == 0 {
		t.Error("the API did not read the session")
	}
}

// TestTheDefaultWebUIIsTheEmbeddedOne: a server built with no WebUI
// serves what this binary carries, rather than nothing at all.
//
// @decision(DL-055)
func TestTheDefaultWebUIIsTheEmbeddedOne(t *testing.T) {
	f := newAPIFixture(t)
	resp, body := f.do(http.MethodGet, "/", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ = %d: %s", resp.StatusCode, body)
	}
	if len(body) == 0 {
		t.Error("/ served nothing")
	}
}

// countingSessionStore records how many session lookups a request causes.
type countingSessionStore struct {
	inner scs.Store
	finds int
}

func (c *countingSessionStore) Find(token string) ([]byte, bool, error) {
	c.finds++
	return c.inner.Find(token)
}

func (c *countingSessionStore) Commit(token string, b []byte, expiry time.Time) error {
	return c.inner.Commit(token, b, expiry)
}

func (c *countingSessionStore) Delete(token string) error { return c.inner.Delete(token) }
