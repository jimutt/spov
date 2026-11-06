package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/server"
)

// launchedSync launches with --keep, so nothing is exported on exit, and
// reports whether the session started from the local checkout.
func launchedSync(t *testing.T, f *apiFixture, name string, args ...string) (*server.SessionSyncView, string) {
	t.Helper()
	_, stderr, err := runWith(t, f.env, append([]string{"fly", "--keep", "--name", name}, args...)...)
	if err != nil {
		t.Fatalf("fly %v: %v\n%s", args, err, stderr)
	}
	out, _, err := runWith(t, f.env, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Sessions []server.SessionView `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	for _, s := range body.Sessions {
		if s.Name == name {
			return s.Sync, stderr
		}
	}
	t.Fatalf("no session %q in %s", name, out)
	return nil, ""
}

// A plain `spov fly` inside a checkout launches that repository synced,
// and every other source of repositories launches unsynced.
//
// @decision(DL-095)
func TestAPlainFlyInACheckoutSyncsIt(t *testing.T) {
	t.Run("a clean checkout is launched synced", func(t *testing.T) {
		f := newAPIFixtureWithConfig(t, defaultConfig)
		head := initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
		sync, _ := launchedSync(t, f, "auto")
		if sync == nil || sync.Branch != "main" || sync.BaseCommit != head {
			t.Fatalf("sync = %+v, want main at %s", sync, head)
		}
	})

	t.Run("--repo is never synced by itself, even for the checkout's repository", func(t *testing.T) {
		f := newAPIFixtureWithConfig(t, platformOnlyConfig)
		initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
		if sync, _ := launchedSync(t, f, "named", "--repo", "alice/spov"); sync != nil {
			t.Fatalf("a --repo launch synced: %+v", sync)
		}
	})

	t.Run("a project file's repos win over the checkout", func(t *testing.T) {
		f := newAPIFixtureWithConfig(t, platformOnlyConfig)
		initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
		writeProject(t, f.wd, "repos:\n  - platform: codefloe\n    owner: alice\n    name: spov\n")
		runCLIGit(t, f.wd, "add", ".spov.yaml")
		runCLIGit(t, f.wd, "commit", "-m", "project file")
		if sync, _ := launchedSync(t, f, "project"); sync != nil {
			t.Fatalf("a project file's repos launch synced: %+v", sync)
		}
	})

	t.Run("the checkout wins over the user file's repos", func(t *testing.T) {
		f := newAPIFixtureWithConfig(t, defaultConfig)
		initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
		if sync, _ := launchedSync(t, f, "user-repos"); sync == nil {
			t.Fatal("the user file's repos beat the checkout")
		}
	})

	t.Run("outside a checkout the user file's repos launch unsynced", func(t *testing.T) {
		f := newAPIFixtureWithConfig(t, defaultConfig)
		if sync, stderr := launchedSync(t, f, "elsewhere"); sync != nil || strings.Contains(stderr, "sync") {
			t.Fatalf("sync = %+v, stderr = %q", sync, stderr)
		}
	})

	t.Run("--no-sync and sandbox.sync: false launch the configured repos", func(t *testing.T) {
		f := newAPIFixtureWithConfig(t, defaultConfig)
		initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
		if sync, _ := launchedSync(t, f, "flag-off", "--no-sync"); sync != nil {
			t.Fatalf("--no-sync synced: %+v", sync)
		}

		off := newAPIFixtureWithConfig(t, defaultConfig+"sandbox:\n  sync: false\n")
		initCLIGitRepo(t, off.wd, "https://codefloe.com/alice/spov.git")
		if sync, _ := launchedSync(t, off, "config-off"); sync != nil {
			t.Fatalf("sandbox.sync: false synced: %+v", sync)
		}
	})

	t.Run("a checkout no platform matches launches from its bundle, with a note", func(t *testing.T) {
		f := newAPIFixtureWithConfig(t, platformOnlyConfig)
		initCLIGitRepo(t, f.wd, "")
		sync, stderr := launchedSync(t, f, "local")
		if sync == nil {
			t.Fatal("a checkout with no remote was not synced")
		}
		if !strings.Contains(stderr, "has no remote") || !strings.Contains(stderr, "cannot push") {
			t.Errorf("no note about the bundle:\n%s", stderr)
		}
	})

	t.Run("an explicit --sync=false launches the configured repos", func(t *testing.T) {
		f := newAPIFixtureWithConfig(t, defaultConfig)
		initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
		if sync, _ := launchedSync(t, f, "explicit-off", "--sync=false"); sync != nil {
			t.Fatalf("--sync=false synced: %+v", sync)
		}
	})
}

// Several platforms matching the checkout is DL-066's error, and in the
// automatic mode its advice keeps the launch synced.
//
// @decision(DL-095) @decision(DL-066)
func TestAPlainFlyOnAnAmbiguousCheckoutSaysToKeepSync(t *testing.T) {
	f := newAPIFixtureWithConfig(t, `platforms:
  - name: work
    kind: forgejo
    base_url: https://codefloe.com
    account: bob
  - name: home
    kind: forgejo
    base_url: https://codefloe.com
    account: carol
`)
	initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
	_, _, err := runWith(t, f.env, "fly", "--keep", "--name", "ambiguous")
	if err == nil {
		t.Fatal("the launch went ahead")
	}
	for _, want := range []string{"several configured platforms", "--repo <platform>:alice/spov", "with --sync", "--no-sync"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// A checkout that cannot be synced stops a plain `spov fly` before the
// server is asked, and says how to launch without it.
//
// @decision(DL-095)
func TestAPlainFlyRefusesACheckoutItCannotSync(t *testing.T) {
	cases := []struct {
		name  string
		dirty func(t *testing.T, dir string)
		want  string
	}{
		{"uncommitted changes", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "uncommitted"},
		{"a detached HEAD", func(t *testing.T, dir string) {
			runCLIGit(t, dir, "checkout", "--detach")
		}, "detached HEAD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixtureWithConfig(t, defaultConfig)
			initCLIGitRepo(t, f.wd, "https://codefloe.com/alice/spov.git")
			tc.dirty(t, f.wd)
			_, _, err := runWith(t, f.env, "fly", "--keep", "--name", "refused")
			if err == nil {
				t.Fatal("the launch went ahead")
			}
			for _, want := range []string{tc.want, "by default", "--no-sync"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if f.provider.Running() != 0 {
				t.Errorf("a sandbox was created: running=%d", f.provider.Running())
			}
		})
	}
}
