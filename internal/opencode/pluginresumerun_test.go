package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the plugin's resume command under a JS runtime to check
// it sends a continuation, except at the limit or during a turn in flight.
// The stand-in ctx is plugincompletionrun_test.go's: it proves the
// plugin's logic, not that OpenCode passes what the stand-in passes.
//
// @decision(DL-054)

// resumeRun is what driving the plugin's resume command produced.
type resumeRun struct {
	Evidence []map[string]any
	Synth    []map[string]any
	Stdout   string
}

// resumeOptions shapes the state the plugin is in when resume is
// called.
type resumeOptions struct {
	// Spent is how many continuations the loop has already sent.
	Spent int
	// InFlight makes the plugin believe a turn is executing, by feeding
	// it a session.execution.started with no matching succeeded.
	InFlight bool
	// Interrupted feeds a started followed by an interrupted, which is
	// what ESC produces — the gesture that precedes most steering.
	Interrupted bool
	// Stopped puts the loop in the state a `/spov-goal stop` leaves it
	// in, which a resume must not undo.
	Stopped bool
	// SubagentFinished runs a child session's whole turn inside the
	// job session's own, which is what a subagent looks like IF
	// OpenCode emits execution events for children.
	SubagentFinished bool
}

// boolInt renders a bool for the harness's JavaScript.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// driveResume renders the plugin, runs its real setup(), invokes the
// registered command with "resume", and reports what the plugin did.
func driveResume(t *testing.T, opts resumeOptions) resumeRun {
	return driveCommand(t, "resume", opts)
}

// driveCommand is driveResume for any of the plugin's own command
// arguments, so a test can drive the pause through the same harness.
//
// @decision(DL-039)
func driveCommand(t *testing.T, arg string, opts resumeOptions) resumeRun {
	t.Helper()
	runtime := jsRuntime(t)

	dir := t.TempDir()
	evidence := filepath.Join(dir, "evidence.jsonl")
	synthOut := filepath.Join(dir, "synthetic.jsonl")

	source, err := Plugin{
		Kind: "report", Goal: "g",
		CheckCommand: "exit 0",
	}.Source()
	if err != nil {
		t.Fatal(err)
	}
	source = strings.Replace(source, jsonString(t, PluginEvidencePath), jsonString(t, evidence), 1)
	source = strings.Replace(source, jsonString(t, PluginOutcomePath), jsonString(t, evidence+".outcome"), 1)
	source = strings.Replace(source,
		`"checkTimeoutMs":`+itoa(int(CheckTimeout/1e6)),
		`"checkTimeoutMs":`+itoa(5000), 1)

	// The event the harness feeds the plugin before resuming, if any.
	// It is delivered through the plugin's OWN subscription rather than
	// by setting loop.inFlight directly, so what is under test is the
	// tracking as well as the guard.
	events := "[]"
	switch {
	case opts.InFlight:
		events = `[{ type: "session.execution.started", data: { sessionID: "ses_test" } }]`
	case opts.Interrupted:
		events = `[
			{ type: "session.execution.started", data: { sessionID: "ses_test" } },
			{ type: "session.execution.interrupted", data: { sessionID: "ses_test" } },
		]`
	case opts.SubagentFinished:
		events = `[
			{ type: "session.execution.started", data: { sessionID: "ses_test" } },
			{ type: "session.execution.started", data: { sessionID: "ses_child" } },
			{ type: "session.execution.succeeded", data: { sessionID: "ses_child" } },
		]`
	}

	harness := `

import * as __fs from "node:fs"

const __synth = []
let __command = null

const __events = ` + events + `
// A subscription that yields the seeded events and then blocks, which
// is what a live one does between turns. Blocking rather than ending
// matters: an iterator that completed would let the plugin's loop exit
// before the command is called.
const __subscription = {
  [Symbol.asyncIterator]: () => {
    let i = 0
    return {
      next: async () => {
        if (i < __events.length) return { value: __events[i++], done: false }
        return new Promise(() => {})
      },
    }
  },
}

const __ctx = {
  session: {
    hook: () => {},
    synthetic: async (msg) => { __synth.push(msg) },
  },
  permission: { hook: () => {} },
  command: {
    transform: (fn) => fn({ add: (def) => { __command = def } }),
  },
  tool: { transform: () => {} },
  event: { subscribe: () => __subscription },
}

await __spovDefault.setup(__ctx)

if (__command === null) {
  console.log("SPOV_TEST no command was registered")
  process.exit(3)
}

// Let the event loop deliver the seeded events before the command runs.
await new Promise((r) => setTimeout(r, 50))

// The budget already spent, set on the plugin's own loop state — the
// alternative is driving ` + itoa(opts.Spent) + ` real turns, which
// measures the harness rather than the guard.
loop.continuations = ` + itoa(opts.Spent) + `

// Stopped as well as paused when the test asks for it. A stop is
// FINAL by DL-039, so a resume must not undo it.
if (` + itoa(boolInt(opts.Stopped)) + `) loop.stopped = true

// Suspended, as a steered job's plugin is.
loop.paused = true

await __command.execute({ sessionID: "ses_test", prompt: { text: ` + jsonString(t, arg) + ` } })

__fs.writeFileSync(` + jsonString(t, synthOut) + `, JSON.stringify(__synth))
console.log("SPOV_TEST done")
`

	source = strings.Replace(source, "export default {", "const __spovDefault = {", 1)
	if strings.Contains(source, "export default") {
		t.Fatal("the plugin has more than one default export; the harness bound the wrong one")
	}
	source += harness

	script := filepath.Join(dir, "plugin.mjs")
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(runtime, script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driving the resume command: %v\n%s", err, out)
	}

	run := resumeRun{Stdout: string(out)}
	run.Evidence = readJSONLines(t, evidence, run.Stdout)
	if body, err := os.ReadFile(synthOut); err == nil {
		if err := json.Unmarshal(body, &run.Synth); err != nil {
			t.Fatalf("the synthetic log is not JSON: %v", err)
		}
	}
	return run
}

