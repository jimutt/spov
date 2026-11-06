package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
)

// @decision(DL-027)
func TestLaunchAppliesSyncSeedWithBundleOnStdin(t *testing.T) {
	f := newFixture(t)
	sha := strings.Repeat("a", 40)
	bundle := []byte("BUNDLE-BYTES-NOT-IN-ARGV")
	f.opts.Sync = &SyncSeed{
		Branch:     "feat/local-sync",
		BaseCommit: sha,
		Bundle:     bundle,
	}

	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	h := sess.Handle.(*sandbox.FakeHandle)
	execs := h.ExecLog()
	inputs := h.ExecInputs()
	if len(execs) != 1 || len(inputs) != 1 {
		t.Fatalf("execs=%d inputs=%d, want 1", len(execs), len(inputs))
	}
	if string(inputs[0]) != string(bundle) {
		t.Errorf("stdin = %q, want bundle", inputs[0])
	}
	joined := strings.Join(execs[0], " ")
	if strings.Contains(joined, "BUNDLE-BYTES-NOT-IN-ARGV") {
		t.Error("bundle bytes leaked into argv")
	}
	if !strings.Contains(joined, "feat/local-sync") || !strings.Contains(joined, sha) {
		t.Errorf("exec command did not carry branch and base commit: %q", joined)
	}
}

// A session resumed from its snapshot is not seeded again: the snapshot
// holds the branch as the session left it, the sandbox's own commits
// included.
//
// @decision(DL-027) @decision(DL-088)
func TestASnapshotResumeIsNotSeededAgain(t *testing.T) {
	f := newFixture(t)
	f.opts.Sync = &SyncSeed{Branch: "main", BaseCommit: strings.Repeat("a", 40)}
	f.opts.Resume = opencode.ResumeSnapshot
	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if execs := sess.Handle.(*sandbox.FakeHandle).ExecLog(); len(execs) != 0 {
		t.Errorf("a snapshot resume was seeded: %q", execs)
	}
}

// @decision(DL-027)
func TestExportSyncDecodesSandboxOutput(t *testing.T) {
	f := newFixture(t)
	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.Repeat("1", 40)
	head := strings.Repeat("2", 40)
	payload, _ := json.Marshal(map[string]any{
		"branch":       "feat/local-sync",
		"base_commit":  base,
		"head_commit":  head,
		"bundle":       base64.StdEncoding.EncodeToString([]byte("bundle-data")),
		"worktree_tar": base64.StdEncoding.EncodeToString([]byte("tar-data")),
		"deleted":      []string{"old.txt"},
	})

	// Record canned output for any Exec call by matching the exact command.
	h := sess.Handle.(*sandbox.FakeHandle)
	// Prime by calling once to capture the command key, then set ExecResults.
	_, _ = ExportSync(context.Background(), h, "app", base)
	cmdKey := strings.Join(h.ExecLog()[0], " ")
	f.provider.ExecResults[cmdKey] = sandbox.ExecResult{ExitCode: 0, Stdout: string(payload)}

	got, err := ExportSync(context.Background(), h, "app", base)
	if err != nil {
		t.Fatalf("ExportSync: %v", err)
	}
	if got.Branch != "feat/local-sync" || got.BaseCommit != base || got.HeadCommit != head {
		t.Errorf("got = %+v", got)
	}
	if string(got.Bundle) != "bundle-data" || string(got.WorktreeTar) != "tar-data" {
		t.Errorf("bundle=%q tar=%q", got.Bundle, got.WorktreeTar)
	}
	if len(got.Deleted) != 1 || got.Deleted[0] != "old.txt" {
		t.Errorf("deleted = %v", got.Deleted)
	}

	// Error cases for applySyncSeed and ExportSync.
	if err := applySyncSeed(context.Background(), h, "app", SyncSeed{Branch: "bad..branch", BaseCommit: base}); err == nil {
		t.Error("expected invalid branch to fail")
	}
	if err := applySyncSeed(context.Background(), h, "app", SyncSeed{Branch: "main", BaseCommit: "not-a-sha"}); err == nil {
		t.Error("expected invalid base commit to fail")
	}
	if _, err := ExportSync(context.Background(), h, "app", "not-a-sha"); err == nil {
		t.Error("expected invalid base commit in ExportSync to fail")
	}
}

