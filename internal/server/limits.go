package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/telemetry"
)

// The request limits. Every one is in memory and per replica. The body
// limits are MaxBodyBytes and MaxSessionBodyBytes in api.go.
//
// @decision(DL-133)
const (
	// SignInsPerMinute and SignInBurst bound the sign-ins one client
	// address starts: GET /login and POST /device/code together.
	SignInsPerMinute = 30
	SignInBurst      = 30
	// DevicePollsPerMinute and DevicePollBurst bound one client
	// address's polls of the device token endpoint.
	DevicePollsPerMinute = 120
	DevicePollBurst      = 120
	// NetworkSignInsPerMinute and NetworkDevicePollsPerMinute bound the
	// same for one network (netKey), burst equal to the rate, so one
	// allocation's many addresses cannot multiply the per-address rate.
	NetworkSignInsPerMinute     = 300
	NetworkDevicePollsPerMinute = 1_200
	// MaxPendingPerNetwork is the most places one network holds in
	// each of the two caps below.
	MaxPendingPerNetwork = 500
	// MaxPendingLogins and MaxPendingDeviceCodes bound the sign-ins
	// this replica starts within a row's life (FlowLifetime and
	// DeviceCodeLifetime), whoever starts them.
	MaxPendingLogins      = 10_000
	MaxPendingDeviceCodes = 10_000
	// RequestsPerMinute and RequestBurst bound one user's requests
	// under APIPrefix.
	RequestsPerMinute = 600
	RequestBurst      = 100
	// CreatesPerMinute and CreateBurst bound one user's session
	// creates, job creates and session resumes (createOperations).
	CreatesPerMinute = 20
	CreateBurst      = 20
	// MaxStreamsPerUser bounds one user's open event streams.
	MaxStreamsPerUser = 20
	// MaxInFlightPerUser bounds one user's requests being served at
	// once, streams not counted.
	MaxInFlightPerUser = 32
	// ReadTimeout is how long a request has to send its body, and
	// BundleReadTimeout the same for a route that takes a bundle.
	ReadTimeout       = 30 * time.Second
	BundleReadTimeout = 5 * time.Minute
	// HeaderTimeout is how long a request has to send its headers.
	HeaderTimeout = 10 * time.Second
	// IdleTimeout closes a keep-alive connection that sends nothing.
	IdleTimeout = 2 * time.Minute
)

const (
	// maxLimiterKeys bounds the keys one bucket set holds. Past it, a
	// new key shares one overflow bucket until idle ones are swept.
	maxLimiterKeys = 100_000
	// overflowKey is that shared bucket's key; no address or user
	// renders as it.
	overflowKey = "\x00overflow"
	// sweepEvery is how often a bucket set drops its idle keys.
	sweepEvery = time.Minute
	// streamRetryAfter and inFlightRetryAfter are what a refusal by a
	// count says to wait, since a count has no refill time.
	streamRetryAfter   = 10 * time.Second
	inFlightRetryAfter = time.Second
)

// createOperations are the operations CreatesPerMinute counts: each
// starts a sandbox.
//
// @decision(DL-133)
var createOperations = map[string]bool{
	"create-session": true,
	"create-job":     true,
	"resume-session": true,
}

// Rate is a token bucket: PerMinute tokens a minute, at most Burst held.
//
// @decision(DL-133)
type Rate struct {
	PerMinute int
	Burst     int
}

// Limits are the limits in force. The constants above are the only
// values a server runs with; Deps.Limits is a seam for a test.
//
// @decision(DL-133)
type Limits struct {
	SignIns, DevicePolls             Rate
	NetworkSignIns, NetworkPolls     Rate
	PendingLogins, PendingDeviceCode int
	PendingPerNetwork                int
	Requests, Creates                Rate
	Streams, InFlight                int
	ReadTimeout, BundleReadTimeout   time.Duration
	// Now is the limiters' clock; nil means time.Now. It is not
	// Deps.Now, which tests pin to a fixed instant a bucket would never
	// refill at.
	Now func() time.Time
}

