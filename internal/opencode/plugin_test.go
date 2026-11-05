package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

// testPlugin is a plugin with everything Source needs.
func testPlugin() Plugin {
	return Plugin{
		Kind:        "pr",
		Goal:        "Fix the flaky test in internal/cli.",
		Permissions: PermissionsForKind("pr"),
	}
}

// configFrom extracts the rendered config document back out of the
// JavaScript, so the tests assert on what the plugin will actually read
// rather than on the Go value that produced it.
func configFrom(t *testing.T, source string) pluginConfig {
	t.Helper()
	const marker = "const CONFIG = "
	i := strings.Index(source, marker)
	if i < 0 {
		t.Fatalf("source has no %q line:\n%s", marker, source)
	}
	rest := source[i+len(marker):]
	end := strings.IndexByte(rest, '\n')
	if end < 0 {
		t.Fatal("the config line is not terminated")
	}
	var cfg pluginConfig
	if err := json.Unmarshal([]byte(rest[:end]), &cfg); err != nil {
		t.Fatalf("the rendered config is not valid JSON: %v\n%s", err, rest[:end])
	}
	return cfg
}

func TestPluginSourceCarriesTheConfig(t *testing.T) {
	source, err := testPlugin().Source()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(source, configPlaceholder) {
		t.Errorf("the placeholder survived into the rendered source")
	}
	cfg := configFrom(t, source)
	if cfg.Kind != "pr" {
		t.Errorf("kind = %q, want pr", cfg.Kind)
	}
	if !strings.Contains(cfg.GoalText, "Fix the flaky test in internal/cli.") {
		t.Errorf("goal text does not carry the goal: %q", cfg.GoalText)
	}
	if cfg.Command != PluginCommand {
		t.Errorf("command = %q, want %q", cfg.Command, PluginCommand)
	}
	if cfg.EvidencePath != PluginEvidencePath {
		t.Errorf("evidencePath = %q, want %q", cfg.EvidencePath, PluginEvidencePath)
	}
	if cfg.ContinueText == "" {
		t.Error("continueText is empty, so a continuation would carry no text")
	}
}

// The continuation must go out as a synthetic. A prompt arrives
// indistinguishable from a human's and would move the job to steered on
// its own first turn.
func TestPluginContinuesWithSyntheticAndNeverPrompt(t *testing.T) {
	source, err := testPlugin().Source()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, "ctx.session.synthetic(") {
		t.Error("the plugin never calls ctx.session.synthetic")
	}
	if strings.Contains(source, "ctx.session.prompt(") {
		t.Error("the plugin calls ctx.session.prompt; a continuation must be a synthetic")
	}
	// The continuation is the one that resumes; a reply must not.
	if !strings.Contains(source, "resume: true") {
		t.Error("no continuation sets resume: true, so no turn would start")
	}
	if !strings.Contains(source, "resume: false") {
		t.Error("no reply sets resume: false, so a status would start a turn")
	}
}

// The plugin must not import the published package: its V2 context is
// narrower than the in-repo one and cannot drive a loop.
func TestPluginImportsNoOpenCodePackage(t *testing.T) {
	source, err := testPlugin().Source()
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"@opencode/plugin", "@opencode-ai/plugin"} {
		if strings.Contains(source, pkg) {
			t.Errorf("the plugin imports %s", pkg)
		}
	}
	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "import ") && !strings.Contains(trimmed, "await import(") {
			continue
		}
		if !strings.Contains(trimmed, "node:") {
			t.Errorf("the plugin imports something other than a node built-in: %q", trimmed)
		}
	}
}

func TestPluginRegistersTheFourPrimitives(t *testing.T) {
	source, err := testPlugin().Source()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ctx.session.hook(name, pushGoal)",
		`ctx.permission.hook("evaluate"`,
		"ctx.command.transform(",
		"ctx.event.subscribe()",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the plugin never calls %s", want)
		}
	}
}

