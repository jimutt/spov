package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
)

// followFixture is a job with a session and a conversation to hang
// events on.
type followFixture struct {
	sup  *Supervisor
	st   store.Store
	job  store.Job
	conv string
	pos  int64
}

func newFollowFixture(t *testing.T, state store.JobState) *followFixture {
	t.Helper()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, state)
	conv := store.NewID(time.Now())
	if _, err := st.Conversations().UpsertConversation(context.Background(), store.Conversation{
		ID: conv, OrgID: job.OrgID, SessionID: job.SessionID, Title: "c",
	}); err != nil {
		t.Fatal(err)
	}
	return &followFixture{
		sup:  &Supervisor{Store: st, Warn: func(string) {}},
		st:   st,
		job:  job,
		conv: conv,
	}
}

// put appends one sandbox event.
func (f *followFixture) put(t *testing.T, name string, raw []byte) {
	t.Helper()
	f.pos++
	ev := store.Event{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		ConversationID: f.conv, Source: store.SourceOpenCode, Seq: f.pos,
		Name: name, At: time.Now(), Raw: raw,
	}
	if _, err := f.st.Conversations().PutEvents(context.Background(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
}

// putEphemeral appends a permission frame, which has no seq.
func (f *followFixture) putEphemeral(t *testing.T, name string) {
	t.Helper()
	ev := store.Event{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		ConversationID: f.conv, Source: store.SourceEphemeral,
		Name: name, At: time.Now(),
	}
	if _, err := f.st.Conversations().PutEvents(context.Background(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
}

// putSpovMarker appends one of spov's own events. They differ only in
// name and text, and seeding them three ways is three places to get the
// source or the missing conversation id wrong.
func (f *followFixture) putSpovMarker(t *testing.T, name, text string) {
	t.Helper()
	ev := store.Event{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		Source: store.SourceSpov, Name: name,
		Text: text, At: time.Now(),
	}
	if _, err := f.st.Conversations().PutEvents(context.Background(), []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
}

// putGap appends the recorder's gap marker.
func (f *followFixture) putGap(t *testing.T) {
	t.Helper()
	f.putSpovMarker(t, MarkerGap, "recording resumed")
}

// inboxItemRaw is an enqueue of one item, with its id and, when after is
// not nil, the spov metadata saying what follows it.
func inboxItemRaw(t *testing.T, itemType, id string, after *string) []byte {
	t.Helper()
	item := map[string]any{"type": itemType}
	if after != nil {
		item["payload"] = map[string]any{"text": "hi", "metadata": map[string]any{"spov": map[string]any{"after": *after}}}
	}
	raw, err := json.Marshal(map[string]any{
		"type": EventInboxEnqueued,
		"data": map[string]any{"inboxID": id, "item": item},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func deliveredRaw(t *testing.T, id string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": EventInboxDelivered,
		"data": map[string]any{"inboxID": id, "sessionID": "ses_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// putHuman is a person's message that takes the job over, enqueued and
// then delivered, which is when it takes effect (DL-052).
func (f *followFixture) putHuman(t *testing.T) {
	t.Helper()
	id := "msg_" + store.NewID(time.Now())
	f.put(t, EventInboxEnqueued, inboxItemRaw(t, "user", id, nil))
	f.put(t, EventInboxDelivered, deliveredRaw(t, id))
}

func TestFollowParksOnAPermission(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.put(t, "session.execution.succeeded", nil)
	f.putEphemeral(t, "permission.asked")

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked", res.State)
	}
	if !res.Moved {
		t.Error("the pass did not report a move")
	}
	if res.TurnsEnded != 1 {
		t.Errorf("turns = %d, want 1", res.TurnsEnded)
	}
}

// A person's message steers when it is delivered, unless it says the job
// continues; the loop's own synthetic never does, and an enqueue alone
// moves nothing.
//
// @decision(DL-052) @decision(DL-045)
func TestFollowSteersOnTheDeliveryOfATakeoverItem(t *testing.T) {
	cont, handover := AfterContinue, AfterHandover
	for _, tc := range []struct {
		name      string
		itemType  string
		after     *string
		delivered bool
		want      store.JobState
	}{
		{"a person's message with nothing said", "user", nil, true, store.JobSteered},
		{"a handover", "user", &handover, true, store.JobSteered},
		{"a message that continues", "user", &cont, true, store.JobRunning},
		{"a handover not yet delivered", "user", &handover, false, store.JobRunning},
		{"the loop's own synthetic", "synthetic", nil, true, store.JobRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFollowFixture(t, store.JobRunning)
			id := "msg_" + store.NewID(time.Now())
			f.put(t, EventInboxEnqueued, inboxItemRaw(t, tc.itemType, id, tc.after))
			if tc.delivered {
				f.put(t, EventInboxDelivered, deliveredRaw(t, id))
			}
			res, err := f.sup.follow(context.Background(), f.job, 0)
			if err != nil {
				t.Fatal(err)
			}
			if res.State != tc.want {
				t.Errorf("state = %v, want %v", res.State, tc.want)
			}
		})
	}
}

// A delivery read in a later pass than its enqueue still finds it, in the
// store.
//
// @decision(DL-052)
func TestATakeoverDeliveredInALaterPassIsFoundInTheStore(t *testing.T) {
	handover := AfterHandover
	f := newFollowFixture(t, store.JobRunning)
	id := "msg_" + store.NewID(time.Now())
	f.put(t, EventInboxEnqueued, inboxItemRaw(t, "user", id, &handover))
	first, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != store.JobRunning {
		t.Fatalf("the enqueue alone left the job %v", first.State)
	}
	f.put(t, EventInboxDelivered, deliveredRaw(t, id))
	res, err := f.sup.follow(context.Background(), f.job, first.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobSteered {
		t.Errorf("state = %v, want steered", res.State)
	}
}

// DL-046: a gap is "unknown", never "nothing happened". It must not
// move the job by itself, and it must be reported.
func TestAGapIsUnknownAndMovesNothing(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putGap(t)

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SawGap {
		t.Error("the gap was not reported")
	}
	if res.Moved || res.State != store.JobRunning {
		t.Errorf("a gap moved the job to %v", res.State)
	}
}

// What the events after a gap say is still true: the gap does not
// poison the rest of the pass.
func TestEventsAfterAGapAreStillRead(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putGap(t)
	f.putEphemeral(t, "permission.asked")

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SawGap {
		t.Error("the gap was not reported")
	}
	if res.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked; the park after the gap was dropped", res.State)
	}
}

// The cursor must advance over events that signalled nothing, or a busy
// session re-delivers the same rows forever.
func TestTheCursorAdvancesOverUninterestingEvents(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	for i := 0; i < 5; i++ {
		f.put(t, "session.compaction.started", nil)
	}

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cursor == 0 {
		t.Error("the cursor did not advance over events that signalled nothing")
	}
	if res.Moved {
		t.Error("uninteresting events moved the job")
	}

	// A second pass from the returned cursor sees nothing new.
	again, err := f.sup.follow(context.Background(), f.job, res.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if again.Cursor != res.Cursor {
		t.Errorf("a second pass moved the cursor from %d to %d with no new events",
			res.Cursor, again.Cursor)
	}
}

// A finished job is not re-examined: its outcome is decided.
func TestFollowLeavesATerminalJobAlone(t *testing.T) {
	for _, state := range []store.JobState{
		store.JobSucceeded, store.JobFailed, store.JobCancelled,
	} {
		f := newFollowFixture(t, state)
		f.putEphemeral(t, "permission.asked")

		res, err := f.sup.follow(context.Background(), f.job, 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.Moved || res.State != state {
			t.Errorf("a %v job was moved to %v", state, res.State)
		}
	}
}

// More events than one batch must all be read.
func TestFollowReadsBeyondOneBatch(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	for i := 0; i < FollowBatch+10; i++ {
		f.put(t, "session.execution.succeeded", nil)
	}

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsEnded != FollowBatch+10 {
		t.Errorf("turns = %d, want %d; the pass stopped at a batch boundary",
			res.TurnsEnded, FollowBatch+10)
	}
}

func TestApplyFollowWritesTheState(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	res := followResult{State: store.JobBlocked, Moved: true}
	if _, err := f.sup.applyFollow(context.Background(), f.job, res); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked", got.State)
	}
}

// A cancellation that lands while a pass is in flight wins: the pass's
// conclusion is stale, and resurrecting a cancelled job is worse than
// missing a park.
func TestApplyFollowDoesNotResurrectADecidedJob(t *testing.T) {
	for _, decided := range []store.JobState{
		store.JobCancelled, store.JobFailed, store.JobSucceeded,
	} {
		f := newFollowFixture(t, decided)
		res := followResult{State: store.JobBlocked, Moved: true}
		if _, err := f.sup.applyFollow(context.Background(), f.job, res); err != nil {
			t.Fatal(err)
		}
		got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != decided {
			t.Errorf("a %v job was resurrected as %v", decided, got.State)
		}
	}
}

// The row moving under the pass must not be overwritten with a
// conclusion drawn from a state that no longer holds.
func TestApplyFollowRefusesAnIllegalTransition(t *testing.T) {
	f := newFollowFixture(t, store.JobSteered)
	// blocked is not reachable from steered.
	res := followResult{State: store.JobBlocked, Moved: true}
	if _, err := f.sup.applyFollow(context.Background(), f.job, res); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobSteered {
		t.Errorf("state = %v; an illegal transition was forced", got.State)
	}
}

func TestApplyFollowWritesNothingWhenNothingMoved(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	res := followResult{State: store.JobRunning}
	if _, err := f.sup.applyFollow(context.Background(), f.job, res); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobRunning {
		t.Errorf("state = %v, want running", got.State)
	}
}

// Run must actually reach the follow pass and write what it concluded.
// The pieces above are each tested; this is the wiring between them.
func TestRunFollowsAndWritesTheState(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.put(t, "session.execution.succeeded", nil)
	f.putEphemeral(t, "permission.asked")

	err := f.sup.Run(context.Background(),
		JobArgs{JobID: f.job.ID, OrgID: f.job.OrgID}, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked; Run did not apply what the pass concluded", got.State)
	}
}

// A gap is reported rather than swallowed: a job whose state was decided
// across one is worth knowing about later.
func TestRunReportsAGap(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	var warnings []string
	f.sup.Warn = func(s string) { warnings = append(warnings, s) }
	f.putGap(t)

	if err := f.sup.Run(context.Background(),
		JobArgs{JobID: f.job.ID, OrgID: f.job.OrgID}, 1); err != nil {
		t.Fatal(err)
	}
	var said bool
	for _, w := range warnings {
		if strings.Contains(w, "gap") {
			said = true
		}
	}
	if !said {
		t.Errorf("the gap was not reported: %v", warnings)
	}
}

// The bug this exists to prevent: a human releases a parked or steered
// job, the next pass replays the same old events, and the release is
// undone. Both re-derivations are LEGAL transitions, so no validation
// catches them — only a durable cursor does.
func TestAHumanReleaseSurvivesTheNextPass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		put    func(f *followFixture)
		parked store.JobState
	}{
		{
			name:   "steered by a human's prompt",
			put:    func(f *followFixture) { f.putHuman(t) },
			parked: store.JobSteered,
		},
		{
			name:   "blocked on a permission",
			put:    func(f *followFixture) { f.putEphemeral(t, "permission.asked") },
			parked: store.JobBlocked,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFollowFixture(t, store.JobRunning)
			tc.put(f)
			args := JobArgs{JobID: f.job.ID, OrgID: f.job.OrgID}

			if err := f.sup.Run(ctx, args, 1); err != nil {
				t.Fatal(err)
			}
			got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.parked {
				t.Fatalf("pass 1 left the job %v, want %v", got.State, tc.parked)
			}
			if got.Cursor == 0 {
				t.Fatal("pass 1 did not persist a cursor, so pass 2 will replay")
			}

			// A human releases it.
			got.State = store.JobRunning
			if err := f.st.Jobs().Update(ctx, got); err != nil {
				t.Fatal(err)
			}

			// A River retry delivers the job again, with no new events.
			if err := f.sup.Run(ctx, args, 2); err != nil {
				t.Fatal(err)
			}
			after, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != store.JobRunning {
				t.Errorf("the release was undone: the job is %v again", after.State)
			}
		})
	}
}

// The cursor advances even when no state moved, or a busy session's
// uninteresting events are re-read on every pass forever.
func TestThePassPersistsItsCursorWithoutMovingTheState(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, store.JobRunning)
	for i := 0; i < 3; i++ {
		f.put(t, "session.compaction.started", nil)
	}
	args := JobArgs{JobID: f.job.ID, OrgID: f.job.OrgID}
	if err := f.sup.Run(ctx, args, 1); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor == 0 {
		t.Error("the cursor did not advance over events that moved no state")
	}
	if got.State != store.JobRunning {
		t.Errorf("state = %v, want running", got.State)
	}
}

// A job resumed after a park must be able to park again on a NEW
// permission — the cursor must not make the supervisor blind to what
// happens next.
func TestAResumedJobCanParkAgainOnANewEvent(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, store.JobRunning)
	f.putEphemeral(t, "permission.asked")
	args := JobArgs{JobID: f.job.ID, OrgID: f.job.OrgID}

	if err := f.sup.Run(ctx, args, 1); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
	got.State = store.JobRunning
	if err := f.st.Jobs().Update(ctx, got); err != nil {
		t.Fatal(err)
	}

	// A genuinely new permission arrives.
	f.putEphemeral(t, "permission.asked")
	if err := f.sup.Run(ctx, args, 2); err != nil {
		t.Fatal(err)
	}
	after, _ := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
	if after.State != store.JobBlocked {
		t.Errorf("state = %v; a new permission after a resume was not seen", after.State)
	}
}

// A state concluded across a gap is still written — the job was parked,
// and a silently-running parked job is worse than a stale one — but the
// row must say the range was incomplete rather than presenting the
// conclusion as a clean observation.
func TestAStateConcludedAcrossAGapSaysSo(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, store.JobRunning)
	f.putEphemeral(t, "permission.asked")
	f.putGap(t)

	if err := f.sup.Run(ctx, JobArgs{JobID: f.job.ID, OrgID: f.job.OrgID}, 1); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked; a parked job must not read as running", got.State)
	}
	if !strings.Contains(got.Error, "gap") {
		t.Errorf("the row does not record that the range was incomplete: %q", got.Error)
	}
}

// A clean pass must not carry the gap note.
func TestACleanConclusionCarriesNoGapNote(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, store.JobRunning)
	f.putEphemeral(t, "permission.asked")

	if err := f.sup.Run(ctx, JobArgs{JobID: f.job.ID, OrgID: f.job.OrgID}, 1); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error != "" {
		t.Errorf("a clean pass left %q on the row", got.Error)
	}
}

// putResumed appends spov's own resume boundary.
//
// @decision(DL-054)
func (f *followFixture) putResumed(t *testing.T) {
	t.Helper()
	f.putSpovMarker(t, MarkerResumed, "resumed by someone")
}

// TestAStaleHumanItemDoesNotUndoAResume checks that a human item that
// was still unread when a resume moved the row to running does not get
// re-read and write `steered` straight back over the resume.
//
// @decision(DL-054)
func TestAStaleHumanItemDoesNotUndoAResume(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	// The order the store holds them in: the human typed first, the
	// resume came after.
	f.putHuman(t)
	f.putResumed(t)

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobRunning {
		t.Errorf("state = %v, want running; the resume was undone by the message that preceded it", res.State)
	}
	if res.Moved {
		t.Error("the pass moved the job; a resume boundary and the item it answers should leave it alone")
	}
}

// TestAHumanItemAfterAResumeStillSteers checks the boundary is
// retroactive but not prospective: a message typed after the resume is
// a new takeover.
//
// @decision(DL-054)
func TestAHumanItemAfterAResumeStillSteers(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putResumed(t)
	f.putHuman(t)

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobSteered {
		t.Errorf("state = %v, want steered; a message typed after the resume is a new takeover", res.State)
	}
}

// TestTheResumeBoundaryReachesAcrossABatch checks the boundary survives
// the stale item and the resume that answers it falling on either side
// of a FollowBatch edge.
//
// @decision(DL-054)
func TestTheResumeBoundaryReachesAcrossABatch(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putHuman(t)
	// Fill the rest of the batch and one more, so the marker lands in
	// a later read than the item it answers.
	for i := 0; i < FollowBatch; i++ {
		f.put(t, "session.compaction.started", nil)
	}
	f.putResumed(t)

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobRunning {
		t.Errorf("state = %v, want running; the boundary did not survive the batch edge", res.State)
	}
}

// TestASteerIsInEffectForTheRestOfThePass checks a steer held for a
// possible resume is still the state the rest of the pass folds
// against, so a stop marker read after it is correctly dropped.
//
// @decision(DL-054) @decision(DL-045)
func TestASteerIsInEffectForTheRestOfThePass(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putHuman(t)
	f.put(t, EventInboxEnqueued, inboxTextRaw(t, "synthetic", stopMarkerText()))

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobSteered {
		t.Errorf("state = %v, want steered; the stop after the steer was acted on", res.State)
	}
}

// TestAStopAfterAResumeIsRead checks the same shape once a resume has
// retracted the steer: the state really is running again.
//
// @decision(DL-054)
func TestAStopAfterAResumeIsRead(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putHuman(t)
	f.putResumed(t)
	f.put(t, EventInboxEnqueued, inboxTextRaw(t, "synthetic", stopMarkerText()))

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobDelivering {
		t.Errorf("state = %v, want delivering", res.State)
	}
	if !res.Stopped {
		t.Error("the stop was not reported")
	}
}

// TestWritingSteeredPausesTheLoop checks the row reaching steered pauses
// the plugin's loop, which has no notion of steering on its own.
//
// @decision(DL-054)
func TestWritingSteeredPausesTheLoop(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	var paused []bool
	f.sup.Loop = func(_ context.Context, _ auth.Principal, _ store.Job, p bool) error {
		paused = append(paused, p)
		return nil
	}
	f.putHuman(t)

	if err := f.sup.followOnce(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	if len(paused) != 1 || !paused[0] {
		t.Fatalf("the loop was told %v; steering must pause it exactly once", paused)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobSteered {
		t.Errorf("state = %v, want steered", got.State)
	}
}

// TestAPassThatDidNotWriteSteeredPausesNothing checks the pause fires on
// the row reaching steered, never on the pass having concluded it: a
// conclusion the store refuses must not pause the loop.
//
// @decision(DL-054)
func TestAPassThatDidNotWriteSteeredPausesNothing(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	var calls int
	f.sup.Loop = func(context.Context, auth.Principal, store.Job, bool) error {
		calls++
		return nil
	}
	// Decided while the pass runs: the row is cancelled before the
	// conclusion is applied.
	f.putHuman(t)
	job, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.State = store.JobCancelled
	if err := f.st.Jobs().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	if err := f.sup.followOnce(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("the loop was written to %d times for a job the store never moved to steered", calls)
	}
}

// TestAPauseThatCannotBeDeliveredStillSteers checks a pause failure is
// only a warning: the row still moves, since refusing the steering the
// events already established would be the larger error.
//
// @decision(DL-054) @decision(DL-017)
func TestAPauseThatCannotBeDeliveredStillSteers(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	var warned []string
	f.sup.Warn = func(s string) { warned = append(warned, s) }
	f.sup.Loop = func(context.Context, auth.Principal, store.Job, bool) error {
		return errors.New("the sandbox is gone")
	}
	f.putHuman(t)

	if err := f.sup.followOnce(context.Background(), f.job); err != nil {
		t.Fatalf("a failed pause failed the pass: %v", err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobSteered {
		t.Errorf("state = %v, want steered; a failed pause withheld the steering itself", got.State)
	}
	if len(warned) == 0 {
		t.Error("a pause that could not be delivered said nothing")
	}
}

// TestANilLoopSeamStillSteers checks a nil Loop seam is a working
// configuration.
//
// @decision(DL-054)
func TestANilLoopSeamStillSteers(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putHuman(t)
	if err := f.sup.followOnce(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobSteered {
		t.Errorf("state = %v, want steered", got.State)
	}
}

// TestTheResumeBoundaryAcrossOrderings checks orderings the boundary
// must get right; the last case matters most: a park read before a
// retracted steer restores `blocked` rather than assuming `running`.
//
// @decision(DL-054) @decision(DL-045)
func TestTheResumeBoundaryAcrossOrderings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, f *followFixture)
		want  store.JobState
	}{
		{"two steers then a resume", func(t *testing.T, f *followFixture) {
			f.putHuman(t)
			f.putHuman(t)
			f.putResumed(t)
		}, store.JobRunning},
		{"a resume with no steer", func(t *testing.T, f *followFixture) {
			f.putResumed(t)
		}, store.JobRunning},
		{"steer, resume, steer", func(t *testing.T, f *followFixture) {
			f.putHuman(t)
			f.putResumed(t)
			f.putHuman(t)
		}, store.JobSteered},
		{"two resumes after one steer", func(t *testing.T, f *followFixture) {
			f.putHuman(t)
			f.putResumed(t)
			f.putResumed(t)
		}, store.JobRunning},
		{"a gap between the steer and the resume", func(t *testing.T, f *followFixture) {
			f.putHuman(t)
			f.putGap(t)
			f.putResumed(t)
		}, store.JobRunning},
		{"a park before a retracted steer survives it", func(t *testing.T, f *followFixture) {
			f.putEphemeral(t, "permission.asked")
			f.putHuman(t)
			f.putResumed(t)
		}, store.JobBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFollowFixture(t, store.JobRunning)
			tc.build(t, f)
			res, err := f.sup.follow(context.Background(), f.job, 0)
			if err != nil {
				t.Fatal(err)
			}
			if res.State != tc.want {
				t.Errorf("state = %v, want %v", res.State, tc.want)
			}
		})
	}
}

// TestADeclaredStopInsideTheSteerWindowSurvivesTheResume checks a stop
// declared between a steer and the resume that retracts it survives:
// the retraction restores the interrupted state without discarding it.
//
// @decision(DL-054) @decision(DL-048)
func TestADeclaredStopInsideTheSteerWindowSurvivesTheResume(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putHuman(t)
	f.put(t, EventInboxEnqueued, inboxTextRaw(t, "synthetic",
		opencode.LoopStoppedMarker+` {"reason":"`+opencode.CompletionReason+`"}`))
	f.putResumed(t)

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobDelivering {
		t.Errorf("state = %v, want delivering; the resume discarded the declaration it followed", res.State)
	}
	if !res.Moved {
		t.Error("the pass reported no move, so applyFollow would write only the cursor")
	}
}

// TestAParkInsideTheSteerWindowSurvivesTheResume checks a permission
// park inside the steer window survives the retraction too: DL-045's
// machine has no steered -> blocked edge, so the park moves nothing when
// read and must not be lost behind the cursor.
//
// @decision(DL-054) @decision(DL-046)
func TestAParkInsideTheSteerWindowSurvivesTheResume(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putHuman(t)
	f.putEphemeral(t, "permission.asked")
	f.putResumed(t)

	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked; the park was lost with the steer that swallowed it", res.State)
	}
}
