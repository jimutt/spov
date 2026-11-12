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

// writeFile writes body at rel under dir, creating its directories.
func writeFile(t *testing.T, dir, rel, body string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// commitHead commits everything staged and returns HEAD.
func commitHead(t *testing.T, dir, msg string) string {
	t.Helper()
	runGit(t, dir, "commit", "-q", "-m", msg)
	return strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
}

// sandboxCommits clones dir, lets edit change the clone and stage what
// it wants, commits, and returns the export of that commit as a bundle.
func sandboxCommits(t *testing.T, dir, base string, edit func(sandbox string)) ExportedState {
	t.Helper()
	sandbox := filepath.Join(t.TempDir(), "sandbox")
	runGit(t, dir, "clone", "-q", dir, sandbox)
	runGit(t, sandbox, "config", "user.name", "Agent")
	runGit(t, sandbox, "config", "user.email", "agent@example.com")
	edit(sandbox)
	head := commitHead(t, sandbox, "session")
	bundle := filepath.Join(t.TempDir(), "b.bundle")
	runGit(t, sandbox, "bundle", "create", "-q", bundle, base+"..HEAD")
	raw, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return ExportedState{Branch: "main", BaseCommit: base, HeadCommit: head, Bundle: raw}
}

// tarEntry is one entry of a test archive: a file, or a symlink when
// link is set.
type tarEntry struct{ name, body, link string }

func makeTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.link != "" {
			hdr = &tar.Header{Name: e.name, Mode: 0o777, Linkname: e.link, Typeflag: tar.TypeSymlink}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
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

// wantFallback checks a result landed on the fallback branch with a
// reason naming want, and that the checkout is still at head.
func wantFallback(t *testing.T, dir, head string, res SyncResult, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatalf("ApplySync: %v", err)
	}
	if res.InPlace || res.FallbackBranch == "" || !strings.Contains(res.Reason, want) {
		t.Errorf("result = %+v, want a fallback whose reason names %q", res, want)
	}
	if got := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")); got != head {
		t.Errorf("the checkout moved to %s", got)
	}
}

// wantContent checks the file at rel under dir holds body.
func wantContent(t *testing.T, dir, rel, body string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil || string(got) != body {
		t.Errorf("%s = %q, %v; want %q unchanged", rel, got, err, body)
	}
}

// wantAbsent checks nothing exists at rel under dir.
func wantAbsent(t *testing.T, dir, rel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
		t.Errorf("%s exists", rel)
	}
}

// userCommits runs the user's next `git commit` with the checkout's
// own hooks, as the user would.
func userCommits(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "commit", "-q", "--allow-empty", "-m", "next")
	cmd.Dir = dir
	_ = cmd.Run() // a hook may fail the commit; only its marker matters
}

