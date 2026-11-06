package opencode

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// PluginDir and PluginFile locate the spov-goal plugin inside the
// sandbox. The path carries a literal $HOME for the shell to expand, for
// the same reason ConfigDir does.
//
// @decision(DL-039)
const (
	PluginDir  = ConfigDir + "/plugins"
	PluginFile = PluginDir + "/spov-goal.js"
)

// SandboxHome is what $HOME resolves to inside the sandbox, spelled out
// because the plugin compares against paths the agent sees already
// expanded.
//
// @decision(DL-036)
const SandboxHome = "/root"

// resolvedPluginFile is PluginFile as the agent sees it.
//
// @decision(DL-040)
const resolvedPluginFile = SandboxHome + "/.config/opencode/plugins/spov-goal.js"

// PluginEvidencePath is where the plugin records what it saw. A reply
// sent into the session starts no turn and is therefore not readable
// promptly; a file is.
//
// @decision(DL-039)
const PluginEvidencePath = "/tmp/spov-goal.jsonl"

// PluginOutcomePath holds a copy of the few lines that decide a job's
// outcome (the recorded pull requests, the completion, the check and the
// loop's stop), so they stay small however many permission lines the
// evidence file collects.
//
// @decision(DL-051) @decision(DL-050)
const PluginOutcomePath = "/tmp/spov-outcome.jsonl"

// PluginCommand is the slash command the plugin registers.
//
// @decision(DL-039)
const PluginCommand = "spov-goal"

// configPlaceholder is the token in the JavaScript source the rendered
// config replaces.
const configPlaceholder = "SPOV_GOAL_CONFIG"

//go:embed plugin/spov-goal.js
var pluginSource string

// JobKindPermissions is the allowlist for one kind of job: the actions
// its agent may take. Anything unmatched is denied.
//
// @decision(DL-040)
type JobKindPermissions struct {
	// Allow lists permitted action names, matched exactly.
	Allow []string
	// Deny lists substrings that refuse an action whatever Allow says.
	// It is a narrowing of the allowlist, never a policy on its own.
	Deny []string
	// WriteRoots allow an edit that Allow does not, when every path it
	// names resolves inside one of them. Absolute and clean.
	//
	// @decision(DL-040)
	WriteRoots []string
}

// pluginConfig is what the JavaScript reads. It is rendered into the
// source rather than written beside it: one file is one thing to place,
// and the plugin cannot then load with a config that failed to arrive.
type pluginConfig struct {
	Kind           string `json:"kind"`
	Command        string `json:"command"`
	CheckCommand   string `json:"checkCommand"`
	CheckTimeoutMS int    `json:"checkTimeoutMs"`
	CheckEvent     string `json:"checkEvent"`
	CompletionTool string `json:"completionTool"`
	// PullRequestTool is registered only when non-empty, which is a pr
	// job's plugin.
	PullRequestTool  string       `json:"pullRequestTool"`
	DeliverableEvent string       `json:"deliverableEvent"`
	Repos            []pluginRepo `json:"repos"`
	EvidencePath     string       `json:"evidencePath"`
	OutcomePath      string       `json:"outcomePath"`
	Dir              string       `json:"dir"`
	GoalText         string       `json:"goalText"`
	ContinueText     string       `json:"continueText"`
	Allow            []string     `json:"allow"`
	Deny             []string     `json:"deny"`
	WriteRoots       []string     `json:"writeRoots"`
	DenialNoteAt     int          `json:"denialNoteAt"`
	DenialStopAt     int          `json:"denialStopAt"`
	DenialNote       string       `json:"denialNote"`
	DeniedReason     string       `json:"deniedReason"`
	// TurnRetryDelays are in milliseconds.
	TurnRetryDelays  []int64 `json:"turnRetryDelays"`
	TurnFailedReason string  `json:"turnFailedReason"`
}

// pluginRepo is one selected repository as the pull request tool
// matches it: by slug, or by host and slug when two share a slug.
//
// @decision(DL-051)
type pluginRepo struct {
	Platform string `json:"platform"`
	Slug     string `json:"slug"`
	Host     string `json:"host"`
}

