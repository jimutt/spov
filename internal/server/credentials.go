package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/store"
)

// Tier names where a resolved credential came from. It travels to the
// client and is recorded on the session; the credential itself never is.
//
// @decision(DL-042)
type Tier string

const (
	// TierUser is a credential the user gave spov themselves.
	TierUser Tier = "user"
	// TierOrg is the deployment's own credential, shared by everyone.
	TierOrg Tier = "org"
	// TierNone is a platform no credential could be resolved for.
	TierNone Tier = "none"
)

// TierPolicy says which credential tier each platform resolves to.
// Prefer names a tier tried first, the other being the fallback when the
// preferred one has no credential; Only names the one tier allowed, as a
// launch recorded it. A platform in neither is personal-first. Keyed by
// configured platform name.
//
// @decision(DL-042)
type TierPolicy struct {
	Prefer map[string]Tier
	Only   map[string]Tier
}

// policyFromRequest is the tier policy a launch resolves with: the
// request's preferences.
//
// @decision(DL-042)
func policyFromRequest(req store.SessionRequest) TierPolicy {
	p := TierPolicy{}
	for platform, tier := range req.CredentialPreference {
		if Tier(tier) == TierOrg || Tier(tier) == TierUser {
			if p.Prefer == nil {
				p.Prefer = map[string]Tier{}
			}
			p.Prefer[platform] = Tier(tier)
		}
	}
	return p
}

// policyOf is the tier policy every step after a launch resolves with:
// the tier the launch recorded for each platform, else the request's
// preference.
//
// @decision(DL-042)
func policyOf(sess store.Session) TierPolicy {
	p := policyFromRequest(sess.Request)
	for _, c := range sess.Credentials {
		if Tier(c.Tier) == TierOrg || Tier(c.Tier) == TierUser {
			if p.Only == nil {
				p.Only = map[string]Tier{}
			}
			p.Only[c.Platform] = Tier(c.Tier)
		}
	}
	return p
}

// PlatformGitHub and the per-host kinds name the platforms a user may
// hold their own credential for. A Forgejo, Gitea or GitLab credential
// is per host, since a deployment may be configured with more than one.
//
// @decision(DL-008) @decision(DL-041)
const (
	PlatformGitHub  = "github"
	ForgejoPrefix   = "forgejo:"
	ProviderPrefix  = "provider:"
	maxSecretLength = 4096
)

// HostKinds are the kinds whose credential is keyed `<kind>:<host>`, in
// the order help text names them.
//
// @decision(DL-041)
var HostKinds = []string{git.KindForgejo, git.KindGitea, git.KindGitLab}

// HostPlatform names the credential for one host of a per-host kind.
//
// @decision(DL-041)
func HostPlatform(kind, host string) string { return kind + ":" + strings.ToLower(host) }

// ForgejoPlatform names the credential for one Forgejo host.
func ForgejoPlatform(host string) string { return HostPlatform(git.KindForgejo, host) }

// PlatformHost splits a per-host credential key into its kind and host.
//
// @decision(DL-041)
func PlatformHost(key string) (kind, host string, ok bool) {
	for _, k := range HostKinds {
		if h, found := strings.CutPrefix(key, k+":"); found && h != "" {
			return k, h, true
		}
	}
	return "", "", false
}

// ProviderPlatform names the credential for one model provider variable.
func ProviderPlatform(envVar string) string { return ProviderPrefix + strings.ToUpper(envVar) }

// CanonicalPlatform validates a platform key and returns the single
// spelling it is stored under: a host lowercased, a provider variable
// upper-cased. Every endpoint canonicalises before it touches the
// store. An unknown key is refused rather than stored.
//
// @decision(DL-008)
func CanonicalPlatform(platform string) (string, error) {
	p := strings.TrimSpace(platform)
	switch {
	case strings.EqualFold(p, PlatformGitHub):
		return PlatformGitHub, nil
	case hasPrefixFold(p, ProviderPrefix):
		name := p[len(ProviderPrefix):]
		if name == "" {
			return "", errors.New("a provider credential needs a variable name, as provider:ANTHROPIC_API_KEY")
		}
		name = strings.ToUpper(name)
		if !strings.HasSuffix(name, "_API_KEY") {
			return "", fmt.Errorf("%q is not a provider key variable; those end in _API_KEY", name)
		}
		return ProviderPrefix + name, nil
	}
	// @decision(DL-041)
	for _, kind := range HostKinds {
		if !hasPrefixFold(p, kind+":") {
			continue
		}
		host := p[len(kind)+1:]
		if host == "" {
			return "", fmt.Errorf("a %s credential needs a host, as %s", kind, HostPlatform(kind, exampleHost(kind)))
		}
		return HostPlatform(kind, host), nil
	}
	return "", fmt.Errorf("%q is not a platform spov stores credentials for"+
		" (github, forgejo:<host>, gitea:<host>, gitlab:<host>, or provider:<VAR>)", platform)
}

