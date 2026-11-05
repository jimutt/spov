package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode/contract"
)

// upstream stands in for Modal's edge plus the OpenCode server behind it:
// it rejects a request whose connect-token cookie is not current the way
// the edge does (401, no WWW-Authenticate), and one without Basic auth
// the way OpenCode does (401, WWW-Authenticate: Basic).
type upstream struct {
	mu sync.Mutex
	// token is the cookie value currently accepted.
	token string
	// password is the Basic password currently accepted.
	password string
	// requests records what arrived, after the proxy rewrote it.
	requests []recorded
	// handler serves an authenticated request; nil answers 200.
	handler http.HandlerFunc
}

type recorded struct {
	Path   string
	Host   string
	Cookie string
	Auth   string
}

func newUpstream(token, password string) *upstream {
	return &upstream{token: token, password: password}
}

func (u *upstream) setToken(tok string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.token = tok
}

func (u *upstream) log() []recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recorded(nil), u.requests...)
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	wantToken, wantPassword := u.token, u.password
	got := recorded{Path: r.URL.Path, Host: r.Host, Auth: r.Header.Get("Authorization")}
	if c, err := r.Cookie(TokenCookie); err == nil {
		got.Cookie = c.Value
	}
	u.requests = append(u.requests, got)
	handler := u.handler
	u.mu.Unlock()

	// The edge checks the token first and answers without a challenge,
	// which is what distinguishes it from the server behind it.
	if got.Cookie != wantToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "No auth token provided")
		return
	}
	if wantPassword != "" {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != wantPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="Secure Area"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if handler != nil {
		handler(w, r)
		return
	}
	_, _ = io.WriteString(w, "ok")
}

// startProxy wires a proxy to an upstream and returns both.
func startProxy(t *testing.T, u *upstream, mint Mint) *Proxy {
	t.Helper()
	p, err := Start(context.Background(), Options{
		Mint: mint,
		Warn: func(msg string) { t.Logf("warn: %s", msg) },
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func staticMint(url, token, password string) Mint {
	return func(context.Context) (Credential, error) {
		return Credential{URL: url, Token: token, User: "opencode", Password: password}, nil
	}
}

// result is what a test needs from a finished response. The live
// *http.Response is deliberately not returned: its body is already
// closed, so handing it back would be an invitation to read from it.
type result struct {
	StatusCode int
	Header     http.Header
}

func get(t *testing.T, url string, opts ...func(*http.Request)) (result, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return result{StatusCode: resp.StatusCode, Header: resp.Header.Clone()}, string(body)
}

func TestProxyAttachesBothCredentials(t *testing.T) {
	u := newUpstream("tok-1", "pw-1")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok-1", "pw-1"))

	// A client with no credential of its own — a browser — gets through.
	resp, body := get(t, p.URL+"/api/info")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", resp.StatusCode, body)
	}

	log := u.log()
	if len(log) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(log))
	}
	if log[0].Cookie != "tok-1" {
		t.Errorf("cookie = %q, want the connect token", log[0].Cookie)
	}
	if log[0].Auth == "" {
		t.Error("no Authorization header reached the server; the browser would have been prompted for a password")
	}
	if log[0].Path != "/api/info" {
		t.Errorf("path = %q", log[0].Path)
	}
}

func TestProxyRewritesHostToTheEdge(t *testing.T) {
	u := newUpstream("tok", "pw")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", "pw"))
	get(t, p.URL+"/api/info")

	log := u.log()
	if len(log) != 1 {
		t.Fatalf("upstream saw %d requests", len(log))
	}
	// The edge routes on Host; sending the loopback address would not
	// reach the sandbox.
	if strings.Contains(log[0].Host, "127.0.0.1:") && !strings.Contains(srv.URL, log[0].Host) {
		t.Errorf("Host = %q, want the edge host", log[0].Host)
	}
	if !strings.Contains(srv.URL, log[0].Host) {
		t.Errorf("Host = %q, want it to match the upstream %q", log[0].Host, srv.URL)
	}
}

