package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/storetest"
)

// TestMemoryMeetsTheContract.
//
// @decision(DL-003)
func TestMemoryMeetsTheContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}

// TestMemoryTxViewDiesWithTheTransaction: a retained view would bypass
// the lock, so it panics instead.
//
// @decision(DL-003)
func TestMemoryTxViewDiesWithTheTransaction(t *testing.T) {
	m := store.NewMemory()
	var leaked store.Store
	if err := m.Tx(context.Background(), func(tx store.Store) error { leaked = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Error("a view used after its transaction did not panic")
		}
	}()
	_, _ = leaked.Sessions().List(context.Background(), "org", store.SessionFilter{})
}

// TestNewIDIsAULIDThatSortsByTime.
//
// @decision(DL-003)
func TestNewIDIsAULIDThatSortsByTime(t *testing.T) {
	t0 := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	a, b := store.NewID(t0), store.NewID(t0.Add(time.Second))
	if !store.ValidID(a) || !store.ValidID(b) {
		t.Fatalf("not ULIDs: %s %s", a, b)
	}
	if a >= b {
		t.Errorf("%s does not sort before %s", a, b)
	}
	if store.ValidID("not-an-id") || store.ValidID("") {
		t.Error("garbage validated")
	}
}

func TestMemoryImages(t *testing.T) {
	storetest.RunImages(t, func(*testing.T) store.ImageStore { return store.NewMemoryImages() })
}
