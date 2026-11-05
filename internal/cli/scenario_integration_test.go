//go:build integration

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// scenarioServer is a hosted spov server built the way `spov serve`
// builds one (serverDeps, wireLogin, wireCredentials), on its own fresh
// Postgres database, against scenarioGitHub and a fake sandbox provider.
// The periodic passes (renewal, sidecar checks) are called directly
// rather than waited for, and its clock is the scenario's. A restart
// builds a new server on the same database and the same sandboxes.
//
// @decision(DL-022)
type scenarioServer struct {
	t        *testing.T
	gh       *scenarioGitHub
	clock    *scenarioClock
	provider *sandbox.Fake
	vars     map[string]string
	dir      string
	token    string // the shared bearer

	log    lockedBuffer
	bodies []string

	srv   *server.Server
	recon *server.Reconciler
	store store.Store
	base  string
	stop  func()

	users map[string]string // name -> CLI token

	// jobs is the supervisor, wired as `spov serve` wires it, with a
	// queue that drops what it is given: a suite runs each pass itself.
	jobs    *jobs.Supervisor
	appKeys []string

	// metrics is this process's Prometheus listener, started as
	// `spov serve` starts it; each restart starts a fresh one.
	metrics string
}

type scenarioClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *scenarioClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *scenarioClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type scenarioHealth struct{}

func (scenarioHealth) WaitHealthy(context.Context, time.Duration) (opencode.Health, error) {
	return opencode.Health{Healthy: true, Version: "0.0.0-scenario"}, nil
}

// newScenarioServer makes the database, the config and the two users'
// allowlist entries, and boots the first server.
//
// extra is appended to the server's platforms, as YAML list entries.
func newScenarioServer(t *testing.T, extra ...string) *scenarioServer {
	t.Helper()
	admin := os.Getenv("SPOV_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("set SPOV_TEST_DATABASE_URL to run the combined scenarios against Postgres")
	}
	ctx := context.Background()
	dbURL := freshDatabase(t, admin)

	// Behind the real clock by more than a run advances it: the store
	// stamps an event's created_at with its own clock, and the renewal
	// pass compares that with mint times taken from this clock. So once a
	// session has an event it stays in use; the suite does not test idle
	// sessions.
	clock := &scenarioClock{t: time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)}
	s := &scenarioServer{
		t: t, clock: clock, provider: sandbox.NewFake(), dir: t.TempDir(),
		gh: newScenarioGitHub(t, clock.Now), token: "scenario-bearer-" + randomAlnum(24),
		users: map[string]string{},
	}
	s.gh.appRepos = []string{"both", "shared-only"}

	cfgDir := filepath.Join(s.dir, "config", "spov")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("platforms:\n  - name: github\n    kind: github\n    base_url: %s\n    account: %s\n", s.gh.srv.URL, s.gh.org) +
		strings.Join(extra, "")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	allow := filepath.Join(s.dir, "allowlist")
	if err := os.WriteFile(allow, []byte("admin@acme.test admin\nmember@acme.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	s.vars = map[string]string{
		"XDG_CONFIG_HOME":                 filepath.Join(s.dir, "config"),
		DatabaseURLEnvVar:                 dbURL,
		"SPOV_CREDENTIAL_KEY":             base64.StdEncoding.EncodeToString(key),
		"SPOV_GITHUB_OAUTH_CLIENT_ID":     "scenario-oauth-client",
		"SPOV_GITHUB_OAUTH_CLIENT_SECRET": "scenario-oauth-secret-" + randomAlnum(20),
		"SPOV_ALLOWLIST_FILE":             allow,
	}
	if _, stderr, err := runWith(t, Env{Environ: config.MapEnv{Vars: s.vars}}, "serve", "migrate", "up"); err != nil {
		t.Fatalf("migrate: %v\n%s", err, stderr)
	}
	s.boot(nil)
	for _, u := range []string{"admin", "member"} {
		s.users[u] = s.seedUser(ctx, u+"@acme.test", u == "admin")
	}
	t.Cleanup(func() { s.stop() })
	return s
}

