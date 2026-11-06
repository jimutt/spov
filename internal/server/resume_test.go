package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// resumable is a session launched under the proxy with snapshots on, on
// a server that keeps its sessions, then stopped.
func resumable(t *testing.T, more ...func(*Deps)) *renewFixture {
	t.Helper()
	f := newRecoverFixtureWith(t, func(d *Deps) {
		withSnapshots(d)
		d.Persistent = true
		for _, m := range more {
			m(d)
		}
	})
	if res, body := f.do(http.MethodDelete, "/api/v1/sessions/"+f.id, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("stop: %d %s", res.StatusCode, body)
	}
	return f
}

func (f *renewFixture) resume(body any) (int, SessionView, string) {
	f.t.Helper()
	res, raw := f.do(http.MethodPost, "/api/v1/sessions/"+f.id+"/resume", body)
	var view SessionView
	_ = json.Unmarshal(raw, &view)
	return res.StatusCode, view, string(raw)
}

// waitRunning waits for the session's launch to finish.
func (f *renewFixture) waitRunning() store.Session {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess := f.row(); sess.State != store.StateStarting {
			return sess
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("the resume never finished launching")
	return store.Session{}
}

func (f *renewFixture) stored(name string) []store.Event {
	f.t.Helper()
	evs, err := f.store.Conversations().ListEvents(context.Background(), auth.LocalID, f.id, 0, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range evs {
		if ev.Name == name {
			out = append(out, ev)
		}
	}
	return out
}

// A stopped session whose exit snapshot is kept resumes from it: the
// same row, a new sandbox booted from the snapshot under a run name and
// tag, a new run, and a seam in the log. The view told the owner so
// beforehand.
//
// @decision(DL-087) @decision(DL-088)
func TestAStoppedSessionResumesFromItsSnapshot(t *testing.T) {
	f := resumable(t)
	first := f.handle()
	f.srv.svc.CollectSnapshots(context.Background())
	res, raw := f.do(http.MethodGet, "/api/v1/sessions/"+f.id, nil)
	var before SessionView
	_ = json.Unmarshal(raw, &before)
	if res.StatusCode != http.StatusOK || before.Resume == nil || before.Resume.Mode != ResumeSnapshot || before.Resume.Until == nil {
		t.Fatalf("the stopped session's view: %s", raw)
	}

	code, view, body := f.resume(map[string]any{})
	if code != http.StatusAccepted || view.ID != f.id || view.State != store.StateStarting {
		t.Fatalf("resume: %d %s", code, body)
	}
	sess := f.waitRunning()
	if sess.State != store.StateRunning || sess.SandboxID == first.ID() {
		t.Fatalf("after the resume: %+v", sess)
	}
	specs := f.provider.Specs()
	spec := specs[len(specs)-1]
	if spec.ImageID != "im-snap-"+first.ID() || spec.Name != sess.Name+"-run2" || spec.Tags[sandbox.TagRun] != "2" ||
		spec.Tags[sandbox.TagName] != sess.Name || !spec.ExitSnapshot {
		t.Errorf("the resumed sandbox's spec: %+v", spec)
	}
	if !strings.Contains(strings.Join(spec.Command, " "), "/workspace/") {
		t.Errorf("no restored check in the entrypoint: %v", spec.Command)
	}
	run := f.run()
	if run.N != 2 || run.Origin != store.RunFromSnapshot || run.BootedImageID != "im-snap-"+first.ID() || !run.Ran() || run.SandboxID != sess.SandboxID {
		t.Errorf("run 2 = %+v", run)
	}
	if seams := f.stored(MarkerResumed); len(seams) != 1 || !strings.Contains(seams[0].Text, "snapshot") {
		t.Errorf("seams = %+v", seams)
	}
}

// A snapshot still being taken is waited for; one that cannot be had in
// time refuses the resume, and the next is offered the transcript.
//
// @decision(DL-088)
func TestAResumeWaitsForAPendingSnapshot(t *testing.T) {
	f := resumable(t)
	h := f.handle()
	// Stopped in the store, still exiting at Modal: pending.
	h.End()
	code, _, body := f.resume(map[string]any{})
	if code != http.StatusAccepted {
		t.Fatalf("a snapshot that settles within the wait: %d %s", code, body)
	}
	if f.waitRunning(); f.run().Origin != store.RunFromSnapshot {
		t.Errorf("run = %+v", f.run())
	}

	g := resumable(t)
	g.provider.SnapshotErrs = map[string]error{g.handle().ID(): sandbox.ErrSnapshotPending}
	code, _, body = g.resume(map[string]any{})
	if code != http.StatusConflict || !strings.Contains(body, "transcript") {
		t.Fatalf("a snapshot not had in time: %d %s", code, body)
	}
	_, raw := g.do(http.MethodGet, "/api/v1/sessions/"+g.id, nil)
	var view SessionView
	_ = json.Unmarshal(raw, &view)
	if view.Resume == nil || view.Resume.Mode != ResumeTranscript {
		t.Errorf("after the refusal the view offers %+v", view.Resume)
	}
	if code, _, body := g.resume(map[string]any{}); code != http.StatusAccepted {
		t.Errorf("the transcript resume: %d %s", code, body)
	}
}

// With no snapshot the session resumes from its transcript: a fresh
// sandbox with no image ID, the transcript note in its guide, and the
// import attempted and noted when it fails.
//
// @decision(DL-089)
func TestWithNoSnapshotAResumeIsFromTheTranscript(t *testing.T) {
	f := resumable(t, func(d *Deps) { d.Snapshots = nil })
	ctx := context.Background()
	conv, err := f.store.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(time.Now()), OrgID: auth.LocalID, SessionID: f.id, OpenCodeID: "ses_1", Info: []byte(`{"id":"ses_1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Conversations().PutMessages(ctx, []store.Message{{
		ID: store.NewID(time.Now()), OrgID: auth.LocalID, SessionID: f.id, ConversationID: conv.ID,
		OpenCodeID: "msg_1", Role: "user", Text: "hi", Raw: []byte(`{"id":"msg_1"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	code, view, body := f.resume(map[string]any{})
	if code != http.StatusAccepted || view.State != store.StateStarting {
		t.Fatalf("resume: %d %s", code, body)
	}
	f.waitRunning()
	spec := f.provider.Specs()[len(f.provider.Specs())-1]
	if spec.ImageID != "" || spec.ExitSnapshot {
		t.Errorf("a transcript resume booted %+v", spec)
	}
	if run := f.run(); run.Origin != store.RunFromTranscript || run.N != 2 {
		t.Errorf("run = %+v", run)
	}
	// The fake sandbox runs no OpenCode, so the import fails and says so.
	if failed := f.stored(MarkerImportFailed); len(failed) != 1 {
		t.Errorf("import notes = %+v", failed)
	}
}

// Resume refuses what it cannot reopen, before anything starts.
//
// @decision(DL-087)
func TestResumeRefusals(t *testing.T) {
	t.Run("a live session", func(t *testing.T) {
		f := newRecoverFixtureWith(t, func(d *Deps) { d.Persistent = true })
		if code, _, body := f.resume(map[string]any{}); code != http.StatusConflict {
			t.Errorf("%d %s", code, body)
		}
	})
	t.Run("a server that keeps nothing", func(t *testing.T) {
		f := resumable(t, func(d *Deps) { d.Persistent = false })
		if code, _, body := f.resume(map[string]any{}); code != http.StatusNotImplemented {
			t.Errorf("%d %s", code, body)
		}
	})
	t.Run("a session that never ran", func(t *testing.T) {
		f := resumable(t)
		run := f.run()
		run.RunningAt = nil
		if err := f.store.Runs().Update(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if code, _, body := f.resume(map[string]any{}); code != http.StatusConflict || !strings.Contains(body, "never ran") {
			t.Errorf("%d %s", code, body)
		}
	})
	t.Run("a timeout over the limit", func(t *testing.T) {
		f := resumable(t)
		if code, _, body := f.resume(map[string]any{"timeout_secs": 13 * 3600}); code != http.StatusUnprocessableEntity {
			t.Errorf("%d %s", code, body)
		}
	})
	t.Run("a message with no text", func(t *testing.T) {
		f := resumable(t)
		if code, _, body := f.resume(map[string]any{"message": map[string]any{"text": " "}}); code != http.StatusUnprocessableEntity {
			t.Errorf("%d %s", code, body)
		}
	})
	t.Run("someone else's session", func(t *testing.T) {
		f := resumable(t)
		admin := auth.Principal{OrgID: auth.LocalID, UserID: "an-admin", Admin: true}
		_, err := f.srv.svc.Resume(context.Background(), admin, f.id, ResumeRequest{})
		var se huma.StatusError
		if !errors.As(err, &se) || se.GetStatus() != http.StatusForbidden {
			t.Errorf("an admin resumed: %v", err)
		}
	})
}

// A session whose name a live session has taken since resumes under the
// next free name, and says so; a stopped session is found by its name.
//
// @decision(DL-087)
func TestAResumeTakesAFreeName(t *testing.T) {
	f := resumable(t)
	name := f.row().Name
	if got, err := f.srv.svc.Get(context.Background(), auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}, name); err != nil || got.ID != f.id {
		t.Fatalf("the stopped session by name: %+v, %v", got, err)
	}
	holder := store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID, Name: name, State: store.StateRunning}
	if err := f.store.Sessions().Create(context.Background(), holder); err != nil {
		t.Fatal(err)
	}
	code, view, body := f.resume(map[string]any{})
	if code != http.StatusAccepted || view.Name != name+"-2" {
		t.Fatalf("resume: %d %s", code, body)
	}
	if seams := f.stored(MarkerResumed); len(seams) != 1 || !strings.Contains(seams[0].Text, name+"-2") {
		t.Errorf("seams = %+v", seams)
	}
}

// A resume's message is checked when the resume is asked, kept with the
// run, and sent once the session runs; one that cannot be delivered is
// noted, and the run no longer holds it.
//
// @decision(DL-087)
func TestAResumesFirstMessageIsSentOnceRunning(t *testing.T) {
	f := resumable(t)
	code, _, body := f.resume(map[string]any{"message": map[string]any{"text": "carry on", "id": "msg_resume1"}})
	if code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", code, body)
	}
	f.waitRunning()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.run().FirstMessage) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(f.run().FirstMessage) > 0 {
		t.Fatal("the first message was never taken off the run")
	}
	// The fake sandbox answers no prompt, so delivery fails and is noted.
	if failed := f.stored(MarkerFirstMessageFailed); len(failed) != 1 {
		t.Errorf("notes = %+v", failed)
	}
}

// The timeout is the launch's, cut to the limit, unless one is asked,
// which the limit refuses.
//
// @decision(DL-087) @decision(DL-009)
func TestResumeTimeout(t *testing.T) {
	for _, c := range []struct {
		launched time.Duration
		secs     int
		limit    time.Duration
		want     time.Duration
		err      bool
	}{
		{time.Hour, 0, 8 * time.Hour, time.Hour, false},
		{10 * time.Hour, 0, 8 * time.Hour, 8 * time.Hour, false},
		{time.Hour, 7200, 8 * time.Hour, 2 * time.Hour, false},
		{time.Hour, 9 * 3600, 8 * time.Hour, 0, true},
	} {
		got, err := resumeTimeout(c.launched, c.secs, c.limit)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%+v: %s, %v", c, got, err)
		}
	}
}

// The entrypoint takes up a restored workspace instead of cloning, and
// rebuilds the trust store; a transcript resume says so in the guide.
//
// @decision(DL-088) @decision(DL-089)
func TestTheEntrypointsResumeModes(t *testing.T) {
	base := opencode.Entrypoint{Port: 4096, Proxied: true,
		Clones:    []opencode.Clone{{URL: "https://git.test/acme/app.git", Dir: "app"}},
		Workspace: []opencode.WorkspaceRepo{{Dir: "app"}}}
	snap := base
	snap.Resume = opencode.ResumeSnapshot
	script, err := snap.Script()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "git clone") || !strings.Contains(script, "[ -d '/workspace/app' ]") ||
		!strings.Contains(script, "update-ca-certificates --fresh") {
		t.Errorf("snapshot mode:\n%s", script)
	}
	tr := base
	tr.Resume = opencode.ResumeTranscript
	script, _ = tr.Script()
	if !strings.Contains(script, "git clone") && !strings.Contains(script, "clone") || !strings.Contains(script, "resumed from its transcript") {
		t.Errorf("transcript mode:\n%s", script)
	}
}
