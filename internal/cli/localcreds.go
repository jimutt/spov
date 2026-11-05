package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
)

// This machine's credential store, which a local server reads: what
// `spov credentials` adds, lists and removes when the CLI talks to its
// own local server.
//
// @decision(DL-008)

// addLocalPlatform stores a git platform credential in this machine's
// store, verifying it first.
//
// @decision(DL-008)
func addLocalPlatform(ctx context.Context, env *Env, name string, opts addOptions) error {
	res, err := env.config()
	if err != nil {
		return err
	}
	p, err := localPlatform(res.Config, name)
	if err != nil {
		return err
	}
	if env.Credentials == nil {
		return fmt.Errorf("no credential store configured")
	}

	secret, err := platformSecret(env, p, opts)
	if err != nil {
		return err
	}

	if !opts.NoVerify {
		if err := verifyPlatform(ctx, env, p, secret); err != nil {
			return err
		}
	}

	key, err := credentials.PlatformKey(p.Name)
	if err != nil {
		return err
	}
	if err := env.Credentials.Set(key, secret); err != nil {
		return fmt.Errorf("storing the credential for %q: %w", p.Name, err)
	}
	_, _ = fmt.Fprintf(env.Out, "Stored the credential for %s on this machine (%s).\n", p.Name, env.Credentials.Name())
	return nil
}

// platformSecret builds the stored value for a platform, which differs
// by kind: Forgejo, Gitea and GitLab store a bare token, GitHub a JSON
// App credential.
//
// @decision(DL-008) @decision(DL-041)
func platformSecret(env *Env, p config.Platform, opts addOptions) (string, error) {
	switch p.Kind {
	case config.KindForgejo, config.KindGitea, config.KindGitLab:
		if opts.AppID != "" || opts.KeyFile != "" || opts.InstallationID != "" {
			return "", fmt.Errorf("platform %q is a %s platform; --app-id, --key-file and --installation-id are for GitHub", p.Name, p.Kind)
		}
		token := opts.Token
		if token == "" {
			prompt := fmt.Sprintf("Access token for %s on %s: ", p.Account, p.Name)
			if p.Account == "" {
				prompt = fmt.Sprintf("%s access token for %s: ", git.KindName(string(p.Kind)), p.Name)
			}
			prompted, err := env.promptSecret(prompt)
			if err != nil {
				return "", err
			}
			token = prompted
		}
		return requireNonEmpty(token, "token")

	case config.KindGitHub:
		return githubSecret(p, opts)

	default:
		return "", fmt.Errorf("platform %q has unknown kind %q", p.Name, p.Kind)
	}
}

// githubSecret assembles the JSON App credential from the flags. The
// private key is read from a path, never taken as a flag value.
//
// @decision(DL-008) @decision(DL-041)
func githubSecret(p config.Platform, opts addOptions) (string, error) {
	if opts.Token != "" {
		return "", fmt.Errorf("platform %q is a GitHub platform and needs an App credential; "+
			"pass --app-id and --key-file rather than --token", p.Name)
	}
	if opts.AppID == "" {
		return "", fmt.Errorf("platform %q needs --app-id (the App id or client id)", p.Name)
	}
	if opts.KeyFile == "" {
		return "", fmt.Errorf("platform %q needs --key-file, the path to the App private key (.pem)", p.Name)
	}

	key, err := os.ReadFile(opts.KeyFile)
	if err != nil {
		return "", fmt.Errorf("reading the App private key: %w", err)
	}
	cred := git.GitHubCredential{
		AppID:      opts.AppID,
		PrivateKey: string(key),
	}
	if opts.InstallationID != "" {
		id, err := strconv.ParseInt(opts.InstallationID, 10, 64)
		if err != nil {
			return "", fmt.Errorf("--installation-id must be a number: %w", err)
		}
		cred.InstallationID = id
	}

	encoded, err := json.Marshal(cred)
	if err != nil {
		return "", fmt.Errorf("encoding the GitHub credential: %w", err)
	}
	return string(encoded), nil
}

