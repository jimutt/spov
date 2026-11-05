package server

import (
	"context"
	"errors"
	"testing"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
)

func testService(t *testing.T, adjust func(*Deps)) *Service {
	t.Helper()
	deps := Deps{Store: store.NewMemory(), Warn: func(string) {}}
	if adjust != nil {
		adjust(&deps)
	}
	return newService(deps.withDefaults())
}

// Without a hosted resolver every user gets the deployment's forge,
// which is what local mode is.
func TestForgeForFallsBackToTheOrgForge(t *testing.T) {
	org := &fakeForge{platform: git.NewFake()}
	s := testService(t, func(d *Deps) { d.Forge = org })
	forge, tiers, err := s.forgeFor(context.Background(), "org", "user", TierPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if forge != Forge(org) {
		t.Errorf("forgeFor = %v, want the org forge", forge)
	}
	if tiers != nil {
		t.Errorf("tiers = %v, want none", tiers)
	}
}

// A resolver that answers with no forge still falls back, so a
// deployment can report tiers without replacing the forge.
func TestForgeForUsesTheOrgForgeWhenTheResolverHasNone(t *testing.T) {
	org := &fakeForge{platform: git.NewFake()}
	s := testService(t, func(d *Deps) {
		d.Forge = org
		d.UserForge = func(context.Context, string, string, TierPolicy) (Forge, []PlatformTier, error) {
			return nil, []PlatformTier{{Platform: "gh", Tier: TierOrg}}, nil
		}
	})
	forge, tiers, err := s.forgeFor(context.Background(), "org", "user", TierPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if forge != Forge(org) {
		t.Error("a nil resolver result did not fall back to the org forge")
	}
	if len(tiers) != 1 {
		t.Errorf("tiers = %v", tiers)
	}
}

// An anonymous principal has no credentials of its own.
func TestForgeForWithNoUserIsTheOrgForge(t *testing.T) {
	org := &fakeForge{platform: git.NewFake()}
	called := false
	s := testService(t, func(d *Deps) {
		d.Forge = org
		d.UserForge = func(context.Context, string, string, TierPolicy) (Forge, []PlatformTier, error) {
			called = true
			return nil, nil, nil
		}
	})
	if _, _, err := s.forgeFor(context.Background(), "org", "", TierPolicy{}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("the per-user resolver was called for an anonymous principal")
	}
}

// A resolver that fails must fail the launch rather than silently
// falling back to the deployment's credentials.
func TestForgeForPropagatesTheResolverError(t *testing.T) {
	want := errors.New("the key is gone")
	s := testService(t, func(d *Deps) {
		d.Forge = &fakeForge{platform: git.NewFake()}
		d.UserForge = func(context.Context, string, string, TierPolicy) (Forge, []PlatformTier, error) {
			return nil, nil, want
		}
	})
	if _, _, err := s.forgeFor(context.Background(), "org", "user", TierPolicy{}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestProviderKeysForFallsBack(t *testing.T) {
	org := map[string]string{"ANTHROPIC_API_KEY": "org-a"}
	s := testService(t, func(d *Deps) { d.ProviderKeys = org })
	got, err := s.providerKeysFor(context.Background(), "org", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got["ANTHROPIC_API_KEY"] != "org-a" {
		t.Errorf("keys = %v", got)
	}
}

func TestProviderKeysForUsesTheResolver(t *testing.T) {
	s := testService(t, func(d *Deps) {
		d.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "org-a"}
		d.UserProviderKeys = func(context.Context, string, string) (map[string]string, error) {
			return map[string]string{"ANTHROPIC_API_KEY": "user-a"}, nil
		}
	})
	got, err := s.providerKeysFor(context.Background(), "org", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got["ANTHROPIC_API_KEY"] != "user-a" {
		t.Errorf("keys = %v, want the user's", got)
	}
}

func TestProviderKeysForPropagatesTheError(t *testing.T) {
	want := errors.New("unreadable")
	s := testService(t, func(d *Deps) {
		d.UserProviderKeys = func(context.Context, string, string) (map[string]string, error) {
			return nil, want
		}
	})
	if _, err := s.providerKeysFor(context.Background(), "org", "user"); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// The launching user's profile is what authors their commits.
func TestUserIdentityComesFromTheProfile(t *testing.T) {
	s := testService(t, nil)
	ctx := context.Background()
	if err := s.deps.Store.Users().Create(ctx, store.User{
		ID: "u1", OrgID: "org", Email: "me@example.com",
		GitName: "Alice", GitEmail: "alice@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	got := s.userIdentity(ctx, "org", "u1")
	if got.Name != "Alice" || got.Email != "alice@example.com" {
		t.Errorf("identity = %+v", got)
	}
}

// A user the store does not know is not a launch failure: the identity
// falls through to the server's configured one.
func TestUserIdentityOfAnUnknownUserIsEmpty(t *testing.T) {
	s := testService(t, nil)
	if got := s.userIdentity(context.Background(), "org", "nobody"); got != (git.Identity{}) {
		t.Errorf("identity = %+v, want empty", got)
	}
}

func TestUserIdentityWithNoUserIsEmpty(t *testing.T) {
	s := testService(t, nil)
	if got := s.userIdentity(context.Background(), "org", ""); got != (git.Identity{}) {
		t.Errorf("identity = %+v, want empty", got)
	}
}

// Gate 2's forge is the owner's, not the deployment's.
//
// @decision(DL-050)
func TestTheJobOwnersForgeIsTheOnesTheLaunchWouldUse(t *testing.T) {
	org := &fakeForge{platform: git.NewFake()}
	mine := &fakeForge{platform: git.NewFake()}
	var askedOrg, askedUser string
	s := testService(t, func(d *Deps) {
		d.Forge = org
		d.UserForge = func(_ context.Context, orgID, userID string, _ TierPolicy) (Forge, []PlatformTier, error) {
			askedOrg, askedUser = orgID, userID
			return mine, []PlatformTier{{Platform: "codefloe", Tier: TierUser}}, nil
		}
	})
	got, err := s.JobOwnerForge(context.Background(), store.Session{OrgID: "org", OwnerID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if got != jobs.Forge(mine) {
		t.Error("gate 2 would query with the deployment's forge rather than the owner's")
	}
	if askedOrg != "org" || askedUser != "user" {
		t.Errorf("resolved for %q/%q, want the job owner", askedOrg, askedUser)
	}
}

// A deployment with no per-user resolver keeps the deployment's forge,
// and the fix must not cost it gate 2. NOT local mode, which is the
// tempting example and the wrong one; see DL-050.
//
// @decision(DL-050)
func TestTheJobOwnersForgeFallsBackToTheDeployments(t *testing.T) {
	org := &fakeForge{platform: git.NewFake()}
	s := testService(t, func(d *Deps) { d.Forge = org })
	got, err := s.JobOwnerForge(context.Background(), store.Session{OrgID: "org", OwnerID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if got != jobs.Forge(org) {
		t.Errorf("JobOwnerForge = %v, want the deployment's forge in local mode", got)
	}
}

// A deployment with no forge at all answers nil rather than an error:
// gate 2 reads a nil forge as ErrUnsupported and stays quiet, and
// warning here would be noise once per job forever.
//
// @decision(DL-050)
func TestTheJobOwnersForgeIsNilWhenThereIsNone(t *testing.T) {
	s := testService(t, nil)
	got, err := s.JobOwnerForge(context.Background(), store.Session{OrgID: "org", OwnerID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("JobOwnerForge = %v, want nil so gate 2 reports it cannot answer", got)
	}
}

// A resolver that broke is spov's own failure and must reach gate 2 as
// an error, which warns and reports unverified. Swallowing it would
// look identical to a deployment with no forge.
//
// @decision(DL-050)
func TestTheJobOwnersForgePropagatesAResolverFailure(t *testing.T) {
	want := errors.New("the credential store is unreachable")
	s := testService(t, func(d *Deps) {
		d.Forge = &fakeForge{platform: git.NewFake()}
		d.UserForge = func(context.Context, string, string, TierPolicy) (Forge, []PlatformTier, error) {
			return nil, nil, want
		}
	})
	if _, err := s.JobOwnerForge(context.Background(), store.Session{OrgID: "org", OwnerID: "user"}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
