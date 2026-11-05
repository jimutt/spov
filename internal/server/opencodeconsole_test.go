package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/console"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// testClock is a clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type consoleFixture struct {
	*apiFixture
	console *console.Fake
	clock   *testClock
	grants  ConsoleStore
}

// newConsoleFixture is a server that can hold OpenCode sign-ins, against
// a fake console. With proxied, launches get the credential proxy.
func newConsoleFixture(t *testing.T, proxied bool, adjust ...func(*Deps)) *consoleFixture {
	t.Helper()
	fake := console.NewFake()
	t.Cleanup(fake.Close)
	f := &consoleFixture{console: fake, clock: &testClock{now: time.Now().UTC()}, grants: &MemoryConsoleStore{}}
	f.apiFixture = newAPIFixtureWith(t, func(d *Deps) {
		d.Console = &console.Client{BaseURL: fake.URL, UserAgent: "spov/test"}
		d.ConsoleGrants = f.grants
		d.Now = f.clock.Now
		if proxied {
			image := func(context.Context) (string, error) { return "spov-proxy-test", nil }
			d.Launch = NewLaunch(&session.Launcher{
				Sandboxes:     d.Provider,
				NewClient:     func(string, string) session.HealthChecker { return stubHealth{} },
				HealthTimeout: 5 * time.Second,
				ProxyImage:    image,
			})
			d.ProxyImage = image
		}
		for _, fn := range adjust {
			fn(d)
		}
	})
	return f
}

// connect runs a whole sign-in through the API and returns the attempt.
func (f *consoleFixture) connect() OpenCodeAttemptView {
	f.t.Helper()
	resp, body := f.do(http.MethodPost, "/api/v1/me/opencode/connect", nil)
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("start = %d: %s", resp.StatusCode, body)
	}
	var a OpenCodeAttemptView
	if err := json.Unmarshal(body, &a); err != nil {
		f.t.Fatal(err)
	}
	f.console.Approve(a.UserCode)
	f.clock.advance(time.Duration(a.Interval) * time.Second)
	got := f.check(a.ID)
	if got.State != AttemptConnected {
		f.t.Fatalf("after approval the attempt is %+v", got)
	}
	return got
}

func (f *consoleFixture) check(id string) OpenCodeAttemptView {
	f.t.Helper()
	resp, body := f.do(http.MethodGet, "/api/v1/me/opencode/connect/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("check = %d: %s", resp.StatusCode, body)
	}
	var a OpenCodeAttemptView
	if err := json.Unmarshal(body, &a); err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *consoleFixture) grant() ConsoleGrant {
	f.t.Helper()
	g, ok, err := readGrant(context.Background(), f.grants, auth.LocalID, auth.LocalID)
	if err != nil || !ok {
		f.t.Fatalf("no grant: %v", err)
	}
	return g
}

