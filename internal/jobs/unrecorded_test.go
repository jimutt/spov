package jobs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// putSpov appends one of spov's own markers.
func (f *followFixture) putSpov(t *testing.T, name string, raw []byte) {
	t.Helper()
	if _, err := f.st.Conversations().PutEvents(context.Background(), []store.Event{{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		Source: store.SourceSpov, Name: name, At: time.Now(), Raw: raw,
	}}); err != nil {
		t.Fatal(err)
	}
}

func steerRaw(t *testing.T, after string) []byte {
	t.Helper()
	raw, err := json.Marshal(SteerRecord{Message: "msg_1", Delivery: "steer", After: after})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAPersonsHandoverReachesAJobWhoseRecordingStopped is DL-131 and
// DL-052: once a session's recording has stopped, the person's message
// never reaches the log as an inbox frame, so spov's own record of the
// send is read in its place: a handover takes the job over, in this
// pass or a later one, and a message that said continue does not.
func TestAPersonsHandoverReachesAJobWhoseRecordingStopped(t *testing.T) {
	ctx := context.Background()
	state := func(t *testing.T, f *followFixture) store.JobState {
		t.Helper()
		if err := f.sup.followOnce(ctx, f.job); err != nil {
			t.Fatal(err)
		}
		got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.job = got
		return got.State
	}
	t.Run("a handover after the stop", func(t *testing.T) {
		f := newFollowFixture(t, store.JobRunning)
		f.putSpov(t, MarkerRecordingStopped, nil)
		f.putSpov(t, MarkerSteered, steerRaw(t, AfterHandover))
		if got := state(t, f); got != store.JobSteered {
			t.Errorf("state = %v, want steered", got)
		}
	})
	t.Run("a handover in a later pass", func(t *testing.T) {
		f := newFollowFixture(t, store.JobRunning)
		f.putSpov(t, MarkerRecordingStopped, nil)
		if got := state(t, f); got != store.JobRunning {
			t.Fatalf("state = %v before the send", got)
		}
		f.putSpov(t, MarkerSteered, steerRaw(t, AfterHandover))
		if got := state(t, f); got != store.JobSteered {
			t.Errorf("state = %v, want steered", got)
		}
	})
	t.Run("a continue after the stop", func(t *testing.T) {
		f := newFollowFixture(t, store.JobRunning)
		f.putSpov(t, MarkerRecordingStopped, nil)
		f.putSpov(t, MarkerSteered, steerRaw(t, AfterContinue))
		if got := state(t, f); got != store.JobRunning {
			t.Errorf("state = %v, want running", got)
		}
	})
	t.Run("a handover while the recording runs waits for its delivery", func(t *testing.T) {
		f := newFollowFixture(t, store.JobRunning)
		f.putSpov(t, MarkerSteered, steerRaw(t, AfterHandover))
		if got := state(t, f); got != store.JobRunning {
			t.Errorf("state = %v, want running", got)
		}
	})
}

// TestAPersonWroteOnceTheySentAfterTheRecordingStopped is DL-131 and
// DL-050: a proposed check stops deciding on spov's record of a send the
// log could not show.
func TestAPersonWroteOnceTheySentAfterTheRecordingStopped(t *testing.T) {
	steer := store.Event{Source: store.SourceSpov, Name: MarkerSteered, Raw: steerRaw(t, AfterContinue)}
	stop := store.Event{Source: store.SourceSpov, Name: MarkerRecordingStopped}
	if !personWrote([]store.Event{stop, steer}) {
		t.Error("a send after the stop is not a person writing")
	}
	if personWrote([]store.Event{steer, stop}) {
		t.Error("a send before the stop was read from its record, not its inbox frame")
	}
	forged := store.Event{Source: store.SourceOpenCode, Name: MarkerSteered, Raw: steer.Raw}
	if personWrote([]store.Event{stop, forged}) {
		t.Error("a sandbox's frame was read as spov's record")
	}
}