// DefaultLimits returns the constants above.
//
// @decision(DL-133)
func DefaultLimits() Limits {
	return Limits{
		SignIns:           Rate{SignInsPerMinute, SignInBurst},
		DevicePolls:       Rate{DevicePollsPerMinute, DevicePollBurst},
		NetworkSignIns:    Rate{NetworkSignInsPerMinute, NetworkSignInsPerMinute},
		NetworkPolls:      Rate{NetworkDevicePollsPerMinute, NetworkDevicePollsPerMinute},
		PendingLogins:     MaxPendingLogins,
		PendingDeviceCode: MaxPendingDeviceCodes,
		PendingPerNetwork: MaxPendingPerNetwork,
		Requests:          Rate{RequestsPerMinute, RequestBurst},
		Creates:           Rate{CreatesPerMinute, CreateBurst},
		Streams:           MaxStreamsPerUser,
		InFlight:          MaxInFlightPerUser,
		ReadTimeout:       ReadTimeout,
		BundleReadTimeout: BundleReadTimeout,
	}
}

// limiter holds the limits' state for one server.
//
// @decision(DL-133)
type limiter struct {
	l                        Limits
	signIns, polls           *buckets
	netSignIns, netPolls     *buckets
	requests, creates        *buckets
	logins, deviceCodes      *window
	streams, inFlight        *counts
	trusted                  []netip.Prefix
	warn                     func(string)
	metrics                  *telemetry.Metrics
	forwardedWarned          sync.Once
	classes                  map[string]routeClass
	routeOf                  func(*http.Request) string
	loginDeny, deviceRefusal func(http.ResponseWriter, int, string)
}

// routeClass is what the per-user limits need to know of a route.
type routeClass struct{ stream, create bool }

func newLimiter(l Limits, trusted []netip.Prefix, warn func(string), m *telemetry.Metrics) *limiter {
	if l.Now == nil {
		l.Now = time.Now
	}
	return &limiter{
		l:           l,
		signIns:     newBuckets(l.SignIns),
		polls:       newBuckets(l.DevicePolls),
		netSignIns:  newBuckets(l.NetworkSignIns),
		netPolls:    newBuckets(l.NetworkPolls),
		requests:    newBuckets(l.Requests),
		creates:     newBuckets(l.Creates),
		logins:      newWindow(l.PendingLogins, l.PendingPerNetwork, FlowLifetime),
		deviceCodes: newWindow(l.PendingDeviceCode, l.PendingPerNetwork, DeviceCodeLifetime),
		streams:     &counts{max: l.Streams, m: map[string]int{}},
		inFlight:    &counts{max: l.InFlight, m: map[string]int{}},
		trusted:     trusted,
		warn:        warn,
		metrics:     m,
	}
}

// readDeadline gives a request that has a body ReadTimeout, or
// BundleReadTimeout on a bundle route, to send it. net/http clears the
// deadline when the body ends and it starts watching for the client to
// go away, so a handler still working is not cut by it. A request with
// no body, which every stream is, gets none, and ReadHeaderTimeout
// alone covers it.
//
// @decision(DL-133)
func (l *limiter) readDeadline(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return
	}
	d := l.l.ReadTimeout
	if maxBodyBytesFor(r.URL.Path) > MaxBodyBytes {
		d = l.l.BundleReadTimeout
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
}

// keepReadDeadline stops huma replacing readDeadline's deadline with its
// own five-second one per operation, while letting it lift the deadline
// once it has read the body.
//
// @decision(DL-133)
func keepReadDeadline(ctx huma.Context, next func(huma.Context)) {
	next(deadlineContext{ctx})
}

// humaContext is embedded under its own name, since huma.Context has a
// method called Context.
type humaContext = huma.Context

type deadlineContext struct{ humaContext }

func (c deadlineContext) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		return c.humaContext.SetReadDeadline(t)
	}
	return nil
}

