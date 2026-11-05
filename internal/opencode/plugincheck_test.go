package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run gate 1's half of the plugin under a real JS runtime (bun,
// else node; skipped if neither) and check that exit codes, timeouts and
// output land as evidence JSON the server can read.
//
// @decision(DL-050) @decision(DL-039)

// runPluginCheck renders the plugin with a check command, drives its
// runCheck under jsRuntime, and returns the evidence lines it wrote.
func runPluginCheck(t *testing.T, command string, timeoutMS int) []map[string]any {
	t.Helper()
	return runPluginCheckIn(t, jsRuntime(t), command, timeoutMS)
}

// jsRuntime picks an interpreter, preferring bun, which the sandbox runs
// the plugin under, and falling back to node, which agreed with bun on
// these cases.
func jsRuntime(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"bun", "node"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("neither bun nor node is installed; this exercises the plugin's own JavaScript")
	return ""
}

func runPluginCheckIn(t *testing.T, runtime, command string, timeoutMS int) []map[string]any {
	t.Helper()

	dir := t.TempDir()
	evidence := filepath.Join(dir, "evidence.jsonl")

	// Dir is the test's own directory, not WorkspaceRoot: the check spawns
	// with an explicit cwd, and /workspace does not exist here.
	//
	// @decision(DL-037)
	source, err := Plugin{
		Kind: "report", Goal: "g", CheckCommand: command, Dir: dir,
	}.Source()
	if err != nil {
		t.Fatal(err)
	}
	// The rendered config points at the real evidence path and the
	// real timeout; both are overridden here so the test writes into
	// its own directory and does not wait out CheckTimeout.
	source = strings.Replace(source, jsonString(t, PluginEvidencePath), jsonString(t, evidence), 1)
	source = strings.Replace(source, jsonString(t, PluginOutcomePath), jsonString(t, evidence+".outcome"), 1)
	source = strings.Replace(source,
		`"checkTimeoutMs":`+itoa(int(CheckTimeout/1e6)),
		`"checkTimeoutMs":`+itoa(timeoutMS), 1)

	// Driven through the plugin's own runCheck, so what is measured is
	// the shipped function rather than a copy of it.
	source += "\n\nawait runCheck()\n"
	script := filepath.Join(dir, "plugin.mjs")
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(runtime, script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the plugin: %v\n%s", err, out)
	}

	body, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatalf("the plugin wrote no evidence: %v\n%s", err, out)
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

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// checkLineFrom finds the check event among the evidence.
func checkLineFrom(t *testing.T, lines []map[string]any) map[string]any {
	t.Helper()
	for _, l := range lines {
		if l["event"] == CheckEvent {
			return l
		}
	}
	t.Fatalf("no %q line among %d evidence lines: %v", CheckEvent, len(lines), lines)
	return nil
}

// A passing check records exit 0, which is what gate 1 reads as a pass.
//
// @decision(DL-050)
func TestTheCheckRecordsAPassingCommand(t *testing.T) {
	got := checkLineFrom(t, runPluginCheck(t, "exit 0", 5000))
	if got["exit"] != float64(0) {
		t.Errorf("exit = %v, want 0: a passing check must record a zero status", got["exit"])
	}
	if got["timeout"] == true {
		t.Error("a command that exited immediately was recorded as a timeout")
	}
}

// TestTheCheckRunsInTheSessionsDirectory pins the check's cwd to the
// plugin's Dir rather than the directory the process was started in.
//
// @decision(DL-037) @decision(DL-050)
func TestTheCheckRunsInTheSessionsDirectory(t *testing.T) {
	got := checkLineFrom(t, runPluginCheck(t, "pwd", 5000))
	out, _ := got["output"].(string)
	if strings.TrimSpace(out) == "" {
		t.Fatalf("the check recorded no output, so this proves nothing: %v", got)
	}
	// runPluginCheckIn sets Dir to its own t.TempDir and passes the
	// same one as the evidence directory, so the check's cwd is
	// observable in the recorded output.
	if got["exit"] != float64(0) {
		t.Fatalf("pwd did not run (exit %v, output %q)", got["exit"], out)
	}
	// The cwd must be the directory the plugin was given, not the one
	// this test process happens to be running in.
	self, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) == self {
		t.Errorf("the check ran in the test process's own directory (%s):"+
			" the cwd is inherited rather than sent", self)
	}
}

// A failing check records its exit code and its output. The output
// matters: it is what a human reads on the job row, and gate 1 puts its
// first line into the summary.
//
// @decision(DL-050)
func TestTheCheckRecordsAFailingCommandAndItsOutput(t *testing.T) {
	got := checkLineFrom(t, runPluginCheck(t, "echo 'FAIL internal/server' >&2; exit 3", 5000))
	if got["exit"] != float64(3) {
		t.Errorf("exit = %v, want 3", got["exit"])
	}
	out, _ := got["output"].(string)
	if !strings.Contains(out, "FAIL internal/server") {
		t.Errorf("output = %q, want the command's stderr: a failing check with no output says nothing", out)
	}
}

// A check that runs too long is recorded as a timeout with no exit code:
// a killed process's signal exit would look like an ordinary failure.
//
// @decision(DL-050)
func TestASlowCheckIsATimeoutRatherThanAFailure(t *testing.T) {
	got := checkLineFrom(t, runPluginCheck(t, "sleep 30", 300))
	if got["timeout"] != true {
		t.Fatalf("timeout = %v, want true: a slow suite would be read as a broken one", got["timeout"])
	}
	if _, hasExit := got["exit"]; hasExit {
		t.Errorf("a timed-out check also recorded exit %v; gate 1 would read it as a failure", got["exit"])
	}
}

