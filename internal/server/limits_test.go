package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry/telemetrytest"
)

// limitClock is a clock a test moves by hand.
type limitClock struct {
	mu  sync.Mutex
	now time.Time
}

func newLimitClock() *limitClock {
	return &limitClock{now: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
}

func (c *limitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *limitClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// limitsWith returns the default limits on clock, adjusted.
func limitsWith(clock *limitClock, adjust func(*Limits)) *Limits {
	l := DefaultLimits()
	l.Now = clock.Now
	if adjust != nil {
		adjust(&l)
	}
	return &l
}

// userHeaderAuth signs a request in as the user its X-Test-User names.
type userHeaderAuth struct{}

func (userHeaderAuth) Authenticate(r *http.Request) (auth.Principal, error) {
	u := r.Header.Get("X-Test-User")
	if u == "" {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return auth.Principal{UserID: u, OrgID: "org"}, nil
}

// limitFixture is a bare server whose requests carry their user in a
// header, built for the address of a listener it can also serve on.
type limitFixture struct {
	t    *testing.T
	s    *Server
	host string
	ln   net.Listener
}

func newLimitServer(t *testing.T, limits *Limits, adjust func(*Deps)) *limitFixture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	deps := Deps{Store: store.NewMemory(), Auth: userHeaderAuth{}, Limits: limits, Warn: func(s string) { t.Log(s) }}
	if adjust != nil {
		adjust(&deps)
	}
	s, err := New(deps, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.mux.HandleFunc("GET /api/v1/test/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return &limitFixture{t: t, s: s, host: ln.Addr().String(), ln: ln}
}

// serve starts serving over the listener through NewHTTPServer, as
// spov serve does, and returns the base URL.
func (f *limitFixture) serve() string {
	hs := f.s.NewHTTPServer("")
	go func() { _ = hs.Serve(f.ln) }()
	f.t.Cleanup(func() { _ = hs.Close() })
	return "http://" + f.host
}

func (f *limitFixture) hit(method, target, peer, user string, body io.Reader) *httptest.ResponseRecorder {
	return hit(f.s.Handler(), f.host, method, target, peer, user, body)
}

// hit serves one request in memory, from peer, as user (none when "").
func hit(h http.Handler, host, method, target, peer, user string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	req.Host = host
	req.RemoteAddr = peer
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	if method == http.MethodPost && strings.HasPrefix(target, "/device/") {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// deviceHit is hit against a device fixture's server, anonymously.
func deviceHit(f *deviceFixture) func(method, target, peer string, body io.Reader) *httptest.ResponseRecorder {
	h, host := f.server.Handler(), f.http.Listener.Addr().String()
	return func(method, target, peer string, body io.Reader) *httptest.ResponseRecorder {
		return hit(h, host, method, target, peer, "", body)
	}
}

// mustRefuse fails unless rec is the refusal status with a Retry-After.
func mustRefuse(t *testing.T, rec *httptest.ResponseRecorder, status int, what string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("%s = %d %s, want %d", what, rec.Code, rec.Body, status)
	}
	if n, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || n < 1 {
		t.Fatalf("%s: Retry-After = %q, want whole seconds", what, rec.Header().Get("Retry-After"))
	}
}

// oauthBody is an RFC 6749 error body.
type oauthBody struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// The values are the ones DL-133 decided; a change to one is a change
// to the decision.
//
// @decision(DL-133)
func TestTheDefaultLimitsAreTheDecidedValues(t *testing.T) {
	got := DefaultLimits()
	want := Limits{
		SignIns: Rate{30, 30}, DevicePolls: Rate{120, 120},
		PendingLogins: 10_000, PendingDeviceCode: 10_000,
		Requests: Rate{600, 100}, Creates: Rate{20, 20},
		Streams: 20, InFlight: 32,
		ReadTimeout: 30 * time.Second, BundleReadTimeout: 5 * time.Minute,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("DefaultLimits() = %+v, want %+v", got, want)
	}
	if MaxBodyBytes != 1<<20 || MaxSessionBodyBytes != 24<<20 {
		t.Errorf("body limits = %d and %d, want 1 MiB and 24 MiB", MaxBodyBytes, MaxSessionBodyBytes)
	}
	if FlowLifetime != 15*time.Minute || DeviceCodeLifetime != 10*time.Minute {
		t.Errorf("the caps' windows = %s and %s, want the rows' 15 m and 10 m lives", FlowLifetime, DeviceCodeLifetime)
	}
}

// The listener closes an idle keep-alive connection and bounds the
// headers, and sets no whole-request read or write deadline, either of
// which would cut a stream.
//
// @decision(DL-133)
func TestTheHTTPServerSetsItsTimeoutsAndNoneThatCutsAStream(t *testing.T) {
	hs := newLimitServer(t, nil, nil).s.NewHTTPServer("")
	if hs.ReadHeaderTimeout != 10*time.Second || hs.IdleTimeout != 2*time.Minute {
		t.Errorf("ReadHeaderTimeout %s, IdleTimeout %s, want 10s and 2m", hs.ReadHeaderTimeout, hs.IdleTimeout)
	}
	if hs.ReadTimeout != 0 || hs.WriteTimeout != 0 {
		t.Errorf("ReadTimeout %s, WriteTimeout %s, want neither", hs.ReadTimeout, hs.WriteTimeout)
	}
}

// One address may start 30 sign-ins a minute, browser and device
// together, and another address is not affected.
//
// @decision(DL-133)
func TestOneAddressStartsThirtySignInsAMinute(t *testing.T) {
	clock := newLimitClock()
	r := telemetrytest.New(t)
	f := newDeviceFixtureWith(t, "alice@example.com\n", func(d *Deps) {
		d.Limits = limitsWith(clock, nil)
		d.Metrics = r.Metrics
	})
	send := deviceHit(f)
	for i := range 30 {
		target, method := LoginPath, http.MethodGet
		if i%2 == 1 {
			target, method = DeviceCodePath, http.MethodPost
		}
		if rec := send(method, target, "203.0.113.5:4000", strings.NewReader("client_id=spov-cli")); rec.Code >= 400 {
			t.Fatalf("start %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	refused := send(http.MethodGet, LoginPath, "203.0.113.5:4001", nil)
	mustRefuse(t, refused, http.StatusTooManyRequests, "the 31st browser sign-in")
	if refused.Header().Get("Set-Cookie") != "" {
		t.Errorf("a refused sign-in set a cookie, so it wrote a session row")
	}
	device := send(http.MethodPost, DeviceCodePath, "203.0.113.5:4002", strings.NewReader("client_id=spov-cli"))
	mustRefuse(t, device, http.StatusTooManyRequests, "the 31st start, as a device code")
	var body oauthBody
	if err := json.Unmarshal(device.Body.Bytes(), &body); err != nil || body.Error != "slow_down" || !strings.Contains(body.Description, "Too many sign-ins") {
		t.Errorf("the device refusal = %s, want slow_down with the reason", device.Body)
	}
	if rec := send(http.MethodGet, LoginPath, "198.51.100.9:4000", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("another address's sign-in = %d, want it to start", rec.Code)
	}
	clock.advance(2 * time.Second)
	if rec := send(http.MethodGet, LoginPath, "203.0.113.5:4003", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("a sign-in two seconds later = %d, want the refilled token to admit it", rec.Code)
	}
	if got := r.Count("spov_requests_limited", "limit", "sign_in_rate"); got != 2 {
		t.Errorf("sign-in refusals recorded = %d, want 2", got)
	}
}

// The CLI's polls are limited per address too, and a refusal is
// slow_down, which x/oauth2's device client answers by polling less.
//
// @decision(DL-133) @decision(DL-006)
func TestOneAddressPollsForADeviceToken120TimesAMinute(t *testing.T) {
	clock := newLimitClock()
	f := newDeviceFixtureWith(t, "alice@example.com\n", func(d *Deps) { d.Limits = limitsWith(clock, nil) })
	send := deviceHit(f)
	form := url.Values{"grant_type": {DeviceGrantType}, "device_code": {"unknown"}, "client_id": {DeviceClientID}}.Encode()
	for i := range 120 {
		if rec := send(http.MethodPost, deviceTokenPath, "203.0.113.5:4000", strings.NewReader(form)); rec.Code != http.StatusBadRequest {
			t.Fatalf("poll %d = %d %s, want the endpoint's own invalid_grant", i+1, rec.Code, rec.Body)
		}
	}
	rec := send(http.MethodPost, deviceTokenPath, "203.0.113.5:4000", strings.NewReader(form))
	mustRefuse(t, rec, http.StatusTooManyRequests, "the 121st poll")
	var body oauthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != "slow_down" {
		t.Errorf("the refused poll = %s, want slow_down", rec.Body)
	}
	if rec := send(http.MethodPost, deviceTokenPath, "198.51.100.9:4000", strings.NewReader(form)); rec.Code != http.StatusBadRequest {
		t.Errorf("another address's poll = %d, want it served", rec.Code)
	}
}

// However many addresses start them, one replica holds at most its cap
// of sign-ins within a row's life, so the rows it can create are
// bounded; a start past the cap is a 503 saying when to come back.
//
// @decision(DL-133)
func TestSignInsInProgressAreCappedPerReplica(t *testing.T) {
	clock := newLimitClock()
	r := telemetrytest.New(t)
	f := newDeviceFixtureWith(t, "alice@example.com\n", func(d *Deps) {
		d.Limits = limitsWith(clock, func(l *Limits) { l.PendingLogins, l.PendingDeviceCode = 3, 2 })
		d.Metrics = r.Metrics
	})
	send := deviceHit(f)
	cookies := 0
	for i := range 3 {
		rec := send(http.MethodGet, LoginPath, fmt.Sprintf("203.0.113.%d:4000", i+1), nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("login %d = %d", i+1, rec.Code)
		}
		if rec.Header().Get("Set-Cookie") != "" {
			cookies++
		}
	}
	clock.advance(FlowLifetime - time.Minute)
	refused := send(http.MethodGet, LoginPath, "198.51.100.1:4000", nil)
	mustRefuse(t, refused, http.StatusServiceUnavailable, "a login past the cap")
	if got := refused.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %s, want the 60 s until the oldest start leaves the window", got)
	}
	if refused.Header().Get("Set-Cookie") != "" || cookies != 3 {
		t.Errorf("rows written = %d, and the refusal set a cookie: %v", cookies, refused.Header().Get("Set-Cookie") != "")
	}
	for i := range 2 {
		if rec := send(http.MethodPost, DeviceCodePath, fmt.Sprintf("192.0.2.%d:4000", i+1), strings.NewReader("client_id=spov-cli")); rec.Code != http.StatusOK {
			t.Fatalf("device code %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	device := send(http.MethodPost, DeviceCodePath, "192.0.2.9:4000", strings.NewReader("client_id=spov-cli"))
	mustRefuse(t, device, http.StatusServiceUnavailable, "a device code past the cap")
	var body oauthBody
	if err := json.Unmarshal(device.Body.Bytes(), &body); err != nil || body.Error != "temporarily_unavailable" {
		t.Errorf("the device refusal = %s, want temporarily_unavailable", device.Body)
	}
	clock.advance(time.Minute)
	if rec := send(http.MethodGet, LoginPath, "198.51.100.1:4000", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("a login once the oldest left the window = %d", rec.Code)
	}
	if r.Count("spov_requests_limited", "limit", "sign_in_cap") != 1 || r.Count("spov_requests_limited", "limit", "device_code_cap") != 1 {
		t.Errorf("cap refusals were not recorded by limit")
	}
}

// One user may send 600 API requests a minute after a burst of 100;
// another user has their own bucket.
//
// @decision(DL-133)
func TestOneUserSends600RequestsAMinuteAfterABurstOf100(t *testing.T) {
	clock := newLimitClock()
	f := newLimitServer(t, limitsWith(clock, nil), nil)
	for i := range 100 {
		if rec := f.hit(http.MethodGet, "/api/v1/test/ok", "192.0.2.1:1", "alice", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("request %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := f.hit(http.MethodGet, "/api/v1/test/ok", "192.0.2.1:1", "alice", nil)
	mustRefuse(t, rec, http.StatusTooManyRequests, "the 101st request")
	var p struct{ Title, Detail string }
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Detail == "" || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Errorf("the refusal = %s, want a problem whose detail a client prints", rec.Body)
	}
	if rec := f.hit(http.MethodGet, "/api/v1/test/ok", "192.0.2.1:1", "bob", nil); rec.Code != http.StatusNoContent {
		t.Errorf("another user's request = %d", rec.Code)
	}
	clock.advance(time.Second)
	for i := range 10 {
		if rec := f.hit(http.MethodGet, "/api/v1/test/ok", "192.0.2.1:1", "alice", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("refilled request %d = %d, want ten a second", i+1, rec.Code)
		}
	}
	mustRefuse(t, f.hit(http.MethodGet, "/api/v1/test/ok", "192.0.2.1:1", "alice", nil), http.StatusTooManyRequests, "the eleventh in that second")
}

// Session creates, job creates and resumes share one bucket of 20 a
// minute, apart from the request bucket.
//
// @decision(DL-133)
func TestOneUserStarts20SessionsOrJobsAMinute(t *testing.T) {
	clock := newLimitClock()
	f := newLimitServer(t, limitsWith(clock, nil), nil)
	routes := []string{"/api/v1/sessions", "/api/v1/jobs", "/api/v1/sessions/s1/resume"}
	for i := range 20 {
		// Not JSON, so huma refuses it before any handler runs; the
		// limit is taken first either way.
		if rec := f.hit(http.MethodPost, routes[i%3], "192.0.2.1:1", "alice", strings.NewReader("x")); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("create %d was refused: %s", i+1, rec.Body)
		}
	}
	for _, route := range routes {
		mustRefuse(t, f.hit(http.MethodPost, route, "192.0.2.1:1", "alice", strings.NewReader("x")), http.StatusTooManyRequests, "a 21st create at "+route)
	}
	if rec := f.hit(http.MethodGet, "/api/v1/test/ok", "192.0.2.1:1", "alice", nil); rec.Code != http.StatusNoContent {
		t.Errorf("an ordinary request after the creates ran out = %d", rec.Code)
	}
	if rec := f.hit(http.MethodPost, "/api/v1/sessions", "192.0.2.1:1", "bob", strings.NewReader("x")); rec.Code == http.StatusTooManyRequests {
		t.Errorf("another user's create was refused")
	}
	clock.advance(3 * time.Second)
	if rec := f.hit(http.MethodPost, "/api/v1/sessions", "192.0.2.1:1", "alice", strings.NewReader("x")); rec.Code == http.StatusTooManyRequests {
		t.Errorf("a create three seconds later was refused")
	}
}

// stateStream is an open state stream: its status and headers, a reader
// past its first event, and how to close it.
type stateStream struct {
	StatusCode int
	Header     http.Header
}

// openStateStream opens user's state stream and returns it once the
// first event has arrived, by when the stream has been admitted. The
// body is closed when the test ends, or by cancel.
func openStateStream(t *testing.T, base, user string) (stateStream, *bufio.Scanner, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/sessions/events", nil)
	req.Header.Set("X-Test-User", user)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	if resp.StatusCode == http.StatusOK {
		for sc.Scan() && !strings.HasPrefix(sc.Text(), "event: sessions") {
		}
	}
	t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	return stateStream{StatusCode: resp.StatusCode, Header: resp.Header}, sc, cancel
}

// One user holds at most 20 open streams; the 21st is refused while
// another user's opens, and closing one frees its place.
//
// @decision(DL-133)
func TestOneUserHoldsTwentyStreams(t *testing.T) {
	r := telemetrytest.New(t)
	f := newLimitServer(t, nil, func(d *Deps) { d.Metrics = r.Metrics })
	base := f.serve()
	var cancels []context.CancelFunc
	for i := range 20 {
		resp, _, cancel := openStateStream(t, base, "alice")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d = %d", i+1, resp.StatusCode)
		}
		cancels = append(cancels, cancel)
	}
	resp, _, _ := openStateStream(t, base, "alice")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("the 21st stream = %d (Retry-After %q), want 429", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if resp, _, _ := openStateStream(t, base, "bob"); resp.StatusCode != http.StatusOK {
		t.Errorf("another user's stream = %d", resp.StatusCode)
	}
	cancels[0]()
	// The place frees when the handler has returned, which follows the
	// client's close by a moment.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, _, _ := openStateStream(t, base, "alice")
		if resp.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a stream after one closed = %d", resp.StatusCode)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r.Count("spov_requests_limited", "limit", "streams") < 1 {
		t.Errorf("the stream refusal was not recorded")
	}
}

// One user has at most 32 requests in progress, and open streams do not
// count against them.
//
// @decision(DL-133)
func TestOneUserHasThirtyTwoRequestsInProgressStreamsApart(t *testing.T) {
	f := newLimitServer(t, nil, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	f.s.mux.HandleFunc("GET /api/v1/test/block", func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	base := f.serve()
	for range 2 {
		if resp, _, _ := openStateStream(t, base, "alice"); resp.StatusCode != http.StatusOK {
			t.Fatalf("a stream = %d", resp.StatusCode)
		}
	}
	get := func(path string) int {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("X-Test-User", "alice")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if code := get("/api/v1/test/block"); code != http.StatusNoContent {
				t.Errorf("a held request = %d", code)
			}
		})
		<-entered
	}
	if code := get("/api/v1/test/ok"); code != http.StatusTooManyRequests {
		t.Errorf("the 33rd request in progress = %d, want 429", code)
	}
	close(release)
	wg.Wait()
	if code := get("/api/v1/test/ok"); code != http.StatusNoContent {
		t.Errorf("a request once they finished = %d", code)
	}
}

// A body is refused over 1 MiB on every route but the bundle routes,
// which keep their 24 MiB, the device flow's open endpoints included.
//
// @decision(DL-133) @decision(DL-005)
func TestABodyOverOneMiBIsRefusedOutsideTheBundleRoutes(t *testing.T) {
	send := deviceHit(newDeviceFixtureWith(t, "alice@example.com\n", nil))
	big := strings.Repeat("a", MaxBodyBytes+1)
	if rec := send(http.MethodPost, DeviceCodePath, "192.0.2.1:1", strings.NewReader(big)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an open endpoint's large body = %d, want 413", rec.Code)
	}
	f := newLimitServer(t, nil, nil)
	if rec := f.hit(http.MethodPatch, "/api/v1/me", "192.0.2.1:1", "alice", strings.NewReader(big)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an API route's large body = %d, want 413", rec.Code)
	}
	if rec := f.hit(http.MethodPost, "/api/v1/sessions", "192.0.2.1:1", "alice", strings.NewReader(big+big)); rec.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("a bundle route refused 2 MiB")
	}
}

// Health, the pin and the web app are outside every limit.
//
// @decision(DL-133)
func TestHealthThePinAndTheWebAppAreNotLimited(t *testing.T) {
	f := newLimitServer(t, limitsWith(newLimitClock(), func(l *Limits) {
		l.SignIns, l.Requests, l.DevicePolls = Rate{1, 1}, Rate{1, 1}, Rate{1, 1}
	}), nil)
	for range 5 {
		for _, path := range []string{"/livez", OpenCodePinPath, "/", "/sessions/"} {
			if rec := f.hit(http.MethodGet, path, "192.0.2.1:1", "", nil); rec.Code == http.StatusTooManyRequests {
				t.Fatalf("%s was limited", path)
			}
		}
	}
}

func proxyLimiter(trusted ...string) (*limiter, *[]string) {
	var warnings []string
	var prefixes []netip.Prefix
	for _, p := range trusted {
		prefixes = append(prefixes, netip.MustParsePrefix(p))
	}
	return newLimiter(DefaultLimits(), prefixes, func(s string) { warnings = append(warnings, s) }, nil), &warnings
}

func forwarded(peer string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.RemoteAddr = peer
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

// With no trusted proxy the peer is the client, whatever the request
// says, and the first header that is ignored is logged once naming the
// setting.
//
// @decision(DL-133)
func TestAnUntrustedPeersForwardedHeaderIsIgnored(t *testing.T) {
	l, warnings := proxyLimiter()
	for range 3 {
		if got := l.clientKey(forwarded("203.0.113.5:4000", "198.51.100.1")); got != "203.0.113.5" {
			t.Fatalf("client = %s, want the peer", got)
		}
	}
	if len(*warnings) != 1 || !strings.Contains((*warnings)[0], "server.trusted_proxies") {
		t.Errorf("warnings = %q, want one naming server.trusted_proxies", *warnings)
	}
	l, _ = proxyLimiter("10.0.0.0/8")
	if got := l.clientKey(forwarded("203.0.113.5:4000", "198.51.100.1")); got != "203.0.113.5" {
		t.Errorf("a peer outside the trusted range set its address: %s", got)
	}
}

// From a trusted proxy, the client is the right-most address the
// trusted hops did not add; an entry a client wrote on the left is never
// read.
//
// @decision(DL-133)
func TestATrustedProxysForwardedHeaderNamesTheClient(t *testing.T) {
	l, warnings := proxyLimiter("10.0.0.0/8", "192.0.2.10/32")
	cases := []struct {
		name, peer string
		xff        []string
		want       string
	}{
		{"one proxy", "10.1.1.1:80", []string{"203.0.113.5"}, "203.0.113.5"},
		{"a spoofed left-most entry", "10.1.1.1:80", []string{"1.2.3.4, 203.0.113.5"}, "203.0.113.5"},
		{"two proxies, over two headers", "10.1.1.1:80", []string{"1.2.3.4, 203.0.113.5", "192.0.2.10"}, "203.0.113.5"},
		{"no header", "10.1.1.1:80", nil, "10.1.1.1"},
		{"only trusted hops", "10.1.1.1:80", []string{"10.2.2.2"}, "10.2.2.2"},
		{"an entry with a port", "10.1.1.1:80", []string{"203.0.113.5:5555"}, "203.0.113.5"},
		{"a garbled entry ends the walk", "10.1.1.1:80", []string{"203.0.113.5, garbage"}, "10.1.1.1"},
		{"IPv6 counts by its /64", "10.1.1.1:80", []string{"2001:db8:1:2:3:4:5:6"}, "2001:db8:1:2::/64"},
		{"an IPv4-mapped peer", "[::ffff:10.1.1.1]:80", []string{"203.0.113.5"}, "203.0.113.5"},
	}
	for _, c := range cases {
		if got := l.clientKey(forwarded(c.peer, c.xff...)); got != c.want {
			t.Errorf("%s: client = %s, want %s", c.name, got, c.want)
		}
	}
	if len(*warnings) != 0 {
		t.Errorf("a trusted proxy's header was warned about: %q", *warnings)
	}
}

// Behind a trusted proxy each client gets its own sign-in bucket, end
// to end.
//
// @decision(DL-133)
func TestSignInsBehindATrustedProxyAreCountedPerClient(t *testing.T) {
	f := newDeviceFixtureWith(t, "alice@example.com\n", func(d *Deps) {
		d.Limits = limitsWith(newLimitClock(), func(l *Limits) { l.SignIns = Rate{1, 1} })
		d.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	})
	h, host := f.server.Handler(), f.http.Listener.Addr().String()
	login := func(client string) int {
		req := httptest.NewRequest(http.MethodGet, LoginPath, nil)
		req.Host, req.RemoteAddr = host, "10.0.0.2:4000"
		req.Header.Set("X-Forwarded-For", client)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if login("203.0.113.5") != http.StatusSeeOther || login("198.51.100.1") != http.StatusSeeOther {
		t.Fatal("two clients behind one proxy shared a bucket")
	}
	if got := login("203.0.113.5"); got != http.StatusTooManyRequests {
		t.Errorf("a client's second sign-in = %d, want 429", got)
	}
}

// A bucket that has refilled is dropped at the next sweep, one that is
// still draining is kept, and past the key bound new keys share one
// bucket rather than growing the set.
//
// @decision(DL-133)
func TestIdleBucketsAreEvictedAndTheSetIsBounded(t *testing.T) {
	b := newBuckets(Rate{PerMinute: 60, Burst: 10})
	t0 := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	for i := range 5 {
		b.take("idle"+strconv.Itoa(i), t0)
	}
	for range 10 {
		b.take("busy", t0.Add(55*time.Second))
	}
	b.take("new", t0.Add(sweepEvery))
	if got := b.len(); got != 2 {
		t.Errorf("keys after a sweep = %d, want the draining one and the new one", got)
	}
	if _, ok := b.m["busy"]; !ok {
		t.Errorf("a draining bucket was dropped, which would hand its key a full one")
	}

	full := newBuckets(Rate{PerMinute: 60, Burst: 1})
	for i := range maxLimiterKeys {
		full.take(strconv.Itoa(i), t0)
	}
	if ok, _ := full.take("stranger", t0); !ok {
		t.Fatal("the first key past the bound was refused")
	}
	if ok, _ := full.take("another", t0); ok {
		t.Errorf("a second key past the bound got a bucket of its own")
	}
	if got := full.len(); got != maxLimiterKeys+1 {
		t.Errorf("keys = %d, want the bound plus the shared one", got)
	}
	c := &counts{max: 1, m: map[string]int{}}
	c.acquire("a")
	c.release("a")
	if len(c.m) != 0 {
		t.Errorf("a released count kept its key")
	}
}

// A stream has no body, so no read deadline: it outlives one many times
// over, sending its heartbeats.
//
// @decision(DL-133) @decision(DL-018)
func TestAStreamOutlivesTheReadDeadline(t *testing.T) {
	f := newLimitServer(t, limitsWith(newLimitClock(), func(l *Limits) { l.ReadTimeout = 10 * time.Millisecond }), nil)
	f.s.heartbeat = 20 * time.Millisecond
	resp, sc, _ := openStateStream(t, f.serve(), "alice")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d", resp.StatusCode)
	}
	start, beats := time.Now(), 0
	for beats < 10 && sc.Scan() {
		if strings.HasPrefix(sc.Text(), ":") {
			beats++
		}
	}
	if beats < 10 {
		t.Fatalf("the stream ended after %s and %d heartbeats: %v", time.Since(start), beats, sc.Err())
	}
}

// rawRequest writes a request's head and the first part of its body on
// a fresh connection, and returns the connection to finish it on.
func rawRequest(t *testing.T, host, method, path string, length int, first string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, _ = fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: %s\r\nX-Test-User: alice\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		method, path, host, length, first)
	return conn
}

// readStatus reads the response's status, or 0 when the server closed
// the connection without one.
func readStatus(t *testing.T, conn net.Conn) int {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A body must arrive within the read timeout, a bundle route's within
// its longer one, and a handler still working once its body is read is
// not cut by the deadline.
//
// @decision(DL-133)
func TestABodyMustArriveWithinItsReadTimeout(t *testing.T) {
	const short = 40 * time.Millisecond
	f := newLimitServer(t, limitsWith(newLimitClock(), func(l *Limits) {
		l.ReadTimeout, l.BundleReadTimeout = short, 5*time.Second
	}), nil)
	cut := make(chan error, 1)
	f.s.mux.HandleFunc("POST /api/v1/test/slow", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
			cut <- r.Context().Err()
		case <-time.After(5 * short):
			cut <- nil
		}
		w.WriteHeader(http.StatusNoContent)
	})
	f.serve()

	start := time.Now()
	stalled := rawRequest(t, f.host, http.MethodPatch, "/api/v1/me", 20, "{")
	if code := readStatus(t, stalled); code == http.StatusOK || time.Since(start) > 3*time.Second {
		t.Errorf("a body that never arrived = %d after %s, want it cut at the read timeout", code, time.Since(start))
	}

	bundle := rawRequest(t, f.host, http.MethodPost, "/api/v1/sessions/s1/resume", 2, "{")
	// Past the short timeout, which only a real wait can show.
	time.Sleep(3 * short)
	_, _ = io.WriteString(bundle, "}")
	if code := readStatus(t, bundle); code == 0 {
		t.Errorf("a bundle route's body was cut at the short timeout")
	}

	slow := rawRequest(t, f.host, http.MethodPost, "/api/v1/test/slow", 2, "{}")
	if code := readStatus(t, slow); code != http.StatusNoContent {
		t.Errorf("the slow handler = %d", code)
	}
	if err := <-cut; err != nil {
		t.Errorf("a handler that had read its body was cancelled by the read deadline: %v", err)
	}
}

// Every operation the API document marks as a stream is counted as one,
// and the creates are exactly the routes that take a bundle.
//
// @decision(DL-133)
func TestEveryStreamAndCreateRouteIsClassified(t *testing.T) {
	f := newLimitServer(t, nil, nil)
	streams, creates := 0, 0
	for _, item := range f.s.api.OpenAPI().Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch} {
			if op == nil {
				continue
			}
			class := f.s.lim.classes[op.Method+" "+op.Path]
			if stream, _ := op.Metadata[streamMetadata].(bool); stream != class.stream {
				t.Errorf("%s: stream %v, classified %v", op.OperationID, stream, class.stream)
			}
			bundle := maxBodyBytesFor(strings.ReplaceAll(op.Path, "{ref}", "s1")) > MaxBodyBytes && op.Method == http.MethodPost
			if bundle != class.create {
				t.Errorf("%s: takes a bundle %v, classified a create %v", op.OperationID, bundle, class.create)
			}
			if class.stream {
				streams++
			}
			if class.create {
				creates++
			}
		}
	}
	if streams != 3 || creates != len(createOperations) {
		t.Errorf("streams %d and creates %d, want the three event streams and every one of %v", streams, creates, createOperations)
	}
}
