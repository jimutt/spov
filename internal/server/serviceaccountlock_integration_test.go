//go:build integration

package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

// On Postgres, a service account's create waits for a delete that holds
// the account's quota locks, and then reads the account as deleted:
// the two serialise on those locks, so no create commits after the
// delete without being seen by the off-boarding that follows it.
//
// @decision(DL-140)
func TestIntegrationAServiceAccountsCreateWaitsForItsDelete(t *testing.T) {
	pool := jobPool(t)
	ctx := context.Background()
	st := postgres.NewWithPool(pool)
	svc := store.User{ID: store.NewID(time.Now()), OrgID: DefaultOrgID, Kind: store.UserService, Name: "ci-lock"}
	svc.Email = svc.ID + "@" + auth.ServiceAccountDomain
	if err := st.Users().Create(ctx, svc); err != nil {
		t.Fatal(err)
	}

	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- st.Tx(ctx, func(tx store.Store) error {
			if err := lockOwnerQuotas(ctx, tx, svc.OrgID, svc.ID); err != nil {
				return err
			}
			if err := tx.Users().Disable(ctx, svc.OrgID, svc.ID, time.Now()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	created := make(chan error, 1)
	go func() {
		created <- st.Tx(ctx, func(tx store.Store) error { return liveServiceOwner(ctx, tx, svc.OrgID, svc.ID) })
	}()
	select {
	case err := <-created:
		t.Fatalf("the create did not wait for the delete's locks: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	err := <-created
	var se huma.StatusError
	if !errors.As(err, &se) || se.GetStatus() != 401 {
		t.Errorf("the create after the delete = %v, want a 401", err)
	}
}
