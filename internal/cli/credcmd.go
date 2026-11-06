package cli

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
)

// newCredentialsCmd is the group for the caller's stored tokens and
// keys. Called bare it lists, as `credentials list` does. Where they are
// stored follows the server the CLI talks to (hostedServer).
//
// @decision(DL-008) @decision(DL-023)
func newCredentialsCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "List, add and remove your stored tokens and keys",
		Long: "A credential is a token for a git host or a model provider key. It is named as the listing\n" +
			"shows it: `github`, `forgejo:<host>`, `gitea:<host>`, `gitlab:<host>`, a provider's\n" +
			"environment variable such as OPENROUTER_API_KEY, or, with a local server, a platform's\n" +
			"name in config.\n\n" +
			"With a local server, credentials are kept in this machine's credential store, which that\n" +
			"server reads. With a hosted server they are stored on the server, for your sessions there.\n" +
			"No command prints a credential back. The listing ends with whether OpenCode is connected\n" +
			"(`spov login opencode`).",
		Args: cobra.NoArgs,
	}
	listCredentials(env, cmd)
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your stored credentials",
		Args:    cobra.NoArgs,
	}
	listCredentials(env, list)
	cmd.AddCommand(list, newCredentialsAddCmd(env), newCredentialsRemoveCmd(env))
	return cmd
}

// listCredentials makes cmd list the caller's credentials.
//
// @decision(DL-008) @decision(DL-023)
func listCredentials(env *Env, cmd *cobra.Command) {
	var asJSON bool
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		hosted, err := env.hostedServer()
		if err != nil {
			return err
		}
		if !hosted {
			if err := listLocal(env, asJSON); err != nil {
				return err
			}
			if !asJSON {
				printOpenCodeLine(cmd.Context(), env)
			}
			return nil
		}
		return runListCredentials(cmd.Context(), env, asJSON)
	}
	jsonFlag(cmd, &asJSON)
}

// hostedServer reports whether the CLI talks to a server someone else
// runs, which stores credentials itself, rather than its own local one,
// which reads this machine's store.
//
// @decision(DL-008)
func (e *Env) hostedServer() (bool, error) {
	url, err := e.configuredServerURL()
	return url != "", err
}

// credentialName is a credential as the user names it: a model provider
// variable, or a platform in whichever spelling the server takes. Raw is
// what was typed.
type credentialName struct {
	Provider string
	Platform string
	Raw      string
}

// providerShape is an environment variable as a provider key is named.
var providerShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// parseCredentialName reads a name. `provider:<VAR>`, any spelling of a
// name ending in _API_KEY, or an upper-case environment variable is a
// provider key; anything else names a platform. With a local server a
// configured platform's own name wins over the last rule (localName).
//
// @decision(DL-008)
func parseCredentialName(arg string) credentialName {
	s := strings.TrimSpace(arg)
	if v, ok := cutPrefixFold(s, server.ProviderPrefix); ok && v != "" {
		return credentialName{Provider: strings.ToUpper(v), Raw: s}
	}
	if upper := strings.ToUpper(s); strings.HasSuffix(upper, "_API_KEY") && providerShape.MatchString(upper) {
		return credentialName{Provider: upper, Raw: s}
	}
	if providerShape.MatchString(s) {
		return credentialName{Provider: s, Raw: s}
	}
	return credentialName{Platform: s, Raw: s}
}

// serverKey is the key a hosted server stores the name under, and the
// prompt for its secret.
//
// @decision(DL-008)
func (n credentialName) serverKey() (key, prompt string, err error) {
	if n.Provider != "" {
		if err := validProviderVar(n.Provider); err != nil {
			return "", "", err
		}
		key, err := server.CanonicalPlatform(server.ProviderPlatform(n.Provider))
		return key, n.Provider + ": ", err
	}
	key, err = server.CanonicalPlatform(n.Platform)
	if err != nil {
		return "", "", fmt.Errorf("%w; a hosted server names a credential github, forgejo:<host>, gitea:<host>, gitlab:<host> or a provider variable", err)
	}
	if key == server.PlatformGitHub {
		return key, "GitHub fine-grained personal access token: ", nil
	}
	// @decision(DL-041)
	if kind, host, ok := server.PlatformHost(key); ok {
		return key, fmt.Sprintf("%s access token for %s: ", git.KindName(kind), host), nil
	}
	return key, fmt.Sprintf("Access token for %s: ", key), nil
}

