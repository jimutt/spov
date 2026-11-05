package server

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// resolveGitAccess picks the git access a launch asks for: the request's
// own value, then the caller's stored default, then repos. The bool says
// the value is a preference, which falls back where all cannot apply
// rather than refusing the launch.
//
// @decision(DL-043)
func resolveGitAccess(requested store.GitAccess, preferred bool, stored store.GitAccess) (store.GitAccess, bool) {
	if requested != "" {
		return requested, preferred
	}
	if stored != "" {
		return stored, true
	}
	return store.GitAccessRepos, true
}

// mayReachAll reports whether a platform's credential may be used with
// all: a user's own always may, the deployment's only where it allows
// it. A platform the tiers do not name has the deployment's credential.
// Local mode sets GitAllowAll, since every credential there is the
// user's.
//
// @decision(DL-043) @decision(DL-042)
func (s *Service) mayReachAll(tiers []PlatformTier, platform string) bool {
	if s.deps.GitAllowAll {
		return true
	}
	for _, t := range tiers {
		if t.Platform == platform {
			return t.Tier == TierUser
		}
	}
	return false
}

// deniedAll names the platforms, of those the caller holds a credential
// for, that may not have all.
//
// @decision(DL-043)
func (s *Service) deniedAll(tiers []PlatformTier, platforms []git.Platform) []string {
	var denied []string
	for _, p := range platforms {
		if !s.mayReachAll(tiers, p.Name()) {
			denied = append(denied, p.Name())
		}
	}
	return denied
}

// gitAccessRefusal is why a launch that named all itself was refused.
//
// @decision(DL-043)
func gitAccessRefusal(denied []string) string {
	return fmt.Sprintf("git access all cannot apply to %s: the credential is this deployment's, which may reach "+
		"only the session's repositories; store your own token with `spov credentials add`, or ask an admin to set SPOV_GIT_ALLOW_ALL",
		strings.Join(denied, ", "))
}

// gitAccessFallback is the launch log's line for platforms that asked
// for all through a preference and got repos.
//
// @decision(DL-043)
func gitAccessFallback(denied []string) string {
	return fmt.Sprintf("git access: %s reach only this session's repositories, not all: the credential is this "+
		"deployment's, and it does not allow all", strings.Join(denied, ", "))
}

// refuseGitAccess refuses, before anything is created, a launch that
// named all itself when a platform the caller holds a credential for may
// not have it.
//
// @decision(DL-043)
func (s *Service) refuseGitAccess(ctx context.Context, orgID, userID string, req store.SessionRequest) error {
	if req.GitAccess != store.GitAccessAll || req.GitAccessPreferred || s.deps.GitAllowAll {
		return nil
	}
	forge, tiers, err := s.forgeFor(ctx, orgID, userID, policyFromRequest(req))
	if err != nil {
		return err
	}
	platforms, err := forge.Platforms(ctx)
	if err != nil {
		return err
	}
	if denied := s.deniedAll(tiers, platforms); len(denied) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, gitAccessRefusal(denied))
	}
	return nil
}

// grantSelections turns a launch's repositories into selections, widened
// when it asked for all: each platform that may have it is All, and one
// the caller holds a credential for but selected nothing from is added
// with no repositories. denied names the platforms that may not.
//
// @decision(DL-043)
func (s *Service) grantSelections(ctx context.Context, forge Forge, tiers []PlatformTier, req store.SessionRequest) ([]git.Selection, []string, error) {
	selections, err := forge.Selections(ctx, req.Repos)
	if err != nil || req.GitAccess != store.GitAccessAll {
		return selections, nil, err
	}
	platforms, err := forge.Platforms(ctx)
	if err != nil {
		return nil, nil, err
	}
	denied := s.deniedAll(tiers, platforms)
	all := map[string]bool{}
	for _, p := range platforms {
		all[p.Name()] = s.mayReachAll(tiers, p.Name())
	}
	return widen(selections, platforms, all), denied, nil
}

// recordedSelections are the selections a session's credentials are
// minted from after its launch: all for each platform its launch
// recorded with all and that may still have it, repos otherwise. A
// platform that may no longer have all is recorded as repos.
//
// @decision(DL-043) @decision(DL-021)
func (s *Service) recordedSelections(ctx context.Context, forge Forge, tiers []PlatformTier, sess store.Session) ([]git.Selection, error) {
	selections, err := forge.Selections(ctx, sess.Request.Repos)
	if err != nil {
		return nil, err
	}
	all := map[string]bool{}
	var narrowed []string
	for _, c := range sess.Credentials {
		switch {
		case c.Access != store.GitAccessAll:
		case s.mayReachAll(tiers, c.Platform):
			all[c.Platform] = true
		default:
			narrowed = append(narrowed, c.Platform)
		}
	}
	if len(narrowed) > 0 {
		s.recordNarrowed(ctx, sess, narrowed)
	}
	if len(all) == 0 {
		return selections, nil
	}
	platforms, err := forge.Platforms(ctx)
	if err != nil {
		return nil, err
	}
	return widen(selections, platforms, all), nil
}

// widen marks the selections of platforms in all as All, and adds a
// selection with no repositories for each such platform not selected.
//
// @decision(DL-043)
func widen(selections []git.Selection, platforms []git.Platform, all map[string]bool) []git.Selection {
	selected := map[string]bool{}
	for i, sel := range selections {
		if sel.Platform == nil {
			continue
		}
		name := sel.Platform.Name()
		selected[name] = true
		selections[i].All = all[name]
	}
	for _, p := range platforms {
		if all[p.Name()] && !selected[p.Name()] {
			selections = append(selections, git.Selection{Platform: p, All: true})
		}
	}
	return selections
}

// recordNarrowed records repos for platforms a session had all on, so
// the session page stops saying they reach every repository. A failure
// is warned about: the minting it accompanies is already narrow.
//
// @decision(DL-043)
func (s *Service) recordNarrowed(ctx context.Context, sess store.Session, platforms []string) {
	s.deps.Warn(fmt.Sprintf("session %s: git access all no longer applies to %s, whose credential is now the deployment's; "+
		"new tokens reach only the session's repositories", sess.ID, strings.Join(platforms, ", ")))
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		cur, err := tx.Sessions().Get(ctx, sess.OrgID, sess.ID)
		if err != nil {
			return err
		}
		for i, c := range cur.Credentials {
			if slices.Contains(platforms, c.Platform) {
				cur.Credentials[i].Access = store.GitAccessRepos
			}
		}
		return tx.Sessions().Update(ctx, cur)
	})
	if err != nil {
		s.deps.Warn(fmt.Sprintf("session %s: record the narrowed git access: %s", sess.ID, redact.Error(err)))
	}
}