// continuationsIn counts the synthetics that are continuations: text
// matching the configured continueText, sent with resume true so a turn
// actually starts.
func continuationsIn(run resumeRun) int {
	n := 0
	for _, m := range run.Synth {
		text, _ := m["text"].(string)
		resume, _ := m["resume"].(bool)
		if resume && text == continueText {
			n++
		}
	}
	return n
}

// TestResumingSendsAContinuation checks a resume on an idle session sends
// one continuation, since no turn end will fire one.
//
// @decision(DL-054)
func TestResumingSendsAContinuation(t *testing.T) {
	run := driveResume(t, resumeOptions{})
	if got := continuationsIn(run); got != 1 {
		t.Fatalf("resume sent %d continuations, want 1:\n%s", got, run.Stdout)
	}
	if eventNamed(run.Evidence, "nudge.sent") == nil {
		t.Error("the nudge was not recorded")
	}
}

// TestResumingAfterManyContinuationsStillContinues checks that no count
// of continuations ends a run: a resume after many still sends one, and
// nothing announces a stop.
//
// @decision(DL-049) @decision(DL-054)
func TestResumingAfterManyContinuationsStillContinues(t *testing.T) {
	run := driveResume(t, resumeOptions{Spent: 400})
	if got := continuationsIn(run); got != 1 {
		t.Errorf("resume after 400 continuations sent %d, want 1:\n%s", got, run.Stdout)
	}
	if stopped := eventNamed(run.Evidence, "loop.stopped"); stopped != nil {
		t.Errorf("the loop stopped on a count: %v", stopped)
	}
}

