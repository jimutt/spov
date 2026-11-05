package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests feed the plugin's real setup() a sequence of bus events and
// report what it sent. Like pluginresumerun_test.go's, the stand-in ctx
// proves the plugin's logic, not that OpenCode delivers these events;
// TestIntegrationPluginSeesTheHumanInbox measures that.
//
// @decision(DL-052)

const (
	holdStarted   = `{ type: "session.execution.started", data: { sessionID: "ses_test" } }`
	holdSucceeded = `{ type: "session.execution.succeeded", data: { sessionID: "ses_test" } }`
	// A message from the attached TUI: no spov metadata, so it takes over.
	holdHuman = `{ type: "session.inbox.enqueued", data: { sessionID: "ses_test", inboxID: "msg_h",` +
		` item: { type: "user", payload: { text: "stop" }, delivery: "steer" } } }`
	holdHumanDelivered = `{ type: "session.inbox.delivered", data: { sessionID: "ses_test", inboxID: "msg_h" } }`
	holdHumanCancelled = `{ type: "session.inbox.cancelled", data: { sessionID: "ses_test", inboxID: "msg_h" } }`
	holdContinue       = `{ type: "session.inbox.enqueued", data: { sessionID: "ses_test", inboxID: "msg_c",` +
		` item: { type: "user", payload: { text: "also do x", metadata: { spov: { after: "continue" } } }, delivery: "queue" } } }`
	holdContinueDelivered = `{ type: "session.inbox.delivered", data: { sessionID: "ses_test", inboxID: "msg_c" } }`
	holdSynthetic         = `{ type: "session.inbox.enqueued", data: { sessionID: "ses_test", inboxID: "msg_s",` +
		` item: { type: "synthetic", payload: { text: "continue" } } } }`
)

// driveEvents renders the plugin, runs setup(), delivers events through
// its own subscription and then, if command is set, runs /spov-goal with
// it.
func driveEvents(t *testing.T, events []string, command string) resumeRun {
	t.Helper()
	return driveEventsPaced(t, events, command, 0, nil)
}

// driveEventsPaced is driveEvents with gapMs between events, for a
// plugin that acts on a timer, and edit applied to the rendered source.
func driveEventsPaced(t *testing.T, events []string, command string, gapMs int, edit func(string) string) resumeRun {
	t.Helper()
	runtime := jsRuntime(t)

	dir := t.TempDir()
	evidence := filepath.Join(dir, "evidence.jsonl")
	synthOut := filepath.Join(dir, "synthetic.jsonl")

	source, err := Plugin{Kind: "report", Goal: "g", CheckCommand: "exit 0"}.Source()
	if err != nil {
		t.Fatal(err)
	}
	source = strings.Replace(source, jsonString(t, PluginEvidencePath), jsonString(t, evidence), 1)
	source = strings.Replace(source, jsonString(t, PluginOutcomePath), jsonString(t, evidence+".outcome"), 1)
	if edit != nil {
		source = edit(source)
	}

	harness := `

import * as __fs from "node:fs"

const __synth = []
let __command = null
const __events = [` + strings.Join(events, ",\n") + `]
const __subscription = {
  [Symbol.asyncIterator]: () => {
    let i = 0
    return {
      next: async () => {
        if (i < __events.length) {
          if (` + strconv.Itoa(gapMs) + ` > 0) await new Promise((r) => setTimeout(r, ` + strconv.Itoa(gapMs) + `))
          return { value: __events[i++], done: false }
        }
        return new Promise(() => {})
      },
    }
  },
}
const __ctx = {
  session: { hook: () => {}, synthetic: async (msg) => { __synth.push(msg) } },
  permission: { hook: () => {} },
  command: { transform: (fn) => fn({ add: (def) => { __command = def } }) },
  tool: { transform: () => {} },
  event: { subscribe: () => __subscription },
}

await __spovDefault.setup(__ctx)
await new Promise((r) => setTimeout(r, 50 + ` + strconv.Itoa(2*gapMs*len(events)) + `))
if (` + jsonString(t, command) + ` !== "") {
  await __command.execute({ sessionID: "ses_test", prompt: { text: ` + jsonString(t, command) + ` } })
  await new Promise((r) => setTimeout(r, 50))
}
__fs.writeFileSync(` + jsonString(t, synthOut) + `, JSON.stringify(__synth))
console.log("SPOV_TEST done")
process.exit(0)
`
	source = strings.Replace(source, "export default {", "const __spovDefault = {", 1)
	source += harness

	script := filepath.Join(dir, "plugin.mjs")
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(runtime, script).CombinedOutput()
	if err != nil {
		t.Fatalf("driving the plugin: %v\n%s", err, out)
	}
	run := resumeRun{Stdout: string(out)}
	run.Evidence = readJSONLines(t, evidence, run.Stdout)
	body, err := os.ReadFile(synthOut)
	if err != nil {
		t.Fatalf("the harness wrote no synthetic log:\n%s", out)
	}
	if err := json.Unmarshal(body, &run.Synth); err != nil {
		t.Fatalf("the synthetic log is not JSON: %v", err)
	}
	return run
}