// exampleHost is a host a per-host kind is commonly found on, for help
// text.
func exampleHost(kind string) string {
	switch kind {
	case git.KindGitea:
		return "gitea.com"
	case git.KindGitLab:
		return "gitlab.com"
	}
	return "codefloe.com"
}

// hasPrefixFold is strings.HasPrefix, case-insensitively.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// ValidPlatform reports whether a platform key is one spov stores.
//
// @decision(DL-008)
func ValidPlatform(platform string) error {
	_, err := CanonicalPlatform(platform)
	return err
}

// CredentialStore reads and writes a user's own credentials, sealing
// them on the way in and opening them on the way out. The plaintext
// exists only in the caller's memory, for as long as a launch or a
// verification takes.
//
// @decision(DL-008)
type CredentialStore struct {
	Store  store.CredentialStore
	Sealer *auth.Sealer
	Now    func() time.Time
}

// Get returns the user's own credential for a platform, or "" when they
// have none. A row that cannot be opened is an error rather than a
// miss: silently falling back to the org tier would hide a key rotation
// that lost data.
//
// @decision(DL-008)
func (c *CredentialStore) Get(ctx context.Context, orgID, userID, platform string) (string, error) {
	if c == nil || c.Store == nil || c.Sealer == nil {
		return "", nil
	}
	rec, err := c.Store.Get(ctx, orgID, userID, platform)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	plain, err := c.Sealer.Open(rec.Ciphertext, rec.Nonce, rec.KeyVersion,
		auth.CredentialAAD(orgID, userID, platform))
	if err != nil {
		return "", fmt.Errorf("credential for %s: %w", platform, err)
	}
	return string(plain), nil
}

// Put seals a credential and stores it, replacing whatever was there.
// verifiedAs is the account the token proved it owns, kept for display.
//
// @decision(DL-008)
func (c *CredentialStore) Put(ctx context.Context, orgID, userID, platform, secret string, verified Verified) error {
	if c == nil || c.Store == nil || c.Sealer == nil {
		return auth.ErrNoKey
	}
	if secret == "" {
		return errors.New("the credential is empty")
	}
	if len(secret) > maxSecretLength {
		return fmt.Errorf("the credential is longer than %d bytes", maxSecretLength)
	}
	ct, nonce, version, err := c.Sealer.Seal([]byte(secret), auth.CredentialAAD(orgID, userID, platform))
	if err != nil {
		return err
	}
	now := c.now()
	return c.Store.Put(ctx, store.UserCredential{
		ID:         store.NewID(now),
		OrgID:      orgID,
		UserID:     userID,
		Platform:   platform,
		Ciphertext: ct,
		Nonce:      nonce,
		KeyVersion: version,
		VerifiedAs: verified.As,
		Scopes:     verified.Scopes,
		ExpiresAt:  verified.ExpiresAt,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
}

// Delete removes a user's credential for a platform.
func (c *CredentialStore) Delete(ctx context.Context, orgID, userID, platform string) error {
	if c == nil || c.Store == nil {
		return nil
	}
	return c.Store.Delete(ctx, orgID, userID, platform)
}

// List describes what a user holds, without opening anything: the
// listing endpoint has no reason to decrypt, so it does not.
//
// @decision(DL-008)
func (c *CredentialStore) List(ctx context.Context, orgID, userID string) ([]CredentialView, error) {
	if c == nil || c.Store == nil {
		return nil, nil
	}
	recs, err := c.Store.ListByUser(ctx, orgID, userID)
	if err != nil {
		return nil, err
	}
	out := make([]CredentialView, 0, len(recs))
	for _, r := range recs {
		// The OpenCode sign-in has an endpoint of its own.
		//
		// @decision(DL-015)
		if isConsoleSlot(r.Platform) {
			continue
		}
		// A routed credential has an endpoint of its own too.
		//
		// @decision(DL-073)
		if routedcred.IsKey(r.Platform) {
			continue
		}
		out = append(out, CredentialView{
			Platform:   r.Platform,
			Present:    true,
			VerifiedAs: r.VerifiedAs,
			Scopes:     r.Scopes,
			ExpiresAt:  r.ExpiresAt,
			UpdatedAt:  r.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Platform < out[j].Platform })
	return out, nil
}

func (c *CredentialStore) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}
