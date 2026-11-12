package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jimutt/spov/internal/store"
)

// MarkerRecordingStopped is the recorder's note that it stopped recording
// a session: at its storage limit, after its redials, or after a panic.
// MarkerSteered is the server's own record of a person's send, written
// whether or not anything is recorded. Both are declared in
// internal/server too; TestTheRecordingMarkerNamesMatch pins them.
//
// @decision(DL-131) @decision(DL-019)
const (
	MarkerRecordingStopped = store.SpovNamespace + "recorder.stopped"
	MarkerSteered          = store.SpovNamespace + "session.steered"
)

// SteerRecord is MarkerSteered's raw body: which message, how it was
// delivered and, on a job, what follows it.
//
// @decision(DL-131) @decision(DL-052)
type SteerRecord struct {
	Message  string `json:"message,omitempty"`
	Delivery string `json:"delivery,omitempty"`
	After    string `json:"after,omitempty"`
}

// steerRecordOf reads spov's record of a send, or reports that e is not
// one. The source is checked: a sandbox chooses its frames' names.
//
// @decision(DL-131)
func steerRecordOf(e store.Event) (SteerRecord, bool) {
	if e.Source != store.SourceSpov || e.Name != MarkerSteered {
		return SteerRecord{}, false
	}
	var r SteerRecord
	_ = json.Unmarshal(e.Raw, &r)
	return r, true
}

func isRecordingStopped(e store.Event) bool {
	return e.Source == store.SourceSpov && e.Name == MarkerRecordingStopped
}

// unrecorded tracks, over one pass, whether the session's recording had
// stopped before a given position: from a stop marker read in the pass,
// else from one store read, made at most once per pass and only when a
// send needs it.
//
// @decision(DL-131)
type unrecorded struct {
	stoppedAt int64
	looked    bool
}

func (u *unrecorded) saw(e store.Event) {
	if isRecordingStopped(e) && u.stoppedAt == 0 {
		u.stoppedAt = e.Pos
	}
}

// before reports whether the recording stopped before pos. A stop
// marker the store holds past pos says nothing about one before it, and
// is read as no stop.
func (u *unrecorded) before(ctx context.Context, s *Supervisor, job store.Job, pos int64) (bool, error) {
	if u.stoppedAt == 0 && !u.looked {
		u.looked = true
		e, err := s.Store.Conversations().FindEvent(ctx, job.OrgID, job.SessionID, MarkerRecordingStopped, nil)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return false, fmt.Errorf("supervisor: job %s: reading whether its recording stopped: %w", job.ID, err)
		case isRecordingStopped(e):
			u.stoppedAt = e.Pos
		}
	}
	return u.stoppedAt != 0 && u.stoppedAt < pos, nil
}

// sentAfterStop reports whether e is spov's record of a person's send
// that takes the job over and that the recording, stopped before it, can
// no longer show as an inbox frame (DL-052 steers on the delivery).
//
// @decision(DL-131) @decision(DL-052)
func (s *Supervisor) sentAfterStop(ctx context.Context, job store.Job, e store.Event, u *unrecorded) (bool, error) {
	r, ok := steerRecordOf(e)
	if !ok || r.After == AfterContinue {
		return false, nil
	}
	return u.before(ctx, s, job, e.Pos)
}
