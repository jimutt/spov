package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/routedcred"
)

func TestEntrypointScript(t *testing.T) {
	e := Entrypoint{
		Clones: []Clone{
			{URL: "https://codefloe.com/acme/widgets.git", Dir: "widgets"},
			{URL: "https://github.com/acme/api.git", Dir: "api"},
		},
		Landing: "widgets",
		Port:    DefaultPort,
	}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}

	// Asserted by content rather than by line count: clones run
	// concurrently (DL-036), so the number of lines around them is an
	// implementation detail while each of these is a contract.
	for _, want := range []string{
		"set -e\n",
		"git clone --depth 1 -- 'https://codefloe.com/acme/widgets.git' '/workspace/widgets'",
		"git clone --depth 1 -- 'https://github.com/acme/api.git' '/workspace/api'",
		"cd '/workspace/widgets'\n",
		"exec /root/.opencode/bin/opencode serve --hostname 0.0.0.0 --port 4096\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not contain %q:\n%s", want, script)
		}
	}
	if !strings.HasPrefix(script, "set -e\n") {
		t.Errorf("script does not start with `set -e`:\n%s", script)
	}
}

// TestEntrypointOrdering pins the ordering that matters: clones happen
// before the server starts.
func TestEntrypointOrdering(t *testing.T) {
	e := Entrypoint{
		Clones:  []Clone{{URL: "https://example.com/r.git", Dir: "r"}},
		Landing: "r",
		Port:    DefaultPort,
	}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	cloneAt := strings.Index(script, "git clone")
	execAt := strings.Index(script, "exec ")
	if cloneAt < 0 || execAt < 0 {
		t.Fatalf("script is missing a step:\n%s", script)
	}
	if cloneAt >= execAt {
		t.Errorf("steps out of order (clone=%d exec=%d):\n%s", cloneAt, execAt, script)
	}
	if !strings.HasPrefix(script, "set -e") {
		t.Error("script does not abort on the first failure")
	}
}

// TestEntrypointRunsTheBinaryInPlace: the server runs from where the image
// installs it, and nothing copies it first.
//
// @decision(DL-032)
func TestEntrypointRunsTheBinaryInPlace(t *testing.T) {
	script, err := Entrypoint{Port: DefaultPort}.Script()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "exec "+imageBinary+" serve") {
		t.Errorf("exec does not run the image's binary:\n%s", script)
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "cp ") && strings.Contains(line, imageBinary) {
			t.Errorf("the entrypoint still copies the binary: %q", line)
		}
	}
}

func TestEntrypointCommandIsNotALoginShell(t *testing.T) {
	cmd, err := Entrypoint{Port: DefaultPort}.Command()
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd) != 3 || cmd[0] != "bash" {
		t.Fatalf("command = %v", cmd)
	}
	// `bash -lc` sources /etc/profile, which resets PATH and loses the
	// environment the sandbox was created with.
	if cmd[1] != "-c" {
		t.Errorf("command flag = %q, want -c", cmd[1])
	}
}