// addOptions are `credentials add`'s flags. The App flags and
// --no-verify apply to this machine's store only.
type addOptions struct {
	// @decision(DL-073)
	Route          routeOptions
	Token          string
	AppID          string
	KeyFile        string
	InstallationID string
	NoVerify       bool
}

func (o addOptions) app() bool { return o.AppID != "" || o.KeyFile != "" || o.InstallationID != "" }

func newCredentialsAddCmd(env *Env) *cobra.Command {
	var opts addOptions
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Store a token for a git host or a model provider key",
		Long: "Store a credential, verifying it first.\n\n" +
			"  spov credentials add github               a GitHub token (hosted server)\n" +
			"  spov credentials add github --app-id ID --key-file app.pem\n" +
			"                                            a GitHub App (local server)\n" +
			"  spov credentials add forgejo:codeberg.org a Forgejo token\n" +
			"  spov credentials add OPENROUTER_API_KEY   a model provider key\n" +
			"  spov credentials add acme-npm --host npm.acme.com --bearer\n" +
			"                                            a routed credential, set only on its host\n\n" +
			"The secret is prompted for without echo unless --token gives it. A routed credential's\n" +
			"secret comes from the prompt or --secret-file, and is stored unchecked.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// @decision(DL-073)
			if _, prefixed := routeName(args[0]); prefixed || opts.Route.set() {
				if opts.app() || opts.NoVerify {
					return fmt.Errorf("--app-id, --key-file, --installation-id and --no-verify are for a platform, not a routed credential")
				}
				return addRoutedCredential(cmd.Context(), env, args[0], opts.Route, opts.Token)
			}
			return addCredential(cmd.Context(), env, parseCredentialName(args[0]), opts)
		},
	}
	addRouteFlags(cmd, &opts.Route)
	cmd.Flags().StringVar(&opts.Token, "token", "", "the token or key; prompted for if omitted")
	// @decision(DL-041)
	cmd.Flags().StringVar(&opts.AppID, "app-id", "", "GitHub App id or client id (local server)")
	cmd.Flags().StringVar(&opts.KeyFile, "key-file", "", "path to the GitHub App private key, .pem (local server)")
	cmd.Flags().StringVar(&opts.InstallationID, "installation-id", "",
		"GitHub App installation id; discovered from the account if omitted (local server)")
	cmd.Flags().BoolVar(&opts.NoVerify, "no-verify", false,
		"store without listing repositories to check it; the platform is still contacted while being built (local server)")
	return cmd
}

// addCredential stores one credential where the server reads it.
//
// @decision(DL-008)
func addCredential(ctx context.Context, env *Env, n credentialName, opts addOptions) error {
	hosted, err := env.hostedServer()
	if err != nil {
		return err
	}
	if !hosted {
		if n, err = localName(env, n); err != nil {
			return err
		}
		if n.Provider != "" {
			// Silently ignoring these would accept a mistyped command and
			// store something the user did not mean to store.
			if opts.app() {
				return fmt.Errorf("--app-id, --key-file and --installation-id configure a GitHub platform, not a provider key")
			}
			return addLocalProvider(env, n.Provider, opts)
		}
		return addLocalPlatform(ctx, env, n.Platform, opts)
	}
	switch {
	case opts.app() && n.Provider != "":
		return fmt.Errorf("--app-id, --key-file and --installation-id configure a GitHub platform, not a provider key")
	case opts.app():
		return fmt.Errorf("a hosted server takes a GitHub token, not an App; the deployment's own App is" +
			" configured by an admin")
	case opts.NoVerify:
		return fmt.Errorf("--no-verify applies to this machine's store; a hosted server checks what it" +
			" stores itself")
	}
	key, prompt, err := n.serverKey()
	if err != nil {
		return err
	}
	return runServerCredential(ctx, env, key, prompt, opts.Token)
}