// freshDatabase creates a database of its own for one run and drops it
// afterwards, so a scenario never meets another run's rows.
func freshDatabase(t *testing.T, adminURL string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to %s: %v", adminURL, err)
	}
	name := "scn_" + strings.ToLower(randomAlnum(10))
	if _, err := conn.Exec(ctx, "create database "+name); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "drop database if exists "+name+" with (force)")
		_ = c.Close(context.Background())
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// boot builds and serves a server with the base environment and extra.
func (s *scenarioServer) boot(extra map[string]string) {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	s.base = "http://" + ln.Addr().String()
	vars := maps.Clone(s.vars)
	vars[PublicURLEnvVar] = s.base
	maps.Copy(vars, extra)
	env := &Env{
		Out: io.Discard, Err: &s.log, Environ: config.MapEnv{Vars: vars, Home: s.dir},
		Workdir: s.dir, StateDir: filepath.Join(s.dir, "state"), Provider: s.provider,
		NewHealthChecker: func(string, string) session.HealthChecker { return scenarioHealth{} },
		Now:              s.clock.Now,
		ProxyBinary:      func() []byte { return []byte("scenario-proxy") },
	}
	res, err := env.config()
	if err != nil {
		s.t.Fatal(err)
	}
	deps, closeStore, err := env.serverDeps(ctx, res.Config, s.provider, s.token, false)
	if err != nil {
		cancel()
		s.t.Fatal(err)
	}
	// Until the server is up, a failure here releases what boot took.
	s.stop = func() {
		cancel()
		closeStore()
	}
	// @decision(DL-011) @decision(DL-022)
	telem, err := telemetry.Start(ctx, telemetry.Options{PrometheusListen: "127.0.0.1:0", Version: "scenario", Warn: deps.Warn})
	if err != nil || telem.Metrics == nil {
		s.t.Fatalf("metrics: %v", err)
	}
	s.metrics = telem.PrometheusAddr
	deps.Metrics = telem.Metrics
	if deps.Jobs != nil {
		deps.Jobs.Metrics = telem.Metrics
	}
	deps.PublicURL = s.base
	stopSessions, err := env.wireLogin(ctx, &deps)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := env.wireCredentials(&deps, res.Config, false); err != nil {
		s.t.Fatal(err)
	}
	srv, err := server.New(deps, ln.Addr().String())
	if err != nil {
		s.t.Fatal(err)
	}
	// The job wiring `spov serve` uses, with no recorder running and a
	// queue a suite drains by calling the supervisor itself.
	//
	// @decision(DL-022)
	if deps.Jobs != nil {
		env.wireJobs(deps, srv, server.NewRecorder(srv.Service(), deps.Store, env.now, deps.Warn), res.Config.Prep)
		deps.Jobs.Queue = func(context.Context, jobs.JobArgs) error { return nil }
		s.jobs = deps.Jobs
	}
	hs := srv.NewHTTPServer(ln.Addr().String())
	go func() { _ = hs.Serve(ln) }()
	s.srv, s.store = srv, deps.Store
	s.recon = env.newReconciler(deps, s.provider, srv)
	s.stop = func() {
		cancel()
		_ = hs.Close()
		srv.Close()
		stopSessions()
		closeStore()
		telem.Shutdown(context.Background())
	}
}

// restart stops the server, builds another on the same database, and
// runs one reconciler pass, which is what a booting `spov serve` does to
// the sessions it finds.
func (s *scenarioServer) restart(extra map[string]string) {
	s.t.Helper()
	s.stop()
	s.stop = func() {}
	s.provider.Reattach()
	s.boot(extra)
	if err := s.recon.Once(context.Background()); err != nil {
		s.t.Fatalf("reconcile after the restart: %v", err)
	}
}

// seedUser makes a user and a CLI token for them, as an OAuth login and
// `spov login` would, without GitHub.
func (s *scenarioServer) seedUser(ctx context.Context, email string, admin bool) string {
	s.t.Helper()
	now := s.clock.Now()
	u := store.User{ID: store.NewID(now), OrgID: server.DefaultOrgID, Email: email, Name: email, Admin: admin}
	if err := s.store.Users().Create(ctx, u); err != nil {
		s.t.Fatal(err)
	}
	tok, err := auth.NewToken()
	if err != nil {
		s.t.Fatal(err)
	}
	exp := now.Add(90 * 24 * time.Hour)
	if err := s.store.Tokens().Create(ctx, store.APIToken{
		ID: store.NewID(now), OrgID: server.DefaultOrgID, UserID: u.ID, Hash: auth.HashToken(tok),
		Name: "scenario", ExpiresAt: &exp, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		s.t.Fatal(err)
	}
	return tok
}

// call sends one API request as a user and keeps the body for the scan.
func (s *scenarioServer) call(user, method, path string, body any) (int, []byte) {
	s.t.Helper()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.base+path, payload)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.users[user])
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	s.bodies = append(s.bodies, string(out))
	return resp.StatusCode, out
}

// must is call that fails the test on an unexpected status, decoding the
// body into out when given.
func (s *scenarioServer) must(want int, user, method, path string, body, out any) {
	s.t.Helper()
	code, raw := s.call(user, method, path, body)
	if code != want {
		s.t.Fatalf("%s %s as %s: %d, want %d: %s", method, path, user, code, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			s.t.Fatalf("%s %s: decode: %v: %s", method, path, err, raw)
		}
	}
}

