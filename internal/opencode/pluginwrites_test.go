package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// This file runs the plugin's real permission hook against a stand-in
// context, as plugincompletionrun_test.go does for the completion tool.
// The resource shapes fed in are the ones spike S-W1 measured.

// permissionRun is what feeding a sequence of evaluations produced.
type permissionRun struct {
	Effects  []string
	Evidence []map[string]any
	Synth    []map[string]any
}

// evaluation is one permission evaluation as the hook receives it.
type evaluation struct {
	Action    string `json:"action"`
	Resources any    `json:"resources"`
	SessionID string `json:"sessionID,omitempty"`
	// Effect is what reached the hook; empty means "allow".
	Effect string `json:"effect,omitempty"`
}

func edit(resources ...any) evaluation {
	return evaluation{Action: ActionEdit, Resources: resources, SessionID: "ses_test"}
}

// drivePermissions renders p, runs its setup() and passes each
// evaluation through the hook it registered, in order.
func drivePermissions(t *testing.T, p Plugin, evals []evaluation) permissionRun {
	t.Helper()
	runtime := jsRuntime(t)
	dir := t.TempDir()
	evidence := filepath.Join(dir, "evidence.jsonl")
	out := filepath.Join(dir, "out.json")

	source, err := p.Source()
	if err != nil {
		t.Fatal(err)
	}
	source = strings.Replace(source, jsonString(t, PluginEvidencePath), jsonString(t, evidence), 1)
	source = strings.Replace(source, jsonString(t, PluginOutcomePath), jsonString(t, evidence+".outcome"), 1)
	source = strings.Replace(source, "export default {", "const __spovDefault = {", 1)
	input, err := json.Marshal(evals)
	if err != nil {
		t.Fatal(err)
	}
	source += `
import * as __fs from "node:fs"
const __synth = []
let __hook = null
let __command = null
const __ctx = {
  session: { hook: () => {}, synthetic: async (msg) => { __synth.push(msg) } },
  permission: { hook: (name, fn) => { __hook = fn } },
  command: { transform: (fn) => fn({ add: (def) => { __command = def } }) },
  tool: { transform: () => {} },
  event: { subscribe: () => ({ [Symbol.asyncIterator]: () => ({ next: async () => ({ done: true }) }) }) },
}
await __spovDefault.setup(__ctx)
const __effects = []
for (const ev of ` + string(input) + `) {
  if (ev.action === "__pause") {
    await __command.execute({ sessionID: "ses_test", prompt: "pause" })
    __synth.length = 0
    continue
  }
  ev.effect = ev.effect || "allow"
  __hook(ev)
  __effects.push(ev.effect)
}
await new Promise((r) => setTimeout(r, 100))
__fs.writeFileSync(` + jsonString(t, out) + `, JSON.stringify({ effects: __effects, synth: __synth }))
`
	script := filepath.Join(dir, "plugin.mjs")
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, err := exec.Command(runtime, script).CombinedOutput()
	if err != nil {
		t.Fatalf("driving the permission hook: %v\n%s", err, stdout)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("no result: %v\n%s", err, stdout)
	}
	var got struct {
		Effects []string         `json:"effects"`
		Synth   []map[string]any `json:"synth"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	return permissionRun{Effects: got.Effects, Synth: got.Synth, Evidence: readJSONLines(t, evidence, string(stdout))}
}

func reportPlugin(dir string) Plugin {
	return Plugin{Kind: "report", Goal: "g", Dir: dir}
}

// A report job's edit is allowed only when every path resolves inside
// its write roots, against the session's directory.
//
// @decision(DL-040)
func TestAReportJobMayEditOnlyUnderItsWriteRoots(t *testing.T) {
	cases := []struct {
		dir  string
		eval evaluation
		want string
	}{
		{"", edit("out/report.md"), "allow"},
		{"", edit("/workspace/out/report.md"), "allow"},
		{"", edit(".scratch/new/deep/c.txt"), "allow"},
		{"", edit("out/a.md", ".scratch/b.txt"), "allow"},
		{"", edit("./out/./x.md"), "allow"},
		{"", edit("repo/a.txt"), "deny"},
		{"", edit("out/../repo/a.txt"), "deny"},
		{"", edit("../../workspace/out/../repo/x"), "deny"},
		{"", edit("/workspace/output/x"), "deny"},
		{"", edit("/workspace/.scratchy"), "deny"},
		{"", edit("out/a.md", "repo/b.txt"), "deny"},
		{"", edit(), "deny"},
		{"", edit(""), "deny"},
		{"", edit(7), "deny"},
		{"", evaluation{Action: ActionEdit, Resources: "out/a.md"}, "deny"},
		// The deny list is checked first.
		{"", edit("out" + SandboxHome + "/.git-credentials"), "deny"},
		// A landing inside a repository: relative paths are relative to
		// it, and anything outside it arrives absolute (spike S-W1).
		{"/workspace/repo", edit("../out/r.md"), "allow"},
		{"/workspace/repo", edit("/workspace/out/r.md"), "allow"},
		{"/workspace/repo", edit("a.txt"), "deny"},
		{"/workspace/repo", edit("out/r.md"), "deny"},
	}
	for _, c := range cases {
		run := drivePermissions(t, reportPlugin(c.dir), []evaluation{c.eval})
		if len(run.Effects) != 1 || run.Effects[0] != c.want {
			t.Errorf("dir %q, %s %v: %v, want %s", c.dir, c.eval.Action, c.eval.Resources, run.Effects, c.want)
		}
	}
}

// The path grant is a report job's alone: a pr job's edit is unscoped,
// and a kind with no policy still denies.
//
// @decision(DL-040)
func TestOnlyAReportJobsEditIsScopedByPath(t *testing.T) {
	pr := drivePermissions(t, Plugin{Kind: "pr", Goal: "g"}, []evaluation{edit("repo/a.txt")})
	if !slices.Equal(pr.Effects, []string{"allow"}) {
		t.Errorf("a pr job's edit: %v", pr.Effects)
	}
	unknown := drivePermissions(t, Plugin{Kind: "audit", Goal: "g"}, []evaluation{edit("out/a.md")})
	if !slices.Equal(unknown.Effects, []string{"deny"}) {
		t.Errorf("an unknown kind's edit under out: %v", unknown.Effects)
	}
	if got := PermissionsForKind("report").WriteRoots; !slices.Equal(got, []string{"/workspace/out", ScratchDir}) {
		t.Errorf("report write roots %v", got)
	}
	if got := PermissionsForKind("pr").WriteRoots; len(got) != 0 {
		t.Errorf("pr write roots %v", got)
	}
}

func repeat(n int, e evaluation) []evaluation {
	out := make([]evaluation, n)
	for i := range out {
		out[i] = e
	}
	return out
}

func ofEvent(entries []map[string]any, event string) []map[string]any {
	var out []map[string]any
	for _, e := range entries {
		if e["event"] == event {
			out = append(out, e)
		}
	}
	return out
}

// The same denied call gets one note at DenialNoteAt and ends the run at
// DenialStopAt, naming the call in the stop marker.
//
// @decision(DL-040)
func TestARepeatedDenialIsNotedOnceThenEndsTheRun(t *testing.T) {
	denied := edit("repo/a.txt")
	run := drivePermissions(t, reportPlugin(""), repeat(DenialStopAt, denied))
	if len(run.Synth) != 2 {
		t.Fatalf("%d synthetics, want the note and the stop marker: %v", len(run.Synth), run.Synth)
	}
	note, marker := run.Synth[0], run.Synth[1]
	text, _ := note["text"].(string)
	for _, want := range []string{"denied 5 times", `edit ["repo/a.txt"]`, "/workspace/out", ScratchDir, "shell"} {
		if !strings.Contains(text, want) {
			t.Errorf("the note does not say %q: %s", want, text)
		}
	}
	if note["resume"] != false || note["sessionID"] != "ses_test" {
		t.Errorf("the note is %v", note)
	}
	stop, _ := marker["text"].(string)
	if !strings.HasPrefix(stop, LoopStoppedMarker) || !strings.Contains(stop, `"reason":"denied"`) || !strings.Contains(stop, `repo/a.txt`) {
		t.Errorf("the stop marker is %s", stop)
	}
	if n := len(ofEvent(run.Evidence, "denial.noted")); n != 1 {
		t.Errorf("%d notes recorded", n)
	}
	if n := len(ofEvent(run.Evidence, "denial.stopped")); n != 1 {
		t.Errorf("%d stops recorded", n)
	}
}

// An allowed evaluation resets the count, calls that differ do not add
// up, and a call already noted is not noted again.
//
// @decision(DL-040)
func TestOnlyAnUnbrokenRunOfOneDeniedCallCounts(t *testing.T) {
	a, b := edit("repo/a.txt"), edit("repo/b.txt")
	read := evaluation{Action: ActionRead, Resources: []any{"repo/a.txt"}, SessionID: "ses_test"}

	var interrupted []evaluation
	interrupted = append(interrupted, repeat(DenialNoteAt-1, a)...)
	interrupted = append(interrupted, read)
	interrupted = append(interrupted, repeat(DenialNoteAt-1, a)...)
	if run := drivePermissions(t, reportPlugin(""), interrupted); len(run.Synth) != 0 {
		t.Errorf("a run broken by an allow was noted: %v", run.Synth)
	}

	var alternating []evaluation
	for range DenialStopAt {
		alternating = append(alternating, a, b)
	}
	if run := drivePermissions(t, reportPlugin(""), alternating); len(run.Synth) != 0 {
		t.Errorf("two alternating calls were noted: %v", run.Synth)
	}

	var twice []evaluation
	twice = append(twice, repeat(DenialNoteAt, a)...)
	twice = append(twice, read)
	twice = append(twice, repeat(DenialNoteAt, a)...)
	if run := drivePermissions(t, reportPlugin(""), twice); len(run.Synth) != 1 {
		t.Errorf("the same call was noted %d times", len(run.Synth))
	}
}

// A report job's goal says the repositories are read-only and where it
// may write; a pr job's says neither, and its denial note names no roots.
//
// @decision(DL-040)
func TestAReportJobIsToldWhereItMayWrite(t *testing.T) {
	report := goalOf(t, Plugin{Kind: "report", Goal: "g"})
	for _, want := range []string{"read-only", "/workspace/out and " + ScratchDir} {
		if !strings.Contains(report, want) {
			t.Errorf("the report goal does not say %q:\n%s", want, report)
		}
	}
	if pr := goalOf(t, Plugin{Kind: "pr", Goal: "g"}); strings.Contains(pr, "read-only") {
		t.Errorf("a pr goal calls the repositories read-only:\n%s", pr)
	}
	if note := denialNote("pr"); strings.Contains(note, ScratchDir) || !strings.Contains(note, "will not change it") {
		t.Errorf("the pr denial note: %s", note)
	}
}

// goalOf is the goal text the plugin is rendered with.
func goalOf(t *testing.T, p Plugin) string {
	t.Helper()
	source, err := p.Source()
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(source, `"goalText":`)
	if i < 0 {
		t.Fatal("no goalText in the rendered config")
	}
	var goal string
	if err := json.NewDecoder(strings.NewReader(source[i+len(`"goalText":`):])).Decode(&goal); err != nil {
		t.Fatal(err)
	}
	return goal
}

// The external_directory allow that precedes an edit outside the
// session's directory does not break the run, and nothing is noted or
// stopped while the loop is paused for a person.
//
// @decision(DL-040)
func TestTheGuardSeesThroughTheDirectoryCheckAndWaitsOutAPause(t *testing.T) {
	outside := evaluation{Action: ActionExternalDirectory, Resources: []any{"/tmp/*"}, SessionID: "ses_test"}
	var attempts []evaluation
	for range DenialStopAt {
		attempts = append(attempts, outside, edit("/tmp/report.md"))
	}
	run := drivePermissions(t, reportPlugin(""), attempts)
	if len(run.Synth) != 2 {
		t.Errorf("%d synthetics for a write outside the directory, want the note and the stop", len(run.Synth))
	}

	paused := append([]evaluation{{Action: "__pause"}}, repeat(DenialStopAt, edit("repo/a.txt"))...)
	if run := drivePermissions(t, reportPlugin(""), paused); len(run.Synth) != 0 {
		t.Errorf("a paused loop was noted or stopped: %v", run.Synth)
	}
}

// A deny that reached the hook, as GA's policy hook can send, is never
// relaxed by the allowlist, while the same action arriving as allow or
// ask is decided by it.
//
// @decision(DL-039)
func TestAnIncomingDenyIsNeverRelaxed(t *testing.T) {
	read := func(effect string) evaluation {
		return evaluation{Action: ActionRead, Resources: []any{"README.md"}, SessionID: "ses_test", Effect: effect}
	}
	run := drivePermissions(t, reportPlugin(""), []evaluation{read("deny"), read("allow"), read("ask")})
	if want := []string{"deny", "allow", "allow"}; !slices.Equal(run.Effects, want) {
		t.Errorf("effects = %v, want %v", run.Effects, want)
	}
}