// TestSyncBackNeverLandsInPlaceOverAParentOfTheHooksDirectory: a change
// to a directory above the hooks directory, or to a config file git
// reads for the checkout, lands on the fallback branch, so nothing it
// names runs at the user's next commit.
//
// @decision(DL-126)
func TestSyncBackNeverLandsInPlaceOverAParentOfTheHooksDirectory(t *testing.T) {
	ctx := context.Background()
	const marker = "#!/bin/sh\necho ran > hook-ran\n"

	t.Run("a commit replaces the hooks directory's parent with a symlink", func(t *testing.T) {
		dir, _ := initTestRepo(t, "")
		writeFile(t, dir, ".husky/pre-commit", "#!/bin/sh\n", 0o755)
		runGit(t, dir, "add", ".husky")
		head := commitHead(t, dir, "husky")
		writeFile(t, dir, ".husky/_/.gitignore", "*\n", 0o644)
		writeFile(t, dir, ".husky/_/h", "#!/bin/sh\n", 0o755)
		runGit(t, dir, "config", "core.hooksPath", ".husky/_")

		state := sandboxCommits(t, dir, head, func(s string) {
			runGit(t, s, "rm", "-q", "-r", ".husky")
			writeFile(t, s, "tools/_/pre-commit", marker, 0o755)
			if err := os.Symlink("tools", filepath.Join(s, ".husky")); err != nil {
				t.Fatal(err)
			}
			runGit(t, s, "add", "-A")
		})
		res, err := ApplySync(ctx, dir, "s", "main", state)
		wantFallback(t, dir, head, res, err, ".husky")
		wantContent(t, dir, ".husky/_/h", "#!/bin/sh\n")
		userCommits(t, dir)
		wantAbsent(t, dir, "hook-ran")
	})

	t.Run("an uncommitted symlink above a hooks directory not made yet", func(t *testing.T) {
		dir, head := initTestRepo(t, "")
		runGit(t, dir, "config", "core.hooksPath", "hooks/active")
		res, err := ApplySync(ctx, dir, "s", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: head,
			WorktreeTar: makeTar(t, tarEntry{name: "hooks", link: "tools"}, tarEntry{name: "tools/active/pre-commit", body: marker}),
		})
		wantFallback(t, dir, head, res, err, "hooks")
		wantAbsent(t, dir, "hooks")
		userCommits(t, dir)
		wantAbsent(t, dir, "hook-ran")
	})

	t.Run("a config file the checkout includes from the work tree", func(t *testing.T) {
		dir, _ := initTestRepo(t, "")
		writeFile(t, dir, "shared.gitconfig", "[core]\n\tautocrlf = false\n", 0o644)
		runGit(t, dir, "add", "shared.gitconfig")
		head := commitHead(t, dir, "shared config")
		runGit(t, dir, "config", "include.path", "../shared.gitconfig")
		state := sandboxCommits(t, dir, head, func(s string) {
			writeFile(t, s, "shared.gitconfig", "[core]\n\thooksPath = tools\n", 0o644)
			writeFile(t, s, "tools/pre-commit", marker, 0o755)
			runGit(t, s, "add", "-A")
		})
		res, err := ApplySync(ctx, dir, "s", "main", state)
		wantFallback(t, dir, head, res, err, "shared.gitconfig")
		wantContent(t, dir, "shared.gitconfig", "[core]\n\tautocrlf = false\n")
	})

	t.Run("a script a config value names", func(t *testing.T) {
		dir, _ := initTestRepo(t, "")
		writeFile(t, dir, "scripts/clean.sh", "#!/bin/sh\ncat\n", 0o755)
		runGit(t, dir, "add", "scripts")
		head := commitHead(t, dir, "filter script")
		runGit(t, dir, "config", "filter.probe.clean", "scripts/clean.sh")
		res, err := ApplySync(ctx, dir, "s", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: head,
			WorktreeTar: makeTar(t, tarEntry{name: "scripts/clean.sh", body: marker}),
		})
		wantFallback(t, dir, head, res, err, "scripts/clean.sh")
		wantContent(t, dir, "scripts/clean.sh", "#!/bin/sh\ncat\n")
	})
}

// TestSyncBackCommitsNeverReplaceAFileTheCheckoutDoesNotTrack: commits
// that add, replace or change the type of a path where the checkout
// holds an untracked or ignored file, or a directory holding one, land
// on the fallback branch, and the file is unchanged.
//
// @decision(DL-126)
func TestSyncBackCommitsNeverReplaceAFileTheCheckoutDoesNotTrack(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (string, string) {
		dir, _ := initTestRepo(t, "")
		writeFile(t, dir, ".gitignore", ".env\nnode_modules/\nbuild/out.bin\n", 0o644)
		writeFile(t, dir, "build/keep.txt", "k\n", 0o644)
		runGit(t, dir, "add", ".")
		head := commitHead(t, dir, "ignore rules")
		writeFile(t, dir, ".env", "SECRET=1\n", 0o644)
		writeFile(t, dir, "node_modules/.bin/tsc", "real\n", 0o755)
		writeFile(t, dir, "build/out.bin", "bin\n", 0o644)
		return dir, head
	}
	unchanged := func(t *testing.T, dir string) {
		t.Helper()
		wantContent(t, dir, ".env", "SECRET=1\n")
		wantContent(t, dir, "node_modules/.bin/tsc", "real\n")
		wantContent(t, dir, "build/out.bin", "bin\n")
	}
	for _, tc := range []struct {
		name, want string
		edit       func(t *testing.T, s string)
	}{
		{"an ignored file", ".env", func(t *testing.T, s string) {
			writeFile(t, s, ".env", "PLANTED=1\n", 0o644)
			runGit(t, s, "add", "-f", ".env")
		}},
		{"a file inside an ignored directory", "node_modules/.bin/tsc", func(t *testing.T, s string) {
			writeFile(t, s, "node_modules/.bin/tsc", "planted\n", 0o755)
			runGit(t, s, "add", "-f", "node_modules")
		}},
		{"a file in place of an ignored directory", "node_modules", func(t *testing.T, s string) {
			writeFile(t, s, "node_modules", "planted\n", 0o644)
			runGit(t, s, "add", "-f", "node_modules")
		}},
		{"a file in place of a tracked directory holding an ignored file", "build", func(t *testing.T, s string) {
			runGit(t, s, "rm", "-q", "-r", "build")
			writeFile(t, s, "build", "planted\n", 0o644)
			runGit(t, s, "add", "build")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, head := setup(t)
			state := sandboxCommits(t, dir, head, func(s string) { tc.edit(t, s) })
			res, err := ApplySync(ctx, dir, "s", "main", state)
			wantFallback(t, dir, head, res, err, tc.want)
			unchanged(t, dir)
			if got := runGit(t, dir, "status", "--porcelain"); got != "" {
				t.Errorf("the checkout is no longer clean:\n%s", got)
			}
		})
	}
}