func TestPluginSourceRefusesAnEmptyGoal(t *testing.T) {
	for _, goal := range []string{"", "   ", "\n\t "} {
		p := testPlugin()
		p.Goal = goal
		if _, err := p.Source(); err == nil {
			t.Errorf("Source() accepted the goal %q", goal)
		}
	}
}

// A goal is user text that reaches the sandbox inside a JavaScript
// source, so it must not be able to escape the literal it sits in.
//
// The property asserted is the one that matters at the destination: the
// config stays exactly one line of source, and the goal survives
// unchanged. That catches U+2028 and U+2029 — which JavaScript reads as
// line terminators where JSON does not, and which would otherwise split
// the config across lines — as well as a goal that closes the script.
func TestPluginSourceEscapesTheGoal(t *testing.T) {
	for _, goal := range []string{
		"line\u2028break",
		"para\u2029break",
		"</script><script>alert(1)</script>",
		`quote " and backslash \ and ampersand & and newline
here`,
		"SPOV_PLUGIN_EOF",
		"`backtick` and ${template}",
	} {
		p := testPlugin()
		p.Goal = goal
		source, err := p.Source()
		if err != nil {
			t.Fatalf("goal %q: %v", goal, err)
		}

		// The config must occupy exactly one line of JavaScript.
		//
		// Counted over the whole source rather than read off the config
		// line, because a helper that splits on terminators would stop
		// at the very character this is looking for and report nothing.
		if got := jsLines(source); got != jsLines(pluginSource) {
			t.Errorf("goal %q changes the source from %d JavaScript lines to %d; "+
				"a terminator escaped the config literal", goal, jsLines(pluginSource), got)
		}
		if strings.Contains(source, "<script>") {
			t.Errorf("goal %q leaves a literal <script> in the source", goal)
		}

		cfg := configFrom(t, source)
		if !strings.Contains(cfg.GoalText, goal) {
			t.Errorf("goal %q did not survive the round trip: %q", goal, cfg.GoalText)
		}
	}
}

// jsLines counts the lines JavaScript sees, which is not the same as
// the lines Go sees: U+2028 and U+2029 terminate a line for JS and not
// for strings.Split.
func jsLines(source string) int {
	n := 1
	for _, r := range source {
		if r == '\n' || r == '\u2028' || r == '\u2029' {
			n++
		}
	}
	return n
}

// The incoming effect is "allow", so the policy has to be an allowlist.
// A kind nobody recognises must therefore deny everything rather than
// inherit the permissive baseline.
func TestPermissionsForAnUnknownKindDenyEverything(t *testing.T) {
	for _, kind := range []string{"", "unknown", "PR", "pr ", "deploy"} {
		p := PermissionsForKind(kind)
		if len(p.Allow) != 0 {
			t.Errorf("kind %q allows %v; an unrecognised kind must permit nothing", kind, p.Allow)
		}
	}
}

func TestPermissionsPerKind(t *testing.T) {
	pr := PermissionsForKind("pr")
	report := PermissionsForKind("report")

	// ActionEdit is the single write gate: the write tool asserts it
	// too, so a job holding it can change files and a job without it
	// cannot, whichever file tool the agent reaches for.
	if !contains(pr.Allow, ActionEdit) {
		t.Errorf("a pr job cannot edit: %v", pr.Allow)
	}
	if contains(report.Allow, ActionEdit) {
		t.Errorf("a report job holds the write gate, but it produces no changes: %v", report.Allow)
	}

	// Both kinds work: reading and running commands are not optional.
	for _, kind := range []struct {
		name string
		p    JobKindPermissions
	}{{"pr", pr}, {"report", report}} {
		for _, need := range []string{ActionRead, ActionShell, ActionGlob, ActionGrep} {
			if !contains(kind.p.Allow, need) {
				t.Errorf("a %s job cannot %q: %v", kind.name, need, kind.p.Allow)
			}
		}
		// Nobody is present to answer, and an ask parks the run.
		if contains(kind.p.Allow, ActionQuestion) {
			t.Errorf("a %s job may ask a question, which would park it with nobody to answer", kind.name)
		}
		if len(kind.p.Deny) == 0 {
			t.Errorf("a %s job has no deny patterns, so the credentials are unguarded", kind.name)
		}
	}
}

