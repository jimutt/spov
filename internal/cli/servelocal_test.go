package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/version"
)

// localServe runs `spov serve --local` in-process over a fake provider
// and returns once its state file is readable.
func localServe(t *testing.T, provider sandbox.Provider, idle time.Duration) (*localState, string, <-chan error, context.CancelFunc) {
	t.Helper()
	return localServeWith(t, provider, idle, "")
}

func localServeWith(t *testing.T, provider sandbox.Provider, idle time.Duration, userConfig string) (*localState, string, <-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	wd := t.TempDir()
	vars := map[string]string{ServerTokenEnvVar: "must-be-ignored"}
	if userConfig != "" {
		vars["XDG_CONFIG_HOME"] = userConfigEnv(t, userConfig).Vars["XDG_CONFIG_HOME"]
	}
	creds := credentials.NewMemoryStore()
	key, _ := credentials.PlatformKey("codefloe")
	_ = creds.Set(key, "stored-token")
	env := &Env{
		Err:     &syncBuffer{},
		Environ: config.MapEnv{Vars: vars},
		Workdir: wd,
		// As runServe sets it through serve's PersistentPreRun.
		ServerConfig: true,
		StateDir:     dir,
		Provider:     provider,
		Credentials:  creds,
		NewPlatform:  func(context.Context, config.Platform, string) (git.Platform, error) { return git.NewFake(), nil },
		NewHealthChecker: func(string, string) session.HealthChecker {
			return &stubHealth{health: opencode.Health{Healthy: true, Version: config.DefaultOpenCodeVersion}}
		},
		HealthTimeout: 2 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, env, serveOptions{Local: true, idleExit: idle}) }()
	path := filepath.Join(dir, stateFileName)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := readState(path)
		if err == nil && st != nil {
			t.Cleanup(cancel)
			return st, path, done, cancel
		}
		select {
		case err := <-done:
			t.Fatalf("serve exited before writing its state: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no state file within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeLocalWritesAPrivateStateFileAndAcceptsItsToken: the state
// file is the handoff between server and CLI.
//
// @decision(DL-002) @decision(DL-006)
func TestServeLocalWritesAPrivateStateFileAndAcceptsItsToken(t *testing.T) {
	st, path, done, cancel := localServe(t, sandbox.NewFake(), time.Hour)
	if st.PID != os.Getpid() || !strings.HasPrefix(st.URL, "http://127.0.0.1:") || st.Version != version.Get().String() || st.StartedAt.IsZero() {
		t.Errorf("state = %+v", st)
	}
	if len(st.Token) < 40 || st.Token == "must-be-ignored" {
		t.Errorf("token = %q, want a generated one, never the environment's", st.Token)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	c := client.New(st.URL, st.Token)
	if me, err := c.Me(context.Background()); err != nil || me.UserID != "local" {
		t.Errorf("me = %+v, %v", me, err)
	}
	if _, err := client.New(st.URL, "must-be-ignored").Me(context.Background()); client.StatusOf(err) != 401 {
		t.Errorf("the environment token was accepted: %v", err)
	}

	// Stopping removes the file, but only its own.
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve returned %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the state file outlived its server")
	}
	if err := writeState(path, localState{PID: 999999}); err != nil {
		t.Fatal(err)
	}
	removeOwnState(path, os.Getpid())
	if _, err := os.Stat(path); err != nil {
		t.Error("another server's state file was removed")
	}
}

// TestServeLocalExitsWhenIdle: nothing to do for the idle period means
// exit; a request resets the clock.
//
// @decision(DL-002)
func TestServeLocalExitsWhenIdle(t *testing.T) {
	// The RATIO is what this test is about — hits far more frequent
	// than the window, so a scheduling stall cannot fake an idle — and
	// not the absolute values, so the window is short.
	const idle = 250 * time.Millisecond
	st, path, done, _ := localServe(t, sandbox.NewFake(), idle)
	c := client.New(st.URL, st.Token)
	// Keep it busy past one idle period, with hits far more frequent
	// than the window so a scheduling stall cannot fake an idle.
	for i := 0; i < 8; i++ {
		time.Sleep(idle / 5)
		if _, err := c.Me(context.Background()); err != nil {
			t.Fatalf("request %d: %v; the server exited while in use", i, err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the idle server did not exit")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the state file outlived the idle exit")
	}
}

// TestServeLocalStaysUpWhileASessionStarts: a launch outlives the
// request that asked for it, and must not be cut off by the idle exit.
//
// @decision(DL-002)
func TestServeLocalStaysUpWhileASessionStarts(t *testing.T) {
	gate := &gatedProvider{Fake: sandbox.NewFake(), release: make(chan struct{})}
	st, _, done, _ := localServeWith(t, gate, 200*time.Millisecond, defaultConfig)
	c := client.New(st.URL, st.Token)
	req := server.SessionRequest{Name: "slow", Repos: []server.RepoRef{{Platform: "codefloe", Owner: "alice", Name: "spov"}}}
	if _, err := c.CreateSession(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the server exited while a session was starting")
	default:
	}
	close(gate.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not exit once the launch had ended")
	}
}

// gatedProvider holds sandbox creation until released.
type gatedProvider struct {
	*sandbox.Fake
	release chan struct{}
}

func (g *gatedProvider) Create(ctx context.Context, spec sandbox.Spec) (sandbox.Handle, error) {
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.Fake.Create(ctx, spec)
}

// TestServeLocalReconcilesBeforeAnnouncing: a sandbox launched by a
// previous server is already known when the CLI can see the new one.
//
// @decision(DL-002) @decision(DL-004)
func TestServeLocalReconcilesBeforeAnnouncing(t *testing.T) {
	provider := sandbox.NewFake()
	h, err := provider.Create(context.Background(), sandbox.Spec{
		Name: "kept", Image: "img", Command: []string{"true"}, Ports: []int{4096}, CPU: 0.5, MemoryMiB: 2048,
		Timeout: time.Hour, IdleTimeout: time.Minute, Egress: sandbox.EgressOpen(),
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "kept", User: "alice", CreatedAt: time.Now()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _, _, _ := localServe(t, provider, time.Hour)
	c := client.New(st.URL, st.Token)
	page, err := c.ListSessions(context.Background(), client.Page{})
	sessions := page.Sessions
	if err != nil || len(sessions) != 1 || sessions[0].Name != "kept" {
		t.Fatalf("sessions = %+v, %v; the kept sandbox must be known before the state file exists", sessions, err)
	}
	if err := c.DeleteSession(context.Background(), "kept"); err != nil {
		t.Fatal(err)
	}
	if !h.(*sandbox.FakeHandle).WasTerminated() {
		t.Error("down through the new server did not terminate the sandbox")
	}
}

// TestALocalServerRefusesANetworkAddress: --local binds loopback only,
// whatever --listen names.
//
// @decision(DL-064)
func TestALocalServerRefusesANetworkAddress(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:8080", "[::]:0", "example.com:80", "[::1%lo]:0", "garbage"} {
		err := runServe(context.Background(), &Env{}, serveOptions{Local: true, Listen: addr})
		if err == nil || !strings.Contains(err.Error(), "only on loopback") {
			t.Errorf("--local --listen %s: err = %v, want a loopback refusal", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0", "localhost:0"} {
		if !loopbackListen(addr) {
			t.Errorf("loopbackListen(%q) = false, want true", addr)
		}
	}
}
