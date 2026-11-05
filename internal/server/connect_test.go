package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// serverMints are the tokens minted by the connect endpoint. The
// launcher's own proxy mints one during a launch (DL-030), which is not
// part of what these tests measure.
func serverMints(f *apiFixture) []sandbox.ConnectRequest {
	var out []sandbox.ConnectRequest
	for _, r := range f.provider.ConnectTokens {
		if r.Metadata != "" {
			out = append(out, r)
		}
	}
	return out
}

func connectBody(t *testing.T, body []byte) ConnectionView {
	t.Helper()
	var v ConnectionView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return v
}

// TestConnectMintsPerRequestAndRecordsIt: every call is a fresh token,
// carries the principal as verified metadata, and leaves an audit row.
//
// @decision(DL-030)
func TestConnectMintsPerRequestAndRecordsIt(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)

	res, body := f.do(http.MethodPost, "/api/v1/sessions/alpha/connect", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("connect: %d %s", res.StatusCode, body)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: the response carries credentials", cc)
	}
	first := connectBody(t, body)
	if first.URL == "" || first.Token == "" || first.Password == "" {
		t.Fatalf("incomplete connection: %+v", first)
	}
	if first.User != opencode.BasicAuthUser {
		t.Errorf("user = %q", first.User)
	}
	if first.IdleTimeoutSecs != 600 {
		t.Errorf("idle_timeout_secs = %d, want 600", first.IdleTimeoutSecs)
	}

	_, body = f.do(http.MethodPost, "/api/v1/sessions/alpha/connect", nil)
	second := connectBody(t, body)
	if second.Token == first.Token {
		t.Error("the same token was handed out twice")
	}

	reqs := serverMints(f)
	if len(reqs) != 2 {
		t.Fatalf("%d tokens minted", len(reqs))
	}
	var meta struct {
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(reqs[0].Metadata), &meta); err != nil {
		t.Fatalf("metadata %q: %v", reqs[0].Metadata, err)
	}
	if meta.UserID != auth.LocalID || meta.SessionID != id {
		t.Errorf("metadata = %+v", meta)
	}
	if len(reqs[0].Metadata) > sandbox.MaxConnectMetadata {
		t.Errorf("metadata is %d bytes", len(reqs[0].Metadata))
	}

	mints, err := f.store.Mints().ListBySession(context.Background(), auth.LocalID, id)
	if err != nil || len(mints) != 2 {
		t.Fatalf("mints = %d, %v", len(mints), err)
	}
	if mints[0].UserID != auth.LocalID || mints[0].Port != 4096 || mints[0].ExpiresAt.IsZero() {
		t.Errorf("mint = %+v", mints[0])
	}
	for _, m := range mints {
		if strings.Contains(fmt.Sprint(m), first.Token) || strings.Contains(fmt.Sprint(m), first.Password) {
			t.Error("a credential was stored in the audit row")
		}
	}
}

// TestConnectReadsTheTokenExpiry: a real connect token is a JWT whose
// exp claim is what the audit row records.
//
// @decision(DL-030)
func TestConnectReadsTheTokenExpiry(t *testing.T) {
	exp := time.Now().Add(90 * time.Minute).Truncate(time.Second).UTC()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"sub":"sb-1"}`, exp.Unix())))
	f := newAPIFixture(t)
	f.provider.ConnectTokenValue = "header." + payload + ".sig"
	id := f.create(repoReq("alpha"))
	f.events(id)

	_, body := f.do(http.MethodPost, "/api/v1/sessions/alpha/connect", nil)
	got := connectBody(t, body)
	if !got.ExpiresAt.Equal(exp) {
		t.Errorf("expires_at = %s, want %s", got.ExpiresAt, exp)
	}
	mints, _ := f.store.Mints().ListBySession(context.Background(), auth.LocalID, id)
	if len(mints) != 1 || !mints[0].ExpiresAt.Equal(exp) {
		t.Errorf("recorded expiry = %v", mints)
	}
}

// TestTokenExpiryFallsBackWhenUnreadable: the fake's opaque token has no
// claims, so the observed 24 h is assumed and warned about.
//
// @decision(DL-030)
func TestTokenExpiryFallsBackWhenUnreadable(t *testing.T) {
	for _, tok := range []string{"opaque", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":0}`)) + ".c"} {
		if got := tokenExpiry(tok); !got.IsZero() {
			t.Errorf("tokenExpiry(%q) = %s, want zero", tok, got)
		}
	}
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)
	before := time.Now()
	_, body := f.do(http.MethodPost, "/api/v1/sessions/alpha/connect", nil)
	got := connectBody(t, body)
	if got.ExpiresAt.Before(before.Add(AssumedTokenTTL-time.Minute)) || got.ExpiresAt.After(time.Now().Add(AssumedTokenTTL+time.Minute)) {
		t.Errorf("expires_at = %s, want about %s from now", got.ExpiresAt, AssumedTokenTTL)
	}
}