// TestProxyReplacesAClientsOwnAuthorization pins that the proxy always
// substitutes its own credential and never forwards a caller's.
//
// @decision(DL-030)
func TestProxyReplacesAClientsOwnAuthorization(t *testing.T) {
	u := newUpstream("tok", "pw")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", "pw"))

	for _, supplied := range []string{"Basic " + basic("opencode", "guessed"), "Bearer attacker-controlled"} {
		t.Run(strings.SplitN(supplied, " ", 2)[0], func(t *testing.T) {
			resp, _ := get(t, p.URL+"/api/info", func(r *http.Request) {
				r.Header.Set("Authorization", supplied)
			})
			// The proxy's own credential is used, so the request
			// succeeds and reveals nothing about the guess.
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			log := u.log()
			got := log[len(log)-1].Auth
			if got == supplied {
				t.Fatalf("the client's own Authorization was forwarded: %q", got)
			}
			if want := "Basic " + basic("opencode", "pw"); got != want {
				t.Errorf("Authorization = %q, want the proxy's own %q", got, want)
			}
		})
	}
}

// basic renders a Basic credential the way net/http does.
func basic(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}

// TestProxyRefusesForeignHosts pins the DNS-rebinding guard: a request
// whose Host does not name the proxy is refused before it reaches the
// sandbox.
//
// @decision(DL-030)
func TestProxyRefusesForeignHosts(t *testing.T) {
	u := newUpstream("tok", "pw")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", "pw"))

	resp, body := get(t, p.URL+"/api/info", func(r *http.Request) {
		r.Host = "evil.example.com"
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body %q)", resp.StatusCode, body)
	}
	if n := len(u.log()); n != 0 {
		t.Errorf("the request reached the sandbox anyway (%d upstream requests)", n)
	}
}

// TestProxyRefusesCrossSiteRequests pins the CSRF guard: a request
// carrying a foreign Origin is refused before it reaches the sandbox.
//
// @decision(DL-030)
func TestProxyRefusesCrossSiteRequests(t *testing.T) {
	u := newUpstream("tok", "pw")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", "pw"))

	req, err := http.NewRequest(http.MethodPost, p.URL+"/api/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if n := len(u.log()); n != 0 {
		t.Errorf("a cross-site request reached the sandbox (%d upstream requests)", n)
	}
}

// TestProxyRefusesCrossSiteGETsWithNoOrigin pins the Sec-Fetch-Site
// check that closes the hole an Origin check alone leaves: browsers omit
// Origin on cross-site subresource GETs.
//
// @decision(DL-030)
func TestProxyRefusesCrossSiteGETsWithNoOrigin(t *testing.T) {
	u := newUpstream("tok", "pw")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", "pw"))

	// The header set Chrome actually sends for each of these. None
	// carries an Origin.
	cases := []struct{ name, dest string }{
		{"img", "image"},
		{"script", "script"},
		{"stylesheet", "style"},
		{"iframe", "iframe"},
		{"top-level navigation", "document"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(u.log())
			resp, body := get(t, p.URL+"/api/pty/x/connect", func(r *http.Request) {
				r.Header.Set("Sec-Fetch-Site", "cross-site")
				r.Header.Set("Sec-Fetch-Dest", tc.dest)
			})
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (body %q)", resp.StatusCode, body)
			}
			if n := len(u.log()); n != before {
				t.Errorf("a cross-site %s GET reached the sandbox", tc.name)
			}
		})
	}
}

// TestProxyRefusesSameSiteButCrossOrigin pins that a "same-site" request
// (another service on a different loopback port) is still refused.
//
// @decision(DL-030)
func TestProxyRefusesSameSiteButCrossOrigin(t *testing.T) {
	u := newUpstream("tok", "pw")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", "pw"))

	resp, _ := get(t, p.URL+"/api/info", func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "same-site")
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestProxyStripsCookieDomainFromTheSandbox pins that a Domain attribute
// on a Set-Cookie from the sandbox is stripped, while the rest of the
// cookie and any unrelated cookies survive.
//
// @decision(DL-030)
func TestProxyStripsCookieDomainFromTheSandbox(t *testing.T) {
	u := newUpstream("tok", "")
	u.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "evil=1; Path=/; Domain=localhost; HttpOnly")
		w.Header().Add("Set-Cookie", "ordinary=2; Path=/")
	}
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", ""))
	resp, _ := get(t, p.URL+"/")

	got := resp.Header.Values("Set-Cookie")
	if len(got) != 2 {
		t.Fatalf("Set-Cookie = %q, want both cookies preserved", got)
	}
	for _, c := range got {
		if strings.Contains(strings.ToLower(c), "domain=") {
			t.Errorf("a Domain attribute survived: %q", c)
		}
	}
	// The cookies themselves must still work, or the web UI breaks.
	if !strings.Contains(got[0], "evil=1") || !strings.Contains(got[0], "HttpOnly") {
		t.Errorf("the cookie was mangled beyond its Domain: %q", got[0])
	}
	if !strings.Contains(got[1], "ordinary=2") {
		t.Errorf("an unrelated cookie was lost: %q", got[1])
	}
}

