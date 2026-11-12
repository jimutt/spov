package cli

import (
	"context"
	"testing"

	"github.com/jimutt/spov/internal/server"
)

// bothTiers lists the way the server does: each tier on its own, then
// combined.
func bothTiers(t *testing.T, h *hostedCredentials, orgID, userID string) ([]server.TieredRepo, []string, error) {
	t.Helper()
	var listings []server.RepoListing
	for _, tier := range []server.Tier{server.TierUser, server.TierOrg} {
		l, err := h.reposOfTier(context.Background(), orgID, userID, tier)
		if err != nil {
			return nil, nil, err
		}
		listings = append(listings, l)
	}
	repos, warnings := server.CombineRepoListings(listings...)
	return repos, warnings, nil
}
