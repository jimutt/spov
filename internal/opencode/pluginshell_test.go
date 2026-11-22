package opencode

import (
	"strings"
	"testing"
)

// A job's background shell commands in the shapes spike S-OI1 measured
// at 2.3.0: the call's success says the command still runs, the shell's
// exit names its id, and its result is a synthetic item to the job.
const (
	shellBackgrounded = `{ type: "session.tool.success", data: { sessionID: "ses_test", id: "call_s",` +
		` metadata: { status: "running", truncated: false, shellID: "sh_1", pid: 3500 } } }`
	shellFinished = `{ type: "session.tool.success", data: { sessionID: "ses_test", id: "call_f", metadata: { truncated: false } } }`
	shellExited   = `{ type: "shell.exited", data: { id: "sh_1", exit: 3, status: "exited" } }`
	shellResult   = `{ type: "session.inbox.enqueued", data: { sessionID: "ses_test", inboxID: "msg_s",` +
		` item: { type: "synthetic", payload: { text: "<shell id=\"sh_1\" state=\"completed\" command=\"make\">\nok\n</shell>",` +
		` description: "make", metadata: { source: "shell", shellID: "sh_1", jobID: "sh_1", state: "completed", exit: 3 } },` +
		` delivery: "steer" } } }`
)

// The job's turn ending while its background command runs sends no
// continuation; the command's result starts the next turn, and that
// turn's end continues as any other.
//
// @decision(DL-182)
func TestTheLoopWaitsWhileABackgroundCommandOfTheJobRuns(t *testing.T) {
	run := driveEvents(t, []string{holdStarted, shellBackgrounded, holdSucceeded}, "")
	if got := continuations(run); len(got) != 0 {
		t.Fatalf("a continuation went out while the job's background command ran: %v", got)
	}
	waiting := evidenceNamed(run, "continue.waiting")
	if len(waiting) != 1 || waiting[0]["shells"] != float64(1) || waiting[0]["running"] != float64(0) {
		t.Errorf("continue.waiting = %v, want one naming one background command and no subagent", waiting)
	}

	run = driveEvents(t, []string{holdStarted, shellBackgrounded, holdSucceeded, shellResult, holdStarted, holdSucceeded}, "")
	got := continuations(run)
	if len(got) != 1 || got[0]["text"] != continueText {
		t.Errorf("after the result's turn the plugin sent %v, want one ordinary continuation", got)
	}
	if len(evidenceNamed(run, "loop.held")) != 0 {
		t.Error("the command's result took the job over")
	}
}

// The shell's exit ends its part in a hold even when its result never
// reaches the job; a command that finished within its call never holds;
// and another session's background command is not the job's.
//
// @decision(DL-182)
func TestOnlyTheJobsOwnRunningCommandsHoldTheLoop(t *testing.T) {
	for name, events := range map[string][]string{
		"an exited command":          {holdStarted, shellBackgrounded, shellExited, holdSucceeded},
		"a command that finished":    {holdStarted, shellFinished, holdSucceeded},
		"a subagent's command":       {holdStarted, strings.ReplaceAll(shellBackgrounded, `sessionID: "ses_test"`, `sessionID: "ses_kid"`), holdSucceeded},
		"a result that was reported": {holdStarted, shellBackgrounded, shellResult, holdSucceeded},
	} {
		if got := continuations(driveEvents(t, events, "")); len(got) != 1 {
			t.Errorf("%s: the plugin sent %d continuations, want 1", name, len(got))
		}
	}
}

