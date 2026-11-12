package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// fakeForge answers with one fake platform.
type fakeForge struct {
	platform *git.Fake
	// second, when set, is a second platform every selection also names,
	// with the same repositories.
	second *git.Fake
	// other is a platform the caller holds a credential for that no
	// selection names.
	other    *git.Fake
	warnings []string
	err      error

	mu       sync.Mutex
	selected int
}

// selections is how many times Selections was called, for tests that
// assert which forge a launch used.
//
// @decision(DL-042)
func (f *fakeForge) selections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.selected
}

func (f *fakeForge) ListRepos(ctx context.Context) ([]git.PlatformRepo, []string, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	repos, err := f.platform.ListRepos(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]git.PlatformRepo, len(repos))
	for i, r := range repos {
		out[i] = git.PlatformRepo{Platform: f.platform, Repo: r}
	}
	return out, f.warnings, nil
}

func (f *fakeForge) Selections(_ context.Context, refs []store.RepoRef) ([]git.Selection, error) {
	f.mu.Lock()
	f.selected++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	sel := git.Selection{Platform: f.platform}
	for _, r := range refs {
		sel.Repos = append(sel.Repos, git.Repo{Owner: r.Owner, Name: r.Name, CloneURL: "https://git.test/" + r.Owner + "/" + r.Name + ".git"})
	}
	if f.second == nil {
		return []git.Selection{sel}, nil
	}
	return []git.Selection{sel, {Platform: f.second, Repos: sel.Repos}}, nil
}

