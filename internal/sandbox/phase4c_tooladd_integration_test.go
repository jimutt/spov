//go:build integration

// Spike S10 reads, out of the published OpenCode binary, whether a
// plugin-added tool is permissioned and under what action, since DL-040's
// allowlist would deny DL-048's completion tool if it asserted one.
//
//	go test -tags integration ./internal/sandbox/ -run S10 -v
//
// It creates no sandbox and needs no Modal credential or model key.
//
// Measured on 0.0.0-beta-18684: each tool's own execute writes its
// permission assertion, and the only computed action is MCP's
// `<server>_<tool>`, so spov's tool asserts nothing. A separate toolset
// filter on `options.permission ?? name` reads OpenCode's own rules,
// which spov does not set. The test fails on a build that asserts on
// every tool's behalf.
package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestIntegrationS10PluginToolPermissionAction reads whether a
// plugin-contributed tool's execution asserts a permission action in the
// build under test.
//
// @decision(DL-048)
func TestIntegrationS10PluginToolPermissionAction(t *testing.T) {
	version := testOpenCodeVersion()
	t.Logf("reading the plugin-tool permission path out of OpenCode %s", version)

	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	downloadOpenCodeBinary(t, version, bin)
	blob, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob)

	// 1. The registration mechanism DL-048 uses must exist; without it
	//    the tool cannot be registered at all.
	transformAdd := regexp.MustCompile(`tool:\{[^}]{0,200}transform`)
	if !transformAdd.MatchString(text) {
		t.Errorf("OpenCode %s exposes no tool.transform on the plugin context: "+
			"DL-048's completion tool cannot be registered in this build", version)
	}
	if !strings.Contains(text, "add:(") && !strings.Contains(text, "add(") {
		t.Errorf("OpenCode %s exposes no add on the tool registry", version)
	}

	// 2. Enumerate every assert call site, whatever the shape of its
	//    action. Matching only literal actions misses the variable-bound
	//    and computed sites, which are the ones that matter.
	sites := regexp.MustCompile(`assert\(\{[^}]{0,140}`).FindAllString(text, -1)
	t.Logf("MEASURED %d assert({...}) call sites", len(sites))
	if len(sites) == 0 {
		t.Fatalf("OpenCode %s has no assert({...}) call sites at all: this spike can no "+
			"longer read the permission path and must not be trusted either way", version)
	}

	// 3. Does any assert compute its action from a value? Only MCP's
	//    `gg(l.server, l.name)` does; another such site would mean
	//    contributed tools are permissioned under a derived name.
	computed := regexp.MustCompile(`assert\(\{action:([A-Za-z_$][A-Za-z0-9_$]*)\(([^)]{0,60})\)`)
	var computedSites []string
	for _, m := range computed.FindAllStringSubmatch(text, -1) {
		computedSites = append(computedSites, m[1]+"("+m[2]+")")
		t.Logf("MEASURED an assert whose action is COMPUTED: %s(%s)", m[1], m[2])
	}

	// MCP's site takes a server and a tool name; any other computed site
	// is new and must be read before the completion tool is trusted.
	for _, site := range computedSites {
		if strings.Contains(site, ".server") && strings.Contains(site, ".name") {
			continue
		}
		t.Errorf("OpenCode %s computes a permission action at %s, which is not MCP's "+
			"<server>_<tool> site: contributed tools may now be permissioned under a "+
			"derived name, and DL-048's completion tool would be DENIED by the plugin's "+
			"allowlist on every job. Read the site before shipping.", version, site)
	}

	// 4. A dispatcher-level assert, applied to every tool's execution,
	//    would permission spov's tool without spov asking. If one
	//    appears, the completion tool needs an allowlist entry.
	for _, name := range []string{"tool", "plugin", "custom", "spov_complete", "spov-complete"} {
		if assertsAction(text, name) {
			t.Errorf("OpenCode %s asserts %q as a permission action: plugin-added tools are "+
				"permissioned under it and DL-048's completion tool needs it allowlisted",
				version, name)
		}
	}

	// 5. The toolset filter keyed on `options.permission ?? name`. It
	//    reads OpenCode's own rules, which spov does not set, but it is
	//    how a tool could vanish before the model sees it.
	filter := regexp.MustCompile(`options\?\.permission\?\?[A-Za-z_$][A-Za-z0-9_$]*`)
	if m := filter.FindString(text); m != "" {
		t.Logf("MEASURED the toolset filter keys on %q "+
			"(a contributed tool falls back to its own NAME)", m)
	} else {
		t.Logf("the toolset filter's `options.permission ?? name` shape is no longer present; " +
			"a contributed tool's exposure may now be decided some other way")
	}

	// 6. The literal vocabulary, logged as context for a reader
	//    comparing this against TestIntegrationActionVocabulary.
	literal := regexp.MustCompile(`assert\(\{action:"([a-z_][a-z0-9_]*)"`)
	seen := map[string]bool{}
	var actions []string
	for _, m := range literal.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			actions = append(actions, m[1])
		}
	}
	t.Logf("literal assert actions (the rest are variable-bound): %v", actions)
}
