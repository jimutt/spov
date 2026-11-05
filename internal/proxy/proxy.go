// Package proxy runs the loopback reverse proxy that fronts a sandbox.
//
// The sandbox exposes no public port. Every client — the OpenCode TUI, a
// browser, a health check — talks to this proxy on 127.0.0.1, and the
// proxy is the only thing holding the credentials needed to reach the
// sandbox: the provider's connect token and the OpenCode password.
//
// @decision(DL-030)
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenCookie is the cookie Modal's edge reads a connect token from. The
// token cannot be sent as `Authorization: Bearer`, since the edge
// consumes that header and leaves OpenCode's own Basic credential with
// nowhere to go.
//
// @decision(DL-030)
const TokenCookie = "_modal_connect_token"

// Credential is what the proxy presents upstream: a connect token for the
// edge and Basic auth for the OpenCode server behind it.
//
// @decision(DL-030)
type Credential struct {
	// URL is the edge endpoint the token was minted for.
	URL string
	// Token authenticates to the edge.
	Token string
	// User and Password are the Basic credential for OpenCode.
	User     string
	Password string
}

// Mint produces a fresh credential. The proxy calls it once at start and
// again when the edge rejects a token, which is how a session outlives
// the token's expiry.
//
// @decision(DL-030)
type Mint func(ctx context.Context) (Credential, error)

// Proxy is a running loopback proxy. It is created by Start and stopped
// by Close.
//
// @decision(DL-030)
type Proxy struct {
	// URL is the loopback address clients connect to.
	URL string

	// addr and port are this proxy's own address, used to reject
	// requests aimed at any other name.
	//
	// @decision(DL-030)
	addr string
	port string

	listener net.Listener
	server   *http.Server
	mint     Mint
	// warn reports non-fatal problems, such as a failed re-mint.
	warn func(string)

	mu   sync.RWMutex
	cred Credential
	// mints counts every credential this proxy has minted, including
	// the first.
	//
	// @decision(DL-030)
	mints int
	// remintAfter throttles re-minting so a server answering 401 for its
	// own reasons cannot drive a mint loop.
	remintAfter time.Time
	now         func() time.Time

	closeOnce sync.Once
	// stopBeat ends the heartbeat goroutine; nil when there is none.
	//
	// @decision(DL-030)
	stopBeat chan struct{}
}

// remintInterval is the shortest gap between two token mints. A 401 from
// OpenCode itself (a wrong password) would otherwise mint a token per
// request.
//
// @decision(DL-030)
const remintInterval = 30 * time.Second

// remintBackoff is the shorter gap after a mint that failed, so a
// transient control-plane failure is retried rather than waiting out the
// full interval.
//
// @decision(DL-030)
const remintBackoff = 2 * time.Second

// maxMints caps how many tokens one session will ever mint. The 401s
// that drive re-minting come from the sandbox, the untrusted side, and
// Modal cannot revoke a minted token. A day-long session needs a handful.
//
// @decision(DL-030)
const maxMints = 64

// Options configure a proxy.
type Options struct {
	// Mint supplies credentials. Required.
	Mint Mint
	// Addr is the listen address; empty means 127.0.0.1 on a free port.
	Addr string
	// Warn receives non-fatal problems. Nil discards them.
	Warn func(string)
	// Now supplies timestamps; defaults to time.Now.
	Now func() time.Time
	// Heartbeat is how often to send a keepalive request upstream while
	// the proxy runs. Zero disables it. Use HeartbeatFor to derive one
	// from a sandbox's idle timeout.
	//
	// @decision(DL-030)
	Heartbeat time.Duration
}

// HeartbeatPath is the request a heartbeat makes, OpenCode's identity
// endpoint (opencode.InfoPath).
//
// @decision(DL-030) @decision(DL-035)
const HeartbeatPath = "/api/info"

// The heartbeat's bounds. A provider counts requests as activity but not
// an open stream, so a client that only holds a stream — a TUI between
// keystrokes — needs one of these to keep its sandbox alive.
//
// @decision(DL-030)
const (
	MaxHeartbeatInterval = 30 * time.Second
	MinHeartbeatInterval = 10 * time.Second
	// heartbeatFailuresBeforeWarning is how many consecutive failures
	// are reported to the user, so a blip stays quiet.
	heartbeatFailuresBeforeWarning = 3
)