// A sign-in started through the API waits for approval, polls the
// console no more often than it asked, and ends connected. No answer
// carries a token or the device code.
//
// @decision(DL-015)
func TestASignInConnectsAndNoAnswerCarriesAToken(t *testing.T) {
	f := newConsoleFixture(t, false)
	resp, body := f.do(http.MethodPost, "/api/v1/me/opencode/connect", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start = %d: %s", resp.StatusCode, body)
	}
	var a OpenCodeAttemptView
	_ = json.Unmarshal(body, &a)
	if a.State != AttemptPending || a.UserCode == "" || !strings.HasPrefix(a.VerificationURL, f.console.URL) || a.Interval != 5 {
		t.Fatalf("attempt = %+v", a)
	}
	f.console.Approve(a.UserCode)
	if got := f.check(a.ID); got.State != AttemptPending {
		t.Errorf("a check before the interval polled the console: %+v", got)
	}
	f.clock.advance(5 * time.Second)
	if got := f.check(a.ID); got.State != AttemptConnected {
		t.Fatalf("after the interval: %+v", got)
	}
	if got := f.check(a.ID); got.State != AttemptConnected {
		t.Errorf("a later check of a finished attempt: %+v", got)
	}

	g := f.grant()
	if g.Generation != 1 || g.OrgID != "org_1" {
		t.Errorf("grant = generation %d, org %q", g.Generation, g.OrgID)
	}
	resp, body = f.do(http.MethodGet, "/api/v1/me/opencode", nil)
	var v OpenCodeConnectionView
	_ = json.Unmarshal(body, &v)
	if resp.StatusCode != http.StatusOK || !v.Enabled || !v.Connected || v.OrgID != "org_1" {
		t.Errorf("connection = %d %+v", resp.StatusCode, v)
	}
	for _, path := range []string{"/api/v1/me/opencode", "/api/v1/me/opencode/connect/" + a.ID, "/api/v1/models"} {
		_, body := f.do(http.MethodGet, path, nil)
		for _, secret := range []string{g.Access, g.Refresh, "dc_"} {
			if strings.Contains(string(body), secret) {
				t.Errorf("%s carries a secret: %s", path, body)
			}
		}
	}
	for _, ua := range f.console.UserAgents {
		if ua != "spov/test" {
			t.Errorf("a console call identified as %q", ua)
		}
	}
}

// A connected sign-in is the caller's OPENCODE_API_KEY, so both OpenCode
// providers are offered as usable.
//
// @decision(DL-015) @decision(DL-013)
func TestASignInMakesTheOpenCodeModelsUsable(t *testing.T) {
	f := newConsoleFixture(t, true)
	held := func() bool {
		h, err := f.srv.svc.heldProviders(context.Background(), auth.LocalID, auth.LocalID)
		if err != nil {
			t.Fatal(err)
		}
		return h[prep.OpenCodeKeyEnv]
	}
	if held() {
		t.Fatal("OPENCODE_API_KEY is held before any sign-in")
	}
	f.connect()
	if !held() {
		t.Error("a connected sign-in is not counted as OPENCODE_API_KEY")
	}
}

// The model list says which providers a sign-in can unlock, so the
// browser can point at it, and only where a session would be given it.
//
// @decision(DL-015)
func TestTheModelListNamesTheSignInThatUnlocksAProvider(t *testing.T) {
	signIns := func(f *consoleFixture) map[string]string {
		_, body := f.do(http.MethodGet, "/api/v1/models", nil)
		var out struct{ Providers []ModelProviderView }
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, p := range out.Providers {
			got[p.Provider] = p.SignIn
		}
		return got
	}
	got := signIns(newConsoleFixture(t, true))
	if got["opencode-go"] != "opencode" || got["opencode"] != "opencode" {
		t.Errorf("the OpenCode providers name no sign-in: %v", got)
	}
	for id, s := range got {
		if s != "" && id != "opencode-go" && id != "opencode" {
			t.Errorf("%s names sign-in %q", id, s)
		}
	}
	if got := signIns(newConsoleFixture(t, false)); got["opencode-go"] != "" {
		t.Errorf("a server without the proxy offers the sign-in: %v", got)
	}
}

// A denial, an expiry, and a console that keeps failing each end the
// attempt with a reason; an unknown attempt is a 404.
//
// @decision(DL-015)
func TestASignInThatCannotFinishSaysWhy(t *testing.T) {
	f := newConsoleFixture(t, false)
	start := func() OpenCodeAttemptView {
		_, body := f.do(http.MethodPost, "/api/v1/me/opencode/connect", nil)
		var a OpenCodeAttemptView
		_ = json.Unmarshal(body, &a)
		return a
	}

	a := start()
	f.console.Deny(a.UserCode)
	f.clock.advance(5 * time.Second)
	if got := f.check(a.ID); got.State != AttemptFailed || !strings.Contains(got.Reason, "denied") {
		t.Errorf("denied: %+v", got)
	}

	a = start()
	f.clock.advance(11 * time.Minute)
	if got := f.check(a.ID); got.State != AttemptExpired {
		t.Errorf("expired: %+v", got)
	}

	a = start()
	for i := 0; i < consolePollRetries; i++ {
		f.console.PollError = "server_error"
		f.clock.advance(5 * time.Second)
		if got := f.check(a.ID); got.State != AttemptPending {
			t.Fatalf("retry %d ended the attempt: %+v", i, got)
		}
	}
	f.console.PollError = "server_error"
	f.clock.advance(5 * time.Second)
	if got := f.check(a.ID); got.State != AttemptFailed {
		t.Errorf("a console that kept failing: %+v", got)
	}

	if resp, _ := f.do(http.MethodGet, "/api/v1/me/opencode/connect/oca_nope", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown attempt = %d", resp.StatusCode)
	}
	if _, ok, _ := readGrant(context.Background(), f.grants, auth.LocalID, auth.LocalID); ok {
		t.Error("an attempt that never connected stored a grant")
	}
}

