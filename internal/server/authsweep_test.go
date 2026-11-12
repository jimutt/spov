package server

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The unauthenticated surface. Everything under /api/ and huma's own
// document paths answers 401 with no credential, and the only other
// paths that answer without one are the list below. A route added to
// the mux that is not in the list fails the source scan; a path that
// starts answering without a credential fails the probe.
//
// @decision(DL-006) @decision(DL-010) @decision(DL-035)

// unauthenticated is every raw route the server mounts that answers
// with no credential, keyed by the first argument of its mux call as the
// source writes it. Adding one is a decision to expose it to anyone who
// can reach the port: say why.
var unauthenticated = []struct {
	// pattern is the mux registration's first argument, as source text.
	pattern string
	method  string
	path    string
	// body is a form body that gets past the first parse.
	body string
	// want is the status it answers a bare request with.
	want int
	why  string
}{
	{pattern: `"GET /livez"`, method: "GET", path: "/livez", want: 204,
		why: "liveness: a load balancer has no credential and touches no dependency"},
	{pattern: `"GET /readyz"`, method: "GET", path: "/readyz", want: 204,
		why: "readiness: a fixed word, never the reason (DL-010)"},
	{pattern: `"GET /startupz"`, method: "GET", path: "/startupz", want: 204,
		why: "startup probe: the same checks with a longer budget"},
	{pattern: `"GET " + OpenCodePinPath`, method: "GET", path: OpenCodePinPath, want: 200,
		why: "the OpenCode version a client compares itself with, before it has a credential (DL-035)"},
	{pattern: `"GET " + LoginPath`, method: "GET", path: LoginPath, want: 303,
		why: "starts the GitHub sign-in; there is nobody signed in yet"},
	{pattern: `"GET " + CallbackPath`, method: "GET", path: CallbackPath, want: 400,
		why: "where GitHub sends the browser back; the state nonce in the session is what guards it"},
	{pattern: `"POST " + LogoutPath`, method: "POST", path: LogoutPath, want: 303,
		why: "destroys the caller's own session cookie, if any"},
	{pattern: `"POST " + DeviceCodePath`, method: "POST", path: DeviceCodePath, want: 200,
		why: "a CLI starting a device flow has no credential yet; it is what it is asking for"},
	{pattern: `"POST " + deviceTokenPath`, method: "POST", path: deviceTokenPath, body: "grant_type=bad", want: 400,
		why: "a CLI polling for the token an approval will mint, authenticated by the device code itself"},
	{pattern: `"GET " + DeviceVerifyPath`, method: "GET", path: DeviceVerifyPath, want: 303,
		why: "the approval page sends an anonymous visitor to sign in, and answers a cookie alone"},
	{pattern: `"POST " + DeviceVerifyPath`, method: "POST", path: DeviceVerifyPath, want: 303,
		why: "the approval form: cookie only, a CLI token or the shared bearer is refused"},
}

