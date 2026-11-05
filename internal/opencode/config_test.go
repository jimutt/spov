package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/opencode/contract"
)

// externalDirectoryGrants reads the external_directory rules out of a
// config document's ordered permissions list, by resource.
func externalDirectoryGrants(t *testing.T, doc []byte) map[string]string {
	t.Helper()
	var cfg struct {
		Permissions []struct {
			Action, Resource, Effect string
		} `json:"permissions"`
	}
	if err := json.Unmarshal(doc, &cfg); err != nil {
		t.Fatalf("the config is not valid JSON: %v\n%s", err, doc)
	}
	out := map[string]string{}
	for _, r := range cfg.Permissions {
		if r.Action == ActionExternalDirectory {
			out[r.Resource] = r.Effect
		}
	}
	return out
}

// TestSandboxConfigJSON checks the config's shape against the schema
// OpenCode publishes, since a wrong key fails silently (OpenCode merges
// what it recognises and ignores the rest). The permissions list is the
// form GA 2.0.22 itself normalised the V1 `permission` map into.
//
// @decision(DL-038) @decision(DL-031)
func TestSandboxConfigJSON(t *testing.T) {
	var cfg struct {
		Schema    string `json:"$schema"`
		WebSearch struct {
			Provider string `json:"provider"`
		} `json:"websearch"`
		Permission json.RawMessage `json:"permission"`
	}
	out := []byte(SandboxConfigJSON("", false))
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("the generated config is not valid JSON: %v", err)
	}

	if cfg.Schema != configSchema {
		t.Errorf("$schema = %q, want %q", cfg.Schema, configSchema)
	}
	if cfg.Permission != nil {
		t.Error("the config still writes V1's permission key")
	}
	if cfg.WebSearch.Provider != WebSearchProvider {
		t.Errorf("web search provider = %q, want %q", cfg.WebSearch.Provider, WebSearchProvider)
	}
	grants := externalDirectoryGrants(t, out)
	for _, pattern := range workspaceGlobs {
		got, ok := grants[pattern]
		if !ok {
			t.Fatalf("no rule for %q; external_directory = %v", pattern, grants)
		}
		if got != "allow" {
			t.Errorf("rule for %q is %q, want \"allow\"", pattern, got)
		}
	}
}

// Both sessions pin web search to the one provider the egress presets
// admit.
//
// @decision(DL-031) @decision(DL-038)
func TestEverySessionPinsItsWebSearch(t *testing.T) {
	for name, doc := range map[string]string{"interactive": SandboxConfigJSON("m", false), "job": JobConfigJSON("m", false)} {
		var cfg struct {
			WebSearch struct {
				Provider string `json:"provider"`
			} `json:"websearch"`
		}
		if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.WebSearch.Provider != WebSearchProvider {
			t.Errorf("%s: web search provider = %q, want %q", name, cfg.WebSearch.Provider, WebSearchProvider)
		}
	}
}

// TestSandboxConfigIsScopedToTheWorkspace: a bare "allow" would silence
// the prompt for the git credentials and the OpenCode auth store too,
// which are the ones worth seeing.
//
// @decision(DL-038)
func TestSandboxConfigIsScopedToTheWorkspace(t *testing.T) {
	var cfg struct {
		Permissions []struct {
			Action, Resource, Effect string
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(SandboxConfigJSON("", false)), &cfg); err != nil {
		t.Fatal(err)
	}
	for _, r := range cfg.Permissions {
		if r.Effect != "allow" {
			continue
		}
		if r.Action != ActionExternalDirectory {
			t.Errorf("the config allows %q, which is not the workspace grant", r.Action)
		}
		if r.Resource != WorkspaceRoot && !strings.HasPrefix(r.Resource, WorkspaceRoot+"/") {
			t.Errorf("resource %q allows access outside %s", r.Resource, WorkspaceRoot)
		}
	}
}

// TestEntrypointWritesTheConfig: the file has to exist before the
// server starts, or the first prompt appears anyway.
//
// @decision(DL-038)
func TestEntrypointWritesTheConfig(t *testing.T) {
	e := Entrypoint{
		Port:    4096,
		Clones:  []Clone{{URL: "https://example.com/o/r.git", Dir: "r"}},
		Landing: "r",
	}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`mkdir -p "` + ConfigDir + `"`,
		`cat > "` + ConfigFile + `"`,
		`"external_directory"`,
		WorkspaceRoot + "/**",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the entrypoint does not contain %q", want)
		}
	}

	// Written before the exec, or the server never reads it. The anchor
	// is the redirect itself: ConfigFile alone also appears in the mkdir
	// line, so a change to either constant could silently move what is
	// being measured.
	write := strings.Index(script, `cat > "`+ConfigFile+`"`)
	start := strings.Index(script, "exec "+imageBinary)
	if write < 0 || start < 0 {
		t.Fatalf("could not locate the config write (%d) or the exec (%d)", write, start)
	}
	if write > start {
		t.Error("the config is written after the server is started")
	}
}

// TestEntrypointWritesTheConfigForASingleRepo: the working directory can
// change within a session, so the config is not conditional on how many
// repositories were cloned.
//
// @decision(DL-038)
func TestEntrypointWritesTheConfigForASingleRepo(t *testing.T) {
	e := Entrypoint{Port: 4096}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, ConfigFile) {
		t.Error("no config was written for a session with no clones")
	}
}