// Without a console or a place to keep sign-ins, the endpoints say so.
//
// @decision(DL-015)
func TestAServerThatCannotHoldASignInSaysSo(t *testing.T) {
	f := newAPIFixture(t)
	if resp, _ := f.do(http.MethodPost, "/api/v1/me/opencode/connect", nil); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("start = %d", resp.StatusCode)
	}
	resp, body := f.do(http.MethodGet, "/api/v1/me/opencode", nil)
	var v OpenCodeConnectionView
	_ = json.Unmarshal(body, &v)
	if resp.StatusCode != http.StatusOK || v.Enabled || v.Connected {
		t.Errorf("connection = %d %+v", resp.StatusCode, v)
	}
}

// Disconnecting forgets the grant.
//
// @decision(DL-015)
func TestDisconnectingForgetsTheSignIn(t *testing.T) {
	f := newConsoleFixture(t, false)
	f.connect()
	if resp, body := f.do(http.MethodDelete, "/api/v1/me/opencode", nil); resp.StatusCode >= 300 {
		t.Fatalf("delete = %d: %s", resp.StatusCode, body)
	}
	if g, ok, _ := readGrant(context.Background(), f.grants, auth.LocalID, auth.LocalID); ok && (g.Access != "" || g.Refresh != "") {
		t.Error("the grant's tokens survived a disconnect")
	}
	_, body := f.do(http.MethodGet, "/api/v1/me/opencode", nil)
	var v OpenCodeConnectionView
	_ = json.Unmarshal(body, &v)
	if v.Connected {
		t.Errorf("still connected after a disconnect: %+v", v)
	}
}

// A launch carries the sign-in's token in the proxy only, set on the
// listed opencode.ai paths, and records which generation it holds.
//
// @decision(DL-015)
func TestALaunchGivesTheSignInToTheProxyAndRecordsItsGeneration(t *testing.T) {
	f := newConsoleFixture(t, true)
	f.connect()
	g := f.grant()
	id := f.create(repoReq("alpha"))
	f.events(id)

	specs := f.provider.Specs()
	if len(specs) != 1 {
		t.Fatalf("%d sandboxes", len(specs))
	}
	spec := specs[0]
	if v := spec.Secrets[prep.OpenCodeKeyEnv]; v == "" || v == g.Access {
		t.Errorf("the main container holds %q, not a placeholder", v)
	}
	var cfg credproxy.SidecarConfig
	if err := json.Unmarshal([]byte(spec.Sidecar.Secrets[credproxy.EnvConfig]), &cfg); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(cfg.Providers, func(p credproxy.Provider) bool { return p.Env == prep.OpenCodeKeyEnv })
	if i < 0 || cfg.Providers[i].Key != g.Access || cfg.Providers[i].Host != "opencode.ai" || len(cfg.Providers[i].Paths) == 0 {
		t.Fatalf("the sidecar's providers = %+v", cfg.Providers)
	}
	row, err := f.store.Sessions().Get(context.Background(), auth.LocalID, id)
	if err != nil || row.OpenCodeGrant != g.Generation {
		t.Errorf("the session holds generation %d, want %d (%v)", row.OpenCodeGrant, g.Generation, err)
	}
}