// Every name in an allowlist must be an action OpenCode actually
// asserts. A name that is not — "bash" for the shell tool, or "write"
// for a tool that asserts "edit" — silently grants nothing, and a job
// under it denies every call while still finishing.
func TestAllowlistNamesAreRealActions(t *testing.T) {
	known := map[string]bool{
		ActionShell: true, ActionEdit: true, ActionRead: true,
		ActionGlob: true, ActionGrep: true, ActionWebfetch: true,
		ActionWebsearch: true, ActionSkill: true, ActionSubagent: true,
		ActionQuestion: true, ActionExternalDirectory: true,
	}
	for _, kind := range []string{"pr", "report"} {
		for _, name := range PermissionsForKind(kind).Allow {
			if !known[name] {
				t.Errorf("kind %q allows %q, which OpenCode %s never asserts; it grants nothing",
					kind, name, ActionVocabularyVersion)
			}
		}
	}
}

// The deny patterns are compared, inside the sandbox, against paths the
// agent already sees expanded. A pattern carrying a literal $HOME can
// never match, so this asserts what each pattern *matches* rather than
// that it is present — a presence check passes on a pattern that
// protects nothing.
func TestDenyPatternsMatchTheRuntimePaths(t *testing.T) {
	for _, path := range []string{
		"/root/.git-credentials",
		"/root/.config/tea/config.yml",
		"/root/.config/opencode/auth.json",
		"/root/.local/share/opencode/auth.json",
		"/root/.config/opencode/plugins/spov-goal.js",
	} {
		if !matchedByDeny(path) {
			t.Errorf("no deny pattern matches %q, so it is not protected: %v", path, denyPatterns)
		}
	}
	// A pattern that still needs a shell to resolve it protects nothing.
	for _, p := range denyPatterns {
		if strings.Contains(p, "$") {
			t.Errorf("deny pattern %q is unexpanded and can never match a runtime path", p)
		}
		if !strings.HasPrefix(p, "/") {
			t.Errorf("deny pattern %q is not an absolute path", p)
		}
	}
}

// matchedByDeny mirrors what the JavaScript does: a substring test of
// each pattern against the subject.
func matchedByDeny(subject string) bool {
	for _, p := range denyPatterns {
		if strings.Contains(subject, p) {
			return true
		}
	}
	return false
}

// The plugin's own file is on the deny list, so the agent cannot edit
// away the policy that binds it.
func TestTheAgentCannotEditItsOwnGuardrail(t *testing.T) {
	if !matchedByDeny(resolvedPluginFile) {
		t.Errorf("the plugin file %q is not denied; the agent can edit its own policy", resolvedPluginFile)
	}
	// The constant the entrypoint writes with and the one the plugin
	// matches on must stay the same file.
	if want := strings.Replace(PluginFile, "$HOME", SandboxHome, 1); want != resolvedPluginFile {
		t.Errorf("resolvedPluginFile is %q but PluginFile expands to %q", resolvedPluginFile, want)
	}
}

