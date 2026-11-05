package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// putInterrupted appends spov's own record of having interrupted the
// job's turn.
//
// @decision(DL-053)
func (f *followFixture) putInterrupted(t *testing.T) {
	t.Helper()
	f.putSpovMarker(t, MarkerInterrupted, "stopped by someone")
}

// TestTheInterruptMarkerIsReadAsASteer asserts the premise every other
// test in this file rests on directly, since the state assertions below
// pass equally against a signalFor that has never heard of this marker.
//
// @decision(DL-053)
func TestTheInterruptMarkerIsReadAsASteer(t *testing.T) {
	e := store.Event{Source: store.SourceSpov, Name: MarkerInterrupted, Text: "stopped by someone"}
	if got := signalFor(e); got != signalSteered {
		t.Errorf("signalFor(%s) = %v, want signalSteered", MarkerInterrupted, got)
	}
}

// TestAnInterruptMarkerSteersARunningJobAndPausesItsLoop asserts both
// halves of the marker's effect together: the row moves to steered and
// the loop is paused, since either alone is a half-state DL-053 refuses.
//
// @decision(DL-053) @decision(DL-054)
func TestAnInterruptMarkerSteersARunningJobAndPausesItsLoop(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	var paused []bool
	f.sup.Loop = func(_ context.Context, _ auth.Principal, _ store.Job, p bool) error {
		paused = append(paused, p)
		return nil
	}
	f.putInterrupted(t)

	if err := f.sup.followOnce(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobSteered {
		t.Errorf("state = %v, want steered; the marker did not reach the machine", got.State)
	}
	if len(paused) != 1 || !paused[0] {
		t.Fatalf("the loop was told %v; an interrupt must pause it exactly once", paused)
	}
}

// TestAnInterruptOnABlockedJobMovesNothing checks that a marker reaching
// a job that parked between the endpoint's refusal and the write leaves
// it blocked: DL-045 has no blocked -> steered edge.
//
// @decision(DL-053) @decision(DL-045)
func TestAnInterruptOnABlockedJobMovesNothing(t *testing.T) {
	f := newFollowFixture(t, store.JobBlocked)
	var calls int
	f.sup.Loop = func(context.Context, auth.Principal, store.Job, bool) error {
		calls++
		return nil
	}
	f.putInterrupted(t)

	if err := f.sup.followOnce(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked; an interrupt took a job off its unanswered question", got.State)
	}
	if calls != 0 {
		t.Errorf("the loop was written to %d times for a job that never reached steered", calls)
	}
}

// TestAnInterruptOnASteeredJobMovesNothingAndPausesNothing checks the
// ordinary case, Stop on an already-steered job: the row does not move
// and the pause does not fire again, since DL-054's guard keys on the
// row reaching steered rather than on it being there.
//
// @decision(DL-053) @decision(DL-054)
func TestAnInterruptOnASteeredJobMovesNothingAndPausesNothing(t *testing.T) {
	f := newFollowFixture(t, store.JobSteered)
	var calls int
	f.sup.Loop = func(context.Context, auth.Principal, store.Job, bool) error {
		calls++
		return nil
	}
	f.putInterrupted(t)

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
	if calls != 0 {
		t.Errorf("the loop was written to %d times for a row that was already steered", calls)
	}
}

// TestASandboxCannotForgeAnInterrupt checks both halves of the defence:
// the store refuses a spov.-prefixed name on any row the sandbox
// sourced, and signalFor reads the marker only from SourceSpov. The
// ingest half is checked for both sandbox sources, since Validate
// carries a separate prefix check on each branch.
//
// @decision(DL-053) @decision(DL-058)
func TestASandboxCannotForgeAnInterrupt(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	for _, src := range []store.EventSource{store.SourceOpenCode, store.SourceEphemeral} {
		ev := store.Event{
			ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
			ConversationID: f.conv, Source: src,
			Name: MarkerInterrupted, At: time.Now(),
		}
		// Only a durable event carries a position; an ephemeral one is
		// refused for a seq it does not have.
		if src == store.SourceOpenCode {
			ev.Seq = 1
		}
		if _, err := f.st.Conversations().PutEvents(context.Background(), []store.Event{ev}); !errors.Is(err, store.ErrInvalidEvent) {
			t.Errorf("the store took a %s-sourced %s: %v", src, MarkerInterrupted, err)
		}
		if got := signalFor(store.Event{Source: src, Name: MarkerInterrupted}); got != signalNone {
			t.Errorf("a %s-sourced %s gave signal %v, want none", src, MarkerInterrupted, got)
		}
	}
}

// TestAResumeRetractsAnInterrupt checks the sequence DL-053 admits
// deliberately: Stop from steered, then Resume, where the resume must
// not be undone by the interrupt marker still behind the cursor. Two
// passes are asserted because the first is where the steer is visible;
// asserting only the end state would pass against an unwired signalFor.
//
// @decision(DL-053) @decision(DL-054)
func TestAResumeRetractsAnInterrupt(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.putInterrupted(t)

	steer, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if steer.State != store.JobSteered || !steer.Moved {
		t.Fatalf("the interrupt alone gave state %v moved=%t, want steered and moved",
			steer.State, steer.Moved)
	}

	f.putResumed(t)
	res, err := f.sup.follow(context.Background(), f.job, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.JobRunning {
		t.Errorf("state = %v, want running; the resume was undone by the interrupt that preceded it", res.State)
	}
	if res.Moved {
		t.Error("the pass moved the job; a resume and the interrupt it answers should leave it alone")
	}
}

// TestAnInterruptThatLandsOnAFinishedRunHoldsIt pins both orderings of
// Stop landing on a run that has already finished: the interleaving is
// pure fold logic that a later tidy-up of the signalStopped case could
// change silently.
//
// @decision(DL-053) @decision(DL-045) @decision(DL-048)
func TestAnInterruptThatLandsOnAFinishedRunHoldsIt(t *testing.T) {
	t.Run("the stop was announced first", func(t *testing.T) {
		f := newFollowFixture(t, store.JobRunning)
		f.put(t, EventInboxEnqueued, inboxTextRaw(t, "synthetic", stopMarkerText()))
		f.putInterrupted(t)

		res, err := f.sup.follow(context.Background(), f.job, 0)
		if err != nil {
			t.Fatal(err)
		}
		// The stop moved it to delivering first, and DL-045 has no
		// delivering -> steered edge, so the interrupt is dropped.
		if res.State != store.JobDelivering {
			t.Errorf("state = %v, want delivering; an interrupt took a finished run off its delivery", res.State)
		}
		if !res.Stopped {
			t.Error("the pass did not record that the loop had stopped")
		}
	})
	t.Run("the interrupt was written first", func(t *testing.T) {
		f := newFollowFixture(t, store.JobRunning)
		f.putInterrupted(t)
		f.put(t, EventInboxEnqueued, inboxTextRaw(t, "synthetic", stopMarkerText()))

		res, err := f.sup.follow(context.Background(), f.job, 0)
		if err != nil {
			t.Fatal(err)
		}
		// nextState refuses steered -> delivering for signalStopped, so
		// the job holds at steered with its stop already read. DL-045's
		// checkAbandoned is what concludes it once the sandbox is gone.
		if res.State != store.JobSteered {
			t.Errorf("state = %v, want steered", res.State)
		}
		if !res.Stopped {
			t.Error("the pass did not record that the loop had stopped")
		}
	})
}
