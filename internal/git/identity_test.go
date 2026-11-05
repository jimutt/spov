package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveIdentityLayersHalvesIndependently: configuring only an
// email is a real setup, and it must not discard the detected name.
//
// @decision(DL-044)
func TestResolveIdentityLayersHalvesIndependently(t *testing.T) {
	local := Identity{Name: "Local Name", Email: "local@example.com"}
	detect := func() Identity { return local }

	tests := []struct {
		name     string
		explicit Identity
		want     Identity
	}{
		{"nothing explicit", Identity{}, local},
		{"both explicit", Identity{Name: "N", Email: "e@x"}, Identity{Name: "N", Email: "e@x"}},
		{"only email", Identity{Email: "e@x"}, Identity{Name: "Local Name", Email: "e@x"}},
		{"only name", Identity{Name: "N"}, Identity{Name: "N", Email: "local@example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveIdentity(tt.explicit, detect); got != tt.want {
				t.Errorf("ResolveIdentity(%+v) = %+v, want %+v", tt.explicit, got, tt.want)
			}
		})
	}
}

// TestResolveIdentitySkipsDetectionWhenComplete: detection shells out to
// git, so a fully explicit identity must not pay for it.
//
// @decision(DL-044)
func TestResolveIdentitySkipsDetectionWhenComplete(t *testing.T) {
	called := false
	got := ResolveIdentity(Identity{Name: "N", Email: "e@x"}, func() Identity {
		called = true
		return Identity{}
	})
	if called {
		t.Error("the local git config was read despite a complete explicit identity")
	}
	if !got.Complete() {
		t.Errorf("got %+v", got)
	}
}

func TestIdentityCompleteAndEmpty(t *testing.T) {
	tests := []struct {
		id              Identity
		complete, empty bool
	}{
		{Identity{}, false, true},
		{Identity{Name: "N"}, false, false},
		{Identity{Email: "e@x"}, false, false},
		{Identity{Name: "N", Email: "e@x"}, true, false},
	}
	for _, tt := range tests {
		if got := tt.id.Complete(); got != tt.complete {
			t.Errorf("%+v.Complete() = %v", tt.id, got)
		}
		if got := tt.id.Empty(); got != tt.empty {
			t.Errorf("%+v.Empty() = %v", tt.id, got)
		}
	}
}

// TestLocalIdentityReadsGitConfig runs the real git, so what is asserted
// is git's own answer rather than an assumption about where it looks.
//
// @decision(DL-044)
func TestLocalIdentityReadsGitConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	// An isolated HOME so the developer's real identity is not read, and
	// the test cannot depend on it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	ctx := context.Background()
	if got := LocalIdentity(ctx); !got.Empty() {
		t.Fatalf("an empty HOME yielded %+v, want nothing", got)
	}

	cfg := "[user]\n\tname = Test Person\n\temail = test@example.com\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LocalIdentity(ctx)
	if got.Name != "Test Person" || got.Email != "test@example.com" {
		t.Errorf("LocalIdentity() = %+v, want the configured identity", got)
	}
}

// TestLocalIdentityWithNoGit: a machine without git degrades to nothing
// rather than failing the launch.
//
// @decision(DL-044)
func TestLocalIdentityWithNoGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := LocalIdentity(context.Background()); !got.Empty() {
		t.Errorf("LocalIdentity() = %+v with no git on PATH, want nothing", got)
	}
}

// TestLocalIdentityReportsABrokenConfig: git exits 128 for a config it
// cannot parse. Treating that as "no identity set" sends the user to fix
// a value in the very file git is refusing to read.
//
// @decision(DL-044)
func TestLocalIdentityReportsABrokenConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	if err := os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("this is not valid ini [[[\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LocalIdentityIn(context.Background(), "")
	if err == nil {
		t.Fatal("a malformed git config was reported as no identity")
	}
	// git's own message names the file and line; passing it through is
	// the whole point.
	if !strings.Contains(err.Error(), ".gitconfig") {
		t.Errorf("the error does not name the offending file: %v", err)
	}
}

// TestLocalIdentityUnsetIsNotAnError: an unset key exits 1, which is the
// ordinary case for someone who has never configured git.
//
// @decision(DL-044)
func TestLocalIdentityUnsetIsNotAnError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	got, err := LocalIdentityIn(context.Background(), "")
	if err != nil {
		t.Errorf("an unconfigured git reported an error: %v", err)
	}
	if !got.Empty() {
		t.Errorf("got %+v, want nothing", got)
	}
}

// TestLocalIdentityInHonoursTheDirectory checks that an includeIf.gitdir
// rule resolves against the directory git runs in.
//
// @decision(DL-044)
func TestLocalIdentityInHonoursTheDirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	// gitdir matches against a repository's location, so the rule only
	// applies inside one — a bare directory keeps the personal identity
	// however the includeIf is written.
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepo := exec.Command("git", "init", "-q", work)
	initRepo.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := initRepo.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	workConfig := filepath.Join(home, ".gitconfig-work")
	if err := os.WriteFile(workConfig,
		[]byte("[user]\n\temail = work@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// gitdir requires a trailing slash to match a directory tree.
	main := "[user]\n\tname = Test Person\n\temail = personal@example.com\n" +
		"[includeIf \"gitdir:" + work + "/\"]\n\tpath = " + workConfig + "\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	personal, err := LocalIdentityIn(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if personal.Email != "personal@example.com" {
		t.Errorf("outside the work tree: email = %q, want the personal one", personal.Email)
	}

	inWork, err := LocalIdentityIn(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if inWork.Email != "work@example.com" {
		t.Errorf("inside the work tree: email = %q, want the work one", inWork.Email)
	}
}
