//go:build integration

// The browser session table against a real server. It is the one part of
// login whose schema is owned by a third-party library, so the check is
// that scs and the migration agree.
//
//	SPOV_TEST_DATABASE_URL=postgres://... go test -tags integration ./internal/store/postgres/ -run WebSession -v
//
// @decision(DL-007)
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
)

// TestWebSessionsTableServesSCS: the migration creates the table the
// library expects, under the name spov gives it. A mismatch here is
// invisible until someone tries to log in to a real deployment.
//
// @decision(DL-007)
func TestWebSessionsTableServesSCS(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	sessions := scs.New()
	// The same configuration the server builds: notably the table name,
	// which is not the library's default "sessions" — that name is
	// spov's own sessions table, in this very schema.
	store := pgxstore.NewWithConfig(s.Pool(), pgxstore.Config{TableName: "web_sessions"})
	t.Cleanup(store.StopCleanup)
	sessions.Store = store
	sessions.Lifetime = time.Hour

	// Write a session...
	writeCtx, err := sessions.Load(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	sessions.Put(writeCtx, "user_id", "user-1")
	token, expiry, err := sessions.Commit(writeCtx)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if token == "" {
		t.Fatal("commit produced no token")
	}
	if !expiry.After(time.Now()) {
		t.Errorf("expiry %s is not in the future", expiry)
	}

	// ...and read it back through a fresh load, as another replica
	// would.
	readCtx, err := sessions.Load(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessions.GetString(readCtx, "user_id"); got != "user-1" {
		t.Errorf("user_id = %q, want user-1", got)
	}

	// Iterate is what the allowlist reaper and the session listing use.
	seen := 0
	if err := sessions.Iterate(ctx, func(ctx context.Context) error {
		if sessions.GetString(ctx, "user_id") == "user-1" {
			seen++
		}
		return nil
	}); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if seen != 1 {
		t.Errorf("Iterate saw the session %d times, want 1", seen)
	}

	// Destroy is how logout and revocation work: the row goes, so the
	// token stops working everywhere rather than only in this browser.
	destroyCtx, err := sessions.Load(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Destroy(destroyCtx); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	goneCtx, err := sessions.Load(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessions.GetString(goneCtx, "user_id"); got != "" {
		t.Errorf("a destroyed session still resolves to %q", got)
	}
}

// TestWebSessionsIsSeparateFromSpovSessions: both tables exist in the
// same schema, so the library's default name would have collided with
// spov's own.
//
// @decision(DL-007)
func TestWebSessionsIsSeparateFromSpovSessions(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, table := range []string{"sessions", "web_sessions"} {
		var n int
		if err := s.Pool().QueryRow(ctx,
			"select count(*) from information_schema.tables where table_name = $1 and table_schema = current_schema()",
			table).Scan(&n); err != nil {
			t.Fatalf("looking for %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("found %d tables named %s, want 1", n, table)
		}
	}
	// The columns scs requires, with the types it writes.
	rows, err := s.Pool().Query(ctx,
		"select column_name from information_schema.columns where table_name = 'web_sessions' and table_schema = current_schema()")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		found[name] = true
	}
	for _, col := range []string{"token", "data", "expiry"} {
		if !found[col] {
			t.Errorf("web_sessions has no %s column", col)
		}
	}
}
