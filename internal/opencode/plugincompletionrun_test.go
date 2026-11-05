package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file tests the completion tool by RUNNING it against a stand-in
// context (the shape spov-goal.js already assumes for command.transform),
// rather than by reading the source as plugincompletion_test.go does. It
// proves the plugin's own logic, not that OpenCode passes what the
// stand-in passes; spike S10 and TestIntegrationSpovGoalPluginDrivesTheLoop
// cover the rest.
//
// @decision(DL-048)

// completionRun is what driving the plugin's completion tool produced.
type completionRun struct {
	Evidence []map[string]any
	Outcome  []map[string]any
	Synth    []map[string]any
	Stdout   string
	Returned string
	// LoopStopped is the plugin's own loop.stopped after the call. It
	// is read out of the running plugin rather than inferred, because
	// on the no-session path there is no marker to infer it from.
	LoopStopped bool
}

// driveCompletionTool renders the plugin, runs its real setup() against
// a stand-in context, invokes the registered completion tool, and
// reports what the plugin did.
func driveCompletionTool(t *testing.T, summary string, withSessionID bool) completionRun {
	t.Helper()
	return driveCompletionToolAfter(t, summary, withSessionID, "")
}

// driveCompletionToolAfter is driveCompletionTool with before run in the
// plugin's own module first, for a call made in a particular state.
func driveCompletionToolAfter(t *testing.T, summary string, withSessionID bool, before string) completionRun {
	t.Helper()
	runtime := jsRuntime(t)

	dir := t.TempDir()
	evidence := filepath.Join(dir, "evidence.jsonl")
	synthOut := filepath.Join(dir, "synthetic.jsonl")

	source, err := Plugin{
		Kind: "report", Goal: "g",
		// A check that passes quickly: announceStop runs it before the
		// marker, and this test is about the marker rather than the
		// check. `exit 0` keeps the ordering real without adding time.
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

	sessionLine := `sessionID: "ses_test"`
	if !withSessionID {
		sessionLine = `noSession: true`
	}

	// The harness drives the plugin's OWN default export. Nothing here
	// reimplements the tool; setup() registers it and this invokes what
	// setup() registered.
	harness := `

import * as __fs from "node:fs"

const __synth = []
let __registered = null

const __ctx = {
  session: {
    hook: () => {},
    synthetic: async (msg) => { __synth.push(msg) },
  },
  permission: { hook: () => {} },
  command: { transform: () => {} },
  tool: {
    transform: (fn) => fn({ add: (def) => { __registered = def } }),
  },
  event: { subscribe: () => ({ [Symbol.asyncIterator]: () => ({ next: async () => ({ done: true }) }) }) },
}

await __spovDefault.setup(__ctx)

if (__registered === null) {
  console.log("SPOV_TEST no tool was registered")
  process.exit(3)
}
console.log("SPOV_TEST registered name: " + __registered.name)
` + before + `

const __returned = await __registered.execute(
  { summary: ` + jsonString(t, summary) + ` },
  { ` + sessionLine + ` },
)
console.log("SPOV_TEST returned: " + JSON.stringify(__returned ?? null))
console.log("SPOV_TEST loop.stopped: " + String(loop.stopped))
__fs.writeFileSync(` + jsonString(t, synthOut) + `, JSON.stringify(__synth))
`

	// The plugin's default export is anonymous, so it is bound to a
	// name the harness can reach before the harness is appended.
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
		t.Fatalf("driving the completion tool: %v\n%s", err, out)
	}

	run := completionRun{Stdout: string(out)}
	for _, line := range strings.Split(run.Stdout, "\n") {
		if strings.HasPrefix(line, "SPOV_TEST returned: ") {
			run.Returned = strings.TrimPrefix(line, "SPOV_TEST returned: ")
		}
		if strings.TrimSpace(line) == "SPOV_TEST loop.stopped: true" {
			run.LoopStopped = true
		}
	}
	run.Evidence = readJSONLines(t, evidence, run.Stdout)
	if _, err := os.Stat(evidence + ".outcome"); err == nil {
		run.Outcome = readJSONLines(t, evidence+".outcome", run.Stdout)
	}
	if body, err := os.ReadFile(synthOut); err == nil {
		if err := json.Unmarshal(body, &run.Synth); err != nil {
			t.Fatalf("the synthetic log is not JSON: %v", err)
		}
	}
	return run
}

func readJSONLines(t *testing.T, path, stdout string) []map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the plugin wrote no evidence: %v\n%s", err, stdout)
	}
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("evidence line is not JSON: %q", line)
		}
		lines = append(lines, m)
	}
	return lines
}

