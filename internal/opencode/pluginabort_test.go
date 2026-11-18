package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests stop the plugin's tools the way OpenCode does, through the
// abort signal on the call's context (spike S-SA3), and run the plugin's
// real setup() as plugincompletionrun_test.go does.
//
// @decision(DL-146)

// abortRun is what a script of completion calls produced.
type abortRun struct {
	Evidence []map[string]any
	Synth    []map[string]any
	Stdout   string
	// Returned is each call's content, in order.
	Returned []string
	// LoopStopped is loop.stopped once the script has run.
	LoopStopped bool
}

// driveCompletionScript renders a report job's plugin whose check is
// check, runs setup(), and then script, which calls the completion tool
// through `complete(context)`, may run /spov-goal with `command(text)`,
// deliver an event through the plugin's subscription with `push(event)`
// and wait with `pause(ms)`.
func driveCompletionScript(t *testing.T, check, script string) abortRun {
	t.Helper()
	runtime := jsRuntime(t)
	dir := t.TempDir()
	evidence := filepath.Join(dir, "evidence.jsonl")
	synthOut := filepath.Join(dir, "synthetic.jsonl")
	source, err := Plugin{Kind: "report", Goal: "g", CheckCommand: check, Dir: dir}.Source()
	if err != nil {
		t.Fatal(err)
	}
	source = strings.Replace(source, jsonString(t, PluginEvidencePath), jsonString(t, evidence), 1)
	source = strings.Replace(source, jsonString(t, PluginOutcomePath), jsonString(t, evidence+".outcome"), 1)
	source = strings.Replace(source, "export default {", "const __spovDefault = {", 1)
	source += `

import * as __fs from "node:fs"
const __synth = []
let __tool = null
let __command = null
const __queue = []
let __wake = null
const push = (ev) => { __queue.push(ev); if (__wake) { __wake(); __wake = null } }
const __ctx = {
  session: { hook: () => {}, synthetic: async (msg) => { __synth.push(msg) }, get: async (arg) => ({ id: arg?.sessionID }) },
  permission: { hook: () => {} },
  command: { transform: (fn) => fn({ add: (def) => { __command = def } }) },
  tool: { transform: (fn) => fn({ add: (def) => { if (def.name === CONFIG.completionTool) __tool = def } }) },
  event: { subscribe: () => ({ [Symbol.asyncIterator]: () => ({ next: async () => {
    while (__queue.length === 0) await new Promise((r) => { __wake = r })
    return { value: __queue.shift(), done: false }
  } }) }) },
}
await __spovDefault.setup(__ctx)
const pause = (ms) => new Promise((r) => setTimeout(r, ms))
const command = (text) => __command.execute({ sessionID: "ses_test", prompt: { text } })
const complete = async (context) => {
  const r = await __tool.execute({ summary: "done" }, { sessionID: "ses_test", ...context })
  console.log("SPOV_TEST returned: " + JSON.stringify(r?.content ?? null))
}
` + script + `
console.log("SPOV_TEST loop.stopped: " + String(loop.stopped))
__fs.writeFileSync(` + jsonString(t, synthOut) + `, JSON.stringify(__synth))
process.exit(0)
`
	path := filepath.Join(dir, "plugin.mjs")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(runtime, path)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driving the completion tool: %v\n%s", err, out)
	}
	run := abortRun{Stdout: string(out)}
	for _, line := range strings.Split(run.Stdout, "\n") {
		if r, ok := strings.CutPrefix(line, "SPOV_TEST returned: "); ok {
			run.Returned = append(run.Returned, r)
		}
		if strings.TrimSpace(line) == "SPOV_TEST loop.stopped: true" {
			run.LoopStopped = true
		}
	}
	run.Evidence = readJSONLines(t, evidence, run.Stdout)
	body, err := os.ReadFile(synthOut)
	if err != nil {
		t.Fatalf("the harness wrote no synthetic log:\n%s", out)
	}
	if err := json.Unmarshal(body, &run.Synth); err != nil {
		t.Fatal(err)
	}
	return run
}