// TestProxyCapsTotalMints pins maxMints as a hard ceiling: a sandbox that
// answers every request with 401 cannot drive unbounded reminting.
//
// @decision(DL-030)
func TestProxyCapsTotalMints(t *testing.T) {
	u := newUpstream("never-matches", "")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	var mints atomic.Int32
	mint := func(context.Context) (Credential, error) {
		mints.Add(1)
		return Credential{URL: srv.URL, Token: "wrong", User: "opencode"}, nil
	}

	now := time.Now()
	p, err := Start(context.Background(), Options{
		Mint: mint,
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// Every request is rejected, and time advances past the throttle
	// each round, so only the cap can stop this.
	for range maxMints + 20 {
		get(t, p.URL+"/api/info")
		now = now.Add(remintInterval + time.Second)
	}
	if got := int(mints.Load()); got > maxMints {
		t.Errorf("minted %d tokens, want no more than %d", got, maxMints)
	}
	if got := int(mints.Load()); got != maxMints {
		t.Errorf("minted %d tokens, want the cap of %d to be reached", got, maxMints)
	}
}

// TestProxyAllowsItsOwnClients pins that the guard admits its own
// clients: the TUI (no headers), the web UI (same-origin) and any
// loopback spelling.
//
// @decision(DL-030)
func TestProxyAllowsItsOwnClients(t *testing.T) {
	u := newUpstream("tok", "pw")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", "pw"))
	_, port, err := net.SplitHostPort(strings.TrimPrefix(p.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"no headers at all, as spov's own Go clients send", func(*http.Request) {}},
		{"same-origin, as the web UI's own fetches send", func(r *http.Request) {
			r.Header.Set("Origin", p.URL)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}},
		{"user typed the URL, which browsers mark as none", func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "none")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
		}},
		{"localhost spelling", func(r *http.Request) {
			r.Host = "localhost:" + port
			r.Header.Set("Origin", "http://localhost:"+port)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := get(t, p.URL+"/api/info", tc.mutate)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 (body %q)", resp.StatusCode, body)
			}
		})
	}
}

func TestProxyReplacesAStaleTokenCookie(t *testing.T) {
	u := newUpstream("current", "pw")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "current", "pw"))
	// A browser that visited an earlier session still holds its cookie.
	resp, _ := get(t, p.URL+"/api/info", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: TokenCookie, Value: "stale"})
		r.AddCookie(&http.Cookie{Name: "other", Value: "kept"})
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the stale cookie was forwarded", resp.StatusCode)
	}
	log := u.log()
	if log[0].Cookie != "current" {
		t.Errorf("cookie = %q, want the current token", log[0].Cookie)
	}
}

func TestProxyPreservesOtherCookies(t *testing.T) {
	u := newUpstream("tok", "")
	var gotOther string
	u.handler = func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session"); err == nil {
			gotOther = c.Value
		}
	}
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", ""))
	get(t, p.URL+"/", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: TokenCookie, Value: "stale"})
		r.AddCookie(&http.Cookie{Name: "session", Value: "keep-me"})
	})
	if gotOther != "keep-me" {
		t.Errorf("other cookie = %q, want it preserved", gotOther)
	}
}

func TestProxyRemintsWhenTheEdgeRejectsTheToken(t *testing.T) {
	u := newUpstream("first", "pw")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	var mints atomic.Int32
	mint := func(context.Context) (Credential, error) {
		n := mints.Add(1)
		return Credential{
			URL:      srv.URL,
			Token:    fmt.Sprintf("token-%d", n),
			User:     "opencode",
			Password: "pw",
		}, nil
	}

	p := startProxy(t, u, mint)
	// The first mint produced token-1, which the upstream does not
	// accept: the edge rejects it, and that must trigger a new mint.
	u.setToken("token-2")

	if resp, _ := get(t, p.URL+"/api/info"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first request status = %d, want 401", resp.StatusCode)
	}
	if got := mints.Load(); got != 2 {
		t.Fatalf("mints = %d after a rejected token, want 2", got)
	}

	// The retry the client makes now carries the renewed token.
	resp, body := get(t, p.URL+"/api/info")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after re-mint status = %d, want 200 (body %q)", resp.StatusCode, body)
	}
}