func TestEntrypointWorkingDir(t *testing.T) {
	tests := []struct {
		name    string
		landing string
		want    string
	}{
		{"no landing repo", "", "/workspace"},
		{"named repo", "spov", "/workspace/spov"},
		{"nested", "group/repo", "/workspace/group/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Entrypoint{Landing: tt.landing, Port: DefaultPort}
			if tt.landing != "" {
				e.Clones = []Clone{{URL: "https://example.com/r.git", Dir: tt.landing}}
			}
			if got := e.WorkingDir(); got != tt.want {
				t.Errorf("WorkingDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEntrypointQuotesCloneURLs matters because a clone URL can embed a
// short-lived credential and repo names come from a remote API.
func TestEntrypointQuotesCloneURLs(t *testing.T) {
	e := Entrypoint{
		Clones: []Clone{{
			URL: "https://x-token:secret@codefloe.com/o/r.git; rm -rf /",
			Dir: "r",
		}},
		Landing: "r",
		Port:    DefaultPort,
	}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "git clone") {
			if !strings.Contains(line, `'https://x-token:secret@codefloe.com/o/r.git; rm -rf /'`) {
				t.Errorf("clone URL is not quoted as one word: %q", line)
			}
		}
	}
}

func TestEntrypointQuotesEmbeddedQuotes(t *testing.T) {
	e := Entrypoint{
		Clones:  []Clone{{URL: "https://example.com/it's.git", Dir: "r"}},
		Landing: "r",
		Port:    DefaultPort,
	}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, `'https://example.com/it'\''s.git'`) {
		t.Errorf("a single quote in the URL was not escaped:\n%s", script)
	}
}

func TestEntrypointValidate(t *testing.T) {
	tests := []struct {
		name string
		in   Entrypoint
		want string
	}{
		{"port zero", Entrypoint{}, "port"},
		{"port too high", Entrypoint{Port: 70000}, "port"},
		{"clone without a URL", Entrypoint{Port: 1, Clones: []Clone{{Dir: "r"}}}, "no URL"},
		{"clone without a dir", Entrypoint{Port: 1, Clones: []Clone{{URL: "u"}}}, "empty"},
		{"absolute clone dir", Entrypoint{Port: 1, Clones: []Clone{{URL: "u", Dir: "/etc"}}}, "relative"},
		{"traversing clone dir", Entrypoint{Port: 1, Clones: []Clone{{URL: "u", Dir: "../x"}}}, "traverse"},
		{"unclean clone dir", Entrypoint{Port: 1, Clones: []Clone{{URL: "u", Dir: "./x"}}}, "clean"},
		{"duplicate clone dirs", Entrypoint{Port: 1, Clones: []Clone{
			{URL: "a", Dir: "r"}, {URL: "b", Dir: "r"},
		}}, "both target"},
		{"landing that was never cloned", Entrypoint{Port: 1,
			Clones:  []Clone{{URL: "a", Dir: "r"}},
			Landing: "other",
		}, "not one of the cloned"},
		{"traversing landing", Entrypoint{Port: 1, Landing: "../etc"}, "traverse"},
		{"landing with no clones at all", Entrypoint{Port: 1, Landing: "somewhere"}, "not one of the cloned"},
		{"clone URL read as a git option", Entrypoint{Port: 1, Clones: []Clone{
			{URL: "--upload-pack=touch /tmp/pwn", Dir: "r"},
		}}, "git option"},
		{"clone dir read as a git option", Entrypoint{Port: 1, Clones: []Clone{
			{URL: "https://example.com/r.git", Dir: "-x"},
		}}, "git option"},
		{"ext transport", Entrypoint{Port: 1, Clones: []Clone{
			{URL: "ext::sh -c whoami", Dir: "r"},
		}}, "not allowed"},
		{"file transport", Entrypoint{Port: 1, Clones: []Clone{
			{URL: "file:///etc", Dir: "r"},
		}}, "not allowed"},
		{"newline in the URL", Entrypoint{Port: 1, Clones: []Clone{
			{URL: "https://example.com/r.git\nrm -rf /", Dir: "r"},
		}}, "newline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.in.Validate()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			// Script must refuse to render an invalid entrypoint.
			if _, err := tt.in.Script(); err == nil {
				t.Error("Script rendered an invalid entrypoint")
			}
		})
	}
}

func TestEntrypointValidateAccepts(t *testing.T) {
	valid := []Entrypoint{
		{Port: DefaultPort},
		{Port: DefaultPort, Clones: []Clone{{URL: "ssh://git@codefloe.com/o/r.git", Dir: "r"}}, Landing: "r"},
		{Port: DefaultPort, Clones: []Clone{{URL: "u", Dir: "r"}}, Landing: "r"},
		{Port: 8080, Clones: []Clone{{URL: "u", Dir: "a/b"}}, Landing: "a/b"},
	}
	for i, e := range valid {
		if err := e.Validate(); err != nil {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestEntrypointCommandRefusesInvalidEntrypoint(t *testing.T) {
	// Command must not hand a half-rendered script to the sandbox.
	if _, err := (Entrypoint{Port: 0}).Command(); err == nil {
		t.Fatal("Command rendered an invalid entrypoint")
	}
}

// TestEntrypointNeverWritesATokenIntoTheScript: any process inside the
// sandbox can read another's command line, and the script is the
// container's argv.
//
// @decision(DL-032) @decision(DL-036)
func TestEntrypointNeverWritesATokenIntoTheScript(t *testing.T) {
	const secret = "gto-do-not-appear-in-argv"
	e := Entrypoint{
		Clones:  []Clone{{URL: "https://example.com/o/r.git", Dir: "r"}},
		Landing: "r",
		Port:    DefaultPort,
		Logins: []CLILogin{
			{CLI: CLITea, Name: "codefloe", URL: "https://codefloe.com", User: "alice", TokenEnv: "SPOV_GIT_TOKEN_CODEFLOE"},
			{CLI: CLIGh, Name: "gh", URL: "https://github.com", User: "alice", TokenEnv: "SPOV_GIT_TOKEN_GH"},
		},
	}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, secret) {
		t.Fatal("a token reached the script")
	}
	// The tokens must be referenced, not embedded.
	for _, want := range []string{`"$SPOV_GIT_TOKEN_CODEFLOE"`, `"$SPOV_GIT_TOKEN_GH"`} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not reference %s:\n%s", want, script)
		}
	}
	if !strings.Contains(script, "chmod 600 /root/.config/tea/config.yml") {
		t.Error("tea's config, which holds the token, is not restricted")
	}
	if !strings.Contains(script, GhConfigDir+"/hosts.d") {
		t.Error("gh is not authenticated")
	}
}

// TestEntrypointWritesTheWorkspaceGuide checks the generated AGENTS.md is
// emitted through a quoted heredoc, so its content cannot be expanded or
// interpreted by the shell.
//
// @decision(DL-032)
func TestEntrypointWritesTheWorkspaceGuide(t *testing.T) {
	e := Entrypoint{
		Clones:  []Clone{{URL: "https://example.com/o/r.git", Dir: "r"}},
		Landing: "r",
		Port:    DefaultPort,
		Workspace: []WorkspaceRepo{
			{Slug: "o/r", Dir: "r", Host: "example.com", CLI: CLITea},
		},
	}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "cat > "+AgentsFile+" <<'SPOV_AGENTS_EOF'") {
		t.Errorf("the guide is not written through a quoted heredoc:\n%s", script)
	}
	if !strings.Contains(script, "SPOV_AGENTS_EOF\n") {
		t.Error("the heredoc is not terminated")
	}
	// It must land before the server starts, or the agent's first look
	// misses it.
	guideAt := strings.Index(script, "SPOV_AGENTS_EOF")
	execAt := strings.Index(script, "exec ")
	if guideAt < 0 || execAt < 0 || guideAt > execAt {
		t.Error("the guide is written after the server starts")
	}
}

