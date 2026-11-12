package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/auth"
)

// RepoListTTL is how long a user's repository listing is served from
// memory, per tier, before the forge is asked again.
//
// @decision(DL-134)
const RepoListTTL = 60 * time.Second

// RepoListMaxEntries bounds the cache. Past it the expired entries go,
// then the oldest.
//
// @decision(DL-134)
const RepoListMaxEntries = 1024

// RepoListing is what one tier's credentials reach.
//
// @decision(DL-042) @decision(DL-134)
type RepoListing struct {
	Repos    []TieredRepo
	Warnings []string
	// Absent names each platform the tier holds no credential for, with
	// how to add one. The listing says so only for a platform neither
	// tier has.
	Absent []AbsentPlatform
	// Failed is set when a credential could not be read or a forge failed
	// for any platform. A failed listing is served once and never kept.
	Failed bool
}

// AbsentPlatform is a platform a tier has no credential for.
type AbsentPlatform struct {
	Name string
	Hint string
}

// CombineRepoListings joins the tiers' listings, as the browser and the
// CLI show them: every tier's repositories, each tier's warnings, and a
// warning for each platform none of them has a credential for.
//
// @decision(DL-042) @decision(DL-134)
func CombineRepoListings(listings ...RepoListing) ([]TieredRepo, []string) {
	var (
		repos    []TieredRepo
		warnings []string
	)
	for _, l := range listings {
		repos = append(repos, l.Repos...)
		warnings = append(warnings, l.Warnings...)
	}
	if len(listings) == 0 {
		return repos, warnings
	}
	for _, a := range listings[0].Absent {
		missing := true
		for _, l := range listings[1:] {
			if !l.absent(a.Name) {
				missing = false
			}
		}
		if missing {
			warnings = append(warnings, fmt.Sprintf("no credential reaches %s: %s", a.Name, a.Hint))
		}
	}
	return repos, warnings
}

func (l RepoListing) absent(name string) bool {
	for _, a := range l.Absent {
		if a.Name == name {
			return true
		}
	}
	return false
}

// repoKey names one cached listing: a user's, and the tier it was read
// through. A listing through the forge a request resolved to (local mode,
// or no tiered listing configured) is keyed by the tiers that resolution
// chose.
type repoKey struct {
	org, user, tier string
}

type repoEntry struct {
	at      time.Time
	listing RepoListing
	plain   []RepoView
	// warnings are the plain listing's.
	warnings []string
}

// repoCache keeps a user's repository listings for RepoListTTL in this
// process, so a loop on the listing endpoint cannot spend a forge's rate
// limit, the organisation's installation's included. An error from the
// forge is never kept (DL-134).
type repoCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[repoKey]repoEntry
}

func newRepoCache(now func() time.Time) *repoCache {
	return &repoCache{ttl: RepoListTTL, max: RepoListMaxEntries, now: now, entries: map[repoKey]repoEntry{}}
}

func (c *repoCache) get(k repoKey) (repoEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return repoEntry{}, false
	}
	if c.now().Sub(e.at) >= c.ttl {
		delete(c.entries, k)
		return repoEntry{}, false
	}
	return e, true
}

func (c *repoCache) put(k repoKey, e repoEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	e.at = now
	if _, ok := c.entries[k]; !ok && len(c.entries) >= c.max {
		for key, old := range c.entries {
			if now.Sub(old.at) >= c.ttl {
				delete(c.entries, key)
			}
		}
		for len(c.entries) >= c.max {
			var oldest repoKey
			var at time.Time
			for key, old := range c.entries {
				if at.IsZero() || old.at.Before(at) {
					oldest, at = key, old.at
				}
			}
			delete(c.entries, oldest)
		}
	}
	c.entries[k] = e
}

// dropUser forgets every listing of one user, whichever tier.
func (c *repoCache) dropUser(org, user string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.org == org && k.user == user {
			delete(c.entries, k)
		}
	}
}

// dropOrg forgets every listing in an organisation: the organisation's
// credential reaches each of its users' listings.
func (c *repoCache) dropOrg(org string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.org == org {
			delete(c.entries, k)
		}
	}
}

// ForgetRepoListings drops a user's cached listings. A credential the
// user stored or removed changes what they reach.
//
// @decision(DL-134)
func (s *Service) ForgetRepoListings(orgID, userID string) {
	s.repos.dropUser(orgID, userID)
}

// ForgetOrgRepoListings drops every cached listing of an organisation,
// for a change to the organisation's own credential.
//
// @decision(DL-134)
func (s *Service) ForgetOrgRepoListings(orgID string) {
	s.repos.dropOrg(orgID)
}

// tieredListing reads both of a user's tiers through Deps.UserRepos, each
// from the cache while it is fresh.
//
// @decision(DL-134) @decision(DL-042)
func (s *Service) tieredListing(ctx context.Context, p auth.Principal) ([]TieredRepo, []string, error) {
	tiers := []Tier{TierUser, TierOrg}
	listings := make([]RepoListing, len(tiers))
	errs := make([]error, len(tiers))
	var wg sync.WaitGroup
	for i, t := range tiers {
		wg.Go(func() {
			key := repoKey{org: p.OrgID, user: p.UserID, tier: string(t)}
			if e, ok := s.repos.get(key); ok {
				listings[i] = e.listing
				return
			}
			l, err := s.deps.UserRepos(ctx, p.OrgID, p.UserID, t)
			if err != nil {
				errs[i] = err
				return
			}
			listings[i] = l
			if !l.Failed {
				s.repos.put(key, repoEntry{listing: l})
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, nil, err
		}
	}
	repos, warnings := CombineRepoListings(listings...)
	return repos, warnings, nil
}

// plainListing reads the forge a request resolved to, from the cache
// while it is fresh. tiers are the resolution's platform tiers, which
// name the credentials the forge holds. A listing with a warning is not
// kept, since a warning may be a platform the forge failed on.
//
// @decision(DL-134)
func (s *Service) plainListing(ctx context.Context, p auth.Principal, forge Forge, tiers []PlatformTier) ([]RepoView, []string, error) {
	parts := make([]string, len(tiers))
	for i, t := range tiers {
		parts[i] = t.Platform + "=" + string(t.Tier)
	}
	sort.Strings(parts)
	key := repoKey{org: p.OrgID, user: p.UserID, tier: "plain:" + strings.Join(parts, ",")}
	if e, ok := s.repos.get(key); ok {
		return e.plain, e.warnings, nil
	}
	repos, warnings, err := forge.ListRepos(ctx)
	if err != nil {
		return nil, nil, err
	}
	views := make([]RepoView, 0, len(repos))
	for _, r := range repos {
		views = append(views, RepoView{
			Platform: r.Platform.Name(), Owner: r.Repo.Owner, Name: r.Repo.Name,
			Private: r.Repo.Private, Archived: r.Repo.Archived, UpdatedAt: r.Repo.UpdatedAt,
		})
	}
	if len(warnings) == 0 {
		s.repos.put(key, repoEntry{plain: views})
	}
	return views, warnings, nil
}