// Plugin describes the spov-goal plugin for one job.
//
// @decision(DL-039) @decision(DL-040)
type Plugin struct {
	// Kind is the job's kind, which decides the permission allowlist.
	Kind string
	// Goal is the canonical task text, re-asserted on every request.
	Goal string
	// Permissions is the allowlist. Left zero it is derived from Kind,
	// so the two cannot disagree; set explicitly it is used as given,
	// which is what a test needs. Either way a zero value denies
	// everything, which is the safe direction to fail in.
	Permissions JobKindPermissions
	// Workspace is the repositories cloned into the sandbox, used to
	// orient the agent on every request. Empty renders no preamble,
	// which is a test's plugin and must keep working.
	//
	// @decision(DL-039)
	Workspace []WorkspaceRepo
	// CheckCommand is the repo's own check, run once when the loop
	// stops so its exit status can be gate 1 of DL-050's verdict.
	// Empty leaves gate 1 unverified rather than failed.
	//
	// @decision(DL-050)
	CheckCommand string
	// Dir is the session's directory, and what the check command runs
	// in. Empty means WorkspaceRoot. Carried explicitly rather than
	// inherited from the OpenCode server's own cwd. Reports do NOT use
	// this — they are fixed at OutDir.
	//
	// @decision(DL-037)
	Dir string
}

// dir is the session's directory, defaulted.
//
// @decision(DL-037)
func (p Plugin) dir() string {
	if p.Dir == "" {
		return WorkspaceRoot
	}
	return p.Dir
}

// workspaceNote orients the agent: where it is, and what it has. It
// rides on the re-asserted goal rather than the opening turn, so it
// survives compaction (DL-039), and is the same text for every job
// kind. States facts, not policy: orientation, never containment.
//
// @decision(DL-039) @decision(DL-032)
func (p Plugin) workspaceNote() string {
	if len(p.Workspace) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nYou are working in a sandbox spov prepared. These repositories are " +
		"already cloned and their git credentials are configured:\n")
	for _, r := range p.Workspace {
		cli := string(r.CLI)
		if cli == "" {
			cli = "no CLI available"
		}
		fmt.Fprintf(&b, "\n- %s at %s (%s, use `%s`)",
			r.Slug, path.Join(WorkspaceRoot, r.Dir), r.Host, cli)
	}
	b.WriteString("\n\nWork in those directories. " + AgentsFile +
		" has the rest, including how to open a pull request.")
	return b.String()
}

// deliverableNote is the deliverable instruction for the job's kind.
//
// @decision(DL-051)
func (p Plugin) deliverableNote() string {
	switch p.Kind {
	case "report":
		return reportEpilogue()
	case "pr":
		// @decision(DL-051)
		if usesCLI(p.Workspace, CLIGlab) {
			return branchEpilogue + mergeRequestNote
		}
		return branchEpilogue
	default:
		return ""
	}
}

// policy is the allowlist this plugin runs under.
//
// @decision(DL-040)
func (p Plugin) policy() JobKindPermissions {
	if len(p.Permissions.Allow) == 0 && len(p.Permissions.Deny) == 0 && len(p.Permissions.WriteRoots) == 0 {
		return PermissionsForKind(p.Kind)
	}
	return p.Permissions
}

// goalPreamble prefixes the re-asserted goal so the model can tell the
// standing instruction from the turn's own text.
const goalPreamble = "Your standing goal for this session, which does not change:\n\n"

// OutDir is where a `report` job leaves its reports and the images they
// link, and the one directory delivery collects from. spov owns the
// convention — OpenCode has no notion of a job's deliverables — so spov is
// what has to state it. It is under the workspace root and never the
// session's directory, so a landing cannot move where deliverables are
// collected from.
//
// It is stated in the goal, which the plugin re-asserts on every request
// (DL-039), and the server reads this constant when it collects.
//
// @decision(DL-037) @decision(DL-051)
const OutDir = WorkspaceRoot + "/out"

// reportEpilogue tells a report job where to leave its reports.
//
// @decision(DL-037) @decision(DL-051) @decision(DL-063)
func reportEpilogue() string {
	return "\n\nWrite each report as its own Markdown file directly in " + OutDir +
		", named for its subject (for example " + OutDir + "/findings.md), with its title " +
		"as the first `# ` heading. Every .md file directly in that directory is " +
		"collected and delivered as one of this job's reports; work written anywhere " +
		"else is not collected. One report is fine when one is what the goal needs. " +
		"A report may show images: save PNG, JPEG, GIF or WebP files under " + OutDir +
		" (for example in " + OutDir + "/images/) and link them with a relative path " +
		"such as ![chart](images/chart.png). SVG and remote images are not shown. " +
		"The path is absolute on purpose — it is the same wherever you are working, " +
		"and is not relative to any repository." +
		reportWritesNote
}