// TestConnectIsRateLimitedPerSession: a compromised sandbox provoking
// 401s must not be able to mint unrevocable tokens without bound.
//
// @decision(DL-030)
func TestConnectIsRateLimitedPerSession(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)
	for i := range MintsPerMinute {
		if res, body := f.do(http.MethodPost, "/api/v1/sessions/"+id+"/connect", nil); res.StatusCode != http.StatusOK {
			t.Fatalf("mint %d: %d %s", i, res.StatusCode, body)
		}
	}
	res, body := f.do(http.MethodPost, "/api/v1/sessions/"+id+"/connect", nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("mint %d: %d %s", MintsPerMinute+1, res.StatusCode, body)
	}
	if n := len(serverMints(f)); n != MintsPerMinute {
		t.Errorf("%d tokens minted past the limit", n)
	}
	// The window rolls.
	f.srv.svc.deps.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if res, _ := f.do(http.MethodPost, "/api/v1/sessions/"+id+"/connect", nil); res.StatusCode != http.StatusOK {
		t.Errorf("after the window: %d", res.StatusCode)
	}
}

// TestConnectRefusesSessionsThatAreNotRunning.
//
// @decision(DL-030)
func TestConnectRefusesSessionsThatAreNotRunning(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)
	if res, _ := f.do(http.MethodDelete, "/api/v1/sessions/"+id, nil); res.StatusCode != http.StatusNoContent {
		t.Fatal("delete failed")
	}
	res, body := f.do(http.MethodPost, "/api/v1/sessions/"+id+"/connect", nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stopped session: %d %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), ProblemTypeSessionStopped) {
		t.Errorf("problem type missing: %s", body)
	}
	if res, _ := f.do(http.MethodPost, "/api/v1/sessions/nope/connect", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session: %d", res.StatusCode)
	}
}

// TestConnectIsScopedToTheOwner: another user must not obtain
// credentials for a session they cannot see.
//
// @decision(DL-030) @decision(DL-006)
func TestConnectIsScopedToTheOwner(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)
	f.auth.inner = auth.StaticAuthenticator{Principal: auth.Principal{UserID: "u2", OrgID: auth.LocalID}}
	if res, body := f.do(http.MethodPost, "/api/v1/sessions/"+id+"/connect", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("another user connected: %d %s", res.StatusCode, body)
	}
	if len(serverMints(f)) != 0 {
		t.Error("a token was minted for another user")
	}
}

// TestConnectRecoversThePasswordFromTheSandbox: a session this process
// did not launch has no password in memory, so it is read back from the
// sandbox once and cached.
//
// @decision(DL-030) @decision(DL-025)
func TestConnectRecoversThePasswordFromTheSandbox(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	h, err := f.provider.Create(ctx, sandbox.Spec{Name: "old", Image: "i", Command: []string{"x"},
		CPU: 0.5, MemoryMiB: 1024, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "old", User: "u", CreatedAt: time.Now()}), Egress: sandbox.EgressOpen()})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(ctx)
	f.provider.ExecResults = map[string]sandbox.ExecResult{
		"printenv " + opencode.PasswordEnvVar: {Stdout: "recovered-password\n"},
	}
	if err := f.store.Sessions().Create(ctx, store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID,
		OwnerID: auth.LocalID, Name: "old", State: store.StateRunning, SandboxID: info.ID}); err != nil {
		t.Fatal(err)
	}
	_, body := f.do(http.MethodPost, "/api/v1/sessions/old/connect", nil)
	if got := connectBody(t, body); got.Password != "recovered-password" {
		t.Fatalf("password = %q", got.Password)
	}
	execs := len(f.provider.Handles()[info.ID].ExecLog())
	if _, body := f.do(http.MethodPost, "/api/v1/sessions/old/connect", nil); connectBody(t, body).Password != "recovered-password" {
		t.Error("the second connect lost the password")
	}
	if again := len(f.provider.Handles()[info.ID].ExecLog()); again != execs {
		t.Errorf("the password was read from the sandbox again: %d then %d execs", execs, again)
	}
}