// Without the proxy the sign-in is left out: it is an account
// credential, and the main container is where the agent reads. A pasted
// key still goes in, as other keys do there, and the model check says
// why the sign-in cannot be used.
//
// @decision(DL-015)
func TestALaunchWithoutTheProxyLeavesTheSignInOut(t *testing.T) {
	f := newConsoleFixture(t, false, func(d *Deps) {
		d.ProviderKeys = map[string]string{prep.OpenCodeKeyEnv: "pasted-key"}
	})
	f.connect()
	id := f.create(repoReq("alpha"))
	f.events(id)
	if v := f.provider.Specs()[0].Secrets[prep.OpenCodeKeyEnv]; v != "pasted-key" {
		t.Errorf("OPENCODE_API_KEY = %q, want the pasted key", v)
	}
	if row, _ := f.store.Sessions().Get(context.Background(), auth.LocalID, id); row.OpenCodeGrant != 0 {
		t.Errorf("generation %d recorded for a sandbox without the proxy", row.OpenCodeGrant)
	}
	g := newConsoleFixture(t, false)
	g.connect()
	resp, body := g.do(http.MethodPost, "/api/v1/sessions", func() SessionRequest { r := repoReq("beta"); r.Model = "opencode-go/kimi-k3"; return r }())
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "credential proxy") {
		t.Errorf("a Go model without the proxy = %d: %s", resp.StatusCode, body)
	}
}

// A sign-in whose access token has lapsed is still offered, and the
// launch refreshes it rather than the model check refusing it.
//
// @decision(DL-015)
func TestALapsedTokenIsRefreshedNotRefused(t *testing.T) {
	f := newConsoleFixture(t, true)
	f.console.ExpiresIn = 3600
	f.connect()
	f.console.ExpiresIn = 30 * 24 * 3600
	f.clock.advance(2 * time.Hour)
	if h, _ := f.srv.svc.heldProviders(context.Background(), auth.LocalID, auth.LocalID); !h[prep.OpenCodeKeyEnv] {
		t.Fatal("a lapsed but refreshable sign-in is not offered")
	}
	req := repoReq("alpha")
	req.Model = "opencode-go/kimi-k3"
	id := f.create(req)
	f.events(id)
	g := f.grant()
	if g.Generation != 2 || f.console.Refreshes != 1 {
		t.Errorf("generation %d after %d refreshes", g.Generation, f.console.Refreshes)
	}
	var cfg credproxy.SidecarConfig
	_ = json.Unmarshal([]byte(f.provider.Specs()[0].Sidecar.Secrets[credproxy.EnvConfig]), &cfg)
	if i := slices.IndexFunc(cfg.Providers, func(p credproxy.Provider) bool { return p.Env == prep.OpenCodeKeyEnv }); i < 0 || cfg.Providers[i].Key != g.Access {
		t.Errorf("the sidecar does not hold the refreshed token")
	}
}

// A refresh the launch was cancelled during still stores its grant: the
// console has already rotated the tokens.
//
// @decision(DL-015)
func TestACancelledLaunchStillStoresItsRefresh(t *testing.T) {
	f := newConsoleFixture(t, true)
	f.console.ExpiresIn = 3600
	f.connect()
	f.console.ExpiresIn = 30 * 24 * 3600
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, gen, _, err := f.srv.svc.freshConsoleToken(ctx, auth.LocalID, auth.LocalID, time.Hour); err != nil || gen != 2 {
		t.Fatalf("gen %d, %v", gen, err)
	}
	if g := f.grant(); g.Generation != 2 || !f.console.Valid(g.Access) {
		t.Error("the refreshed grant was not stored")
	}
}

