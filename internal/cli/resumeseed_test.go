package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/server"
)

// A resume from the transcript sends the session's base commit from its
// checkout, and says when it cannot; any other resume sends nothing.
//
// @decision(DL-098)
func TestAResumeSendsTheBaseFromTheCheckout(t *testing.T) {
	cfg := config.Config{Platforms: []config.Platform{{Name: "codefloe", Kind: "forgejo", BaseURL: "https://codefloe.com", Account: "alice"}}}
	wd := t.TempDir()
	base := initCLIGitRepo(t, wd, "https://codefloe.com/alice/spov.git")
	view := server.SessionView{
		Name:   "synced",
		Repos:  []server.RepoRef{{Platform: "codefloe", Owner: "alice", Name: "spov"}},
		Sync:   &server.SessionSyncView{Branch: "main", BaseCommit: base},
		Resume: &server.ResumeView{Mode: server.ResumeTranscript},
	}
	seed := func(workdir string, v server.SessionView) (string, string) {
		var errs bytes.Buffer
		env := &Env{Workdir: workdir, Err: &errs}
		checkout, err := env.verifySyncCheckout(context.Background(), cfg, v)
		return env.resumeSeed(context.Background(), v, checkout, err), errs.String()
	}

	got, note := seed(wd, view)
	if raw, err := base64.StdEncoding.DecodeString(got); err != nil || !bytes.HasPrefix(raw, []byte("# v2 git bundle")) || note != "" {
		t.Errorf("from the checkout: %d bytes, err %v, note %q", len(raw), err, note)
	}
	if got, note := seed(t.TempDir(), view); got != "" || !strings.Contains(note, "default branch") {
		t.Errorf("from elsewhere: %q, note %q", got, note)
	}
	snapshot := view
	snapshot.Resume = &server.ResumeView{Mode: server.ResumeSnapshot}
	unsynced := view
	unsynced.Sync = nil
	for _, v := range []server.SessionView{snapshot, unsynced} {
		if got, note := seed(wd, v); got != "" || note != "" {
			t.Errorf("sent %q, note %q", got, note)
		}
	}

	// A session with no platform gets the whole history even when the
	// checkout has a remote with refs, since the sandbox has no clone.
	runCLIGit(t, wd, "update-ref", "refs/remotes/origin/main", base)
	if err := os.WriteFile(filepath.Join(wd, "more.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLIGit(t, wd, "add", "more.txt")
	runCLIGit(t, wd, "commit", "-q", "-m", "more")
	head := strings.TrimSpace(runCLIGit(t, wd, "rev-parse", "HEAD"))
	for _, tc := range []struct {
		platform string
		whole    bool
	}{{"codefloe", false}, {"", true}} {
		v := view
		v.Repos = []server.RepoRef{{Platform: tc.platform, Owner: "alice", Name: "spov"}}
		v.Sync = &server.SessionSyncView{Branch: "main", BaseCommit: head}
		got, note := seed(wd, v)
		raw, _ := base64.StdEncoding.DecodeString(got)
		if len(raw) == 0 {
			t.Fatalf("platform %q: nothing sent, note %q", tc.platform, note)
		}
		header := raw[:bytes.Index(raw, []byte("\n\n"))]
		if whole := !bytes.Contains(header, []byte("\n-")); whole != tc.whole {
			t.Errorf("platform %q: the whole history = %v, want %v", tc.platform, whole, tc.whole)
		}
	}
}