// TestConnectReportsASandboxWithNoPassword.
//
// @decision(DL-030)
func TestConnectReportsASandboxWithNoPassword(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	h, err := f.provider.Create(ctx, sandbox.Spec{Name: "old", Image: "i", Command: []string{"x"},
		CPU: 0.5, MemoryMiB: 1024, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "old", User: "u", CreatedAt: time.Now()}), Egress: sandbox.EgressOpen()})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(ctx)
	f.provider.ExecResults = map[string]sandbox.ExecResult{
		"printenv " + opencode.PasswordEnvVar: {ExitCode: 1},
	}
	if err := f.store.Sessions().Create(ctx, store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID,
		OwnerID: auth.LocalID, Name: "old", State: store.StateRunning, SandboxID: info.ID}); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.do(http.MethodPost, "/api/v1/sessions/old/connect", nil); res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d", res.StatusCode)
	}
	if len(serverMints(f)) != 0 {
		t.Error("a token was minted for a session with no password")
	}
}

// TestConnectSurfacesAProviderFailure.
//
// @decision(DL-030)
func TestConnectSurfacesAProviderFailure(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)
	f.provider.ConnectErr = errors.New("modal: edge unavailable")
	res, body := f.do(http.MethodPost, "/api/v1/sessions/"+id+"/connect", nil)
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	// The reservation row stays: it is what makes the rate limit atomic,
	// and it expires with the window. What it must not carry is an
	// expiry, since no token was handed out.
	mints, _ := f.store.Mints().ListBySession(context.Background(), auth.LocalID, id)
	if len(mints) != 1 {
		t.Fatalf("%d rows for a failed mint, want the reservation", len(mints))
	}
	if !mints[0].ExpiresAt.IsZero() {
		t.Errorf("a failed mint recorded an expiry: %s", mints[0].ExpiresAt)
	}
}

// TestConnectMetadataStaysUnderTheLimit.
//
// @decision(DL-030)
func TestConnectMetadataStaysUnderTheLimit(t *testing.T) {
	meta, err := connectMetadata(strings.Repeat("u", 26), strings.Repeat("s", 26))
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) > sandbox.MaxConnectMetadata {
		t.Errorf("metadata is %d bytes", len(meta))
	}
	if _, err := connectMetadata(strings.Repeat("u", sandbox.MaxConnectMetadata), "s"); err == nil {
		t.Error("oversized metadata was accepted")
	}
}

// TestConnectRateLimitHoldsUnderConcurrency: a check-then-act limit
// overshoots, and these tokens cannot be revoked.
//
// @decision(DL-030)
func TestConnectRateLimitHoldsUnderConcurrency(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)

	const callers = 40
	var wg sync.WaitGroup
	var ok, limited atomic.Int32
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := f.do(http.MethodPost, "/api/v1/sessions/"+id+"/connect", nil)
			switch res.StatusCode {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusTooManyRequests:
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := int(ok.Load()); got > MintsPerMinute {
		t.Errorf("%d concurrent callers got %d tokens, over the limit of %d", callers, got, MintsPerMinute)
	}
	if n := len(serverMints(f)); n > MintsPerMinute {
		t.Errorf("%d tokens minted, over the limit of %d", n, MintsPerMinute)
	}
	if ok.Load()+limited.Load() != callers {
		t.Errorf("%d ok + %d limited != %d callers", ok.Load(), limited.Load(), callers)
	}
}

// TestConnectClampsAnAbsurdExpiry: the audit row is what makes an
// unrevocable token acceptable, so it must stay meaningful.
//
// @decision(DL-030)
func TestConnectClampsAnAbsurdExpiry(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":99999999999999999}`))
	f := newAPIFixture(t)
	f.provider.ConnectTokenValue = "h." + payload + ".s"
	id := f.create(repoReq("alpha"))
	f.events(id)
	_, body := f.do(http.MethodPost, "/api/v1/sessions/"+id+"/connect", nil)
	got := connectBody(t, body)
	if got.ExpiresAt.After(time.Now().Add(AssumedTokenTTL + time.Minute)) {
		t.Errorf("expires_at = %s, want it clamped to about %s", got.ExpiresAt, AssumedTokenTTL)
	}
	mints, _ := f.store.Mints().ListBySession(context.Background(), auth.LocalID, id)
	if len(mints) != 1 || mints[0].ExpiresAt.After(time.Now().Add(AssumedTokenTTL+time.Minute)) {
		t.Errorf("recorded expiry = %v", mints)
	}
}

