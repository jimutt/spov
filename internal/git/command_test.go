package git

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestEveryGitCommandIsBuiltByCommand keeps the one place git is
// started the only place: a process spawned anywhere else would run the
// checkout's hooks and filesystem monitor.
//
// @decision(DL-136)
func TestEveryGitCommandIsBuiltByCommand(t *testing.T) {
	spawn := regexp.MustCompile(`exec\.Command(Context)?\([^)]*"git"`)
	root := filepath.Join("..", "..")
	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepathWalk(filepath.Join(root, dir), func(path string) {
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "command.go" {
				return
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if spawn.Match(b) {
				offenders = append(offenders, path)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("these files start git without git.Command: %v", offenders)
	}
}

func filepathWalk(dir string, visit func(path string)) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			visit(path)
		}
		return nil
	})
}

// @decision(DL-136)
func TestACommandRunsWithNoHooksNoMonitorNoPagerAndNoExternalDiff(t *testing.T) {
	t.Setenv("GIT_EXTERNAL_DIFF", "/bin/echo")
	t.Setenv("GIT_PAGER", "evil-pager")
	cmd := Command(context.Background(), t.TempDir(), nil, "status")
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--no-pager", "-c core.hooksPath=/dev/null", "-c core.fsmonitor=false"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if !slices.Contains(cmd.Env, "GIT_PAGER=cat") {
		t.Errorf("GIT_PAGER is not cat: %v", cmd.Env)
	}
	if slices.Contains(cmd.Env, "GIT_PAGER=evil-pager") || slices.Contains(cmd.Env, "GIT_EXTERNAL_DIFF=/bin/echo") {
		t.Error("the user's pager or external diff reaches the command")
	}
	// The one question about the real hooks directory is the exception.
	if kept := strings.Join(commandReadingHooksPath(context.Background(), ".", "rev-parse").Args, " "); strings.Contains(kept, "hooksPath") {
		t.Errorf("the hooks directory read is guarded: %q", kept)
	}
}

// rigged is a checkout whose own configuration would run programs: the
// hooks directory holds every hook a sync could trigger, core.fsmonitor
// names a program, and aliases shadow the commands spov uses. Each one
// writes a line to the marker file when it runs.
func rigged(t *testing.T) (dir, head, marker string) {
	t.Helper()
	dir, head = initTestRepo(t, "https://codefloe.com/alice/spov.git")
	marker = filepath.Join(t.TempDir(), "ran")
	hooks := t.TempDir()
	script := "#!/bin/sh\necho \"$0\" >> " + marker + "\ncat >/dev/null\n"
	for _, h := range []string{"post-checkout", "post-merge", "post-rewrite", "reference-transaction", "pre-commit", "post-commit", "post-index-change", "pre-auto-gc"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	monitor := filepath.Join(t.TempDir(), "monitor")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\necho monitor >> "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "config", "core.hooksPath", hooks)
	runGit(t, dir, "config", "core.fsmonitor", monitor)
	for _, a := range []string{"status", "reset", "rev-parse", "update-ref", "diff", "ls-tree", "fetch"} {
		runGit(t, dir, "config", "alias."+a, "!echo alias-"+a+" >> "+marker)
	}
	return dir, head, marker
}

// @decision(DL-136)
func TestTheRiggedCheckoutDoesRunItsHooksForPlainGit(t *testing.T) {
	// The control for the guard below: without it, a git that ignored
	// the rig would pass the guard for nothing.
	dir, _, marker := rigged(t)
	runGit(t, dir, "update-ref", "refs/heads/control", "HEAD")
	runGit(t, dir, "status")
	b, _ := os.ReadFile(marker)
	if !strings.Contains(string(b), "reference-transaction") || !strings.Contains(string(b), "monitor") {
		t.Skipf("this git does not run the rig's hook or monitor (marker %q)", b)
	}
	if strings.Contains(string(b), "alias-") {
		t.Errorf("git ran an alias in place of a built-in: %q", b)
	}
}

// @decision(DL-136)
func TestAFullSyncBackRunsNoHookNoMonitorAndNoAlias(t *testing.T) {
	ctx := context.Background()
	t.Run("in place", func(t *testing.T) {
		dir, head, marker := rigged(t)
		_, sandboxHead, bundle := sandboxCommit(t, dir, head, "committed.txt")
		if _, err := InspectLocalCheckout(ctx, dir, nil, true); err != nil {
			t.Fatalf("inspect: %v", err)
		}
		if _, err := BundleCommit(ctx, dir, "origin", head); err != nil {
			t.Logf("bundle: %v", err)
		}
		if _, err := BundleHEAD(ctx, dir); err != nil {
			t.Fatalf("bundle head: %v", err)
		}
		if _, err := InspectCommandOutput(ctx, dir, "rev-parse", "--show-toplevel"); err != nil {
			t.Fatal(err)
		}
		res, err := ApplySync(ctx, dir, "s1", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: sandboxHead, Bundle: bundle,
			WorktreeTar: makeWorktreeTar(t, map[string]string{"dirty.txt": "x\n"}),
		})
		if err != nil || !res.InPlace {
			t.Fatalf("result = %+v, %v", res, err)
		}
		assertNoMarker(t, marker)
	})
	t.Run("on a branch", func(t *testing.T) {
		dir, head, marker := rigged(t)
		_, sandboxHead, bundle := sandboxCommit(t, dir, head, "committed.txt")
		if err := os.WriteFile(filepath.Join(dir, "local.txt"), []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := ApplySync(ctx, dir, "s2", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: sandboxHead, Bundle: bundle,
			WorktreeTar: makeWorktreeTar(t, map[string]string{"dirty.txt": "x\n"}),
		})
		if err != nil || res.InPlace || res.FallbackBranch == "" {
			t.Fatalf("result = %+v, %v", res, err)
		}
		assertNoMarker(t, marker)
	})
}

func assertNoMarker(t *testing.T, marker string) {
	t.Helper()
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("the checkout ran a program of its own during the sync: %q", b)
	}
}

// sandboxCommit clones dir as a sandbox would, commits one file and
// returns the clone, its head and the bundle of the new commit.
func sandboxCommit(t *testing.T, dir, base, file string) (sandbox, head string, bundle []byte) {
	t.Helper()
	sandbox = filepath.Join(t.TempDir(), "sandbox")
	// A plain clone, so the rig's configuration is not carried along.
	runGit(t, filepath.Dir(sandbox), "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "clone", "-q", dir, sandbox)
	runGit(t, sandbox, "config", "user.name", "Agent")
	runGit(t, sandbox, "config", "user.email", "agent@example.com")
	if err := os.WriteFile(filepath.Join(sandbox, file), []byte("from the sandbox\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, sandbox, "add", file)
	runGit(t, sandbox, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "sandbox commit")
	head = strings.TrimSpace(runGit(t, sandbox, "rev-parse", "HEAD"))
	bf := filepath.Join(t.TempDir(), "s.bundle")
	runGit(t, sandbox, "bundle", "create", bf, base+"..HEAD")
	raw, err := os.ReadFile(bf)
	if err != nil {
		t.Fatal(err)
	}
	return sandbox, head, raw
}