// TestEntrypointActuallyWritesTheConfig runs the generated script under
// bash and reads the file back, proving the heredoc, mkdir and $HOME
// expansion work rather than only the script text being right.
//
// @decision(DL-038)
func TestEntrypointActuallyWritesTheConfig(t *testing.T) {
	home := t.TempDir()
	// Populated so the config heredoc has to coexist with the AGENTS.md
	// heredoc and the credential lines, which is where a delimiter
	// collision would show up.
	e := Entrypoint{
		Port:    4096,
		Clones:  []Clone{{URL: "https://example.com/o/r.git", Dir: "r"}},
		Landing: "r",
		Workspace: []WorkspaceRepo{
			{Slug: "o/r", Dir: "r", Platform: "codefloe", Host: "codefloe.com", CLI: CLITea},
		},
	}
	runConfigScript(t, e, home)

	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("no config was written: %v", err)
	}

	if externalDirectoryGrants(t, data)[WorkspaceRoot+"/**"] != "allow" {
		t.Errorf("written config = %s", data)
	}
}

// TestWorkspacePatternsCoverTheRealPaths encodes how OpenCode matches
// these patterns: each "*" is substituted with ".*" in a regex
// (anomalyco/opencode#22465), not glob matching.
//
// @decision(DL-038)
func TestWorkspacePatternsCoverTheRealPaths(t *testing.T) {
	match := func(pattern, path string) bool {
		parts := strings.Split(pattern, "*")
		for i, p := range parts {
			parts[i] = regexp.QuoteMeta(p)
		}
		return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$").MatchString(path)
	}
	covered := func(path string) bool {
		for _, pattern := range workspaceGlobs {
			if match(pattern, path) {
				return true
			}
		}
		return false
	}

	// Every path a session actually touches must be allowed.
	for _, path := range []string{
		WorkspaceRoot,                       // the server can land here
		WorkspaceRoot + "/widgets",          // a cloned repository
		WorkspaceRoot + "/widgets/internal", // and anything below
	} {
		if !covered(path) {
			t.Errorf("%q is not covered by %v", path, workspaceGlobs)
		}
	}

	// Paths outside the workspace must still prompt.
	for _, path := range []string{
		"/root/.git-credentials",
		"/root/.local/share/opencode/auth.json",
		"/etc/passwd",
	} {
		if covered(path) {
			t.Errorf("%q is allowed but lies outside %s", path, WorkspaceRoot)
		}
	}
}

// runConfigScript renders an entrypoint and runs the parts of it that do
// not need the image or the network, with HOME pointed at home. Only
// running the script proves the heredoc, the mkdir and the $HOME
// expansion; asserting about the script text proves the string is right.
func runConfigScript(t *testing.T, e Entrypoint, home string) {
	t.Helper()
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}

	var kept []string
	for _, line := range strings.Split(script, "\n") {
		switch {
		case strings.HasPrefix(line, "cp "), strings.HasPrefix(line, "exec "),
			strings.HasPrefix(line, "cd "), strings.HasPrefix(line, "git "),
			strings.Contains(line, "git clone"), strings.Contains(line, "spov_pid"),
			strings.Contains(line, ScratchDir):
			continue
		}
		kept = append(kept, line)
	}

	// The AGENTS.md heredoc writes to the real WorkspaceRoot, which does
	// not exist outside a sandbox. Only its redirect is retargeted at the
	// temp home — a blanket substitution would also rewrite the workspace
	// paths inside the config being asserted.
	body := strings.ReplaceAll(
		strings.Join(kept, "\n"),
		"cat > "+AgentsFile+" ",
		"cat > "+home+"/AGENTS.md ",
	)

	cmd := exec.Command("bash", "-c", body)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}
}

// Both config documents are valid against the config schema the pinned
// build publishes, so a key it would ignore fails here instead.
//
// @decision(DL-038) @decision(DL-035)
func TestTheConfigDocumentsMatchThePinnedSchema(t *testing.T) {
	d, err := contract.Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"interactive": SandboxConfigJSON("anthropic/claude-sonnet-4-5", false), "job": JobConfigJSON("anthropic/claude-sonnet-4-5", false),
		// @decision(DL-038)
		"interactive, declared": SandboxConfigJSON("anthropic/claude-next", true), "job, declared": JobConfigJSON("anthropic/claude-next", true),
	} {
		if err := d.CheckSchema("Config.InfoEncoded", []byte(doc)); err != nil {
			t.Errorf("%s: %v\n%s", name, err, doc)
		}
	}
}

// An unlisted model is declared under its provider by id, variant off,
// with nothing else under the provider; a listed one is not declared.
// The id is encoded like the model string, so it cannot close the
// heredoc.
//
// @decision(DL-038)
func TestAnUnlistedModelIsDeclared(t *testing.T) {
	for name, doc := range map[string]string{
		"interactive": SandboxConfigJSON("openrouter/acme/new-model#high", true),
		"job":         JobConfigJSON("openrouter/acme/new-model#high", true),
	} {
		var cfg struct {
			Providers map[string]map[string]any `json:"providers"`
		}
		if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(cfg.Providers)
		if string(got) != `{"openrouter":{"models":{"acme/new-model":{"name":"acme/new-model"}}}}` {
			t.Errorf("%s declares %s", name, got)
		}
	}
	if strings.Contains(SandboxConfigJSON("anthropic/claude-sonnet-5", false), "providers") {
		t.Error("a listed model was declared")
	}
	evil := SandboxConfigJSON("anthropic/x\nSPOV_CONFIG_EOF\nrm -rf /", true)
	if strings.Contains(evil, "\nSPOV_CONFIG_EOF") {
		t.Error("a declared id can close the heredoc")
	}
}