func TestProxyDoesNotRemintOnAServerChallenge(t *testing.T) {
	// A wrong password makes OpenCode answer 401 with a Basic challenge.
	// That is not the edge rejecting a token, so no token is minted.
	u := newUpstream("tok", "right-password")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	var mints atomic.Int32
	mint := func(context.Context) (Credential, error) {
		mints.Add(1)
		return Credential{URL: srv.URL, Token: "tok", User: "opencode", Password: "wrong-password"}, nil
	}

	p := startProxy(t, u, mint)
	if resp, _ := get(t, p.URL+"/api/info"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1: a password failure must not mint a token", got)
	}
}

func TestProxyThrottlesReminting(t *testing.T) {
	u := newUpstream("never-matches", "")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	var mints atomic.Int32
	mint := func(context.Context) (Credential, error) {
		mints.Add(1)
		return Credential{URL: srv.URL, Token: "wrong", User: "opencode"}, nil
	}

	now := time.Now()
	p, err := Start(context.Background(), Options{
		Mint: mint,
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })

	for range 5 {
		get(t, p.URL+"/api/info")
	}
	// One at start, one for the first rejection, none after.
	if got := mints.Load(); got != 2 {
		t.Errorf("mints = %d over five rejected requests, want 2", got)
	}

	now = now.Add(remintInterval + time.Second)
	get(t, p.URL+"/api/info")
	if got := mints.Load(); got != 3 {
		t.Errorf("mints = %d after the interval elapsed, want 3", got)
	}
}

// TestProxyRetriesAfterAFailedMint pins that a failed mint gets a
// shorter backoff than the ordinary remint throttle.
//
// @decision(DL-030)
func TestProxyRetriesAfterAFailedMint(t *testing.T) {
	u := newUpstream("never-matches", "")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	var mints atomic.Int32
	mint := func(context.Context) (Credential, error) {
		if mints.Add(1) == 2 {
			return Credential{}, errors.New("control plane blip")
		}
		return Credential{URL: srv.URL, Token: "wrong", User: "opencode"}, nil
	}

	now := time.Now()
	p, err := Start(context.Background(), Options{
		Mint: mint,
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// The second mint (the first re-mint) fails.
	get(t, p.URL+"/api/info")
	if got := mints.Load(); got != 2 {
		t.Fatalf("mints = %d, want 2", got)
	}

	// After the backoff — which is far shorter than the full interval —
	// the next rejection tries again.
	now = now.Add(remintBackoff + time.Second)
	get(t, p.URL+"/api/info")
	if got := mints.Load(); got != 3 {
		t.Errorf("mints = %d after the backoff elapsed, want 3: a failed mint was not retried", got)
	}
	if remintBackoff >= remintInterval {
		t.Errorf("the failure backoff (%s) is not shorter than the normal interval (%s)", remintBackoff, remintInterval)
	}
}

func TestProxyStreamsServerSentEvents(t *testing.T) {
	u := newUpstream("tok", "")
	// An event stream that never ends. ReverseProxy flushes
	// text/event-stream immediately on its own, so this guards the whole
	// path — upgrade-free streaming, headers, and the first event
	// arriving before the response completes — rather than the
	// FlushInterval setting specifically.
	u.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream response is not flushable")
			return
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"server.connected\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", ""))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+"/api/event", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("no event arrived before the stream ended: %v", err)
	}
	if !strings.Contains(line, "server.connected") {
		t.Errorf("first line = %q", line)
	}
}

