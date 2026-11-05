package toolchain

import (
	"fmt"
	"maps"
	"strings"
)

// Guide renders the toolchain section of the workspace AGENTS.md for a
// sandbox with the given add-ons. It is built from the same tables as
// the image, so it names exactly what is installed, at the resolved
// versions.
//
// @decision(DL-033)
func Guide(sel []Selection) string {
	var b strings.Builder
	b.WriteString("## Toolchain\n\n")
	b.WriteString("Installed: ")
	var tools []string
	for _, t := range Defaults {
		v := t.Version
		// A pinned language is listed at its pin.
		//
		// @decision(DL-033)
		for _, s := range sel {
			if s.Language() && s.Name == t.Name {
				v = s.Exact()
			}
		}
		tools = append(tools, displayName(t.Name)+" "+v)
	}
	tools = append(tools, "corepack (pnpm, yarn) "+CorepackVersion, "mise "+MiseVersion)
	b.WriteString(strings.Join(tools, ", "))
	b.WriteString(", plus build-essential, pkg-config, jq, sqlite3, fd, ripgrep, zip and unzip.\n\n")
	b.WriteString("- A repository's own version files win inside it: `.tool-versions`, `mise.toml`, `.nvmrc`, " +
		"`.node-version` and `.python-version` through mise, and `go.mod` through Go itself. " +
		"A version that is not installed yet is downloaded on first use.\n")
	b.WriteString("- `mise use <tool>@<version>` installs and selects another version. " +
		"A download needs egress; under a restricted preset it can fail, and the installed version stays.\n")
	b.WriteString("- Docker is not available in this sandbox.\n")
	for _, s := range sel {
		if s.Language() {
			fmt.Fprintf(&b, "- %s\n", s.Note())
		}
	}
	var extra []Selection
	for _, s := range sel {
		if !s.Language() {
			extra = append(extra, s)
		}
	}
	if len(extra) > 0 {
		b.WriteString("\nAdd-ons in this sandbox:\n\n")
		for _, s := range extra {
			fmt.Fprintf(&b, "- %s\n", s.Note())
			if svc := s.Service(); svc != nil {
				fmt.Fprintf(&b, "  It may still be starting during your first command; its start-up log is `%s`.\n", svc.Log)
			}
		}
	}
	return b.String()
}

func displayName(tool string) string {
	switch tool {
	case "python":
		return "Python"
	case "node":
		return "Node"
	case "bun":
		return "Bun"
	case "go":
		return "Go"
	}
	return tool
}

// Services are the selections' services, in selection order.
//
// @decision(DL-033)
func Services(sel []Selection) []Service {
	var out []Service
	for _, s := range sel {
		if svc := s.Service(); svc != nil {
			out = append(out, *svc)
		}
	}
	return out
}

// Env is every environment variable the selections set, merged.
//
// @decision(DL-033)
func Env(sel []Selection) map[string]string {
	env := map[string]string{}
	for _, s := range sel {
		maps.Copy(env, s.Addon().Env)
	}
	return env
}
