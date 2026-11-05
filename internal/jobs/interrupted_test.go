package jobs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// partStoppedByAHuman is the captured shape of a tool part spov's own
// interrupt stopped; TestTheWordingsAreTheMeasuredOnes pins it against
// the constants the rest of this file's tests build from.
// partRefusedByHuman in verdict_test.go holds the other wording.
//
// @decision(DL-053)
const partStoppedByAHuman = `{"type":"tool","name":"shell","executed":false,
  "state":{"status":"error",
           "input":{"command":"sleep 60; echo x"},
           "error":{"type":"aborted","message":"Tool execution interrupted"},
           "metadata":{"shellID":"sh_1"}},
  "time":{"created":1789000413186,"ran":1789000413274,"completed":1789000416668}}`

// TestTheWordingsAreTheMeasuredOnes pins the two message wordings this
// gate leans on as a second signal against independent copies, so a
// rewording in either direction is a test failure rather than a silent
// regression.
//
// @decision(DL-053) @decision(DL-050)
func TestTheWordingsAreTheMeasuredOnes(t *testing.T) {
	parts := toolParts([]byte(`{"content":[` + partStoppedByAHuman + `]}`))
	if len(parts) != 1 {
		t.Fatalf("the S-LS2 capture parsed to %d parts, want 1", len(parts))
	}
	if parts[0].ErrorMessage != interruptedMessage {
		t.Errorf("interruptedMessage = %q, but S-LS2 captured %q", interruptedMessage, parts[0].ErrorMessage)
	}
	if parts[0].RanAt == 0 || parts[0].CompletedAt == 0 {
		t.Errorf("the capture's time.ran/time.completed did not reach the part: %+v", parts[0])
	}
	rejected := toolParts([]byte(`{"content":[` + partRefusedByHuman + `]}`))
	if len(rejected) != 1 {
		t.Fatalf("the S6 capture parsed to %d parts, want 1", len(rejected))
	}
	if rejected[0].ErrorMessage != declinedMessage {
		t.Errorf("declinedMessage = %q, but S6 captured %q", declinedMessage, rejected[0].ErrorMessage)
	}
	if interruptedMessage == declinedMessage {
		t.Error("the two wordings are identical, so the second signal distinguishes nothing")
	}
}

// A plausible sandbox clock, in milliseconds. The absolute value is
// arbitrary; what the tests turn on is the distance between two of
// them.
const clock int64 = 1789000000000

// abortedPart is the shape both a human's reject and spov's own
// interrupt leave, differing only in the message.
func abortedPart(completed int64, message string) string {
	return fmt.Sprintf(`{"type":"tool","name":"shell","executed":false,
	  "state":{"status":"error",
	           "error":{"type":"aborted","message":%q}},
	  "time":{"created":%d,"ran":%d,"completed":%d}}`,
		message, completed-3000, completed-2900, completed)
}

// interruptEvent is session.execution.interrupted as S-LS2 and S-LS3
// captured it: `created` at the frame's top level, `reason` inside
// `data`.
func interruptEvent(created int64, reason string) store.Event {
	return store.Event{
		Source: store.SourceOpenCode,
		Name:   "session.execution.interrupted",
		Raw: []byte(fmt.Sprintf(
			`{"id":"evt_1","created":%d,"type":"session.execution.interrupted",`+
				`"data":{"sessionID":"ses_1","reason":%q}}`, created, reason)),
	}
}

// reportJob is a job whose other two gates pass, so gate 3 is the only
// thing that can decide the outcome.
func reportJob() store.Job {
	return store.Job{ID: "j", OrgID: "o", Kind: store.JobKindReport,
		CheckCommand: "./scripts/check.sh"}
}

func passingGates() []store.Event {
	return []store.Event{evidenceEvent(checkLine(map[string]any{"exit": 0}))}
}

