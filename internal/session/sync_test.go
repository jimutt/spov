package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	return (LocalShellExec{}).ExecBounded(ctx, command, stdin, sandbox.MaxExecOutput)
}

func (LocalShellExec) ExecBounded(ctx context.Context, command []string, stdin []byte, maxOutput int) (sandbox.ExecResult, error) {
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
	return sandbox.BoundOutput(sandbox.ExecResult{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}, maxOutput)
}

// ExecStream runs the command locally with its stdout written to w.
func (LocalShellExec) ExecStream(ctx context.Context, command []string, w io.Writer, maxStdout int64) (sandbox.ExecResult, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	bounded := &boundedWriter{w: w, left: maxStdout}
	var stderr bytes.Buffer
	cmd.Stdout = bounded
	cmd.Stderr = &stderr
	err := cmd.Run()
	if bounded.over {
		return sandbox.ExecResult{}, sandbox.ErrOutputTooLarge
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return sandbox.ExecResult{}, err
		}
		code = exit.ExitCode()
	}
	return sandbox.ExecResult{ExitCode: code, Stderr: stderr.String()}, nil
}

type boundedWriter struct {
	w    io.Writer
	left int64
	over bool
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > b.left {
		b.over = true
		return 0, sandbox.ErrOutputTooLarge
	}
	b.left -= int64(len(p))
	return b.w.Write(p)
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
	// InspectSync measures the same state without carrying it.
	//
	// @decision(DL-101)
	summary, err := InspectSync(ctx, LocalShellExec{}, relFromWorkspace, checkout.HeadCommit)
	if err != nil {
		t.Fatalf("InspectSync: %v", err)
	}
	// The changed files' own sizes: the edit, the untracked file and the
	// nested one.
	wantTree := int64(len("unpushed local work + uncommitted edit\n") + len("brand new untracked\n") + len("nested untracked\n"))
	if summary.Branch != "feat/e2e-sync" || summary.HeadCommit != exported.HeadCommit || summary.Commits != 1 ||
		summary.Files != 4 || summary.Rewritten || summary.WorktreeBytes != wantTree || summary.ObjectBytes == 0 ||
		summary.Bytes != max(summary.ObjectBytes, summary.WorktreeBytes) || summary.Over != "" {
		t.Errorf("InspectSync = %+v, want branch feat/e2e-sync, 1 commit, 4 files, %d bytes of files", summary, wantTree)
	}
	// The streamed archive carries the same state, and is what lands.
	//
	// @decision(DL-176) @decision(DL-178)
	var archive bytes.Buffer
	res, err := StreamSync(ctx, LocalShellExec{}, relFromWorkspace, checkout.HeadCommit, &archive)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("StreamSync = %+v, %v", res, err)
	}
	spool, err := git.SpoolSyncArchive(&archive, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("SpoolSyncArchive: %v", err)
	}
	if spool.HeadCommit != exported.HeadCommit || spool.Branch != exported.Branch || spool.BaseCommit != exported.BaseCommit ||
		strings.Join(spool.Deleted, ",") != strings.Join(exported.Deleted, ",") || spool.BundlePath == "" || spool.WorktreeTarPath == "" {
		t.Errorf("the archive = %+v, the JSON export = %+v", spool, exported)
	}
	if _, err := os.Stat(archiveWorkDir); !os.IsNotExist(err) {
		t.Errorf("the archive left %s behind: %v", archiveWorkDir, err)
	}
	syncRes, err := git.ApplySync(ctx, localDir, "spov-e2e", checkout.Branch, spool)
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

