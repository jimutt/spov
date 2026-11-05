package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// unhealthyStore fails Ping on demand, which is the only store
// behaviour the probes depend on.
type unhealthyStore struct {
	store.Store
	err error
}

func (u *unhealthyStore) Ping(context.Context) error { return u.err }

// TestLivenessNeverTouchesTheStore: a database blip that failed this
// would restart every replica at once, which is the most common way to
// turn an outage into a longer outage.
//
// @decision(DL-010)
func TestLivenessNeverTouchesTheStore(t *testing.T) {
	down := &unhealthyStore{Store: store.NewMemory(), err: errors.New("the database is down")}
	f := newAPIFixtureWith(t, func(d *Deps) { d.Store = down })

	resp, body := f.do(http.MethodGet, "/livez", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("livez = %d %s while the store was down", resp.StatusCode, body)
	}
}

// TestReadinessFailsWhenTheStoreDoes: a replica that cannot reach its
// database must leave the load balancer's rotation.
//
// @decision(DL-010)
func TestReadinessFailsWhenTheStoreDoes(t *testing.T) {
	for _, path := range []string{"/readyz", "/startupz"} {
		t.Run(path, func(t *testing.T) {
			healthy := newAPIFixture(t)
			if resp, body := healthy.do(http.MethodGet, path, nil); resp.StatusCode != http.StatusNoContent {
				t.Fatalf("%s = %d %s while healthy", path, resp.StatusCode, body)
			}
			down := &unhealthyStore{Store: store.NewMemory(), err: errors.New("the database is down")}
			f := newAPIFixtureWith(t, func(d *Deps) { d.Store = down })
			resp, body := f.do(http.MethodGet, path, nil)
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("%s = %d %s while the store was down", path, resp.StatusCode, body)
			}
			if !strings.Contains(string(body), "store unavailable") {
				t.Errorf("%s said %q", path, body)
			}
			// The probe is unauthenticated, so the reason stays in the
			// log rather than the response.
			if strings.Contains(string(body), "the database is down") {
				t.Errorf("%s leaked the failure detail: %q", path, body)
			}
		})
	}
}

// TestReadinessConsultsTheDeploymentsOwnCheck: hosted mode reports
// not-ready until its migrations are at head, which is how a replica
// running old code stays out of rotation.
//
// @decision(DL-010)
func TestReadinessConsultsTheDeploymentsOwnCheck(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Ready = func(context.Context) error { return errors.New("migrations are behind") }
	})
	resp, body := f.do(http.MethodGet, "/readyz", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "migrations are behind") {
		t.Errorf("readyz leaked the reason to an unauthenticated caller: %q", body)
	}
	if !strings.Contains(string(body), "not ready") {
		t.Errorf("readyz said %q", body)
	}
	// Liveness is unaffected: the process can answer.
	if resp, _ := f.do(http.MethodGet, "/livez", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("livez = %d while readiness was failing", resp.StatusCode)
	}
}

// TestHealthNeedsNoCredential: a load balancer has none.
//
// @decision(DL-010)
func TestHealthNeedsNoCredential(t *testing.T) {
	f := newAPIFixture(t)
	for _, path := range []string{"/livez", "/readyz", "/startupz"} {
		resp, _ := f.do(http.MethodGet, path, nil, func(r *http.Request) { r.Header.Del("Authorization") })
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s without a credential = %d", path, resp.StatusCode)
		}
	}
}

// TestPublicHostParsing: a bare hostname or a missing scheme would
// produce a guard that matches nothing, so it is refused at startup
// rather than at the first request.
//
// @decision(DL-010)
func TestPublicHostParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"https://spov.example.com", "spov.example.com"},
		{"http://localhost:8080", "localhost:8080"},
		{"https://spov.example.com/", "spov.example.com"},
		{"https://spov.example.com:8443", "spov.example.com:8443"},
	} {
		got, err := publicHost(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("publicHost(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"spov.example.com", "ftp://spov.example.com", "", "://nope"} {
		if got, err := publicHost(bad); err == nil {
			t.Errorf("publicHost(%q) = %q, want an error", bad, got)
		}
	}
}

// TestPublicURLIsAcceptedAsTheHost: behind a proxy the Host names the
// public origin, and the guard runs before authentication, so without
// this every hosted request is a 403.
//
// @decision(DL-010)
func TestPublicURLIsAcceptedAsTheHost(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) { d.PublicURL = "https://spov.example.com" })

	resp, body := f.do(http.MethodGet, "/api/v1/me", nil, func(r *http.Request) {
		r.Host = "spov.example.com"
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a request for the public host = %d %s", resp.StatusCode, body)
	}
	// Another host is still refused, so the check has not become a
	// rubber stamp.
	resp, _ = f.do(http.MethodGet, "/api/v1/me", nil, func(r *http.Request) {
		r.Host = "attacker.example.com"
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a request for another host = %d, want 403", resp.StatusCode)
	}
}

// TestWithoutAPublicURLTheListenAddressIsTheOrigin: a loopback server is
// its own origin and needs no configuration.
//
// @decision(DL-010)
func TestWithoutAPublicURLTheListenAddressIsTheOrigin(t *testing.T) {
	f := newAPIFixture(t)
	if resp, body := f.do(http.MethodGet, "/api/v1/me", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, _ := f.do(http.MethodGet, "/api/v1/me", nil, func(r *http.Request) {
		r.Host = "spov.example.com"
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("an unconfigured public host was accepted: %d", resp.StatusCode)
	}
}

// TestAMalformedPublicURLIsRefusedAtStartup.
//
// @decision(DL-010)
func TestAMalformedPublicURLIsRefusedAtStartup(t *testing.T) {
	_, err := New(Deps{Store: store.NewMemory(), Auth: auth.StaticAuthenticator{}, PublicURL: "spov.example.com"}, "127.0.0.1:8080")
	if err == nil {
		t.Fatal("a public url with no scheme was accepted")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("err = %v", err)
	}
}