// verifyPlatform checks a credential works before it is stored, so a
// typo fails here rather than partway through creating a sandbox.
//
// @decision(DL-008)
func verifyPlatform(ctx context.Context, env *Env, p config.Platform, secret string) error {
	_, _ = fmt.Fprintf(env.Err, "Verifying against %s...\n", p.Name)

	// Do not wrap the cause with %w below: a wrapped error would carry
	// the unredacted text out through Unwrap.
	platform, err := env.buildPlatform(ctx, p, secret)
	if err != nil {
		return fmt.Errorf("the credential was not stored: %s", redactSecret(err.Error(), secret))
	}
	// What the forge says about the token comes first, so a token that
	// lacks a scope is refused for that, not for an empty listing.
	//
	// @decision(DL-041)
	info, err := git.Inspect(ctx, platform)
	if err != nil && !errors.Is(err, git.ErrUnsupported) {
		return fmt.Errorf("the credential was not stored: %s", redactSecret(err.Error(), secret))
	}
	repos, err := platform.ListRepos(ctx)
	if err != nil {
		return fmt.Errorf("the credential was not stored; %s rejected it or granted nothing: %s",
			p.Name, redactSecret(err.Error(), secret))
	}
	_, _ = fmt.Fprintf(env.Err, "  ok, %d repositories reachable%s\n", len(repos), tokenFacts(info, env.now()))
	return nil
}

// tokenFacts is what a credential check learned about a token, as the
// tail of the line that reports it: ", verified as <user>", then its
// scopes and expiry in parentheses.
//
// @decision(DL-041)
func tokenFacts(info git.TokenInfo, now time.Time) string {
	out := ""
	if info.User != "" {
		out = ", verified as " + info.User
	}
	var extra []string
	if len(info.Scopes) > 0 {
		extra = append(extra, "scopes "+strings.Join(info.Scopes, ", "))
	}
	if !info.ExpiresAt.IsZero() {
		extra = append(extra, expiryPhrase(info.ExpiresAt, now))
	}
	if info.ServerVersion != "" {
		extra = append(extra, "server "+info.ServerVersion)
	}
	if len(extra) > 0 {
		out += " (" + strings.Join(extra, "; ") + ")"
	}
	return out
}

// expiryPhrase says when a token stops working, in days while that is
// close.
//
// @decision(DL-041)
func expiryPhrase(at, now time.Time) string {
	days := int(at.Sub(now).Hours() / 24)
	switch {
	case !now.Before(at):
		return "expired " + at.Format(time.DateOnly)
	case days == 0:
		return fmt.Sprintf("expires %s, within a day", at.Format(time.DateOnly))
	case days == 1:
		return fmt.Sprintf("expires %s, in 1 day", at.Format(time.DateOnly))
	case days < 30:
		return fmt.Sprintf("expires %s, in %d days", at.Format(time.DateOnly), days)
	}
	return "expires " + at.Format(time.DateOnly)
}

// minRedactableSecret is the shortest value replaced literally, so a
// very short credential does not rewrite unrelated words in the message.
//
// @decision(DL-008)
const minRedactableSecret = 8

// pemBlock matches a PEM private key however it is encoded: literal
// newlines, JSON-escaped `\n`, or none at all.
//
// @decision(DL-008)
var pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

// redactSecret removes the credential from text on its way to the user;
// Forgejo quotes a rejected token back in its error. Private keys are
// matched by pattern, since an echo inside JSON escapes their newlines.
//
// @decision(DL-008)
func redactSecret(text, secret string) string {
	// Any PEM block goes, whoever it belongs to.
	text = pemBlock.ReplaceAllString(text, "-----BEGIN PRIVATE KEY----- *** -----END PRIVATE KEY-----")

	if len(secret) >= minRedactableSecret {
		text = strings.ReplaceAll(text, secret, "***")
	}
	// A GitHub credential is JSON; scrub the key it wraps as well, in
	// case it survived the pattern above in some partial form.
	if cred, err := git.ParseGitHubCredential(secret); err == nil && len(cred.PrivateKey) >= minRedactableSecret {
		text = strings.ReplaceAll(text, cred.PrivateKey, "***")
	}
	return git.RedactAll(text)
}

// addLocalProvider stores a model provider key in this machine's store.
//
// @decision(DL-008) @decision(DL-036)
func addLocalProvider(env *Env, name string, opts addOptions) error {
	if err := validProviderVar(name); err != nil {
		return err
	}
	if env.Credentials == nil {
		return fmt.Errorf("no credential store configured")
	}

	secret := opts.Token
	if secret == "" {
		prompted, err := env.promptSecret(fmt.Sprintf("Value for %s: ", name))
		if err != nil {
			return err
		}
		secret = prompted
	}
	secret, err := requireNonEmpty(secret, "value")
	if err != nil {
		return err
	}

	key, keyErr := credentials.ProviderKey(name)
	if keyErr != nil {
		return keyErr
	}
	if err := env.Credentials.Set(key, secret); err != nil {
		return fmt.Errorf("storing %s: %w", name, err)
	}
	_, _ = fmt.Fprintf(env.Out, "Stored %s on this machine (%s).\n", name, env.Credentials.Name())
	if env.Environ.Getenv(name) != "" {
		// The environment wins at launch, so a stored key that is also
		// exported will not be the one used.
		_, _ = fmt.Fprintf(env.Err,
			"spov: note: %s is also set in this environment, which takes precedence over the stored value.\n", name)
	}
	return nil
}