// TestATimedOutCheckStopsWhatItStarted checks a timed-out check kills its
// process group, so an orphan cannot hold the plugin open. It asserts the
// wall clock because the evidence line is correct either way.
//
// @decision(DL-050)
func TestATimedOutCheckStopsWhatItStarted(t *testing.T) {
	// Run in every runtime present: the sandbox's is bun, and a
	// process-group kill could differ between them.
	var ran int
	for _, name := range []string{"bun", "node"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		ran++
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			got := checkLineFrom(t, runPluginCheckIn(t, path, "sleep 30", 300))
			elapsed := time.Since(start)

			if got["timeout"] != true {
				t.Fatalf("timeout = %v, want true", got["timeout"])
			}
			// Generous: the point is 30s versus well under it, not a
			// precise figure. A CI machine may be slow; it will not be
			// 25 seconds slow.
			if elapsed > 10*time.Second {
				t.Errorf("under %s the plugin took %s to exit after a 300ms timeout: the killed "+
					"command's children outlived it and held the sandbox open for as long as the "+
					"check would have run", name, elapsed)
			}
		})
	}
	if ran == 0 {
		t.Skip("neither bun nor node is installed; this exercises the plugin's own JavaScript")
	}
}

// The check runs before the stop is announced, since the supervisor
// collects the evidence file once, when it sees the marker.
//
// @decision(DL-050) @decision(DL-048)
func TestTheCheckRunsBeforeTheStopIsAnnounced(t *testing.T) {
	source, err := Plugin{Kind: "report", Goal: "g", CheckCommand: "true"}.Source()
	if err != nil {
		t.Fatal(err)
	}
	check := strings.Index(source, "await runCheck()")
	if check < 0 {
		t.Fatal("announceStop never runs the check")
	}
	marker := strings.Index(source, "await reply(ctx, sessionID, line)")
	if marker < 0 {
		t.Fatal("announceStop never sends the marker")
	}
	if check > marker {
		t.Error("the check runs after the marker is sent; the supervisor collects evidence when the marker lands, " +
			"so the result would be written to a file nobody reads again")
	}
}

// A job with no check command configured records nothing, which gate 1
// reads as unverified rather than as a failure.
//
// @decision(DL-050)
func TestNoCheckCommandRecordsNoCheck(t *testing.T) {
	source, err := Plugin{Kind: "report", Goal: "g"}.Source()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, `"checkCommand":""`) {
		t.Error("a plugin with no check does not render an empty checkCommand")
	}
	if !strings.Contains(source, "if (!CONFIG.checkCommand) {") {
		t.Error("runCheck has no guard for a missing command, so it would run an empty shell command " +
			"and record a spurious pass")
	}
}

// The check is not subject to the permission hook. The hook guards what
// the AGENT may do; a check the policy could refuse would be a gate the
// thing being judged can switch off.
//
// @decision(DL-050) @decision(DL-040)
func TestTheCheckIsNotSubjectToThePermissionHook(t *testing.T) {
	source, err := Plugin{Kind: "report", Goal: "g", CheckCommand: "true"}.Source()
	if err != nil {
		t.Fatal(err)
	}
	// It spawns its own process rather than going through anything the
	// agent's permission evaluation can see.
	if !strings.Contains(source, `spawn("/bin/sh", ["-c", CONFIG.checkCommand]`) {
		t.Error("the check does not spawn directly; if it went through a tool call the policy could refuse it")
	}
}

// TestTheGoalIsInjectedWhateverShapeSystemIs runs the context hook
// against all four candidate system[] shapes and checks the goal lands in
// each. Spike S8 measured plain strings.
//
// @decision(DL-039) @decision(DL-050)
func TestTheGoalIsInjectedWhateverShapeSystemIs(t *testing.T) {
	runtime := jsRuntime(t)
	source, err := Plugin{
		Kind: "report", Goal: "GOAL-MARKER",
	}.Source()
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		// What S8 measured on 0.0.0-beta-18684.
		"plain strings":       `["You are a helpful assistant."]`,
		"an empty array":      `[]`,
		"parts keyed text":    `[{"type":"text","text":"You are helpful."}]`,
		"parts keyed content": `[{"role":"system","content":"You are helpful."}]`,
	}
	for name, system := range cases {
		t.Run(name, func(t *testing.T) {
			// The default export is captured rather than exported, so
			// setup() can be driven with a stub context.
			script := strings.Replace(source, "export default {", "const mod = {", 1) + `
let hooks = {}
await mod.setup({
  session: { hook: (n, fn) => { hooks[n] = fn }, synthetic: async () => {} },
  permission: { hook: () => {} },
  command: { transform: () => {} },
  event: { subscribe: async function*(){} },
})
const system = ` + system + `
hooks.context({ system })
console.log(JSON.stringify(system).includes("GOAL-MARKER") ? "INJECTED" : "MISSING")
`
			dir := t.TempDir()
			path := filepath.Join(dir, "plugin.mjs")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(runtime, path).CombinedOutput()
			if err != nil {
				t.Fatalf("running the plugin: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "INJECTED") {
				t.Errorf("the goal was not injected into %s system[]; the agent would report "+
					"having no standing goal and the run would spin to its continuation bound.\n%s",
					name, out)
			}
		})
	}
}
