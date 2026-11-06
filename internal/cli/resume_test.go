package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// resumeFixture is a server that keeps its sessions, with one session
// launched through `spov fly --keep` and then stopped.
func resumeFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	f.http.Close()
	f.srv.Close()
	f.adjustDeps = func(d *server.Deps) { d.Persistent = true }
	f.startServer()
	f.env.Client = client.New(f.http.URL, f.token)
	if _, _, err := runWith(t, f.env, "fly", "--name", "work", "--keep"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runWith(t, f.env, "stop", "work"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *apiFixture) state(ref string) store.SessionState {
	f.t.Helper()
	v, err := f.env.Client.GetSession(context.Background(), ref)
	if err != nil {
		f.t.Fatal(err)
	}
	return v.State
}

// `spov resume` says how the session comes back, asks before a
// transcript resume, and attaches as fly does: the TUI runs, and leaving
// it stops the session again unless --keep.
//
// @decision(DL-090)
func TestResumeAsksThenAttaches(t *testing.T) {
	f := resumeFixture(t)
	runs := f.tuiRuns
	asked := ""
	f.env.Confirm = func(prompt string) (bool, error) { asked = prompt; return false, nil }
	if _, stderr, err := runWith(t, f.env, "resume", "work"); err == nil || !strings.Contains(stderr, "from its transcript") || asked == "" {
		t.Fatalf("a refused transcript resume: %v\n%s", err, stderr)
	}
	if f.state("work") != store.StateStopped {
		t.Fatal("a refused resume reopened the session")
	}
	f.env.Confirm = func(string) (bool, error) { return true, nil }
	if _, stderr, err := runWith(t, f.env, "resume", "work"); err != nil {
		t.Fatalf("resume: %v\n%s", err, stderr)
	}
	if f.tuiRuns != runs+1 {
		t.Error("the TUI was not run")
	}
	if f.state("work") != store.StateStopped {
		t.Error("leaving the TUI did not stop the resumed session")
	}
	if _, _, err := runWith(t, f.env, "resume", "work", "--yes", "--keep"); err != nil {
		t.Fatal(err)
	}
	if f.state("work") != store.StateRunning {
		t.Error("--keep stopped the session")
	}
}

// Off a terminal, a transcript resume needs --yes, and --json prints the
// view without attaching.
//
// @decision(DL-090)
func TestResumeOffATerminal(t *testing.T) {
	f := resumeFixture(t)
	f.env.Confirm = func(string) (bool, error) { return false, errors.New("no terminal to ask on") }
	if _, _, err := runWith(t, f.env, "resume", "work"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v", err)
	}
	out, _, err := runWith(t, f.env, "resume", "work", "--yes", "--json")
	if err != nil || !strings.Contains(out, `"state": "starting"`) && !strings.Contains(out, `"state":"starting"`) {
		t.Fatalf("--json: %v\n%s", err, out)
	}
}

// `spov attach` on a stopped session offers the resume, and joins once
// it runs; refusing, or no terminal, names `spov resume`.
//
// @decision(DL-090) @decision(DL-025)
func TestAttachOffersToResumeAStoppedSession(t *testing.T) {
	f := resumeFixture(t)
	f.env.Confirm = func(string) (bool, error) { return false, nil }
	if _, _, err := runWith(t, f.env, "attach", "work"); err == nil || !strings.Contains(err.Error(), "spov resume work") {
		t.Fatalf("declined: %v", err)
	}
	f.env.Confirm = func(string) (bool, error) { return false, errors.New("no terminal to ask on") }
	if _, _, err := runWith(t, f.env, "attach", "work"); err == nil || !strings.Contains(err.Error(), "spov resume work") {
		t.Fatalf("no terminal: %v", err)
	}
	f.env.Confirm = func(string) (bool, error) { return true, nil }
	f.env.AttachPoll = 5 * time.Millisecond
	runs := f.tuiRuns
	if _, stderr, err := runWith(t, f.env, "attach", "work"); err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if f.tuiRuns != runs+1 || f.state("work") != store.StateRunning {
		t.Errorf("after attach's resume: tui runs %d, state %s", f.tuiRuns-runs, f.state("work"))
	}
}

// A live session is not resumed; the error says how to join it.
//
// @decision(DL-090)
func TestResumeRefusesALiveSession(t *testing.T) {
	f := newAPIFixture(t)
	if _, _, err := runWith(t, f.env, "fly", "--name", "live", "--keep"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runWith(t, f.env, "resume", "live"); err == nil || !strings.Contains(err.Error(), "spov attach live") {
		t.Errorf("err = %v", err)
	}
}

// `spov resume` completes the caller's resumable sessions, and `ls`
// marks one with a snapshot; --no-attach resumes without the TUI.
//
// @decision(DL-090) @decision(DL-023)
func TestResumeCompletesAndResumesWithoutAttaching(t *testing.T) {
	f := resumeFixture(t)
	if got := completions(t, f.env, "resume", ""); strings.Join(got, " ") != "work" {
		t.Errorf("resume offers %v", got)
	}
	runs := f.tuiRuns
	if _, stderr, err := runWith(t, f.env, "resume", "work", "--yes", "--no-attach"); err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if f.tuiRuns != runs || f.state("work") != store.StateRunning {
		t.Errorf("--no-attach: tui runs %d, state %s", f.tuiRuns-runs, f.state("work"))
	}
}

// What cannot be resumed says why, and a refusal keeps the server's
// reason.
//
// @decision(DL-090)
func TestWhatCannotBeResumedSaysWhy(t *testing.T) {
	for _, c := range []struct {
		view server.SessionView
		want string
	}{
		{server.SessionView{Name: "a", State: store.StateStarting}, "spov attach a"},
		{server.SessionView{Name: "b", State: store.StateStopped, Job: &server.SessionJobView{ID: "j1"}}, "spov jobs show j1"},
		{server.SessionView{Name: "c", State: store.StateStopped}, "spov fly"},
	} {
		if err := notResumable(c.view); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.view.Name, err)
		}
	}
	if w := resumeWords(&server.ResumeView{Mode: server.ResumeSnapshot, Pending: true}); !strings.Contains(w, "still being taken") {
		t.Errorf("pending: %s", w)
	}
	if _, err := (&Env{}).confirm("?"); err == nil {
		t.Error("asked with no terminal")
	}
	f := resumeFixture(t)
	if _, _, err := runWith(t, f.env, "resume", "work", "--yes", "--timeout", "100h"); err == nil || !strings.Contains(err.Error(), "resume work") {
		t.Errorf("a refused resume: %v", err)
	}
}