func TestProxyForwardsWebSocketUpgrades(t *testing.T) {
	u := newUpstream("tok", "")
	u.handler = func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Errorf("Upgrade header = %q, want websocket", r.Header.Get("Upgrade"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream response is not hijackable")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
		// Echo one line so the tunnel is proven in both directions.
		line, _ := buf.ReadString('\n')
		_, _ = buf.WriteString("echo:" + line)
		_ = buf.Flush()
	}
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", ""))

	req, _ := http.NewRequest(http.MethodGet, p.URL+"/api/pty/x/connect", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("upgrade request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	body, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatal("upgraded body is not writable; the connection was not tunnelled")
	}
	if _, err := io.WriteString(body, "hello\n"); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	line, err := bufio.NewReader(body).ReadString('\n')
	if err != nil {
		t.Fatalf("read through the tunnel: %v", err)
	}
	if line != "echo:hello\n" {
		t.Errorf("echo = %q, want %q", line, "echo:hello\n")
	}
}

// TestProxyStreamsAKnownLengthBody covers what FlushInterval actually
// buys: ReverseProxy forces immediate flushing for text/event-stream and
// for responses of unknown length, but a streamed body with a declared
// Content-Length is buffered unless FlushInterval says otherwise.
func TestProxyStreamsAKnownLengthBody(t *testing.T) {
	u := newUpstream("tok", "")
	release := make(chan struct{})
	u.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "last\n")
	}
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", ""))
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+"/slow", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("nothing arrived before the body completed: %v", err)
	}
	if line != "first\n" {
		t.Errorf("first line = %q", line)
	}
}

func TestStartRejectsAnUnusableCredential(t *testing.T) {
	cases := []struct {
		name string
		mint Mint
	}{
		{"no mint", nil},
		{"mint fails", func(context.Context) (Credential, error) {
			return Credential{}, errors.New("control plane down")
		}},
		{"empty URL", staticMint("", "tok", "")},
		{"relative URL", staticMint("/connect", "tok", "")},
		{"non-HTTP URL", staticMint("ftp://edge.example", "tok", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Start(context.Background(), Options{Mint: tc.mint})
			if err == nil {
				_ = p.Close()
				t.Fatal("Start succeeded on an unusable credential")
			}
		})
	}
}

func TestProxyReportsAnUnreachableUpstream(t *testing.T) {
	// A closed server: the upstream request fails rather than answering.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	var warnings []string
	var mu sync.Mutex
	p, err := Start(context.Background(), Options{
		Mint: staticMint(url, "tok", ""),
		Warn: func(msg string) {
			mu.Lock()
			defer mu.Unlock()
			warnings = append(warnings, msg)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })

	resp, _ := get(t, p.URL+"/api/info")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) == 0 {
		t.Error("an unreachable sandbox produced no warning")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	u := newUpstream("tok", "")
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)

	p := startProxy(t, u, staticMint(srv.URL, "tok", ""))
	if err := p.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

// TestHeartbeatForDerivesAnIntervalFromTheIdleTimeout.
//
// @decision(DL-030)
func TestHeartbeatForDerivesAnIntervalFromTheIdleTimeout(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		0:                0,
		-time.Minute:     0,
		30 * time.Minute: MaxHeartbeatInterval, // clamped: idle/4 is 7m30s
		2 * time.Minute:  30 * time.Second,
		time.Minute:      15 * time.Second,
		20 * time.Second: MinHeartbeatInterval, // clamped up from 5s
	}
	for idle, want := range cases {
		if got := HeartbeatFor(idle); got != want {
			t.Errorf("HeartbeatFor(%s) = %s, want %s", idle, got, want)
		}
	}
}

