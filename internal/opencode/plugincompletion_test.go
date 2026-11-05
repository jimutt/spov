package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

// renderedCompletionPlugin is a plugin rendered as a job would get it.
func renderedCompletionPlugin(t *testing.T) string {
	t.Helper()
	src, err := (&Plugin{
		Kind: "report",
		Goal: "write a report on the flaky test",
	}).Source()
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// TestTheCompletionToolMatchesThePlugin pins CompletionTool to the name
// the rendered plugin registers and the prompt text tells the agent.
//
// @decision(DL-048)
func TestTheCompletionToolMatchesThePlugin(t *testing.T) {
	src := renderedCompletionPlugin(t)

	// The JavaScript reads the name from config rather than repeating
	// it, so the config is what has to carry it.
	cfg := decodeRenderedConfig(t, src)
	if cfg.CompletionTool != CompletionTool {
		t.Errorf("the rendered config names the completion tool %q, Go calls it %q: "+
			"the agent would be told to call a tool that does not exist",
			cfg.CompletionTool, CompletionTool)
	}

	// The registration must read that config value and not a literal of
	// its own, or the config could say one thing and the tool be
	// another.
	if !strings.Contains(src, "name: CONFIG.completionTool") {
		t.Error("the plugin registers the completion tool under something other than " +
			"CONFIG.completionTool, so the name Go renders is not the name the tool has")
	}

	// Both places the agent is TOLD the name must carry the real one.
	// A prompt naming a different tool is the same silent failure from
	// the other direction.
	for _, field := range []struct {
		name string
		text string
	}{
		{"goalText", cfg.GoalText},
		{"continueText", cfg.ContinueText},
	} {
		if !strings.Contains(field.text, CompletionTool) {
			t.Errorf("%s does not name %q, so the agent is never told how to end the job: %q",
				field.name, CompletionTool, field.text)
		}
	}
}

// decodeRenderedConfig reads the config back out of the rendered source.
func decodeRenderedConfig(t *testing.T, src string) pluginConfig {
	t.Helper()
	const prefix = "const CONFIG = "
	i := strings.Index(src, prefix)
	if i < 0 {
		t.Fatal("the rendered plugin has no CONFIG assignment")
	}
	rest := src[i+len(prefix):]
	end := strings.IndexByte(rest, '\n')
	if end < 0 {
		t.Fatal("the CONFIG assignment does not end")
	}
	var cfg pluginConfig
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:end])), &cfg); err != nil {
		t.Fatalf("the rendered config is not JSON: %v", err)
	}
	return cfg
}

// TestTheCompletionToolTakesTheOrdinaryStopPath checks the tool sets
// loop.stopped and announces through the idempotent announceStop rather
// than a stop mechanism of its own.
//
// @decision(DL-048)
func TestTheCompletionToolTakesTheOrdinaryStopPath(t *testing.T) {
	src := renderedCompletionPlugin(t)

	if !strings.Contains(src, `announceStop(ctx, sessionID, "declared")`) {
		t.Error("the completion tool does not announce through announceStop: the run would " +
			"stop continuing but never tell the supervisor, stranding the job at running")
	}
	if !strings.Contains(src, "loop.stopped = true") {
		t.Error("the completion tool does not set loop.stopped, so the event loop would " +
			"continue the run after completion was declared")
	}
	// announceStop is idempotent, and the completion tool relies on it:
	// the event subscription can still deliver turns afterwards.
	if !strings.Contains(src, "if (loop.announced) return") {
		t.Error("announceStop is no longer idempotent; a declared job could be announced " +
			"twice and re-delivered")
	}
}

// TestTheCompletionSummaryIsRecordedAsEvidence checks the summary is
// recorded and that nothing in the plugin branches on it.
//
// @decision(DL-048) @decision(DL-050)
func TestTheCompletionSummaryIsRecordedAsEvidence(t *testing.T) {
	src := renderedCompletionPlugin(t)

	if !strings.Contains(src, `record("completion.declared"`) {
		t.Error("the completion tool records nothing, so a declared completion leaves no " +
			"trace in the evidence file the supervisor collects")
	}
	if !strings.Contains(src, "summary") {
		t.Error("the completion tool takes no summary argument")
	}
	// The summary must not become a gate. Nothing in the plugin should
	// branch on its content — it is written down and otherwise inert.
	if strings.Contains(src, "if (summary") || strings.Contains(src, "summary ===") {
		t.Error("the plugin branches on the summary's content: a job's own claim about its " +
			"work would then decide something, which is what DL-050 refuses")
	}
}

