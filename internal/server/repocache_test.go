package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
)

// tierCalls counts the forge reads behind Deps.UserRepos, by user and
// tier, and answers each with one repository named for both.
type tierCalls struct {
	mu    sync.Mutex
	calls map[string]int
	// err, when set, fails the next call and is cleared.
	err error
	// failed marks the next listing as one a forge failed on.
	failed bool
}

func (c *tierCalls) list(_ context.Context, orgID, userID string, tier Tier) (RepoListing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[orgID+"/"+userID+"/"+string(tier)]++
	if c.err != nil {
		err := c.err
		c.err = nil
		return RepoListing{}, err
	}
	l := RepoListing{Repos: []TieredRepo{{
		PlatformRepo: git.PlatformRepo{Platform: git.NewFake(), Repo: git.Repo{Owner: userID, Name: string(tier)}}, Tier: tier,
	}}, Failed: c.failed}
	c.failed = false
	return l, nil
}

func (c *tierCalls) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

func (c *tierCalls) of(org, user string, tier Tier) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[org+"/"+user+"/"+string(tier)]
}

// clock is a settable time for the cache.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// repoFixture is an API fixture whose repository listing reads through
// tierCalls, on a clock the test moves, with credentials enabled.
func repoFixture(t *testing.T) (*apiFixture, *tierCalls, *clock) {
	t.Helper()
	calls, clk := &tierCalls{}, &clock{t: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	f := credFixture(t, &fakeVerifier{as: "alice"})
	svc := f.srv.Service()
	svc.deps.UserRepos = calls.list
	f.srv.deps.UserRepos = calls.list
	svc.repos.now = clk.now
	return f, calls, clk
}

func (f *apiFixture) listRepos() (int, string) {
	f.t.Helper()
	res, body := f.do(http.MethodGet, "/api/v1/repos", nil)
	return res.StatusCode, string(body)
}

// TestTheRepositoryListingIsServedFromTheCacheForSixtySeconds: the forge
// is read once per tier, and again only after the minute.
//
// @decision(DL-134)
func TestTheRepositoryListingIsServedFromTheCacheForSixtySeconds(t *testing.T) {
	f, calls, clk := repoFixture(t)
	for i := range 5 {
		code, body := f.listRepos()
		if code != http.StatusOK || !strings.Contains(body, `"name":"user"`) || !strings.Contains(body, `"name":"org"`) {
			t.Fatalf("listing %d: %d %s", i, code, body)
		}
	}
	if n := calls.total(); n != 2 {
		t.Fatalf("%d forge reads for five listings, want one per tier", n)
	}
	clk.add(RepoListTTL - time.Second)
	f.listRepos()
	if n := calls.total(); n != 2 {
		t.Errorf("%d forge reads at 59 s", n)
	}
	clk.add(2 * time.Second)
	f.listRepos()
	if n := calls.total(); n != 4 {
		t.Errorf("%d forge reads after the minute, want 4", n)
	}
}

// TestAForgeErrorInTheRepositoryListingIsNotCached: the next request
// asks again, and a listing a forge failed on is served once.
//
// @decision(DL-134)
func TestAForgeErrorInTheRepositoryListingIsNotCached(t *testing.T) {
	f, calls, _ := repoFixture(t)
	calls.err = errors.New("forge down")
	if code, body := f.listRepos(); code != http.StatusBadGateway {
		t.Fatalf("a forge failure: %d %s", code, body)
	}
	before := calls.total()
	if code, body := f.listRepos(); code != http.StatusOK {
		t.Fatalf("after the failure: %d %s", code, body)
	}
	if n := calls.total() - before; n != 1 {
		t.Errorf("%d forge reads after a failure, want 1: the tier that failed asks again and the other is cached", n)
	}
	// A tier that failed on a platform is not kept, the other is.
	f2, calls2, _ := repoFixture(t)
	calls2.failed = true
	f2.listRepos()
	f2.listRepos()
	if n := calls2.total(); n != 3 {
		t.Errorf("%d forge reads for two listings, one tier of which failed: want 3", n)
	}
}

// TestTwoUsersAndTwoTiersDoNotShareARepositoryListing.
//
// @decision(DL-134)
func TestTwoUsersAndTwoTiersDoNotShareARepositoryListing(t *testing.T) {
	f, calls, _ := repoFixture(t)
	svc := f.srv.Service()
	ctx := context.Background()
	alice := auth.Principal{OrgID: "acme", UserID: "alice"}
	bob := auth.Principal{OrgID: "acme", UserID: "bob"}
	otherOrg := auth.Principal{OrgID: "other", UserID: "alice"}
	for _, p := range []auth.Principal{alice, bob, otherOrg, alice, bob, otherOrg} {
		repos, _, err := svc.tieredListing(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if len(repos) != 2 || repos[0].Repo.Owner != p.UserID || repos[0].Tier != TierUser || repos[1].Tier != TierOrg {
			t.Fatalf("%s's listing: %+v", p.UserID, repos)
		}
	}
	for _, p := range []auth.Principal{alice, bob, otherOrg} {
		for _, tier := range []Tier{TierUser, TierOrg} {
			if n := calls.of(p.OrgID, p.UserID, tier); n != 1 {
				t.Errorf("%s/%s through %s was read %d times, want once", p.OrgID, p.UserID, tier, n)
			}
		}
	}
}

// TestStoringOrRemovingACredentialDropsTheUsersListings: the next listing
// reads the forge, and another user's entries stay.
//
// @decision(DL-134)
func TestStoringOrRemovingACredentialDropsTheUsersListings(t *testing.T) {
	f, calls, _ := repoFixture(t)
	svc := f.srv.Service()
	bob := auth.Principal{OrgID: auth.LocalID, UserID: "bob"}
	if _, _, err := svc.tieredListing(context.Background(), bob); err != nil {
		t.Fatal(err)
	}
	f.listRepos()
	f.listRepos()
	if n := calls.of(auth.LocalID, auth.LocalID, TierUser); n != 1 {
		t.Fatalf("%d reads before the change", n)
	}
	if res, body := f.do(http.MethodPut, "/api/v1/me/credentials/github", map[string]string{"token": "github_pat_x"}); res.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %s", res.StatusCode, body)
	}
	f.listRepos()
	if n := calls.of(auth.LocalID, auth.LocalID, TierUser); n != 2 {
		t.Errorf("%d reads after storing a credential, want 2", n)
	}
	f.listRepos()
	if res, body := f.do(http.MethodDelete, "/api/v1/me/credentials/github", nil); res.StatusCode >= 300 {
		t.Fatalf("DELETE = %d %s", res.StatusCode, body)
	}
	f.listRepos()
	if n := calls.of(auth.LocalID, auth.LocalID, TierUser); n != 3 {
		t.Errorf("%d reads after removing it, want 3", n)
	}
	if _, _, err := svc.tieredListing(context.Background(), bob); err != nil {
		t.Fatal(err)
	}
	if n := calls.of(auth.LocalID, "bob", TierUser); n != 1 {
		t.Errorf("bob's listing was dropped by someone else's credential: %d reads", n)
	}
}

// TestTheOrganisationsAppChangeDropsTheOrganisationsListings.
//
// @decision(DL-134) @decision(DL-042)
func TestTheOrganisationsAppChangeDropsTheOrganisationsListings(t *testing.T) {
	f := orgFixture(t, true, &fakeOrgGitHub{})
	calls := &tierCalls{}
	svc := f.srv.Service()
	svc.deps.UserRepos = calls.list
	ctx := context.Background()
	mine := auth.Principal{OrgID: auth.LocalID, UserID: "alice"}
	theirs := auth.Principal{OrgID: "another", UserID: "alice"}
	warm := func() {
		for _, p := range []auth.Principal{mine, theirs} {
			if _, _, err := svc.tieredListing(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	warm()
	if res, raw := f.do(http.MethodPut, "/api/v1/org/github/gh", OrgGitHubApp{AppID: "123", PrivateKey: "-----BEGIN KEY-----secret", Account: "acme"}); res.StatusCode != http.StatusOK {
		t.Fatalf("store: %d %s", res.StatusCode, raw)
	}
	warm()
	if a, b := calls.of(auth.LocalID, "alice", TierOrg), calls.of("another", "alice", TierOrg); a != 2 || b != 1 {
		t.Fatalf("after storing the App: own org read %d times, another %d; want 2 and 1", a, b)
	}
	if res, raw := f.do(http.MethodDelete, "/api/v1/org/github/gh", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("remove: %d %s", res.StatusCode, raw)
	}
	warm()
	if a, b := calls.of(auth.LocalID, "alice", TierOrg), calls.of("another", "alice", TierOrg); a != 3 || b != 1 {
		t.Errorf("after removing it: own org read %d times, another %d; want 3 and 1", a, b)
	}
}

// TestThePlainRepositoryListingIsCachedPerResolvedTiers: the listing
// through the forge a request resolved to is kept too, and a warning
// keeps it out.
//
// @decision(DL-134)
func TestThePlainRepositoryListingIsCachedPerResolvedTiers(t *testing.T) {
	var tiers = []PlatformTier{{Platform: "gh", Tier: TierUser}}
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.UserForge = func(context.Context, string, string, TierPolicy) (Forge, []PlatformTier, error) {
			return nil, tiers, nil
		}
	})
	clk := &clock{t: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	f.srv.Service().repos.now = clk.now
	svc := f.srv.Service()
	ctx := context.Background()
	p := auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}
	counting := &countingListForge{Forge: f.forge}
	for range 3 {
		if _, _, err := svc.plainListing(ctx, p, counting, tiers); err != nil {
			t.Fatal(err)
		}
	}
	if counting.n != 1 {
		t.Errorf("%d forge reads for three listings", counting.n)
	}
	if _, _, err := svc.plainListing(ctx, p, counting, []PlatformTier{{Platform: "gh", Tier: TierOrg}}); err != nil {
		t.Fatal(err)
	}
	if counting.n != 2 {
		t.Errorf("another tier shared the entry: %d reads", counting.n)
	}
	clk.add(RepoListTTL)
	if _, _, err := svc.plainListing(ctx, p, counting, tiers); err != nil || counting.n != 3 {
		t.Errorf("after the minute: %d reads, %v", counting.n, err)
	}
	counting.warn = []string{"gh: rate limited"}
	counting.n = 0
	for range 2 {
		if _, _, err := svc.plainListing(ctx, p, counting, []PlatformTier{{Platform: "cf", Tier: TierUser}}); err != nil {
			t.Fatal(err)
		}
	}
	if counting.n != 2 {
		t.Errorf("a listing with a warning was kept: %d reads", counting.n)
	}
	counting.err = errors.New("forge down")
	if _, _, err := svc.plainListing(ctx, p, counting, []PlatformTier{{Platform: "xx", Tier: TierUser}}); err == nil {
		t.Error("the forge's error was swallowed")
	}
	counting.err = nil
	if _, _, err := svc.plainListing(ctx, p, counting, []PlatformTier{{Platform: "xx", Tier: TierUser}}); err != nil {
		t.Errorf("the error was served from the cache: %v", err)
	}
}

type countingListForge struct {
	Forge
	n    int
	warn []string
	err  error
}

func (c *countingListForge) ListRepos(ctx context.Context) ([]git.PlatformRepo, []string, error) {
	c.n++
	if c.err != nil {
		return nil, nil, c.err
	}
	repos, _, err := c.Forge.ListRepos(ctx)
	return repos, c.warn, err
}

// TestTheRepositoryCacheIsBounded: past its size the expired entries go,
// then the oldest.
//
// @decision(DL-134)
func TestTheRepositoryCacheIsBounded(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	c := newRepoCache(clk.now)
	c.max = 3
	key := func(u string) repoKey { return repoKey{org: "o", user: u, tier: "user"} }
	for _, u := range []string{"a", "b", "c"} {
		c.put(key(u), repoEntry{})
		clk.add(time.Second)
	}
	c.put(key("d"), repoEntry{})
	if len(c.entries) != 3 {
		t.Fatalf("%d entries, want 3", len(c.entries))
	}
	if _, ok := c.get(key("a")); ok {
		t.Error("the oldest entry should have been evicted")
	}
	clk.add(RepoListTTL)
	c.put(key("e"), repoEntry{})
	if len(c.entries) != 1 {
		t.Errorf("%d entries after the others expired, want 1", len(c.entries))
	}
}