// reportWritesNote says where a report job may write, in the goal and in
// the repeated-denial note.
//
// @decision(DL-040)
var reportWritesNote = " The repositories are read-only in a report job: your file " +
	"tools can write only under " + strings.Join(reportWriteRoots, " and ") +
	", the scratchpad being for working files."

// reportWriteRoots are where a report job's file tools may write: the
// reports' directory and the scratchpad.
//
// @decision(DL-040) @decision(DL-037)
var reportWriteRoots = []string{OutDir, ScratchDir}

// The repeated-denial guard: the same call denied DenialNoteAt times in
// a row, with no allow between, gets one note, and at DenialStopAt the
// run ends with DeniedReason.
//
// @decision(DL-040)
const (
	DenialNoteAt = 5
	DenialStopAt = 10
	DeniedReason = "denied"
)

// A failed turn is continued after each of TurnRetryDelays, and one
// more failure in a row ends the run with TurnFailedReason.
//
// @decision(DL-048)
var TurnRetryDelays = []time.Duration{10 * time.Second, 30 * time.Second}

// TurnFailedReason is the stop reason of a run ended by failed turns.
//
// @decision(DL-048)
const TurnFailedReason = "turn-failed"

// denialNote is what the note adds after naming the call.
//
// @decision(DL-040)
func denialNote(kind string) string {
	note := "The denial is this job's permission policy, and repeating the call will not change it. " +
		"Take a different approach."
	if kind == "report" {
		note += reportWritesNote + " The shell can write there too."
	}
	return note + " If the same call keeps being denied, the job will be ended."
}

// branchEpilogue tells a `pr` job to work on a branch. This is a
// guardrail on an honest run, not a boundary: an agent that ignores it
// can still push to the default branch, since DL-040's deny list is a
// substring match and cannot reliably block `git push`. The real
// control is branch protection on the forge, outside spov.
//
// @decision(DL-039) @decision(DL-040) @decision(DL-051)
const branchEpilogue = "\n\nWork on a NEW BRANCH and open a pull request from it. " +
	"Never commit or push to the default branch: create the branch before your first " +
	"commit, push only that branch, and let the pull request be the way your work is " +
	"reviewed. If the goal needs several pull requests, in one repository or across " +
	"the repositories above, use one branch for each. Right after opening each pull " +
	"request, call the " + PullRequestTool + " tool with its repository and branch: " +
	"a pull request you do not record is not part of this job's result. Change only " +
	"the repositories listed above."

// mergeRequestNote is added to branchEpilogue when a GitLab repository is
// selected, where a pull request is a merge request.
//
// @decision(DL-051)
const mergeRequestNote = " On GitLab a pull request is a merge request: open it with glab, then " +
	"record it with the " + PullRequestTool + " tool, as above."

// goalEpilogue tells the agent how to end the job. It rides on the goal
// rather than only on a continuation because the FIRST turn carries no
// continuation — a job whose goal is already met on turn one would
// otherwise have no way to say so until turn two.
//
// @decision(DL-048)
const goalEpilogue = "\n\nWhen this goal is met, call the " + CompletionTool +
	" tool. That is the only way to end the job: stating that you are done " +
	"in your reply does nothing and the run will continue regardless."

// LoopStoppedMarker is what the plugin says into the session when its
// loop stops continuing, the one hard completion signal a job has. A
// literal shared with spov-goal.js; the supervisor matches on it to
// move a job from running to delivering, so drift strands every job at
// `running`. TestTheStopMarkerMatchesThePlugin guards against it.
//
// @decision(DL-048) @decision(DL-039)
const LoopStoppedMarker = "SPOV_LOOP_STOPPED"