// TestReconcilerForgetsWhatAStoppedSessionCached: a sandbox that hit its
// idle timeout stops through the reconciler, not through Delete, and its
// recovered password must not linger.
//
// @decision(DL-030)
func TestReconcilerForgetsWhatAStoppedSessionCached(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	h, err := f.provider.Create(ctx, sandbox.Spec{Name: "old", Image: "i", Command: []string{"x"},
		CPU: 0.5, MemoryMiB: 1024, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "old", User: "u", CreatedAt: time.Now()}), Egress: sandbox.EgressOpen()})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(ctx)
	f.provider.ExecResults = map[string]sandbox.ExecResult{
		"printenv " + opencode.PasswordEnvVar: {Stdout: "recovered\n"},
	}
	sess := store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID,
		Name: "old", State: store.StateRunning, SandboxID: info.ID}
	if err := f.store.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.do(http.MethodPost, "/api/v1/sessions/old/connect", nil); res.StatusCode != http.StatusOK {
		t.Fatal("connect failed")
	}
	if !f.srv.Service().hasCachedPassword(sess.ID) {
		t.Fatal("the password was not cached, so this proves nothing")
	}

	if err := h.Terminate(ctx); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Store: f.store, Provider: f.provider, DefaultOrg: auth.LocalID,
		DefaultOwner: auth.LocalID, Forget: f.srv.Service().ForgetSession}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if f.srv.Service().hasCachedPassword(sess.ID) {
		t.Error("a stopped session's password is still in memory")
	}
}

// TestRequestedEgressReachesTheSandbox is the assertion DL-031 exists
// for: a caller asking for a narrowed preset must not get an open
// sandbox because a value was dropped between the request and the spec.
//
// @decision(DL-031)
func TestRequestedEgressReachesTheSandbox(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.svc.deps.Defaults.EgressExtraDomains = []string{"registry.internal.example"}
	req := repoReq("alpha")
	req.Egress = sandbox.PresetAgent
	id := f.create(req)
	f.events(id)

	if len(f.provider.Created) != 1 {
		t.Fatalf("%d sandboxes created", len(f.provider.Created))
	}
	got := f.provider.Created[0].Egress
	if got.Open() {
		t.Fatalf("a request for the %s preset produced an open sandbox: %+v", sandbox.PresetAgent, got)
	}
	for _, want := range sandbox.EgressAgent().Domains {
		if !slices.Contains(got.Domains, want) {
			t.Errorf("the sandbox cannot reach %s", want)
		}
	}
	if !slices.Contains(got.Domains, "registry.internal.example") {
		t.Errorf("the server's extra domains were dropped: %v", got.Domains)
	}
	// The session's own forge is reachable under the preset, which keeps
	// its name. @decision(DL-031)
	// The extra domains make it custom; the forge alone would not.
	if !slices.Contains(got.Domains, "git.example.com") || got.Preset != sandbox.PresetCustom {
		t.Errorf("the session's forge is not allowed: %+v", got)
	}
	for _, reg := range sandbox.RegistryDomains {
		if slices.Contains(got.Domains, reg) {
			t.Errorf("the agent preset reached the sandbox carrying the registry %s", reg)
		}
	}
}

// TestSandboxConfigReachesTheSandbox: every DL-029 key must survive the
// trip from request to spec, or a user configures something that is
// silently ignored.
//
// @decision(DL-029)
func TestSandboxConfigReachesTheSandbox(t *testing.T) {
	f := newAPIFixture(t)
	req := repoReq("alpha")
	req.Sandbox = SandboxConfigView{
		CPU: 1, CPULimit: 2, MemoryMiB: 2048, MemoryLimitMiB: 4096,
		TimeoutSecs: 3600, IdleSecs: 600,
		Regions: []string{"us-east-1"}, Cloud: "aws", Workdir: "/workspace",
		InboundCIDRs: []string{"203.0.113.0/24"},
	}
	id := f.create(req)
	f.events(id)

	spec := f.provider.Created[0]
	checks := map[string][2]any{
		"cpu":           {spec.CPU, 1.0},
		"cpu limit":     {spec.CPULimit, 2.0},
		"memory":        {spec.MemoryMiB, 2048},
		"memory limit":  {spec.MemoryLimitMiB, 4096},
		"timeout":       {spec.Timeout, time.Hour},
		"idle timeout":  {spec.IdleTimeout, 10 * time.Minute},
		"cloud":         {spec.Cloud, "aws"},
		"workdir":       {spec.Workdir, "/workspace"},
		"regions":       {strings.Join(spec.Regions, ","), "us-east-1"},
		"inbound cidrs": {strings.Join(spec.InboundCIDRs, ","), "203.0.113.0/24"},
	}
	for name, pair := range checks {
		if pair[0] != pair[1] {
			t.Errorf("%s reached the sandbox as %v, want %v", name, pair[0], pair[1])
		}
	}
}

