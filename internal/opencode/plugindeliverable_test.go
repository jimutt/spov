package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pullRequestRun is what driving the plugin's tools produced.
type pullRequestRun struct {
	Registered []string
	Returned   []string
	Evidence   []map[string]any
	Outcome    []map[string]any
}

// drivePullRequestTool renders a plugin of the given kind over two
// repositories that share a slug on different hosts plus one of their
// own, runs its real setup(), and calls the pull request tool once per
// input, if the plugin registered it.
func drivePullRequestTool(t *testing.T, kind string, inputs ...string) pullRequestRun {
	t.Helper()
	runtime := jsRuntime(t)
	dir := t.TempDir()
	evidence := filepath.Join(dir, "evidence.jsonl")

	source, err := Plugin{
		Kind: kind, Goal: "g",
		Workspace: []WorkspaceRepo{
			{Slug: "acme/app", Dir: "app", Platform: "codefloe", Host: "codefloe.com"},
			{Slug: "acme/lib", Dir: "lib", Platform: "codefloe", Host: "codefloe.com"},
			{Slug: "acme/lib", Dir: "lib-gh", Platform: "github", Host: "github.com"},
		},
	}.Source()
	if err != nil {
		t.Fatal(err)
	}
	source = strings.Replace(source, jsonString(t, PluginEvidencePath), jsonString(t, evidence), 1)
	source = strings.Replace(source, jsonString(t, PluginOutcomePath), jsonString(t, evidence+".outcome"), 1)

	var calls strings.Builder
	for _, in := range inputs {
		calls.WriteString(`
if (__tools[` + jsonString(t, PullRequestTool) + `]) {
  const r = await __tools[` + jsonString(t, PullRequestTool) + `].execute(` + in + `, { sessionID: "ses_test" })
  console.log("SPOV_TEST returned: " + JSON.stringify(r?.content ?? null))
}
`)
	}
	harness := `

const __tools = {}
const __ctx = {
  session: { hook: () => {}, synthetic: async () => {} },
  permission: { hook: () => {} },
  command: { transform: () => {} },
  tool: { transform: (fn) => fn({ add: (def) => { __tools[def.name] = def } }) },
  event: { subscribe: () => ({ [Symbol.asyncIterator]: () => ({ next: async () => ({ done: true }) }) }) },
}
await __spovDefault.setup(__ctx)
for (const name of Object.keys(__tools)) console.log("SPOV_TEST registered: " + name)
` + calls.String()

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
	var run pullRequestRun
	for _, line := range strings.Split(string(out), "\n") {
		if name, ok := strings.CutPrefix(line, "SPOV_TEST registered: "); ok {
			run.Registered = append(run.Registered, name)
		}
		if r, ok := strings.CutPrefix(line, "SPOV_TEST returned: "); ok {
			run.Returned = append(run.Returned, r)
		}
	}
	run.Evidence = readJSONLines(t, evidence, string(out))
	if _, err := os.Stat(evidence + ".outcome"); err == nil {
		run.Outcome = readJSONLines(t, evidence+".outcome", string(out))
	}
	return run
}

// recorded is the deliverable lines among the evidence.
func (r pullRequestRun) recorded() []map[string]any {
	var out []map[string]any
	for _, e := range r.Evidence {
		if e["event"] == DeliverableEvent {
			out = append(out, e)
		}
	}
	return out
}

// TestThePullRequestToolIsAPRJobsOnly checks only a pr job's plugin
// registers the tool, beside the completion tool.
//
// @decision(DL-051)
func TestThePullRequestToolIsAPRJobsOnly(t *testing.T) {
	pr := drivePullRequestTool(t, "pr")
	if !strings.Contains(strings.Join(pr.Registered, " "), PullRequestTool) {
		t.Errorf("a pr job's plugin did not register %s: %v", PullRequestTool, pr.Registered)
	}
	if !strings.Contains(strings.Join(pr.Registered, " "), CompletionTool) {
		t.Errorf("registering the pull request tool lost the completion tool: %v", pr.Registered)
	}
	report := drivePullRequestTool(t, "report")
	if strings.Contains(strings.Join(report.Registered, " "), PullRequestTool) {
		t.Errorf("a report job's plugin registered %s: %v", PullRequestTool, report.Registered)
	}
}