func TestEntrypointWithoutWorkspaceOrLogins(t *testing.T) {
	script, err := Entrypoint{Port: DefaultPort}.Script()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "AGENTS.md") {
		t.Error("a guide was written with no repositories")
	}
	if strings.Contains(script, "tea login") || strings.Contains(script, GhConfigDir) {
		t.Error("a CLI was configured with no logins")
	}
}

// jobEntrypoint is an entrypoint for an autonomous job's session.
func jobEntrypoint() Entrypoint {
	return Entrypoint{
		Clones:  []Clone{{URL: "https://example.com/r.git", Dir: "r"}},
		Landing: "r",
		Port:    DefaultPort,
		Job: &Plugin{
			Kind:        "pr",
			Goal:        "Fix the flaky test.",
			Permissions: PermissionsForKind("pr"),
		},
	}
}

func TestEntrypointWritesThePluginForAJob(t *testing.T) {
	script, err := jobEntrypoint().Script()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`mkdir -p "$HOME/.config/opencode/plugins"`,
		`cat > "$HOME/.config/opencode/plugins/spov-goal.js" <<'SPOV_PLUGIN_EOF'`,
		"SPOV_PLUGIN_EOF\n",
		`id: "spov.goal"`,
		"Fix the flaky test.",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not contain %q", want)
		}
	}
	// The plugin has to be in place before the server starts, or the
	// first turn runs with no policy and no loop.
	pluginAt := strings.Index(script, "spov-goal.js")
	execAt := strings.Index(script, "exec ")
	if pluginAt < 0 || execAt < 0 || pluginAt >= execAt {
		t.Errorf("the plugin is not written before the server starts (plugin %d, exec %d)", pluginAt, execAt)
	}
}