func evidenceNamed(run resumeRun, name string) []map[string]any {
	var found []map[string]any
	for _, e := range run.Evidence {
		if e["event"] == name {
			found = append(found, e)
		}
	}
	return found
}

// A person's message delivered mid-turn means the turn's end sends no
// continuation, and the hold itself says nothing to the agent.
//
// @decision(DL-052)
func TestAHumanMessageHoldsTheLoopAtOnce(t *testing.T) {
	run := driveEvents(t, []string{holdStarted, holdHuman, holdHumanDelivered, holdSucceeded}, "")
	if len(run.Synth) != 0 {
		t.Fatalf("the plugin sent %d synthetics after a human message, want none: %v", len(run.Synth), run.Synth)
	}
	if len(evidenceNamed(run, "loop.held")) != 1 {
		t.Errorf("no loop.held record; evidence: %v", run.Evidence)
	}
	skipped := evidenceNamed(run, "continue.skipped")
	if len(skipped) != 1 || skipped[0]["reason"] != "paused" {
		t.Errorf("continue.skipped = %v, want one with reason paused", skipped)
	}
}

// The hold waits for the delivery: an enqueue alone holds nothing, and a
// message cancelled before its delivery never holds.
//
// @decision(DL-052)
func TestATakeoverHoldsOnlyOnceDelivered(t *testing.T) {
	run := driveEvents(t, []string{holdStarted, holdHuman}, "")
	if len(evidenceNamed(run, "loop.held")) != 0 {
		t.Errorf("an enqueue alone held the loop: %v", run.Evidence)
	}
	run = driveEvents(t, []string{holdStarted, holdHuman, holdHumanCancelled, holdHumanDelivered, holdSucceeded}, "")
	if len(evidenceNamed(run, "loop.held")) != 0 {
		t.Errorf("a cancelled message held the loop: %v", run.Evidence)
	}
	if len(evidenceNamed(run, "continue.sent")) != 1 {
		t.Errorf("want one continuation after a cancelled message; evidence: %v", run.Evidence)
	}
}

// A message that says the job continues is read and the loop carries on.
//
// @decision(DL-052)
func TestAMessageThatContinuesDoesNotHold(t *testing.T) {
	run := driveEvents(t, []string{holdStarted, holdContinue, holdContinueDelivered, holdSucceeded}, "")
	if len(evidenceNamed(run, "loop.held")) != 0 {
		t.Fatalf("a continue message held the loop: %v", run.Evidence)
	}
	if len(run.Synth) != 1 {
		t.Fatalf("want one continuation, got %v", run.Synth)
	}
}

// The plugin's own continuations and spov's opening goal are synthetic,
// so they never hold the loop, or every job would stop after its first
// turn. The continuation is queued, so it never goes ahead of a person's
// queued message.
//
// @decision(DL-052) @decision(DL-045)
func TestTheLoopsOwnItemsDoNotHoldIt(t *testing.T) {
	run := driveEvents(t, []string{holdStarted, holdSynthetic, holdSucceeded}, "")
	if len(evidenceNamed(run, "loop.held")) != 0 {
		t.Fatalf("a synthetic item held the loop: %v", run.Evidence)
	}
	if len(evidenceNamed(run, "continue.sent")) != 1 {
		t.Errorf("want one continuation after a synthetic item; evidence: %v", run.Evidence)
	}
	if len(run.Synth) != 1 || run.Synth[0]["delivery"] != "queue" || run.Synth[0]["resume"] != true {
		t.Errorf("the continuation was sent as %v, want a queued one that resumes", run.Synth)
	}
}