// TestThePullRequestToolRecordsASelectedRepository checks a listed
// repository is recorded with its platform, a remote prefix is trimmed,
// and a repository the job was not launched with records nothing.
//
// @decision(DL-051)
func TestThePullRequestToolRecordsASelectedRepository(t *testing.T) {
	run := drivePullRequestTool(t, "pr",
		`{repository: "acme/app", branch: "origin/fix-a"}`,
		`{repository: "github.com/acme/lib", branch: "fix-b"}`,
		`{repository: "acme/other", branch: "fix-c"}`,
		`{repository: "acme/lib", branch: "fix-d"}`,
		`{repository: "acme/app", branch: "  "}`,
		`{repository: "CodeFloe.com/ACME/App", branch: "fix-e"}`,
	)
	got := run.recorded()
	if len(got) != 3 {
		t.Fatalf("recorded %d lines, want 3: %v (returned %v)", len(got), got, run.Returned)
	}
	want := []map[string]string{
		{"platform": "codefloe", "repository": "acme/app", "branch": "fix-a"},
		{"platform": "github", "repository": "acme/lib", "branch": "fix-b"},
		// Matched without case, recorded as configured.
		{"platform": "codefloe", "repository": "acme/app", "branch": "fix-e"},
	}
	for i, w := range want {
		for k, v := range w {
			if got[i][k] != v {
				t.Errorf("line %d: %s = %v, want %q", i, k, got[i][k], v)
			}
		}
	}
	// The outcome file holds the same three lines and nothing else the
	// plugin records, so the evidence file's bound cannot lose them.
	if len(run.Outcome) != 3 || run.Outcome[2]["branch"] != "fix-e" {
		t.Errorf("the outcome file = %v, want the three recorded pull requests", run.Outcome)
	}
	for _, e := range run.Outcome {
		if e["event"] != DeliverableEvent {
			t.Errorf("the outcome file holds %v", e["event"])
		}
	}
	if len(run.Returned) != 6 {
		t.Fatalf("returned %d replies: %v", len(run.Returned), run.Returned)
	}
	if !strings.Contains(run.Returned[2], "not one of this job's repositories") {
		t.Errorf("an unselected repository was not refused with a reason: %s", run.Returned[2])
	}
	if !strings.Contains(run.Returned[3], "host/owner/name") {
		t.Errorf("an ambiguous name was not refused with how to name it: %s", run.Returned[3])
	}
	if !strings.Contains(run.Returned[4], "branch") {
		t.Errorf("an empty branch was not refused: %s", run.Returned[4])
	}
}

// TestThePullRequestToolMatchesThePlugin checks the tool and event names
// reach the plugin from Go, and the plugin uses them rather than literals.
//
// @decision(DL-051)
func TestThePullRequestToolMatchesThePlugin(t *testing.T) {
	src, err := (&Plugin{Kind: "pr", Goal: "g"}).Source()
	if err != nil {
		t.Fatal(err)
	}
	cfg := decodeRenderedConfig(t, src)
	if cfg.PullRequestTool != PullRequestTool || cfg.DeliverableEvent != DeliverableEvent {
		t.Errorf("rendered %q/%q, want %q/%q", cfg.PullRequestTool, cfg.DeliverableEvent, PullRequestTool, DeliverableEvent)
	}
	if strings.Contains(pluginSource, `"`+PullRequestTool+`"`) || strings.Contains(pluginSource, `"`+DeliverableEvent+`"`) {
		t.Error("the plugin spells a name as a literal instead of reading CONFIG")
	}
	if !strings.Contains(branchEpilogue, PullRequestTool) {
		t.Error("a pr job's goal does not name the tool it records pull requests with")
	}
}

// A pr job with a GitLab repository is told a pull request is a merge
// request there; one without is not.
//
// @decision(DL-051)
func TestAGitLabJobIsToldAboutMergeRequests(t *testing.T) {
	gl := Plugin{Kind: "pr", Workspace: []WorkspaceRepo{{Slug: "g/s/r", Dir: "r", Host: "gitlab.com", CLI: CLIGlab}}}
	if !strings.Contains(gl.deliverableNote(), "merge request") || !strings.Contains(mergeRequestNote, PullRequestTool) {
		t.Errorf("note = %q", gl.deliverableNote())
	}
	gh := Plugin{Kind: "pr", Workspace: []WorkspaceRepo{{Slug: "o/r", Dir: "r", Host: "github.com", CLI: CLIGh}}}
	if strings.Contains(gh.deliverableNote(), "merge request") {
		t.Errorf("note = %q", gh.deliverableNote())
	}
}

// A withdrawal is its own record line with the flag, for a selected
// repository only, and says it waits on the reader.
//
// @decision(DL-051)
func TestThePullRequestToolRecordsAWithdrawal(t *testing.T) {
	run := drivePullRequestTool(t, "pr",
		`{repository: "acme/app", branch: "fix-a"}`,
		`{repository: "acme/app", branch: "fix-a", withdrawn: true}`,
		`{repository: "acme/other", branch: "fix-a", withdrawn: true}`,
	)
	got := run.recorded()
	if len(got) != 2 || got[0]["withdrawn"] != nil || got[1]["withdrawn"] != true || got[1]["branch"] != "fix-a" {
		t.Fatalf("recorded %v, want the record and its withdrawal", got)
	}
	if len(run.Returned) < 2 || !strings.Contains(run.Returned[1], "Withdrew") || !strings.Contains(run.Returned[1], "wrote to you") {
		t.Errorf("the withdrawal said %v", run.Returned)
	}
	if len(run.Outcome) != 2 {
		t.Errorf("the outcome file = %v, want both lines", run.Outcome)
	}
}
