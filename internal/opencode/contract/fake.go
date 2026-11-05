package contract

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"testing"
)

// Checked wraps a fake OpenCode server's handler so that every request
// spov sends it is checked against the committed document. A request
// that fails is reported on t and still reaches the handler, so the
// test's own assertions run as well. A sandbox.Fake connect prefix
// (/connect/<sandbox>/<port>) is dropped before the path is checked.
//
// @decision(DL-035)
func Checked(t testing.TB, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("contract: reading %s %s: %v", r.Method, r.URL, err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		check(t, r.Method, fromAPI(r.URL), body)
		h.ServeHTTP(w, r)
	})
}

// CheckRequest checks one request given as a method, a path with its
// query, and a body, for a fake that is not an HTTP server.
//
// @decision(DL-035)
func CheckRequest(t testing.TB, method, target string, body []byte) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Errorf("contract: %v", err)
		return
	}
	check(t, method, u, body)
}

// connectPrefix is the path sandbox.Fake puts before a sandbox's own.
var connectPrefix = regexp.MustCompile(`^/connect/[^/]+/[^/]+/`)

// fromAPI drops a fake connect URL's prefix.
func fromAPI(u *url.URL) *url.URL {
	path := u.EscapedPath()
	loc := connectPrefix.FindStringIndex(path)
	if loc == nil {
		return u
	}
	out := *u
	out.RawPath = path[loc[1]-1:]
	out.Path, _ = url.PathUnescape(out.RawPath)
	return &out
}

func check(t testing.TB, method string, u *url.URL, body []byte) {
	t.Helper()
	d, err := Load()
	if err != nil {
		t.Errorf("contract: %v", err)
		return
	}
	if _, err := d.Check(method, u, body); err != nil {
		t.Errorf("contract: %v", err)
	}
}
