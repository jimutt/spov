package server

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
)

// TestRemovingACredentialThatIsNotThereDropsNoListing is DL-134: a
// delete that found nothing changes nothing, so a loop of them and of
// listings reads the forge, the organisation's App included, once.
func TestRemovingACredentialThatIsNotThereDropsNoListing(t *testing.T) {
	f, calls, _ := repoFixture(t)
	for range 11 {
		if res, body := f.do(http.MethodDelete, "/api/v1/me/credentials/gitlab:gitlab.example.com", nil); res.StatusCode >= 300 {
			t.Fatalf("DELETE = %d %s", res.StatusCode, body)
		}
		if code, body := f.listRepos(); code != http.StatusOK {
			t.Fatalf("listing: %d %s", code, body)
		}
	}
	for _, tier := range []Tier{TierUser, TierOrg} {
		if n := calls.of(auth.LocalID, auth.LocalID, tier); n != 1 {
			t.Errorf("the %s tier was read %d times, want once", tier, n)
		}
	}
}

// TestAUsersCredentialChangeKeepsTheOrganisationTiersListing is DL-134:
// storing or removing a credential of one's own drops one's own tier and
// plain listings, never the organisation tier's, which it cannot change.
func TestAUsersCredentialChangeKeepsTheOrganisationTiersListing(t *testing.T) {
	f, calls, _ := repoFixture(t)
	f.listRepos()
	if res, body := f.do(http.MethodPut, "/api/v1/me/credentials/github", map[string]string{"token": "github_pat_x"}); res.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %s", res.StatusCode, body)
	}
	f.listRepos()
	if res, body := f.do(http.MethodDelete, "/api/v1/me/credentials/github", nil); res.StatusCode >= 300 {
		t.Fatalf("DELETE = %d %s", res.StatusCode, body)
	}
	f.listRepos()
	if n := calls.of(auth.LocalID, auth.LocalID, TierUser); n != 3 {
		t.Errorf("own tier read %d times, want 3", n)
	}
	if n := calls.of(auth.LocalID, auth.LocalID, TierOrg); n != 1 {
		t.Errorf("organisation tier read %d times, want once", n)
	}
	// Off-boarding drops all of it.
	f.srv.Service().ForgetRepoListings(auth.LocalID, auth.LocalID)
	f.listRepos()
	if n := calls.of(auth.LocalID, auth.LocalID, TierOrg); n != 2 {
		t.Errorf("organisation tier read %d times after an off-boarding's drop, want 2", n)
	}
}

// slowTiers answers each listing after a pause, so concurrent callers
// overlap.
type slowTiers struct {
	tierCalls
	wait time.Duration
}

func (s *slowTiers) list(ctx context.Context, orgID, userID string, tier Tier) (RepoListing, error) {
	time.Sleep(s.wait)
	return s.tierCalls.list(ctx, orgID, userID, tier)
}

// TestConcurrentListingsOfAColdEntryReadTheForgeOnce is DL-134: misses
// of one key at the same time share one read.
func TestConcurrentListingsOfAColdEntryReadTheForgeOnce(t *testing.T) {
	f, _, _ := repoFixture(t)
	slow := &slowTiers{wait: 100 * time.Millisecond}
	svc := f.srv.Service()
	svc.deps.UserRepos = slow.list
	p := auth.Principal{OrgID: "acme", UserID: "alice"}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if repos, _, err := svc.tieredListing(context.Background(), p); err != nil || len(repos) != 2 {
				t.Errorf("listing: %d repositories, %v", len(repos), err)
			}
		})
	}
	wg.Wait()
	for _, tier := range []Tier{TierUser, TierOrg} {
		if n := slow.of("acme", "alice", tier); n != 1 {
			t.Errorf("20 concurrent listings read the %s tier %d times, want once", tier, n)
		}
	}
}

// TestACallerThatHangsUpLeavesTheSharedReadToTheOthers is DL-134: one
// caller's cancelled context ends its own wait, not the read the others
// share.
func TestACallerThatHangsUpLeavesTheSharedReadToTheOthers(t *testing.T) {
	f, _, _ := repoFixture(t)
	slow := &slowTiers{wait: 100 * time.Millisecond}
	svc := f.srv.Service()
	svc.deps.UserRepos = slow.list
	p := auth.Principal{OrgID: "acme", UserID: "alice"}
	gone, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := svc.tieredListing(gone, p)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Error("a cancelled caller kept waiting")
	}
	if repos, _, err := svc.tieredListing(context.Background(), p); err != nil || len(repos) != 2 {
		t.Errorf("the next caller: %d repositories, %v", len(repos), err)
	}
	for _, tier := range []Tier{TierUser, TierOrg} {
		if n := slow.of("acme", "alice", tier); n != 1 {
			t.Errorf("the %s tier was read %d times, want once", tier, n)
		}
	}
}

// TestALocalServerListsRepositoriesAfreshEveryTime is DL-134: a local
// server's CLI writes credentials to the keyring without telling it, so
// its listing is never cached.
func TestALocalServerListsRepositoriesAfreshEveryTime(t *testing.T) {
	var tiers = []PlatformTier{{Platform: "gh", Tier: TierUser}}
	f := newAPIFixtureWith(t, func(d *Deps) { d.Local = true })
	svc := f.srv.Service()
	counting := &countingListForge{Forge: f.forge}
	p := auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}
	for range 3 {
		if _, _, err := svc.plainListing(context.Background(), p, counting, tiers); err != nil {
			t.Fatal(err)
		}
	}
	if counting.n != 3 {
		t.Errorf("%d forge reads for three listings, want three", counting.n)
	}
	if len(svc.repos.entries) != 0 {
		t.Errorf("a local server kept %d listings", len(svc.repos.entries))
	}
}