// HeartbeatFor returns the interval to use for a sandbox with the given
// idle timeout: a quarter of it, clamped, so several beats fit inside
// the window. A zero idle timeout means no heartbeat is needed.
//
// @decision(DL-030)
func HeartbeatFor(idle time.Duration) time.Duration {
	if idle <= 0 {
		return 0
	}
	interval := idle / 4
	if interval > MaxHeartbeatInterval {
		interval = MaxHeartbeatInterval
	}
	if interval < MinHeartbeatInterval {
		interval = MinHeartbeatInterval
	}
	return interval
}

// Start mints a first credential and begins serving on loopback. The
// returned proxy must be closed by the caller.
//
// @decision(DL-030)
func Start(ctx context.Context, opts Options) (*Proxy, error) {
	if opts.Mint == nil {
		return nil, errors.New("proxy: no way to mint a credential")
	}
	cred, err := opts.Mint(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := parseTarget(cred.URL); err != nil {
		return nil, err
	}

	listenAddr := opts.Addr
	if listenAddr == "" {
		// Loopback only: the credentials live in this process, so
		// anything that can reach the port can reach the session.
		listenAddr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("proxy: listen on %s: %w", listenAddr, err)
	}

	addr := ln.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("proxy: unusable listen address %q: %w", addr, err)
	}

	p := &Proxy{
		URL:      "http://" + addr,
		addr:     addr,
		port:     port,
		listener: ln,
		mint:     opts.Mint,
		warn:     opts.Warn,
		cred:     cred,
		mints:    1,
		now:      opts.Now,
	}
	p.server = &http.Server{Handler: p.guard(p.handler())}
	go func() { _ = p.server.Serve(ln) }()
	if opts.Heartbeat > 0 {
		p.stopBeat = make(chan struct{})
		go p.heartbeat(opts.Heartbeat)
	}
	return p, nil
}

// heartbeat keeps the sandbox out of its idle timeout for as long as a
// client is attached. It goes through the proxy's own listener, so it
// carries whatever credential is current and re-mints like any request.
//
// @decision(DL-030)
func (p *Proxy) heartbeat(interval time.Duration) {
	client := &http.Client{Timeout: interval / 2}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	misrouted := false
	for {
		select {
		case <-p.stopBeat:
			return
		case <-ticker.C:
		}
		req, err := http.NewRequest(http.MethodGet, p.URL+HeartbeatPath, nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
		}
		// A failure during shutdown is the shutdown, not the sandbox:
		// Close stops the listener, so a beat in flight fails.
		select {
		case <-p.stopBeat:
			return
		default:
		}
		// The beat reached the sandbox, but a 404 or 405 means the route
		// may have moved, which is said once. An expired edge token's
		// 401 is re-minted by the proxy and is not this.
		//
		// @decision(DL-035)
		moved := err == nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed)
		if moved && !misrouted {
			misrouted = true
			p.warnf("proxy: the sandbox answered a heartbeat to %s with %s; the route may have moved", HeartbeatPath, resp.Status)
		}
		if err != nil || resp.StatusCode >= 500 {
			failures++
			if failures == heartbeatFailuresBeforeWarning {
				p.warnf("proxy: the sandbox has not answered %d heartbeats; it may have stopped", failures)
			}
			continue
		}
		failures = 0
	}
}

// Close stops serving and ends the heartbeat. It is safe to call more
// than once.
func (p *Proxy) Close() error {
	var err error
	p.closeOnce.Do(func() {
		if p.stopBeat != nil {
			close(p.stopBeat)
		}
		err = p.server.Close()
	})
	return err
}

// handler builds the reverse proxy. Requests are rewritten to the edge
// endpoint carried by the current credential, which can change under a
// re-mint, so the target is read per request rather than captured.
//
// @decision(DL-030)
func (p *Proxy) handler() http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: p.rewrite,
		// Responses are streamed rather than buffered: OpenCode's event
		// stream is server-sent events, and buffering holds every event
		// until the connection closes.
		FlushInterval: -1,
		Transport:     newTransport(),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// The upstream error can carry the edge host; the client is
			// a local TUI or browser, so a generic message is enough.
			p.warnf("proxy: upstream request failed: %v", err)
			w.WriteHeader(http.StatusBadGateway)
		},
		ModifyResponse: p.modifyResponse,
	}
	return rp
}

