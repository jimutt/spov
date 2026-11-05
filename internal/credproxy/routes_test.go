package credproxy

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

var (
	npmRoute = Route{Name: "acme-npm", Hosts: []string{"npm.acme.com"}, Scheme: SchemeBearer, Secret: "npm_realsecret"}
	mvnRoute = Route{Name: "acme-maven", Hosts: []string{"repo.acme.com"}, Paths: []string{"/artifactory/libs/"},
		Scheme: SchemeBasic, User: "ci", Secret: "maven-real"}
	hdrRoute = Route{Name: "jfrog", Hosts: []string{"jfrog.acme.com", "cdn.acme.com"}, Scheme: SchemeHeader,
		Header: "X-JFrog-Art-Api", Secret: "jfrog-real"}
)

// A route's secret is set in its own scheme, on its own hosts and only on
// its paths. Every other request to the host is forwarded with the
// client's credential headers removed and nothing set, and other hosts
// never see the secret.
//
// @decision(DL-072)
func TestARouteSetsItsSecretOnlyOnItsHostsAndPaths(t *testing.T) {
	s := newKeyStore(t, SidecarConfig{Platforms: []Platform{forgejo}, Providers: []Provider{anthropic},
		Routes: []Route{npmRoute, mvnRoute, hdrRoute}})
	maven := "Basic " + base64.StdEncoding.EncodeToString([]byte("ci:maven-real"))
	for _, c := range []struct {
		method, url, header, want string
	}{
		{http.MethodGet, "https://npm.acme.com/@acme%2fpkg", "Authorization", "Bearer npm_realsecret"},
		{http.MethodPut, "https://npm.acme.com/@acme/pkg", "Authorization", "Bearer npm_realsecret"},
		{http.MethodGet, "https://repo.acme.com/artifactory/libs/com/acme/a/1/a-1.jar", "Authorization", maven},
		{http.MethodGet, "https://jfrog.acme.com/api/npm/x", "X-Jfrog-Art-Api", "jfrog-real"},
		{http.MethodGet, "https://cdn.acme.com/blob", "X-Jfrog-Art-Api", "jfrog-real"},
	} {
		r := originForm(httptest.NewRequest(c.method, c.url, nil))
		r.Header.Set("Authorization", "Bearer from-the-client")
		stripCredentialHeaders(r.Header)
		if !s.Rewrite(r) || r.Header.Get(c.header) != c.want {
			t.Errorf("%s %s: %s = %q", c.method, c.url, c.header, r.Header.Get(c.header))
		}
		if c.header != "Authorization" && r.Header.Get("Authorization") != "" {
			t.Errorf("%s: the client's Authorization survived", c.url)
		}
		if s.Streamed(r) {
			t.Errorf("%s: a route's stream would pass through unread", c.url)
		}
	}
	for _, url := range []string{
		"https://repo.acme.com/artifactory/other/a.jar",       // outside the prefix
		"https://repo.acme.com/artifactory/libs/%2e%2e/other", // an escape
		"https://repo.acme.com/artifactory/libs/../other/a",   // a dot segment
		"https://repo.acme.com/artifactory//libs/a",           // an empty segment
		"https://npm.acme.com.evil.example/pkg",               // a lookalike host
		"https://acme.com/pkg",                                // the parent domain
	} {
		r := originForm(httptest.NewRequest(http.MethodGet, url, nil))
		if s.Rewrite(r) || r.Header.Get("Authorization") != "" {
			t.Errorf("%s got the secret: %v", url, r.Header)
		}
	}
	r := originForm(httptest.NewRequest(http.MethodGet, "https://repo.acme.com/artifactory/libs/a", nil))
	if s.Rewrite(r); s.RouteName(r) != "acme-maven" {
		t.Errorf("the audit names %q", s.RouteName(r))
	}
	if got := s.Secrets(r); !slices.Equal(got, []string{"maven-real", strings.TrimPrefix(maven, "Basic ")}) {
		t.Errorf("secrets %v", got)
	}
}