// TestAMissingToolDomainDoesNotStopThePluginLoading pins that a build
// with no ctx.tool still loads the plugin and attaches every other hook;
// each hook is wrapped in its own try for exactly this reason. With no
// completion tool the run stops after its first turn, announced with its
// own reason, rather than running to its wall clock.
//
// @decision(DL-048) @decision(DL-039) @decision(DL-049)
func TestAMissingToolDomainDoesNotStopThePluginLoading(t *testing.T) {
	runtime := jsRuntime(t)

	dir := t.TempDir()
	evidence := filepath.Join(dir, "evidence.jsonl")
	source := mustRenderPlugin(t)
	source = strings.Replace(source, jsonString(t, PluginEvidencePath), jsonString(t, evidence), 1)
	source = strings.Replace(source, jsonString(t, PluginOutcomePath), jsonString(t, evidence+".outcome"), 1)
	source = strings.Replace(source, "export default {", "const __spovDefault = {", 1)

	// A context with every domain the plugin uses EXCEPT tool.
	source += `

const __turn = [{ type: "session.execution.succeeded", data: { sessionID: "ses_test" } }]
const __ctx = {
  session: { hook: () => {}, synthetic: async () => {} },
  permission: { hook: () => {} },
  command: { transform: () => {} },
  event: { subscribe: () => ({ [Symbol.asyncIterator]: () => ({
    next: async () => (__turn.length ? { value: __turn.shift(), done: false } : { done: true }),
  }) }) },
}
await __spovDefault.setup(__ctx)
await new Promise((r) => setTimeout(r, 300))
console.log("SPOV_TEST setup survived")
`
	script := filepath.Join(dir, "plugin.mjs")
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(runtime, script).CombinedOutput()
	if err != nil {
		t.Fatalf("setup threw when ctx.tool was absent, so the plugin would not load at all "+
			"and the job would run with no goal and no permission policy: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "SPOV_TEST setup survived") {
		t.Fatalf("setup did not complete without ctx.tool:\n%s", out)
	}

	lines := readJSONLines(t, evidence, string(out))
	// The other hooks must still have attached.
	for _, want := range []string{"goal.hooked", "permission.hooked", "command.registered"} {
		if eventNamed(lines, want) == nil {
			t.Errorf("%s was not recorded: a missing tool domain took another hook down with it", want)
		}
	}
	failed := eventNamed(lines, "completion.tool.failed")
	if failed == nil {
		t.Fatalf("the missing tool domain was not recorded, so a job unable to declare "+
			"completion would give no sign why: %v", lines)
	}
	// And said out loud.
	if !strings.Contains(string(out), "the completion tool could not be registered") {
		t.Errorf("nothing was said on stdout about the failed registration:\n%s", out)
	}
	// The first turn's end stops the run instead of continuing it.
	if eventNamed(lines, "continue.sent") != nil {
		t.Error("the loop continued a run that has no way to declare completion")
	}
	stopped := eventNamed(lines, "loop.stopped")
	if stopped == nil || stopped["reason"] != "no-completion-tool" {
		t.Errorf("the run was not stopped with its own reason: %v\n%s", stopped, out)
	}
}

// registeredNameFrom reads the name the running plugin actually bound.
func registeredNameFrom(t *testing.T, stdout string) string {
	t.Helper()
	const prefix = "SPOV_TEST registered name: "
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	t.Fatalf("the harness reported no registered tool name:\n%s", stdout)
	return ""
}

// mustRenderPlugin renders a job's plugin as the sandbox would get it.
func mustRenderPlugin(t *testing.T) string {
	t.Helper()
	src, err := Plugin{Kind: "report", Goal: "g", CheckCommand: "exit 0"}.Source()
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func eventNamed(lines []map[string]any, name string) map[string]any {
	for _, l := range lines {
		if l["event"] == name {
			return l
		}
	}
	return nil
}

// TestCallingTheCompletionToolEndsTheRun pins that calling the tool
// announces with the declared reason.
//
// @decision(DL-048)
func TestCallingTheCompletionToolEndsTheRun(t *testing.T) {
	run := driveCompletionTool(t, "wrote the report", true)

	if !strings.Contains(run.Stdout, "SPOV_TEST registered name: "+CompletionTool) {
		t.Fatalf("the plugin registered no tool called %q:\n%s", CompletionTool, run.Stdout)
	}

	// The name the agent is TOLD must be the name that was actually
	// registered. Comparing the two Go-side constants would agree with
	// itself; this compares the prompt text against the name the
	// running plugin bound, so a JavaScript literal that drifted from
	// the config would be caught.
	cfg := decodeRenderedConfig(t, mustRenderPlugin(t))
	registered := registeredNameFrom(t, run.Stdout)
	for _, field := range []struct{ name, text string }{
		{"goalText", cfg.GoalText},
		{"continueText", cfg.ContinueText},
	} {
		if !strings.Contains(field.text, registered) {
			t.Errorf("%s tells the agent to call something other than the registered %q: "+
				"it would call nothing and the job would run to its limit",
				field.name, registered)
		}
	}

	stopped := eventNamed(run.Evidence, "loop.stopped")
	if stopped == nil {
		t.Fatalf("calling the tool did not stop the loop; evidence: %v", run.Evidence)
	}
	if stopped["reason"] != CompletionReason {
		t.Errorf("the loop stopped for %q, want %q: the verdict needs to tell a job "+
			"that finished from one that ran out", stopped["reason"], CompletionReason)
	}

	// The marker is what the supervisor actually matches on. An
	// announcement that stopped the loop without sending it would leave
	// the job stranded, and nothing else here would notice.
	var marker string
	for _, m := range run.Synth {
		text, _ := m["text"].(string)
		if strings.HasPrefix(text, LoopStoppedMarker) {
			marker = text
			if m["resume"] != false {
				t.Error("the stop marker was sent with resume true: it would start another " +
					"turn and continue the job it is reporting the end of")
			}
		}
	}
	if marker == "" {
		t.Fatalf("no stop marker was sent; the supervisor would never see the job end: %v", run.Synth)
	}
	if !strings.Contains(marker, CompletionReason) {
		t.Errorf("the marker payload does not carry the declared reason: %q", marker)
	}
}

// TestADeclaredCompletionStillRunsTheCheckFirst pins that the check
// runs before the marker: the supervisor collects evidence in the same
// visit the marker triggers (DL-048), so a check recorded after it is
// never read.
//
// @decision(DL-048) @decision(DL-050)
func TestADeclaredCompletionStillRunsTheCheckFirst(t *testing.T) {
	run := driveCompletionTool(t, "done", true)

	checkAt, stopAt := -1, -1
	for i, l := range run.Evidence {
		switch l["event"] {
		case CheckEvent:
			checkAt = i
		case "loop.stopped":
			stopAt = i
		}
	}
	if checkAt < 0 {
		t.Fatalf("a declared completion ran no check, so gate 1 would be unverified on "+
			"every job that finishes properly: %v", run.Evidence)
	}
	if stopAt < 0 {
		t.Fatal("the loop never recorded stopping")
	}
	if checkAt > stopAt {
		t.Errorf("the check was recorded after the stop (%d > %d): the supervisor collects "+
			"the evidence when the marker arrives, so this check would never be read",
			checkAt, stopAt)
	}
}

// The summary reaches the evidence file, where a human reading the job's
// report can find it.
//
// @decision(DL-048)
func TestTheCompletionSummaryReachesTheEvidence(t *testing.T) {
	const summary = "Replaced the flaky sleep with a fake clock; the suite is green."
	run := driveCompletionTool(t, summary, true)

	declared := eventNamed(run.Evidence, "completion.declared")
	if declared == nil {
		t.Fatalf("no completion.declared line: %v", run.Evidence)
	}
	if declared["summary"] != summary {
		t.Errorf("the recorded summary is %q, want %q", declared["summary"], summary)
	}
}

// TestADeclarationWithoutASessionStillStopsTheLoop pins that a call
// arriving without a session still stops the loop, even though it
// cannot announce; the event subscription announces on the next turn's
// end instead.
//
// @decision(DL-048)
func TestADeclarationWithoutASessionStillStopsTheLoop(t *testing.T) {
	run := driveCompletionTool(t, "done", false)

	if eventNamed(run.Evidence, "completion.declared") == nil {
		t.Fatalf("the declaration was not recorded: %v", run.Evidence)
	}
	if eventNamed(run.Evidence, "completion.no.session") == nil {
		t.Errorf("a declaration with no session was not reported, so the reason the job "+
			"did not announce would be invisible: %v", run.Evidence)
	}
	for _, m := range run.Synth {
		if text, _ := m["text"].(string); strings.HasPrefix(text, LoopStoppedMarker) {
			t.Error("a marker was sent with no session id to send it to")
		}
	}
	// Read out of the plugin's own state rather than inferred from the
	// absence of a marker, which would be true either way.
	if !run.LoopStopped {
		t.Error("a declaration with no session left the loop running: the agent would keep " +
			"working after declaring completion, to its wall clock")
	}
	if !strings.Contains(run.Returned, "ending") {
		t.Errorf("the tool did not tell the agent the job was ending: %q", run.Returned)
	}
}

// TestTheCompletionToolReturnsWhatTheDispatcherAccepts pins that the
// tool's execute returns an object carrying content and declares no
// output schema, matching the dispatcher's contract.
//
// @decision(DL-048) @decision(DL-050)
func TestTheCompletionToolReturnsWhatTheDispatcherAccepts(t *testing.T) {
	run := driveCompletionTool(t, "done", true)

	// The harness prints the JSON-encoded return. A bare string would
	// come back quoted; an object is what the dispatcher needs.
	if run.Returned == "" {
		t.Fatalf("the tool returned nothing:\n%s", run.Stdout)
	}
	var decoded any
	if err := json.Unmarshal([]byte(run.Returned), &decoded); err != nil {
		t.Fatalf("the tool's return is not JSON: %q", run.Returned)
	}
	obj, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("the tool returned %T, not an object: the dispatcher evaluates "+
			`"output" in result and throws "is not an Object" on anything else, `+
			"and the errored part is then read as a refusal that fails the job. Got %q",
			decoded, run.Returned)
	}
	if _, has := obj["content"]; !has {
		t.Errorf("the return carries no content, so the agent is told nothing: %q", run.Returned)
	}
	// With no output schema declared, a return CARRYING output is
	// refused outright ("declared output without an output schema").
	src := mustRenderPlugin(t)
	i := strings.Index(src, "name: CONFIG.completionTool")
	window := src[i:]
	if j := strings.Index(window, "execute:"); j > 0 {
		window = window[:j]
	}
	declaresOutput := strings.Contains(window, "output:")
	if _, has := obj["output"]; has != declaresOutput {
		t.Errorf("the return %s an output but the definition %s an output schema: "+
			"the dispatcher refuses both mismatches",
			map[bool]string{true: "carries", false: "omits"}[has],
			map[bool]string{true: "declares", false: "omits"}[declaresOutput])
	}
}

// TestTheOutcomeLinesAreKeptApart checks a declared completion copies the
// completion, the check and the loop's stop to the outcome file, and
// nothing else, so the evidence file's size bound cannot lose them.
//
// @decision(DL-051) @decision(DL-050)
func TestTheOutcomeLinesAreKeptApart(t *testing.T) {
	run := driveCompletionTool(t, "done", true)
	var events []string
	for _, e := range run.Outcome {
		events = append(events, e["event"].(string))
	}
	if strings.Join(events, ",") != "completion.declared,"+CheckEvent+",loop.stopped" {
		t.Errorf("the outcome file holds %v", events)
	}
	if len(run.Evidence) <= len(run.Outcome) {
		t.Errorf("the evidence file (%d lines) should hold everything the outcome file (%d) does and more",
			len(run.Evidence), len(run.Outcome))
	}
}

// A completion declared while a person's message is queued is deferred:
// nothing is recorded as declared, no check runs, no stop is announced,
// and the agent is told to read the message first. Once it is delivered,
// the same call ends the run.
//
// @decision(DL-048)
func TestCompletionWaitsForAQueuedMessage(t *testing.T) {
	enqueue := `trackInbox({ type: "session.inbox.enqueued", data: { inboxID: "msg_q", item: { type: "user", delivery: "queue" } } })`
	run := driveCompletionToolAfter(t, "done", true, enqueue)
	if run.LoopStopped {
		t.Error("the loop stopped with a person's message still queued")
	}
	if eventNamed(run.Evidence, "completion.declared") != nil {
		t.Error("a deferred call was recorded as a declaration")
	}
	if eventNamed(run.Evidence, "completion.deferred") == nil {
		t.Errorf("no completion.deferred record; evidence: %v", run.Evidence)
	}
	for _, m := range run.Synth {
		if text, _ := m["text"].(string); strings.HasPrefix(text, LoopStoppedMarker) {
			t.Errorf("the stop was announced: %q", text)
		}
	}
	if !strings.Contains(run.Returned, "Not yet") {
		t.Errorf("the agent was told %s", run.Returned)
	}

	delivered := enqueue + `
trackInbox({ type: "session.inbox.delivered", data: { inboxID: "msg_q" } })`
	run = driveCompletionToolAfter(t, "done", true, delivered)
	if !run.LoopStopped || eventNamed(run.Evidence, "completion.declared") == nil {
		t.Errorf("a completion after the message was delivered did not end the run; evidence: %v", run.Evidence)
	}
}
