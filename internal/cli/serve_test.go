package cli

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// TestServeOpenAPIPrintsTheDocument: the contract is printable without
// binding a port.
//
// @decision(DL-005)
func TestServeOpenAPIPrintsTheDocument(t *testing.T) {
	out, _, err := runWith(t, Env{}, "serve", "openapi")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"openapi: 3.1", "/api/v1/sessions", "/api/v1/sessions/{ref}/connect", "text/event-stream"} {
		if !strings.Contains(out, want) {
			t.Errorf("document lacks %q", want)
		}
	}
	out, _, err = runWith(t, Env{}, "serve", "openapi", "--3.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "openapi: 3.0") {
		t.Errorf("--3.0 did not downgrade:\n%.200s", out)
	}
}

// TestServeRefusesToStartWithoutAToken: an unauthenticated server would
// hand the Modal credential to anything on the machine.
//
// @decision(DL-006) @decision(DL-002)
func TestServeRefusesToStartWithoutAToken(t *testing.T) {
	_, _, err := runWith(t, Env{Provider: sandbox.NewFake()}, "serve", "--listen", "127.0.0.1:0")
	if err == nil || !strings.Contains(err.Error(), ServerTokenEnvVar) {
		t.Errorf("err = %v", err)
	}
}

// TestServeListensAndStopsOnCancel runs the real command: it prints its
// address, answers health, refuses an unauthenticated API call, accepts
// the token, and exits when the context ends.
//
// @decision(DL-002)
func TestServeListensAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := oneConnClient(t)
	var errBuf syncBuffer
	env := Env{
		Err:      &errBuf,
		Out:      &strings.Builder{},
		Environ:  config.MapEnv{Vars: map[string]string{ServerTokenEnvVar: "tok", TokensInSandboxEnvVar: "1"}},
		Workdir:  t.TempDir(),
		Provider: sandbox.NewFake(),
	}
	root := NewRoot(env)
	root.SetArgs([]string{"serve", "--listen", "127.0.0.1:0"})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	addr := waitForURL(t, &errBuf)

	resp, err := client.Get(addr + "/livez")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("livez = %d", resp.StatusCode)
	}
	resp, err = client.Get(addr + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated me = %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, addr+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authenticated me = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v", err)
		}
	case <-time.After(shutdownBound):
		t.Fatal("serve did not stop after its context was cancelled")
	}
}

// oneConnClient is a client that cannot leave a connection the server's
// drain has to wait out.
//
// net/http's Shutdown closes idle connections at once, but a connection
// in StateNew — accepted, no request header read yet — is only treated
// as idle after five seconds (net/http issue 22682), and until then the
// server is not quiescent. http.Transport can dial a second, speculative
// connection while a request waits for the first to come free; since the
// requests here are sequential, MaxConnsPerHost costs nothing and stops
// that connection from ever arriving.
func oneConnClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{MaxConnsPerHost: 1}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// shutdownBound is how long serve gets to return after its context is
// cancelled. It sits above net/http's five-second StateNew grace
// (oneConnClient), so a stray connection is a slower pass rather than a
// failure, and below ShutdownBudget, so a drain that genuinely never
// finishes still fails here.
const shutdownBound = 12 * time.Second

func TestServeReportsAListenFailure(t *testing.T) {
	env := Env{Environ: config.MapEnv{Vars: map[string]string{ServerTokenEnvVar: "tok", TokensInSandboxEnvVar: "1"}}, Provider: sandbox.NewFake()}
	if _, _, err := runWith(t, env, "serve", "--listen", "256.256.256.256:1"); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("err = %v", err)
	}
}

