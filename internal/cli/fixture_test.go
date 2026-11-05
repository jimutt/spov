package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// TestMain stops a spawned child from running the suite. A test that
// reaches the real spawn (TestSpawnExecsThisBinary does on purpose)
// execs this test binary as `serve --local`; the testing package ignores
// those arguments and would run every test again, spawning again, in a
// chain that outlives `go test`.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runWith executes the command tree with a prepared environment.
func runWith(t *testing.T, env Env, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	env.Out, env.Err = &out, &errBuf
	if env.Environ == nil {
		env.Environ = config.MapEnv{Vars: map[string]string{}}
	}
	if env.Workdir == "" {
		env.Workdir = t.TempDir()
	}
	// Never the developer's real state directory: a test must not find
	// or stop a local server they are actually using.
	if env.StateDir == "" {
		env.StateDir = t.TempDir()
	}
	// Stubbed by default so tests do not shell out to the developer's
	// git and inherit whatever identity that machine happens to have.
	//
	// @decision(DL-044)
	if env.LocalIdentity == nil {
		env.LocalIdentity = func(context.Context, string) (git.Identity, error) {
			return git.Identity{Name: "Test Person", Email: "test@example.com"}, nil
		}
	}
	// A test that wants a local server spawned says so; the real spawn
	// execs this test binary (see TestMain).
	if env.Spawn == nil {
		env.Spawn = func(context.Context) error { return errors.New("this test spawns no local server") }
	}
	root := NewRoot(env)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errBuf.String(), err
}

// stubHealth answers a health wait without an OpenCode server.
type stubHealth struct {
	health opencode.Health
	err    error
}

func (s *stubHealth) WaitHealthy(context.Context, time.Duration) (opencode.Health, error) {
	return s.health, s.err
}

// defaultConfig is the user file most fixtures start from: one Forgejo
// platform and one repository on it. platformOnlyConfig configures the
// platform and leaves the repositories to be chosen. A user file, since
// a project file may not declare platforms and the server reads none
// (DL-026).
const platformOnlyConfig = `platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
`

const defaultConfig = `platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
repos:
  - platform: codefloe
    owner: alice
    name: spov
`

// apiFixture is the real API served in-process, assembled by the same
// serverDeps that `spov serve` uses, over a fake provider and a fake
// platform; and a client Env pointed at it. It is what every command
// test runs against, so the CLI is exercised exactly as a user's would
// be, HTTP included.
//
// @decision(DL-002)
type apiFixture struct {
	// adjustDeps, when set, changes the server's deps after they are
	// wired and before it starts.
	adjustDeps func(*server.Deps)
	t          *testing.T
	wd         string
	provider   *sandbox.Fake
	platform   *git.Fake
	creds      *credentials.MemoryStore
	token      string
	// serverEnv is the environment `spov serve` assembled the server
	// from; serverErr collects what it warned about.
	serverEnv *Env
	serverErr bytes.Buffer
	deps      server.Deps
	srv       *server.Server
	http      *httptest.Server
	// env is the client's environment.
	env Env
	// health is what both health stubs report; proxyHealthErr fails only
	// the client's own check through the proxy.
	health         opencode.Health
	proxyHealthErr error
	tuiRuns        int
	tuiSeen        opencode.TUI
	tuiCode        int
	tuiErr         error
	// proxyStarts counts client proxies started; proxyErr fails them.
	proxyStarts int
	proxyErr    error
}

func newAPIFixture(t *testing.T) *apiFixture {
	return newAPIFixtureWithConfig(t, defaultConfig)
}

func newAPIFixtureWithConfig(t *testing.T, body string) *apiFixture {
	t.Helper()
	wd := t.TempDir()
	cfgHome := t.TempDir()
	if body != "" {
		writeUserConfig(t, cfgHome, body)
	}
	creds := credentials.NewMemoryStore()
	key, _ := credentials.PlatformKey("codefloe")
	if err := creds.Set(key, "stored-token"); err != nil {
		t.Fatal(err)
	}

	f := &apiFixture{
		t: t, wd: wd, provider: sandbox.NewFake(), platform: git.NewFake(), creds: creds,
		token: "tok-" + strings.ReplaceAll(t.Name(), "/", "-"),
		// The pinned version by default, so the gate passes unless a test
		// deliberately breaks it.
		health: opencode.Health{Healthy: true, Version: config.DefaultOpenCodeVersion, PID: 2},
	}
	f.platform.PlatformName = "codefloe"
	f.platform.HostName = "codefloe.com"
	f.platform.Repos = []git.Repo{{
		Owner: "alice", Name: "spov",
		CloneURL: "https://codefloe.com/alice/spov.git",
	}}

	f.serverEnv = &Env{
		Err: &f.serverErr,
		Environ: config.MapEnv{Vars: map[string]string{
			"USER": "alice", ServerTokenEnvVar: f.token, "XDG_CONFIG_HOME": cfgHome,
		}},
		ServerConfig: true,
		Workdir:      wd,
		Credentials:  creds,
		Provider:     f.provider,
		NewPlatform: func(context.Context, config.Platform, string) (git.Platform, error) {
			return f.platform, nil
		},
		NewHealthChecker: func(string, string) session.HealthChecker { return &stubHealth{health: f.health} },
		HealthTimeout:    2 * time.Second,
		// Whether the build embeds a real proxy must not decide what a
		// test sees.
		ProxyBinary: func() []byte { return []byte("fake spov-proxy") },
		// The shipped list, never a fetch from models.dev (DL-013).
		Models: server.FixedModels(modelcat.Snapshot()),
	}
	f.startServer()

	f.env = Env{
		Workdir: wd,
		Environ: config.MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": cfgHome}},
		Client:  client.New(f.http.URL, f.token),
		NewHealthChecker: func(string, string) session.HealthChecker {
			return &stubHealth{health: f.health, err: f.proxyHealthErr}
		},
		HealthTimeout: time.Second,
		LocateBinary: func(context.Context, string) (opencode.Located, error) {
			return opencode.Located{Path: "/fake/opencode"}, nil
		},
		RunTUI: func(_ context.Context, tui opencode.TUI) (int, error) {
			f.tuiRuns++
			f.tuiSeen = tui
			return f.tuiCode, f.tuiErr
		},
		// The proxy is started for real, minting through the server, so
		// the loopback URL clients receive is a real, listening address.
		//
		// @decision(DL-030)
		StartClientProxy: func(ctx context.Context, c session.Connector, warn func(string)) (*proxy.Proxy, error) {
			f.proxyStarts++
			if f.proxyErr != nil {
				return nil, f.proxyErr
			}
			return session.StartClientProxy(ctx, c, warn)
		},
	}
	return f
}

