package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/version"
)

// localState is what a local server writes once it is listening, and
// what the CLI reads to find it. The token is the local bearer
// credential (DL-006); the file's mode is what keeps it to this user.
//
// @decision(DL-002)
type localState struct {
	PID       int       `json:"pid"`
	URL       string    `json:"url"`
	Token     string    `json:"token"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

const (
	// stateFileName and serverLogName live in the state directory.
	stateFileName = "server.json"
	serverLogName = "server.log"
	// SpawnTimeout bounds the wait for a spawned server's state file.
	//
	// @decision(DL-002)
	SpawnTimeout = 5 * time.Second
	// probeTimeout bounds the check that a state file names a live
	// server.
	probeTimeout = 500 * time.Millisecond
	// stopTimeout bounds the wait for an old server to exit after
	// SIGTERM.
	stopTimeout = 5 * time.Second
	// maxServerLog is the size above which the log is truncated at
	// spawn.
	maxServerLog = 1 << 20
)

// stateDir is where the local server's state file and log live:
// $XDG_STATE_HOME/spov, else ~/.local/state/spov.
//
// @decision(DL-002)
func (e *Env) stateDir() (string, error) {
	if e.StateDir != "" {
		return e.StateDir, nil
	}
	if dir := e.Environ.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "spov"), nil
	}
	home, err := e.Environ.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the state directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "spov"), nil
}

func (e *Env) stateFilePath() (string, error) {
	dir, err := e.stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, stateFileName), nil
}

// serverLogPath is where a spawned server's stderr goes, and what
// local-mode failure messages name.
//
// @decision(DL-002)
func (e *Env) serverLogPath() string {
	dir, err := e.stateDir()
	if err != nil {
		return "~/.local/state/spov/" + serverLogName
	}
	return filepath.Join(dir, serverLogName)
}

// readState reads the state file. A missing file is (nil, nil).
func readState(path string) (*localState, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st localState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}

// writeState writes the state file atomically with mode 0600, so a
// reader never sees a partial file and no other user can read the
// token.
//
// @decision(DL-002) @decision(DL-006)
func writeState(path string, st localState) error {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), stateFileName+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// localClient finds or starts the local server. A state file is live
// only if its process is running and its server answers with the
// token; a server of another build is stopped and replaced.
//
// @decision(DL-002)
func (e *Env) localClient(ctx context.Context) (*client.Client, error) {
	path, err := e.stateFilePath()
	if err != nil {
		return nil, err
	}
	st, err := readState(path)
	if err != nil {
		// Unreadable is treated as stale: the file is this program's own,
		// so a corrupt one is replaced rather than reported.
		st = nil
	}
	if st != nil {
		if c, ok := e.liveLocal(ctx, st); ok {
			return c, nil
		}
		// Stale or foreign. Remove it before spawning so a second CLI
		// cannot pick it up in the meantime.
		_ = os.Remove(path)
	}
	return e.spawnLocal(ctx, path)
}

// liveLocal reports whether the state describes a server this CLI can
// use, stopping one of another build on the way.
//
// @decision(DL-002)
func (e *Env) liveLocal(ctx context.Context, st *localState) (*client.Client, bool) {
	if st.PID <= 0 || st.Token == "" || !loopbackURL(st.URL) || !e.processAlive(st.PID) {
		return nil, false
	}
	if st.Version != version.Get().String() {
		// Two builds of spov never talk to each other. The old server is
		// asked to stop, and given time to, before a new one takes the
		// state file over.
		e.stopProcess(st.PID)
		return nil, false
	}
	c := client.New(st.URL, st.Token)
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if _, err := c.Me(probeCtx); err != nil {
		return nil, false
	}
	return c, true
}

// loopbackListen reports whether a listen address binds only loopback. An
// empty host (":8080") binds every interface.
//
// @decision(DL-064)
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && loopbackHost(host)
}

// loopbackHost is the one rule both sides use: the host the local server
// binds and the host the CLI will trust in its state file. An empty host
// binds every interface.
//
// @decision(DL-002) @decision(DL-064)
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loopbackURL reports whether a URL names this machine. The state file
// is trusted for its token, not for where to send it: a file another
// process managed to write must not be able to point the CLI, and the
// token with it, at a host of its choosing.
//
// @decision(DL-002) @decision(DL-030)
func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return false
	}
	return loopbackHost(u.Hostname())
}

// spawnLocal starts `spov serve --local` and waits for its state file.
//
// @decision(DL-002)
func (e *Env) spawnLocal(ctx context.Context, path string) (*client.Client, error) {
	// A local server cannot start without Modal, and its failure would
	// surface here only as a timeout that names a log file.
	//
	// @decision(DL-125)
	if e.Spawn == nil && e.Provider == nil && e.NewProvider == nil {
		// Only when there are none at all: anything else is the Modal
		// SDK's to judge, which merges the environment and its file.
		if _, err := credentials.ResolveModal(e.Environ.Getenv, e.Environ.UserHomeDir); errors.Is(err, credentials.ErrNoModalCredentials) {
			return nil, fmt.Errorf("%s; `spov init` walks you through the whole setup",
				strings.TrimPrefix(err.Error(), "credentials: "))
		}
	}
	if err := e.spawn(ctx); err != nil {
		return nil, fmt.Errorf("starting the local spov server: %w", err)
	}
	deadline := time.Now().Add(e.spawnTimeout())
	for {
		st, err := readState(path)
		if err == nil && st != nil {
			if c, ok := e.liveLocal(ctx, st); ok {
				return c, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the local spov server did not start within %s; see %s", e.spawnTimeout(), e.serverLogPath())
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (e *Env) spawnTimeout() time.Duration {
	if e.SpawnTimeout > 0 {
		return e.SpawnTimeout
	}
	return SpawnTimeout
}

// spawn runs the server as a detached child of this process. Its stderr
// goes to the server log, since its terminal is about to belong to
// OpenCode; nothing else is inherited.
//
// @decision(DL-002)
func (e *Env) spawn(ctx context.Context) error {
	if e.Spawn != nil {
		return e.Spawn(ctx)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := e.stateDir()
	if err != nil {
		return err
	}
	if err := privateDir(dir); err != nil {
		return err
	}
	logFile, err := openServerLog(filepath.Join(dir, serverLogName))
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(self, spawnArgs(filepath.Join(dir, stateFileName))...)
	// Not the clone the CLI runs in: the server outlives it and serves
	// every directory.
	//
	// @decision(DL-026)
	cmd.Dir = dir
	cmd.Env = e.Environ.Environ()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = logFile
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped by init once detached; nothing here waits for it.
	go func() { _ = cmd.Wait() }()
	return nil
}

// spawnArgs is the child's command line. The state file is named
// explicitly so the child writes where this CLI will look; nothing
// secret is here, since the token is the child's to generate.
//
// @decision(DL-002)
func spawnArgs(statePath string) []string {
	return []string{"serve", "--local", "--state-file", statePath}
}

// privateDir creates the state directory and makes sure it is this
// user's alone, since an existing directory keeps whatever mode it had
// and a writable one would let another user replace the state file.
//
// @decision(DL-006)
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// openServerLog opens the log for appending, truncating it first when
// it has grown past maxServerLog, so a long-lived install does not
// accumulate a log nobody rotates.
func openServerLog(path string) (*os.File, error) {
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if info, err := os.Stat(path); err == nil && info.Size() > maxServerLog {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(f, "--- spov serve --local started %s by pid %d\n", time.Now().UTC().Format(time.RFC3339), os.Getpid())
	return f, nil
}

func (e *Env) processAlive(pid int) bool {
	if e.ProcessAlive != nil {
		return e.ProcessAlive(pid)
	}
	return processAlive(pid)
}

// stopProcess asks a server to exit and waits for it, bounded. Whether
// it actually died is not checked: the old server keeps its own port
// either way, and the new one binds another.
func (e *Env) stopProcess(pid int) {
	if e.StopProcess != nil {
		e.StopProcess(pid)
		return
	}
	terminateProcess(pid)
	deadline := time.Now().Add(stopTimeout)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}
