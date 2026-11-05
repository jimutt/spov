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

// TestTheOrgCannotBeRacedIntoHavingNoAdmin is the claim DL-007's guard
// rests on. Once the seed file is inert, an org with no administrator
// can only be repaired by a restart, so "this change would leave no
// admin" has to hold under two replicas demoting at once.
//
// A transaction alone does not deliver it: count(*) takes no row locks,
// so under READ COMMITTED every demotion that counts before any of them
// writes sees two admins and proceeds. The sleep forces exactly that
// interleaving, which is otherwise rare enough to pass by luck. It is
// the same failure DL-009 hit on quotas, and the same advisory lock.
//
// @decision(DL-007) @decision(DL-009)
func TestTheOrgCannotBeRacedIntoHavingNoAdmin(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	admins := []string{"ada@example.com", "bob@example.com"}
	for _, email := range admins {
		if err := st.Allowlist().Put(ctx, store.AllowlistEntry{
			OrgID: "default", Email: email, Admin: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	demoted := make([]bool, len(admins))
	start := make(chan struct{})
	for i, email := range admins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := st.Tx(ctx, func(tx store.Store) error {
				n, err := tx.Allowlist().CountAdmins(ctx, "default")
				if err != nil {
					return err
				}
				// Held open past the count so both racers read before
				// either writes.
				time.Sleep(150 * time.Millisecond)
				if n <= 1 {
					return errLastAdmin
				}
				return tx.Allowlist().Put(ctx, store.AllowlistEntry{
					OrgID: "default", Email: email, Admin: false,
				})
			})
			switch {
			case err == nil:
				demoted[i] = true
			case errors.Is(err, errLastAdmin):
			default:
				t.Errorf("racer %d: %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	n, err := st.Allowlist().CountAdmins(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("the org was raced into having no administrator, which no API call can undo")
	}
	won := 0
	for _, ok := range demoted {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d demotions succeeded, want exactly 1", won)
	}
}

// TestConcurrentSeedersConverge: N replicas start together under a
// rolling deploy, and "if there is no admin, import the file" is
// check-then-act. They must converge on one allowlist rather than
// racing, which is why the seed holds the same lock.
//
// @decision(DL-007)
func TestConcurrentSeedersConverge(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	const replicas = 6
	var wg sync.WaitGroup
	seeded := make([]bool, replicas)
	start := make(chan struct{})
	for i := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := st.Tx(ctx, func(tx store.Store) error {
				n, err := tx.Allowlist().CountAdmins(ctx, "default")
				if err != nil {
					return err
				}
				if n > 0 {
					return nil
				}
				time.Sleep(100 * time.Millisecond)
				seeded[i] = true
				return tx.Allowlist().Put(ctx, store.AllowlistEntry{
					OrgID: "default", Email: "ada@example.com", Admin: true,
				})
			})
			if err != nil {
				t.Errorf("replica %d: %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	entries, err := st.Allowlist().List(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want the one address the file names", entries)
	}
	won := 0
	for _, ok := range seeded {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d replicas seeded, want exactly 1: the rest must see the first one's work", won)
	}
	// And the upsert leaves one row however many replicas raced.
	if !entries[0].Admin {
		t.Error("the seeded entry is not an admin")
	}
}

// errLastAdmin stands in for the server's own refusal, which the store
// package must not import.
var errLastAdmin = errors.New("last administrator")
