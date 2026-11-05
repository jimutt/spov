package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// accountOpenCode is the one account `login` takes by name; with none
// it signs in to the configured spov server.
const accountOpenCode = "opencode"

// newLoginCmd signs in to an account: the configured hosted server, or
// the OpenCode Console through the server. Tokens and keys are
// `spov credentials`'.
//
// @decision(DL-006) @decision(DL-015) @decision(DL-008)
func newLoginCmd(env *Env) *cobra.Command {
	var name string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login [opencode]",
		Short: "Sign in to the spov server, or connect OpenCode",
		Long: "Sign in to the configured hosted server: spov prints a code to enter in a browser and stores\n" +
			"the token that comes back. A local server needs no login.\n\n" +
			"`spov login opencode` connects your OpenCode Console account (OpenCode Go or Zen) through the\n" +
			"server, which keeps the sign-in for your sessions.\n\n" +
			"Tokens for git hosts and model provider keys are stored with `spov credentials add`.",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{accountOpenCode},
		RunE: func(cmd *cobra.Command, args []string) error {
			openCode, err := namedAccount("login", args)
			if err != nil {
				return err
			}
			if openCode {
				if name != "" {
					return fmt.Errorf("--name names a spov server token; `spov login opencode` takes none")
				}
				return runOpenCodeLogin(cmd.Context(), env, noBrowser)
			}
			return runServerLogin(cmd.Context(), env, serverLoginOptions{Name: name, NoBrowser: noBrowser})
		},
	}
	// @decision(DL-006)
	cmd.Flags().StringVar(&name, "name", "",
		"name for a server token, shown on the approval page (default: this host and version)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "never try to open a browser while signing in")
	return cmd
}

// newLogoutCmd signs out of an account, the reverse of `login`.
//
// @decision(DL-006) @decision(DL-015) @decision(DL-008)
func newLogoutCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "logout [opencode]",
		Short: "Sign out of the spov server, or disconnect OpenCode",
		Long: "Sign out of the configured hosted server: the token is revoked there and deleted here.\n\n" +
			"`spov logout opencode` forgets your OpenCode Console sign-in on the server.\n\n" +
			"Stored tokens and keys are removed with `spov credentials remove`.",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{accountOpenCode},
		RunE: func(cmd *cobra.Command, args []string) error {
			openCode, err := namedAccount("logout", args)
			if err != nil {
				return err
			}
			if openCode {
				return runOpenCodeLogout(cmd.Context(), env)
			}
			return runServerLogout(cmd.Context(), env)
		},
	}
}

// namedAccount reports whether args name the OpenCode account. Anything
// else is most likely a credential, which has its own command.
//
// @decision(DL-008)
func namedAccount(verb string, args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if strings.EqualFold(args[0], accountOpenCode) {
		return true, nil
	}
	other := "add"
	if verb == "logout" {
		other = "remove"
	}
	return false, fmt.Errorf("`spov %s` takes no name but opencode; a token or key such as %q is"+
		" handled by `spov credentials %s %s`", verb, args[0], other, args[0])
}