// startServer assembles and serves the API over a real listener.
func (f *apiFixture) startServer() {
	f.t.Helper()
	res, err := f.serverEnv.config()
	if err != nil {
		f.t.Fatal(err)
	}
	f.deps, _, err = f.serverEnv.serverDeps(context.Background(), res.Config, f.provider, f.token, true)
	if err != nil {
		f.t.Fatal(err)
	}
	// The credential store and verifier, as `spov serve` wires them.
	//
	// @decision(DL-042)
	if err := f.serverEnv.wireCredentials(&f.deps, res.Config, true); err != nil {
		f.t.Fatal(err)
	}
	if f.adjustDeps != nil {
		f.adjustDeps(&f.deps)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	f.srv, err = server.New(f.deps, ln.Addr().String())
	if err != nil {
		f.t.Fatal(err)
	}
	f.http = httptest.NewUnstartedServer(f.srv.Handler())
	f.http.Listener = ln
	f.http.Start()
	f.t.Cleanup(func() { f.http.Close(); f.srv.Close() })
}

// reconcile runs the reconciler once, which is how a sandbox the server
// did not start becomes a session it knows.
func (f *apiFixture) reconcile() {
	f.t.Helper()
	r := &server.Reconciler{Store: f.deps.Store, Provider: f.provider, Forget: f.srv.Service().ForgetSession}
	if err := r.Once(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// seedSession creates a running sandbox in the provider and lets the
// reconciler adopt it, which is the state every attach starts from.
func (f *apiFixture) seedSession(name string, age time.Duration) sandbox.Handle {
	f.t.Helper()
	created := time.Now().Add(-age)
	f.provider.Now = func() time.Time { return created }
	h, err := f.provider.Create(context.Background(), sandbox.Spec{
		Name:        name,
		Image:       "img",
		Command:     []string{"true"},
		Ports:       []int{4096},
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     time.Hour,
		IdleTimeout: time.Minute,
		Tags:        sandbox.BuildTags(sandbox.TagSet{Name: name, User: "alice", Repos: []string{"acme/app"}, CreatedAt: created}),
		Egress:      sandbox.EgressOpen(),
		Secrets:     map[string]string{opencode.PasswordEnvVar: "seeded-password"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.provider.Now = nil
	// What the server's password recovery reads back out of the sandbox.
	if f.provider.ExecResults == nil {
		f.provider.ExecResults = map[string]sandbox.ExecResult{}
	}
	f.provider.ExecResults["printenv "+opencode.PasswordEnvVar] = sandbox.ExecResult{Stdout: "seeded-password\n"}
	f.reconcile()
	return h
}

// session looks a session up by name straight from the store, in any
// state; the newest wins when a name was reused.
func (f *apiFixture) session(name string) store.Session {
	f.t.Helper()
	all, err := f.deps.Store.Sessions().List(context.Background(), "local", store.SessionFilter{})
	if err != nil {
		f.t.Fatal(err)
	}
	var found *store.Session
	for i := range all {
		if all[i].Name == name {
			found = &all[i]
		}
	}
	if found == nil {
		f.t.Fatalf("session %q: not in the store", name)
	}
	return *found
}

// launchedPassword is the OpenCode password the launcher generated for
// the most recent sandbox, read back from the spec the provider saw.
func (f *apiFixture) launchedPassword() string {
	f.t.Helper()
	if len(f.provider.Created) == 0 {
		f.t.Fatal("no sandbox was created")
	}
	spec := f.provider.Created[len(f.provider.Created)-1]
	pw := spec.Secrets[opencode.PasswordEnvVar]
	if pw == "" {
		f.t.Fatal("the created sandbox carries no password")
	}
	return pw
}

// waitFor polls cond for up to five seconds, for outcomes the server
// completes after it has already answered the client.
func (f *apiFixture) waitFor(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
