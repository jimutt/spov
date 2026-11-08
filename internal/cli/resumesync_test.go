package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// syncedResume is a server that keeps its sessions, with a session launched
// synced from a checkout (the fixture's working directory) and stopped, and
// the sandbox's work ready to be exported when the resumed TUI exits.
type syncedResume struct {
	*apiFixture
	baseHead    string
	sandboxHead string
	// export cans the export of the sandbox's work for every handle; a
	// test runs it from the TUI, as the work happens there.
	export func()
}

func syncedResumeFixture(t *testing.T) *syncedResume {
	t.Helper()
	f := newAPIFixtureWithConfig(t, platformOnlyConfig)
	f.http.Close()
	f.srv.Close()
	f.adjustDeps = func(d *server.Deps) { d.Persistent = true }
	f.startServer()
	f.env.Client = client.New(f.http.URL, f.token)
	// With the platform's token at hand the launch names the platform, as a
	// real checkout of a hosted repository does; without it the session is
	// local-only and cannot be resumed away from its checkout.
	f.env.Credentials = f.creds
	baseHead := initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")

	if _, stderr, err := runWith(t, f.env, "fly", "--sync", "--name", "work", "--keep"); err != nil {
		t.Fatalf("fly --sync: %v\n%s", err, stderr)
	}
	if _, stderr, err := runWith(t, f.env, "stop", "work"); err != nil {
		t.Fatalf("stop: %v\n%s", err, stderr)
	}

	sandboxClone := filepath.Join(t.TempDir(), "sandbox")
	runCLIGit(t, f.wd, "clone", f.wd, sandboxClone)
	runCLIGit(t, sandboxClone, "config", "user.name", "Sandbox Agent")
	runCLIGit(t, sandboxClone, "config", "user.email", "agent@example.com")
	if err := os.WriteFile(filepath.Join(sandboxClone, "agent.txt"), []byte("committed by agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLIGit(t, sandboxClone, "add", "agent.txt")
	runCLIGit(t, sandboxClone, "commit", "-m", "agent commit")
	sandboxHead := strings.TrimSpace(runCLIGit(t, sandboxClone, "rev-parse", "HEAD"))
	bundlePath := filepath.Join(t.TempDir(), "agent.bundle")
	runCLIGit(t, sandboxClone, "bundle", "create", bundlePath, baseHead+"..HEAD")
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}

	r := &syncedResume{apiFixture: f, baseHead: baseHead, sandboxHead: sandboxHead}
	r.export = func() {
		outJSON, _ := json.Marshal(map[string]any{
			"branch":       "main",
			"base_commit":  baseHead,
			"head_commit":  sandboxHead,
			"bundle":       base64.StdEncoding.EncodeToString(bundleBytes),
			"worktree_tar": cliWorktreeTar(t, map[string]string{"wip.txt": "uncommitted edit\n"}),
		})
		for _, h := range f.provider.Handles() {
			_, _ = sessionExportProbe(h, "spov", baseHead, string(outJSON), f.provider)
		}
	}
	f.env.RunTUI = func(context.Context, opencode.TUI) (int, error) {
		f.tuiRuns++
		r.export()
		return 0, nil
	}
	return r
}

func (r *syncedResume) head() string {
	r.t.Helper()
	return strings.TrimSpace(runCLIGit(r.t, r.wd, "rev-parse", "HEAD"))
}

// Leaving the TUI of a resumed synced session, in its checkout, applies
// the sandbox's work to the checkout and only then stops the session.
//
// @decision(DL-104)
func TestResumingASyncedSessionSyncsItBackOnExit(t *testing.T) {
	r := syncedResumeFixture(t)
	_, stderr, err := runWith(t, r.env, "resume", "work", "--yes")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, stderr)
	}
	if r.state("work") != store.StateStopped {
		t.Errorf("the session is %s, want stopped after the sync", r.state("work"))
	}
	if !strings.Contains(stderr, "Synced to main (1 commit, 1 uncommitted file).") {
		t.Errorf("stderr did not report the sync:\n%s", stderr)
	}
	if got := r.head(); got != r.sandboxHead {
		t.Errorf("local HEAD = %s, want %s", got, r.sandboxHead)
	}
	if b, err := os.ReadFile(filepath.Join(r.wd, "agent.txt")); err != nil || string(b) != "committed by agent\n" {
		t.Errorf("agent.txt = %q, err = %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(r.wd, "wip.txt")); err != nil || string(b) != "uncommitted edit\n" {
		t.Errorf("wip.txt = %q, err = %v", b, err)
	}
}

