package git

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initTestRepo(t *testing.T, remoteURL string) (dir, head string) {
	t.Helper()
	dir = t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	if remoteURL != "" {
		runGit(t, dir, "remote", "add", "origin", remoteURL)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial")
	head = strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	return dir, head
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// @decision(DL-027)
func TestParseRemoteURL(t *testing.T) {
	kinds := map[string]string{
		"github.com":          KindGitHub,
		"codefloe.com":        KindForgejo,
		"gitlab.com":          KindGitLab,
		"git.example.com/sub": KindGitLab,
	}
	tests := []struct {
		name     string
		url      string
		wantHost string
		wantBase string
		wantOwn  string
		wantName string
		wantErr  bool
	}{
		{"https github", "https://github.com/alice/spov.git", "github.com", "", "alice", "spov", false},
		{"https port 443", "https://codefloe.com:443/alice/spov", "codefloe.com", "", "alice", "spov", false},
		{"scp ssh", "git@codefloe.com:alice/spov.git", "codefloe.com", "", "alice", "spov", false},
		{"ssh scheme with port", "ssh://git@gitlab.com:2222/acme-testing/subgroup/widgets-subgroup-sandbox.git", "gitlab.com", "", "acme-testing/subgroup", "widgets-subgroup-sandbox", false},
		{"subpath gitlab", "https://git.example.com/sub/group/team/proj.git", "git.example.com", "/sub", "group/team", "proj", false},
		{"non-gitlab rejects nested owner", "https://github.com/a/b/c.git", "", "", "", "", true},
		{"empty", "", "", "", "", "", true},
		{"unsupported scheme", "ftp://github.com/a/b.git", "", "", "", "", true},
		{"missing repo path", "https://github.com/", "", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, base, owner, name, err := ParseRemoteURL(tt.url, kinds)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if host != tt.wantHost || base != tt.wantBase || owner != tt.wantOwn || name != tt.wantName {
				t.Errorf("got (%q, %q, %q, %q), want (%q, %q, %q, %q)",
					host, base, owner, name, tt.wantHost, tt.wantBase, tt.wantOwn, tt.wantName)
			}
		})
	}
}

// @decision(DL-027)
func TestValidBranchNameAndCommitSHA(t *testing.T) {
	for _, okName := range []string{"main", "feature/sync-back", "r11_local.sync"} {
		if err := ValidBranchName(okName); err != nil {
			t.Errorf("ValidBranchName(%q) = %v", okName, err)
		}
	}
	for _, bad := range []string{"", "@", "-dash", "/lead", "trail/", "dot.", "a..b", "a//b", "a@{b", "a.lock", "seg/.hidden", "space here"} {
		if err := ValidBranchName(bad); err == nil {
			t.Errorf("ValidBranchName(%q) succeeded", bad)
		}
	}
	if !ValidCommitSHA(strings.Repeat("a", 40)) {
		t.Error("expected 40-char hex SHA to be valid")
	}
	if !ValidCommitSHA("not-a-sha") && !ValidCommitSHA(strings.Repeat("g", 40)) {
		if got := SingleRepoDir("alice/sub", "out"); got != "alice-sub-out" {
			t.Errorf("SingleRepoDir reserved = %q", got)
		}
		if got := SingleRepoDir("alice", "spov"); got != "spov" {
			t.Errorf("SingleRepoDir regular = %q", got)
		}
	}
}

// @decision(DL-027)
func TestInspectLocalCheckoutCleanAndBundle(t *testing.T) {
	ctx := context.Background()
	kinds := map[string]string{"codefloe.com": KindForgejo}

	t.Run("outside git repo fails clearly", func(t *testing.T) {
		_, err := InspectLocalCheckout(ctx, t.TempDir(), kinds, true)
		if err == nil || !strings.Contains(err.Error(), "not in a git repository") {
			t.Fatalf("err = %v", err)
		}
	})

	dir, baseHead := initTestRepo(t, "git@codefloe.com:alice/spov.git")
	// Simulate remote-tracking ref at baseHead.
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", baseHead)
	runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	t.Run("clean checkout at remote tip produces no bundle", func(t *testing.T) {
		got, err := InspectLocalCheckout(ctx, dir, kinds, true)
		if err != nil {
			t.Fatalf("InspectLocalCheckout: %v", err)
		}
		if got.Host != "codefloe.com" || got.Owner != "alice" || got.Name != "spov" || got.Branch != "main" || got.HeadCommit != baseHead {
			t.Errorf("got = %+v", got)
		}
		if len(got.Bundle) != 0 {
			t.Errorf("expected empty bundle when HEAD matches origin/main, got %d bytes", len(got.Bundle))
		}
	})

	t.Run("dirty working tree or untracked file is refused", func(t *testing.T) {
		untracked := filepath.Join(dir, "scratch.txt")
		if err := os.WriteFile(untracked, []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(untracked) }()
		if _, err := InspectLocalCheckout(ctx, dir, kinds, true); err == nil || !strings.Contains(err.Error(), "uncommitted or untracked") {
			t.Fatalf("expected dirty checkout error, got %v", err)
		}
	})

	t.Run("detached HEAD is refused", func(t *testing.T) {
		runGit(t, dir, "checkout", "--detach", "HEAD")
		defer runGit(t, dir, "checkout", "main")
		if _, err := InspectLocalCheckout(ctx, dir, kinds, true); err == nil || !strings.Contains(err.Error(), "detached HEAD") {
			t.Fatalf("expected detached HEAD error, got %v", err)
		}
	})

	t.Run("unpushed local commit produces incremental bundle", func(t *testing.T) {
		runGit(t, dir, "checkout", "-b", "feat/local")
		if err := os.WriteFile(filepath.Join(dir, "local.txt"), []byte("unpushed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "local.txt")
		runGit(t, dir, "commit", "-m", "local commit")
		localHead := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

		got, err := InspectLocalCheckout(ctx, dir, kinds, true)
		if err != nil {
			t.Fatalf("InspectLocalCheckout: %v", err)
		}
		if got.Branch != "feat/local" || got.HeadCommit != localHead {
			t.Errorf("got branch=%q head=%q", got.Branch, got.HeadCommit)
		}
		if len(got.Bundle) == 0 {
			t.Fatal("expected non-empty bundle for unpushed commit")
		}
	})
}

func makeWorktreeTar(t *testing.T, files map[string]string) []byte {
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
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// @decision(DL-027)
func TestApplySyncInPlaceAndFallback(t *testing.T) {
	ctx := context.Background()
	localDir, _ := initTestRepo(t, "https://codefloe.com/alice/spov.git")
	if err := os.WriteFile(filepath.Join(localDir, "obsolete.txt"), []byte("remove me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, localDir, "add", "obsolete.txt")
	runGit(t, localDir, "commit", "-m", "add obsolete")
	baseHead := strings.TrimSpace(runGit(t, localDir, "rev-parse", "HEAD"))

	// Create a second clone representing the sandbox repository.
	sandboxDir := filepath.Join(t.TempDir(), "sandbox")
	runGit(t, localDir, "clone", localDir, sandboxDir)
	runGit(t, sandboxDir, "config", "user.name", "Sandbox Agent")
	runGit(t, sandboxDir, "config", "user.email", "agent@example.com")
	if err := os.WriteFile(filepath.Join(sandboxDir, "committed.txt"), []byte("from sandbox commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, sandboxDir, "add", "committed.txt")
	runGit(t, sandboxDir, "commit", "-m", "sandbox commit")
	sandboxHead := strings.TrimSpace(runGit(t, sandboxDir, "rev-parse", "HEAD"))

	bundleFile := filepath.Join(t.TempDir(), "sandbox.bundle")
	runGit(t, sandboxDir, "bundle", "create", bundleFile, baseHead+"..HEAD")
	bundleBytes, err := os.ReadFile(bundleFile)
	if err != nil {
		t.Fatal(err)
	}

	tarBytes := makeWorktreeTar(t, map[string]string{
		"uncommitted.txt":     "dirty worktree edit\n",
		"sub/nested-file.txt": "nested content\n",
	})

	t.Run("no changes leaves repo untouched", func(t *testing.T) {
		res, err := ApplySync(ctx, localDir, "spov-1", "main", ExportedState{
			Branch:     "main",
			BaseCommit: baseHead,
			HeadCommit: baseHead,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.NoChanges {
			t.Errorf("expected NoChanges, got %+v", res)
		}
	})

	t.Run("clean checkout at base updates branch and working tree in place", func(t *testing.T) {
		res, err := ApplySync(ctx, localDir, "spov-1", "main", ExportedState{
			Branch:      "main",
			BaseCommit:  baseHead,
			HeadCommit:  sandboxHead,
			Bundle:      bundleBytes,
			WorktreeTar: tarBytes,
			Deleted:     []string{"obsolete.txt"},
		})
		if err != nil {
			t.Fatalf("ApplySync: %v", err)
		}
		if !res.InPlace || res.Commits != 1 || res.FilesChanged != 3 {
			t.Errorf("unexpected SyncResult: %+v", res)
		}
		gotHead := strings.TrimSpace(runGit(t, localDir, "rev-parse", "HEAD"))
		if gotHead != sandboxHead {
			t.Errorf("local HEAD = %s, want %s", gotHead, sandboxHead)
		}
		if b, err := os.ReadFile(filepath.Join(localDir, "committed.txt")); err != nil || string(b) != "from sandbox commit\n" {
			t.Errorf("committed.txt = %q, err = %v", b, err)
		}
		if b, err := os.ReadFile(filepath.Join(localDir, "uncommitted.txt")); err != nil || string(b) != "dirty worktree edit\n" {
			t.Errorf("uncommitted.txt = %q, err = %v", b, err)
		}
		if _, err := os.Stat(filepath.Join(localDir, "obsolete.txt")); !os.IsNotExist(err) {
			t.Errorf("expected obsolete.txt to be removed, stat err = %v", err)
		}
	})

	t.Run("diverged or dirty local checkout lands on fallback branch spov/<session>", func(t *testing.T) {
		// Dirty local checkout still exists from the previous subtest's uncommitted files.
		res, err := ApplySync(ctx, localDir, "spov-diverged", "main", ExportedState{
			Branch:      "main",
			BaseCommit:  baseHead,
			HeadCommit:  sandboxHead,
			Bundle:      bundleBytes,
			WorktreeTar: tarBytes,
			Deleted:     []string{"obsolete.txt"},
		})
		if err != nil {
			t.Fatalf("ApplySync fallback: %v", err)
		}
		if res.InPlace || res.FallbackBranch != "spov/spov-diverged" || !res.DirtyCommitted {
			t.Errorf("unexpected fallback SyncResult: %+v", res)
		}
		refSHA := strings.TrimSpace(runGit(t, localDir, "rev-parse", "refs/heads/spov/spov-diverged"))
		if refSHA == "" || refSHA == sandboxHead {
			t.Errorf("expected synthetic WIP commit on spov/spov-diverged, got %s", refSHA)
		}
		msg := strings.TrimSpace(runGit(t, localDir, "log", "-1", "--format=%s", "refs/heads/spov/spov-diverged"))
		if !strings.Contains(msg, "uncommitted changes from spov-diverged") {
			t.Errorf("fallback commit message = %q", msg)
		}
	})

	t.Run("tar traversal or .git entry is rejected", func(t *testing.T) {
		cleanDir, head := initTestRepo(t, "https://codefloe.com/alice/spov.git")
		for _, badPath := range []string{"../escape.txt", ".git/config", ".Git/hooks/post-checkout", "/abs/path"} {
			badTar := makeWorktreeTar(t, map[string]string{badPath: "bad"})
			if _, err := ApplySync(ctx, cleanDir, "spov-bad", "main", ExportedState{
				Branch:      "main",
				BaseCommit:  head,
				HeadCommit:  head,
				WorktreeTar: badTar,
			}); err == nil {
				t.Errorf("expected %q to be rejected", badPath)
			}
		}
		var symBuf bytes.Buffer
		symGz := gzip.NewWriter(&symBuf)
		symTw := tar.NewWriter(symGz)
		if err := symTw.WriteHeader(&tar.Header{
			Name:     "symdir",
			Linkname: ".git/hooks",
			Typeflag: tar.TypeSymlink,
			Mode:     0o777,
		}); err != nil {
			t.Fatal(err)
		}
		hookBody := "#!/bin/sh\n"
		if err := symTw.WriteHeader(&tar.Header{
			Name:     "symdir/post-checkout",
			Typeflag: tar.TypeReg,
			Mode:     0o755,
			Size:     int64(len(hookBody)),
		}); err != nil {
			t.Fatal(err)
		}
		_, _ = symTw.Write([]byte(hookBody))
		_ = symTw.Close()
		_ = symGz.Close()
		if _, err := ApplySync(ctx, cleanDir, "spov-sym", "main", ExportedState{
			Branch:      "main",
			BaseCommit:  head,
			HeadCommit:  head,
			WorktreeTar: symBuf.Bytes(),
		}); err == nil {
			t.Error("expected .git symlink target to be rejected")
		}
		var symBuf2 bytes.Buffer
		symGz2 := gzip.NewWriter(&symBuf2)
		symTw2 := tar.NewWriter(symGz2)
		_ = symTw2.WriteHeader(&tar.Header{
			Name:     "symdir",
			Linkname: "realdir",
			Typeflag: tar.TypeSymlink,
			Mode:     0o777,
		})
		_ = symTw2.WriteHeader(&tar.Header{
			Name:     "symdir/child.txt",
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     4,
		})
		_, _ = symTw2.Write([]byte("bad\n"))
		_ = symTw2.Close()
		_ = symGz2.Close()
		if _, err := ApplySync(ctx, cleanDir, "spov-sym2", "main", ExportedState{
			Branch:      "main",
			BaseCommit:  head,
			HeadCommit:  head,
			WorktreeTar: symBuf2.Bytes(),
		}); err == nil {
			t.Error("expected write through symlinked parent directory to be rejected")
		}
		if _, err := ApplySync(ctx, cleanDir, "spov-bad-sha", "main", ExportedState{
			Branch:     "main",
			BaseCommit: head,
			HeadCommit: "invalid",
		}); err == nil {
			t.Error("expected invalid HeadCommit to be rejected")
		}
		if _, err := ApplySync(ctx, cleanDir, "spov-bad-del", "main", ExportedState{
			Branch:     "main",
			BaseCommit: head,
			HeadCommit: head,
			Deleted:    []string{"../outside.txt"},
		}); err == nil {
			t.Error("expected invalid Deleted path to be rejected")
		}
		_ = os.Remove(filepath.Join(cleanDir, "symdir"))
		var okSymBuf bytes.Buffer
		okSymGz := gzip.NewWriter(&okSymBuf)
		okSymTw := tar.NewWriter(okSymGz)
		_ = okSymTw.WriteHeader(&tar.Header{
			Name:     "link-readme",
			Linkname: "README.md",
			Typeflag: tar.TypeSymlink,
			Mode:     0o777,
		})
		_ = okSymTw.Close()
		_ = okSymGz.Close()
		if res, err := ApplySync(ctx, cleanDir, "spov-ok-sym", "main", ExportedState{
			Branch:      "main",
			BaseCommit:  head,
			HeadCommit:  head,
			WorktreeTar: okSymBuf.Bytes(),
		}); err != nil || !res.InPlace || res.FilesChanged != 1 {
			t.Errorf("valid relative symlink ApplySync = %+v, %v", res, err)
		}
	})
}

// InCheckout is true anywhere under a working tree and false outside
// one or without git.
//
// @decision(DL-095)
func TestInCheckout(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if !InCheckout(ctx, repo) || !InCheckout(ctx, sub) {
		t.Error("a working tree and a directory under it are not in a checkout")
	}
	if InCheckout(ctx, t.TempDir()) {
		t.Error("an empty directory is in a checkout")
	}
	t.Setenv("PATH", t.TempDir())
	if InCheckout(ctx, repo) {
		t.Error("without git, a working tree is in a checkout")
	}
}