// TestAFailedToolRegistrationSaysSoOutLoud checks registration and its
// failure are recorded, and the failure also printed to stdout, since a
// cancelled job never has its evidence file collected.
//
// @decision(DL-048)
func TestAFailedToolRegistrationSaysSoOutLoud(t *testing.T) {
	src := renderedCompletionPlugin(t)

	if !strings.Contains(src, `record("completion.tool.registered"`) {
		t.Error("a successful registration is not recorded, so a job that never declares " +
			"cannot be told from one whose tool was never there")
	}
	if !strings.Contains(src, `record("completion.tool.failed"`) {
		t.Error("a failed registration is not recorded")
	}
	// The stdout half. record() already logs, but the failure path adds
	// its own line because the consequence needs saying in words rather
	// than as an event name.
	if !strings.Contains(src, "the completion tool could not be registered") {
		t.Error("a failed registration says nothing on stdout; for a job that is later " +
			"cancelled that is the only channel that survives")
	}
}

// TestThePromptDoesNotAskTheAgentToAnnounceInProse checks neither the
// goal nor the continuation invites a prose announcement, and both say a
// reply has no effect.
//
// @decision(DL-048)
func TestThePromptDoesNotAskTheAgentToAnnounceInProse(t *testing.T) {
	cfg := decodeRenderedConfig(t, renderedCompletionPlugin(t))

	for _, field := range []struct {
		name string
		text string
	}{
		{"goalText", cfg.GoalText},
		{"continueText", cfg.ContinueText},
	} {
		lowered := strings.ToLower(field.text)
		// A phrasing that invites a prose announcement.
		if strings.Contains(lowered, "say so and stop") {
			t.Errorf("%s still asks the agent to say it is done: that phrasing produced "+
				"forty turns of prose nothing could hear", field.name)
		}
		// Whatever the wording, the agent must be told that saying it
		// does not work — otherwise prose remains the obvious move.
		if !strings.Contains(lowered, "does nothing") && !strings.Contains(lowered, "has no effect") {
			t.Errorf("%s does not tell the agent that declaring completion in a reply has "+
				"no effect, so prose is still the obvious way to try to stop: %q",
				field.name, field.text)
		}
	}
}

// TestTheCompletionToolIsNotInAnyAllowlist checks no kind's allowlist
// names the completion tool: it asserts no permission action, so an entry
// would grant nothing.
//
// @decision(DL-048) @decision(DL-040)
func TestTheCompletionToolIsNotInAnyAllowlist(t *testing.T) {
	for _, kind := range []string{"pr", "report", "unknown"} {
		p := PermissionsForKind(kind)
		for _, a := range p.Allow {
			if a == CompletionTool || strings.Contains(a, "spov") {
				t.Errorf("kind %q allows %q: the completion tool asserts no permission "+
					"action, so this grants nothing and reads as protection that is not there",
					kind, a)
			}
		}
	}
}

// TestTheCompletionToolIsADirectCallNotCodemodeOnly checks the tool's own
// definition declares codemode: false; without it the tool lands in the
// codemode catalog only and the agent cannot call it.
//
// @decision(DL-048)
func TestTheCompletionToolIsADirectCallNotCodemodeOnly(t *testing.T) {
	src := renderedCompletionPlugin(t)

	if !strings.Contains(src, "options: { codemode: false }") {
		t.Error("the completion tool does not declare codemode: false, so it lands in the " +
			"codemode catalog only: the agent cannot call it directly, the loop never " +
			"stops, and the job runs to its wall clock")
	}
	// It must be on the tool's own definition, beside its name, rather
	// than anywhere in the file.
	i := strings.Index(src, "name: CONFIG.completionTool")
	if i < 0 {
		t.Fatal("the completion tool registration is gone")
	}
	window := src[i:]
	if j := strings.Index(window, "description:"); j > 0 {
		window = window[:j]
	}
	if !strings.Contains(window, "codemode: false") {
		t.Error("codemode: false is not on the completion tool's own definition")
	}
}