// LocalShellExec is a sandbox.Handle whose Exec and ExecInput run
// commands on the local machine, for end-to-end tests of sandbox
// scripts without Modal.
type LocalShellExec struct {
	sandbox.Handle
}

func (LocalShellExec) Exec(ctx context.Context, command []string) (sandbox.ExecResult, error) {
	return (LocalShellExec{}).ExecInput(ctx, command, nil)
}

func (LocalShellExec) ExecInput(ctx context.Context, command []string, stdin []byte) (sandbox.ExecResult, error) {
	if len(command) == 0 {
		return sandbox.ExecResult{}, fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else {
			return sandbox.ExecResult{}, err
		}
	}
	return sandbox.ExecResult{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// @decision(DL-027)
func TestSyncSeedAndExportScriptsEndToEnd(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// 1. Set up a bare "remote" repository and a local checkout.
	remoteDir := filepath.Join(tmp, "remote.git")
	runSyncGit(t, tmp, "init", "--bare", "-b", "main", remoteDir)

	localDir := filepath.Join(tmp, "local")
	runSyncGit(t, tmp, "clone", remoteDir, localDir)
	runSyncGit(t, localDir, "config", "user.name", "Local Dev")
	runSyncGit(t, localDir, "config", "user.email", "dev@example.com")
	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "remove-me.txt"), []byte("bye\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSyncGit(t, localDir, "add", "README.md", "remove-me.txt")
	runSyncGit(t, localDir, "commit", "-m", "initial on main")
	runSyncGit(t, localDir, "push", "-u", "origin", "main")

	// 2. Create a local-only branch with an unpushed commit.
	runSyncGit(t, localDir, "checkout", "-b", "feat/e2e-sync")
	if err := os.WriteFile(filepath.Join(localDir, "local-feature.txt"), []byte("unpushed local work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSyncGit(t, localDir, "add", "local-feature.txt")
	runSyncGit(t, localDir, "commit", "-m", "local unpushed commit")

	// Set a forge-shaped remote URL so InspectLocalCheckout parses it, while keeping origin/main tracking ref.
	runSyncGit(t, localDir, "remote", "set-url", "origin", "https://codefloe.com/acme/widgets.git")
	checkout, err := git.InspectLocalCheckout(ctx, localDir, map[string]string{"codefloe.com": git.KindForgejo}, true)
	if err != nil {
		t.Fatalf("InspectLocalCheckout: %v", err)
	}
	if len(checkout.Bundle) == 0 {
		t.Fatal("expected unpushed commit to be bundled")
	}

	// 3. Simulate the sandbox's shallow clone of the remote's default branch.
	wsRoot := filepath.Join(tmp, "workspace")
	sandboxRepo := filepath.Join(wsRoot, "spov")
	runSyncGit(t, tmp, "clone", "--depth", "1", "file://"+remoteDir, sandboxRepo)
	runSyncGit(t, sandboxRepo, "config", "user.name", "Sandbox Agent")
	runSyncGit(t, sandboxRepo, "config", "user.email", "agent@example.com")

	// Run applySyncSeed against the real sandboxRepo by passing the relative path from "/workspace".
	relFromWorkspace, err := filepath.Rel("/workspace", sandboxRepo)
	if err != nil {
		t.Fatal(err)
	}
	if err := applySyncSeed(ctx, LocalShellExec{}, relFromWorkspace, SyncSeed{
		Branch:     checkout.Branch,
		BaseCommit: checkout.HeadCommit,
		Bundle:     checkout.Bundle,
	}); err != nil {
		t.Fatalf("applySyncSeed: %v", err)
	}

	// Verify the sandbox is now on feat/e2e-sync with the unpushed local file.
	gotSandboxBranch := strings.TrimSpace(runSyncGit(t, sandboxRepo, "symbolic-ref", "--short", "HEAD"))
	if gotSandboxBranch != "feat/e2e-sync" {
		t.Errorf("sandbox branch = %q, want feat/e2e-sync", gotSandboxBranch)
	}
	if b, err := os.ReadFile(filepath.Join(sandboxRepo, "local-feature.txt")); err != nil || string(b) != "unpushed local work\n" {
		t.Fatalf("sandbox missing unpushed local file: %v", err)
	}

	// 4. Make a commit, an uncommitted edit, an untracked file, and a deletion inside the sandbox.
	if err := os.WriteFile(filepath.Join(sandboxRepo, "sandbox-committed.txt"), []byte("committed in sandbox\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSyncGit(t, sandboxRepo, "add", "sandbox-committed.txt")
	runSyncGit(t, sandboxRepo, "commit", "-m", "sandbox commit")
	if err := os.WriteFile(filepath.Join(sandboxRepo, "local-feature.txt"), []byte("unpushed local work + uncommitted edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sandboxRepo, "new-untracked.txt"), []byte("brand new untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sandboxRepo, "newdir", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sandboxRepo, "newdir", "sub", "nested-untracked.txt"), []byte("nested untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sandboxRepo, "remove-me.txt")); err != nil {
		t.Fatal(err)
	}

	// 5. Run the real ExportSync script against the sandbox repo and apply it back to localDir.
	exported, err := ExportSync(ctx, LocalShellExec{}, relFromWorkspace, checkout.HeadCommit)
	if err != nil {
		t.Fatalf("ExportSync: %v", err)
	}
	syncRes, err := git.ApplySync(ctx, localDir, "spov-e2e", checkout.Branch, exported)
	if err != nil {
		t.Fatalf("ApplySync: %v", err)
	}
	if !syncRes.InPlace || syncRes.Commits != 1 || syncRes.FilesChanged != 4 {
		t.Errorf("unexpected SyncResult: %+v", syncRes)
	}
	if b, err := os.ReadFile(filepath.Join(localDir, "sandbox-committed.txt")); err != nil || string(b) != "committed in sandbox\n" {
		t.Errorf("sandbox-committed.txt = %q, err = %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(localDir, "local-feature.txt")); err != nil || string(b) != "unpushed local work + uncommitted edit\n" {
		t.Errorf("local-feature.txt = %q, err = %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(localDir, "new-untracked.txt")); err != nil || string(b) != "brand new untracked\n" {
		t.Errorf("new-untracked.txt = %q, err = %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(localDir, "newdir", "sub", "nested-untracked.txt")); err != nil || string(b) != "nested untracked\n" {
		t.Errorf("nested-untracked.txt = %q, err = %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "remove-me.txt")); !os.IsNotExist(err) {
		t.Errorf("remove-me.txt still exists: %v", err)
	}

	// 6. Also verify bundle-only initialization into a directory that has no remote clone yet.
	bundleOnlyRepo := filepath.Join(wsRoot, "bundle-only")
	relBundleOnly, err := filepath.Rel("/workspace", bundleOnlyRepo)
	if err != nil {
		t.Fatal(err)
	}
	fullBundle, err := git.BundleHEAD(ctx, localDir)
	if err != nil {
		t.Fatalf("BundleHEAD: %v", err)
	}
	curLocalHead := strings.TrimSpace(runSyncGit(t, localDir, "rev-parse", "HEAD"))
	if err := applySyncSeed(ctx, LocalShellExec{}, relBundleOnly, SyncSeed{
		Branch:     checkout.Branch,
		BaseCommit: curLocalHead,
		Bundle:     fullBundle,
	}); err != nil {
		t.Fatalf("applySyncSeed bundle-only: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(bundleOnlyRepo, "sandbox-committed.txt")); err != nil || string(b) != "committed in sandbox\n" {
		t.Errorf("bundle-only repo missing committed file: %v", err)
	}
}

func runSyncGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
