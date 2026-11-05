package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/version"
)

// localEnv is an Env whose local-server plumbing is stubbed: a state
// directory of its own, a fake pid table, and a spawn that a test
// decides.
func localEnv(t *testing.T) *Env {
	t.Helper()
	return &Env{
		Err:          &strings.Builder{},
		Environ:      config.MapEnv{Vars: map[string]string{}},
		Workdir:      t.TempDir(),
		StateDir:     t.TempDir(),
		SpawnTimeout: 500 * time.Millisecond,
		ProcessAlive: func(int) bool { return false },
		Spawn:        func(context.Context) error { return errors.New("no spawn in this test") },
	}
}

// fakeServer answers /api/v1/me for one token.
func fakeServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me" || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"title":"Unauthorized","status":401}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"user_id":"local","org_id":"local","admin":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *Env) writeTestState(t *testing.T, st localState) string {
	t.Helper()
	path, _ := e.stateFilePath()
	if err := writeState(path, st); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestStateFileIsPrivateAndAtomic: the token lives in the file, so the
// file is what scopes it to this user.
//
// @decision(DL-002) @decision(DL-006)
func TestStateFileIsPrivateAndAtomic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	env := localEnv(t)
	path := env.writeTestState(t, localState{PID: 1, URL: "http://x", Token: "t", Version: "v"})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
	got, err := readState(path)
	if err != nil || got.Token != "t" {
		t.Errorf("readState = %+v, %v", got, err)
	}
	if st, err := readState(filepath.Join(t.TempDir(), "none")); st != nil || err != nil {
		t.Errorf("a missing file = %+v, %v", st, err)
	}
}

// TestLocalClientUsesALiveServer: pid running, version matching, probe
// answering — no spawn.
//
// @decision(DL-002)
func TestLocalClientUsesALiveServer(t *testing.T) {
	env := localEnv(t)
	srv := fakeServer(t, "tok")
	env.ProcessAlive = func(int) bool { return true }
	env.writeTestState(t, localState{PID: 4242, URL: srv.URL, Token: "tok", Version: version.Get().String()})

	c, err := env.client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != srv.URL || c.Token != "tok" {
		t.Errorf("client = %+v", c)
	}
}

// TestLocalClientReplacesAStaleStateFile: a dead pid or a server that
// does not answer means the file is stale; it is removed and a server
// is spawned.
//
// @decision(DL-002)
func TestLocalClientReplacesAStaleStateFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alive bool
		token string
	}{
		{"dead pid", false, "tok"},
		{"wrong token", true, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := localEnv(t)
			srv := fakeServer(t, "tok")
			env.ProcessAlive = func(int) bool { return tc.alive }
			path := env.writeTestState(t, localState{PID: 4242, URL: srv.URL, Token: tc.token, Version: version.Get().String()})
			spawned := false
			env.Spawn = func(context.Context) error {
				spawned = true
				if _, err := os.Stat(path); err == nil {
					t.Error("the stale file was still there when the spawn began")
				}
				return env.writeStateFor(srv.URL, "tok")
			}
			if _, err := env.client(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !spawned {
				t.Error("no server was spawned")
			}
		})
	}
}

// writeStateFor is what a spawned server does once listening, from a
// test's point of view.
func (e *Env) writeStateFor(url, token string) error {
	path, _ := e.stateFilePath()
	e.ProcessAlive = func(int) bool { return true }
	return writeState(path, localState{PID: os.Getpid(), URL: url, Token: token, Version: version.Get().String()})
}