// A header route removes the client's own value of its header, in any
// spelling with underscores, even on a path it does not cover, so a
// client cannot send a second value beside the secret.
//
// @decision(DL-072)
func TestARoutesHeaderIsTheProxysAlone(t *testing.T) {
	narrowed := hdrRoute
	narrowed.Paths = []string{"/api/"}
	s := newKeyStore(t, SidecarConfig{Routes: []Route{narrowed}})
	for _, url := range []string{"https://jfrog.acme.com/api/x", "https://jfrog.acme.com/other"} {
		r := originForm(httptest.NewRequest(http.MethodGet, url, nil))
		r.Header["X_JFrog_Art_Api"] = []string{"spoofed"}
		r.Header.Set("X-JFrog-Art-Api", "spoofed")
		s.Rewrite(r)
		for name, v := range r.Header {
			if slices.Contains(v, "spoofed") {
				t.Errorf("%s: the client's %s survived", url, name)
			}
		}
	}
}

// A route is checked on its own before the proxy trusts it.
//
// @decision(DL-072)
func TestABadRouteIsRefused(t *testing.T) {
	long := make([]string, MaxRouteHosts+1)
	for i := range long {
		long[i] = string(rune('a'+i)) + ".acme.com"
	}
	for name, r := range map[string]Route{
		"a bad name":        {Name: "Acme_NPM", Hosts: []string{"npm.acme.com"}, Scheme: SchemeBearer, Secret: "s"},
		"no host":           {Name: "a", Scheme: SchemeBearer, Secret: "s"},
		"a wildcard":        {Name: "a", Hosts: []string{"*.acme.com"}, Scheme: SchemeBearer, Secret: "s"},
		"a port":            {Name: "a", Hosts: []string{"npm.acme.com:8443"}, Scheme: SchemeBearer, Secret: "s"},
		"an IP":             {Name: "a", Hosts: []string{"10.0.0.1"}, Scheme: SchemeBearer, Secret: "s"},
		"a single label":    {Name: "a", Hosts: []string{"registry"}, Scheme: SchemeBearer, Secret: "s"},
		"an uppercase host": {Name: "a", Hosts: []string{"NPM.acme.com"}, Scheme: SchemeBearer, Secret: "s"},
		"a host twice":      {Name: "a", Hosts: []string{"npm.acme.com", "npm.acme.com"}, Scheme: SchemeBearer, Secret: "s"},
		"too many hosts":    {Name: "a", Hosts: long, Scheme: SchemeBearer, Secret: "s"},
		"a method path":     {Name: "a", Hosts: []string{"npm.acme.com"}, Paths: []string{"GET /a"}, Scheme: SchemeBearer, Secret: "s"},
		"an escaped path":   {Name: "a", Hosts: []string{"npm.acme.com"}, Paths: []string{"/a%2f"}, Scheme: SchemeBearer, Secret: "s"},
		"no secret":         {Name: "a", Hosts: []string{"npm.acme.com"}, Scheme: SchemeBearer},
		"a newline":         {Name: "a", Hosts: []string{"npm.acme.com"}, Scheme: SchemeBearer, Secret: "s\r\nX: y"},
		"a huge secret":     {Name: "a", Hosts: []string{"npm.acme.com"}, Scheme: SchemeBearer, Secret: strings.Repeat("s", MaxRouteSecret+1)},
		"no scheme":         {Name: "a", Hosts: []string{"npm.acme.com"}, Secret: "s"},
		"basic, no user":    {Name: "a", Hosts: []string{"npm.acme.com"}, Scheme: SchemeBasic, Secret: "s"},
		"basic, a colon":    {Name: "a", Hosts: []string{"npm.acme.com"}, Scheme: SchemeBasic, User: "a:b", Secret: "s"},
		"a bad header name": {Name: "a", Hosts: []string{"npm.acme.com"}, Scheme: SchemeHeader, Header: "X Api", Secret: "s"},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := npmRoute.Validate(); err != nil {
		t.Errorf("a good route was refused: %v", err)
	}
}

// A route may not set a header that changes how a request is routed or
// framed, or one the proxy owns.
//
// @decision(DL-072)
func TestARouteCannotNameAForbiddenHeader(t *testing.T) {
	for _, h := range []string{"Host", "cookie", "Content-Length", "Transfer-Encoding", "Connection", "Upgrade",
		"TE", "Trailer", "Keep-Alive", "Expect", "Forwarded", "Proxy-Authorization", "proxy_authorization",
		"X-Forwarded-For", "x-forwarded-host"} {
		r := Route{Name: "a", Hosts: []string{"api.acme.com"}, Scheme: SchemeHeader, Header: h, Secret: "s"}
		if err := r.Validate(); err == nil {
			t.Errorf("a route may set %s", h)
		}
	}
	for _, h := range []string{"Authorization", "X-Api-Key", "Private-Token", "X-JFrog-Art-Api"} {
		r := Route{Name: "a", Hosts: []string{"api.acme.com"}, Scheme: SchemeHeader, Header: h, Secret: "s"}
		if err := r.Validate(); err != nil {
			t.Errorf("a route may not set %s: %v", h, err)
		}
	}
}

// A route's hosts are its own: a forge's, a provider's or another route's
// host is refused, as is a name listed twice.
//
// @decision(DL-072)
func TestARouteHostCannotOverlapAForgeOrProvider(t *testing.T) {
	on := func(host string) Route {
		return Route{Name: "r", Hosts: []string{host}, Scheme: SchemeBearer, Secret: "s"}
	}
	gh := Platform{Name: "gh", Kind: KindGitHub, Host: "github.com", Token: "t", Scoped: true}
	for name, cfg := range map[string]SidecarConfig{
		"a forge host":         {Platforms: []Platform{forgejo}, Routes: []Route{on("codefloe.com")}},
		"GitHub's API host":    {Platforms: []Platform{gh}, Routes: []Route{on("api.github.com")}},
		"a provider host":      {Providers: []Provider{anthropic}, Routes: []Route{on("api.anthropic.com")}},
		"another route's host": {Routes: []Route{npmRoute, {Name: "b", Hosts: []string{"npm.acme.com"}, Scheme: SchemeBearer, Secret: "s"}}},
		"one name twice":       {Routes: []Route{npmRoute, npmRoute}},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A route's hosts are decrypted and covered by the sandbox's CA.
//
// @decision(DL-072)
func TestARouteHostJoinsTheDecryptedHostsAndTheCA(t *testing.T) {
	cfg := SidecarConfig{Platforms: []Platform{forgejo}, Routes: []Route{npmRoute, hdrRoute}}
	for _, h := range []string{"npm.acme.com", "jfrog.acme.com", "cdn.acme.com"} {
		if !slices.Contains(cfg.DecryptHosts(), h) || !slices.Contains(cfg.CAHosts(), h) {
			t.Errorf("%s is not decrypted or not in the CA: %v", h, cfg.CAHosts())
		}
	}
}

// A route's secret echoed by the upstream, raw or Basic-encoded, never
// reaches the client, and the audit entry names the route.
//
// @decision(DL-072)
func TestARoutesEchoedSecretIsScrubbed(t *testing.T) {
	route := Route{Name: "acme", Hosts: []string{"api.example.com"}, Scheme: SchemeBasic, User: "ci", Secret: "route-real"}
	ca, _ := testCA(t, "api.example.com")
	store := newKeyStore(t, SidecarConfig{Routes: []Route{route}})
	r := newRig(t, Config{
		CA: ca, Decrypt: func(h string) bool { return h == "api.example.com" },
		Rewrite: store.Rewrite, Secrets: store.Secrets, Streamed: store.Streamed, RouteName: store.RouteName,
	}, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Echo", req.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"auth":"`+req.Header.Get("Authorization")+`"}`)
	})
	resp, err := r.client.Get("https://api.example.com/v1/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	encoded := base64.StdEncoding.EncodeToString([]byte("ci:route-real"))
	for _, leaked := range []string{"route-real", encoded} {
		if strings.Contains(string(body), leaked) || strings.Contains(resp.Header.Get("X-Echo"), leaked) {
			t.Errorf("the response carried %q: %s / %s", leaked, body, resp.Header.Get("X-Echo"))
		}
	}
	if audit := r.audits(1); len(audit) != 1 || audit[0].Route != "acme" {
		t.Errorf("audit %+v", audit)
	}
}

// The placeholder's variable follows the name.
//
// @decision(DL-072)
func TestARoutesPlaceholderVariableFollowsItsName(t *testing.T) {
	if got := RouteEnvVar("acme-npm-2"); got != "SPOV_CREDENTIAL_ACME_NPM_2" {
		t.Errorf("got %s", got)
	}
}
