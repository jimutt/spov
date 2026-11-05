package opencode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode/contract"
)

// newServer starts a stand-in OpenCode server that enforces Basic auth the
// way the real one does.
func newServer(t *testing.T, password string, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != BasicAuthUser || pass != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r)
	})))
	t.Cleanup(srv.Close)
	return srv
}

func TestClientHealth(t *testing.T) {
	srv := newServer(t, "pw", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/info" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"version":"2.0.22","pid":2,"urls":[],"paths":{"tmp":"/tmp"}}`))
	})

	h, err := NewClient(srv.URL, "pw").Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !h.Healthy || h.Version != "2.0.22" || h.PID != 2 {
		t.Errorf("health = %+v", h)
	}
}

func TestClientHealthUnauthorized(t *testing.T) {
	srv := newServer(t, "right", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"2.0.22","pid":2,"urls":[],"paths":{"tmp":"/tmp"}}`))
	})

	_, err := NewClient(srv.URL, "wrong").Health(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestClientHealthServerError(t *testing.T) {
	srv := newServer(t, "pw", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := NewClient(srv.URL, "pw").Health(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Error("a 502 must not be reported as an auth failure")
	}
}

func TestClientHealthBadJSON(t *testing.T) {
	srv := newServer(t, "pw", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})
	if _, err := NewClient(srv.URL, "pw").Health(context.Background()); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestClientHealthUnreachable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "pw")
	c.HTTP = &http.Client{Timeout: time.Second}
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("expected a connection error")
	}
}

// TestWaitHealthyRetries covers the real cold-start shape: the server
// refuses connections, then answers unhealthy, then becomes healthy.
func TestWaitHealthyRetries(t *testing.T) {
	var calls atomic.Int32
	srv := newServer(t, "pw", func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			_, _ = w.Write([]byte(`{"pid":2}`))
		default:
			_, _ = w.Write([]byte(`{"version":"v1","pid":2,"urls":[],"paths":{"tmp":"/tmp"}}`))
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, err := NewClient(srv.URL, "pw").WaitHealthy(ctx, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != "v1" {
		t.Errorf("health = %+v", h)
	}
	if calls.Load() < 3 {
		t.Errorf("gave up after %d calls", calls.Load())
	}
}

// TestWaitHealthyStopsOnBadPassword: retrying cannot fix a wrong password,
// so it must fail immediately rather than burn the whole timeout.
func TestWaitHealthyStopsOnBadPassword(t *testing.T) {
	var calls atomic.Int32
	srv := newServer(t, "right", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"version":"2.0.22","pid":2,"urls":[],"paths":{"tmp":"/tmp"}}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := NewClient(srv.URL, "wrong").WaitHealthy(ctx, time.Millisecond)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if time.Since(start) > time.Second {
		t.Error("WaitHealthy kept retrying an unauthorized response")
	}
	if calls.Load() != 0 {
		t.Errorf("the handler ran %d times; auth is rejected before it", calls.Load())
	}
}

func TestWaitHealthyTimesOut(t *testing.T) {
	srv := newServer(t, "pw", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"pid":2}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := NewClient(srv.URL, "pw").WaitHealthy(ctx, time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !strings.Contains(err.Error(), "did not become healthy") {
		t.Errorf("err = %v", err)
	}
}

func TestWaitHealthyDefaultsInterval(t *testing.T) {
	srv := newServer(t, "pw", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"2.0.22","pid":2,"urls":[],"paths":{"tmp":"/tmp"}}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := NewClient(srv.URL, "pw").WaitHealthy(ctx, 0); err != nil {
		t.Fatal(err)
	}
}

func TestCheckVersion(t *testing.T) {
	tests := []struct {
		name          string
		local, remote string
		wantErr       bool
		wantMismatch  bool
	}{
		{"match", "0.0.0-beta-18684", "0.0.0-beta-18684", false, false},
		{"mismatch", "0.0.0-beta-18684", "0.0.0-beta-18700", true, true},
		{"empty local", "", "v1", true, false},
		{"empty remote", "v1", "", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckVersion(tt.local, "this deployment pins", tt.remote)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			var mismatch VersionMismatchError
			if got := errors.As(err, &mismatch); got != tt.wantMismatch {
				t.Errorf("errors.As mismatch = %v, want %v (err = %v)", got, tt.wantMismatch, err)
			}
			if tt.wantMismatch {
				if !strings.Contains(err.Error(), tt.local) || !strings.Contains(err.Error(), tt.remote) {
					t.Errorf("error %q does not name both versions", err)
				}
				// The source travels too, or the message attributes the
				// expected version to nobody.
				//
				// @decision(DL-035)
				if !strings.Contains(err.Error(), "this deployment pins") {
					t.Errorf("error %q does not say where the expected version came from", err)
				}
			}
		})
	}
}

func TestClientUsesDefaultHTTPClient(t *testing.T) {
	srv := newServer(t, "pw", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"2.0.22","pid":2,"urls":[],"paths":{"tmp":"/tmp"}}`))
	})
	c := &Client{BaseURL: srv.URL, Password: "pw"} // no HTTP client set
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestClientSendsNoAuthWithoutAPassword checks a client reaching the
// server through the loopback proxy sends no Authorization header, since
// the proxy supplies the credential.
//
// @decision(DL-030)
func TestClientSendsNoAuthWithoutAPassword(t *testing.T) {
	var gotAuth string
	var sawHeader bool
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, sawHeader = r.Header["Authorization"]
		_, _ = io.WriteString(w, `{"version":"1.0.0","pid":2,"urls":[],"paths":{"tmp":"/tmp"}}`)
	})))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "").Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if sawHeader {
		t.Errorf("an Authorization header was sent without a password: %q", gotAuth)
	}
}

// TestClientSendsAuthWithAPassword guards the fix from swallowing the
// credential when there is one.
//
// @decision(DL-025)
func TestClientSendsAuthWithAPassword(t *testing.T) {
	var gotUser, gotPass string
	var ok bool
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok = r.BasicAuth()
		_, _ = io.WriteString(w, `{"version":"1.0.0","pid":2,"urls":[],"paths":{"tmp":"/tmp"}}`)
	})))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "s3cret").Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if !ok || gotUser != BasicAuthUser || gotPass != "s3cret" {
		t.Errorf("basic auth = (%q, %q, ok=%v), want (%q, s3cret, true)", gotUser, gotPass, ok, BasicAuthUser)
	}
}