// A job's pull request policy reaches both the plugin's goal and the
// generated guide the entrypoint writes.
//
// @decision(DL-137)
func TestTheEntrypointCarriesThePullRequestPolicyToTheGoalAndTheGuide(t *testing.T) {
	e := jobEntrypoint()
	e.Workspace = []WorkspaceRepo{{Slug: "acme/r", Dir: "r", Platform: "gh", Host: "github.com", CLI: CLIGh}}
	e.Job.Workspace = e.Workspace
	e.Job.PRPolicy = PRPolicyGoal
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## This job's pull request", "The goal says which branch to work on"} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not contain %q", want)
		}
	}
	if strings.Contains(script, "gh pr create") || strings.Contains(script, "NEW BRANCH") {
		t.Error("the script still carries spov's own pull request instructions")
	}
}

func TestEntrypointWritesNoPluginForAnInteractiveSession(t *testing.T) {
	e := jobEntrypoint()
	e.Job = nil
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "spov-goal.js") {
		t.Error("an interactive session got the job plugin")
	}
}

// TestJobSessionLosesTheExternalDirectoryGrant checks a job's config
// carries no permission keys while an interactive config keeps DL-038's
// grant. It parses the config document, because the plugin's allowlist
// also names external_directory and a substring search cannot tell the
// two apart.
//
// @decision(DL-038)
func TestJobSessionLosesTheExternalDirectoryGrant(t *testing.T) {
	job, err := jobEntrypoint().Script()
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(configDocumentIn(t, job)), &cfg); err != nil {
		t.Fatalf("a job's config is not valid JSON: %v", err)
	}
	// All three normalise into the same concatenated rule list, so all
	// three are the exclusion.
	for _, key := range []string{"permission", "permissions", "tools"} {
		if _, ok := cfg[key]; ok {
			t.Errorf("a job's config carries %q: a deny there short-circuits "+
				"permission.evaluate without consulting the plugin's policy", key)
		}
	}

	e := jobEntrypoint()
	e.Job = nil
	interactive, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	// Read as a document for the same reason the job half is: a
	// substring search for the action name would pass on text from
	// anywhere in the script.
	grants := externalDirectoryGrants(t, []byte(configDocumentIn(t, interactive)))
	for _, pattern := range workspaceGlobs {
		if grants[pattern] != "allow" {
			t.Errorf("an interactive session lost the external_directory grant for %q: %v", pattern, grants)
		}
	}
}

// TestBothKindsOfSessionCarryTheModel: the config is the model's one
// hop into the sandbox, and it is written at a single site for both.
//
// @decision(DL-014) @decision(DL-038)
func TestBothKindsOfSessionCarryTheModel(t *testing.T) {
	const model = "anthropic/claude-sonnet-5"

	job := jobEntrypoint()
	job.Model = model
	interactive := jobEntrypoint()
	interactive.Job = nil
	interactive.Model = model

	for name, e := range map[string]Entrypoint{"job": job, "interactive": interactive} {
		t.Run(name, func(t *testing.T) {
			script, err := e.Script()
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(configDocumentIn(t, script)), &cfg); err != nil {
				t.Fatalf("the config is not valid JSON: %v", err)
			}
			if cfg.Model != model {
				t.Errorf("model = %q, want %q", cfg.Model, model)
			}
		})
	}
}

// TestNoModelLeavesTheKeyOut: an empty model means OpenCode's default, so
// the key must be absent; an empty string is a model name OpenCode would
// fail to resolve.
//
// @decision(DL-014)
func TestNoModelLeavesTheKeyOut(t *testing.T) {
	for _, out := range []string{SandboxConfigJSON("", false), JobConfigJSON("", false)} {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatal(err)
		}
		if _, ok := cfg["model"]; ok {
			t.Errorf("an unset model still wrote the key: %s", out)
		}
	}
}

