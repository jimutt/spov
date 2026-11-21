package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A plugin is an npm package spec and nothing that would make OpenCode
// fetch from anywhere but the registry.
//
// @decision(DL-171)
func TestCheckPluginSpec(t *testing.T) {
	for _, ok := range []string{
		"envsitter-guard", "envsitter-guard@0.1.1", "envsitter-guard@latest", "@scope/name", "@scope/name@1.2.0",
		"opencode-rules@^2.0.0", "a@>=2.0.0", "pkg@2.x", "pkg@1.0.0-beta.1", "@opencode/plugin-browser@2.3.2",
	} {
		if err := CheckPluginSpec(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", " pkg", "pkg ", "Pkg", "pkg@", "@scope", "@scope/", "../evil", "./local", "/abs/path", "file:../x",
		"git+https://github.com/a/b", "https://example.com/p.tgz", "github:a/b", "a/b", "pkg@1 || 2",
		"pkg@file:../x", "pkg@https://x", "pkg@" + strings.Repeat("1", 65), strings.Repeat("a", 215), ".hidden", "_under",
		"pkg\nnext", "pkg@1;rm",
	} {
		if err := CheckPluginSpec(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A list is bounded and lists a package once.
//
// @decision(DL-171)
func TestCheckPluginsBoundsTheList(t *testing.T) {
	var many []string
	for i := range MaxPlugins + 1 {
		many = append(many, fmt.Sprintf("p%d", i))
	}
	if CheckPlugins(many) == nil {
		t.Error("a list over the limit was accepted")
	}
	if CheckPlugins(many[:MaxPlugins]) != nil {
		t.Error("a list at the limit was refused")
	}
	if CheckPlugins([]string{"a", "a"}) == nil {
		t.Error("a package listed twice was accepted")
	}
	if CheckPlugins(nil) != nil {
		t.Error("no plugins was refused")
	}
}

// The list reaches an interactive session's config as OpenCode 2's
// `plugins` key, and a job's config carries none.
//
// @decision(DL-171)
func TestAPluginListReachesTheSandboxConfig(t *testing.T) {
	var cfg struct {
		Plugins []string `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(SandboxConfigJSON("", false, []string{"envsitter-guard@0.1.1", "@scope/x"})), &cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Plugins, ",") != "envsitter-guard@0.1.1,@scope/x" {
		t.Errorf("plugins = %v", cfg.Plugins)
	}
	if strings.Contains(SandboxConfigJSON("", false, nil), "plugins") {
		t.Error("an empty list wrote the key")
	}
	if strings.Contains(JobConfigJSON("", false), "plugins") {
		t.Error("a job's config lists plugins")
	}
	e := Entrypoint{Port: 4096, Plugins: []string{"envsitter-guard"}}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, `"plugins": [`) || !strings.Contains(script, `"envsitter-guard"`) {
		t.Errorf("the entrypoint does not write the plugins:\n%s", script)
	}
	e.Plugins = []string{"git+https://example.com/x"}
	if _, err := e.Script(); err == nil {
		t.Error("the entrypoint wrote a plugin that is not a package spec")
	}
}
