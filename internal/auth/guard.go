package auth

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// BrowserGuard refuses requests a web page could have caused, before any
// authentication runs: the Host must name this server, and
// Sec-Fetch-Site or Origin must not point at another site. An absent
// header is allowed, since Go clients (the spov CLI) send neither.
//
// @decision(DL-006)
type BrowserGuard struct {
	addr string
	port string
	// public is the externally visible host (SPOV_PUBLIC_URL's host,
	// with its port if it has one). Behind a proxy the Host header
	// names it rather than the listen address.
	//
	// @decision(DL-010)
	public string
	// navigable holds paths that a top-level cross-site navigation may
	// reach, such as the OAuth callback.
	//
	// @decision(DL-007)
	navigable map[string]bool
	cop       *http.CrossOriginProtection
	warn      func(string)
}

// NewBrowserGuard guards a server listening on addr (host:port). Any
// loopback spelling of the same port is accepted as the Host, since a
// client may say localhost, 127.0.0.1 or ::1 for the same listener.
func NewBrowserGuard(addr string, warn func(string)) (*BrowserGuard, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if warn == nil {
		warn = func(string) {}
	}
	g := &BrowserGuard{addr: addr, port: port, cop: http.NewCrossOriginProtection(), warn: warn}
	g.cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.refuse(w, "a request from another site")
	}))
	return g, nil
}

// Wrap returns next behind the guard.
func (g *BrowserGuard) Wrap(next http.Handler) http.Handler {
	inner := g.cop.Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.allowedHost(r.Host) {
			g.refuse(w, "a request for host "+r.Host)
			return
		}
		// @decision(DL-007)
		navigation := r.Method == http.MethodGet && g.navigable[r.URL.Path]
		// CrossOriginProtection exempts safe methods from both checks
		// below; DL-030's rule does not, so they run here for every
		// method and CrossOriginProtection only adds its own on the rest.
		if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" && site != "same-origin" && site != "none" && !navigation {
			g.refuse(w, "a "+site+" request")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !g.allowedOrigin(origin) && !navigation {
			g.refuse(w, "a request from origin "+origin)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// AllowPublicHost accepts the host of the externally visible origin, in
// addition to the listen address. Behind a proxy the two differ.
//
// @decision(DL-010)
func (g *BrowserGuard) AllowPublicHost(host string) { g.public = strings.ToLower(host) }

// AllowCrossSiteNavigation exempts path from the Sec-Fetch-Site and
// Origin checks, for a route an identity provider redirects the browser
// to. The Host check still applies, and the route must carry its own CSRF
// defence (for the OAuth callback, the `state` nonce). Only GET is
// exempted.
//
// @decision(DL-007)
func (g *BrowserGuard) AllowCrossSiteNavigation(path string) {
	if g.navigable == nil {
		g.navigable = map[string]bool{}
	}
	g.navigable[path] = true
}

func (g *BrowserGuard) allowedHost(host string) bool {
	if host == "" {
		return false
	}
	if host == g.addr {
		return true
	}
	if g.public != "" && strings.EqualFold(host, g.public) {
		return true
	}
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		// A Host with no port is the common shape behind a proxy on
		// 80 or 443; it can only match the public host, which the
		// comparison above already tried.
		return false
	}
	if port != g.port {
		return false
	}
	switch name {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// allowedOrigin accepts an Origin that names this server over http or
// https. "null" and anything else is another site.
func (g *BrowserGuard) allowedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return g.allowedHost(u.Host)
}

func (g *BrowserGuard) refuse(w http.ResponseWriter, what string) {
	g.warn("auth: refused " + what)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(problem{
		Title:  "Forbidden",
		Status: http.StatusForbidden,
		Detail: "this address serves only the spov CLI and its own pages",
	})
}
