package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/store"
)

// clockFixture is a followFixture whose job has a one-hour wall clock,
// with the supervisor's clock set relative to when that clock started,
// and EndRun and Mark recorded.
type clockFixture struct {
	*followFixture
	start time.Time
	// endRunSaw is the job's stored state each time EndRun was called.
	endRunSaw []store.JobState
}

func newClockFixture(t *testing.T, state store.JobState, maxDuration time.Duration) *clockFixture {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJobWith(t, st, state, func(j *store.Job) { j.MaxDuration = maxDuration })
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	conv := store.NewID(time.Now())
	if _, err := st.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: conv, OrgID: job.OrgID, SessionID: job.SessionID, Title: "c",
	}); err != nil {
		t.Fatal(err)
	}
	f := &followFixture{sup: &Supervisor{Store: st, Warn: func(string) {}}, st: st, job: got, conv: conv}
	c := &clockFixture{followFixture: f}
	if got.ClockStartedAt != nil {
		c.start = *got.ClockStartedAt
	}
	f.sup.EndRun = func(ctx context.Context, job store.Job) error {
		cur, err := f.st.Jobs().Get(ctx, job.OrgID, job.ID)
		if err != nil {
			return err
		}
		c.endRunSaw = append(c.endRunSaw, cur.State)
		return nil
	}
	f.sup.Mark = func(_ context.Context, _ store.Job, name, text string) error {
		f.putSpovMarker(t, name, text)
		return nil
	}
	return c
}

// at sets the supervisor's clock to d after the job's clock started.
func (c *clockFixture) at(d time.Duration) {
	c.sup.Now = func() time.Time { return c.start.Add(d) }
}