// tooLarge refuses a body whose declared length is over the route's
// limit before any of it is read. A body that does not declare its
// length is cut by http.MaxBytesReader as it is read.
//
// @decision(DL-133) @decision(DL-005)
func tooLarge(w http.ResponseWriter, r *http.Request) bool {
	limit := maxBodyBytesFor(r.URL.Path)
	if r.ContentLength <= limit {
		return false
	}
	writeProblem(w, http.StatusRequestEntityTooLarge, "Request Entity Too Large",
		"the request body is over the "+strconv.FormatInt(limit>>20, 10)+" MiB this route accepts")
	return true
}

// signIn admits the start of a browser login or a device code: the
// client address's bucket, then its network's, then the network's
// share of the replica's count of starts still within their row's
// life, then that count. It runs behind the browser guard, so a request
// the guard refuses spends nothing. A refusal is already written.
//
// @decision(DL-133)
func (l *limiter) signIn(w http.ResponseWriter, r *http.Request, device bool) bool {
	refuse := l.loginDeny
	pending, limit, share := l.logins, telemetry.LimitSignInCap, telemetry.LimitSignInShare
	if device {
		refuse, pending, limit, share = l.deviceRefusal, l.deviceCodes, telemetry.LimitDeviceCodeCap, telemetry.LimitDeviceCodeShare
	}
	now := l.l.Now()
	addr, network := l.clientKeys(r)
	if ok, wait, from := take2(l.signIns, l.netSignIns, addr, network, now); !ok {
		l.refused(r, telemetry.LimitSignInRate)
		w.Header().Set("Retry-After", retryAfter(wait))
		refuse(w, http.StatusTooManyRequests, "Too many sign-ins were started from your "+from+". Try again in "+retryAfter(wait)+" seconds.")
		return false
	}
	switch ok, wait, ours := pending.admit(network, now); {
	case ok:
		return true
	case ours:
		l.refused(r, share)
		w.Header().Set("Retry-After", retryAfter(wait))
		refuse(w, http.StatusTooManyRequests, "Too many of this server's sign-ins in progress were started from your network. Try again in "+retryAfter(wait)+" seconds.")
	default:
		l.refused(r, limit)
		w.Header().Set("Retry-After", retryAfter(wait))
		refuse(w, http.StatusServiceUnavailable, "This server has too many sign-ins in progress. Try again in "+retryAfter(wait)+" seconds.")
	}
	return false
}

// take2 takes a token from the address's bucket and then the network's,
// giving the address's back when the network's is empty, and names
// which one refused.
//
// @decision(DL-133)
func take2(byAddr, byNet *buckets, addr, network string, now time.Time) (bool, time.Duration, string) {
	if ok, wait := byAddr.take(addr, now); !ok {
		return false, wait, "address"
	}
	if ok, wait := byNet.take(network, now); !ok {
		byAddr.give(addr)
		return false, wait, "network"
	}
	return true, 0, ""
}

// devicePoll admits one poll of the device token endpoint, by the
// address's bucket and the network's. A refusal is RFC 8628's
// slow_down, which the CLI's client answers by polling less often.
//
// @decision(DL-133) @decision(DL-006)
func (l *limiter) devicePoll(w http.ResponseWriter, r *http.Request) bool {
	addr, network := l.clientKeys(r)
	ok, wait, from := take2(l.polls, l.netPolls, addr, network, l.l.Now())
	if ok {
		return true
	}
	l.refused(r, telemetry.LimitDevicePollRate)
	w.Header().Set("Retry-After", retryAfter(wait))
	oauthError(w, http.StatusTooManyRequests, "slow_down", "too many polls from this "+from)
	return false
}