// validProviderVar keeps the credential key readable and rejects a name
// that is obviously not an environment variable.
//
// @decision(DL-008)
func validProviderVar(name string) error {
	if name == "" {
		return fmt.Errorf("a provider key needs an environment variable name, e.g. OPENROUTER_API_KEY")
	}
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return fmt.Errorf("%q is not an environment variable name; expected upper-case letters, digits and underscores", name)
		}
	}
	// A name outside providerEnvVars is allowed: that list is what spov
	// forwards from its own environment, not the set of keys a user may
	// store.
	return nil
}

// requireNonEmpty rejects a blank credential and returns it trimmed, so
// a stray pasted space is not stored to fail at the next launch.
//
// @decision(DL-008)
func requireNonEmpty(s, what string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", fmt.Errorf("the %s is empty", what)
	}
	return trimmed, nil
}

// removeLocal deletes a credential from this machine's store. Nothing
// stored is reported as such rather than failing: the end state the user
// asked for is the same.
//
// @decision(DL-008)
func removeLocal(env *Env, n credentialName) error {
	if env.Credentials == nil {
		return fmt.Errorf("no credential store configured")
	}
	key, label, err := localKey(env, n)
	if err != nil {
		return err
	}
	// A name that is not configured is still looked up: a platform
	// removed from config still has a credential worth removing. The
	// cost is that a typo reports success, so the message names what was
	// actually looked for.
	if _, err := env.Credentials.Get(key); err != nil {
		_, _ = fmt.Fprintf(env.Out, "No stored credential for %s on this machine.\n", label)
		return nil
	}
	if err := env.Credentials.Delete(key); err != nil {
		return fmt.Errorf("removing the credential for %s: %w", label, err)
	}
	_, _ = fmt.Fprintf(env.Out, "Removed the credential for %s from this machine (%s).\n", label, env.Credentials.Name())
	return nil
}

// localKey is the store key a name refers to on this machine, and the
// label to report it by.
//
// @decision(DL-008)
func localKey(env *Env, n credentialName) (key, label string, err error) {
	if n.Provider != "" {
		if err := validProviderVar(n.Provider); err != nil {
			return "", "", err
		}
		key, err = credentials.ProviderKey(n.Provider)
		return key, n.Provider, err
	}
	label = n.Platform
	if res, cerr := env.config(); cerr == nil {
		if p, perr := localPlatform(res.Config, n.Platform); perr == nil {
			label = p.Name
		}
	}
	key, err = credentials.PlatformKey(label)
	return key, label, err
}

// localName lets a configured platform's own name win: `GH` is a
// provider variable by shape, but not when config has a platform by that
// name.
//
// @decision(DL-008)
func localName(env *Env, n credentialName) (credentialName, error) {
	if n.Provider == "" || n.Raw != n.Provider {
		return n, nil
	}
	res, err := env.config()
	if err != nil {
		return n, err
	}
	if _, err := findPlatform(res.Config, n.Raw); err == nil {
		return credentialName{Platform: n.Raw, Raw: n.Raw}, nil
	}
	return n, nil
}

// localPlatform finds the configured platform a name refers to: its own
// name in config, `github` for the GitHub platform, or `<kind>:<host>`
// for the Forgejo platform on that host, the names a hosted server uses.
// An alias several platforms answer to is refused rather than guessed.
//
// @decision(DL-008)
func localPlatform(cfg config.Config, name string) (config.Platform, error) {
	if p, err := findPlatform(cfg, name); err == nil {
		return p, nil
	}
	if matches := aliasMatches(cfg, name); len(matches) == 1 {
		return matches[0], nil
	} else if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, p := range matches {
			names[i] = p.Name
		}
		return config.Platform{}, fmt.Errorf("%s matches several configured platforms (%s); name one of them",
			name, strings.Join(names, ", "))
	}
	return findPlatform(cfg, name)
}