// The permission decision lives in JavaScript, so what Go can assert is
// that the source still has the properties the decision rests on. These
// are the two that would fail silently if they were lost.
func TestPermissionDecisionScansTheUntruncatedSubject(t *testing.T) {
	source, err := testPlugin().Source()
	if err != nil {
		t.Fatal(err)
	}
	decideBody := functionBody(t, source, "function decide(evaluation) {")

	// A truncated subject lets a long command reach a denied path in
	// its tail and still be allowed by its action.
	if strings.Contains(decideBody, "describe(") {
		t.Error("decide() scans a truncated subject; a long command could carry a denied path past the cut-off")
	}
	if !strings.Contains(decideBody, "JSON.stringify(evaluation?.resources") {
		t.Error("decide() does not scan the whole resources value")
	}
	// An unreadable subject is not evidence that it is safe.
	if !strings.Contains(decideBody, `return "deny"`) {
		t.Error("decide() has no deny path")
	}
	// One allow for the allowlist and one for DL-040's path grant.
	if strings.Count(decideBody, `return "allow"`) != 2 || !strings.Contains(decideBody, `&& writesInsideRoots(`) {
		t.Errorf("decide() has %d allow paths, want the allowlist's and the write roots'", strings.Count(decideBody, `return "allow"`))
	}
}

// functionBody returns the source of one brace-balanced function.
func functionBody(t *testing.T, source, header string) string {
	t.Helper()
	i := strings.Index(source, header)
	if i < 0 {
		t.Fatalf("source has no %q", header)
	}
	rest := source[i+len(header)-1:]
	depth := 0
	for j, r := range rest {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:j+1]
			}
		}
	}
	t.Fatalf("%q is not brace-balanced", header)
	return ""
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// The placeholder guard exists so a plugin source that lost its config
// token fails loudly rather than shipping a plugin that reads an
// undefined CONFIG and dies inside the sandbox.
func TestPluginSourceRefusesASourceWithoutThePlaceholder(t *testing.T) {
	original := pluginSource
	t.Cleanup(func() { pluginSource = original })

	pluginSource = "export default { id: \"spov.goal\" }\n"
	if _, err := testPlugin().Source(); err == nil {
		t.Error("Source() accepted a plugin source carrying no config placeholder")
	}
}

// The shipped source must carry exactly one placeholder: a second one
// would be left unreplaced, since only the first is substituted.
func TestShippedPluginSourceHasExactlyOnePlaceholder(t *testing.T) {
	if n := strings.Count(pluginSource, configPlaceholder); n != 1 {
		t.Errorf("the plugin source has %d occurrences of %s, want exactly 1", n, configPlaceholder)
	}
}

// Kind and Permissions are two fields that could disagree. A plugin that
// leaves Permissions zero derives it from Kind, so the common case
// cannot produce a pr job running under a report policy.
func TestPluginDerivesItsPolicyFromKind(t *testing.T) {
	p := Plugin{Kind: "pr", Goal: "g"}
	source, err := p.Source()
	if err != nil {
		t.Fatal(err)
	}
	cfg := configFrom(t, source)
	if !contains(cfg.Allow, "edit") {
		t.Errorf("a pr job with no explicit policy cannot edit: %v", cfg.Allow)
	}

	// An explicit policy is still honoured.
	p.Permissions = JobKindPermissions{Allow: []string{"read"}}
	source, err = p.Source()
	if err != nil {
		t.Fatal(err)
	}
	if cfg := configFrom(t, source); len(cfg.Allow) != 1 || cfg.Allow[0] != "read" {
		t.Errorf("an explicit policy was overridden: %v", cfg.Allow)
	}
}

// An unknown kind with no explicit policy must still deny everything
// rather than inheriting the permissive baseline.
func TestPluginWithAnUnknownKindAllowsNothing(t *testing.T) {
	p := Plugin{Kind: "deploy", Goal: "g"}
	source, err := p.Source()
	if err != nil {
		t.Fatal(err)
	}
	if cfg := configFrom(t, source); len(cfg.Allow) != 0 {
		t.Errorf("an unknown kind allows %v", cfg.Allow)
	}
}

// The allowlist is compared case-folded inside the sandbox, so the names
// must be lowercase or they can never match.
func TestAllowlistNamesAreLowercase(t *testing.T) {
	for _, kind := range []string{"pr", "report"} {
		for _, name := range PermissionsForKind(kind).Allow {
			if name != strings.ToLower(name) {
				t.Errorf("kind %q allows %q, which is not lowercase and cannot match a case-folded action", kind, name)
			}
		}
	}
}