// perUser applies the signed-in limits to every request under
// APIPrefix. It sits behind authentication, so every request it sees
// has a principal.
//
// @decision(DL-133)
func (l *limiter) perUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, APIPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		p, _ := auth.From(r.Context())
		key := p.OrgID + "\x00" + p.UserID
		now := l.l.Now()
		if ok, wait := l.requests.take(key, now); !ok {
			l.tooMany(w, r, telemetry.LimitRequests, wait, "You are sending requests faster than this server allows")
			return
		}
		class := l.classes[l.routeOf(r)]
		if class.create {
			if ok, wait := l.creates.take(key, now); !ok {
				l.tooMany(w, r, telemetry.LimitCreates, wait, "You are starting sessions and jobs faster than this server allows")
				return
			}
		}
		held, limit, wait, detail := l.inFlight, telemetry.LimitInFlight, inFlightRetryAfter,
			"You have "+strconv.Itoa(l.l.InFlight)+" requests in progress, the most this server serves at once for one user"
		if class.stream {
			held, limit, wait, detail = l.streams, telemetry.LimitStreams, streamRetryAfter,
				"You have "+strconv.Itoa(l.l.Streams)+" live views open, the most this server allows; close a tab or a terminal that is following a session"
		}
		if !held.acquire(key) {
			l.tooMany(w, r, limit, wait, detail)
			return
		}
		defer held.release(key)
		next.ServeHTTP(w, r)
	})
}

func (l *limiter) tooMany(w http.ResponseWriter, r *http.Request, limit telemetry.RequestLimit, wait time.Duration, detail string) {
	l.refused(r, limit)
	w.Header().Set("Retry-After", retryAfter(wait))
	writeProblem(w, http.StatusTooManyRequests, "Too Many Requests", detail+". Try again in "+retryAfter(wait)+" seconds.")
}

func (l *limiter) refused(r *http.Request, limit telemetry.RequestLimit) {
	l.metrics.RequestLimited(r.Context(), limit)
}

// clientKey is the address a per-address limit counts a request
// against: the peer, or behind a trusted proxy the right-most
// X-Forwarded-For entry that is not itself trusted. Nothing else reads
// it. An IPv6 address counts as its /64, the block one subscriber
// usually holds.
//
// @decision(DL-133)
func (l *limiter) clientKey(r *http.Request) string {
	addr, _ := l.clientKeys(r)
	return addr
}

// clientKeys is clientKey and the network that address is in (netKey).
// A peer that does not parse is its own address and network.
//
// @decision(DL-133)
func (l *limiter) clientKeys(r *http.Request) (addr, network string) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr, r.RemoteAddr
	}
	client := peer.Addr().Unmap()
	forwarded := r.Header.Values("X-Forwarded-For")
	if !l.isTrusted(client) {
		if len(forwarded) > 0 {
			l.forwardedWarned.Do(func() {
				l.warn("a request from " + client.String() + " carried X-Forwarded-For, which is ignored because that address is not in server.trusted_proxies;" +
					" behind a reverse proxy every client shares the proxy's address for the sign-in limits until the setting names it")
			})
		}
	} else {
		client = l.forwardedClient(client, forwarded)
	}
	return addrKey(client), netKey(client)
}

// forwardedClient walks X-Forwarded-For from the right, past the
// trusted proxies, to the first address one of them did not add. An
// entry that does not parse ends the walk at the last good one.
func (l *limiter) forwardedClient(peer netip.Addr, values []string) netip.Addr {
	var hops []string
	for _, v := range values {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			return client
		}
		client = a
		if !l.isTrusted(a) {
			return a
		}
	}
	return client
}

func (l *limiter) isTrusted(a netip.Addr) bool {
	for _, p := range l.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}

func addrKey(a netip.Addr) string {
	if a.Is6() {
		return netip.PrefixFrom(a, 64).Masked().String()
	}
	return a.String()
}

// netKey is the network an address counts in for the coarser limits:
// an IPv4 address's /24, an IPv6 address's /48.
//
// @decision(DL-133)
func netKey(a netip.Addr) string {
	bits := 24
	if a.Is6() {
		bits = 48
	}
	return netip.PrefixFrom(a, bits).Masked().String()
}

// retryAfter renders a wait as Retry-After's whole seconds, at least 1.
func retryAfter(d time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(d.Seconds()))))
}