// The hold is bounded as a subagent's is, but a command still running at
// the wait is kept, named in the note, and holds the next turn end too.
//
// @decision(DL-182) @decision(DL-143)
func TestTheWaitNamesABackgroundCommandAndKeepsIt(t *testing.T) {
	run := driveEventsPaced(t, []string{holdStarted, kidCreated, kidStarted, shellBackgrounded, holdSucceeded}, "", 30, withSubagentWait(90))
	got := continuations(run)
	if len(got) != 1 {
		t.Fatalf("at the wait the plugin sent %v, want one continuation", got)
	}
	text, _ := got[0]["text"].(string)
	// The quiet child is dropped; the command is not.
	if !strings.HasPrefix(text, continueText) || !strings.Contains(text, ": 1 background command.") {
		t.Errorf("the bounded continuation says %q, want the ordinary text naming the one background command", text)
	}

	// The bounded continuation's turn ends with the command still
	// running: that end holds again rather than continuing at once.
	run = driveEventsPaced(t, []string{holdStarted, shellBackgrounded, holdSucceeded, holdStarted, holdSucceeded}, "", 120, withSubagentWait(100))
	if waiting := evidenceNamed(run, "continue.waiting"); len(waiting) != 2 {
		t.Errorf("continue.waiting = %v, want the command to hold both turn ends", waiting)
	}
	sent := evidenceNamed(run, "continue.sent")
	if len(sent) == 0 {
		t.Fatalf("no bounded continuation went out: %v", run.Evidence)
	}
	for _, c := range sent {
		if c["waited"] != true || c["shells"] != float64(1) {
			t.Errorf("a continuation went out without the wait, or without naming the command: %v", c)
		}
	}
}

// The bounded note names what is still running, each kind counted, and
// the guide says how a background command reports.
//
// @decision(DL-182)
func TestTheWaitNoteCountsBothKinds(t *testing.T) {
	kidWorking := `{ type: "session.tool.called", data: { sessionID: "ses_kid", id: "call_k" } }`
	second := strings.ReplaceAll(strings.ReplaceAll(shellBackgrounded, "sh_1", "sh_2"), "call_s", "call_t")
	for want, events := range map[string][]string{
		": 1 subagent and 1 background command.": {holdStarted, kidCreated, kidStarted, shellBackgrounded, holdSucceeded, kidWorking},
		": 2 background commands.":               {holdStarted, shellBackgrounded, second, holdSucceeded},
	} {
		run := driveEventsPaced(t, events, "", 30, withSubagentWait(120))
		got := continuations(run)
		if len(got) != 1 {
			t.Errorf("%q: the plugin sent %v, want one bounded continuation", want, got)
			continue
		}
		if text, _ := got[0]["text"].(string); !strings.Contains(text, want) {
			t.Errorf("the bounded continuation says %q, want it to contain %q", text, want)
		}
	}
	if !strings.Contains(SubagentNote, "background") || !strings.Contains(continueText, SubagentNote) {
		t.Errorf("the continuation and the completion tool do not say how a background command reports: %q", SubagentNote)
	}
	var b strings.Builder
	writeSubagentSection(&b, "report")
	if !strings.Contains(b.String(), "background") {
		t.Errorf("a job's guide does not say how a background command reports: %q", b.String())
	}
}

// An end read before the call's own success (a short command, or one
// moved to the background just as it finished) never leaves it held;
// a removed shell has ended too.
//
// @decision(DL-182)
func TestACommandWhoseEndCameFirstIsNeverHeld(t *testing.T) {
	deleted := strings.ReplaceAll(shellExited, "shell.exited", "shell.deleted")
	for name, events := range map[string][]string{
		"exit first":   {holdStarted, shellExited, shellBackgrounded, holdSucceeded},
		"result first": {holdStarted, shellResult, shellBackgrounded, holdSucceeded},
		"removed":      {holdStarted, shellBackgrounded, deleted, holdSucceeded},
	} {
		if got := continuations(driveEvents(t, events, "")); len(got) != 1 {
			t.Errorf("%s: the plugin sent %d continuations, want 1", name, len(got))
		}
	}
}

// A person's own command, which no call of the job's started, changes
// nothing; a stop during a hold is announced at once.
//
// @decision(DL-182)
func TestAPersonsCommandAndAStopAreNotHeld(t *testing.T) {
	persons := strings.ReplaceAll(strings.ReplaceAll(shellResult, "sh_1", "sh_9"), `description: "make", `, "")
	if got := continuations(driveEvents(t, []string{holdStarted, persons, holdSucceeded}, "")); len(got) != 1 {
		t.Errorf("a person's command changed the loop: %d continuations", len(got))
	}
	run := driveEvents(t, []string{holdStarted, shellBackgrounded}, "stop")
	stopped := false
	for _, m := range run.Synth {
		if text, _ := m["text"].(string); strings.HasPrefix(text, LoopStoppedMarker) {
			stopped = true
		}
	}
	if !stopped {
		t.Errorf("a running background command kept the stop from being announced: %v", run.Synth)
	}
}
