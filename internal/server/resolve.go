package server

import (
	"context"
	"errors"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/store"
)

// forgeFor resolves the repositories one user can reach, using their
// own credentials where they have them and the deployment's where they
// do not.
//
// The policy says which tier each platform may come from: a launch's
// preference, or what it recorded (DL-042).
//
// @decision(DL-042)
func (s *Service) forgeFor(ctx context.Context, orgID, userID string, policy TierPolicy) (Forge, []PlatformTier, error) {
	if s.deps.UserForge == nil || userID == "" {
		return s.deps.Forge, nil, nil
	}
	forge, tiers, err := s.deps.UserForge(ctx, orgID, userID, policy)
	if err != nil {
		return nil, nil, err
	}
	if forge == nil {
		return s.deps.Forge, tiers, nil
	}
	return forge, tiers, nil
}

// providerKeysFor resolves the model provider keys for one user, the
// user's own overriding the deployment's per variable.
//
// @decision(DL-042) @decision(DL-036)
func (s *Service) providerKeysFor(ctx context.Context, orgID, userID string) (map[string]string, error) {
	keys, err := s.storedProviderKeys(ctx, orgID, userID)
	if err != nil {
		return nil, err
	}
	// A usable OpenCode sign-in is the user's OPENCODE_API_KEY, ahead of
	// a stored or deployment key. A launch overlays a refreshed one
	// itself.
	//
	// @decision(DL-015)
	token, _ := s.consoleKey(ctx, orgID, userID)
	return withConsoleKey(keys, token), nil
}

// storedProviderKeys are the user's own keys over the deployment's,
// without the OpenCode sign-in.
//
// @decision(DL-042)
func (s *Service) storedProviderKeys(ctx context.Context, orgID, userID string) (map[string]string, error) {
	if s.deps.UserProviderKeys == nil || userID == "" {
		return s.deps.ProviderKeys, nil
	}
	return s.deps.UserProviderKeys(ctx, orgID, userID)
}

// profileFor reads the caller's stored profile, or the zero User when
// there is none. A miss is silent, since a principal with no row is
// ordinary; anything else is warned about. It is one read for two
// facts, called once per create.
//
// @decision(DL-014) @decision(DL-009)
func (s *Service) profileFor(ctx context.Context, orgID, userID string) store.User {
	if userID == "" || s.deps.Store == nil {
		return store.User{}
	}
	u, err := s.deps.Store.Users().Get(ctx, orgID, userID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.deps.Warn("profile for " + userID + ": " + err.Error())
		}
		return store.User{}
	}
	return u
}

// userIdentity is the git author from the launching user's profile. A
// store that cannot answer is not a launch failure: the identity falls
// through to the server's configured one.
//
// @decision(DL-044)
func (s *Service) userIdentity(ctx context.Context, orgID, userID string) git.Identity {
	if userID == "" || s.deps.Store == nil {
		return git.Identity{}
	}
	u, err := s.deps.Store.Users().Get(ctx, orgID, userID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.deps.Warn("git identity for " + userID + ": " + err.Error())
		}
		return git.Identity{}
	}
	return git.Identity{Name: u.GitName, Email: u.GitEmail}
}

// MergeProviderKeys layers a user's own provider keys over the
// deployment's, per variable.
//
// @decision(DL-042)
func MergeProviderKeys(org, user map[string]string) map[string]string {
	merged := make(map[string]string, len(org)+len(user))
	for k, v := range org {
		merged[k] = v
	}
	for k, v := range user {
		if v != "" {
			merged[k] = v
		}
	}
	return merged
}