// TestResumingDuringATurnSendsNothing checks a resume while a turn is in
// flight skips the nudge, since that turn sends its own continuation. The
// in-flight state comes through the real subscription, so the tracking of
// session.execution.started is covered as well as the guard.
//
// @decision(DL-054)
func TestResumingDuringATurnSendsNothing(t *testing.T) {
	run := driveResume(t, resumeOptions{InFlight: true})
	if got := continuationsIn(run); got != 0 {
		t.Errorf("resume sent %d continuations during a turn, want 0:\n%s", got, run.Stdout)
	}
	skipped := eventNamed(run.Evidence, "nudge.skipped")
	if skipped == nil {
		t.Fatalf("the skip was not recorded:\n%s", run.Stdout)
	}
	if skipped["reason"] != "in-flight" {
		t.Errorf("the skip reason is %v, want in-flight", skipped["reason"])
	}
}

// TestResumingAfterAnInterruptedTurnStillNudges checks an interrupted
// turn (ESC) clears the in-flight flag, so a later resume still nudges.
//
// @decision(DL-054)
func TestResumingAfterAnInterruptedTurnStillNudges(t *testing.T) {
	run := driveResume(t, resumeOptions{Interrupted: true})
	if got := continuationsIn(run); got != 1 {
		t.Fatalf("resume sent %d continuations after an interrupted turn, want 1:\n%s", got, run.Stdout)
	}
}

// TestResumingDoesNotUndoAStop checks a resume leaves loop.stopped alone:
// `/spov-goal stop` is final.
//
// @decision(DL-054) @decision(DL-039)
func TestResumingDoesNotUndoAStop(t *testing.T) {
	run := driveResume(t, resumeOptions{Stopped: true})
	if got := continuationsIn(run); got != 0 {
		t.Errorf("resume sent %d continuations after a stop, want 0:\n%s", got, run.Stdout)
	}
}

// TestASubagentTurnDoesNotClearTheJobsInFlightFlag checks a child
// session's .succeeded does not clear the job session's in-flight flag,
// in case OpenCode emits execution events for children (unmeasured).
//
// @decision(DL-054)
func TestASubagentTurnDoesNotClearTheJobsInFlightFlag(t *testing.T) {
	run := driveResume(t, resumeOptions{SubagentFinished: true})
	if got := continuationsIn(run); got != 0 {
		t.Errorf("resume sent %d continuations while the job's own turn was running, want 0:\n%s",
			got, run.Stdout)
	}
	skipped := eventNamed(run.Evidence, "nudge.skipped")
	if skipped == nil || skipped["reason"] != "in-flight" {
		t.Errorf("the skip was %v, want in-flight:\n%s", skipped, run.Stdout)
	}
}

// TestPausingSendsTheNoticeWithoutStartingATurn drives the pause command
// and checks the notice is actually sent, with resume:false, addressed to
// the agent and without naming the completion tool.
//
// @decision(DL-039)
func TestPausingSendsTheNoticeWithoutStartingATurn(t *testing.T) {
	run := driveCommand(t, "pause", resumeOptions{})

	var notice map[string]any
	for _, m := range run.Synth {
		text, _ := m["text"].(string)
		if strings.Contains(text, "paused") {
			notice = m
		}
	}
	if notice == nil {
		t.Fatalf("the pause command sent no notice:\n%s", run.Stdout)
	}
	// resume:false, or the pause starts the turn it exists to prevent
	// and spends a continuation from the job's budget saying so.
	if resume, _ := notice["resume"].(bool); resume {
		t.Error("the pause notice was sent with resume:true, which starts a turn")
	}
	text, _ := notice["text"].(string)
	if !strings.Contains(strings.ToLower(text), "to the agent") {
		t.Errorf("the notice the pause actually sends is not addressed to the agent: %q", text)
	}
	if strings.Contains(text, CompletionTool) {
		t.Errorf("the notice names %s, so an agent told to stop could end the job", CompletionTool)
	}
	// And a pause sends no continuation, which is the mechanism itself.
	if got := continuationsIn(run); got != 0 {
		t.Errorf("pause sent %d continuations, want 0", got)
	}
}
