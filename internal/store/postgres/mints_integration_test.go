//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// TestTheMintLimitHoldsAcrossReplicas: counting a session's mints and
// inserting one are one step only while the session's row is locked.
// Without the lock every racer counts before any inserts and all pass a
// limit of one; the sleep forces that interleaving.
//
// @decision(DL-065)
func TestTheMintLimitHoldsAcrossReplicas(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Users().Create(ctx, store.User{ID: "u1", OrgID: "default", Email: "u1@example.com"}); err != nil {
		t.Fatal(err)
	}
	sess := store.Session{ID: store.NewID(time.Now()), OrgID: "default", OwnerID: "u1", Name: "minted", State: store.StateRunning}
	if err := st.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	const limit = 1
	const racers = 8
	errOverLimit := errors.New("over the mint limit")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := st.Tx(ctx, func(tx store.Store) error {
				if err := tx.Sessions().Lock(ctx, "default", sess.ID); err != nil {
					return err
				}
				now := time.Now()
				n, err := tx.Mints().CountSince(ctx, "default", sess.ID, now.Add(-time.Minute))
				if err != nil {
					return err
				}
				time.Sleep(150 * time.Millisecond)
				if n >= limit {
					return errOverLimit
				}
				return tx.Mints().Create(ctx, store.Mint{ID: store.NewID(now), OrgID: "default", SessionID: sess.ID, UserID: "u1", CreatedAt: now})
			})
			if err != nil && !errors.Is(err, errOverLimit) {
				t.Errorf("racer %d: %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	n, err := st.Mints().CountSince(ctx, "default", sess.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != limit {
		t.Errorf("%d mints recorded, want exactly %d", n, limit)
	}
}