// Outside the session's checkout there is nowhere to put the work, so the
// session is left running with the commands that bring it back, and the
// person is told before the TUI opens.
//
// @decision(DL-104)
func TestResumingASyncedSessionElsewhereKeepsItRunning(t *testing.T) {
	r := syncedResumeFixture(t)
	r.env.Workdir = t.TempDir()
	_, stderr, err := runWith(t, r.env, "resume", "work", "--yes")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, stderr)
	}
	if r.state("work") != store.StateRunning {
		t.Errorf("the session is %s, want running", r.state("work"))
	}
	for _, want := range []string{
		"leaving the TUI will keep the session running",
		"The sandbox is still running.",
		"spov attach work --sync",
		"spov stop work --sync",
		"discard: spov stop work",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "Syncing") {
		t.Errorf("a sync was attempted outside the checkout:\n%s", stderr)
	}
	if got := r.head(); got != r.baseHead {
		t.Errorf("local HEAD moved to %s", got)
	}
}

// --no-sync stops the session on exit and leaves the checkout alone.
//
// @decision(DL-104)
func TestResumeNoSyncTerminatesWithoutSyncing(t *testing.T) {
	r := syncedResumeFixture(t)
	_, stderr, err := runWith(t, r.env, "resume", "work", "--yes", "--no-sync")
	if err != nil {
		t.Fatalf("resume --no-sync: %v\n%s", err, stderr)
	}
	if r.state("work") != store.StateStopped {
		t.Errorf("the session is %s, want stopped", r.state("work"))
	}
	if strings.Contains(stderr, "Sync") || r.head() != r.baseHead {
		t.Errorf("--no-sync synced:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(r.wd, "agent.txt")); err == nil {
		t.Error("the sandbox's file reached the checkout")
	}
	if _, _, err := runWith(t, r.env, "resume", "work", "--sync", "--no-sync"); err == nil {
		t.Error("--sync and --no-sync were accepted together")
	}
}

// --sync fails fast: outside the checkout, or on a session that was not
// synced, nothing is resumed.
//
// @decision(DL-104)
func TestResumeSyncIsRefusedOutsideTheCheckoutAndOnAnUnsyncedSession(t *testing.T) {
	r := syncedResumeFixture(t)
	runs := r.tuiRuns
	r.env.Workdir = t.TempDir()
	if _, _, err := runWith(t, r.env, "resume", "work", "--yes", "--sync"); err == nil || !strings.Contains(err.Error(), "--sync needs the session's checkout") {
		t.Errorf("outside the checkout: %v", err)
	}
	if r.state("work") != store.StateStopped || r.tuiRuns != runs {
		t.Errorf("a refused --sync resumed the session: %s, tui runs %d", r.state("work"), r.tuiRuns-runs)
	}

	u := resumeFixture(t)
	runs = u.tuiRuns
	if _, _, err := runWith(t, u.env, "resume", "work", "--yes", "--sync"); err == nil || !strings.Contains(err.Error(), "was not started from a checkout") {
		t.Errorf("an unsynced session: %v", err)
	}
	if u.state("work") != store.StateStopped || u.tuiRuns != runs {
		t.Errorf("a refused --sync resumed the session: %s, tui runs %d", u.state("work"), u.tuiRuns-runs)
	}
}

// A sync that fails leaves the session running, with the retry hints.
//
// @decision(DL-104) @decision(DL-027)
func TestAFailedSyncOnResumeLeavesTheSessionRunning(t *testing.T) {
	r := syncedResumeFixture(t)
	r.env.RunTUI = func(context.Context, opencode.TUI) (int, error) {
		r.provider.ExecErr = errors.New("sandbox exec failed")
		return 0, nil
	}
	_, stderr, err := runWith(t, r.env, "resume", "work", "--yes")
	if err == nil {
		t.Fatal("expected an error when the export fails")
	}
	if r.state("work") != store.StateRunning {
		t.Errorf("the session is %s, want running", r.state("work"))
	}
	for _, want := range []string{"The sandbox was NOT terminated", "spov stop work --sync", "spov attach work --sync", "spov stop work"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

// A session that was not synced is stopped on exit with nothing synced,
// even when resumed from a checkout.
//
// @decision(DL-104)
func TestResumingAnUnsyncedSessionTerminatesAsBefore(t *testing.T) {
	f := resumeFixture(t)
	initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
	_, stderr, err := runWith(t, f.env, "resume", "work", "--yes")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, stderr)
	}
	if f.state("work") != store.StateStopped {
		t.Errorf("the session is %s, want stopped", f.state("work"))
	}
	for _, unwanted := range []string{"Sync", "keep the session running", "still running"} {
		if strings.Contains(stderr, unwanted) {
			t.Errorf("stderr mentions %q for an unsynced session:\n%s", unwanted, stderr)
		}
	}
}