// CheckEvent is the evidence event the plugin writes when it has run
// the repo's check command, and CheckTimeout bounds that run. CheckEvent
// is a literal shared with spov-goal.js; TestTheCheckEventMatchesThePlugin
// guards against drift. The timeout is the plugin's own, not the
// sandbox's, so a slow suite can be distinguished from a killed one.
//
// @decision(DL-050) @decision(DL-039)
const (
	CheckEvent   = "check.finished"
	CheckTimeout = 20 * time.Minute
)

// CompletionTool is the tool the plugin registers for the agent to
// declare its goal met, and CompletionReason is the stop reason that
// declaration produces. The name is a literal shared with spov-goal.js;
// TestTheCompletionToolMatchesThePlugin guards against drift. It
// asserts no permission action (measured, spike S10) and must not be
// given one in PermissionsForKind. CompletionReason is also read by the
// supervisor to release a steered job (DL-048).
//
// @decision(DL-048)
const (
	CompletionTool   = "spov_complete"
	CompletionReason = "declared"
)

// PullRequestTool is the tool a pr job records each pull request with,
// and DeliverableEvent the evidence line it writes. Both are literals
// shared with spov-goal.js (TestThePullRequestToolMatchesThePlugin). The
// tool asserts no permission action and must not be given one in
// PermissionsForKind.
//
// @decision(DL-051)
const (
	PullRequestTool  = "spov_pull_request"
	DeliverableEvent = "deliverable.recorded"
)

// continueText is what a continuation says. It carries no goal of its
// own: the goal arrives through the context hook on the same request.
// It names the completion tool and must not invite the agent to
// announce completion in prose instead of calling it.
//
// @decision(DL-048)
const continueText = "Continue working towards your standing goal. " +
	"If it is already complete, call the " + CompletionTool + " tool to end this job. " +
	"Saying that you are finished has no effect — only the tool call ends the run."

// Source renders the plugin's JavaScript with this job's config baked
// in.
//
// @decision(DL-039)
func (p Plugin) Source() (string, error) {
	if strings.TrimSpace(p.Goal) == "" {
		return "", fmt.Errorf("opencode: plugin has no goal")
	}
	cfg := pluginConfig{
		Kind:             p.Kind,
		Command:          PluginCommand,
		CheckCommand:     p.CheckCommand,
		CheckTimeoutMS:   int(CheckTimeout / time.Millisecond),
		CheckEvent:       CheckEvent,
		CompletionTool:   CompletionTool,
		PullRequestTool:  p.pullRequestTool(),
		DeliverableEvent: DeliverableEvent,
		Repos:            p.repos(),
		EvidencePath:     PluginEvidencePath,
		OutcomePath:      PluginOutcomePath,
		Dir:              p.dir(),
		GoalText:         goalPreamble + p.Goal + p.workspaceNote() + p.deliverableNote() + goalEpilogue,
		ContinueText:     continueText,
		// Never null: the plugin iterates them, and a throw in the hook
		// leaves the incoming allow standing.
		Allow:        orEmpty(p.policy().Allow),
		Deny:         orEmpty(p.policy().Deny),
		WriteRoots:   orEmpty(p.policy().WriteRoots),
		DenialNoteAt: DenialNoteAt,
		DenialStopAt: DenialStopAt,
		DenialNote:   denialNote(p.Kind),
		DeniedReason: DeniedReason,
		// @decision(DL-048)
		TurnRetryDelays:  retryDelaysMs(),
		TurnFailedReason: TurnFailedReason,
	}
	// json.Marshal is enough to make this a safe JavaScript literal, and
	// that is not obvious: it escapes U+2028 and U+2029 — which JS reads
	// as line terminators where JSON does not — as well as < > and &,
	// so a goal cannot close the script or break the line it sits on.
	// Anything replacing it must escape the same five.
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("opencode: rendering plugin config: %w", err)
	}
	literal := string(encoded)
	if !strings.Contains(pluginSource, configPlaceholder) {
		return "", fmt.Errorf("opencode: plugin source has no %s placeholder", configPlaceholder)
	}
	return strings.Replace(pluginSource, configPlaceholder, literal, 1), nil
}

// pullRequestTool is the tool's name for a pr job, and empty otherwise,
// which leaves it unregistered.
//
// @decision(DL-051)
func (p Plugin) pullRequestTool() string {
	if p.Kind == "pr" {
		return PullRequestTool
	}
	return ""
}