// runServerCredential stores one credential on the hosted server. The
// token is prompted for without echo unless it was given, so it does not
// reach the process list or the shell history.
//
// @decision(DL-008)
func runServerCredential(ctx context.Context, env *Env, platform, prompt, secret string) error {
	client, err := env.client(ctx)
	if err != nil {
		return err
	}
	if secret == "" {
		if secret, err = env.promptSecret(prompt); err != nil {
			return err
		}
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return fmt.Errorf("the token is empty")
	}
	_, _ = fmt.Fprintf(env.Err, "Verifying with the server...\n")
	view, err := client.PutCredential(ctx, platform, secret)
	if err != nil {
		// The secret can reach an error only by being echoed back in a
		// message; redact it in case a platform ever does.
		return fmt.Errorf("%s", redactSecret(err.Error(), secret))
	}
	name := displayCredential(platform)
	// @decision(DL-041)
	facts := tokenFacts(git.TokenInfo{User: view.VerifiedAs, Scopes: view.Scopes, ExpiresAt: view.ExpiresAt}, env.now())
	_, _ = fmt.Fprintf(env.Out, "Stored %s on the server%s.\n", name, facts)
	return nil
}

// displayCredential is a server key as `add` and `remove` take it: a
// provider key by its bare variable.
//
// @decision(DL-008)
func displayCredential(platform string) string {
	if v, ok := strings.CutPrefix(platform, server.ProviderPrefix); ok {
		return v
	}
	return platform
}

func runListCredentials(ctx context.Context, env *Env, asJSON bool) error {
	client, err := env.client(ctx)
	if err != nil {
		return err
	}
	creds, enabled, err := client.ListCredentials(ctx)
	if err != nil {
		return err
	}
	// The OpenCode sign-in is its own view (GET /me/opencode), so it is
	// left out here.
	//
	// @decision(DL-023)
	// @decision(DL-073)
	routes, err := listRoutedCredentials(ctx, env)
	if err != nil {
		return err
	}
	// @decision(DL-074)
	orgRoutes, err := listOrgRoutedCredentials(ctx, env)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(env.Out, credentialsBody{Credentials: orEmpty(creds), Enabled: enabled,
			RoutedCredentials: orEmpty(routes), OrgRoutedCredentials: orgRoutes})
	}
	defer printOpenCodeLine(ctx, env)
	defer printPreferences(env)
	defer printRoutedCredentials(env, orgRoutes, "The organisation's routed credentials, which your sessions get too:")
	defer printRoutedCredentials(env, routes, "Routed credentials:")
	if !enabled {
		_, _ = fmt.Fprintln(env.Out, "This server stores no user credentials:"+
			" it has no SPOV_CREDENTIAL_KEY configured.")
		_, _ = fmt.Fprintln(env.Out, "Your sessions use the server's own credentials.")
		return nil
	}
	if len(creds) == 0 {
		_, _ = fmt.Fprintln(env.Out, "No credentials stored on the server. Your sessions use the server's own.")
		return nil
	}
	sort.Slice(creds, func(i, j int) bool { return creds[i].Platform < creds[j].Platform })
	_, _ = fmt.Fprintln(env.Out, "Stored on the server:")
	for _, c := range creds {
		line := "  " + displayCredential(c.Platform)
		if c.VerifiedAs != "" {
			line += "  (" + c.VerifiedAs + ")"
		}
		// @decision(DL-041)
		if !c.ExpiresAt.IsZero() {
			line += "  " + expiryPhrase(c.ExpiresAt, env.now())
		}
		if !c.UpdatedAt.IsZero() {
			line += "  stored " + c.UpdatedAt.Format("2006-01-02")
		}
		_, _ = fmt.Fprintln(env.Out, line)
	}
	return nil
}