// Platforms are every platform the forge knows.
func (f *fakeForge) Platforms(context.Context) ([]git.Platform, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := []git.Platform{f.platform}
	for _, p := range []*git.Fake{f.second, f.other} {
		if p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

type stubHealth struct{}

func (stubHealth) WaitHealthy(context.Context, time.Duration) (opencode.Health, error) {
	return opencode.Health{Healthy: true, Version: "0.0.0-test"}, nil
}

type apiFixture struct {
	t         *testing.T
	srv       *Server
	http      *httptest.Server
	provider  *sandbox.Fake
	store     *store.Memory
	forge     *fakeForge
	auth      *countingAuth
	launched  atomic.Int32
	launchErr error
	token     string
}

type countingAuth struct {
	inner auth.Authenticator
	calls atomic.Int32
}

func (c *countingAuth) Authenticate(r *http.Request) (auth.Principal, error) {
	c.calls.Add(1)
	return c.inner.Authenticate(r)
}

// testStorePoll is the cross-replica poll interval every fixture runs
// with, milliseconds rather than the production two seconds so a test
// is not measuring the clock. Tests that need to act "while the stream
// is open" scale their own wait off this.
//
// @decision(DL-003)
const testStorePoll = 5 * time.Millisecond

// newFixture serves the real API over a real listener, with the real
// session.Launcher driving the fake provider and a real loopback proxy.
func newAPIFixture(t *testing.T) *apiFixture { return newAPIFixtureWith(t, nil) }

// newAPIFixtureWith builds the fixture and lets a test adjust Deps
// before the server is constructed.
func newAPIFixtureWith(t *testing.T, adjust func(*Deps)) *apiFixture {
	t.Helper()
	f := &apiFixture{t: t, provider: sandbox.NewFake(), store: store.NewMemory(), token: "tok-" + t.Name()}
	f.forge = &fakeForge{platform: git.NewFake()}
	f.forge.platform.Repos = []git.Repo{{Owner: "acme", Name: "app", CloneURL: "https://git.test/acme/app.git", Private: true}}
	local, err := auth.NewLocal(f.token)
	if err != nil {
		t.Fatal(err)
	}
	f.auth = &countingAuth{inner: local}

	launcher := &session.Launcher{
		Sandboxes:     f.provider,
		NewClient:     func(string, string) session.HealthChecker { return stubHealth{} },
		HealthTimeout: 5 * time.Second,
	}
	realLaunch := NewLaunch(launcher)
	deps := Deps{
		Store: f.store, Provider: f.provider, Forge: f.forge, Auth: f.auth,
		Launch: func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error) {
			f.launched.Add(1)
			if f.launchErr != nil {
				steps.Start(session.StepTokens)
				return nil, f.launchErr
			}
			return realLaunch(ctx, opts, steps)
		},
		Defaults: Defaults{Image: "img", Port: 4096, Sandbox: store.SandboxConfig{CPU: 0.5, MemoryMiB: 2048, Timeout: time.Hour, IdleTimeout: 10 * time.Minute}, Egress: "open"},
		User:     "tester",
		Warn:     func(s string) { t.Log(s) },
		// See testStorePoll.
		//
		// @decision(DL-003)
		StorePoll: testStorePoll,
	}
	if adjust != nil {
		adjust(&deps)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.srv, err = New(deps, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	f.http = httptest.NewUnstartedServer(f.srv.Handler())
	f.http.Listener = ln
	f.http.Start()
	t.Cleanup(func() { f.http.Close(); f.srv.Close() })
	return f
}

// result is what a test needs from a finished response; the body is
// already read and closed.
type result struct {
	StatusCode int
	Header     http.Header
}

func (f *apiFixture) do(method, path string, body any, mod ...func(*http.Request)) (result, []byte) {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.http.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+f.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, m := range mod {
		m(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return result{StatusCode: resp.StatusCode, Header: resp.Header}, b
}

func (f *apiFixture) create(req SessionRequest) string {
	f.t.Helper()
	resp, body := f.do(http.MethodPost, "/api/v1/sessions", req)
	if resp.StatusCode != http.StatusAccepted {
		f.t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var out struct{ ID string }
	_ = json.Unmarshal(body, &out)
	return out.ID
}

type event struct {
	Name string
	Data json.RawMessage
}

// events reads the SSE stream to its end.
func (f *apiFixture) events(id string) []event {
	f.t.Helper()
	res, raw := f.do(http.MethodGet, "/api/v1/sessions/"+id+"/events", nil)
	if res.StatusCode != http.StatusOK {
		f.t.Fatalf("events: %d %s", res.StatusCode, raw)
	}
	return parseEvents(raw)
}

// parseEvents reads a server-sent event stream's events.
func parseEvents(raw []byte) []event {
	var out []event
	var cur event
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			cur.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.Data = json.RawMessage(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if cur.Name != "" || cur.Data != nil {
				out = append(out, cur)
				cur = event{}
			}
		}
	}
	return out
}

func repoReq(name string) SessionRequest {
	return SessionRequest{Name: name, Repos: []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}}}
}

// TestCreateStreamsStepsAndEndsReady is the happy path end to end:
// 202, the three launch steps, ready, and a running session with a
// sandbox behind it.
//
// @decision(DL-005)
func TestCreateStreamsStepsAndEndsReady(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	if !store.ValidID(id) {
		t.Fatalf("id %q is not a ULID", id)
	}
	evs := f.events(id)
	var names []string
	for _, e := range evs {
		names = append(names, e.Name)
	}
	// The notes are the missing-provider-key warning, since the fixture
	// sets no key and the server is the side that knows, and after the
	// launch the stored-token line (DL-043, DL-034), which needs to know
	// whether the proxy took the token.
	want := "note step step step step step step note ready"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
	var ready ReadyEvent
	_ = json.Unmarshal(evs[len(evs)-1].Data, &ready)
	if ready.Session.State != store.StateRunning || ready.Session.SandboxID == "" || len(ready.Session.Clones) != 1 {
		t.Errorf("ready = %+v", ready.Session)
	}

	resp, body := f.do(http.MethodGet, "/api/v1/sessions/alpha", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), ready.Session.ID) {
		t.Errorf("get by name: %d %s", resp.StatusCode, body)
	}
	resp, body = f.do(http.MethodGet, "/api/v1/sessions", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"alpha"`) {
		t.Errorf("list: %d %s", resp.StatusCode, body)
	}
	if f.provider.Running() != 1 {
		t.Errorf("%d sandboxes running", f.provider.Running())
	}
	if strings.Contains(string(body), "password") {
		t.Error("a password field is in the session listing")
	}
	// Replaying the finished stream gives the same events again.
	if again := f.events(id); len(again) != len(evs) {
		t.Errorf("replay gave %d events, want %d", len(again), len(evs))
	}
}

// TestCreateRejectsABadRequest.
//
// @decision(DL-005)
func TestCreateRejectsABadRequest(t *testing.T) {
	f := newAPIFixture(t)
	resp, body := f.do(http.MethodPost, "/api/v1/sessions", SessionRequest{Name: "x"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("no repos: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q", ct)
	}
	resp, body = f.do(http.MethodPost, "/api/v1/sessions", map[string]any{"repos": []map[string]string{{"platform": "fake", "owner": "a", "name": "b"}}, "name": "has space"})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "spaces") {
		t.Errorf("bad name: %d %s", resp.StatusCode, body)
	}
	if f.launched.Load() != 0 {
		t.Error("a launch ran for an invalid request")
	}
}

// TestCreateRefusesALiveName.
//
// @decision(DL-005)
func TestCreateRefusesALiveName(t *testing.T) {
	f := newAPIFixture(t)
	f.create(repoReq("alpha"))
	resp, body := f.do(http.MethodPost, "/api/v1/sessions", repoReq("alpha"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

// TestFailedLaunchEndsWithAnErrorEvent: the failure reaches the stream,
// the record, and leaves no sandbox.
//
// @decision(DL-005)
func TestFailedLaunchEndsWithAnErrorEvent(t *testing.T) {
	f := newAPIFixture(t)
	f.launchErr = errors.New("modal said no")
	id := f.create(repoReq("alpha"))
	evs := f.events(id)
	last := evs[len(evs)-1]
	if last.Name != "error" || !strings.Contains(string(last.Data), "modal said no") {
		t.Fatalf("last event = %s %s", last.Name, last.Data)
	}
	if evs[len(evs)-2].Name != "step" || !strings.Contains(string(evs[len(evs)-2].Data), "failed") {
		t.Errorf("the running step was not marked failed: %s", evs[len(evs)-2].Data)
	}
	_, body := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	if !strings.Contains(string(body), `"state":"failed"`) || !strings.Contains(string(body), "modal said no") {
		t.Errorf("record: %s", body)
	}
	if f.provider.Running() != 0 {
		t.Error("a failed launch left a sandbox")
	}
}

// TestDeleteTerminatesAndIsIdempotent.
//
// @decision(DL-005)
func TestDeleteTerminatesAndIsIdempotent(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)
	resp, body := f.do(http.MethodDelete, "/api/v1/sessions/alpha", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, body)
	}
	if f.provider.Running() != 0 {
		t.Error("the sandbox is still running")
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/sessions/"+id, nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("second delete: %d", resp.StatusCode)
	}
	_, body = f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	if !strings.Contains(string(body), `"state":"stopped"`) || !strings.Contains(string(body), "stopped_at") {
		t.Errorf("after delete: %s", body)
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/sessions/nope", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown: %d", resp.StatusCode)
	}
}

// TestDeleteOfAnAdoptedSessionTerminatesThroughTheProvider: a session
// this process did not launch has no live handle.
//
// @decision(DL-005)
func TestDeleteOfAnAdoptedSessionTerminatesThroughTheProvider(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	h, err := f.provider.Create(ctx, sandbox.Spec{Name: "old", Image: "i", Command: []string{"x"}, CPU: 0.5, MemoryMiB: 1024, Timeout: time.Hour, IdleTimeout: time.Minute, Tags: sandbox.BuildTags(sandbox.TagSet{Name: "old", User: "u", CreatedAt: time.Now()}), Egress: sandbox.EgressOpen()})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(ctx)
	adopted := store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID, Name: "old", State: store.StateRunning, SandboxID: info.ID}
	if err := f.store.Sessions().Create(ctx, adopted); err != nil {
		t.Fatal(err)
	}
	if resp, body := f.do(http.MethodDelete, "/api/v1/sessions/old", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if f.provider.Running() != 0 {
		t.Error("the adopted sandbox was not terminated")
	}
	// Its events stream, with no launch log, is a single terminal event
	// derived from the state — the session stopped, so a client that
	// was following its launch is told so rather than handed a session
	// it cannot reach.
	evs := f.events(adopted.ID)
	if len(evs) != 1 || evs[0].Name != "error" {
		t.Errorf("events for an adopted session: %+v", evs)
	}
}

// TestAuthenticationAndGuardOrder: the API needs a credential, health
// does not, and a cross-site request is refused before the authenticator
// ever runs.
//
// @decision(DL-005) @decision(DL-006)
func TestAuthenticationAndGuardOrder(t *testing.T) {
	f := newAPIFixture(t)
	resp, _ := f.do(http.MethodGet, "/api/v1/me", nil, func(r *http.Request) { r.Header.Del("Authorization") })
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credential: %d", resp.StatusCode)
	}
	resp, body := f.do(http.MethodGet, "/api/v1/me", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"user_id":"local"`) {
		t.Errorf("me: %d %s", resp.StatusCode, body)
	}
	for _, path := range []string{"/livez", "/readyz"} {
		if resp, _ := f.do(http.MethodGet, path, nil, func(r *http.Request) { r.Header.Del("Authorization") }); resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
	before := f.auth.calls.Load()
	resp, _ = f.do(http.MethodDelete, "/api/v1/sessions/x", nil, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "https://evil.example")
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site: %d", resp.StatusCode)
	}
	if f.auth.calls.Load() != before {
		t.Error("the authenticator ran for a request the guard should have refused")
	}
	// The documentation describes the API's shape, which is free
	// reconnaissance for anyone who can reach the port but is not this
	// user; only health is unauthenticated.
	//
	// @decision(DL-006)
	for _, path := range []string{"/docs", "/openapi.json", "/openapi.yaml", "/schemas/ErrorModel.json"} {
		if resp, _ := f.do(http.MethodGet, path, nil, func(r *http.Request) { r.Header.Del("Authorization") }); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a credential: %d, want 401", path, resp.StatusCode)
		}
	}
	if resp, _ := f.do(http.MethodGet, "/docs", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("docs with a credential: %d", resp.StatusCode)
	}
}

// TestSessionsAreScopedToTheirOwner: a non-admin sees only their own,
// and another user's session reads as not found.
//
// @decision(DL-005) @decision(DL-006)
func TestSessionsAreScopedToTheirOwner(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)
	other := auth.Principal{UserID: "u2", OrgID: auth.LocalID}
	f.auth.inner = auth.StaticAuthenticator{Principal: other}
	if resp, _ := f.do(http.MethodGet, "/api/v1/sessions/alpha", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another user's session: %d", resp.StatusCode)
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/sessions/alpha", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another user's delete: %d", resp.StatusCode)
	}
	_, body := f.do(http.MethodGet, "/api/v1/sessions", nil)
	if strings.Contains(string(body), "alpha") {
		t.Errorf("another user's session listed: %s", body)
	}
	if f.provider.Running() != 1 {
		t.Error("the sandbox was terminated by another user")
	}
}

