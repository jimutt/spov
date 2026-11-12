package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheUsersConfigCannotWeakenTheInPlaceRules checks, by experiment
// in temporary repositories, that settings a user might carry do not
// let an unsafe sync land in place.
//
// @decision(DL-126)
func TestTheUsersConfigCannotWeakenTheInPlaceRules(t *testing.T) {
	ctx := context.Background()
	const hook = "#!/bin/sh\necho ran > hook-ran\n"

	// husky builds a repo whose hooks run from a directory in the work
	// tree, with a committed symlink attack staged in a sandbox.
	husky := func(t *testing.T) (string, string, ExportedState) {
		dir, _ := initTestRepo(t, "")
		writeFile(t, dir, ".husky/pre-commit", "#!/bin/sh\n", 0o755)
		runGit(t, dir, "add", ".husky")
		head := commitHead(t, dir, "husky")
		runGit(t, dir, "config", "core.hooksPath", ".husky/_")
		state := sandboxCommits(t, dir, head, func(s string) {
			runGit(t, s, "rm", "-q", "-r", ".husky")
			writeFile(t, s, "tools/_/pre-commit", hook, 0o755)
			if err := os.Symlink("tools", filepath.Join(s, ".husky")); err != nil {
				t.Fatal(err)
			}
			runGit(t, s, "add", "-A")
		})
		return dir, head, state
	}

	t.Run("core.symlinks=false does not let a committed symlink plant a hook", func(t *testing.T) {
		dir, head, state := husky(t)
		runGit(t, dir, "config", "core.symlinks", "false")
		res, err := ApplySync(ctx, dir, "s", "main", state)
		wantFallback(t, dir, head, res, err, ".husky")
		userCommits(t, dir)
		wantAbsent(t, dir, "hook-ran")
	})

	t.Run("core.protectNTFS=false in the checkout does not admit a .git alias", func(t *testing.T) {
		dir, _ := initTestRepo(t, "")
		runGit(t, dir, "config", "core.protectNTFS", "false")
		runGit(t, dir, "config", "core.protectHFS", "false")
		head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
		state := sandboxCommits(t, dir, head, func(s string) {
			writeFile(t, s, "git~1/config", "[core]\n", 0o644)
			// A hostile sandbox builds the alias with git's own
			// protections off, as an attacker would.
			runGit(t, s, "-c", "core.protectNTFS=false", "-c", "core.protectHFS=false", "add", "-A")
		})
		res, err := ApplySync(ctx, dir, "s", "main", state)
		wantFallback(t, dir, head, res, err, "git~1/config")
	})

	t.Run("a sparse checkout falls back", func(t *testing.T) {
		dir, head := initTestRepo(t, "")
		runGit(t, dir, "config", "core.sparseCheckout", "true")
		res, err := ApplySync(ctx, dir, "s", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: head,
			WorktreeTar: makeTar(t, tarEntry{name: "a.txt", body: "x\n"}),
		})
		wantFallback(t, dir, head, res, err, "sparse")
		wantAbsent(t, dir, "a.txt")
	})

	t.Run("a relocated work tree falls back", func(t *testing.T) {
		dir, head := initTestRepo(t, "")
		runGit(t, dir, "config", "core.worktree", t.TempDir())
		res, err := ApplySync(ctx, dir, "s", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: head,
			WorktreeTar: makeTar(t, tarEntry{name: "a.txt", body: "x\n"}),
		})
		wantFallback(t, dir, head, res, err, "work tree")
	})

	t.Run("merge.ff=false and pull.ff=false do not break the in-place fast-forward", func(t *testing.T) {
		dir, head := initTestRepo(t, "")
		runGit(t, dir, "config", "merge.ff", "false")
		runGit(t, dir, "config", "pull.ff", "false")
		state := sandboxCommits(t, dir, head, func(s string) {
			writeFile(t, s, "added.txt", "x\n", 0o644)
			runGit(t, s, "add", "added.txt")
		})
		res, err := ApplySync(ctx, dir, "s", "main", state)
		if err != nil || !res.InPlace {
			t.Fatalf("an honest fast-forward with merge.ff=false: %+v, %v", res, err)
		}
		wantContent(t, dir, "added.txt", "x\n")
	})
}