// A dead sign-in does not shadow a pasted key, and the model check asks
// for a reconnect when there is no key at all.
//
// @decision(DL-015)
func TestADeadSignInFallsBackOrAsksForAReconnect(t *testing.T) {
	f := newConsoleFixture(t, true, func(d *Deps) {
		d.ProviderKeys = map[string]string{prep.OpenCodeKeyEnv: "pasted-key"}
	})
	f.console.ExpiresIn = 3600
	f.connect()
	f.srv.svc.deps.Console = refusingConsole{f.srv.svc.deps.Console}
	id := f.create(repoReq("alpha"))
	f.events(id)
	var cfg credproxy.SidecarConfig
	_ = json.Unmarshal([]byte(f.provider.Specs()[0].Sidecar.Secrets[credproxy.EnvConfig]), &cfg)
	if i := slices.IndexFunc(cfg.Providers, func(p credproxy.Provider) bool { return p.Env == prep.OpenCodeKeyEnv }); i < 0 || cfg.Providers[i].Key != "pasted-key" {
		t.Errorf("the sidecar holds %+v, want the pasted key", cfg.Providers)
	}

	g := newConsoleFixture(t, true)
	g.console.ExpiresIn = 3600
	g.connect()
	g.srv.svc.deps.Console = refusingConsole{g.srv.svc.deps.Console}
	if _, _, _, err := g.srv.svc.freshConsoleToken(context.Background(), auth.LocalID, auth.LocalID, time.Hour); err != nil {
		t.Fatal(err)
	}
	resp, body := g.do(http.MethodPost, "/api/v1/sessions", func() SessionRequest { r := repoReq("beta"); r.Model = "opencode-go/kimi-k3"; return r }())
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "reconnect OpenCode") {
		t.Errorf("a Go model on a dead sign-in = %d: %s", resp.StatusCode, body)
	}
}

// Disconnecting keeps the generation, so a later sign-in is newer than
// what a live session holds; and an approval nobody waited for is
// collected when the connection is next read.
//
// @decision(DL-015)
func TestReconnectingMovesTheGenerationOn(t *testing.T) {
	f := newConsoleFixture(t, true)
	f.connect()
	if resp, _ := f.do(http.MethodDelete, "/api/v1/me/opencode", nil); resp.StatusCode >= 300 {
		t.Fatal(resp.StatusCode)
	}
	_, body := f.do(http.MethodPost, "/api/v1/me/opencode/connect", nil)
	var a OpenCodeAttemptView
	_ = json.Unmarshal(body, &a)
	f.console.Approve(a.UserCode)
	f.clock.advance(5 * time.Second)
	_, body = f.do(http.MethodGet, "/api/v1/me/opencode", nil)
	var v OpenCodeConnectionView
	_ = json.Unmarshal(body, &v)
	if !v.Connected {
		t.Fatalf("an approval was not collected by reading the connection: %+v", v)
	}
	if g := f.grant(); g.Generation != 2 {
		t.Errorf("generation %d after a reconnect, want 2", g.Generation)
	}
}

// Concurrent launches that need a refresh refresh once: the second waits
// for the lock, reads the new grant and uses it.
//
// @decision(DL-015)
func TestConcurrentLaunchesRefreshOnce(t *testing.T) {
	f := newConsoleFixture(t, true)
	f.console.ExpiresIn = 3600 // an hour: too short for a 1 h session plus the margin
	f.connect()
	f.console.ExpiresIn = 30 * 24 * 3600
	first := f.grant()

	var wg sync.WaitGroup
	tokens := make([]string, 6)
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, gen, note, err := f.srv.svc.freshConsoleToken(context.Background(), auth.LocalID, auth.LocalID, time.Hour)
			if err != nil || note != "" || gen != 2 {
				t.Errorf("launch %d: gen %d, note %q, err %v", i, gen, note, err)
			}
			tokens[i] = tok
		}()
	}
	wg.Wait()
	if f.console.Refreshes != 1 {
		t.Errorf("%d refreshes; concurrent launches must refresh once", f.console.Refreshes)
	}
	now := f.grant()
	for i, tok := range tokens {
		if tok != now.Access {
			t.Errorf("launch %d got a token that is not the stored one", i)
		}
	}
	if f.console.Valid(first.Access) || !f.console.Valid(now.Access) {
		t.Error("the refresh did not replace the token")
	}
	// A token that outlasts the session is used as it is.
	if _, _, _, err := f.srv.svc.freshConsoleToken(context.Background(), auth.LocalID, auth.LocalID, time.Hour); err != nil || f.console.Refreshes != 1 {
		t.Errorf("a fresh grant was refreshed again: %d, %v", f.console.Refreshes, err)
	}
}