// TestAModelCannotEscapeTheConfigHeredoc: the model is the one
// caller-supplied value in the config's quoted heredoc, so a model
// carrying the terminator must not end it early. The test runs the script,
// since only that proves json.Marshal's newline escaping holds.
//
// @decision(DL-038)
func TestAModelCannotEscapeTheConfigHeredoc(t *testing.T) {
	home := t.TempDir()
	pwned := filepath.Join(home, "pwned")
	hostile := "p/m\nSPOV_CONFIG_EOF\ntouch " + pwned + "\n"

	runConfigScript(t, Entrypoint{Port: 4096, Model: hostile}, home)

	if _, err := os.Stat(pwned); err == nil {
		t.Error("the model closed the heredoc and its command ran")
	}

	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("no config was written: %v", err)
	}
	var cfg struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("the written config is not valid JSON: %v\n%s", err, data)
	}
	if cfg.Model != hostile {
		t.Errorf("model = %q, want it to round-trip intact", cfg.Model)
	}
}

// configDocumentIn extracts the config heredoc's body from a generated
// script.
func configDocumentIn(t *testing.T, script string) string {
	t.Helper()
	open := "cat > \"" + ConfigFile + "\" <<'SPOV_CONFIG_EOF'\n"
	start := strings.Index(script, open)
	if start < 0 {
		t.Fatalf("the script writes no config document:\n%s", script)
	}
	rest := script[start+len(open):]
	end := strings.Index(rest, "\nSPOV_CONFIG_EOF\n")
	if end < 0 {
		t.Fatalf("the config heredoc is never terminated:\n%s", rest)
	}
	return rest[:end]
}

// The plugin travels through a quoted heredoc, so nothing in it is
// expanded by the shell. A goal that contains the terminator would end
// the heredoc early and leave the rest of the JavaScript as commands.
func TestEntrypointRefusesAPluginContainingTheHeredocTerminator(t *testing.T) {
	e := jobEntrypoint()
	e.Job.Goal = "do the thing\nSPOV_PLUGIN_EOF\nrm -rf /"
	if _, err := e.Script(); err == nil {
		t.Error("Script() accepted a plugin carrying the heredoc terminator")
	}
}

func TestEntrypointRefusesAnInvalidPlugin(t *testing.T) {
	e := jobEntrypoint()
	e.Job.Goal = ""
	if _, err := e.Script(); err == nil {
		t.Error("Script() accepted a plugin with no goal")
	}
}

// A goal is user text and must not be able to break out of the heredoc
// into the entrypoint's own shell.
func TestEntrypointQuotesAGoalWithShellMetacharacters(t *testing.T) {
	e := jobEntrypoint()
	e.Job.Goal = "fix $(whoami) and `id` and $HOME and 'quotes'"
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	// The heredoc is quoted, so the metacharacters travel literally.
	if !strings.Contains(script, "<<'SPOV_PLUGIN_EOF'") {
		t.Error("the plugin heredoc is not quoted, so the goal would be expanded")
	}
	if !strings.Contains(script, `$(whoami)`) {
		t.Error("the goal did not reach the script")
	}
}

// With the proxy sidecar, the entrypoint installs its CA and waits for it
// before anything that goes out through it, and exits with a code of its
// own if it never answers.
//
// @decision(DL-034)
func TestAProxiedEntrypointWaitsForTheProxyFirst(t *testing.T) {
	e := Entrypoint{
		Clones:  []Clone{{URL: "https://github.com/o/r.git", Dir: "r"}},
		Port:    DefaultPort,
		Logins:  []CLILogin{{CLI: CLIGh, Name: "gh", URL: "https://github.com", User: "x-access-token", TokenEnv: "SPOV_GIT_TOKEN_GH"}},
		Proxied: true,
	}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	wait := strings.Index(script, "/dev/tcp/proxy/3128")
	for _, later := range []string{"credential.helper", "git clone", "serve --hostname"} {
		if i := strings.Index(script, later); wait < 0 || i < wait {
			t.Errorf("%q comes before the proxy wait (%d < %d)", later, i, wait)
		}
	}
	for _, want := range []string{`"$SPOV_PROXY_CA" > '/usr/local/share/ca-certificates/spov-proxy.crt'`, ">> '/etc/ssl/certs/ca-certificates.crt'", "exit 97"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q:\n%s", want, script)
		}
	}
	e.Proxied = false
	if plain, _ := e.Script(); strings.Contains(plain, "/dev/tcp/proxy") {
		t.Error("an entrypoint without the proxy waits for it")
	}
}

