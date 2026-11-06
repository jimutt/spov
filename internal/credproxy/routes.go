package credproxy

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// The ways a routed credential is presented.
//
// @decision(DL-072)
const (
	SchemeBearer = "bearer"
	SchemeBasic  = "basic"
	SchemeHeader = "header"
)

// Limits on one routed credential.
//
// @decision(DL-072)
const (
	MaxRouteHosts  = 8
	MaxRoutePaths  = 8
	MaxRouteSecret = 8 << 10
	MaxRouteName   = 40
)

// routeName is a routed credential's name: lowercase letters, digits and
// "-", starting with a letter.
var routeName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidRouteName reports whether name can name a routed credential.
//
// @decision(DL-072)
func ValidRouteName(name string) bool {
	return len(name) <= MaxRouteName && routeName.MatchString(name)
}

// Route is a routed credential as the sidecar holds it: a secret and
// where it is set.
//
// @decision(DL-072)
type Route struct {
	// Name is the credential's name, which the audit log and the
	// placeholder's variable carry.
	Name string `json:"name"`
	// Hosts are exact host names, on port 443.
	Hosts []string `json:"hosts"`
	// Paths, when set, are the path prefixes the secret is set on, as a
	// provider's Paths are but without a method. Empty means any path.
	Paths []string `json:"paths,omitempty"`
	// Scheme is SchemeBearer, SchemeBasic or SchemeHeader.
	Scheme string `json:"scheme"`
	// User is the Basic username.
	User string `json:"user,omitempty"`
	// Header is the header a SchemeHeader secret is set in.
	Header string `json:"header,omitempty"`
	// Secret is the real credential. It never leaves the sidecar.
	Secret string `json:"secret"`
}

// forbiddenRouteHeaders are the headers a route may not set: the proxy's
// own, and those that change how a request is routed or framed.
//
// @decision(DL-072)
var forbiddenRouteHeaders = []string{"Host", "Cookie", "Content-Length", "Transfer-Encoding", "Connection",
	"Upgrade", "Te", "Trailer", "Keep-Alive", "Expect", "Forwarded"}

// ForbiddenRouteHeader reports whether a route may not set name.
//
// @decision(DL-072)
func ForbiddenRouteHeader(name string) bool {
	c := http.CanonicalHeaderKey(strings.ReplaceAll(name, "_", "-"))
	return slices.Contains(forbiddenRouteHeaders, c) ||
		strings.HasPrefix(c, "Proxy-") || strings.HasPrefix(c, "X-Forwarded-")
}

// headerToken matches an HTTP header name (RFC 9110's token).
var headerToken = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// ValidRouteHost accepts an exact, lowercase DNS name of at least two
// labels: no wildcard, port, IP literal or trailing dot.
//
// @decision(DL-072)
func ValidRouteHost(h string) bool {
	if h == "" || len(h) > 253 || h != strings.ToLower(h) || net.ParseIP(h) != nil {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// Validate checks one route on its own; SidecarConfig.Validate checks it
// against the session's other credentials.
//
// @decision(DL-072)
func (r Route) Validate() error {
	switch {
	case !ValidRouteName(r.Name):
		return fmt.Errorf("credproxy: %q is not a routed credential name", r.Name)
	case len(r.Hosts) == 0:
		return fmt.Errorf("credproxy: routed credential %s has no host", r.Name)
	case len(r.Hosts) > MaxRouteHosts:
		return fmt.Errorf("credproxy: routed credential %s has more than %d hosts", r.Name, MaxRouteHosts)
	case len(r.Paths) > MaxRoutePaths:
		return fmt.Errorf("credproxy: routed credential %s has more than %d paths", r.Name, MaxRoutePaths)
	case r.Secret == "":
		return fmt.Errorf("credproxy: routed credential %s has no secret", r.Name)
	case len(r.Secret) > MaxRouteSecret:
		return fmt.Errorf("credproxy: routed credential %s's secret is over %d bytes", r.Name, MaxRouteSecret)
	case strings.ContainsAny(r.Secret, "\r\n\x00"):
		return fmt.Errorf("credproxy: routed credential %s's secret is not a valid header value", r.Name)
	}
	seen := map[string]bool{}
	for _, h := range r.Hosts {
		if !ValidRouteHost(h) {
			return fmt.Errorf("credproxy: routed credential %s has a bad host %q", r.Name, h)
		}
		if seen[h] {
			return fmt.Errorf("credproxy: routed credential %s lists %s twice", r.Name, h)
		}
		seen[h] = true
	}
	for _, p := range r.Paths {
		if strings.Contains(p, " ") || !validPath(p) {
			return fmt.Errorf("credproxy: routed credential %s has a bad path %q", r.Name, p)
		}
	}
	switch r.Scheme {
	case SchemeBearer:
	case SchemeBasic:
		if r.User == "" || strings.ContainsAny(r.User, ":\r\n\x00") {
			return fmt.Errorf("credproxy: routed credential %s needs a Basic username without a colon", r.Name)
		}
	case SchemeHeader:
		if !headerToken.MatchString(r.Header) {
			return fmt.Errorf("credproxy: routed credential %s has a bad header name %q", r.Name, r.Header)
		}
		if ForbiddenRouteHeader(r.Header) {
			return fmt.Errorf("credproxy: routed credential %s may not set %s", r.Name, r.Header)
		}
	default:
		return fmt.Errorf("credproxy: routed credential %s has unknown scheme %q", r.Name, r.Scheme)
	}
	return nil
}

// covers reports whether the secret is set on req, a request to one of
// the route's hosts. The path rule is a provider's.
func (r Route) covers(req *http.Request) bool {
	return ProviderAPI{Paths: r.Paths}.allows(req)
}

// stripHeader removes a client's own value of a SchemeHeader route's
// header, in any spelling with underscores.
func (r Route) stripHeader(h http.Header) {
	if r.Scheme != SchemeHeader {
		return
	}
	want := http.CanonicalHeaderKey(r.Header)
	for name := range h {
		if http.CanonicalHeaderKey(strings.ReplaceAll(name, "_", "-")) == want {
			delete(h, name)
		}
	}
}

// set puts the secret in the request in the route's scheme.
func (r Route) set(h http.Header) {
	switch r.Scheme {
	case SchemeBearer:
		h.Set("Authorization", "Bearer "+r.Secret)
	case SchemeBasic:
		h.Set("Authorization", basic(r.User, r.Secret))
	case SchemeHeader:
		h.Set(r.Header, r.Secret)
	}
}

// secrets are the forms of the secret an upstream might echo.
func (r Route) secrets() []string {
	if r.Scheme == SchemeBasic {
		return []string{r.Secret, strings.TrimPrefix(basic(r.User, r.Secret), "Basic ")}
	}
	return []string{r.Secret}
}

// RouteEnvVar is the main container's variable holding a routed
// credential's placeholder.
//
// @decision(DL-072)
func RouteEnvVar(name string) string {
	return "SPOV_CREDENTIAL_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}