// TestLocalClientStopsAServerOfAnotherBuild: two builds of spov never
// talk to each other; the old server is told to stop and a new one is
// spawned.
//
// @decision(DL-002)
func TestLocalClientStopsAServerOfAnotherBuild(t *testing.T) {
	env := localEnv(t)
	srv := fakeServer(t, "tok")
	env.ProcessAlive = func(int) bool { return true }
	env.writeTestState(t, localState{PID: 4242, URL: srv.URL, Token: "tok", Version: "spov 0.0.1 (deadbeef)"})
	var stopped []int
	env.StopProcess = func(pid int) { stopped = append(stopped, pid) }
	env.Spawn = func(context.Context) error { return env.writeStateFor(srv.URL, "tok") }

	if _, err := env.client(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 1 || stopped[0] != 4242 {
		t.Errorf("stopped = %v, want the old server's pid", stopped)
	}
}

// TestLocalClientReportsASpawnThatNeverAppears: the failure names the
// server log, since the server's terminal is not the user's.
//
// @decision(DL-002)
func TestLocalClientReportsASpawnThatNeverAppears(t *testing.T) {
	env := localEnv(t)
	env.Spawn = func(context.Context) error { return nil }
	_, err := env.client(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not start") || !strings.Contains(err.Error(), serverLogName) {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(err.Error(), env.StateDir) {
		t.Errorf("err %q does not point at the state directory", err)
	}
}

func TestLocalClientStopsWaitingWhenCancelled(t *testing.T) {
	env := localEnv(t)
	env.SpawnTimeout = time.Minute
	env.Spawn = func(context.Context) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := env.client(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

// TestLocalClientTreatsACorruptStateFileAsStale.
func TestLocalClientTreatsACorruptStateFileAsStale(t *testing.T) {
	env := localEnv(t)
	path, _ := env.stateFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := fakeServer(t, "tok")
	env.Spawn = func(context.Context) error { return env.writeStateFor(srv.URL, "tok") }
	if _, err := env.client(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestLocalClientRefusesANonLoopbackStateFile: the file is trusted for
// its token, not for where to send it.
//
// @decision(DL-002) @decision(DL-030)
func TestLocalClientRefusesANonLoopbackStateFile(t *testing.T) {
	for _, bad := range []string{"http://attacker.example:80", "https://127.0.0.1:1", "http://user:pw@127.0.0.1:1", "http://10.0.0.5:8080"} {
		if loopbackURL(bad) {
			t.Errorf("%q accepted as loopback", bad)
		}
	}
	for _, good := range []string{"http://127.0.0.1:41234", "http://[::1]:5", "http://localhost:8080"} {
		if !loopbackURL(good) {
			t.Errorf("%q refused as loopback", good)
		}
	}
	env := localEnv(t)
	evil := fakeServer(t, "tok")
	env.ProcessAlive = func(int) bool { return true }
	// The fake listens on loopback; the file claims otherwise.
	env.writeTestState(t, localState{PID: 4242, URL: strings.Replace(evil.URL, "127.0.0.1", "127.0.0.1.nip.io", 1), Token: "tok", Version: version.Get().String()})
	local := fakeServer(t, "tok")
	env.Spawn = func(context.Context) error { return env.writeStateFor(local.URL, "tok") }
	c, err := env.client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != local.URL {
		t.Errorf("client = %q, want the spawned loopback server, not the file's host", c.BaseURL)
	}
}

// TestSpawnArgsCarryNoSecret: the token is the child's to generate; the
// command line names only the state file.
//
// @decision(DL-002) @decision(DL-006)
func TestSpawnArgsCarryNoSecret(t *testing.T) {
	args := spawnArgs("/state/server.json")
	if strings.Join(args, " ") != "serve --local --state-file /state/server.json" {
		t.Errorf("args = %v", args)
	}
}

// TestStateDirIsMadePrivate: an existing world-writable directory is
// tightened, since anyone who can replace the file owns the token.
//
// @decision(DL-006)
func TestStateDirIsMadePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	dir := filepath.Join(t.TempDir(), "spov")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := writeState(filepath.Join(dir, stateFileName), localState{PID: 1}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o", info.Mode().Perm())
	}
}

// TestLostServerIgnoresACancelledCaller: giving up is not a lost
// server, and must not spawn one on the way out.
func TestLostServerIgnoresACancelledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if lostServer(ctx, errors.New("dial refused")) {
		t.Error("a cancelled context counted as a lost server")
	}
	if !lostServer(context.Background(), errors.New("dial refused")) {
		t.Error("a transport error did not count")
	}
	if lostServer(context.Background(), &client.Error{Status: 404}) {
		t.Error("an answer counted as a lost server")
	}
	env := localEnv(t)
	env.Spawn = func(context.Context) error { t.Fatal("spawned for a cancelled caller"); return nil }
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	env.Client = client.New(gone.URL, "old")
	if err := env.deleteSession(ctx, env.Client, server.SessionView{ID: "x"}); err == nil {
		t.Error("expected the transport error back")
	}
}

// TestConfiguredServerWins: a flag, the environment or config means a
// hosted server; the local one is neither probed nor spawned.
//
// @decision(DL-002)
func TestConfiguredServerWins(t *testing.T) {
	env := localEnv(t)
	env.Environ = config.MapEnv{Vars: map[string]string{ServerEnvVar: "https://hosted.example", ServerTokenEnvVar: "t"}}
	env.Spawn = func(context.Context) error { t.Fatal("spawned despite a configured server"); return nil }
	c, err := env.client(context.Background())
	if err != nil || c.BaseURL != "https://hosted.example" {
		t.Errorf("client = %+v, %v", c, err)
	}
}

// TestStateDirResolution: XDG first, then the home directory.
func TestStateDirResolution(t *testing.T) {
	e := &Env{Environ: config.MapEnv{Vars: map[string]string{"XDG_STATE_HOME": "/xdg"}}}
	if got, _ := e.stateDir(); got != filepath.Join("/xdg", "spov") {
		t.Errorf("stateDir = %q", got)
	}
	e = &Env{Environ: config.MapEnv{Home: "/home/u"}}
	if got, _ := e.stateDir(); got != filepath.Join("/home/u", ".local", "state", "spov") {
		t.Errorf("stateDir = %q", got)
	}
	e = &Env{Environ: config.MapEnv{}}
	if _, err := e.stateDir(); err == nil {
		t.Error("no home and no XDG did not fail")
	}
	if got := e.serverLogPath(); !strings.HasSuffix(got, serverLogName) {
		t.Errorf("serverLogPath = %q", got)
	}
}

// TestServerLogIsTruncatedWhenLarge: a log nobody rotates must not grow
// forever.
func TestServerLogIsTruncatedWhenLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), serverLogName)
	if err := os.WriteFile(path, make([]byte, maxServerLog+1), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openServerLog(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	info, _ := os.Stat(path)
	if info.Size() > 1024 {
		t.Errorf("log is %d bytes after truncation", info.Size())
	}
	// A small log is appended to.
	f, _ = openServerLog(path)
	_ = f.Close()
	if again, _ := os.Stat(path); again.Size() <= info.Size() {
		t.Error("a small log was truncated")
	}
}

// TestSpawnRunsTheRealBinaryDetached builds spov and lets the real
// spawn path start it: the server must detach, write the state file,
// answer with its token, and its stderr must land in the log. The
// provider is Modal in that process, which needs no credential until a
// sandbox is touched, so `ls` against an empty server is enough.
//
// @decision(DL-002)
func TestSpawnRunsTheRealBinaryDetached(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("detaching is unix-only")
	}
	// The spawned server connects to Modal at startup; without a
	// credential it exits, which is itself correct but not this test.
	realHome, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(realHome, ".modal.toml")); err != nil && os.Getenv("MODAL_TOKEN_ID") == "" {
		t.Skip("needs a Modal credential")
	}
	bin := filepath.Join(t.TempDir(), "spov")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/spov")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	stateDir := t.TempDir()
	// The real home for the Modal credential; everything spov itself
	// reads or writes is redirected to temporary directories.
	cmdEnv := []string{"HOME=" + realHome, "PATH=" + os.Getenv("PATH"), "XDG_STATE_HOME=" + stateDir,
		"XDG_CONFIG_HOME=" + t.TempDir(), "XDG_DATA_HOME=" + t.TempDir(),
		"MODAL_TOKEN_ID=" + os.Getenv("MODAL_TOKEN_ID"), "MODAL_TOKEN_SECRET=" + os.Getenv("MODAL_TOKEN_SECRET")}

	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = cmdEnv
		cmd.Dir = t.TempDir()
		var out, errBuf strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errBuf
		err := cmd.Run()
		return out.String(), errBuf.String(), err
	}
	out, stderr, err := run("ls")
	if err != nil {
		t.Fatalf("ls: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, "No spov sessions") {
		t.Errorf("ls = %q", out)
	}
	path := filepath.Join(stateDir, "spov", stateFileName)
	st, err := readState(path)
	if err != nil || st == nil {
		t.Fatalf("state file: %+v, %v", st, err)
	}
	if !processAlive(st.PID) {
		t.Error("the spawned server is not running")
	}
	// @decision(DL-026)
	if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", st.PID)); err == nil && cwd != filepath.Join(stateDir, "spov") {
		t.Errorf("the server runs in %s, want the state directory", cwd)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(st.PID); err == nil {
			_ = p.Kill()
		}
	})
	// A second command reuses it rather than spawning again.
	if _, stderr, err := run("ls"); err != nil {
		t.Fatalf("second ls: %v\n%s", err, stderr)
	}
	if again, _ := readState(path); again == nil || again.PID != st.PID {
		t.Errorf("state = %+v, want the same server reused", again)
	}
	logBytes, err := os.ReadFile(filepath.Join(stateDir, "spov", serverLogName))
	if err != nil || !strings.Contains(string(logBytes), "listening on") {
		t.Errorf("server log = %q, %v", logBytes, err)
	}
	if strings.Contains(string(logBytes), st.Token) {
		t.Error("the token is in the server log")
	}
}

