package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// putAsk appends a permission frame naming its request, in the measured
// shape: permission.asked at data.id, permission.replied at
// data.requestID.
func (f *followFixture) putAsk(t *testing.T, name, id string) {
	t.Helper()
	raw := `{"type":"` + name + `","data":{"id":"` + id + `","sessionID":"ses"}}`
	if name == eventPermissionReplied {
		raw = `{"type":"` + name + `","data":{"sessionID":"ses","requestID":"` + id + `","reply":"once"}}`
	}
	ev := store.Event{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		ConversationID: f.conv, Source: store.SourceEphemeral, Name: name, At: time.Now(), Raw: []byte(raw),
	}
	if _, err := f.st.Conversations().PutEvents(context.Background(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
}

// putAnswered appends the server's record of a person's answer.
func (f *followFixture) putAnswered(t *testing.T, id string) {
	t.Helper()
	ev := store.Event{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		Source: store.SourceSpov, Name: MarkerAnswered, Text: "permission " + id + " answered by u",
		At: time.Now(), Raw: []byte(`{"requestID":"` + id + `"}`),
	}
	if _, err := f.st.Conversations().PutEvents(context.Background(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
}

// A person's answer through spov releases a blocked job, whether its
// replied frame is stored yet or not; the sandbox's replied frame alone
// releases nothing.
//
// @decision(DL-054)
func TestAnAnswerThroughSpovReleasesABlockedJob(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putAsk(t, eventPermissionAsked, "per_a")
	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil || res.State != store.JobBlocked {
		t.Fatalf("state = %v, %v; want blocked", res.State, err)
	}
	f.job.State = store.JobBlocked
	f.putAsk(t, eventPermissionReplied, "per_a")
	held, err := f.sup.follow(context.Background(), f.job, res.Cursor)
	if err != nil || held.State != store.JobBlocked {
		t.Fatalf("a replied frame alone: state = %v, %v; want blocked", held.State, err)
	}
	f.putAnswered(t, "per_a")
	got, err := f.sup.follow(context.Background(), f.job, held.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobRunning || !got.Moved {
		t.Errorf("state = %v (moved %v), want running", got.State, got.Moved)
	}

	// The marker read before the sandbox's replied frame is stored.
	g := newFollowFixture(t, store.JobBlocked)
	g.putAsk(t, eventPermissionAsked, "per_b")
	g.putAnswered(t, "per_b")
	if res, err := g.sup.follow(context.Background(), g.job, 0); err != nil || res.State != store.JobRunning {
		t.Errorf("answered before its replied frame: state = %v, %v; want running", res.State, err)
	}
}

// Another request still unanswered keeps the job blocked, even one
// asked in an earlier pass; answering that one too releases it.
//
// @decision(DL-054)
func TestAnotherUnansweredRequestKeepsTheJobBlocked(t *testing.T) {
	f := newFollowFixture(t, store.JobBlocked)
	f.putAsk(t, eventPermissionAsked, "per_a")
	f.putAsk(t, eventPermissionAsked, "per_b")
	first, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.putAnswered(t, "per_a")
	res, err := f.sup.follow(context.Background(), f.job, first.Cursor)
	if err != nil || res.State != store.JobBlocked || res.Moved {
		t.Fatalf("state = %v (moved %v), %v; want blocked", res.State, res.Moved, err)
	}
	f.putAnswered(t, "per_b")
	res, err = f.sup.follow(context.Background(), f.job, res.Cursor)
	if err != nil || res.State != store.JobRunning {
		t.Errorf("both answered: state = %v, %v; want running", res.State, err)
	}
}

// The marker moves only a blocked job: a running or steered one is not
// parked, and a release would undo a steer.
//
// @decision(DL-054)
func TestAnAnswerMovesOnlyABlockedJob(t *testing.T) {
	for _, state := range []store.JobState{store.JobRunning, store.JobSteered} {
		f := newFollowFixture(t, state)
		f.putAnswered(t, "per_a")
		res, err := f.sup.follow(context.Background(), f.job, 0)
		if err != nil || res.State != state || res.Moved {
			t.Errorf("%s: state = %v (moved %v), %v", state, res.State, res.Moved, err)
		}
	}
}

// Writing the release clears the suspension's note, as Resume does.
//
// @decision(DL-054)
func TestAReleaseClearsTheSuspensionsNote(t *testing.T) {
	f := newFollowFixture(t, store.JobBlocked)
	ctx := context.Background()
	job, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.Error = "waiting on a permission"
	if err := f.st.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	f.putAsk(t, eventPermissionAsked, "per_a")
	f.putAnswered(t, "per_a")
	res, err := f.sup.follow(ctx, job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sup.applyFollow(ctx, job, res); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Jobs().Get(ctx, job.OrgID, job.ID)
	if got.State != store.JobRunning || got.Error != "" {
		t.Errorf("job = %s %q, want running with no note", got.State, got.Error)
	}
}

// A request whose turn ended without a reply is gone (an interrupt drops
// it, spike S-F1), so it does not keep a later answer from releasing the
// job.
//
// @decision(DL-054) @decision(DL-020)
func TestARequestWhoseTurnEndedIsNotUnanswered(t *testing.T) {
	f := newFollowFixture(t, store.JobBlocked)
	f.putAsk(t, eventPermissionAsked, "per_old")
	f.put(t, "session.execution.interrupted", nil)
	f.putAsk(t, eventPermissionAsked, "per_new")
	f.putAnswered(t, "per_new")
	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil || res.State != store.JobRunning {
		t.Errorf("state = %v, %v; want running", res.State, err)
	}
}

// An answer folded while a steer is held clears the park the steer
// swallowed, so a Resume's retraction does not block the job again on a
// request already answered.
//
// @decision(DL-054)
func TestAnAnsweredParkIsNotReappliedOnResume(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putSpovMarker(t, MarkerInterrupted, "interrupted by u")
	f.putAsk(t, eventPermissionAsked, "per_a")
	f.putAnswered(t, "per_a")
	f.putResumed(t)
	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil || res.State != store.JobRunning {
		t.Errorf("state = %v, %v; want running", res.State, err)
	}
}

// An answer releasing a blocked job releases the plugin's loop too, as
// Resume does, since someone typing into the parked job held it.
//
// @decision(DL-050) @decision(DL-054)
func TestAnAnswerThatReleasesAJobReleasesItsLoop(t *testing.T) {
	f := newFollowFixture(t, store.JobBlocked)
	var told []bool
	f.sup.Loop = func(_ context.Context, _ auth.Principal, _ store.Job, paused bool) error {
		told = append(told, paused)
		return nil
	}
	f.putAsk(t, eventPermissionAsked, "per_a")
	f.putAnswered(t, "per_a")
	if err := f.sup.followOnce(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobRunning {
		t.Fatalf("state = %v, want running", got.State)
	}
	if len(told) != 1 || told[0] {
		t.Errorf("the loop was told %v, want one release", told)
	}

	// A pass that leaves the job blocked releases nothing.
	g := newFollowFixture(t, store.JobBlocked)
	g.sup.Loop = func(context.Context, auth.Principal, store.Job, bool) error {
		t.Error("the loop was told something while the job stayed blocked")
		return nil
	}
	g.putAsk(t, eventPermissionAsked, "per_b")
	if err := g.sup.followOnce(context.Background(), g.job); err != nil {
		t.Fatal(err)
	}
}
