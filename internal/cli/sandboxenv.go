package cli

import (
	"fmt"
	"maps"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/toolchain"
)

// addEnvFlag adds the repeatable --env KEY=VALUE.
//
// @decision(DL-096)
func addEnvFlag(cmd *cobra.Command, into *[]string) {
	cmd.Flags().StringArrayVar(into, "env", nil,
		"set an environment variable in the sandbox, as KEY=VALUE (repeatable), over sandbox.env;"+
			" not for secrets, which go in a routed credential")
}

// launchEnv is sandbox.env with each --env set over it by name, checked
// as the server will check it, so a bad value fails before the server is
// asked. Nil when there are none.
//
// @decision(DL-096) @decision(DL-097)
func launchEnv(configured map[string]string, flags, addons []string) (map[string]string, error) {
	env := maps.Clone(configured)
	for _, f := range flags {
		name, value, ok := strings.Cut(f, "=")
		if !ok {
			return nil, fmt.Errorf("--env %q: write it as KEY=VALUE (KEY= for an empty value)", f)
		}
		if env == nil {
			env = map[string]string{}
		}
		env[name] = value
	}
	if err := checkSandboxEnv(env, addons, "sandbox.env or --env"); err != nil {
		return nil, err
	}
	return env, nil
}

// checkSandboxEnv is DL-097's check for a sandbox with these add-ons.
// An add-on list that does not parse is refused elsewhere.
//
// @decision(DL-097)
func checkSandboxEnv(env map[string]string, addons []string, label string) error {
	sel, _ := toolchain.Parse(addons)
	if err := opencode.CheckUserEnv(env, opencode.SpovEnvNames(sel, prep.ProviderKeyEnvs())); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}