// TestSpawnExecsThisBinary: with nothing substituted, spawn starts this
// executable detached with its stderr in the server log. The test
// binary does not understand `serve --local` and exits at once, which
// is fine: what is checked is the launch, not the server.
func TestSpawnExecsThisBinary(t *testing.T) {
	env := localEnv(t)
	env.Spawn = nil
	if err := env.spawn(context.Background()); err != nil {
		t.Fatal(err)
	}
	logBytes, err := os.ReadFile(filepath.Join(env.StateDir, serverLogName))
	if err != nil || !strings.Contains(string(logBytes), "started") {
		t.Errorf("server log = %q, %v", logBytes, err)
	}
	if info, _ := os.Stat(filepath.Join(env.StateDir, serverLogName)); info.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %o", info.Mode().Perm())
	}
}

func TestProcessAliveAndStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals")
	}
	if !processAlive(os.Getpid()) {
		t.Error("this process is dead")
	}
	if processAlive(1 << 30) {
		t.Error("an impossible pid is alive")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	// Reaped as soon as it exits, as init would reap a detached server;
	// a zombie child would otherwise still answer signal 0.
	go func() { _ = cmd.Wait() }()
	env := &Env{}
	started := time.Now()
	env.stopProcess(cmd.Process.Pid)
	if processAlive(cmd.Process.Pid) {
		t.Error("the process survived stopProcess")
	}
	if time.Since(started) > 2*time.Second {
		t.Errorf("stopProcess took %s for a process that exited at once", time.Since(started))
	}
}

func TestWriteStateReportsAnUnwritableDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeState(filepath.Join(file, stateFileName), localState{}); err == nil {
		t.Error("writing under a file succeeded")
	}
}

// sessionStub is a scripted server for the CLI's reconnection paths.
func sessionStub(t *testing.T, token string, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"user_id":"local","org_id":"local","admin":true}`))
	})
	for p, h := range routes {
		mux.HandleFunc(p, h)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"title":"Unauthorized","status":401}`, http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDeleteSessionSurvivesAReplacedServer: the server that launched
// the session has exited; a new one is found, which knows the session
// under a new ID, so the delete falls back to the name.
//
// @decision(DL-002)
func TestDeleteSessionSurvivesAReplacedServer(t *testing.T) {
	env := localEnv(t)
	var deleted []string
	replacement := sessionStub(t, "new", map[string]http.HandlerFunc{
		"DELETE /api/v1/sessions/{ref}": func(w http.ResponseWriter, r *http.Request) {
			deleted = append(deleted, r.PathValue("ref"))
			if r.PathValue("ref") == "old-id" {
				http.Error(w, `{"title":"Not Found","status":404}`, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		},
	})
	env.Spawn = func(context.Context) error { return env.writeStateFor(replacement.URL, "new") }
	// The client that launched the session points at a server that is
	// gone.
	gone := httptest.NewServer(http.NotFoundHandler())
	env.Client = client.New(gone.URL, "old")
	gone.Close()

	v := server.SessionView{ID: "old-id", Name: "kept"}
	if err := env.deleteSession(context.Background(), env.Client, v); err != nil {
		t.Fatal(err)
	}
	if strings.Join(deleted, " ") != "old-id kept" {
		t.Errorf("deleted = %v, want the ID tried then the name", deleted)
	}
	if env.Client.BaseURL != replacement.URL {
		t.Error("the CLI did not switch to the replacement server")
	}
}

// TestConnectSurvivesAReplacedServer: a mint after the local server has
// exited resolves a new server and mints there, by name.
//
// @decision(DL-002) @decision(DL-030)
func TestConnectSurvivesAReplacedServer(t *testing.T) {
	env := localEnv(t)
	var minted []string
	replacement := sessionStub(t, "new", map[string]http.HandlerFunc{
		"POST /api/v1/sessions/{ref}/connect": func(w http.ResponseWriter, r *http.Request) {
			minted = append(minted, r.PathValue("ref"))
			_, _ = w.Write([]byte(`{"url":"https://edge","token":"t","user":"u","password":"p","idle_timeout_secs":60}`))
		},
	})
	env.Spawn = func(context.Context) error { return env.writeStateFor(replacement.URL, "new") }
	gone := httptest.NewServer(http.NotFoundHandler())
	env.Client = client.New(gone.URL, "old")
	gone.Close()
	env.StartClientProxy = func(ctx context.Context, c session.Connector, _ func(string)) (*proxy.Proxy, error) {
		conn, err := c.Connect(ctx)
		if err != nil {
			return nil, err
		}
		if conn.URL != "https://edge" || conn.IdleTimeout != time.Minute {
			t.Errorf("connection = %+v", conn)
		}
		return nil, errors.New("stop here")
	}
	_, err := env.connectSession(context.Background(), env.Client, server.SessionView{ID: "old-id", Name: "kept"})
	if err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(minted, " ") != "kept" {
		t.Errorf("minted = %v, want one mint by name on the replacement", minted)
	}
}