// TestAnInterruptedRunIsNotARefusedOne checks that Stop's own evidence is
// not read by gate 3 as the agent having been forbidden.
//
// @decision(DL-053) @decision(DL-050)
func TestAnInterruptedRunIsNotARefusedOne(t *testing.T) {
	events := append(passingGates(), interruptEvent(clock, stopReasonUser))
	messages := []store.Message{toolMessage(abortedPart(clock+9, interruptedMessage))}

	got := judge(reportJob(), events, messages, aReport())
	if got.State == store.JobBlocked {
		t.Errorf("a job whose turn spov stopped was recorded blocked (%q)", got.Summary)
	}
	var conduct GateResult
	for _, gate := range got.Gates {
		if gate.Gate == GateClassification {
			conduct = gate
		}
	}
	if conduct.Outcome == GateFailed {
		t.Errorf("conduct = failed (%q) for a turn spov itself stopped", conduct.Detail)
	}
	// The detail is what has to be checked, not the outcome: DL-050's
	// softening rewrites an undecided refusal into a passed gate whose
	// detail still says a call was refused.
	//
	// @decision(DL-053) @decision(DL-050)
	for _, phrase := range []string{"refused", "declined through the permission reply"} {
		if strings.Contains(conduct.Detail, phrase) {
			t.Errorf("conduct says %q; nobody refused anything, spov stopped the turn", conduct.Detail)
		}
	}
	if conduct.Outcome != GatePassed {
		t.Errorf("conduct = %v (%q), want passed: every call this run made was allowed to run",
			conduct.Outcome, conduct.Detail)
	}
}

// TestAHumanRejectIsStillARefusal checks that a human's reject, which
// also raises session.execution.interrupted, is not discounted as spov's
// own interrupt.
//
// @decision(DL-053) @decision(DL-050)
func TestAHumanRejectIsStillARefusal(t *testing.T) {
	events := append(passingGates(), interruptEvent(clock, stopReasonShutdown))
	messages := []store.Message{toolMessage(abortedPart(clock-10, declinedMessage))}

	got := judge(reportJob(), events, messages, aReport())
	if got.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked: a refused run was read as one spov stopped (%q)",
			got.State, got.Summary)
	}
	if !strings.Contains(got.Summary, "refused") {
		t.Errorf("summary = %q, want it to say the run was refused", got.Summary)
	}
}