// refusingConsole refuses every refresh as the console does a dead grant.
type refusingConsole struct{ ConsoleClient }

func (refusingConsole) Refresh(context.Context, string, time.Time) (console.Grant, error) {
	return console.Grant{}, console.ErrInvalidGrant
}

// A grant the console refuses is marked for reconnecting, launches go
// ahead without it, and the providers are no longer offered.
//
// @decision(DL-015)
func TestARefusedRefreshAsksForAReconnect(t *testing.T) {
	f := newConsoleFixture(t, true)
	f.console.ExpiresIn = 3600
	f.connect()
	f.srv.svc.deps.Console = refusingConsole{f.srv.svc.deps.Console}

	tok, _, note, err := f.srv.svc.freshConsoleToken(context.Background(), auth.LocalID, auth.LocalID, time.Hour)
	if err != nil || tok != "" || !strings.Contains(note, "reconnect") {
		t.Fatalf("token %q, note %q, err %v", tok, note, err)
	}
	if g := f.grant(); !g.NeedsReconnect {
		t.Error("the grant was not marked")
	}
	_, body := f.do(http.MethodGet, "/api/v1/me/opencode", nil)
	var v OpenCodeConnectionView
	_ = json.Unmarshal(body, &v)
	if v.Connected || !v.NeedsReconnect {
		t.Errorf("connection = %+v", v)
	}
	if h, _ := f.srv.svc.heldProviders(context.Background(), auth.LocalID, auth.LocalID); h[prep.OpenCodeKeyEnv] {
		t.Error("a dead grant is still offered as a key")
	}
}

// failingGrantWrites fails a grant write made under the lock, as a store
// that breaks between the refresh and its write would.
type failingGrantWrites struct{ *MemoryConsoleStore }

func (s failingGrantWrites) Locked(ctx context.Context, orgID, userID string, fn func(ConsoleStore) error) error {
	return s.MemoryConsoleStore.Locked(ctx, orgID, userID, func(cs ConsoleStore) error {
		return fn(failingPut{cs})
	})
}

type failingPut struct{ ConsoleStore }

func (failingPut) Put(context.Context, string, string, string, string) error {
	return errors.New("disk full")
}

// A refresh whose new grant cannot be stored fails the launch: the old
// tokens are dead, so going ahead would hand out a grant nobody holds.
//
// @decision(DL-015)
func TestARefreshThatCannotBeStoredFailsTheLaunch(t *testing.T) {
	mem := &MemoryConsoleStore{}
	f := newConsoleFixture(t, true)
	f.console.ExpiresIn = 3600
	f.grants = mem
	f.srv.svc.deps.ConsoleGrants = mem
	f.connect()
	f.srv.svc.deps.ConsoleGrants = failingGrantWrites{mem}
	if _, _, _, err := f.srv.svc.freshConsoleToken(context.Background(), auth.LocalID, auth.LocalID, time.Hour); err == nil ||
		!strings.Contains(err.Error(), "connect OpenCode again") {
		t.Fatalf("err = %v", err)
	}
}