// rewrite points a request at the edge and attaches both credentials.
//
// @decision(DL-030)
func (p *Proxy) rewrite(r *httputil.ProxyRequest) {
	cred := p.credential()
	target, err := parseTarget(cred.URL)
	if err != nil {
		// Start validates the first URL and remint rejects a bad one, so
		// this is unreachable. Leaving the request unrewritten would aim
		// it at the proxy itself, so it is sent somewhere that cannot
		// resolve and fails as a bad gateway rather than taking the
		// whole process down from inside a handler goroutine.
		p.warnf("proxy: unusable connect URL %q: %v", cred.URL, err)
		r.Out.URL.Scheme = "http"
		r.Out.URL.Host = "invalid.spov.internal"
		r.Out.Host = "invalid.spov.internal"
		return
	}
	r.SetURL(target)
	// The edge routes on Host, so it must name the edge rather than the
	// loopback address the client used.
	r.Out.Host = target.Host

	// Any cookie the client sent is preserved; only spov's is replaced,
	// so a stale token from an earlier credential cannot linger.
	stripCookie(r.Out, TokenCookie)
	r.Out.AddCookie(&http.Cookie{Name: TokenCookie, Value: cred.Token})

	// The proxy's credential always wins. Forwarding a client's own
	// Authorization would make this a password-checking oracle, and a
	// Bearer header is consumed by the edge, whose 401 the remint
	// heuristic would read as an expired token.
	r.Out.Header.Del("Authorization")
	if cred.Password != "" {
		r.Out.SetBasicAuth(cred.User, cred.Password)
	}
}

// guard rejects a request that a web page in a browser could have sent
// on the user's behalf, separating a client spov started from remote
// content running in a tab the user happens to have open.
//
// @decision(DL-030)
func (p *Proxy) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Host defeats DNS rebinding: a rebound name reaches this port
		// but still carries the attacker's hostname.
		if !p.allowedHost(r.Host) {
			p.warnf("proxy: refused a request for host %q", r.Host)
			http.Error(w, "spov: this address serves only the local OpenCode session", http.StatusForbidden)
			return
		}
		// Sec-Fetch-Site is what actually separates a cross-site request
		// from one made by the session's own page; Origin alone is not
		// enough. An absent header stays allowed, since Go's http client
		// sends none — that is spov's own TUI and health checker.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			p.warnf("proxy: refused a %s request from another site", site)
			http.Error(w, "spov: this address serves only the local OpenCode session", http.StatusForbidden)
			return
		}
		// Origin is still checked, for the cross-site requests that do
		// carry one and for any browser that omits Sec-Fetch-Site.
		if origin := r.Header.Get("Origin"); origin != "" && !p.allowedOrigin(origin) {
			p.warnf("proxy: refused a request from origin %q", origin)
			http.Error(w, "spov: this address serves only the local OpenCode session", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedHost reports whether a Host header names this proxy.
//
// @decision(DL-030)
func (p *Proxy) allowedHost(host string) bool {
	if host == "" {
		// HTTP/1.1 requires a Host; an absent one is not a browser.
		return false
	}
	if host == p.addr {
		return true
	}
	// A client may address the port by any loopback spelling.
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	if port != p.port {
		return false
	}
	switch name {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// allowedOrigin reports whether an Origin header names this proxy.
//
// @decision(DL-030)
func (p *Proxy) allowedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	return p.allowedHost(u.Host)
}

// confineRedirect refuses a Location that leaves this proxy, since the
// upstream agent controls its own responses and could otherwise send
// the browser at any other address.
//
// @decision(DL-030)
func (p *Proxy) confineRedirect(resp *http.Response) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return
	}
	u, err := url.Parse(loc)
	if err != nil {
		p.warnf("proxy: refused an unparseable redirect")
		resp.Header.Del("Location")
		return
	}
	// A relative Location stays on this origin, which is what a client
	// navigating the session's own pages produces.
	if u.Scheme == "" && u.Host == "" {
		return
	}
	if !p.allowedHost(u.Host) {
		p.warnf("proxy: refused a redirect to another host")
		resp.Header.Del("Location")
	}
}

// modifyResponse re-mints the connect token when the edge rejects it.
//
// The edge answers an expired or missing token with 401 and no
// WWW-Authenticate header; OpenCode answers a bad password with 401 and
// `WWW-Authenticate: Basic`. That difference is what tells the two apart,
// and only the first is worth a new token.
//
// @decision(DL-030)
func (p *Proxy) modifyResponse(resp *http.Response) error {
	stripCookieDomain(resp)
	p.confineRedirect(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		return nil
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		return nil
	}
	p.remint(resp.Request.Context())
	return nil
}

// stripCookieDomain removes the Domain attribute from every Set-Cookie
// the sandbox returns, so the cookie stays host-only rather than shared
// with every other service on a loopback port.
//
// @decision(DL-030)
func stripCookieDomain(resp *http.Response) {
	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	rewritten := make([]string, 0, len(cookies))
	for _, raw := range cookies {
		parts := strings.Split(raw, ";")
		kept := parts[:0]
		for _, part := range parts {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(part)), "domain=") {
				continue
			}
			kept = append(kept, part)
		}
		rewritten = append(rewritten, strings.Join(kept, ";"))
	}
	resp.Header.Del("Set-Cookie")
	for _, c := range rewritten {
		resp.Header.Add("Set-Cookie", c)
	}
}

