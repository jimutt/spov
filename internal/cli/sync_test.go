package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
)

func initCLIGitRepo(t *testing.T, dir, remoteURL string) string {
	t.Helper()
	runCLIGit(t, dir, "init", "-b", "main")
	runCLIGit(t, dir, "config", "user.name", "Test User")
	runCLIGit(t, dir, "config", "user.email", "test@example.com")
	if remoteURL != "" {
		runCLIGit(t, dir, "remote", "add", "origin", remoteURL)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# spov\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLIGit(t, dir, "add", "README.md")
	runCLIGit(t, dir, "commit", "-m", "initial")
	return strings.TrimSpace(runCLIGit(t, dir, "rev-parse", "HEAD"))
}

func runCLIGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func cliWorktreeTar(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// @decision(DL-027)
func TestFlySyncSeedsAndSyncsBackBeforeTerminating(t *testing.T) {
	f := newAPIFixtureWithConfig(t, platformOnlyConfig)
	baseHead := initCLIGitRepo(t, f.wd, "git@codefloe.com:alice/spov.git")

	// Simulate sandbox changes: a new commit and an uncommitted working-tree file.
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

	// During the TUI run, wire the fake sandbox handle's export response.
	f.env.RunTUI = func(_ context.Context, tui opencode.TUI) (int, error) {
		f.tuiRuns++
		f.tuiSeen = tui
		for _, h := range f.provider.Handles() {
			// Register the export output for any spov-export Exec call.
			outJSON, _ := json.Marshal(map[string]any{
				"branch":       "main",
				"base_commit":  baseHead,
				"head_commit":  sandboxHead,
				"bundle":       base64.StdEncoding.EncodeToString(bundleBytes),
				"worktree_tar": cliWorktreeTar(t, map[string]string{"wip.txt": "uncommitted edit\n"}),
			})
			// Match by pre-populating ExecResults after probing or setting a wildcard:
			// since FakeHandle looks up strings.Join(command, " "), compute the exact command ExportSync uses.
			_, _ = sessionExportProbe(h, "spov", baseHead, string(outJSON), f.provider)
		}
		return 0, nil
	}

	_, stderr, err := runWith(t, f.env, "fly", "--sync", "--name", "sync-run")
	if err != nil {
		t.Fatalf("fly --sync: %v\nstderr:\n%s", err, stderr)
	}
	if f.provider.Running() != 0 {
		t.Errorf("expected sandbox to be terminated after successful sync, running=%d", f.provider.Running())
	}
	if !strings.Contains(stderr, "Synced to main (1 commit, 1 uncommitted file).") {
		t.Errorf("stderr did not report sync summary:\n%s", stderr)
	}
	gotHead := strings.TrimSpace(runCLIGit(t, f.wd, "rev-parse", "HEAD"))
	if gotHead != sandboxHead {
		t.Errorf("local HEAD = %s, want %s", gotHead, sandboxHead)
	}
	if b, err := os.ReadFile(filepath.Join(f.wd, "agent.txt")); err != nil || string(b) != "committed by agent\n" {
		t.Errorf("agent.txt = %q, err = %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(f.wd, "wip.txt")); err != nil || string(b) != "uncommitted edit\n" {
		t.Errorf("wip.txt = %q, err = %v", b, err)
	}
}

func sessionExportProbe(h *sandbox.FakeHandle, dir, base, stdout string, p *sandbox.Fake) (string, error) {
	before := len(h.ExecLog())
	_, _ = session.ExportSync(context.Background(), h, dir, base)
	execs := h.ExecLog()
	if len(execs) <= before {
		return "", errors.New("no exec recorded")
	}
	key := strings.Join(execs[len(execs)-1], " ")
	p.ExecResults[key] = sandbox.ExecResult{ExitCode: 0, Stdout: stdout}
	return key, nil
}

// @decision(DL-027)
func TestFlySyncFailureLeavesSandboxRunning(t *testing.T) {
	f := newAPIFixtureWithConfig(t, platformOnlyConfig)
	_ = initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")

	f.env.RunTUI = func(_ context.Context, _ opencode.TUI) (int, error) {
		f.provider.ExecErr = errors.New("sandbox exec failed")
		return 0, nil
	}

	_, stderr, err := runWith(t, f.env, "fly", "--sync", "--name", "keep-alive")
	if err == nil {
		t.Fatal("expected an error when sync export fails")
	}
	if f.provider.Running() != 1 {
		t.Fatalf("sandbox was terminated despite sync failure (running=%d)", f.provider.Running())
	}
	for _, want := range []string{
		"The sandbox was NOT terminated",
		"spov stop keep-alive --sync",
		"spov stop keep-alive",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

// @decision(DL-027)
func TestAttachAndStopSync(t *testing.T) {
	f := newAPIFixtureWithConfig(t, platformOnlyConfig)
	baseHead := initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")

	_, stderr, err := runWith(t, f.env, "fly", "--sync", "--no-attach", "--name", "kept-sync")
	if err != nil {
		t.Fatalf("fly --sync --no-attach: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "spov attach kept-sync --sync") || !strings.Contains(stderr, "spov stop kept-sync --sync") {
		t.Errorf("expected --sync hints in stderr:\n%s", stderr)
	}

	outJSON, _ := json.Marshal(map[string]any{
		"branch":       "main",
		"base_commit":  baseHead,
		"head_commit":  baseHead,
		"worktree_tar": cliWorktreeTar(t, map[string]string{"from-attach.txt": "hello from attach\n"}),
	})
	for _, h := range f.provider.Handles() {
		_, _ = sessionExportProbe(h, "spov", baseHead, string(outJSON), f.provider)
	}

	_, stderr, err = runWith(t, f.env, "attach", "kept-sync", "--no-tui", "--sync")
	if err != nil {
		t.Fatalf("attach --sync: %v\n%s", err, stderr)
	}
	if f.provider.Running() != 1 {
		t.Errorf("attach --sync must leave sandbox running, running=%d", f.provider.Running())
	}
	if b, err := os.ReadFile(filepath.Join(f.wd, "from-attach.txt")); err != nil || string(b) != "hello from attach\n" {
		t.Errorf("from-attach.txt = %q, err = %v", b, err)
	}

	// Clean up the file and test `spov stop kept-sync --sync`.
	_ = os.Remove(filepath.Join(f.wd, "from-attach.txt"))
	out, stderr, err := runWith(t, f.env, "stop", "kept-sync", "--sync")
	if err != nil {
		t.Fatalf("stop --sync: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, "Stopped kept-sync.") {
		t.Errorf("out = %q", out)
	}
	if f.provider.Running() != 0 {
		t.Errorf("expected sandbox to be stopped, running=%d", f.provider.Running())
	}
	if b, err := os.ReadFile(filepath.Join(f.wd, "from-attach.txt")); err != nil || string(b) != "hello from attach\n" {
		t.Errorf("from-attach.txt after stop --sync = %q, err = %v", b, err)
	}
}

// @decision(DL-027)
func TestSyncConfigAndFlagPrecedenceAndRefusals(t *testing.T) {
	f := newAPIFixtureWithConfig(t, platformOnlyConfig+`
sandbox:
  sync: true
repos:
  - platform: codefloe
    owner: alice
    name: spov
`)

	t.Run("sandbox.sync in config requires a git checkout unless --no-sync is given", func(t *testing.T) {
		if _, _, err := runWith(t, f.env, "fly"); err == nil || !strings.Contains(err.Error(), "not in a git repository") {
			t.Fatalf("expected git repo error when sandbox.sync=true, got %v", err)
		}
		if _, _, err := runWith(t, f.env, "fly", "--no-sync", "--name", "nosync-ok"); err != nil {
			t.Fatalf("fly --no-sync failed: %v", err)
		}
	})

	t.Run("stop --all --sync is refused", func(t *testing.T) {
		if _, _, err := runWith(t, f.env, "stop", "--all", "--sync"); err == nil || !strings.Contains(err.Error(), "--sync cannot be used with --all") {
			t.Fatalf("expected --all --sync refusal, got %v", err)
		}
	})

	t.Run("repo flag mismatch and multiple repos are refused under --sync", func(t *testing.T) {
		_ = initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
		if _, _, err := runWith(t, f.env, "fly", "--sync", "--repo", "a/b", "--repo", "c/d"); err == nil || !strings.Contains(err.Error(), "single repository") {
			t.Fatalf("expected multi --repo refusal, got %v", err)
		}
		if _, _, err := runWith(t, f.env, "fly", "--sync", "--repo", "alice/other"); err == nil || !strings.Contains(err.Error(), "does not match --repo") {
			t.Fatalf("expected mismatched --repo refusal, got %v", err)
		}
		if _, _, err := runWith(t, f.env, "fly", "--sync", "--no-attach", "--repo", "codefloe:alice/spov", "--name", "explicit-match"); err != nil {
			t.Fatalf("expected matching --repo to succeed, got %v", err)
		}
	})

	t.Run("stop --sync on already stopped session fails without keeping-alive hint", func(t *testing.T) {
		if _, _, err := runWith(t, f.env, "stop", "explicit-match"); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := runWith(t, f.env, "stop", "explicit-match", "--sync")
		if err == nil {
			t.Fatal("expected stopped session error")
		}
		if strings.Contains(stderr, "The sandbox was NOT terminated") {
			t.Errorf("unexpected keep-alive hint on stopped session:\n%s", stderr)
		}
	})

	t.Run("local-only repo without remote or credentials seeds from full bundle and syncs back", func(t *testing.T) {
		fLocal := newAPIFixtureWithConfig(t, "")
		localRepo := filepath.Join(t.TempDir(), "my-local-repo")
		if err := os.MkdirAll(localRepo, 0o755); err != nil {
			t.Fatal(err)
		}
		baseHead := initCLIGitRepo(t, localRepo, "")
		fLocal.env.Workdir = localRepo

		outJSON, _ := json.Marshal(map[string]any{
			"branch":       "main",
			"base_commit":  baseHead,
			"head_commit":  baseHead,
			"worktree_tar": cliWorktreeTar(t, map[string]string{"local-only.txt": "worked without remote\n"}),
		})
		fLocal.env.RunTUI = func(_ context.Context, _ opencode.TUI) (int, error) {
			for _, h := range fLocal.provider.Handles() {
				_, _ = sessionExportProbe(h, "my-local-repo", baseHead, string(outJSON), fLocal.provider)
			}
			return 0, nil
		}

		_, stderr, err := runWith(t, fLocal.env, "fly", "--sync", "--name", "local-only-sync")
		if err != nil {
			t.Fatalf("fly --sync on local-only repo: %v\n%s", err, stderr)
		}
		if b, err := os.ReadFile(filepath.Join(localRepo, "local-only.txt")); err != nil || string(b) != "worked without remote\n" {
			t.Errorf("local-only.txt = %q, err = %v", b, err)
		}
	})

	t.Run("multi-platform host disambiguates by account or repo", func(t *testing.T) {
		f2 := newAPIFixtureWithConfig(t, `platforms:
  - name: cf-other
    kind: forgejo
    base_url: https://codefloe.com
    account: someone
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
`)
		_ = initCLIGitRepo(t, f2.wd, "https://codefloe.com/alice/spov.git")
		if _, _, err := runWith(t, f2.env, "fly", "--sync", "--no-attach", "--name", "disambig"); err != nil {
			t.Fatalf("expected account disambiguation to succeed, got %v", err)
		}
	})
}