// TestServerDepsComeFromConfig: the server is assembled from the
// configuration the CLI reads: the image default, sizing, egress,
// provider keys and the git identity all cross over.
//
// @decision(DL-002)
func TestServerDepsComeFromConfig(t *testing.T) {
	wd := t.TempDir()
	cfg := userConfigEnv(t, defaultConfig+"sandbox:\n  cpu: 2\n  egress: install\n  egress_extra_domains: [x.example]\n  image: custom:1\ngit:\n  name: Cfg Person\n  email: cfg@example.com\n").Vars["XDG_CONFIG_HOME"]
	env := &Env{
		Environ:      config.MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": cfg, "USER": "alice", "OPENAI_API_KEY": "sk-x"}},
		Workdir:      wd,
		ServerConfig: true,
	}
	res, err := env.config()
	if err != nil {
		t.Fatal(err)
	}
	deps, _, err := env.serverDeps(context.Background(), res.Config, sandbox.NewFake(), "tok", false)
	if err != nil {
		t.Fatal(err)
	}
	d := deps.Defaults
	if d.Image != "custom:1" || d.Port != 4096 || d.Sandbox.CPU != 2 || d.Egress != "install" || len(d.EgressExtraDomains) != 1 {
		t.Errorf("defaults = %+v", d)
	}
	if deps.ProviderKeys["OPENAI_API_KEY"] != "sk-x" {
		t.Errorf("provider keys = %v", deps.ProviderKeys)
	}
	if deps.Identity != (git.Identity{Name: "Cfg Person", Email: "cfg@example.com"}) {
		t.Errorf("identity = %+v", deps.Identity)
	}
	if deps.User != "alice" || deps.Store == nil || deps.Auth == nil || deps.Launch == nil || deps.Forge == nil {
		t.Errorf("deps = %+v", deps)
	}
	if _, _, err := env.serverDeps(context.Background(), res.Config, sandbox.NewFake(), "", false); err == nil {
		t.Error("an empty token was accepted")
	}
	// @decision(DL-043)
	if deps.GitAllowAll {
		t.Error("a hosted server lets git access all reach its own credentials without being told to")
	}
}

// A hosted server lets git access all reach its own credentials only
// when git.allow_all is set; a local server's credentials are its user's.
//
// @decision(DL-043)
func TestTheGitAllowAllSwitchReachesTheServer(t *testing.T) {
	wd := t.TempDir()
	cfg := userConfigEnv(t, defaultConfig).Vars["XDG_CONFIG_HOME"]
	env := &Env{Environ: config.MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": cfg, "USER": "alice", "SPOV_GIT_ALLOW_ALL": "1"}},
		Workdir: wd, ServerConfig: true}
	res, err := env.config()
	if err != nil {
		t.Fatal(err)
	}
	if deps, _, err := env.serverDeps(context.Background(), res.Config, sandbox.NewFake(), "tok", false); err != nil || !deps.GitAllowAll {
		t.Errorf("SPOV_GIT_ALLOW_ALL=1 left it %v (%v)", deps.GitAllowAll, err)
	}
	res.Config.Git.AllowAll = false
	if deps, _, err := env.serverDeps(context.Background(), res.Config, sandbox.NewFake(), "tok", true); err != nil || !deps.GitAllowAll {
		t.Errorf("a local server left it %v (%v)", deps.GitAllowAll, err)
	}
}

// syncBuffer is a strings.Builder safe to read while another goroutine
// writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestShutdownBudgetsAreOrdered: the hosted budget must sit below the
// orchestrator's grace period, or the process is killed mid-drain, and
// above the local one, which has a person waiting on a terminal.
//
// @decision(DL-010)
func TestShutdownBudgetsAreOrdered(t *testing.T) {
	if ShutdownBudget >= 30*time.Second {
		t.Errorf("the hosted budget %v is not below Kubernetes' 30s default", ShutdownBudget)
	}
	if LocalShutdownBudget >= ShutdownBudget {
		t.Errorf("the local budget %v is not below the hosted one %v", LocalShutdownBudget, ShutdownBudget)
	}
}

// TestServeAcceptsAPublicURL: a server behind a proxy takes its origin
// from the environment, and the guard accepts that Host.
//
// @decision(DL-010)
func TestServeAcceptsAPublicURL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var errBuf syncBuffer
	env := Env{
		Err: &errBuf,
		Out: &strings.Builder{},
		Environ: config.MapEnv{Vars: map[string]string{
			ServerTokenEnvVar:     "tok",
			TokensInSandboxEnvVar: "1",
			PublicURLEnvVar:       "https://spov.example.test",
		}},
		Workdir:  t.TempDir(),
		Provider: sandbox.NewFake(),
	}
	root := NewRoot(env)
	root.SetArgs([]string{"serve", "--listen", "127.0.0.1:0"})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	url := waitForURL(t, &errBuf)
	req, _ := http.NewRequest(http.MethodGet, url+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Host = "spov.example.test"
	resp, err := oneConnClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a request for the configured public host = %d", resp.StatusCode)
	}
	// Unbounded: this test asserts the public host is accepted, not how
	// long the drain takes. It uses the same client so a stray
	// connection cannot make it sit out that drain for five seconds.
	cancel()
	<-done
}

// TestServeRefusesAMalformedPublicURL: a value that cannot be a host
// fails at startup rather than refusing every request later.
//
// @decision(DL-010)
func TestServeRefusesAMalformedPublicURL(t *testing.T) {
	env := Env{
		Environ: config.MapEnv{Vars: map[string]string{
			ServerTokenEnvVar:     "tok",
			TokensInSandboxEnvVar: "1",
			PublicURLEnvVar:       "spov.example.test",
		}},
		Provider: sandbox.NewFake(),
	}
	_, _, err := runWith(t, env, "serve", "--listen", "127.0.0.1:0")
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Errorf("err = %v", err)
	}
}