// aliasMatches is every configured platform a hosted server's name
// (`github`, `<kind>:<host>`) refers to.
func aliasMatches(cfg config.Config, name string) []config.Platform {
	var out []config.Platform
	for _, p := range cfg.Platforms {
		if alias, ok := credentialPlatform(p); ok && strings.EqualFold(alias, name) {
			out = append(out, p)
		}
	}
	return out
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// listLocal prints which known credentials this machine's store holds:
// every configured platform and every provider variable spov forwards.
// The store cannot enumerate its keys, so nothing else is looked for.
//
// @decision(DL-008)
func listLocal(env *Env, asJSON bool) error {
	lines, views, err := heldLocally(env, env.Err)
	if err != nil {
		return err
	}
	const probed = "Only configured platforms and the provider variables spov knows are checked;" +
		" a key stored under another name is not listed."
	// The hosted listing's shape, with a platform named as config names
	// it, which is what `credentials remove` takes here.
	//
	// @decision(DL-023)
	if asJSON {
		_, _ = fmt.Fprintln(env.Err, "spov: "+probed)
		return writeJSON(env.Out, credentialsBody{Credentials: orEmpty(views), Enabled: true})
	}
	if len(lines) == 0 {
		_, _ = fmt.Fprintf(env.Out, "No credentials stored on this machine (%s).\n", env.Credentials.Name())
	} else {
		_, _ = fmt.Fprintf(env.Out, "Stored on this machine (%s):\n", env.Credentials.Name())
		for _, l := range lines {
			_, _ = fmt.Fprintln(env.Out, "  "+l)
		}
	}
	_, err = fmt.Fprintln(env.Out, probed)
	return err
}

// heldLocally probes this machine's store for the configured platforms
// and the known provider variables: a line per credential found, as the
// listing prints it, and its view. A key that cannot be read is
// reported on warn and skipped.
//
// @decision(DL-008)
func heldLocally(env *Env, warn io.Writer) ([]string, []server.CredentialView, error) {
	if env.Credentials == nil {
		return nil, nil, fmt.Errorf("no credential store configured")
	}
	res, err := env.config()
	if err != nil {
		return nil, nil, err
	}
	var lines []string
	var views []server.CredentialView
	held := func(key string) bool {
		_, err := env.Credentials.Get(key)
		if err != nil && !errors.Is(err, credentials.ErrNotFound) {
			_, _ = fmt.Fprintf(warn, "spov: %s could not be read: %v\n", key, err)
		}
		return err == nil
	}
	platforms := append([]config.Platform(nil), res.Config.Platforms...)
	sort.Slice(platforms, func(i, j int) bool { return platforms[i].Name < platforms[j].Name })
	for _, p := range platforms {
		key, err := credentials.PlatformKey(p.Name)
		if err != nil || !held(key) {
			continue
		}
		line := p.Name
		if alias, ok := credentialPlatform(p); ok && alias != p.Name && len(aliasMatches(res.Config, alias)) == 1 {
			line += "  (" + alias + ")"
		}
		lines = append(lines, line)
		views = append(views, server.CredentialView{Platform: p.Name, Present: true})
	}
	vars := append([]string(nil), providerEnvVars...)
	sort.Strings(vars)
	for _, v := range vars {
		if key, err := credentials.ProviderKey(v); err == nil && held(key) {
			lines = append(lines, v)
			views = append(views, server.CredentialView{Platform: server.ProviderPlatform(v), Present: true})
		}
	}
	return lines, views, nil
}

// promptSecret reads a secret without echoing it.
//
// @decision(DL-008)
func (e *Env) promptSecret(prompt string) (string, error) {
	if e.PromptSecret != nil {
		return e.PromptSecret(prompt)
	}
	if e.Stdin == nil || !term.IsTerminal(int(e.Stdin.Fd())) {
		return "", fmt.Errorf("no terminal to prompt on; pass the credential as a flag instead")
	}
	_, _ = fmt.Fprint(e.Err, prompt)
	secret, err := term.ReadPassword(int(e.Stdin.Fd()))
	_, _ = fmt.Fprintln(e.Err)
	if err != nil {
		return "", fmt.Errorf("reading the credential: %w", err)
	}
	return string(secret), nil
}

// buildPlatform constructs a git platform from a credential that is not
// yet stored, which is what login needs to verify one.
//
// @decision(DL-008)
func (e *Env) buildPlatform(ctx context.Context, p config.Platform, secret string) (git.Platform, error) {
	if e.NewPlatform != nil {
		return e.NewPlatform(ctx, p, secret)
	}
	return newPlatform(ctx, p, secret)
}