// TestProxyHeartbeatsTheSandbox: only requests reset a sandbox's idle
// timeout, so an attached client that merely holds a stream open would
// lose its sandbox without this.
//
// @decision(DL-030)
func TestProxyHeartbeatsTheSandbox(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	beat := make(chan struct{}, 8)
	upstream := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		select {
		case beat <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	})))
	defer upstream.Close()

	p, err := Start(context.Background(), Options{
		Mint:      staticMint(upstream.URL, "tok", "secret"),
		Heartbeat: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for range 2 {
		select {
		case <-beat:
		case <-time.After(3 * time.Second):
			t.Fatal("no heartbeat reached the sandbox")
		}
	}
	mu.Lock()
	got := paths[0]
	mu.Unlock()
	if got != HeartbeatPath {
		t.Errorf("heartbeat hit %q, want %q", got, HeartbeatPath)
	}

	// It stops with the proxy.
	_ = p.Close()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	before := len(paths)
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	after := len(paths)
	mu.Unlock()
	if after != before {
		t.Errorf("the heartbeat kept beating after Close: %d then %d", before, after)
	}
}

// TestProxyWithoutAHeartbeatSendsNothing: a zero interval is the
// launcher's proxy and any test that does not want traffic.
//
// @decision(DL-030)
func TestProxyWithoutAHeartbeatSendsNothing(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	p, err := Start(context.Background(), Options{Mint: staticMint(upstream.URL, "tok", "secret")})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	time.Sleep(80 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d unrequested calls reached the sandbox", n)
	}
}

// TestProxyRefusesARedirectOffItsOwnOrigin: the upstream is an agent
// that chooses its own responses, so a Location it picks would otherwise
// aim the user's browser — already on this origin — anywhere.
//
// @decision(DL-030)
func TestProxyRefusesARedirectOffItsOwnOrigin(t *testing.T) {
	var location string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()
	p, err := Start(context.Background(), Options{Mint: staticMint(upstream.URL, "tok", "secret")})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// A redirect that is kept must not be followed here: the upstream
	// answers every path with another redirect.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	cases := map[string]bool{
		"/api/session/1":              true, // relative: stays here
		"http://evil.example/":        false,
		"http://127.0.0.1:1/internal": false,
		"https://evil.example/":       false,
		"//evil.example/":             false,
		"::::not a url":               false,
		p.URL + "/api/info":           true,
	}
	for loc, keep := range cases {
		t.Run(loc, func(t *testing.T) {
			location = loc
			req, _ := http.NewRequest(http.MethodGet, p.URL+"/api/x", nil)
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			got := res.Header.Get("Location")
			if keep && got != loc {
				t.Errorf("Location = %q, want it forwarded", got)
			}
			if !keep && got != "" {
				t.Errorf("Location = %q, want it dropped", got)
			}
		})
	}
}

// TestProxyHeartbeatStopsOnClose asserts the stop channel specifically:
// closing the listener also ends the loop, so without this the guard is
// unverified.
//
// @decision(DL-030)
func TestProxyHeartbeatStopsOnClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	p, err := Start(context.Background(), Options{
		Mint: staticMint(upstream.URL, "tok", "secret"), Heartbeat: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped := p.stopBeat
	_ = p.Close()
	select {
	case <-stopped:
	default:
		t.Fatal("Close did not signal the heartbeat to stop")
	}
	// Closing twice is safe.
	_ = p.Close()
}

// TestProxyHeartbeatIsQuietDuringShutdown: a beat in flight when the
// proxy closes must not tell the user the sandbox has stopped.
//
// @decision(DL-030)
func TestProxyHeartbeatIsQuietDuringShutdown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	var mu sync.Mutex
	var warnings []string
	p, err := Start(context.Background(), Options{
		Mint: staticMint(upstream.URL, "tok", "secret"), Heartbeat: 20 * time.Millisecond,
		Warn: func(s string) { mu.Lock(); warnings = append(warnings, s); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Long enough for several successful beats, so any warning after
	// Close comes from the shutdown rather than from a sandbox that was
	// already failing.
	time.Sleep(120 * time.Millisecond)
	mu.Lock()
	if len(warnings) != 0 {
		mu.Unlock()
		t.Fatalf("a healthy sandbox warned before shutdown: %v", warnings)
	}
	mu.Unlock()
	_ = p.Close()
	time.Sleep(120 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for _, w := range warnings {
		if strings.Contains(w, "may have stopped") {
			t.Errorf("shutdown warned the user the sandbox died: %q", w)
		}
	}
}

// A heartbeat the sandbox answers with a 404 still reached it, but the
// route may have moved, which is said once rather than on every beat.
//
// @decision(DL-035) @decision(DL-030)
func TestAMovedHeartbeatRouteIsSaidOnce(t *testing.T) {
	var beats atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The first beats meet an expired token, which is not a moved
		// route; the rest find no route at all.
		if beats.Add(1) <= 2 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()
	var mu sync.Mutex
	var warnings []string
	p, err := Start(context.Background(), Options{
		Mint: staticMint(upstream.URL, "tok", "secret"), Heartbeat: 10 * time.Millisecond,
		Warn: func(s string) { mu.Lock(); warnings = append(warnings, s); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	deadline := time.Now().Add(3 * time.Second)
	for beats.Load() < 6 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "404") || !strings.Contains(warnings[0], HeartbeatPath) {
		t.Errorf("warnings after %d beats: %q, want one naming the route and the status", beats.Load(), warnings)
	}
}