// launch starts a session as member on one repository and waits for
// the launch to settle, returning its view.
func (s *scenarioServer) launch(repo string, req map[string]any) server.SessionView {
	s.t.Helper()
	// Named here: a generated name comes from the clock, which the
	// scenario holds still.
	body := map[string]any{
		"name":  "scn-" + strings.ToLower(randomAlnum(8)),
		"repos": []map[string]string{{"platform": "github", "owner": s.gh.org, "name": repo}},
	}
	maps.Copy(body, req)
	var created server.SessionView
	s.must(http.StatusAccepted, "member", http.MethodPost, "/api/v1/sessions", body, &created)
	deadline := time.Now().Add(20 * time.Second)
	for {
		var v server.SessionView
		s.must(http.StatusOK, "member", http.MethodGet, "/api/v1/sessions/"+created.ID, nil, &v)
		if v.State != store.StateStarting {
			return v
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("session %s is still starting after 20 s", v.ID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *scenarioServer) end(id string) {
	s.t.Helper()
	code, raw := s.call("member", http.MethodDelete, "/api/v1/sessions/"+id, nil)
	if code >= 300 {
		s.t.Fatalf("end %s: %d %s", id, code, raw)
	}
}

func (s *scenarioServer) row(id string) store.Session {
	s.t.Helper()
	sess, err := s.store.Sessions().Get(context.Background(), server.DefaultOrgID, id)
	if err != nil {
		s.t.Fatal(err)
	}
	return sess
}

// act stores an agent event on the session, which is what makes it in
// use for the renewal pass.
func (s *scenarioServer) act(id string) {
	s.t.Helper()
	ctx := context.Background()
	now := s.clock.Now()
	conv, err := s.store.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(now), OrgID: server.DefaultOrgID, SessionID: id, OpenCodeID: "ses_scenario",
	})
	if err != nil {
		s.t.Fatal(err)
	}
	ev := store.Event{
		ID: store.NewID(now), OrgID: server.DefaultOrgID, SessionID: id, ConversationID: conv.ID,
		Source: store.SourceOpenCode, Name: "message.part.updated", At: now, Seq: now.UnixNano(),
	}
	if _, err := s.store.Conversations().PutEvents(ctx, []store.Event{ev}); err != nil {
		s.t.Fatal(err)
	}
}

// tierOf is the tier the session view names for the github platform.
func tierOf(v server.SessionView) string {
	for _, c := range v.Credentials {
		if c.Platform == "github" {
			return c.Tier
		}
	}
	return ""
}

// orgApp is the admin's view of the github platform's App.
type orgApp struct {
	Platforms []server.OrgGitHubView  `json:"platforms"`
	Events    []server.AdminEventView `json:"events"`
}

// scan fails on any secret of the run in a response body or the
// server's log: the App's keys, the personal token, every installation
// token minted, the users' CLI tokens and the deployment's own keys.
//
// @decision(DL-022)
func (s *scenarioServer) scan(t *testing.T, keys []string, pat string) {
	t.Helper()
	secrets := map[string]string{"the personal token": pat, "the credential key": s.vars["SPOV_CREDENTIAL_KEY"],
		"the OAuth secret": s.vars["SPOV_GITHUB_OAUTH_CLIENT_SECRET"], "the shared bearer": s.token}
	for i, k := range keys {
		for j, line := range strings.Split(k, "\n") {
			if len(line) >= 40 && !strings.HasPrefix(line, "-----") {
				secrets[fmt.Sprintf("line %d of App key %d", j, i+1)] = line
			}
		}
	}
	for i, sec := range s.gh.secrets() {
		secrets[fmt.Sprintf("installation token %d", i+1)] = sec
	}
	for u, tok := range s.users {
		secrets[u+"'s CLI token"] = tok
	}
	places := map[string]string{"the server log": s.log.String()}
	for i, b := range s.bodies {
		places[fmt.Sprintf("response %d", i+1)] = b
	}
	for name, sec := range secrets {
		for where, text := range places {
			if sec != "" && strings.Contains(text, sec) {
				t.Errorf("%s is in %s", name, where)
			}
		}
	}
	t.Logf("scanned %d responses and the server log for %d secrets", len(s.bodies), len(secrets))
}

// warningsFor is the warnings the server logged that name a session.
func (s *scenarioServer) warningsFor(id string) []string {
	var out []string
	for _, l := range strings.Split(s.log.String(), "\n") {
		if strings.Contains(l, id) {
			out = append(out, l)
		}
	}
	return out
}