// DL-054's resume clears the hold like any other pause, and nudges an
// idle session back into work with a queued continuation.
//
// @decision(DL-052) @decision(DL-054)
func TestResumeClearsTheHold(t *testing.T) {
	run := driveEvents(t, []string{holdStarted, holdHuman, holdHumanDelivered, holdSucceeded}, "resume")
	if len(evidenceNamed(run, "loop.held")) != 1 {
		t.Fatalf("the loop was never held; evidence: %v", run.Evidence)
	}
	if len(evidenceNamed(run, "nudge.sent")) != 1 {
		t.Errorf("resume did not restart the loop after the hold; evidence: %v", run.Evidence)
	}
	var nudge map[string]any
	for _, m := range run.Synth {
		if m["resume"] == true {
			nudge = m
		}
	}
	if nudge == nil || nudge["delivery"] != "queue" {
		t.Errorf("the nudge was %v, want a queued continuation", nudge)
	}
}

// A second message while held changes nothing and records nothing.
//
// @decision(DL-052)
func TestTheHoldIsRecordedOnce(t *testing.T) {
	second := strings.ReplaceAll(holdHuman, "msg_h", "msg_h2")
	secondDelivered := strings.ReplaceAll(holdHumanDelivered, "msg_h", "msg_h2")
	run := driveEvents(t, []string{holdHuman, holdHumanDelivered, second, secondDelivered}, "")
	if got := evidenceNamed(run, "loop.held"); len(got) != 1 {
		t.Errorf("loop.held recorded %d times, want 1: %v", len(got), got)
	}
}

// The wall clock's stop is final and announced as the wall clock, not as
// the person's `stopped`.
//
// @decision(DL-049)
func TestTheWallClockStopIsAnnouncedAsItself(t *testing.T) {
	run := driveEvents(t, nil, "bound")
	var announced string
	for _, m := range run.Synth {
		if text, _ := m["text"].(string); strings.HasPrefix(text, LoopStoppedMarker) {
			announced = text
		}
	}
	if !strings.Contains(announced, `"reason":"wall-clock"`) {
		t.Fatalf("the stop was announced as %q, want reason wall-clock", announced)
	}
	if len(evidenceNamed(run, "command.bound")) != 1 {
		t.Errorf("no command.bound record; evidence: %v", run.Evidence)
	}
}

// A delivery the subscription missed does not defer completion for the
// rest of the run: an execution that succeeds has drained the queue.
//
// @decision(DL-048)
func TestASucceededExecutionReconcilesTheInbox(t *testing.T) {
	run := driveEvents(t, []string{holdStarted, holdContinue, holdSucceeded}, "")
	if got := evidenceNamed(run, "inbox.reconciled"); len(got) != 1 {
		t.Errorf("inbox.reconciled = %v, want one", got)
	}
}

// A subagent's inbox is not the job's: once the job's session is known,
// another session's takeover holds nothing.
//
// @decision(DL-052)
func TestAnotherSessionsMessageDoesNotHold(t *testing.T) {
	other := strings.ReplaceAll(holdHuman, `sessionID: "ses_test"`, `sessionID: "ses_sub"`)
	otherDelivered := strings.ReplaceAll(holdHumanDelivered, `sessionID: "ses_test"`, `sessionID: "ses_sub"`)
	run := driveEvents(t, []string{holdStarted, other, otherDelivered, holdSucceeded}, "")
	if len(evidenceNamed(run, "loop.held")) != 0 {
		t.Errorf("another session's message held the loop: %v", run.Evidence)
	}
}

// A person's interrupt takes the job over: the execution that answers
// what they sent ends with no continuation. A reject's "shutdown" holds
// nothing.
//
// @decision(DL-053)
func TestAUserInterruptHoldsTheLoop(t *testing.T) {
	interrupted := `{ type: "session.execution.interrupted", data: { sessionID: "ses_test", reason: "user" } }`
	run := driveEvents(t, []string{holdStarted, interrupted, holdStarted, holdSucceeded}, "")
	if got := evidenceNamed(run, "loop.held"); len(got) != 1 || got[0]["reason"] != "interrupted" {
		t.Errorf("loop.held = %v, want one for the interrupt", got)
	}
	if len(run.Synth) != 0 {
		t.Errorf("a continuation went out after a person's interrupt: %v", run.Synth)
	}
	shutdown := strings.ReplaceAll(interrupted, `"user"`, `"shutdown"`)
	run = driveEvents(t, []string{holdStarted, shutdown, holdStarted, holdSucceeded}, "")
	if len(evidenceNamed(run, "loop.held")) != 0 || len(run.Synth) != 1 {
		t.Errorf("a reject's shutdown held the loop: %v", run.Evidence)
	}
}