// repos is the workspace as the pull request tool matches it, never null.
//
// @decision(DL-051)
func (p Plugin) repos() []pluginRepo {
	out := make([]pluginRepo, 0, len(p.Workspace))
	for _, r := range p.Workspace {
		out = append(out, pluginRepo{Platform: r.Platform, Slug: r.Slug, Host: r.Host})
	}
	return out
}

// orEmpty renders a nil list as [] rather than null.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// The permission actions OpenCode asserts, read out of the binary at
// ActionVocabularyVersion rather than guessed. Only tools that ask for
// permission appear: a tool that never asserts (todo, list, patch and
// the task/subagent driver itself) is not permissioned at all, so
// naming one in an allowlist protects nothing.
//
// ActionEdit is the single write gate — the write tool asserts it too,
// so there is no separate "write". ActionShell is the shell tool, and
// is emphatically not "bash".
//
// An MCP tool's action is computed as "<server>_<tool>" and so is not a
// closed set. That is the reason this is an allowlist: an action nobody
// listed is denied rather than admitted by a pattern.
//
// @decision(DL-040)
const (
	ActionShell             = "shell"
	ActionEdit              = "edit"
	ActionRead              = "read"
	ActionGlob              = "glob"
	ActionGrep              = "grep"
	ActionWebfetch          = "webfetch"
	ActionWebsearch         = "websearch"
	ActionSkill             = "skill"
	ActionSubagent          = "subagent"
	ActionQuestion          = "question"
	ActionExternalDirectory = "external_directory"
)

// ActionVocabularyVersion is the OpenCode build the action names above
// were read from. Not stable API: re-read them on a bump with
//
//	go test -tags integration ./internal/sandbox/ -run ActionVocabulary
//
// @decision(DL-040)
const ActionVocabularyVersion = "2.2.5"

// PermissionsForKind is the allowlist a job of this kind runs under.
//
// The incoming effect is "allow", so this is written as an allowlist and
// never as a list of things to refuse. An unknown kind gets the empty
// policy, which denies everything.
//
// @decision(DL-040)
func PermissionsForKind(kind string) JobKindPermissions {
	// Every job reads, searches and runs shell commands; none of them
	// may ask a question, since nobody is present to answer it and the
	// ask would park the run (DL-040).
	base := []string{
		ActionRead, ActionGlob, ActionGrep,
		ActionWebfetch, ActionWebsearch,
		ActionSkill, ActionSubagent,
		ActionShell,
		// The workspace is outside the agent's cwd, so reading a file
		// in a cloned repo asserts external_directory as well as read.
		// An interactive session gets this from DL-038's config grant;
		// a job session has no config, so it has to come from here.
		//
		// @decision(DL-040) @decision(DL-038)
		ActionExternalDirectory,
	}
	switch kind {
	case "pr":
		// A pr job changes files, so it gets the write gate.
		return JobKindPermissions{
			Allow: append(append([]string{}, base...), ActionEdit),
			Deny:  denyPatterns,
		}
	case "report":
		// A report job produces reports and changes nothing else,
		// so it does not get ActionEdit, the only action the file tools
		// assert. It may edit under its write roots.
		//
		// @decision(DL-040)
		return JobKindPermissions{
			Allow:      append([]string{}, base...),
			Deny:       denyPatterns,
			WriteRoots: append([]string{}, reportWriteRoots...),
		}
	default:
		return JobKindPermissions{}
	}
}

// denyPatterns narrow the allowlist away from the credentials the
// sandbox holds. A guardrail on the agent, not a boundary — containment
// is the sandbox itself. Every pattern is a path as the agent sees it,
// already expanded; a literal $HOME would never match anything.
//
// @decision(DL-040) @decision(DL-021)
var denyPatterns = []string{
	SandboxHome + "/.git-credentials",
	SandboxHome + "/.config/tea",
	// @decision(DL-021)
	GhConfigDir,
	// @decision(DL-032)
	GlabConfigDir,
	SandboxHome + "/.config/opencode/auth",
	SandboxHome + "/.local/share/opencode/auth",
	resolvedPluginFile,
}

// retryDelaysMs is TurnRetryDelays as the plugin reads them.
func retryDelaysMs() []int64 {
	out := make([]int64, len(TurnRetryDelays))
	for i, d := range TurnRetryDelays {
		out[i] = d.Milliseconds()
	}
	return out
}