// printPreferences says which platforms this machine's config has a CLI
// launch try the organisation's credential on first.
//
// @decision(DL-042)
func printPreferences(env *Env) {
	// Only a hosted server has a shared tier to prefer.
	if hosted, err := env.hostedServer(); err != nil || !hosted {
		return
	}
	res, err := env.config()
	if err != nil {
		return
	}
	for _, p := range res.Config.Platforms {
		if p.Prefer == config.PreferShared {
			_, _ = fmt.Fprintf(env.Out, "Launches from here try the organisation's credential first on %s (prefer: shared).\n", p.Name)
		}
	}
}

// printOpenCodeLine ends a listing with the OpenCode sign-in, which is
// held apart from the stored credentials; a local server holds one too.
// A server that cannot say is left out.
//
// @decision(DL-015) @decision(DL-008)
func printOpenCodeLine(ctx context.Context, env *Env) {
	c, err := env.client(ctx)
	if err != nil {
		return
	}
	if v, err := c.OpenCodeConnection(ctx); err == nil {
		if line := openCodeLine(v); line != "" {
			_, _ = fmt.Fprintln(env.Out, line)
		}
	}
}

// openCodeLine says where the caller's OpenCode sign-in stands, or
// nothing when the server cannot hold one.
//
// @decision(DL-015)
func openCodeLine(v server.OpenCodeConnectionView) string {
	switch {
	case !v.Enabled:
		return ""
	case v.NeedsReconnect:
		return "OpenCode: the sign-in was refused by OpenCode; run `spov login opencode` again"
	case !v.Connected:
		return "OpenCode: not connected (`spov login opencode`)"
	}
	line := "OpenCode: connected"
	if v.OrgID != "" {
		line += "  (organization " + v.OrgID + ")"
	}
	if !v.ConnectedAt.IsZero() {
		line += "  since " + v.ConnectedAt.Format("2006-01-02")
	}
	return line
}

func newCredentialsRemoveCmd(env *Env) *cobra.Command {
	var org bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a stored credential",
		Long: "Remove one of your stored credentials, named as `spov credentials` lists it.\n\n" +
			"On a hosted server your sessions fall back to the server's own credential for that platform,\n" +
			"or lose access to it if the server has none.",
		Args: cobra.ExactArgs(1),
		// @decision(DL-023)
		ValidArgsFunction: completeCredentials(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			// @decision(DL-074)
			if org {
				return removeOrgRoutedCredential(cmd.Context(), env, args[0])
			}
			// A bare name that cannot be a platform is tried as a routed
			// credential first; route:<name> is only ever one.
			//
			// @decision(DL-073)
			if name, ok := isRoutedName(env, args[0]); ok {
				removed, err := removeRoutedCredential(cmd.Context(), env, name)
				if err != nil || removed {
					return err
				}
				if _, prefixed := routeName(args[0]); prefixed {
					_, _ = fmt.Fprintf(env.Out, "No routed credential named %s %s.\n", name, whereRoutesAre(env))
					return nil
				}
			}
			return removeCredential(cmd.Context(), env, parseCredentialName(args[0]))
		},
	}
	cmd.Flags().BoolVar(&org, "org", false, "remove the organisation's routed credential of that name (admins, hosted server)")
	return cmd
}

// removeCredential removes one credential from where the server reads it.
//
// @decision(DL-008)
func removeCredential(ctx context.Context, env *Env, n credentialName) error {
	hosted, err := env.hostedServer()
	if err != nil {
		return err
	}
	if !hosted {
		if n, err = localName(env, n); err != nil {
			return err
		}
		return removeLocal(env, n)
	}
	platform, _, err := n.serverKey()
	if err != nil {
		return err
	}
	client, err := env.client(ctx)
	if err != nil {
		return err
	}
	if err := client.DeleteCredential(ctx, platform); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(env.Out, "Removed %s from the server.\n", displayCredential(platform))
	return nil
}