// A local server keeps its sessions: after a restart the stopped
// session is still listed and resumes, and its history is on disk in
// the documented layout.
//
// @decision(DL-091) @decision(DL-092)
func TestALocalServerResumesAfterARestart(t *testing.T) {
	f := newAPIFixture(t)
	data := t.TempDir()
	f.http.Close()
	f.srv.Close()
	f.serverEnv.Environ.(config.MapEnv).Vars["XDG_DATA_HOME"] = data
	f.startServer()
	f.env.Client = client.New(f.http.URL, f.token)
	if !f.deps.Persistent {
		t.Fatal("a local server with a data directory is not persistent")
	}
	if _, _, err := runWith(t, f.env, "fly", "--name", "kept", "--keep"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runWith(t, f.env, "stop", "kept"); err != nil {
		t.Fatal(err)
	}
	f.http.Close()
	f.srv.Close()
	f.startServer()
	f.env.Client = client.New(f.http.URL, f.token)
	if f.state("kept") != store.StateStopped {
		t.Fatal("the stopped session was forgotten across the restart")
	}
	if _, stderr, err := runWith(t, f.env, "resume", "kept", "--yes", "--keep"); err != nil {
		t.Fatalf("resume after a restart: %v\n%s", err, stderr)
	}
	if f.state("kept") != store.StateRunning {
		t.Error("the resumed session is not running")
	}
	dirs, _ := os.ReadDir(filepath.Join(data, "spov", "sessions"))
	found := false
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(data, "spov", "sessions", d.Name(), "session.json")); err == nil {
			found = true
		}
	}
	if !found {
		t.Errorf("no session.json under %s: %v", data, dirs)
	}
}
