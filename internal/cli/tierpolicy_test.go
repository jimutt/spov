package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// A platform that prefers the shared tier takes it when it has a
// credential, and falls back to the user's only when it has none.
//
// @decision(DL-042)
func TestASharedPreferenceFallsBackOnlyWhenAbsent(t *testing.T) {
	h, creds := testHosted(t, map[string]string{"SPOV_PLATFORM_TOKEN_CF": "org-token"})
	ctx := context.Background()
	if err := creds.Put(ctx, "default", "u1", server.ForgejoPlatform("codefloe.com"), "user-token", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	prefer := server.TierPolicy{Prefer: map[string]server.Tier{"cf": server.TierOrg}}
	if token, tier, _ := h.tokenFor(ctx, "default", "u1", h.cfg.Platforms[1], prefer); tier != server.TierOrg || token != "org-token" {
		t.Errorf("preferred shared: %q %q", tier, token)
	}
	bare, _ := testHosted(t, nil)
	if err := bare.store.Put(ctx, "default", "u1", server.ForgejoPlatform("codefloe.com"), "user-token", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	if token, tier, _ := bare.tokenFor(ctx, "default", "u1", bare.cfg.Platforms[1], prefer); tier != server.TierUser || token != "user-token" {
		t.Errorf("no shared credential: %q %q, want the user's", tier, token)
	}
}

// A recorded tier is the only one used: a gone credential is no
// credential, never the other tier's.
//
// @decision(DL-042)
func TestARecordedTierIsTheOnlyOne(t *testing.T) {
	h, creds := testHosted(t, map[string]string{"SPOV_PLATFORM_TOKEN_CF": "org-token"})
	ctx := context.Background()
	only := func(t server.Tier) server.TierPolicy {
		return server.TierPolicy{Only: map[string]server.Tier{"cf": t}}
	}
	if _, tier, reason := h.tokenFor(ctx, "default", "u1", h.cfg.Platforms[1], only(server.TierUser)); tier != server.TierNone ||
		!strings.Contains(reason, "launched with is gone") {
		t.Errorf("a gone personal credential: %q %q", tier, reason)
	}
	if err := creds.Put(ctx, "default", "u1", server.ForgejoPlatform("codefloe.com"), "user-token", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	if token, tier, _ := h.tokenFor(ctx, "default", "u1", h.cfg.Platforms[1], only(server.TierOrg)); tier != server.TierOrg || token != "org-token" {
		t.Errorf("recorded org: %q %q", tier, token)
	}
	if token, tier, _ := h.tokenFor(ctx, "default", "u1", h.cfg.Platforms[1], only(server.TierUser)); tier != server.TierUser || token != "user-token" {
		t.Errorf("recorded user: %q %q", tier, token)
	}
}

// A GitHub platform the environment sets nothing for reads the App an
// admin stored; the environment wins when it sets one, and the admin
// routes see which.
//
// @decision(DL-042)
func TestAStoredAppIsTheOrgTierUnlessTheEnvironmentSetsOne(t *testing.T) {
	h, creds := testHosted(t, nil)
	ctx := context.Background()
	h.apps = &server.OrgApps{Store: store.NewMemory(), Sealer: sealerOf(t, creds)}
	gh := h.cfg.Platforms[0]
	if _, tier, _ := h.tokenFor(ctx, "default", "u1", gh, server.TierPolicy{}); tier != server.TierNone {
		t.Fatalf("no App anywhere: %q", tier)
	}
	row, err := h.apps.Sealed("default", "gh", "admin@x", git.GitHubCredential{AppID: "7", PrivateKey: "pem", InstallationID: 9},
		server.OrgAppMeta{AppID: "7", Account: "acme", InstallationID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.apps.Store.OrgCredentials().Put(ctx, row); err != nil {
		t.Fatal(err)
	}
	// The writer drops its cached miss, as the admin route does.
	h.apps.Forget("default", "gh")
	token, tier, _ := h.tokenFor(ctx, "default", "u1", gh, server.TierPolicy{})
	var cred git.GitHubCredential
	_ = json.Unmarshal([]byte(token), &cred)
	if tier != server.TierOrg || cred.AppID != "7" || cred.InstallationID != 9 {
		t.Errorf("stored App: %q %+v", tier, cred)
	}
	if plats := (orgGitHub{h: h}).Platforms(); len(plats) != 1 || plats[0].FromEnv {
		t.Errorf("platforms = %+v", plats)
	}

	env, _ := testHosted(t, map[string]string{"SPOV_GITHUB_APP_ID": "1", "SPOV_GITHUB_APP_PRIVATE_KEY": "envpem"})
	env.apps = h.apps
	token, _, _ = env.tokenFor(ctx, "default", "u1", env.cfg.Platforms[0], server.TierPolicy{})
	_ = json.Unmarshal([]byte(token), &cred)
	if cred.AppID != "1" {
		t.Errorf("the environment's App lost to the stored one: %+v", cred)
	}
	if plats := (orgGitHub{h: env}).Platforms(); len(plats) != 1 || !plats[0].FromEnv || plats[0].Env.AppID != "1" {
		t.Errorf("platforms = %+v", plats)
	}
	// A stored row that cannot be opened ends the resolution.
	other, _ := testHosted(t, nil)
	other.apps = &server.OrgApps{Store: h.apps.Store, Sealer: otherSealer(t)}
	if _, tier, reason := other.tokenFor(ctx, "default", "u1", other.cfg.Platforms[0], server.TierPolicy{Prefer: map[string]server.Tier{"gh": server.TierOrg}}); tier != server.TierNone ||
		!strings.Contains(reason, "could not be opened") {
		t.Errorf("unreadable: %q %q", tier, reason)
	}
}

// The config's preference reaches a request as the shared tier, for
// the platforms that name it.
//
// @decision(DL-042)
func TestTheConfigsPreferenceIsSent(t *testing.T) {
	cfg := config.Config{Platforms: []config.Platform{
		{Name: "gh", Kind: config.KindGitHub, Prefer: config.PreferShared},
		{Name: "cf", Kind: config.KindForgejo, Prefer: config.PreferPersonal},
	}}
	if got := credentialPreference(cfg); len(got) != 1 || got["gh"] != "org" {
		t.Errorf("preference = %v", got)
	}
	if credentialPreference(config.Config{}) != nil {
		t.Error("no preference sent something")
	}
}

func sealerOf(t *testing.T, c *server.CredentialStore) *auth.Sealer {
	t.Helper()
	return c.Sealer
}

func otherSealer(t *testing.T) *auth.Sealer {
	t.Helper()
	s, err := auth.NewSealer(map[int]string{1: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Each tier that holds a credential lists its own repositories, each
// through its own token; a tier with none lists nothing.
//
// @decision(DL-042)
func TestEachTierListsItsOwnRepositories(t *testing.T) {
	h, creds := testHosted(t, map[string]string{"SPOV_PLATFORM_TOKEN_CF": "org-token"})
	ctx := context.Background()
	if err := creds.Put(ctx, "default", "u1", server.ForgejoPlatform("codefloe.com"), "user-token", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	h.env.NewPlatform = func(_ context.Context, p config.Platform, token string) (git.Platform, error) {
		f := git.NewFake()
		f.PlatformName = p.Name
		f.Repos = []git.Repo{{Owner: "acme", Name: "shared"}}
		if token == "user-token" {
			f.Repos = append(f.Repos, git.Repo{Owner: "me", Name: "mine"})
		}
		return f, nil
	}
	got, warnings, err := bothTiers(t, h, "default", "u1")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range got {
		lines = append(lines, string(r.Tier)+":"+r.Repo.Owner+"/"+r.Repo.Name)
	}
	if strings.Join(lines, ",") != "user:acme/shared,user:me/mine,org:acme/shared" {
		t.Errorf("listed %v (warnings %v)", lines, warnings)
	}
}

// The CLI's picker keeps what the tier its launch resolves to reaches,
// and says how to reach the rest.
//
// @decision(DL-042)
func TestTheCLIPickerKeepsWhatItsLaunchReaches(t *testing.T) {
	repos := []server.RepoView{
		{Platform: "gh", Owner: "o", Name: "both", Via: []server.Tier{server.TierUser, server.TierOrg}},
		{Platform: "gh", Owner: "o", Name: "shared", Via: []server.Tier{server.TierOrg}},
		{Platform: "gh", Owner: "o", Name: "mine", Via: []server.Tier{server.TierUser}},
		{Platform: "cf", Owner: "o", Name: "local"},
	}
	names := func(rs []server.RepoView) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return strings.Join(out, ",")
	}
	kept, notes := reachableFromHere(repos, []server.PlatformTier{{Platform: "gh", Tier: server.TierUser}})
	if names(kept) != "both,mine,local" || len(notes) != 1 || !strings.Contains(notes[0], "set prefer: shared for gh") {
		t.Errorf("personal: %s %v", names(kept), notes)
	}
	kept, notes = reachableFromHere(repos, []server.PlatformTier{{Platform: "gh", Tier: server.TierOrg}})
	if names(kept) != "both,shared,local" || len(notes) != 1 || !strings.Contains(notes[0], "remove prefer: shared") {
		t.Errorf("shared: %s %v", names(kept), notes)
	}
	kept, notes = reachableFromHere(repos, []server.PlatformTier{{Platform: "gh", Tier: server.TierNone, Reason: "broken"}})
	if names(kept) != "local" || len(notes) != 1 || !strings.Contains(notes[0], "broken") {
		t.Errorf("none: %s %v", names(kept), notes)
	}
}

// A fine-grained token's public repositories are marked as perhaps
// read-only, its private ones and every other credential's are not, and
// the CLI's picker keeps the mark only for a launch through that token.
//
// @decision(DL-042)
func TestAFineGrainedTokensPublicRepositoriesAreMarked(t *testing.T) {
	h, creds := testHosted(t, map[string]string{"SPOV_PLATFORM_TOKEN_GH": "github_pat_org-token"})
	ctx := context.Background()
	if err := creds.Put(ctx, "default", "u1", "github", "github_pat_user-token", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	h.env.NewPlatform = func(_ context.Context, p config.Platform, _ string) (git.Platform, error) {
		f := git.NewFake()
		f.PlatformName = p.Name
		f.Repos = []git.Repo{{Owner: "me", Name: "granted", Private: true}, {Owner: "me", Name: "public"}}
		return f, nil
	}
	got, _, err := bothTiers(t, h, "default", "u1")
	if err != nil {
		t.Fatal(err)
	}
	var marked []string
	for _, r := range got {
		if r.MaybeReadOnly {
			marked = append(marked, string(r.Tier)+":"+r.Repo.Name)
		}
	}
	if strings.Join(marked, ",") != "user:public" {
		t.Errorf("marked %v; want only the personal token's public repository", marked)
	}
	if !git.IsFineGrainedGitHubPAT(" github_pat_x") || git.IsFineGrainedGitHubPAT("ghp_x") {
		t.Error("the fine-grained prefix is not what tells them apart")
	}

	views := []server.RepoView{{Platform: "gh", Name: "public", Via: []server.Tier{server.TierUser, server.TierOrg}, MaybeReadOnly: true}}
	if kept, _ := reachableFromHere(views, []server.PlatformTier{{Platform: "gh", Tier: server.TierUser}}); !kept[0].MaybeReadOnly {
		t.Error("a launch through the token lost the mark")
	}
	if kept, _ := reachableFromHere(views, []server.PlatformTier{{Platform: "gh", Tier: server.TierOrg}}); kept[0].MaybeReadOnly {
		t.Error("a launch through the organisation's credential kept the token's mark")
	}
	if d := repoDetail(RepoChoice{Repo: git.Repo{Name: "public"}, MaybeReadOnly: true}, time.Now()); !strings.Contains(d, "push only if your token was given it") {
		t.Errorf("detail %q", d)
	}
}