// After a refresh the new token is written into every live sidecar that
// holds an older one, on stdin, and the session's generation moves on. A
// second pass writes nothing.
//
// @decision(DL-015)
func TestARefreshedTokenIsWrittenIntoLiveSidecars(t *testing.T) {
	f := newConsoleFixture(t, true)
	f.console.ExpiresIn = 3 * 3600
	f.connect()
	f.console.ExpiresIn = 30 * 24 * 3600
	id := f.create(repoReq("alpha"))
	f.events(id)
	rf := &renewFixture{apiFixture: f.apiFixture, id: id}
	h := rf.handle()

	// A later, longer session needs the grant refreshed.
	if _, gen, _, err := f.srv.svc.freshConsoleToken(context.Background(), auth.LocalID, auth.LocalID, 4*time.Hour); err != nil || gen != 2 {
		t.Fatalf("refresh: gen %d, %v", gen, err)
	}
	f.srv.svc.pushes.Wait()
	f.srv.svc.pushOpenCodeGrants(context.Background(), nil)

	var writes []sandboxExec
	for _, e := range h.SidecarExecs() {
		if slices.Equal(e.Command, credproxy.SetKeyCommand(prep.OpenCodeKeyEnv)) {
			writes = append(writes, sandboxExec{e.Sidecar, string(e.Stdin)})
		}
	}
	now := f.grant()
	if len(writes) != 1 || writes[0].sidecar != credproxy.SidecarName || writes[0].stdin != now.Access+"\n" {
		t.Fatalf("writes = %+v", writes)
	}
	if row := rf.row(); row.OpenCodeGrant != 2 {
		t.Errorf("the session holds generation %d", row.OpenCodeGrant)
	}
}

type sandboxExec struct{ sidecar, stdin string }

// A push forgets the renewal state of sessions that have ended, so a
// replica that never runs RenewGitTokens does not keep it forever.
//
// @decision(DL-015) @decision(DL-021)
func TestAPushForgetsTheStateOfEndedSessions(t *testing.T) {
	f := newConsoleFixture(t, true)
	f.connect()
	id := f.create(repoReq("alpha"))
	f.events(id)
	svc := f.srv.svc
	revoked := 0
	svc.mu.Lock()
	svc.renewalFor(id)
	svc.renewalFor("ses_ended").held["github"] = []func(context.Context) error{
		func(context.Context) error { revoked++; return nil },
	}
	svc.mu.Unlock()

	svc.pushOpenCodeGrants(context.Background(), func(store.Session) bool { return false })

	svc.mu.Lock()
	_, ended := svc.renewals["ses_ended"]
	_, live := svc.renewals[id]
	svc.mu.Unlock()
	if ended || !live {
		t.Errorf("after a push: ended session kept %v, live session kept %v", ended, live)
	}
	if revoked != 1 {
		t.Errorf("the ended session's held token was revoked %d times", revoked)
	}
}

// The sealed store keeps console state out of the credential listing and
// out of the credential API's reach, and its lock is a transaction.
//
// @decision(DL-015)
func TestTheSealedConsoleStoreIsHiddenFromTheCredentialAPI(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, auth.KeyBytes))
	sealer, err := auth.NewSealer(map[int]string{1: key})
	if err != nil {
		t.Fatal(err)
	}
	var sealed SealedConsoleStore
	f := newConsoleFixture(t, false, func(d *Deps) {
		d.Credentials = &CredentialStore{Store: d.Store.Credentials(), Sealer: sealer}
		sealed = SealedConsoleStore{Store: d.Store, Sealer: sealer}
		d.ConsoleGrants = sealed
	})
	f.grants = sealed
	f.connect()
	_, body := f.do(http.MethodGet, "/api/v1/me/credentials", nil)
	if strings.Contains(string(body), "opencode") {
		t.Errorf("the credential listing shows console state: %s", body)
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/me/credentials/opencode:grant", nil); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("the credential API deleted console state: %d", resp.StatusCode)
	}
	err = sealed.Locked(context.Background(), auth.LocalID, auth.LocalID, func(cs ConsoleStore) error {
		return cs.Locked(context.Background(), auth.LocalID, auth.LocalID, func(ConsoleStore) error { return nil })
	})
	if err == nil {
		t.Error("a nested lock was allowed")
	}
}
