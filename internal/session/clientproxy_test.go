package session_test

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

	"github.com/jimutt/spov/internal/session"
)

// upstream stands in for the provider edge plus the OpenCode server: it
// records what credentials arrived.
type upstream struct {
	*httptest.Server
	tokens atomic.Value // string: the last connect token cookie
	auths  atomic.Value // string: the last Authorization header
	hits   atomic.Int32
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		if c, err := r.Cookie("_modal_connect_token"); err == nil {
			u.tokens.Store(c.Value)
		}
		u.auths.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(u.Close)
	return u
}

// TestStartClientProxyMintsThroughTheConnector: the client holds no
// provider credential; everything it presents upstream came from the
// server's connect endpoint.
//
// @decision(DL-030)
func TestStartClientProxyMintsThroughTheConnector(t *testing.T) {
	up := newUpstream(t)
	var mints atomic.Int32
	conn := session.ConnectorFunc(func(context.Context) (session.Connection, error) {
		mints.Add(1)
		return session.Connection{URL: up.URL, Token: "tok-1", User: "opencode", Password: "pw-1"}, nil
	})

	p, err := session.StartClientProxy(context.Background(), conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := mints.Load(); got != 1 {
		t.Fatalf("%d mints before any request, want 1", got)
	}
	if !strings.HasPrefix(p.URL, "http://127.0.0.1:") {
		t.Errorf("proxy URL = %q, want loopback", p.URL)
	}

	resp, err := http.Get(p.URL + "/api/info")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if tok, _ := up.tokens.Load().(string); tok != "tok-1" {
		t.Errorf("upstream saw token %q", tok)
	}
	if auth, _ := up.auths.Load().(string); !strings.HasPrefix(auth, "Basic ") {
		t.Errorf("upstream saw Authorization %q, want Basic", auth)
	}
	// One mint serves the whole session; the proxy does not re-mint per
	// request.
	if got := mints.Load(); got != 1 {
		t.Errorf("%d mints after a request, want 1", got)
	}
}

// TestStartClientProxyFailsWhenItCannotMint: a client with no way to
// reach the sandbox must fail now, not when the user tries to use it.
//
// @decision(DL-030)
func TestStartClientProxyFailsWhenItCannotMint(t *testing.T) {
	conn := session.ConnectorFunc(func(context.Context) (session.Connection, error) {
		return session.Connection{}, errors.New("429 too many requests")
	})
	p, err := session.StartClientProxy(context.Background(), conn, nil)
	if err == nil {
		_ = p.Close()
		t.Fatal("a proxy that cannot mint was started")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error %q does not say why", err)
	}
}

// TestStartClientProxyHeartbeatsFromTheIdleTimeout: the connection's
// idle timeout is what paces the heartbeat, so an attached client keeps
// its sandbox alive.
//
// @decision(DL-030)
func TestStartClientProxyHeartbeatsFromTheIdleTimeout(t *testing.T) {
	up := newUpstream(t)
	conn := session.ConnectorFunc(func(context.Context) (session.Connection, error) {
		// 40s idle clamps to the 10s minimum, which is too slow for a
		// test; the point asserted here is that a heartbeat exists at
		// all, so the shortest legal interval is used.
		return session.Connection{URL: up.URL, Token: "t", User: "opencode", Password: "p", IdleTimeout: 40 * time.Second}, nil
	})
	p, err := session.StartClientProxy(context.Background(), conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if up.hits.Load() > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("no heartbeat reached the sandbox within 15s")
}

// TestStartClientProxyWithoutAnIdleTimeoutDoesNotHeartbeat.
//
// @decision(DL-030)
func TestStartClientProxyWithoutAnIdleTimeoutDoesNotHeartbeat(t *testing.T) {
	up := newUpstream(t)
	conn := session.ConnectorFunc(func(context.Context) (session.Connection, error) {
		return session.Connection{URL: up.URL, Token: "t", User: "opencode", Password: "p"}, nil
	})
	p, err := session.StartClientProxy(context.Background(), conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	time.Sleep(200 * time.Millisecond)
	if n := up.hits.Load(); n != 0 {
		t.Errorf("%d unrequested calls reached the sandbox", n)
	}
}