// A transcript resume's fresh clone is seeded from the bundle the CLI
// sends; without one, a base the clone cannot reach leaves it on its
// default branch, and a first launch still fails.
//
// @decision(DL-098)
func TestATranscriptResumeSeedsFromTheCLIsBundleElseTheDefaultBranch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	remoteDir := filepath.Join(tmp, "remote.git")
	runSyncGit(t, tmp, "init", "--bare", "-b", "main", remoteDir)
	localDir := filepath.Join(tmp, "local")
	runSyncGit(t, tmp, "clone", remoteDir, localDir)
	runSyncGit(t, localDir, "config", "user.name", "Local Dev")
	runSyncGit(t, localDir, "config", "user.email", "dev@example.com")
	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSyncGit(t, localDir, "add", "README.md")
	runSyncGit(t, localDir, "commit", "-m", "pushed")
	runSyncGit(t, localDir, "push", "-u", "origin", "main")
	if err := os.WriteFile(filepath.Join(localDir, "unpushed.txt"), []byte("local only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSyncGit(t, localDir, "add", "unpushed.txt")
	runSyncGit(t, localDir, "commit", "-m", "unpushed")
	base := strings.TrimSpace(runSyncGit(t, localDir, "rev-parse", "HEAD"))
	// The checkout has moved on since the session started from base.
	runSyncGit(t, localDir, "checkout", "-q", "-b", "later")

	bundle, err := git.BundleCommit(ctx, localDir, "origin", base)
	if err != nil || len(bundle) == 0 {
		t.Fatalf("BundleCommit: %d bytes, %v", len(bundle), err)
	}
	if out := runSyncGit(t, localDir, "for-each-ref", "refs/spov/"); strings.TrimSpace(out) != "" {
		t.Errorf("the bundling ref was left behind: %s", out)
	}
	clone := func(name string) string {
		dir := filepath.Join(tmp, "workspace", name)
		runSyncGit(t, tmp, "clone", "-q", "--depth", "1", "file://"+remoteDir, dir)
		rel, err := filepath.Rel("/workspace", dir)
		if err != nil {
			t.Fatal(err)
		}
		return rel
	}

	withBundle := clone("with-bundle")
	if err := applySyncSeed(ctx, LocalShellExec{}, withBundle, SyncSeed{Branch: "main", BaseCommit: base, Bundle: bundle, AllowMissingBase: true}); err != nil {
		t.Fatalf("seed with the CLI's bundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join("/workspace", withBundle, "unpushed.txt")); err != nil {
		t.Errorf("the unpushed commit is not in the resumed clone: %v", err)
	}

	without := clone("without")
	err = applySyncSeed(ctx, LocalShellExec{}, without, SyncSeed{Branch: "main", BaseCommit: base, AllowMissingBase: true})
	if !errors.Is(err, ErrSeedBaseMissing) {
		t.Fatalf("seed without a bundle: %v, want ErrSeedBaseMissing", err)
	}
	if head := strings.TrimSpace(runSyncGit(t, filepath.Join("/workspace", without), "log", "-1", "--format=%s")); head != "pushed" {
		t.Errorf("the clone left without a bundle is at %q, want the default branch's tip", head)
	}

	first := clone("first-launch")
	err = applySyncSeed(ctx, LocalShellExec{}, first, SyncSeed{Branch: "main", BaseCommit: base})
	if err == nil || errors.Is(err, ErrSeedBaseMissing) {
		t.Errorf("a first launch whose base cannot be reached: %v, want a failure", err)
	}

	if _, err := git.BundleCommit(ctx, localDir, "origin", strings.Repeat("b", 40)); err == nil {
		t.Error("a commit the checkout lacks was bundled")
	}
}

// A transcript resume whose seed cannot reach its base launches on the
// default branch and says so; a first launch fails.
//
// @decision(DL-098)
func TestATranscriptResumeLaunchesWhenItsBaseIsGone(t *testing.T) {
	seed := SyncSeed{Branch: "main", BaseCommit: strings.Repeat("c", 40)}
	missing := func(t *testing.T, f *fixture, resume string) (*Session, string, error) {
		t.Helper()
		f.opts.Sync = &seed
		f.opts.Resume = resume
		key := strings.Join(seedCommand(t, seed, resume == opencode.ResumeTranscript), " ")
		f.provider.ExecResults[key] = sandbox.ExecResult{ExitCode: seedBaseMissingExit}
		var progress bytes.Buffer
		f.launcher.Progress = &progress
		sess, err := f.launcher.Launch(context.Background(), f.opts)
		return sess, progress.String(), err
	}
	sess, notes, err := missing(t, newFixture(t), opencode.ResumeTranscript)
	if err != nil || sess == nil {
		t.Fatalf("a transcript resume failed: %v", err)
	}
	if !strings.Contains(notes, "not on the remote") || !strings.Contains(notes, "spov resume inside the checkout") {
		t.Errorf("no note about the missing commits:\n%s", notes)
	}
	if _, _, err := missing(t, newFixture(t), ""); err == nil {
		t.Error("a first launch whose base cannot be reached went ahead")
	}
}

// seedCommand is the command applySyncSeed sends for seed into the
// fixture's repository, for matching a fake's canned result.
func seedCommand(t *testing.T, seed SyncSeed, allowMissing bool) []string {
	t.Helper()
	seed.AllowMissingBase = allowMissing
	rec := &recordingExec{}
	_ = applySyncSeed(context.Background(), rec, "app", seed)
	if len(rec.commands) != 1 {
		t.Fatalf("the seed ran %d commands", len(rec.commands))
	}
	return rec.commands[0]
}

type recordingExec struct {
	sandbox.Handle
	commands [][]string
}

func (r *recordingExec) ExecInput(_ context.Context, command []string, _ []byte) (sandbox.ExecResult, error) {
	r.commands = append(r.commands, command)
	return sandbox.ExecResult{}, nil
}

func (r *recordingExec) ExecBounded(ctx context.Context, command []string, stdin []byte, _ int) (sandbox.ExecResult, error) {
	return r.ExecInput(ctx, command, stdin)
}

// An export from a sandbox that does not hold the recorded base bundles
// from the remote's default branch rather than the whole history.
//
// @decision(DL-098)
func TestAnExportWithoutItsBaseBundlesFromTheDefaultBranch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	remoteDir := filepath.Join(tmp, "remote.git")
	runSyncGit(t, tmp, "init", "--bare", "-b", "main", remoteDir)
	seedDir := filepath.Join(tmp, "seed")
	runSyncGit(t, tmp, "clone", "-q", remoteDir, seedDir)
	runSyncGit(t, seedDir, "config", "user.name", "Dev")
	runSyncGit(t, seedDir, "config", "user.email", "dev@example.com")
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("# x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSyncGit(t, seedDir, "add", "README.md")
	runSyncGit(t, seedDir, "commit", "-q", "-m", "pushed")
	runSyncGit(t, seedDir, "push", "-q", "origin", "main")

	sandboxRepo := filepath.Join(tmp, "workspace", "app")
	runSyncGit(t, tmp, "clone", "-q", remoteDir, sandboxRepo)
	runSyncGit(t, sandboxRepo, "config", "user.name", "Agent")
	runSyncGit(t, sandboxRepo, "config", "user.email", "agent@example.com")
	if err := os.WriteFile(filepath.Join(sandboxRepo, "agent.txt"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSyncGit(t, sandboxRepo, "add", "agent.txt")
	runSyncGit(t, sandboxRepo, "commit", "-q", "-m", "agent")
	rel, err := filepath.Rel("/workspace", sandboxRepo)
	if err != nil {
		t.Fatal(err)
	}
	state, err := ExportSync(ctx, LocalShellExec{}, rel, strings.Repeat("e", 40))
	if err != nil {
		t.Fatal(err)
	}
	header := state.Bundle[:bytes.Index(state.Bundle, []byte("\n\n"))]
	if !bytes.Contains(header, []byte("\n-")) {
		t.Errorf("the export bundled the whole history:\n%s", header)
	}
}

// An unchanged repository measures nothing, and one whose history was
// rewritten past the base says so.
//
// @decision(DL-101)
func TestInspectSyncSeesAnUnchangedAndARewrittenRepository(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	runSyncGit(t, filepath.Dir(repo), "init", "-b", "main", repo)
	runSyncGit(t, repo, "config", "user.name", "Agent")
	runSyncGit(t, repo, "config", "user.email", "agent@example.com")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSyncGit(t, repo, "add", "a.txt")
	runSyncGit(t, repo, "commit", "-m", "base")
	base := strings.TrimSpace(runSyncGit(t, repo, "rev-parse", "HEAD"))
	rel, err := filepath.Rel("/workspace", repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := InspectSync(ctx, LocalShellExec{}, rel, base)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commits != 0 || got.Files != 0 || got.Bytes != 0 || got.Rewritten || got.HeadCommit != base {
		t.Errorf("unchanged = %+v", got)
	}
	runSyncGit(t, repo, "commit", "--amend", "-m", "rewritten")
	if got, err = InspectSync(ctx, LocalShellExec{}, rel, base); err != nil {
		t.Fatal(err)
	}
	if !got.Rewritten || got.Commits != 0 || got.HeadCommit == base {
		t.Errorf("rewritten = %+v", got)
	}
	if _, err := InspectSync(ctx, LocalShellExec{}, rel, "not-a-sha"); err == nil {
		t.Error("an invalid base was accepted")
	}
}

// A local-only synced session starts its server in the workspace, since
// its repository is seeded afterwards, but works in that repository: its
// config lists spov's guide, and the guide says where it works.
//
// @decision(DL-174)
func TestALocalOnlySyncedSessionListsTheGuide(t *testing.T) {
	f := newFixture(t)
	f.opts.Selections = nil
	f.opts.Landing = "notes"
	f.opts.Sync = &SyncSeed{Branch: "main", BaseCommit: strings.Repeat("a", 40), Slug: "local/notes", Dir: "notes", Host: "local", Bundle: []byte("bundle")}
	sess, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Landing != "/workspace/notes" {
		t.Errorf("landing = %q", sess.Landing)
	}
	specs := f.provider.Specs()
	script := strings.Join(specs[len(specs)-1].Command, " ")
	if !strings.Contains(script, `"instructions": [`) || !strings.Contains(script, `"/workspace/AGENTS.md"`) {
		t.Errorf("the config does not list the guide:\n%s", script)
	}
	if !strings.Contains(script, "You are working inside `/workspace/notes`") {
		t.Errorf("the guide does not say where the session works:\n%s", script)
	}
}

// TestAnArchiveOverTheBoundsIsRefusedBeforeAByte: the script measures
// what the landing would, refuses before it writes anything, and names
// the largest files; inspect reports the same reason.
//
// @decision(DL-177) @decision(DL-176)
func TestAnArchiveOverTheBoundsIsRefusedBeforeAByte(t *testing.T) {
	defer func(old string) { syncStateScript = old }(syncStateScript)
	syncStateScript = buildSyncScript(syncBounds{JSON: 1 << 20, Export: 1 << 20, Objects: 4000, Blob: 3000, Meta: 1 << 20})

	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	runSyncGit(t, filepath.Dir(repo), "init", "-b", "main", repo)
	runSyncGit(t, repo, "config", "user.name", "Agent")
	runSyncGit(t, repo, "config", "user.email", "agent@example.com")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSyncGit(t, repo, "add", "a.txt")
	runSyncGit(t, repo, "commit", "-m", "base")
	base := strings.TrimSpace(runSyncGit(t, repo, "rev-parse", "HEAD"))
	rel, err := filepath.Rel("/workspace", repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "dataset.bin"), bytes.Repeat([]byte("x"), 3500), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "small.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := StreamSync(ctx, LocalShellExec{}, rel, base, &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || out.Len() != 0 {
		t.Fatalf("exit %d after %d bytes, want a refusal before any", res.ExitCode, out.Len())
	}
	for _, want := range []string{"dataset.bin is 3500 bytes, over the 3000 bytes one file may be", "The largest files:", ".gitignore"} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("the refusal says %q, want it to say %q", res.Stderr, want)
		}
	}
	got, err := InspectSync(ctx, LocalShellExec{}, rel, base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Over, "dataset.bin") || got.WorktreeBytes != 3503 {
		t.Errorf("inspect = %+v", got)
	}

	// A commit that adds more than the objects bound is refused too.
	runSyncGit(t, repo, "add", "-A")
	runSyncGit(t, repo, "commit", "-m", "add the data")
	out.Reset()
	if res, err = StreamSync(ctx, LocalShellExec{}, rel, base, &out); err != nil || res.ExitCode == 0 || out.Len() != 0 {
		t.Fatalf("a large commit = %+v, %v, %d bytes", res, err, out.Len())
	}
	if !strings.Contains(res.Stderr, "dataset.bin is 3500 bytes") {
		t.Errorf("the refusal of a commit says %q", res.Stderr)
	}
}
