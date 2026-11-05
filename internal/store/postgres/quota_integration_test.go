//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// TestConcurrencyQuotaIsALimitNotASuggestion is the claim DL-009 rests
// on: "counted in the transaction is what makes the number a limit
// rather than a suggestion under two replicas".
//
// A transaction alone does not deliver it. count(*) takes no row locks,
// so under READ COMMITTED every transaction that counts before any of
// them inserts finds room, and eight concurrent creates all pass a
// limit of one — reproduced against PostgreSQL 18 without the advisory
// lock. The sleep forces exactly that interleaving, which is
// otherwise rare enough to pass by luck.
//
// @decision(DL-009)
func TestConcurrencyQuotaIsALimitNotASuggestion(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Users().Create(ctx, store.User{ID: "u1", OrgID: "default", Email: "u1@example.com"}); err != nil {
		t.Fatal(err)
	}

	const max = 1
	const racers = 8
	var wg sync.WaitGroup
	created := make([]bool, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := st.Tx(ctx, func(tx store.Store) error {
				n, err := tx.Sessions().CountLive(ctx, "default", "u1")
				if err != nil {
					return err
				}
				// Held open past the count so every racer reads before
				// any of them writes.
				time.Sleep(150 * time.Millisecond)
				if n >= max {
					return errOverQuota
				}
				return tx.Sessions().Create(ctx, store.Session{
					ID: store.NewID(time.Now()), OrgID: "default", OwnerID: "u1",
					Name: fmt.Sprintf("racer-%d", i), State: store.StateStarting,
				})
			})
			switch {
			case err == nil:
				created[i] = true
			case errors.Is(err, errOverQuota):
			default:
				t.Errorf("racer %d: %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	n, err := st.Sessions().CountLive(ctx, "default", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if n > max {
		t.Errorf("the quota was overshot: %d live sessions, limit %d", n, max)
	}
	won := 0
	for _, ok := range created {
		if ok {
			won++
		}
	}
	if won != max {
		t.Errorf("%d creates succeeded, want exactly %d", won, max)
	}
}

// TestJobConcurrencyQuotaIsALimitNotASuggestion is the same claim for
// jobs, and it is asserted separately rather than assumed from the
// session one: the two counts take different advisory locks, and a job
// lock that collided with — or was omitted from — the session's would
// pass every unit test and fail exactly here.
//
// @decision(DL-009)
func TestJobConcurrencyQuotaIsALimitNotASuggestion(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Users().Create(ctx, store.User{ID: "u1", OrgID: "default", Email: "u1@example.com"}); err != nil {
		t.Fatal(err)
	}

	const max = 1
	const racers = 8
	// One session each, created up front: a session may hold only one
	// job, so the race has to be over the job insert alone.
	sessions := make([]string, racers)
	for i := range racers {
		sessions[i] = store.NewID(time.Now())
		if err := st.Sessions().Create(ctx, store.Session{
			ID: sessions[i], OrgID: "default", OwnerID: "u1",
			Name: fmt.Sprintf("job-racer-%d", i), State: store.StateStarting,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	created := make([]bool, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := st.Tx(ctx, func(tx store.Store) error {
				n, err := tx.Jobs().CountLive(ctx, "default", "u1")
				if err != nil {
					return err
				}
				// Held open past the count so every racer reads before
				// any of them writes.
				time.Sleep(150 * time.Millisecond)
				if n >= max {
					return errOverQuota
				}
				return tx.Jobs().Create(ctx, store.Job{
					ID: store.NewID(time.Now()), OrgID: "default", OwnerID: "u1",
					SessionID: sessions[i], Kind: store.JobKindPR,
					State: store.JobQueued, Goal: "do the thing",
				})
			})
			switch {
			case err == nil:
				created[i] = true
			case errors.Is(err, errOverQuota):
			default:
				t.Errorf("racer %d: %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	n, err := st.Jobs().CountLive(ctx, "default", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if n > max {
		t.Errorf("the job quota was overshot: %d live jobs, limit %d", n, max)
	}
	won := 0
	for _, ok := range created {
		if ok {
			won++
		}
	}
	if won != max {
		t.Errorf("%d job creates succeeded, want exactly %d", won, max)
	}
}

// TestJobAndSessionQuotasDoNotSerialiseOnEachOther: the two locks are
// deliberately different keys, so a user creating a job and a session
// at the same time is not made to wait. Asserted by timing: if the two
// shared a key, the second would block for the first's held-open
// transaction.
//
// @decision(DL-009)
func TestJobAndSessionQuotasDoNotSerialiseOnEachOther(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Users().Create(ctx, store.User{ID: "u1", OrgID: "default", Email: "u1@example.com"}); err != nil {
		t.Fatal(err)
	}
	const hold = 400 * time.Millisecond

	inJobTx := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- st.Tx(ctx, func(tx store.Store) error {
			if _, err := tx.Jobs().CountLive(ctx, "default", "u1"); err != nil {
				return err
			}
			close(inJobTx)
			time.Sleep(hold)
			return nil
		})
	}()
	<-inJobTx

	// The session count must not wait on the job transaction above.
	started := time.Now()
	if err := st.Tx(ctx, func(tx store.Store) error {
		_, err := tx.Sessions().CountLive(ctx, "default", "u1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waited := time.Since(started)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Generous: the point is that it did not wait out the full hold,
	// not that it was instant.
	if waited > hold/2 {
		t.Errorf("the session count waited %s on a job transaction; the two quotas share a lock key", waited)
	}
}

// errOverQuota stands in for the server's own sentinel, which the store
// package must not import.
var errOverQuota = errors.New("over quota")

// TestACredentialLockSerialisesRefreshes is what DL-015's refresh relies
// on across replicas: two transactions that take the same user's
// credential lock run one after the other, so the second reads what the
// first wrote. A different user's lock does not wait.
//
// @decision(DL-015)
func TestACredentialLockSerialisesRefreshes(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	var (
		mu      sync.Mutex
		order   []string
		wg      sync.WaitGroup
		holding = make(chan struct{})
	)
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = st.Tx(ctx, func(tx store.Store) error {
			if err := tx.Credentials().Lock(ctx, "default", "u1", "opencode"); err != nil {
				return err
			}
			note("first in")
			close(holding)
			time.Sleep(300 * time.Millisecond)
			note("first out")
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		<-holding
		_ = st.Tx(ctx, func(tx store.Store) error {
			if err := tx.Credentials().Lock(ctx, "default", "u1", "opencode"); err != nil {
				return err
			}
			note("second in")
			return nil
		})
	}()
	<-holding
	start := time.Now()
	if err := st.Tx(ctx, func(tx store.Store) error {
		return tx.Credentials().Lock(ctx, "default", "u2", "opencode")
	}); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > 200*time.Millisecond {
		t.Errorf("another user's lock waited %s", waited)
	}
	wg.Wait()
	if fmt.Sprint(order) != "[first in first out second in]" {
		t.Errorf("order = %v; the second transaction did not wait for the first", order)
	}
}
