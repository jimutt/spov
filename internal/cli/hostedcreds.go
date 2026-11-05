package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
)

// CredentialKeyEnvVar holds the base64 AES-256 key that seals user
// credentials. Without it the server runs on its own credentials alone
// and refuses to store anyone's.
//
// @decision(DL-008)
const CredentialKeyEnvVar = "SPOV_CREDENTIAL_KEY"

// CredentialKeyOldEnvVar holds the previous key during a rotation, so
// rows sealed under it can still be read while they are rewritten.
//
// @decision(DL-008)
const CredentialKeyOldEnvVar = "SPOV_CREDENTIAL_KEY_OLD"

// hostedCredentials resolves credentials per user: the user's own from
// the sealed store, then the deployment's from its environment.
//
// @decision(DL-042)
type hostedCredentials struct {
	env   *Env
	cfg   config.Config
	store *server.CredentialStore
	warn  func(string)
	// orgKeys are the deployment's model provider keys, resolved once.
	orgKeys map[string]string
	// apps are the org's stored GitHub Apps, read when the environment
	// sets none.
	//
	// @decision(DL-042)
	apps *server.OrgApps
}

// forgeFor builds a Forge that resolves each platform's credential for
// one user, under the tier policy, and reports which tier answered for
// each.
//
// @decision(DL-042)
func (h *hostedCredentials) forgeFor(ctx context.Context, orgID, userID string, policy server.TierPolicy) (server.Forge, []server.PlatformTier, error) {
	tiers := make([]server.PlatformTier, 0, len(h.cfg.Platforms))
	tokens := make(map[string]string, len(h.cfg.Platforms))
	for _, p := range h.cfg.Platforms {
		token, tier, reason := h.tokenFor(ctx, orgID, userID, p, policy)
		tiers = append(tiers, server.PlatformTier{Platform: p.Name, Tier: tier, Reason: reason})
		if tier != server.TierNone {
			tokens[p.Name] = token
		}
	}
	// A per-user Env: the resolver closes over this user's tokens, so
	// the existing listing and selection paths need no user of their
	// own. The tokens live only as long as the request that built them.
	//
	// @decision(DL-042)
	scoped := *h.env
	scoped.PlatformToken = func(_ context.Context, p config.Platform) (string, error) {
		token, ok := tokens[p.Name]
		if !ok {
			return "", fmt.Errorf("no credential for platform %q: %s", p.Name, reasonFor(tiers, p.Name))
		}
		return token, nil
	}
	return &configForge{env: &scoped, cfg: h.cfg, warn: h.warn}, tiers, nil
}

// tokenFor resolves one platform's credential for one user: their own
// first, then the deployment's, unless the policy prefers the
// deployment's or allows only one tier. A tier that holds a credential
// that cannot be read ends the resolution: it never falls through to the
// other tier.
//
// @decision(DL-042)
func (h *hostedCredentials) tokenFor(ctx context.Context, orgID, userID string, p config.Platform, policy server.TierPolicy) (token string, tier server.Tier, reason string) {
	if only, ok := policy.Only[p.Name]; ok {
		token, state, why := h.tierToken(ctx, orgID, userID, p, only)
		switch state {
		case tierPresent:
			return token, only, ""
		case tierBroken:
			return "", server.TierNone, why
		}
		return "", server.TierNone, fmt.Sprintf("the %s this session launched with is gone, and no other credential is used in its place", tierNoun(only))
	}
	order := []server.Tier{server.TierUser, server.TierOrg}
	if policy.Prefer[p.Name] == server.TierOrg {
		order = []server.Tier{server.TierOrg, server.TierUser}
	}
	for _, t := range order {
		token, state, why := h.tierToken(ctx, orgID, userID, p, t)
		switch state {
		case tierPresent:
			return token, t, ""
		case tierBroken:
			return "", server.TierNone, why
		}
	}
	return "", server.TierNone, addCredentialHint(p)
}

