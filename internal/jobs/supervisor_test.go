package jobs

import (
	"encoding/json"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// inboxFrame builds a session.inbox.enqueued frame carrying an item of
// this type, shaped as the S4/S5 spikes observed it.
func inboxFrame(t *testing.T, itemType string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "session.inbox.enqueued",
		"data": map[string]any{
			"sessionID": "ses_1",
			"item": map[string]any{
				"type":    itemType,
				"payload": map[string]any{"text": "do the thing"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The distinction this test holds is the one DL-045's invariant rests
// on: spov's own continuations arrive on the SAME event as a human's
// prompt and are told apart only by the item's type. Reading the event
// name alone would put every job into steered on its own first turn.
func TestOnlyAUserItemIsATakeover(t *testing.T) {
	for _, tc := range []struct {
		itemType string
		want     bool
	}{
		{"user", true},
		{"synthetic", false},
		{"", false},
		{"assistant", false},
	} {
		e := store.Event{
			Source: store.SourceOpenCode,
			Name:   "session.inbox.enqueued",
			Raw:    inboxFrame(t, tc.itemType),
		}
		if got := takesOver(e); got != tc.want {
			t.Errorf("an inbox item typed %q: takesOver = %v, want %v", tc.itemType, got, tc.want)
		}
		if got := signalFor(e); got != signalNone {
			t.Errorf("an inbox item typed %q gave signal %v on its enqueue", tc.itemType, got)
		}
	}
}

// A frame nobody can parse must not steer a job. Failing the other way
// would let a malformed frame end the loop's ownership of a session.
func TestAnUnreadableInboxFrameDoesNotSteer(t *testing.T) {
	for _, raw := range [][]byte{
		nil,
		[]byte(""),
		[]byte("not json"),
		[]byte(`{"data":{}}`),
		[]byte(`{"data":{"item":"a string, not an object"}}`),
	} {
		e := store.Event{Source: store.SourceOpenCode, Name: "session.inbox.enqueued", Raw: raw}
		if got := signalFor(e); got == signalSteered {
			t.Errorf("an unparseable frame %q steered the job", raw)
		}
	}
}

func TestSignalForNamesTheSupervisorActsOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		want jobSignal
	}{
		{"session.execution.succeeded", signalTurnEnded},
		{"session.execution.failed", signalTurnEnded},
		{"session.execution.interrupted", signalTurnEnded},
		{"permission.asked", signalParked},
		{"permission.replied", signalAnswered},
		// The prefixed forms are NOT what the sandbox sends — S5 and S6
		// measured the bare names, and internal/server/frames.go stores
		// exactly those.
		// Matching a suffix would let a frame the sandbox invents drive
		// the state machine.
		{"session.permission.asked", signalNone},
		{"session.permission.replied", signalNone},
		{"session.compaction.started", signalNone},
		{"", signalNone},
	} {
		e := store.Event{Source: store.SourceOpenCode, Name: tc.name}
		if got := signalFor(e); got != tc.want {
			t.Errorf("%q gave signal %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A spov marker is spov's own voice (DL-046). Reading one back as
// though the session had produced it would let the server's own writes
// drive the state machine.
func TestASpovMarkerSignalsNothing(t *testing.T) {
	for _, name := range []string{
		"session.execution.succeeded",
		"permission.asked",
		"session.inbox.enqueued",
	} {
		e := store.Event{Source: store.SourceSpov, Name: name, Raw: inboxFrame(t, "user")}
		if got := signalFor(e); got != signalNone {
			t.Errorf("a spov marker named %q gave signal %v, want none", name, got)
		}
	}
}

// An ephemeral frame is the sandbox's, so it must be read: the permission
// frames the supervisor depends on arrive only that way.
func TestAnEphemeralPermissionFrameIsRead(t *testing.T) {
	e := store.Event{Source: store.SourceEphemeral, Name: "permission.asked"}
	if got := signalFor(e); got != signalParked {
		t.Errorf("an ephemeral permission.asked gave %v, want signalParked", got)
	}
}

func TestNextState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		from  store.JobState
		sig   jobSignal
		want  store.JobState
		moves bool
	}{
		{"running parks on a permission", store.JobRunning, signalParked, store.JobBlocked, true},
		{"blocked does not re-park", store.JobBlocked, signalParked, store.JobBlocked, false},
		{"running is steered by a human", store.JobRunning, signalSteered, store.JobSteered, true},
		{"a parked job is not steered", store.JobBlocked, signalSteered, store.JobBlocked, false},
		{"a turn ending moves nothing by itself", store.JobRunning, signalTurnEnded, store.JobRunning, false},
		{"an answer does not release a parked job", store.JobBlocked, signalAnswered, store.JobBlocked, false},
		{"a finished job is not reopened", store.JobSucceeded, signalParked, store.JobSucceeded, false},
		{"a cancelled job is not reopened", store.JobCancelled, signalSteered, store.JobCancelled, false},
		// DL-048. A declaration is a statement that the work is done,
		// so it releases a steered job; every other way the loop stops
		// is an ending and does not.
		{"running delivers on an ordinary stop", store.JobRunning, signalStopped, store.JobDelivering, true},
		{"running delivers on a declaration", store.JobRunning, signalDeclared, store.JobDelivering, true},
		{"a steered job delivers on a declaration", store.JobSteered, signalDeclared, store.JobDelivering, true},
		{"a steered job is not ended by the loop merely stopping", store.JobSteered, signalStopped, store.JobSteered, false},
		{"a blocked job is not released by a declaration", store.JobBlocked, signalDeclared, store.JobBlocked, false},
		{"a cancelled job is not reopened by a declaration", store.JobCancelled, signalDeclared, store.JobCancelled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, moves := nextState(tc.from, tc.sig)
			if got != tc.want || moves != tc.moves {
				t.Errorf("nextState(%v, %v) = (%v, %v), want (%v, %v)",
					tc.from, tc.sig, got, moves, tc.want, tc.moves)
			}
		})
	}
}

// Every transition the supervisor writes must be one the state machine
// permits, or the write is refused at the store and the job wedges.
func TestEveryTransitionTheSupervisorWritesIsLegal(t *testing.T) {
	states := []store.JobState{
		store.JobQueued, store.JobProvisioning, store.JobRunning,
		store.JobDelivering, store.JobSucceeded, store.JobFailed,
		store.JobBlocked, store.JobSteered, store.JobCancelled,
	}
	// EVERY signal, derived rather than listed. A hand-maintained list
	// is what let signalStopped stay out of this check while the
	// machine had no edge for it.
	var signals []jobSignal
	for sig := signalNone; sig < signalLast; sig++ {
		signals = append(signals, sig)
	}
	for _, from := range states {
		for _, sig := range signals {
			to, moves := nextState(from, sig)
			if !moves {
				continue
			}
			if !store.CanTransition(from, to) {
				t.Errorf("the supervisor would move %v -> %v on signal %v, which the machine forbids",
					from, to, sig)
			}
		}
	}
}
