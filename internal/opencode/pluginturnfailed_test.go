package opencode

import (
	"strings"
	"testing"
)

// failedTurn is a turn the provider refused, in the measured shape
// (2026-11-02, a free model's usage limit).
const failedTurn = `{ type: "session.execution.failed", data: { sessionID: "ses_test", error: ` +
	`{ type: "provider.quota", message: "Free usage limit reached", status: 429 } } }`

// fastRetries renders the plugin with millisecond retry delays.
func fastRetries(source string) string {
	return strings.Replace(source, `"turnRetryDelays":[10000,30000]`, `"turnRetryDelays":[1,1]`, 1)
}

// A failed turn is continued after each retry delay, and one more
// failure in a row ends the run with the error in the stop marker.
//
// @decision(DL-048)
func TestFailedTurnsAreRetriedThenEndTheRun(t *testing.T) {
	events := []string{holdStarted, failedTurn, holdStarted, failedTurn, holdStarted, failedTurn}
	run := driveEventsPaced(t, events, "", 20, fastRetries)
	if len(run.Synth) != len(TurnRetryDelays)+1 {
		t.Fatalf("%d synthetics, want %d retries and the stop: %v", len(run.Synth), len(TurnRetryDelays), run.Synth)
	}
	for _, retry := range run.Synth[:len(TurnRetryDelays)] {
		if retry["resume"] != true || retry["delivery"] != "queue" {
			t.Errorf("a retry is %v, want a queued continuation", retry)
		}
	}
	stop, _ := run.Synth[len(run.Synth)-1]["text"].(string)
	if !strings.HasPrefix(stop, LoopStoppedMarker) || !strings.Contains(stop, `"reason":"`+TurnFailedReason+`"`) ||
		!strings.Contains(stop, "Free usage limit reached") {
		t.Errorf("the stop marker is %s", stop)
	}
	if n := len(evidenceNamed(run, "turn.failed")); n != 3 {
		t.Errorf("%d failures recorded, want 3", n)
	}
}

// A turn that succeeds resets the count, so failures apart never add up.
//
// @decision(DL-048)
func TestASucceededTurnResetsTheFailures(t *testing.T) {
	events := []string{
		holdStarted, failedTurn, holdStarted, failedTurn, holdStarted, holdSucceeded,
		holdStarted, failedTurn,
	}
	run := driveEventsPaced(t, events, "", 20, fastRetries)
	for _, m := range run.Synth {
		if text, _ := m["text"].(string); strings.HasPrefix(text, LoopStoppedMarker) {
			t.Fatalf("the run was ended though no three failures came in a row: %s", text)
		}
	}
	if n := len(evidenceNamed(run, "turn.retried")); n != 3 {
		t.Errorf("%d retries, want one after each failure", n)
	}
}

// A failure while a person holds the loop is theirs to handle: nothing
// is retried and the run is not ended.
//
// @decision(DL-048) @decision(DL-052)
func TestAFailedTurnWhileHeldIsNotRetried(t *testing.T) {
	events := []string{holdHuman, holdHumanDelivered, holdStarted, failedTurn, holdStarted, failedTurn, holdStarted, failedTurn}
	run := driveEventsPaced(t, events, "", 20, fastRetries)
	if len(run.Synth) != 0 {
		t.Errorf("a held loop sent %v", run.Synth)
	}
}