// reposByTier lists, platform by platform, the repositories each of the
// user's tiers reaches, through that tier's own credential, every pair at
// once. A platform neither tier has a credential for says how to add one,
// as the single listing did.
//
// @decision(DL-042) @decision(DL-028)
func (h *hostedCredentials) reposByTier(ctx context.Context, orgID, userID string) ([]server.TieredRepo, []string, error) {
	tiers := []server.Tier{server.TierUser, server.TierOrg}
	type result struct {
		repos []server.TieredRepo
		warn  string
		held  bool
	}
	results := make([]result, len(h.cfg.Platforms)*len(tiers))
	var wg sync.WaitGroup
	for i, p := range h.cfg.Platforms {
		for j, t := range tiers {
			wg.Go(func() {
				r := &results[i*len(tiers)+j]
				token, state, why := h.tierToken(ctx, orgID, userID, p, t)
				switch state {
				case tierBroken:
					r.held, r.warn = true, fmt.Sprintf("%s, %s: %s", p.Name, tierNoun(t), why)
					return
				case tierAbsent:
					return
				}
				r.held = true
				scoped := *h.env
				scoped.PlatformToken = func(context.Context, config.Platform) (string, error) { return token, nil }
				listed, err := scoped.listRepos(ctx, p)
				if err != nil {
					r.warn = listFailure(p, err) + " (with " + tierNoun(t) + ")"
					return
				}
				// @decision(DL-042)
				fineGrained := t == server.TierUser && p.Kind == config.KindGitHub && git.IsFineGrainedGitHubPAT(token)
				for _, repo := range listed.Repos {
					r.repos = append(r.repos, server.TieredRepo{PlatformRepo: git.PlatformRepo{Platform: listed.Platform, Repo: repo}, Tier: t,
						MaybeReadOnly: fineGrained && !repo.Private})
				}
			})
		}
	}
	wg.Wait()
	var (
		out      []server.TieredRepo
		warnings []string
	)
	for i, p := range h.cfg.Platforms {
		held := false
		for j := range tiers {
			r := results[i*len(tiers)+j]
			held = held || r.held
			out = append(out, r.repos...)
			if r.warn != "" {
				warnings = append(warnings, r.warn)
			}
		}
		if !held {
			warnings = append(warnings, fmt.Sprintf("no credential reaches %s: %s", p.Name, addCredentialHint(p)))
		}
	}
	return out, warnings, nil
}

// tierState is what one tier holds for a platform.
type tierState int

const (
	tierAbsent tierState = iota
	tierPresent
	tierBroken
)

// tierToken is one tier's credential for a platform.
//
// @decision(DL-042)
func (h *hostedCredentials) tierToken(ctx context.Context, orgID, userID string, p config.Platform, t server.Tier) (string, tierState, string) {
	switch t {
	case server.TierUser:
		platform, ok := credentialPlatform(p)
		if !ok {
			return "", tierAbsent, ""
		}
		secret, err := h.store.Get(ctx, orgID, userID, platform)
		if err != nil {
			// A credential that exists but cannot be opened must not
			// silently fall through to the org tier: that would hide a
			// lost key behind someone else's credentials.
			h.warn(fmt.Sprintf("credential for %s: %v", p.Name, err))
			return "", tierBroken, "your stored credential could not be read; store it again with `spov credentials add`"
		}
		if secret == "" {
			return "", tierAbsent, ""
		}
		return secret, tierPresent, ""
	case server.TierOrg:
		secret, err := h.orgToken(ctx, orgID, p)
		if errors.Is(err, errOrgCredentialUnreadable) {
			h.warn(fmt.Sprintf("the organisation's credential for %s: %v", p.Name, err))
			return "", tierBroken, "the organisation's credential could not be opened; ask an admin to store it again"
		}
		if errors.Is(err, errOrgCredentialUnavailable) {
			h.warn(fmt.Sprintf("the organisation's credential for %s: %v", p.Name, err))
			return "", tierBroken, "the organisation's credential could not be read just now; try again shortly"
		}
		if err != nil || secret == "" {
			return "", tierAbsent, ""
		}
		return secret, tierPresent, ""
	}
	return "", tierAbsent, ""
}

// tierNoun names a tier for a reader.
func tierNoun(t server.Tier) string {
	if t == server.TierOrg {
		return "organisation's credential"
	}
	return "your own credential"
}

// errOrgCredentialUnreadable is a stored org credential that exists and
// cannot be opened.
var errOrgCredentialUnreadable = errors.New("the stored organisation credential cannot be opened")

// errOrgCredentialUnavailable is a stored org credential that could not
// be read just now, the store failing rather than the row.
var errOrgCredentialUnavailable = errors.New("the stored organisation credential could not be read")

// orgToken is the deployment's own credential for a platform, from its
// environment. It is never a user's.
//
// @decision(DL-042)
//
// The environment wins; a GitHub platform it sets nothing for reads the
// App an admin stored (DL-042).
func (h *hostedCredentials) orgToken(ctx context.Context, orgID string, p config.Platform) (string, error) {
	if v := h.env.Environ.Getenv(orgTokenEnvVar(p.Name)); v != "" {
		return v, nil
	}
	// The GitHub App is the org tier's natural shape, and it is one
	// credential for every host.
	//
	// @decision(DL-042) @decision(DL-041)
	if p.Kind == config.KindGitHub {
		if cred, ok := h.envApp(); ok {
			return git.EncodeGitHubCredential(cred.AppID, cred.PrivateKey), nil
		}
		// @decision(DL-042)
		app, found, err := h.apps.Get(ctx, orgID, p.Name)
		if errors.Is(err, server.ErrOrgAppUnreadable) {
			return "", fmt.Errorf("%w: %w", errOrgCredentialUnreadable, err)
		}
		if err != nil {
			return "", fmt.Errorf("%w: %w", errOrgCredentialUnavailable, err)
		}
		if found {
			raw, err := json.Marshal(app.Credential)
			if err != nil {
				return "", err
			}
			return string(raw), nil
		}
	}
	return "", errors.New("no org credential")
}

