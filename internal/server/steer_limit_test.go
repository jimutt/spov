package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
)

// TestASendPastTheStorageLimitIsRecordedWithItsAfterChoice is DL-131:
// the storage limit stops the recorder, not spov's own record of a send,
// which carries what follows the message so a job's supervisor can read
// a handover the log no longer shows.
func TestASendPastTheStorageLimitIsRecordedWithItsAfterChoice(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	ctx := context.Background()
	rec := NewRecorder(nil, f.svc.deps.Store, time.Now, nil)
	rec.stopAtStorageLimit(ctx, f.sess)
	if _, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "I'll take it", ID: "msg_mine", After: AfterHandover}); err != nil {
		t.Fatal(err)
	}
	evs, err := f.svc.deps.Store.Conversations().ListEvents(ctx, f.sess.OrgID, f.sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	for _, e := range evs {
		if e.Source == store.SourceSpov && e.Name == MarkerStopped {
			stopped = true
		}
		if e.Source != store.SourceSpov || e.Name != MarkerSteered {
			continue
		}
		var r jobs.SteerRecord
		if err := json.Unmarshal(e.Raw, &r); err != nil || r != (jobs.SteerRecord{Message: "msg_mine", Delivery: DeliverySteer, After: AfterHandover}) {
			t.Errorf("the record = %s, %v", e.Raw, err)
		}
		if !stopped {
			t.Error("the record came before the stop note")
		}
		return
	}
	t.Error("no record of the send past the storage limit")
}