// TestTheFastForwardNeverOverwritesAnIgnoredFile: the backstop behind
// the checks refuses an ignored file in the way, writes nothing, and
// says why; with nothing in the way it advances the branch.
//
// @decision(DL-126)
func TestTheFastForwardNeverOverwritesAnIgnoredFile(t *testing.T) {
	ctx := context.Background()
	dir, _ := initTestRepo(t, "")
	writeFile(t, dir, ".gitignore", ".env\n", 0o644)
	runGit(t, dir, "add", ".gitignore")
	head := commitHead(t, dir, "ignore")
	writeFile(t, dir, ".env", "SECRET=1\n", 0o644)
	state := sandboxCommits(t, dir, head, func(s string) {
		writeFile(t, s, ".env", "PLANTED=1\n", 0o644)
		runGit(t, s, "add", "-f", ".env")
	})
	if err := unbundleCommits(ctx, dir, state.Bundle, state.HeadCommit); err != nil {
		t.Fatal(err)
	}
	refused, err := fastForward(ctx, dir, head, state.HeadCommit)
	if err != nil || !strings.Contains(refused, ".env") {
		t.Fatalf("fastForward = %q, %v; want a refusal naming .env", refused, err)
	}
	wantContent(t, dir, ".env", "SECRET=1\n")
	if got := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")); got != head {
		t.Errorf("HEAD moved to %s", got)
	}

	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	if refused, err := fastForward(ctx, dir, head, state.HeadCommit); err != nil || refused != "" {
		t.Fatalf("fastForward with nothing in the way = %q, %v", refused, err)
	}
	if got := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")); got != state.HeadCommit {
		t.Errorf("HEAD = %s, want %s", got, state.HeadCommit)
	}
}

// TestSyncBackNeverWritesInsideASubmodule: an uncommitted path under a
// gitlink is refused with nothing written, and commits that turn a
// submodule into files land on the fallback branch.
//
// @decision(DL-126)
func TestSyncBackNeverWritesInsideASubmodule(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (string, string) {
		dir, _ := initTestRepo(t, "")
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, sub, "init", "-q", "-b", "main")
		runGit(t, sub, "config", "user.name", "Test User")
		runGit(t, sub, "config", "user.email", "test@example.com")
		runGit(t, sub, "commit", "-q", "--allow-empty", "-m", "sub")
		subHead := strings.TrimSpace(runGit(t, sub, "rev-parse", "HEAD"))
		runGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+subHead+",sub")
		return dir, commitHead(t, dir, "gitlink")
	}

	t.Run("an uncommitted file", func(t *testing.T) {
		dir, head := setup(t)
		_, err := ApplySync(ctx, dir, "s", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: head,
			WorktreeTar: makeTar(t, tarEntry{name: "sub/notes.txt", body: "x\n"}),
		})
		if err == nil || !strings.Contains(err.Error(), "sub/notes.txt") {
			t.Fatalf("err = %v, want a refusal naming sub/notes.txt", err)
		}
		wantAbsent(t, dir, "sub/notes.txt")
		if out := runGit(t, dir, "branch", "--list", "spov/*"); out != "" {
			t.Errorf("a refused export wrote a branch: %s", out)
		}
	})

	t.Run("commits that replace the submodule with files", func(t *testing.T) {
		dir, head := setup(t)
		state := sandboxCommits(t, dir, head, func(s string) {
			runGit(t, s, "rm", "-q", "--cached", "sub")
			writeFile(t, s, "sub/notes.txt", "x\n", 0o644)
			runGit(t, s, "add", "sub/notes.txt")
		})
		res, err := ApplySync(ctx, dir, "s", "main", state)
		wantFallback(t, dir, head, res, err, "sub")
		wantAbsent(t, dir, "sub/notes.txt")
	})
}