// envApp is the GitHub App the deployment's environment sets.
//
// @decision(DL-042)
func (h *hostedCredentials) envApp() (git.GitHubCredential, bool) {
	appID := h.env.Environ.Getenv("SPOV_GITHUB_APP_ID")
	key := h.env.Environ.Getenv("SPOV_GITHUB_APP_PRIVATE_KEY")
	if appID == "" || key == "" {
		return git.GitHubCredential{}, false
	}
	return git.GitHubCredential{AppID: appID, PrivateKey: key}, true
}

// orgGitHub is the server's view of the deployment's GitHub platforms,
// for the admin's App routes.
//
// @decision(DL-042)
type orgGitHub struct{ h *hostedCredentials }

// Platforms are the configured GitHub platforms, and whether the
// environment sets each one's App.
func (o orgGitHub) Platforms() []server.OrgGitHubPlatform {
	var out []server.OrgGitHubPlatform
	env, envSet := o.h.envApp()
	for _, p := range o.h.cfg.Platforms {
		if p.Kind != config.KindGitHub {
			continue
		}
		plat := server.OrgGitHubPlatform{Name: p.Name, BaseURL: p.BaseURL, Account: p.Account}
		if raw := o.h.env.Environ.Getenv(orgTokenEnvVar(p.Name)); raw != "" {
			plat.FromEnv = true
			plat.Env, _ = git.ParseGitHubCredential(raw)
		} else if envSet {
			plat.FromEnv, plat.Env = true, env
		}
		out = append(out, plat)
	}
	return out
}

// Check builds the App as a launch would and checks it.
func (o orgGitHub) Check(ctx context.Context, plat server.OrgGitHubPlatform, cred git.GitHubCredential, account string) (git.AppStatus, error) {
	if cred.AppID == "" || cred.PrivateKey == "" {
		return git.AppStatus{}, errors.New("the environment sets a credential that is not a GitHub App, so it is not checked here")
	}
	g, err := git.NewGitHub(git.GitHubConfig{
		Name: plat.Name, BaseURL: plat.BaseURL, Account: account,
		AppID: cred.AppID, PrivateKeyPEM: cred.PrivateKey, InstallationID: cred.InstallationID,
	})
	if err != nil {
		return git.AppStatus{}, err
	}
	return g.Check(ctx)
}

// orgTokenEnvVar is the deployment's credential for one platform, named
// after it: SPOV_PLATFORM_TOKEN_CODEFLOE for a platform called
// "codefloe".
//
// @decision(DL-042)
func orgTokenEnvVar(name string) string {
	var b strings.Builder
	b.WriteString("SPOV_PLATFORM_TOKEN_")
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// credentialPlatform is the user-tier credential key for a configured
// platform, and whether the user tier applies at all.
//
// @decision(DL-042) @decision(DL-041)
func credentialPlatform(p config.Platform) (string, bool) {
	switch p.Kind {
	case config.KindGitHub:
		return server.PlatformGitHub, true
	case config.KindForgejo, config.KindGitea, config.KindGitLab:
		host, err := platformHost(p)
		if err != nil {
			return "", false
		}
		return server.HostPlatform(string(p.Kind), host), true
	}
	return "", false
}

// @decision(DL-041)
func addCredentialHint(p config.Platform) string {
	if p.Kind == config.KindGitHub {
		return "add one with `spov credentials add github`, or ask an admin to install the GitHub App"
	}
	switch p.Kind {
	case config.KindForgejo, config.KindGitea, config.KindGitLab:
		key, ok := credentialPlatform(p)
		if !ok {
			// No usable base URL: name the platform instead.
			key = string(p.Kind) + ":<" + p.Name + "'s host>"
		}
		return fmt.Sprintf("add one with `spov credentials add %s`, or ask an admin to configure a bot token", key)
	}
	return "ask an admin to configure one"
}

func reasonFor(tiers []server.PlatformTier, name string) string {
	for _, t := range tiers {
		if t.Platform == name {
			return t.Reason
		}
	}
	return "none is configured"
}

// providerKeysFor layers a user's own model provider keys over the
// deployment's, per variable.
//
// @decision(DL-042) @decision(DL-036)
func (h *hostedCredentials) providerKeysFor(ctx context.Context, orgID, userID string) (map[string]string, error) {
	own := map[string]string{}
	var unreadable []string
	for _, name := range providerEnvVars {
		secret, err := h.store.Get(ctx, orgID, userID, server.ProviderPlatform(name))
		if err != nil {
			// A key the user stored but that cannot be opened is
			// withheld, not replaced by the deployment's.
			//
			// @decision(DL-042)
			h.warn(fmt.Sprintf("provider key %s: %v", name, err))
			unreadable = append(unreadable, name)
			continue
		}
		if secret != "" {
			own[name] = secret
		}
	}
	keys := server.MergeProviderKeys(h.orgKeys, own)
	for _, name := range unreadable {
		delete(keys, name)
	}
	return keys, nil
}