// TestReposAndConnect.
//
// @decision(DL-005)
func TestReposAndConnect(t *testing.T) {
	f := newAPIFixture(t)
	// First, so the cache holds nothing for it to answer from (DL-134).
	f.forge.err = errors.New("forge down")
	if resp, _ := f.do(http.MethodGet, "/api/v1/repos", nil); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("forge failure: %d", resp.StatusCode)
	}
	f.forge.err = nil
	resp, body := f.do(http.MethodGet, "/api/v1/repos", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"owner":"acme"`) || !strings.Contains(string(body), `"private":true`) {
		t.Errorf("repos: %d %s", resp.StatusCode, body)
	}
	id := f.create(repoReq("alpha"))
	f.events(id)
	if resp, _ := f.do(http.MethodPost, "/api/v1/sessions/alpha/connect", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("connect: %d", resp.StatusCode)
	}
	if resp, _ := f.do(http.MethodPost, "/api/v1/sessions/nope/connect", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("connect unknown: %d", resp.StatusCode)
	}
}

// TestOpenAPIDocumentsSecurityOnEveryOperation: the register helper is
// the only way in, so every /api/v1 operation carries the requirement.
//
// @decision(DL-005)
func TestOpenAPIDocumentsSecurityOnEveryOperation(t *testing.T) {
	f := newAPIFixture(t)
	for _, downgrade := range []bool{false, true} {
		doc, err := f.srv.OpenAPI(downgrade)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(doc, []byte("/api/v1/sessions/{ref}/events")) {
			t.Error("the SSE operation is not documented")
		}
	}
	// Every registered operation under the prefix names the scheme.
	var checked int
	for p, item := range f.srv.api.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{"GET": item.Get, "POST": item.Post, "DELETE": item.Delete} {
			if op == nil {
				continue
			}
			checked++
			if len(op.Security) == 0 || op.Security[0][bearerScheme] == nil {
				t.Errorf("%s %s has no security requirement", method, p)
			}
		}
	}
	if checked < 8 {
		t.Errorf("only %d operations checked", checked)
	}
	defer func() {
		if recover() == nil {
			t.Error("an operation outside the prefix was registered")
		}
	}()
	register(f.srv.api, huma.Operation{OperationID: "rogue", Method: http.MethodGet, Path: "/rogue"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
}

// TestReadyzReportsAStoreThatCannotBeReached.
//
// @decision(DL-005)
func TestReadyzReportsAStoreThatCannotBeReached(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.deps.Store = failingPing{f.store}
	if resp, _ := f.do(http.MethodGet, "/readyz", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz with a dead store: %d", resp.StatusCode)
	}
}

type failingPing struct{ store.Store }

func (failingPing) Ping(context.Context) error { return fmt.Errorf("down") }

// TestBodyLimit.
//
// @decision(DL-005)
func TestBodyLimit(t *testing.T) {
	f := newAPIFixture(t)
	tooLarge := func(code int) bool { return code == http.StatusRequestEntityTooLarge || code == http.StatusBadRequest }
	big := strings.Repeat("x", MaxBodyBytes+1)
	if resp, _ := f.do(http.MethodPost, "/api/v1/sessions/alpha/messages", map[string]string{"text": big}); !tooLarge(resp.StatusCode) {
		t.Errorf("an oversized message: %d", resp.StatusCode)
	}
	// A launch or a resume may carry a sync bundle, past huma's own 1 MiB.
	//
	// @decision(DL-027) @decision(DL-098)
	for _, path := range []string{"/api/v1/sessions", "/api/v1/sessions/alpha/resume"} {
		if resp, body := f.do(http.MethodPost, path, map[string]string{"name": big, "sync_bundle": big}); tooLarge(resp.StatusCode) {
			t.Errorf("%s refused a body under its limit: %d %s", path, resp.StatusCode, body)
		}
		huge := strings.Repeat("x", MaxSessionBodyBytes+1)
		if resp, _ := f.do(http.MethodPost, path, map[string]string{"name": huge}); !tooLarge(resp.StatusCode) {
			t.Errorf("%s took a body over its limit: %d", path, resp.StatusCode)
		}
	}
}

// TestDeleteDuringALaunchTerminatesWhatTheLaunchCreates: the sandbox
// does not exist when the delete arrives, so the launch must finish the
// job rather than overwrite the deletion.
//
// @decision(DL-005)
func TestDeleteDuringALaunchTerminatesWhatTheLaunchCreates(t *testing.T) {
	f := newAPIFixture(t)
	release := make(chan struct{})
	started := make(chan struct{})
	inner := f.srv.deps.Launch
	f.srv.svc.deps.Launch = func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error) {
		close(started)
		<-release
		return inner(ctx, opts, steps)
	}
	id := f.create(repoReq("alpha"))
	<-started
	if resp, body := f.do(http.MethodDelete, "/api/v1/sessions/"+id, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete while starting: %d %s", resp.StatusCode, body)
	}
	close(release)
	evs := f.events(id)
	if last := evs[len(evs)-1]; last.Name != "error" || !strings.Contains(string(last.Data), "deleted") {
		t.Errorf("stream ended with %s %s", last.Name, last.Data)
	}
	if f.provider.Running() != 0 {
		t.Error("the sandbox created by the cancelled launch is still running")
	}
	_, body := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	if !strings.Contains(string(body), `"state":"stopped"`) {
		t.Errorf("record after delete-during-launch: %s", body)
	}
}

// TestDeleteKeepsTheSessionLiveWhenTerminationFails: a retry must retry.
//
// @decision(DL-005)
func TestDeleteKeepsTheSessionLiveWhenTerminationFails(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)
	f.provider.TerminateErr = errors.New("modal: 503")
	if resp, _ := f.do(http.MethodDelete, "/api/v1/sessions/"+id, nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed delete: %d", resp.StatusCode)
	}
	_, body := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	if !strings.Contains(string(body), `"state":"running"`) {
		t.Fatalf("a failed termination was recorded as stopped: %s", body)
	}
	f.provider.TerminateErr = nil
	if resp, _ := f.do(http.MethodDelete, "/api/v1/sessions/"+id, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("retry: %d", resp.StatusCode)
	}
	if f.provider.Running() != 0 {
		t.Error("the retry did not terminate the sandbox")
	}
}

// TestErrorsAreRedactedBeforeTheyLeave.
//
// @decision(DL-005)
func TestErrorsAreRedactedBeforeTheyLeave(t *testing.T) {
	f := newAPIFixture(t)
	f.forge.err = errors.New("clone https://x:ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345@git.test/o/r failed")
	if _, body := f.do(http.MethodGet, "/api/v1/repos", nil); strings.Contains(string(body), "ghp_") {
		t.Errorf("a token reached a client: %s", body)
	}
	id := f.create(repoReq("alpha"))
	evs := f.events(id)
	if strings.Contains(string(evs[len(evs)-1].Data), "ghp_") {
		t.Error("a token reached the event stream")
	}
	if _, body := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil); strings.Contains(string(body), "ghp_") {
		t.Errorf("a token was stored in the session record: %s", body)
	}
}

// TestEveryDocumentedPathIsUnderThePrefix: nothing joins the API
// outside the authenticated prefix.
//
// @decision(DL-005)
func TestEveryDocumentedPathIsUnderThePrefix(t *testing.T) {
	f := newAPIFixture(t)
	for p := range f.srv.api.OpenAPI().Paths {
		if !strings.HasPrefix(p, APIPrefix) {
			t.Errorf("%s is documented outside %s", p, APIPrefix)
		}
	}
}

// TestFinishedLaunchLogsArePruned.
//
// @decision(DL-005)
func TestFinishedLaunchLogsArePruned(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("alpha"))
	f.events(id)
	f.srv.svc.deps.Now = func() time.Time { return time.Now().Add(logRetention + time.Minute) }
	f.create(repoReq("beta"))
	if _, ok := f.srv.svc.Log(id); ok {
		t.Error("a finished log older than the retention survived a create")
	}
	// The pruned session still streams a terminal event from its state.
	if evs := f.events(id); len(evs) != 1 || evs[0].Name != "ready" {
		t.Errorf("pruned session events: %+v", evs)
	}
}

// TestTheDocumentCarriesNoBuildIdentity: info.version names the
// contract (v1), not a build stamp that would fail the drift gate on
// every commit.
//
// @decision(DL-005)
func TestTheDocumentCarriesNoBuildIdentity(t *testing.T) {
	f := newAPIFixture(t)
	if got := f.srv.api.OpenAPI().Info.Version; got != APIVersion {
		t.Errorf("info.version = %q, want %q", got, APIVersion)
	}
	doc, err := f.srv.OpenAPI(false)
	if err != nil {
		t.Fatal(err)
	}
	// "dev", "built" and a bare "spov " are what the build stamp looked
	// like: `spov dev (0123456789ab) built 2026-01-01T00:00:00Z`.
	for _, stamp := range []string{"built ", "spov dev"} {
		if bytes.Contains(doc, []byte(stamp)) {
			t.Errorf("the document contains a build stamp %q", stamp)
		}
	}
}

// TestTheDocumentIsDeterministic: the CI gate regenerates the document
// and diffs it against the committed copy, so two dumps of the same
// build must be byte-identical.
//
// @decision(DL-005)
func TestTheDocumentIsDeterministic(t *testing.T) {
	for _, downgrade := range []bool{false, true} {
		first, err := newAPIFixture(t).srv.OpenAPI(downgrade)
		if err != nil {
			t.Fatal(err)
		}
		// A second server, not a second dump of the first: the gate
		// compares across processes, and map iteration order is the
		// thing that would differ.
		second, err := newAPIFixture(t).srv.OpenAPI(downgrade)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Errorf("downgrade=%v: two dumps differ; the drift gate cannot hold", downgrade)
		}
	}
}