// TestAnExportTheCheckoutIgnoresLandsOnTheFallbackBranch: when the user
// ignores more than the sandbox did, the session's files are not lost
// and not refused; they land on the fallback branch, and the work tree
// is untouched.
//
// @decision(DL-126)
func TestAnExportTheCheckoutIgnoresLandsOnTheFallbackBranch(t *testing.T) {
	ctx := context.Background()
	dir, head := initTestRepo(t, "")
	writeFile(t, dir, ".git/info/exclude", "*.log\n", 0o644)
	res, err := ApplySync(ctx, dir, "s", "main", ExportedState{
		Branch: "main", BaseCommit: head, HeadCommit: head,
		WorktreeTar: makeTar(t, tarEntry{name: "debug.log", body: "trace\n"}, tarEntry{name: "a.txt", body: "a\n"}),
	})
	wantFallback(t, dir, head, res, err, "debug.log")
	wantAbsent(t, dir, "debug.log")
	wantAbsent(t, dir, "a.txt")
	if got := runGit(t, dir, "show", res.FallbackBranch+":debug.log"); got != "trace\n" {
		t.Errorf("the fallback branch holds debug.log = %q", got)
	}
}

// TestABadArchiveEntryIsRefusedBeforeTheCommitsLand: an archive entry
// the unpacker cannot write is refused before the fast-forward, so the
// checkout is not left half updated.
//
// @decision(DL-126)
func TestABadArchiveEntryIsRefusedBeforeTheCommitsLand(t *testing.T) {
	dir, head := initTestRepo(t, "")
	state := sandboxCommits(t, dir, head, func(s string) {
		writeFile(t, s, "a.txt", "a\n", 0o644)
		runGit(t, s, "add", "a.txt")
	})
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "b.txt", Linkname: "a.txt", Typeflag: tar.TypeLink}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	state.WorktreeTar = buf.Bytes()
	if _, err := ApplySync(context.Background(), dir, "s", "main", state); err == nil {
		t.Fatal("an archive with a hard link was applied")
	}
	if got := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")); got != head {
		t.Errorf("the commits landed although the archive was refused: HEAD = %s", got)
	}
	wantAbsent(t, dir, "a.txt")
}

// TestSyncBackNeverLandsInPlaceWhenItChangesWhatGitReads: a change to a
// .gitattributes, .lfsconfig or .gitmodules at any depth lands on the
// fallback branch, so the user's filter drivers do not run on it.
//
// @decision(DL-126)
func TestSyncBackNeverLandsInPlaceWhenItChangesWhatGitReads(t *testing.T) {
	ctx := context.Background()

	t.Run("committed attributes that name the user's filter", func(t *testing.T) {
		dir, head := initTestRepo(t, "")
		runGit(t, dir, "config", "filter.probe.smudge", "touch filter-ran; cat")
		state := sandboxCommits(t, dir, head, func(s string) {
			writeFile(t, s, ".gitattributes", "* filter=probe\n", 0o644)
			writeFile(t, s, "a.txt", "a\n", 0o644)
			runGit(t, s, "add", "-A")
		})
		res, err := ApplySync(ctx, dir, "s", "main", state)
		wantFallback(t, dir, head, res, err, ".gitattributes")
		wantAbsent(t, dir, "filter-ran")
		wantAbsent(t, dir, ".gitattributes")
	})

	for _, p := range []string{"docs/.gitattributes", ".lfsconfig", "lib/.gitmodules", ".GitAttributes"} {
		t.Run("an uncommitted "+p, func(t *testing.T) {
			dir, head := initTestRepo(t, "")
			res, err := ApplySync(ctx, dir, "s", "main", ExportedState{
				Branch: "main", BaseCommit: head, HeadCommit: head,
				WorktreeTar: makeTar(t, tarEntry{name: p, body: "x\n"}),
			})
			wantFallback(t, dir, head, res, err, p)
			wantAbsent(t, dir, p)
		})
	}
}