func (r abortRun) named(name string) []map[string]any {
	var out []map[string]any
	for _, e := range r.Evidence {
		if e["event"] == name {
			out = append(out, e)
		}
	}
	return out
}

func (r abortRun) markers() []string {
	var out []string
	for _, m := range r.Synth {
		if text, _ := m["text"].(string); strings.HasPrefix(text, LoopStoppedMarker) {
			out = append(out, text)
		}
	}
	return out
}

// An interrupt while the check runs stops the check's whole group and
// withdraws the declaration: nothing of it counts, no marker goes out,
// and the loop runs on. A later call runs the check again and declares.
//
// @decision(DL-146) @decision(DL-048)
func TestAnInterruptDuringTheCheckWithdrawsTheCompletion(t *testing.T) {
	run := driveCompletionScript(t, "sleep 1.5; touch late; exit 0", `
const ac = new AbortController()
setTimeout(() => ac.abort(), 200)
await complete({ signal: ac.signal })
console.log("SPOV_TEST after the first call loop.stopped: " + String(loop.stopped))
await pause(2000)
console.log("SPOV_TEST late: " + String(__fs.existsSync("late")))
`)
	if len(run.Returned) != 1 || !strings.Contains(strings.ToLower(run.Returned[0]), "withdrawn") {
		t.Errorf("the call answered %v, want that the declaration was withdrawn", run.Returned)
	}
	if n := len(run.named("completion.declared")); n != 0 {
		t.Errorf("an interrupted completion recorded %d declarations", n)
	}
	if len(run.named("completion.interrupted")) != 1 {
		t.Errorf("no completion.interrupted record: %v", run.Evidence)
	}
	if n := len(run.named(CheckEvent)); n != 0 {
		t.Errorf("the killed check was recorded as %v", run.named(CheckEvent))
	}
	if m := run.markers(); len(m) != 0 {
		t.Errorf("a withdrawn completion sent the stop marker: %v", m)
	}
	if run.LoopStopped {
		t.Error("the loop is still stopped after the declaration was withdrawn")
	}
	if !strings.Contains(run.Stdout, "SPOV_TEST late: false") {
		t.Errorf("the check went on after the interrupt:\n%s", run.Stdout)
	}

	again := driveCompletionScript(t, "exit 0", `
const ac = new AbortController()
ac.abort()
await complete({ signal: ac.signal })
await complete({})
`)
	if len(again.named("completion.interrupted")) != 1 || len(again.named("completion.declared")) != 1 {
		t.Errorf("want one withdrawn and one declared call: %v", again.Evidence)
	}
	if len(again.named(CheckEvent)) != 1 {
		t.Errorf("the accepted call did not run the check: %v", again.Evidence)
	}
	if m := again.markers(); len(m) != 1 || !strings.Contains(m[0], `"reason":"`+CompletionReason+`"`) {
		t.Errorf("the accepted call's marker is %v", m)
	}
}

// A signal that fires once the marker has gone out changes nothing:
// delivery has begun.
//
// @decision(DL-146)
func TestAnInterruptAfterTheMarkerChangesNothing(t *testing.T) {
	run := driveCompletionScript(t, "exit 0", `
const ac = new AbortController()
await complete({ signal: ac.signal })
ac.abort()
await pause(50)
`)
	if !run.LoopStopped || len(run.markers()) != 1 || len(run.named("completion.declared")) != 1 {
		t.Errorf("the declaration did not stand: stopped %v, markers %v, evidence %v", run.LoopStopped, run.markers(), run.Evidence)
	}
	if len(run.named("completion.interrupted")) != 0 {
		t.Errorf("a signal after the marker withdrew the declaration: %v", run.Evidence)
	}
}