// TestTheReportsNeverLandInARepository checks the report instruction
// names OutDir whatever the landing, while the check's directory still
// follows the landing.
//
// @decision(DL-037) @decision(DL-051)
func TestTheReportsNeverLandInARepository(t *testing.T) {
	src, err := (&Plugin{
		Kind: "report", Goal: "g", Dir: "/workspace/spov",
	}).Source()
	if err != nil {
		t.Fatal(err)
	}
	cfg := decodeRenderedConfig(t, src)
	if strings.Contains(cfg.GoalText, "/workspace/spov/out") {
		t.Errorf("an explicit landing moved the reports into a repository: %q", cfg.GoalText)
	}
	if !strings.Contains(cfg.GoalText, OutDir) {
		t.Errorf("the instruction does not name %s: %q", OutDir, cfg.GoalText)
	}
	// The check's cwd still follows the landing; only the reports are
	// pinned. If this stops being true the two have been merged.
	if cfg.Dir != "/workspace/spov" {
		t.Errorf("the check no longer runs in the session's directory: %q", cfg.Dir)
	}
}

// TestAReportJobIsToldWhereToLeaveItsReports checks a report job's
// re-asserted goal, sent on every request so it survives compaction,
// names the absolute reports directory and says each Markdown file there
// is a report, and a pr job's does not.
//
// @decision(DL-050) @decision(DL-039) @decision(DL-051)
func TestAReportJobIsToldWhereToLeaveItsReports(t *testing.T) {
	report, err := (&Plugin{Kind: "report", Goal: "summarise the release notes"}).Source()
	if err != nil {
		t.Fatal(err)
	}
	cfg := decodeRenderedConfig(t, report)

	if !strings.Contains(cfg.GoalText, "summarise the release notes") {
		t.Errorf("the goal did not survive: %q", cfg.GoalText)
	}
	// The directory delivery actually lists, not a hand-written copy.
	// It is absolute: since DL-037 the session is in no repository.
	if !strings.HasPrefix(OutDir, WorkspaceRoot+"/") || !strings.Contains(cfg.GoalText, OutDir) {
		t.Errorf("a report job's re-asserted goal does not name %q, so delivery collects "+
			"nothing and the job fails however well it did the work: %q", OutDir, cfg.GoalText)
	}
	for _, want := range []string{"Every .md file directly in that directory", "first `# ` heading", "relative path"} {
		if !strings.Contains(cfg.GoalText, want) {
			t.Errorf("the report instruction does not say %q: %q", want, cfg.GoalText)
		}
	}
	if strings.Contains(cfg.GoalText, "relative to the repository") {
		t.Error("the instruction still says the path is relative to a repository;" +
			" since DL-037 the session is at the workspace root and is in none")
	}

	// A pr job's deliverable is the pull request, so it must not be told
	// to write reports.
	pr, err := (&Plugin{Kind: "pr", Goal: "fix the flake"}).Source()
	if err != nil {
		t.Fatal(err)
	}
	if prCfg := decodeRenderedConfig(t, pr); strings.Contains(prCfg.GoalText, OutDir) {
		t.Error("a pr job is told to write a report, which is not its deliverable")
	}
}

// TestTheCompletionToolSaysItMustBeTheLastAction checks the description
// says the call must be the agent's last action, what happens to work
// after it, and to finish writes, commits and pushes first.
//
// @decision(DL-048)
func TestTheCompletionToolSaysItMustBeTheLastAction(t *testing.T) {
	src := renderedCompletionPlugin(t)
	i := strings.Index(src, "name: CONFIG.completionTool")
	if i < 0 {
		t.Fatal("the completion tool registration is gone")
	}
	window := src[i:]
	if j := strings.Index(window, "execute:"); j > 0 {
		window = window[:j]
	}
	lowered := strings.ToLower(window)

	// It must say the call is terminal, in words the model can act on.
	if !strings.Contains(lowered, "last action") {
		t.Error("the description does not tell the agent this must be its last action, so " +
			"an agent that declares and then pushes loses the push — and on a pr job the " +
			"verdict reads as a success with no pull request")
	}
	// And it must say what the consequence is, since an instruction
	// without a reason is the one a model talks itself out of.
	if !strings.Contains(lowered, "not collected") && !strings.Contains(lowered, "cut off") {
		t.Error("the description does not say what happens to work done after the call")
	}
	// The specific acts that matter on a pr job.
	for _, act := range []string{"write", "commit", "push"} {
		if !strings.Contains(lowered, act) {
			t.Errorf("the description does not tell the agent to finish its %s before calling", act)
		}
	}
}