// remint replaces the credential, at most once per remintInterval. A
// failure is reported and left alone: the client sees the 401 it was
// already getting.
//
// @decision(DL-030)
func (p *Proxy) remint(ctx context.Context) {
	p.mu.Lock()
	if p.timeNow().Before(p.remintAfter) {
		p.mu.Unlock()
		return
	}
	if p.mints >= maxMints {
		p.mu.Unlock()
		p.warnf("proxy: refusing to mint more than %d connect tokens for one session", maxMints)
		return
	}
	p.mints++
	p.remintAfter = p.timeNow().Add(remintInterval)
	p.mu.Unlock()

	// Minting talks to the provider's control plane, which is a
	// different service from the edge that just failed, so it gets its
	// own timeout rather than the request's remaining budget.
	mintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintTimeout)
	defer cancel()

	cred, err := p.mint(mintCtx)
	if err == nil {
		_, err = parseTarget(cred.URL)
	}
	if err != nil {
		// The throttle exists to stop a mint per request, not to stop
		// retrying after a transient failure: without this the next
		// thirty seconds of requests short-circuit and, since only a
		// response can drive a mint, a client that gives up in that
		// window leaves the session unreachable for good.
		p.mu.Lock()
		p.remintAfter = p.timeNow().Add(remintBackoff)
		p.mu.Unlock()
		p.warnf("proxy: could not renew the sandbox connect token: %v", err)
		return
	}
	p.mu.Lock()
	p.cred = cred
	p.mu.Unlock()
}

// mintTimeout bounds a token mint. It is short because the mint happens
// on a response path: a client is waiting on this request, and a health
// check polling every 250ms must not be stalled behind a hung control
// plane.
//
// @decision(DL-030)
const mintTimeout = 5 * time.Second

func (p *Proxy) credential() Credential {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cred
}

func (p *Proxy) timeNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Proxy) warnf(format string, args ...any) {
	if p.warn == nil {
		return
	}
	p.warn(fmt.Sprintf(format, args...))
}

// newTransport builds the upstream transport.
//
// HTTP/2 is disabled: a WebSocket upgrade cannot be carried over h2, and
// OpenCode's PTY sessions are WebSockets. Go negotiates h2 by default for
// HTTPS, so it has to be switched off explicitly.
//
// @decision(DL-030)
func newTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return t
}

// stripCookie removes one cookie from a request, leaving the rest.
func stripCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	kept := cookies[:0]
	for _, c := range cookies {
		if c.Name != name {
			kept = append(kept, c)
		}
	}
	if len(kept) == len(cookies) {
		return
	}
	r.Header.Del("Cookie")
	for _, c := range kept {
		r.AddCookie(c)
	}
}

// parseTarget validates an edge URL. A relative or non-HTTP URL would
// make the proxy forward to itself or to nothing.
func parseTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy: unusable connect URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("proxy: connect URL %q is not HTTP", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy: connect URL %q has no host", raw)
	}
	return u, nil
}