// TestCreateRefusesBadSandboxConfigSynchronously: a bad value must be a
// 422 to the caller, not a launch that fails minutes later.
//
// @decision(DL-029) @decision(DL-005)
func TestCreateRefusesBadSandboxConfigSynchronously(t *testing.T) {
	cases := map[string]func(*SessionRequest){
		"unknown egress preset": func(r *SessionRequest) { r.Egress = "wide-open" },
		"relative workdir":      func(r *SessionRequest) { r.Sandbox.Workdir = "workspace" },
		"unknown cloud":         func(r *SessionRequest) { r.Sandbox.Cloud = "azure" },
		"cpu limit below cpu":   func(r *SessionRequest) { r.Sandbox.CPU, r.Sandbox.CPULimit = 2, 1 },
		"memory limit below":    func(r *SessionRequest) { r.Sandbox.MemoryMiB, r.Sandbox.MemoryLimitMiB = 2048, 1024 },
		"idle below the floor":  func(r *SessionRequest) { r.Sandbox.IdleSecs = 5 },
		"bad inbound cidr":      func(r *SessionRequest) { r.Sandbox.InboundCIDRs = []string{"203.0.113.1"} },
		"timeout over the cap":  func(r *SessionRequest) { r.Sandbox.TimeoutSecs = 100 * 3600 },
		"idle over timeout":     func(r *SessionRequest) { r.Sandbox.TimeoutSecs, r.Sandbox.IdleSecs = 60, 3600 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAPIFixture(t)
			req := repoReq("alpha")
			mutate(&req)
			res, body := f.do(http.MethodPost, "/api/v1/sessions", req)
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", res.StatusCode, body)
			}
			if len(f.provider.Created) != 0 {
				t.Error("a sandbox was created for a refused request")
			}
		})
	}
}

// lockSpy records, in order, the session locks and mint counts made
// through a store, and whether each ran inside Tx.
type lockSpy struct {
	store.Store
	inTx  bool
	calls *[]string
}

func (s lockSpy) Tx(ctx context.Context, fn func(store.Store) error) error {
	return s.Store.Tx(ctx, func(tx store.Store) error { return fn(lockSpy{Store: tx, inTx: true, calls: s.calls}) })
}
func (s lockSpy) Sessions() store.SessionStore { return spySessions{s.Store.Sessions(), s} }
func (s lockSpy) Mints() store.MintStore       { return spyMints{s.Store.Mints(), s} }
func (s lockSpy) note(call string) {
	if s.inTx {
		call += " in tx"
	}
	*s.calls = append(*s.calls, call)
}

type spySessions struct {
	store.SessionStore
	spy lockSpy
}

func (s spySessions) Lock(ctx context.Context, orgID, id string) error {
	s.spy.note("lock")
	return s.SessionStore.Lock(ctx, orgID, id)
}

type spyMints struct {
	store.MintStore
	spy lockSpy
}

func (s spyMints) CountSince(ctx context.Context, orgID, sessionID string, t time.Time) (int, error) {
	s.spy.note("count")
	return s.MintStore.CountSince(ctx, orgID, sessionID, t)
}

// TestConnectLocksTheSessionBeforeCounting: the mint limit is exact
// across replicas only because the count runs under the session's row
// lock, in the same transaction.
//
// @decision(DL-065)
func TestConnectLocksTheSessionBeforeCounting(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)
	var calls []string
	f.srv.svc.deps.Store = lockSpy{Store: f.srv.svc.deps.Store, calls: &calls}
	if res, body := f.do(http.MethodPost, "/api/v1/sessions/"+id+"/connect", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("connect: %d %s", res.StatusCode, body)
	}
	if got := strings.Join(calls, ", "); !strings.HasPrefix(got, "lock in tx, count in tx") {
		t.Errorf("calls = %q, want the lock then the count, both in one transaction", got)
	}
}
