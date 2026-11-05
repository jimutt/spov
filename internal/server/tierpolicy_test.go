package server

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/store"
)

// A request's preference becomes Prefer; a session's recorded tiers
// become Only, over its request's preference; anything else is ignored.
//
// @decision(DL-042)
func TestTheTierPolicyFollowsTheRecord(t *testing.T) {
	req := store.SessionRequest{CredentialPreference: map[string]string{"gh": "org", "cf": "nonsense"}}
	if p := policyFromRequest(req); len(p.Prefer) != 1 || p.Prefer["gh"] != TierOrg || p.Only != nil {
		t.Errorf("from the request: %+v", p)
	}
	sess := store.Session{Request: req, Credentials: []store.SessionCredential{
		{Platform: "gh", Tier: "user"}, {Platform: "cf"},
	}}
	if p := policyOf(sess); p.Only["gh"] != TierUser || len(p.Only) != 1 || p.Prefer["gh"] != TierOrg {
		t.Errorf("from the session: %+v", p)
	}
	creds := withTiers([]store.SessionCredential{{Platform: "gh"}, {Platform: "cf"}},
		[]PlatformTier{{Platform: "gh", Tier: TierOrg}, {Platform: "cf", Tier: TierNone}})
	if creds[0].Tier != "org" || creds[1].Tier != "" {
		t.Errorf("recorded %+v", creds)
	}
}

// A launch resolves with the request's preference and records the tier
// that answered on the session; gate 2 then resolves only that tier.
//
// @decision(DL-042)
func TestALaunchRecordsItsTierAndGateTwoFollowsIt(t *testing.T) {
	var mu sync.Mutex
	var policies []TierPolicy
	f := newAPIFixtureWith(t, func(d *Deps) {
		forge := d.Forge
		d.UserForge = func(_ context.Context, _, _ string, p TierPolicy) (Forge, []PlatformTier, error) {
			mu.Lock()
			policies = append(policies, p)
			mu.Unlock()
			return forge, []PlatformTier{{Platform: "fake", Tier: TierOrg}}, nil
		}
	})
	resp, body := f.do(http.MethodPost, "/api/v1/sessions", SessionRequest{
		Repos:                []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}},
		CredentialPreference: map[string]string{"fake": "org"},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var sess store.Session
	waitFor(t, "the launch to record its credentials", func() bool {
		all, _ := f.store.Sessions().List(context.Background(), "local", store.SessionFilter{})
		if len(all) == 1 && len(all[0].Credentials) > 0 {
			sess = all[0]
			return true
		}
		return false
	})
	if sess.Credentials[0].Tier != "org" {
		t.Errorf("recorded %+v", sess.Credentials)
	}
	mu.Lock()
	first := policies[len(policies)-1]
	mu.Unlock()
	if first.Prefer["fake"] != TierOrg {
		t.Errorf("the launch resolved with %+v", first)
	}
	if _, err := f.srv.svc.JobOwnerForge(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	last := policies[len(policies)-1]
	mu.Unlock()
	if last.Only["fake"] != TierOrg {
		t.Errorf("gate 2 resolved with %+v, want only the recorded tier", last)
	}
}

// One row per repository, naming every tier that reaches it.
//
// @decision(DL-042)
func TestTheListingMergesTheTiers(t *testing.T) {
	plat := git.NewFake()
	plat.PlatformName = "gh"
	row := func(name string, tier Tier) TieredRepo {
		return TieredRepo{PlatformRepo: git.PlatformRepo{Platform: plat, Repo: git.Repo{Owner: "acme", Name: name}}, Tier: tier}
	}
	readOnly := row("a", TierUser)
	readOnly.MaybeReadOnly = true
	got := mergeTiers([]TieredRepo{readOnly, row("b", TierOrg), row("a", TierOrg), row("a", TierOrg)})
	if len(got) != 2 || len(got[0].Via) != 2 || got[0].Via[0] != TierUser || got[0].Via[1] != TierOrg || got[1].Via[0] != TierOrg {
		t.Errorf("merged %+v", got)
	}
	// The personal token's mark survives the merge with the App's row
	// (DL-042).
	if !got[0].MaybeReadOnly || got[1].MaybeReadOnly {
		t.Errorf("read-only marks %v %v", got[0].MaybeReadOnly, got[1].MaybeReadOnly)
	}
}