// Provider keys cannot overwrite what points the sandbox at the proxy.
//
// @decision(DL-034)
func TestProviderKeysCannotRedirectTheProxy(t *testing.T) {
	for _, k := range []string{"HTTPS_PROXY", "no_proxy", "NODE_EXTRA_CA_CERTS", "SPOV_PROXY_CA"} {
		if _, err := (EnvConfig{Password: "p", ProviderKeys: map[string]string{k: "x"}}).Secrets(); err == nil {
			t.Errorf("a provider key named %s was accepted", k)
		}
	}
}

// The scratchpad exists before the server starts, and no clone may take
// a directory spov reserves.
//
// @decision(DL-037)
func TestEverySandboxGetsAScratchpadNoCloneCanTake(t *testing.T) {
	e := Entrypoint{Port: 4096, Clones: []Clone{{URL: "https://x.example/o/r.git", Dir: "r"}}}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	mkdir := strings.Index(script, "mkdir -p '"+ScratchDir+"'")
	if mkdir < 0 || mkdir > strings.Index(script, "exec ") || mkdir < strings.Index(script, "git clone") {
		t.Errorf("the scratchpad is not created after the clones and before the server:\n%s", script)
	}
	for _, dir := range []string{"out", ".scratch"} {
		e := Entrypoint{Port: 4096, Clones: []Clone{{URL: "https://x.example/o/r.git", Dir: dir}}}
		if err := e.Validate(); err == nil {
			t.Errorf("a clone into %q was accepted", dir)
		}
	}
}

// git's bootstrap reserves the same names the entrypoint refuses.
//
// @decision(DL-037)
func TestTheReservedDirectoriesAgree(t *testing.T) {
	if !slices.Equal(reservedDirs, git.ReservedDirs) {
		t.Errorf("opencode reserves %v, git %v", reservedDirs, git.ReservedDirs)
	}
}

// With the proxy, the entrypoint builds the Java trust store and exports
// JAVA_TOOL_OPTIONS only once it exists, and writes each preset's file
// through a quoted heredoc; a file that could end its heredoc early is
// refused.
//
// @decision(DL-076)
func TestThePresetFilesAndTheJavaTrustStoreAreWritten(t *testing.T) {
	e := Entrypoint{Port: 4096, Proxied: true, Files: []routedcred.File{{Path: routedcred.NPMRCFile, Content: "//npm.acme.com/:_authToken=ph\n"}}}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"-jdktrust anyExtendedKeyUsage",
		"then export JAVA_TOOL_OPTIONS='-Djavax.net.ssl.trustStore=" + credproxy.JavaTrustStore,
		"cat > '/root/.npmrc' <<'SPOV_PRESET_EOF'\n//npm.acme.com/:_authToken=ph\nSPOV_PRESET_EOF\n",
		"mkdir -p '/root'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	if strings.Index(script, "JAVA_TOOL_OPTIONS") > strings.Index(script, "spov_i=0") {
		t.Error("JAVA_TOOL_OPTIONS is exported after the proxy wait, not before the clones")
	}
	plain, _ := Entrypoint{Port: 4096}.Script()
	if strings.Contains(plain, "JAVA_TOOL_OPTIONS") || strings.Contains(plain, "SPOV_PRESET_EOF") {
		t.Error("a sandbox without the proxy got the Java trust store or preset files")
	}
	e.Files = []routedcred.File{{Path: routedcred.NPMRCFile, Content: "x\nSPOV_PRESET_EOF\nrm -rf /\n"}}
	if _, err := e.Script(); err == nil {
		t.Error("a file holding the heredoc's terminator was written")
	}
}
