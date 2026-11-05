package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// TestOpenRejectsAnUnparseableURL: a bad URL must fail at Open with a
// message naming the problem, not at the first query.
//
// @decision(DL-003)
func TestOpenRejectsAnUnparseableURL(t *testing.T) {
	_, err := Open(context.Background(), "://not a url")
	if err == nil {
		t.Fatal("an unparseable url was accepted")
	}
	if got := err.Error(); got == "" {
		t.Error("the error says nothing")
	}
}

// TestStoreExposesEverySubStore: the Store interface is the schema, so a
// missing accessor is a compile error here rather than a nil panic in a
// handler.
//
// @decision(DL-003)
func TestStoreExposesEverySubStore(t *testing.T) {
	s := &Store{}
	var _ store.Store = s
	if s.Sessions() == nil || s.Mints() == nil || s.Users() == nil ||
		s.Tokens() == nil || s.DeviceCodes() == nil || s.Credentials() == nil ||
		s.Allowlist() == nil || s.Conversations() == nil {
		t.Error("a sub-store accessor returned nil")
	}
}

// TestNowUsesTheInjectedClock: the timestamps every write sets come from
// here, and tests need them deterministic.
//
// @decision(DL-003)
func TestNowUsesTheInjectedClock(t *testing.T) {
	fixed := time.Date(2026, 8, 30, 15, 0, 0, 0, time.UTC)
	s := &Store{Now: func() time.Time { return fixed }}
	if got := s.now(); !got.Equal(fixed) {
		t.Errorf("now = %v, want %v", got, fixed)
	}

	real := (&Store{}).now()
	if real.IsZero() {
		t.Error("the default clock returned the zero time")
	}
	if real.Location() != time.UTC {
		t.Errorf("the default clock returned %v, not UTC", real.Location())
	}
}

// TestTxRefusesToNest without a database: the guard is the flag, and it
// must trip before anything tries to open a transaction.
//
// @decision(DL-003)
func TestTxRefusesToNest(t *testing.T) {
	s := &Store{inTx: true}
	called := false
	err := s.Tx(context.Background(), func(store.Store) error {
		called = true
		return nil
	})
	if !errors.Is(err, store.ErrNestedTx) {
		t.Errorf("err = %v, want ErrNestedTx", err)
	}
	if called {
		t.Error("the callback ran inside a refused nested transaction")
	}
}

// TestErrorsNeverEchoTheDatabasePassword: secrets never appear in error
// messages, and a DSN carries one. A well-formed but
// unreachable URL is the case that matters — an unparseable one never
// reaches the connection code.
//
// @decision(DL-003)
func TestErrorsNeverEchoTheDatabasePassword(t *testing.T) {
	const password = "hunter2-should-not-appear"
	// Port 1 is reserved and refuses immediately, so this does not hang.
	dsn := "postgres://spov:" + password + "@127.0.0.1:1/spov?sslmode=disable&connect_timeout=1"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := Open(ctx, dsn); err == nil {
		t.Fatal("connecting to a closed port succeeded")
	} else if strings.Contains(err.Error(), password) {
		t.Errorf("Open leaked the password: %v", err)
	}

	if _, err := MigrateUp(ctx, dsn); err == nil {
		t.Fatal("migrating against a closed port succeeded")
	} else if strings.Contains(err.Error(), password) {
		t.Errorf("MigrateUp leaked the password: %v", err)
	}

	if _, err := MigrationsStatus(ctx, dsn); err == nil {
		t.Fatal("status against a closed port succeeded")
	} else if strings.Contains(err.Error(), password) {
		t.Errorf("MigrationsStatus leaked the password: %v", err)
	}

	// The leader connects too, and warns through a formatted string.
	var warned []string
	l := &Leader{URL: dsn, Key: "spov.test", Retry: time.Second,
		Warn: func(s string) { warned = append(warned, s) }}
	runCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	l.Run(runCtx, func(context.Context) {})
	for _, w := range warned {
		if strings.Contains(w, password) {
			t.Errorf("Leader leaked the password: %s", w)
		}
	}
}