// TestEveryJobKindIsToldWhereItIs checks every kind's re-asserted goal
// carries each repository, its directory and CLI, and a pointer to the
// generated AGENTS.md.
//
// @decision(DL-039)
func TestEveryJobKindIsToldWhereItIs(t *testing.T) {
	ws := []WorkspaceRepo{
		{Slug: "acme/widgets", Dir: "widgets", Platform: "codefloe", Host: "codefloe.com", CLI: CLITea},
		{Slug: "acme/api", Dir: "api", Platform: "gh", Host: "github.com", CLI: CLIGh},
	}
	for _, kind := range []string{"pr", "report"} {
		t.Run(kind, func(t *testing.T) {
			src, err := (&Plugin{Kind: kind, Goal: "do the thing", Workspace: ws}).Source()
			if err != nil {
				t.Fatal(err)
			}
			got := decodeRenderedConfig(t, src).GoalText

			// Each repo, its directory, and the CLI that talks to it.
			for _, want := range []string{
				"acme/widgets", "/workspace/widgets", "tea",
				"acme/api", "/workspace/api", "gh",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("the %s job's goal does not carry %q: %q", kind, want, got)
				}
			}
			// And a pointer to the fuller guide, which this
			// deliberately does not duplicate.
			if !strings.Contains(got, AgentsFile) {
				t.Errorf("the %s job's goal does not point at %s", kind, AgentsFile)
			}
			// The goal itself must still survive.
			if !strings.Contains(got, "do the thing") {
				t.Errorf("the goal was lost: %q", got)
			}
		})
	}
}

// TestAPluginWithNoWorkspaceRendersNoPreamble checks a plugin with no
// workspace renders no preamble rather than an empty repository list.
//
// @decision(DL-039)
func TestAPluginWithNoWorkspaceRendersNoPreamble(t *testing.T) {
	src, err := (&Plugin{Kind: "report", Goal: "g"}).Source()
	if err != nil {
		t.Fatal(err)
	}
	got := decodeRenderedConfig(t, src).GoalText
	if strings.Contains(got, "already cloned") {
		t.Errorf("a plugin with no workspace rendered an orientation preamble: %q", got)
	}
	if !strings.Contains(got, "g") {
		t.Error("the goal was lost")
	}
}

// TestTheOrientationCarriesNoCredential checks the orientation, sent on
// every request, carries nothing shaped like a clone URL credential.
//
// @decision(DL-039)
func TestTheOrientationCarriesNoCredential(t *testing.T) {
	src, err := (&Plugin{
		Kind: "pr", Goal: "g",
		Workspace: []WorkspaceRepo{{
			Slug: "acme/widgets", Dir: "widgets", Platform: "codefloe",
			Host: "codefloe.com", CLI: CLITea,
		}},
	}).Source()
	if err != nil {
		t.Fatal(err)
	}
	got := decodeRenderedConfig(t, src).GoalText
	// The shapes a credential takes in a clone URL.
	for _, bad := range []string{"@codefloe.com", "https://x:", "token ", "://"} {
		if strings.Contains(got, bad) {
			t.Errorf("the orientation contains %q, which is where a credential would ride: %q",
				bad, got)
		}
	}
}

// TestAPRJobIsToldToWorkOnABranch checks a pr job is told to branch
// before its first commit and stay off the default branch, and a report
// job is not.
//
// @decision(DL-039) @decision(DL-040)
func TestAPRJobIsToldToWorkOnABranch(t *testing.T) {
	src, err := (&Plugin{Kind: "pr", Goal: "fix the flake"}).Source()
	if err != nil {
		t.Fatal(err)
	}
	got := decodeRenderedConfig(t, src).GoalText
	lowered := strings.ToLower(got)

	if !strings.Contains(lowered, "new branch") {
		t.Errorf("a pr job is not told to work on a branch: %q", got)
	}
	// The prohibition has to be explicit.
	if !strings.Contains(lowered, "never commit or push to the default branch") {
		t.Errorf("a pr job is not told to stay off the default branch: %q", got)
	}
	// And when: the branch must come before the first commit.
	if !strings.Contains(lowered, "before your first commit") {
		t.Errorf("a pr job is not told to branch before committing, which is the ordering "+
			"the real run got wrong: %q", got)
	}

	// A report job produces reports, not a branch, and must not be
	// told otherwise.
	rep, err := (&Plugin{Kind: "report", Goal: "g"}).Source()
	if err != nil {
		t.Fatal(err)
	}
	if repGoal := decodeRenderedConfig(t, rep).GoalText; strings.Contains(repGoal, "new branch") {
		t.Error("a report job is told to open a pull request, which is not its deliverable")
	}
}