// TestTheLoopAnnouncesEveryWayItStops pins that the loop announces its
// own end on every path that stops it, and never on pause.
//
// @decision(DL-048)
func TestTheLoopAnnouncesEveryWayItStops(t *testing.T) {
	source, err := testPlugin().Source()
	if err != nil {
		t.Fatal(err)
	}
	// Each reason the loop stops continuing must reach the server.
	for _, reason := range []string{"continue-failed"} {
		if !strings.Contains(source, `announceStop(ctx, sessionID, "`+reason+`")`) {
			t.Errorf("the loop does not announce %q; a job stopped that way would sit at running forever", reason)
		}
	}
	// `stop` is final and announces; `pause` is not and must not.
	// DL-039: a pause does not move the job row, and a human who paused
	// to look at something must not find the job concluded.
	if !strings.Contains(source, "if (loop.stopped) {") {
		t.Error("the loop announces on pause as well as stop; a paused job would be concluded under the human who paused it")
	}
	if !strings.Contains(source, "announceStop(ctx, sessionID, reason)") {
		t.Error("the loop does not announce an explicit stop")
	}
	if !strings.Contains(source, "const STOP_MARKER = \""+LoopStoppedMarker+"\"") {
		t.Errorf("the plugin does not declare STOP_MARKER as %q", LoopStoppedMarker)
	}
}

// The announcement must start no turn, or it would trigger the very
// continuation it is reporting the end of.
//
// @decision(DL-048)
func TestTheStopAnnouncementStartsNoTurn(t *testing.T) {
	source, err := testPlugin().Source()
	if err != nil {
		t.Fatal(err)
	}
	// announceStop goes out through reply, which is the resume: false
	// path; the only resume: true call is the continuation itself.
	if !strings.Contains(source, "await reply(ctx, sessionID, line)") {
		t.Error("the announcement does not travel as a reply, so it may start a turn")
	}
	if strings.Count(source, "resume: true") != 1 {
		t.Errorf("resume: true appears %d times; only the continuation may start a turn",
			strings.Count(source, "resume: true"))
	}
}

// It is sent at most once. Turns can still arrive after the loop has
// stopped — a human may be typing into the session — and a marker per
// turn would have the supervisor re-deliver an already-delivered job.
//
// @decision(DL-048)
func TestTheStopIsAnnouncedOnlyOnce(t *testing.T) {
	source, err := testPlugin().Source()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, "if (loop.announced) return") {
		t.Error("announceStop has no guard, so it would fire on every turn after the loop stopped")
	}
	if !strings.Contains(source, "loop.announced = true") {
		t.Error("announceStop never sets its guard")
	}
}

// TestEveryJobKindCanReadItsOwnWorkspace pins that a job's allowlist
// grants external_directory beside read, since the workspace sits
// outside the agent's cwd.
//
// @decision(DL-040) @decision(DL-038)
func TestEveryJobKindCanReadItsOwnWorkspace(t *testing.T) {
	for _, kind := range []string{"pr", "report"} {
		allowed := map[string]bool{}
		for _, a := range PermissionsForKind(kind).Allow {
			allowed[a] = true
		}
		for _, need := range []string{ActionRead, ActionExternalDirectory} {
			if !allowed[need] {
				t.Errorf("kind %q does not allow %q, so the read tool is refused on every "+
					"file in the checkout and the job can only reach its repo through shell",
					kind, need)
			}
		}
	}
	// The grant must stay scoped: the credential paths are refused
	// under this action exactly as under read, and that comes from the
	// deny list rather than from withholding the action.
	for _, path := range []string{
		SandboxHome + "/.git-credentials",
		SandboxHome + "/.config/opencode/auth",
	} {
		if !matchedByDeny(path) {
			t.Errorf("granting external_directory widened the policy: %q is no longer refused", path)
		}
	}
}