func (c *clockFixture) state(t *testing.T) store.Job {
	t.Helper()
	got, err := c.st.Jobs().Get(context.Background(), c.job.OrgID, c.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (c *clockFixture) bounded(t *testing.T) []store.Event {
	t.Helper()
	events, err := c.st.Conversations().ListEvents(context.Background(), c.job.OrgID, c.job.SessionID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var found []store.Event
	for _, e := range events {
		if e.Name == MarkerBounded {
			found = append(found, e)
		}
	}
	return found
}

// A running job past its clock is moved to delivering and marked in one
// pass, and delivery then stops its run before collecting. EndRun must
// find the row already delivering, or the interrupt it sends would read
// as a human steering the job.
//
// @decision(DL-049)
func TestARunningJobIsDeliveredAtItsWallClock(t *testing.T) {
	c := newClockFixture(t, store.JobRunning, time.Hour)
	c.at(time.Hour + time.Minute)
	ctx := context.Background()

	if err := c.sup.followOnce(ctx, c.job); err != nil {
		t.Fatal(err)
	}
	got := c.state(t)
	if got.State != store.JobDelivering {
		t.Fatalf("state = %v, want delivering", got.State)
	}
	if !strings.Contains(got.Error, "1h0m0s wall clock") {
		t.Errorf("the note does not name the bound: %q", got.Error)
	}
	marks := c.bounded(t)
	if len(marks) != 1 || marks[0].Source != store.SourceSpov || marks[0].Text != got.Error {
		t.Errorf("bound markers = %+v, want one spov marker carrying the note", marks)
	}
	if len(c.endRunSaw) != 0 {
		t.Errorf("EndRun ran in the pass (%v); it belongs to delivery", c.endRunSaw)
	}

	if err := c.sup.deliver(ctx, got); err != nil {
		t.Fatal(err)
	}
	if len(c.endRunSaw) != 1 || c.endRunSaw[0] != store.JobDelivering {
		t.Errorf("EndRun saw %v, want one call made while the row said delivering", c.endRunSaw)
	}
	done := c.state(t)
	if !strings.HasPrefix(done.Verdict, marks[0].Text) {
		t.Errorf("the verdict %q does not open with the bound", done.Verdict)
	}
}

// A redelivery after a crash between the move and delivery still stops
// the run: the marker was written with the move, and delivery reads it.
//
// @decision(DL-049)
func TestARedeliveredBoundedJobStillStopsItsRun(t *testing.T) {
	c := newClockFixture(t, store.JobRunning, time.Hour)
	c.at(2 * time.Hour)
	ctx := context.Background()
	if err := c.sup.followOnce(ctx, c.job); err != nil {
		t.Fatal(err)
	}
	// The "crash": nothing ran after the pass. A new attempt runs the
	// step the row calls for.
	if _, err := c.sup.step(ctx, c.state(t)); err != nil {
		t.Fatal(err)
	}
	if len(c.endRunSaw) != 1 {
		t.Errorf("EndRun ran %d times on redelivery, want 1", len(c.endRunSaw))
	}
}

// An ordinary delivery, of a job that was not bounded, stops nothing.
//
// @decision(DL-049)
func TestAnOrdinaryDeliveryDoesNotEndTheRun(t *testing.T) {
	c := newClockFixture(t, store.JobDelivering, time.Hour)
	if err := c.sup.deliver(context.Background(), c.state(t)); err != nil {
		t.Fatal(err)
	}
	if len(c.endRunSaw) != 0 {
		t.Errorf("EndRun ran for a job that ended on its own: %v", c.endRunSaw)
	}
}

// failingEvents makes every event read outside a transaction fail, so a
// pass cannot follow the job.
type failingEvents struct{ store.Store }

func (f failingEvents) Conversations() store.ConversationStore {
	return failingConversations{f.Store.Conversations()}
}

type failingConversations struct{ store.ConversationStore }

func (failingConversations) ListEvents(context.Context, string, string, int64, int) ([]store.Event, error) {
	return nil, errors.New("the store is unwell")
}

// A pass that keeps failing does not leave the job unbounded.
//
// @decision(DL-049)
func TestTheClockAppliesWhenThePassFails(t *testing.T) {
	c := newClockFixture(t, store.JobRunning, time.Hour)
	c.at(2 * time.Hour)
	c.sup.Store = failingEvents{c.st}
	if err := c.sup.followOnce(context.Background(), c.job); err != nil {
		t.Fatalf("followOnce = %v; a job past its clock is ended even when the pass fails", err)
	}
	if got := c.state(t); got.State != store.JobDelivering {
		t.Fatalf("state = %v, want delivering", got.State)
	}

	// Inside the clock, the pass's error is still reported.
	c2 := newClockFixture(t, store.JobRunning, time.Hour)
	c2.at(time.Minute)
	c2.sup.Store = failingEvents{c2.st}
	if err := c2.sup.followOnce(context.Background(), c2.job); err == nil {
		t.Error("a failed pass inside the clock reported success")
	}
}

// Inside the clock nothing happens.
//
// @decision(DL-049)
func TestAJobInsideItsWallClockIsLeftAlone(t *testing.T) {
	c := newClockFixture(t, store.JobRunning, time.Hour)
	c.at(59 * time.Minute)

	if err := c.sup.followOnce(context.Background(), c.job); err != nil {
		t.Fatal(err)
	}
	if got := c.state(t); got.State != store.JobRunning {
		t.Fatalf("state = %v, want running", got.State)
	}
	if len(c.endRunSaw) != 0 || len(c.bounded(t)) != 0 {
		t.Errorf("a job inside its clock was ended: EndRun %v, markers %d", c.endRunSaw, len(c.bounded(t)))
	}
}

// A blocked job keeps counting and is delivered at its clock.
//
// @decision(DL-049)
func TestABlockedJobIsDeliveredAtItsWallClock(t *testing.T) {
	c := newClockFixture(t, store.JobBlocked, time.Hour)
	c.at(2 * time.Hour)

	if err := c.sup.followOnce(context.Background(), c.job); err != nil {
		t.Fatal(err)
	}
	if got := c.state(t); got.State != store.JobDelivering {
		t.Fatalf("state = %v, want delivering", got.State)
	}
	if len(c.bounded(t)) != 1 {
		t.Error("a blocked job delivered at its clock carries no bound marker")
	}
}

// A steered job has a person present, so the clock does not end it.
//
// @decision(DL-049)
func TestASteeredJobIsNotEndedByTheClock(t *testing.T) {
	c := newClockFixture(t, store.JobSteered, time.Hour)
	c.at(3 * time.Hour)

	if err := c.sup.followOnce(context.Background(), c.job); err != nil {
		t.Fatal(err)
	}
	if got := c.state(t); got.State != store.JobSteered {
		t.Fatalf("state = %v, want steered", got.State)
	}
	if len(c.endRunSaw) != 0 || len(c.bounded(t)) != 0 {
		t.Error("the clock ended a steered job")
	}
}

// A job created before the clock existed has none.
//
// @decision(DL-049)
func TestAJobWithNoWallClockIsNeverBounded(t *testing.T) {
	c := newClockFixture(t, store.JobRunning, 0)
	c.at(100 * time.Hour)

	if err := c.sup.followOnce(context.Background(), c.job); err != nil {
		t.Fatal(err)
	}
	if got := c.state(t); got.State != store.JobRunning {
		t.Fatalf("state = %v, want running", got.State)
	}
}

// A stop the pass has just read ends the run in its own words; the bound
// does not overwrite it.
//
// @decision(DL-049) @decision(DL-045)
func TestAStopThePassReadWinsOverTheClock(t *testing.T) {
	c := newClockFixture(t, store.JobRunning, time.Hour)
	c.at(2 * time.Hour)
	c.put(t, EventInboxEnqueued, inboxTextRaw(t, "synthetic", stopMarkerText()))

	if err := c.sup.followOnce(context.Background(), c.job); err != nil {
		t.Fatal(err)
	}
	got := c.state(t)
	if got.State != store.JobDelivering {
		t.Fatalf("state = %v, want delivering", got.State)
	}
	if len(c.bounded(t)) != 0 || strings.Contains(got.Error, "wall clock") || len(c.endRunSaw) != 0 {
		t.Errorf("the bound was applied over a stop the loop announced: note %q", got.Error)
	}
}

// The clock is read from the row inside the transaction, so a steer that
// banked a stretch after the pass's copy was read is respected.
//
// @decision(DL-049) @decision(DL-054)
func TestTheClockIsReadFromTheCurrentRow(t *testing.T) {
	c := newClockFixture(t, store.JobRunning, time.Hour)
	stale := c.job
	ctx := context.Background()
	// Steer, then resume: the steer banks the stretch and the resume
	// starts a new one.
	for _, s := range []store.JobState{store.JobSteered, store.JobRunning} {
		cur := c.state(t)
		cur.State = s
		if err := c.st.Jobs().Update(ctx, cur); err != nil {
			t.Fatal(err)
		}
	}
	restarted := *c.state(t).ClockStartedAt
	// Past the stale clock, inside the restarted one.
	c.sup.Now = func() time.Time { return restarted.Add(30 * time.Minute) }
	old := restarted.Add(-2 * time.Hour)
	stale.ClockStartedAt = &old

	if err := c.sup.followOnce(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if got := c.state(t); got.State != store.JobRunning {
		t.Fatalf("state = %v: the clock was read from the pass's stale copy", got.State)
	}
}

// The verdict opens with the bound, read from spov's own marker only.
//
// @decision(DL-049)
func TestTheVerdictNamesTheWallClock(t *testing.T) {
	note := "the job reached its 1h0m0s wall clock after 1h0m10s unattended"
	job := store.Job{Kind: store.JobKindReport}
	spov := store.Event{Source: store.SourceSpov, Name: MarkerBounded, Text: note}
	v := judge(job, []store.Event{spov}, nil, aReport())
	if !strings.HasPrefix(v.Summary, note+"; ") {
		t.Errorf("summary = %q, want it to open with the bound", v.Summary)
	}

	forged := store.Event{Source: store.SourceOpenCode, Name: MarkerBounded, Text: note}
	if v := judge(job, []store.Event{forged}, nil, aReport()); strings.Contains(v.Summary, "wall clock") {
		t.Errorf("a sandbox event named the bound: %q", v.Summary)
	}
}

// The clock is one allowance across the job: time steered does not
// count, and a resume continues with what is left rather than a fresh
// hour.
//
// @decision(DL-049)
func TestAResumedJobContinuesItsClock(t *testing.T) {
	c := newClockFixture(t, store.JobRunning, time.Hour)
	mem := c.st.(*store.Memory)
	ctx := context.Background()
	move := func(to store.JobState, at time.Duration) {
		t.Helper()
		mem.Now = func() time.Time { return c.start.Add(at) }
		cur := c.state(t)
		cur.State = to
		if err := c.st.Jobs().Update(ctx, cur); err != nil {
			t.Fatal(err)
		}
	}
	move(store.JobSteered, 40*time.Minute)
	move(store.JobRunning, 100*time.Minute)

	// 40 minutes before the steer and 15 after the resume: inside.
	c.at(115 * time.Minute)
	if err := c.sup.followOnce(ctx, c.state(t)); err != nil {
		t.Fatal(err)
	}
	if got := c.state(t); got.State != store.JobRunning {
		t.Fatalf("after 55m of its hour: state = %v, want running", got.State)
	}
	// 40 and 21: past the hour, though the stretch since the resume is
	// only 21 minutes.
	c.at(121 * time.Minute)
	if err := c.sup.followOnce(ctx, c.state(t)); err != nil {
		t.Fatal(err)
	}
	got := c.state(t)
	if got.State != store.JobDelivering {
		t.Fatalf("after 61m of its hour: state = %v, want delivering", got.State)
	}
	if !strings.Contains(got.Error, "after 1h1m0s") {
		t.Errorf("the note does not name the total time used: %q", got.Error)
	}
}
