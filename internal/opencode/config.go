package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ConfigDir and ConfigFile locate the global OpenCode config the
// entrypoint writes; a repository's own opencode.json still layers on
// top since configs are merged, not replaced. The path is expanded by
// the shell rather than hard-coded to /root, since the entrypoint also
// runs under the test user.
//
// @decision(DL-038)
const (
	ConfigDir  = "$HOME/.config/opencode"
	ConfigFile = ConfigDir + "/opencode.json"
)

// configSchema is the schema OpenCode publishes for this file.
const configSchema = "https://opencode.ai/config.json"

// workspaceGlobs are the patterns granted inside the sandbox: the
// workspace root itself, and everything beneath it. OpenCode substitutes
// each "*" with ".*" in a regex rather than matching as a glob, so
// "/workspace/**" does not match the bare "/workspace"; the root is
// listed separately so a server landing there is covered too.
//
// @decision(DL-038)
var workspaceGlobs = []string{
	WorkspaceRoot,
	WorkspaceRoot + "/**",
}

// sandboxConfig is the config document. Only the fields spov sets are
// modelled; anything else in the file would be spov inventing policy for
// the user. A job's document leaves Permissions empty, so the key is
// omitted entirely, which DL-038 requires.
//
// @decision(DL-038)
type sandboxConfig struct {
	Schema      string           `json:"$schema"`
	Model       string           `json:"model,omitempty"`
	WebSearch   webSearch        `json:"websearch"`
	Permissions []permissionRule `json:"permissions,omitempty"`
	// Providers declares a model the list lacks, so OpenCode resolves
	// it; the key is `providers` at the pinned build. Nothing else under
	// a provider is written: its settings and key are the proxy's.
	//
	// @decision(DL-038)
	Providers map[string]declaredProvider `json:"providers,omitempty"`
	// Plugins are the launch's sandbox.plugins, npm package specs that
	// OpenCode installs and loads; the key is `plugins` at OpenCode 2.
	//
	// @decision(DL-171)
	Plugins []string `json:"plugins,omitempty"`
	// Instructions names spov's generated guide for a session that starts
	// inside a repository, where OpenCode's own search for AGENTS.md
	// stops at the repository's root.
	//
	// @decision(DL-174)
	Instructions []string `json:"instructions,omitempty"`
	// Automations is false in a job's config, which takes OpenCode's
	// automation tools out of the agent's Code Mode catalog; unset, an
	// interactive session keeps them.
	//
	// @decision(DL-184)
	Automations *bool `json:"automations,omitempty"`
}

// declaredProvider holds the one declared model, by id.
type declaredProvider struct {
	Models map[string]declaredModel `json:"models"`
}

type declaredModel struct {
	Name string `json:"name"`
}

// declare is the providers block declaring model, a provider/model[#variant]
// reference, or nil when there is nothing to declare.
//
// @decision(DL-038)
func declare(model string, unlisted bool) map[string]declaredProvider {
	if !unlisted {
		return nil
	}
	provider, rest, ok := strings.Cut(model, "/")
	id, _, _ := strings.Cut(rest, "#")
	if !ok || provider == "" || id == "" {
		return nil
	}
	return map[string]declaredProvider{provider: {Models: map[string]declaredModel{id: {Name: id}}}}
}

// withoutMode is a model reference without its #mode, which OpenCode
// ignores in the config's model; spov sets the mode when it opens the
// conversation.
//
// @decision(DL-187)
func withoutMode(model string) string {
	bare, _, _ := strings.Cut(model, "#")
	return bare
}

// webSearch pins the built-in web search to one provider.
type webSearch struct {
	Provider string `json:"provider"`
}

// WebSearchProvider is the web search provider every sandbox uses. Its
// host is the one AgentServiceDomains admits; left unset, OpenCode picks
// among several.
//
// @decision(DL-031) @decision(DL-035)
const WebSearchProvider = "exa"

// permissionRule is one entry of OpenCode's ordered permissions list.
type permissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// allowAll grants an action on every pattern, in order.
func allowAll(action string, patterns []string) []permissionRule {
	out := make([]permissionRule, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, permissionRule{Action: action, Resource: p, Effect: "allow"})
	}
	return out
}

// SandboxConfigJSON is the config written into an interactive session's
// sandbox. The grant is scoped to the workspace rather than a bare
// "allow", so a prompt still appears for the git credentials and the
// OpenCode auth store.
//
// An unlisted model is declared (DL-038), and the launch's plugins are
// listed (DL-171).
//
// @decision(DL-038) @decision(DL-171)
func SandboxConfigJSON(model string, unlisted bool, plugins, instructions []string) string {
	return marshalConfig(sandboxConfig{
		Schema:       configSchema,
		Model:        withoutMode(model),
		WebSearch:    webSearch{Provider: WebSearchProvider},
		Permissions:  allowAll(ActionExternalDirectory, workspaceGlobs),
		Providers:    declare(model, unlisted),
		Plugins:      plugins,
		Instructions: instructions,
	})
}

// JobConfigJSON is the config written into a job's sandbox. It carries no
// permission, permissions or tools key: all three normalise into the
// same rule list, and a deny there would short-circuit the plugin's
// policy without consulting it.
//
// An unlisted model is declared (DL-038), and OpenCode's automations
// are off (DL-184).
//
// @decision(DL-038) @decision(DL-184)
func JobConfigJSON(model string, unlisted bool, instructions []string) string {
	off := false
	return marshalConfig(sandboxConfig{
		Schema:       configSchema,
		Model:        withoutMode(model),
		WebSearch:    webSearch{Provider: WebSearchProvider},
		Providers:    declare(model, unlisted),
		Instructions: instructions,
		// @decision(DL-184)
		Automations: &off,
	})
}

// marshalConfig renders a config document. Encoding the model escapes
// the newlines that would otherwise let it close the entrypoint's
// heredoc; this holds only while that heredoc stays quoted.
//
// @decision(DL-038)
func marshalConfig(cfg sandboxConfig) string {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("opencode: marshalling sandbox config: %v", err))
	}
	return string(out) + "\n"
}