// waitForURL reads the address the server prints on start. A --listen
// :0 caller has no other way to learn the port.
func waitForURL(t *testing.T, errBuf *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if i := strings.Index(errBuf.String(), "http://"); i >= 0 {
			url := strings.TrimSpace(errBuf.String()[i:])
			if j := strings.Index(url, "\n"); j >= 0 {
				url = url[:j]
			}
			return url
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the server never printed its address:\n%s", errBuf.String())
	return ""
}

// TestLocalModeIgnoresAConfiguredDatabase: an exported SPOV_DATABASE_URL
// belongs to a hosted deployment, not to the server a developer's CLI
// just spawned for itself. Local mode has no database (DL-003).
//
// @decision(DL-010) @decision(DL-003)
func TestLocalModeIgnoresAConfiguredDatabase(t *testing.T) {
	env := Env{
		Err: &syncBuffer{},
		Out: &strings.Builder{},
		Environ: config.MapEnv{Vars: map[string]string{
			// Unreachable: if local mode tried to open it, this fails.
			DatabaseURLEnvVar: "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1",
		}},
		Workdir: t.TempDir(),
	}
	st, closeStore, _, err := env.openStore(context.Background(), true, config.Defaults(), func(string) {})
	if err != nil {
		t.Fatalf("local mode tried to open the database: %v", err)
	}
	defer closeStore()
	if _, ok := st.(*store.Memory); !ok {
		t.Errorf("local mode got a %T, want the memory store", st)
	}
}

// TestAConfiguredDatabaseIsOpenedInHostedMode: the other half — hosted
// mode must actually use Postgres, or every replica has its own
// sessions and two of them disagree about every listing.
//
// @decision(DL-010)
func TestAConfiguredDatabaseIsOpenedInHostedMode(t *testing.T) {
	env := Env{
		Err: &syncBuffer{},
		Out: &strings.Builder{},
		Environ: config.MapEnv{Vars: map[string]string{
			DatabaseURLEnvVar: "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1",
		}},
		Workdir: t.TempDir(),
	}
	// The database is unreachable, so opening it must fail rather than
	// silently falling back to memory.
	if _, _, _, err := env.openStore(context.Background(), false, config.Defaults(), func(string) {}); err == nil {
		t.Fatal("hosted mode fell back to the memory store instead of failing")
	}
}

// TestNoDatabaseGivesTheMemoryStore: a bare `spov serve` still works.
//
// @decision(DL-010)
func TestNoDatabaseGivesTheMemoryStore(t *testing.T) {
	env := Env{Environ: config.MapEnv{Vars: map[string]string{}}, Workdir: t.TempDir()}
	st, closeStore, _, err := env.openStore(context.Background(), false, config.Defaults(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	if _, ok := st.(*store.Memory); !ok {
		t.Errorf("got a %T, want the memory store", st)
	}
}

// Local mode has no supervisor at all: the queue is River on Postgres,
// and a memory store cannot carry one. A local server refuses a job and
// says why rather than accepting one nothing will run.
//
// @decision(DL-046) @decision(DL-045)
func TestNoSupervisorWithoutADatabase(t *testing.T) {
	if got := newSupervisor(store.NewMemory(), nil, func(string) {}); got != nil {
		t.Errorf("newSupervisor = %v against a memory store, want nil", got)
	}
}

// The platforms a launch with git access all reaches are the configured
// ones the caller holds a credential for.
//
// @decision(DL-043)
func TestTheForgesPlatformsAreTheOnesWithACredential(t *testing.T) {
	creds := credentials.NewMemoryStore()
	key, _ := credentials.PlatformKey("codefloe")
	if err := creds.Set(key, "stored-token"); err != nil {
		t.Fatal(err)
	}
	env := &Env{Credentials: creds, NewPlatform: func(_ context.Context, p config.Platform, _ string) (git.Platform, error) {
		fake := git.NewFake()
		fake.PlatformName = p.Name
		return fake, nil
	}}
	cfg := config.Config{Platforms: []config.Platform{
		{Name: "codefloe", Kind: config.KindForgejo, BaseURL: "https://codefloe.com"},
		{Name: "gh", Kind: config.KindGitHub},
	}}
	platforms, err := (&configForge{env: env, cfg: cfg, warn: func(string) {}}).Platforms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(platforms) != 1 || platforms[0].Name() != "codefloe" {
		t.Errorf("platforms = %v", platforms)
	}
}