// TestNoPathAnswersWithoutACredentialExceptTheAllowlist probes every
// operation in the API document, every path the server reserves and a
// few that nothing serves, with no credential, a wrong bearer token and a
// forged session cookie.
//
// @decision(DL-006) @decision(DL-010) @decision(DL-035)
func TestNoPathAnswersWithoutACredentialExceptTheAllowlist(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	srv := f.server

	type cred struct {
		name string
		mod  func(*http.Request)
	}
	creds := []cred{
		{"no credential", func(*http.Request) {}},
		{"a wrong bearer token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer spov_not_a_token") }},
		{"a forged session cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "spov_session", Value: "forged-session-token"})
		}},
	}

	var checked int
	check := func(c cred, method, path string, want func(int) bool, wantText string) {
		t.Helper()
		req, err := http.NewRequest(method, f.http.URL+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		c.mod(req)
		resp := f.sendNoCookies(req)
		checked++
		if !want(resp.StatusCode) {
			t.Errorf("%s: %s %s = %d, want %s\n%s", c.name, method, path, resp.StatusCode, wantText, resp.Body)
		}
	}
	is401 := func(s int) bool { return s == http.StatusUnauthorized }

	// Every operation in the document.
	ops := operationsOf(srv.api)
	if len(ops) < 50 {
		t.Fatalf("only %d operations in the API document", len(ops))
	}
	placeholder := regexp.MustCompile(`\{[^}]+\}`)
	for key := range ops {
		method, path, _ := strings.Cut(key, " ")
		path = placeholder.ReplaceAllString(path, "x")
		for _, c := range creds {
			check(c, method, path, is401, "401")
		}
	}
	// The API's own surface, documented or not: a path nothing serves
	// must still say 401, or its 404 tells a stranger which paths exist.
	owned := []string{
		"/api", "/api/", "/api/v1", "/api/v1/", "/api/v1/nothing", "/api/v2/sessions", "/api/v1/sessions/",
		srv.reserved.docs, srv.reserved.openAPI + ".json", srv.reserved.openAPI + ".yaml", srv.reserved.schemas + "/ErrorModel.json",
	}
	for _, path := range owned {
		for _, c := range creds {
			check(c, http.MethodGet, path, is401, "401")
		}
	}

	// The allowlist answers without one, and says what it says.
	for _, u := range unauthenticated {
		req, err := http.NewRequest(u.method, f.http.URL+u.path, strings.NewReader(u.body))
		if err != nil {
			t.Fatal(err)
		}
		if u.body != "" || u.method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp := f.sendNoCookies(req)
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s %s is on the unauthenticated list but answers 401: remove it from the list", u.method, u.path)
		}
		if resp.StatusCode != u.want {
			t.Errorf("%s %s = %d, want %d\n%s", u.method, u.path, resp.StatusCode, u.want, resp.Body)
		}
	}

	// Anything else outside the API is the web app: static bytes, never
	// data, and the same for a path that does not exist.
	for _, path := range []string{"/", "/sessions/", "/session/", "/jobs/", "/settings/", "/assets/app.js", "/no/such/page"} {
		req, _ := http.NewRequest(http.MethodGet, f.http.URL+path, nil)
		resp := f.sendNoCookies(req)
		checked++
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode >= 500 {
			t.Errorf("web app %s = %d", path, resp.StatusCode)
		}
		for _, c := range resp.Cookies {
			t.Errorf("web app %s sets cookie %s", path, c.Name)
		}
		if bytes.Contains(resp.Body, []byte(`"sessions"`)) {
			t.Errorf("web app %s answers with API data: %s", path, resp.Body)
		}
	}
	if checked < 150 {
		t.Errorf("only %d requests were sent", checked)
	}
	// The API's own error shape is JSON, so the 401 is not the web app's
	// HTML either.
	req, _ := http.NewRequest(http.MethodGet, f.http.URL+"/api/v1/me", nil)
	if resp := f.sendNoCookies(req); !strings.Contains(resp.Header.Get("Content-Type"), "json") &&
		!strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("the 401 is %q", resp.Header.Get("Content-Type"))
	}
}

// TestEveryRawRouteIsOnTheUnauthenticatedList reads this package's
// source for each route registered on the mux directly, outside the
// API document, and fails on one the allowlist above does not name.
// huma registers its own document paths, which the probe above
// covers.
//
// @decision(DL-006) @decision(DL-010)
func TestEveryRawRouteIsOnTheUnauthenticatedList(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string][]string{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, call.Args[0]); err != nil {
				t.Fatal(err)
			}
			found[buf.String()] = append(found[buf.String()], name)
			return true
		})
	}
	listed := map[string]bool{}
	for _, u := range unauthenticated {
		listed[u.pattern] = true
	}
	var keys []string
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !listed[k] {
			t.Errorf("%s registers the mux route %s, which is on no authentication: if it must answer without a "+
				"credential add it to `unauthenticated` in authsweep_test.go with the reason, otherwise register "+
				"it as an API operation", found[k][0], k)
		}
	}
	for _, u := range unauthenticated {
		if _, ok := found[u.pattern]; !ok {
			t.Errorf("`unauthenticated` lists %s, which no source file registers any more: remove it", u.pattern)
		}
	}
}