// TestTheInterruptReasonIsLoadBearing checks that only reason "user"
// discounts the part, over otherwise-identical events.
//
// @decision(DL-053)
func TestTheInterruptReasonIsLoadBearing(t *testing.T) {
	part := toolPart{ErrorType: refusalByHuman, ErrorMessage: interruptedMessage, CompletedAt: clock + 9}
	for _, tc := range []struct {
		reason string
		want   bool
	}{
		{stopReasonUser, true},
		{stopReasonShutdown, false},
		{"superseded", false},
		{"inactivity", false},
		{"", false},
	} {
		stops := humanStops([]store.Event{interruptEvent(clock, tc.reason)})
		if got := stoppedByAHuman(part, stops); got != tc.want {
			t.Errorf("reason %q: discounted = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

// TestNeitherSignalDiscountsOnItsOwn checks the message alone and the
// timing alone are each insufficient to discount a part.
//
// @decision(DL-053) @decision(DL-050)
func TestNeitherSignalDiscountsOnItsOwn(t *testing.T) {
	stops := humanStops([]store.Event{interruptEvent(clock, stopReasonUser)})
	near := toolPart{ErrorType: refusalByHuman, ErrorMessage: declinedMessage, CompletedAt: clock + 9}
	if stoppedByAHuman(near, stops) {
		t.Error("a part carrying the reject's wording was discounted because it sat near an interrupt")
	}
	worded := toolPart{ErrorType: refusalByHuman, ErrorMessage: interruptedMessage, CompletedAt: clock + 9}
	if stoppedByAHuman(worded, nil) {
		t.Error("a part was discounted on its message alone, with no interrupt to correlate to")
	}
}

// TestThePartMustSitInsideTheWindow pins stopWindow's two edges.
//
// @decision(DL-053)
func TestThePartMustSitInsideTheWindow(t *testing.T) {
	stops := humanStops([]store.Event{interruptEvent(clock, stopReasonUser)})
	part := func(offset int64) toolPart {
		return toolPart{ErrorType: refusalByHuman, ErrorMessage: interruptedMessage, CompletedAt: clock + offset}
	}
	window := stopWindow.Milliseconds()
	for _, tc := range []struct {
		offset int64
		want   bool
	}{
		{14, true},  // the measured distance
		{-14, true}, // the event can land either side
		{window, true},
		{window + 1, false},
		{-(window + 1), false},
		{17000, false}, // the measured distance to the part a human refused
	} {
		if got := stoppedByAHuman(part(tc.offset), stops); got != tc.want {
			t.Errorf("a part %d ms from the interrupt: discounted = %v, want %v", tc.offset, got, tc.want)
		}
	}
	// A part with no timestamp correlates with nothing.
	if stoppedByAHuman(toolPart{ErrorType: refusalByHuman, ErrorMessage: interruptedMessage}, stops) {
		t.Error("a part carrying no time.completed was discounted anyway")
	}
}

// TestAPolicyRefusalIsNeverDiscounted checks that a different error type
// than an interrupt leaves is never a candidate for the discount.
//
// @decision(DL-053) @decision(DL-050)
func TestAPolicyRefusalIsNeverDiscounted(t *testing.T) {
	stops := humanStops([]store.Event{interruptEvent(clock, stopReasonUser)})
	p := toolPart{ErrorType: refusalByPolicy, ErrorMessage: interruptedMessage, CompletedAt: clock + 9}
	if stoppedByAHuman(p, stops) {
		t.Error("a refusal the plugin's policy made was discounted as spov's own interrupt")
	}
}

// TestAnInterruptedCallCountsAsHavingRun checks that a call a person cut
// off part-way still counts as having run for DL-050's veto.
//
// @decision(DL-053) @decision(DL-050)
func TestAnInterruptedCallCountsAsHavingRun(t *testing.T) {
	stops := humanStops([]store.Event{interruptEvent(clock, stopReasonUser)})
	messages := []store.Message{toolMessage(abortedPart(clock+9, interruptedMessage))}
	if !anyToolRan(messages, stops) {
		t.Error("a call spov interrupted did not count as having run")
	}
	if anyToolRan(messages, nil) {
		t.Error("the same call counted as having run with no interrupt to explain it")
	}
}

// TestAQuestionTheInterruptEndedDoesNotPark checks that an interrupt
// clears only permission questions asked before it, not ones asked
// later or cleared by a human's reject.
//
// @decision(DL-053) @decision(DL-050)
func TestAQuestionTheInterruptEndedDoesNotPark(t *testing.T) {
	ask := func(id string) store.Event {
		return store.Event{Source: store.SourceEphemeral, Name: eventPermissionAsked,
			Raw: []byte(`{"type":"permission.asked","data":{"sessionID":"ses_1","requestID":"` + id + `"}}`)}
	}
	if n := unansweredPermissions([]store.Event{ask("p1")}); n != 1 {
		t.Fatalf("outstanding = %d, want 1; the premise of this test does not hold", n)
	}
	if n := unansweredPermissions([]store.Event{
		ask("p1"), interruptEvent(clock, stopReasonUser),
	}); n != 0 {
		t.Errorf("outstanding = %d, want 0: a question the stop ended still parked the job", n)
	}
	// Asked after the stop, so it belongs to a turn that came later.
	if n := unansweredPermissions([]store.Event{
		interruptEvent(clock, stopReasonUser), ask("p1"),
	}); n != 1 {
		t.Errorf("outstanding = %d, want 1: an earlier stop swallowed a later turn's question", n)
	}
	// A human's reject raises the event with reason "shutdown" and must
	// clear nothing: that job really is parked.
	if n := unansweredPermissions([]store.Event{
		ask("p1"), interruptEvent(clock, stopReasonShutdown),
	}); n != 1 {
		t.Errorf("outstanding = %d, want 1: a reject's event cleared a real park", n)
	}
}

// TestAHumanRejectIsNotASandboxShutdown checks a human's tool-call
// reject, which also carries reason "shutdown", is not read as the
// sandbox timing out.
//
// @decision(DL-053) @decision(DL-050)
func TestAHumanRejectIsNotASandboxShutdown(t *testing.T) {
	events := []store.Event{interruptEvent(clock, stopReasonShutdown)}
	declined := []store.Message{toolMessage(abortedPart(clock-10, declinedMessage))}
	if wasShutDown(events, declined) {
		t.Error("a human's tool-call reject was recorded as a sandbox shutdown worth re-running")
	}
	// A real timeout has no declined call beside it.
	if !wasShutDown(events, []store.Message{toolMessage(partRan)}) {
		t.Error("a genuine shutdown stopped being a retry")
	}
	// Nor does a reject far from the event explain it away.
	far := []store.Message{toolMessage(abortedPart(clock-17000, declinedMessage))}
	if !wasShutDown(events, far) {
		t.Error("a reject 17 s away suppressed the retry note on an unrelated shutdown")
	}
	// The wording is load-bearing on THIS side too, and the mirror of
	// stoppedByAHuman's rule: an aborted part that spov's own interrupt
	// stopped is not a decline, so it must not suppress the note on a
	// shutdown that happens to sit beside it.
	stoppedNearby := []store.Message{toolMessage(abortedPart(clock-10, interruptedMessage))}
	if !wasShutDown(events, stoppedNearby) {
		t.Error("a part spov itself stopped was read as a human's decline and suppressed the retry note")
	}
}

// TestACallStoppedBeforeItStartedDidNotRun checks that time.ran, not
// time.completed, distinguishes a call the stop aborted before it began
// from one cut off part-way, since an aborted part carries a completion
// either way.
//
// @decision(DL-053) @decision(DL-050)
func TestACallStoppedBeforeItStartedDidNotRun(t *testing.T) {
	stops := humanStops([]store.Event{interruptEvent(clock, stopReasonUser)})
	unstarted := `{"type":"tool","name":"shell","executed":false,
	  "state":{"status":"error","error":{"type":"aborted","message":"Tool execution interrupted"}},
	  "time":{"created":1789000000000,"completed":` + fmt.Sprint(clock+9) + `}}`

	if anyToolRan([]store.Message{toolMessage(unstarted)}, stops) {
		t.Error("a call the stop aborted before it began counted as having run")
	}
	if !anyToolRan([]store.Message{toolMessage(partStoppedByAHuman)}, humanStops(
		[]store.Event{interruptEvent(1789000416668, stopReasonUser)})) {
		t.Error("a call a person cut off part-way did not count as having run")
	}

	// DL-050's founding case, with one part that did nothing added.
	job := reportJob()
	events := append(passingGates(), interruptEvent(clock, stopReasonUser))
	got := judge(job, events, []store.Message{
		toolMessage(partRefusedByPolicy, unstarted),
	}, nil)
	if got.State != store.JobBlocked {
		t.Errorf("state = %v, want blocked: a refused run was excused by a call that never ran (%q)",
			got.State, got.Summary)
	}

	// And on its own it is not a pass either: conduct cannot say every
	// call was allowed to run about a call that did not.
	conduct := judgeConduct(events, []store.Message{toolMessage(unstarted)}, stops)
	if conduct.Outcome != GateUnverified {
		t.Errorf("conduct = %v (%q), want unverified for a turn stopped before its call ran",
			conduct.Outcome, conduct.Detail)
	}
	if !strings.Contains(conduct.Detail, "stopped") {
		t.Errorf("conduct says %q, which does not say the calls were stopped", conduct.Detail)
	}
}