// A stop that arrived while the check ran is not the call's to undo:
// the wall clock's `bound` (then EndRun's interrupt) or a person's
// `stop` stands, the declaration is recorded, the stop is announced
// under its own reason, and the killed check is not run again.
//
// @decision(DL-146) @decision(DL-049) @decision(DL-050)
func TestAStopThatCameDuringTheCheckStandsAndTheCheckRunsOnce(t *testing.T) {
	for _, c := range []struct{ command, reason string }{{"bound", "wall-clock"}, {"stop", "stopped"}} {
		run := driveCompletionScript(t, "echo ran >> runs; sleep 1; exit 0", `
const ac = new AbortController()
setTimeout(async () => { await command(`+jsonString(t, c.command)+`); ac.abort() }, 200)
await complete({ signal: ac.signal })
await pause(1500)
console.log("SPOV_TEST runs: " + __fs.readFileSync("runs", "utf8").trim().split("\n").length)
`)
		if !run.LoopStopped {
			t.Errorf("%s: the stop was undone by the interrupted completion", c.command)
		}
		if m := run.markers(); len(m) != 1 || !strings.Contains(m[0], `"reason":"`+c.reason+`"`) {
			t.Errorf("%s: the stop was announced as %v, want once as %s", c.command, m, c.reason)
		}
		if n := len(run.named("completion.declared")); n != 1 {
			t.Errorf("%s: %d declarations recorded, want the call's summary kept", c.command, n)
		}
		if len(run.named("completion.interrupted")) != 0 {
			t.Errorf("%s: the declaration was withdrawn though another stop stood", c.command)
		}
		if !strings.Contains(run.Stdout, "SPOV_TEST runs: 1") || len(run.named(CheckEvent)) != 0 || len(run.named("check.interrupted")) != 1 {
			t.Errorf("%s: the check ran more than once, or its kill was not kept: %v\n%s", c.command, run.Evidence, run.Stdout)
		}
	}
}

// A person's interrupt holds the loop whichever comes first, the abort
// of the completion's call or the turn's interrupted frame.
//
// @decision(DL-146) @decision(DL-053)
func TestAnInterruptDuringTheCheckHoldsTheLoopInEitherOrder(t *testing.T) {
	interrupted := `{ type: "session.execution.interrupted", data: { sessionID: "ses_test", reason: "user" } }`
	for name, order := range map[string]string{
		"the frame first": `push(` + interrupted + `); await pause(50); ac.abort()`,
		"the abort first": `ac.abort(); await pause(50); push(` + interrupted + `)`,
	} {
		run := driveCompletionScript(t, "sleep 1; exit 0", `
push({ type: "session.execution.started", data: { sessionID: "ses_test" } })
const ac = new AbortController()
setTimeout(async () => { `+order+` }, 200)
await complete({ signal: ac.signal })
await pause(300)
console.log("SPOV_TEST paused: " + String(loop.paused))
`)
		if run.LoopStopped || !strings.Contains(run.Stdout, "SPOV_TEST paused: true") {
			t.Errorf("%s: the interrupt did not leave the job held by the person:\n%s", name, run.Stdout)
		}
	}
}

// The pull request tool records nothing on a call already stopped.
//
// @decision(DL-146) @decision(DL-051)
func TestAStoppedPullRequestCallRecordsNothing(t *testing.T) {
	run := drivePullRequestToolIn(t, "pr", `{ sessionID: "ses_test", signal: AbortSignal.abort() }`,
		`{ repository: "acme/app", branch: "fix" }`)
	if len(run.Returned) != 1 || !strings.Contains(strings.ToLower(run.Returned[0]), "stopped") {
		t.Errorf("the call answered %v, want that it was stopped", run.Returned)
	}
	for _, e := range run.Evidence {
		if e["event"] == DeliverableEvent {
			t.Errorf("a stopped call recorded %v", e)
		}
	}
	// Without a signal it records as before.
	run = drivePullRequestToolIn(t, "pr", `{ sessionID: "ses_test" }`, `{ repository: "acme/app", branch: "fix" }`)
	found := false
	for _, e := range run.Evidence {
		found = found || e["event"] == DeliverableEvent
	}
	if !found {
		t.Errorf("a call with no signal recorded nothing: %v", run.Evidence)
	}
}