// writeProblem writes an RFC 9457 body in the shape the API's own
// errors take, so a client reads a limit's refusal as any other.
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail,omitempty"`
	}{title, status, detail})
}

// buckets is a set of token buckets by key. A bucket that has refilled
// to its burst holds nothing a new one would not, so a sweep drops it.
//
// @decision(DL-133)
type buckets struct {
	mu     sync.Mutex
	perSec float64
	burst  float64
	m      map[string]*bucket
	swept  time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newBuckets(r Rate) *buckets {
	return &buckets{perSec: float64(r.PerMinute) / 60, burst: float64(r.Burst), m: map[string]*bucket{}}
}

// take spends one token of key's bucket, or reports how long until one
// is there.
func (b *buckets) take(key string, now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.swept) >= sweepEvery {
		b.sweep(now)
	}
	k := b.m[key]
	if k == nil {
		if len(b.m) >= maxLimiterKeys {
			key = overflowKey
			k = b.m[key]
		}
		if k == nil {
			k = &bucket{tokens: b.burst, at: now}
			b.m[key] = k
		}
	}
	b.refill(k, now)
	if k.tokens >= 1 {
		k.tokens--
		return true, 0
	}
	return false, time.Duration((1 - k.tokens) / b.perSec * float64(time.Second))
}

// give returns a token take spent, for a request a later limit
// refused. A key take sent to the overflow bucket gets nothing back.
func (b *buckets) give(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if k := b.m[key]; k != nil {
		k.tokens = min(b.burst, k.tokens+1)
	}
}

func (b *buckets) refill(k *bucket, now time.Time) {
	if elapsed := now.Sub(k.at); elapsed > 0 {
		k.tokens = min(b.burst, k.tokens+elapsed.Seconds()*b.perSec)
		k.at = now
	}
}

func (b *buckets) sweep(now time.Time) {
	b.swept = now
	for key, k := range b.m {
		if b.refill(k, now); k.tokens >= b.burst {
			delete(b.m, key)
		}
	}
}

// len is how many keys the set holds, for a test.
func (b *buckets) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.m)
}

// window counts the starts within life of now, up to max, and up to
// share of them from one network. Its slice never holds more than max
// starts, and byNet holds a key only while that network has one in it.
//
// @decision(DL-133)
type window struct {
	mu    sync.Mutex
	max   int
	share int
	life  time.Duration
	at    []start
	byNet map[string][]time.Time
}

type start struct {
	at      time.Time
	network string
}

func newWindow(max, share int, life time.Duration) *window {
	return &window{max: max, share: share, life: life, byNet: map[string][]time.Time{}}
}

// admit takes a place for network, or reports how long until one is
// free and whether it was the network's share that refused.
func (w *window) admit(network string, now time.Time) (ok bool, wait time.Duration, share bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	gone := 0
	for gone < len(w.at) && !now.Before(w.at[gone].at.Add(w.life)) {
		n := w.at[gone].network
		if rest := w.byNet[n][1:]; len(rest) > 0 {
			w.byNet[n] = rest
		} else {
			delete(w.byNet, n)
		}
		gone++
	}
	w.at = w.at[gone:]
	if held := w.byNet[network]; len(held) >= w.share {
		return false, held[0].Add(w.life).Sub(now), true
	}
	if len(w.at) >= w.max {
		return false, w.at[0].at.Add(w.life).Sub(now), false
	}
	w.at = append(w.at, start{now, network})
	w.byNet[network] = append(w.byNet[network], now)
	return true, 0, false
}

// networks is how many networks the window holds a start for, for a
// test.
func (w *window) networks() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.byNet)
}

// counts is how many of something each key holds at once. A key is
// dropped when it holds none.
//
// @decision(DL-133)
type counts struct {
	mu  sync.Mutex
	max int
	m   map[string]int
}

func (c *counts) acquire(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[key] >= c.max {
		return false
	}
	c.m[key]++
	return true
}

func (c *counts) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[key]--; c.m[key] <= 0 {
		delete(c.m, key)
	}
}
