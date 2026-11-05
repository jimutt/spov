//go:build integration

// Spike: can CI give us a real PostgreSQL, and do the facilities
// DL-003 rests on behave there?
//
// Run against any reachable server:
//
//	SPOV_TEST_DATABASE_URL=postgres://... go test -tags integration ./internal/store/ -run Phase2 -v
//
// Unlike the sandbox integration tests these create nothing billable;
// every test cleans up the schema it makes.
package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// dsn returns the test database URL or skips.
func dsn(t *testing.T) string {
	t.Helper()
	url := os.Getenv("SPOV_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SPOV_TEST_DATABASE_URL not set")
	}
	return url
}

func connect(t *testing.T) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close(context.WithoutCancel(ctx))
	})
	return conn
}

// TestPhase2PostgresReachable is the spike's headline question: is there a
// server, and which version is it? DL-003 targets 18 and requires >= 16.
func TestPhase2PostgresReachable(t *testing.T) {
	conn := connect(t)
	ctx := context.Background()

	var version string
	if err := conn.QueryRow(ctx, "select version()").Scan(&version); err != nil {
		t.Fatalf("select version(): %v", err)
	}
	t.Logf("server version: %s", version)

	// SHOW returns text whatever the setting's type, so parse it.
	var numText string
	if err := conn.QueryRow(ctx, "show server_version_num").Scan(&numText); err != nil {
		t.Fatalf("show server_version_num: %v", err)
	}
	num, err := strconv.Atoi(numText)
	if err != nil {
		t.Fatalf("server_version_num %q: %v", numText, err)
	}
	t.Logf("server_version_num: %d", num)
	if num < 160000 {
		t.Fatalf("DL-003 requires PostgreSQL 16 or newer, got %d", num)
	}
}

// TestPhase2SchemaIsolation exercises the isolation strategy DL-003 names
// for storetest: each test gets its own schema and drops it afterwards.
// If this does not work, parallel package tests share tables.
func TestPhase2SchemaIsolation(t *testing.T) {
	ctx := context.Background()
	schema := fmt.Sprintf("spov_spike_%d", time.Now().UnixNano())

	conn := connect(t)
	if _, err := conn.Exec(ctx, "create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c := context.WithoutCancel(ctx)
		if _, err := conn.Exec(c, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})

	// search_path per connection is what makes the isolation invisible to
	// the queries themselves, so sqlc-generated SQL needs no schema prefix.
	if _, err := conn.Exec(ctx, "set search_path to "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	if _, err := conn.Exec(ctx, "create table t (id text primary key)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := conn.Exec(ctx, "insert into t (id) values ('a')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var n int
	if err := conn.QueryRow(ctx, "select count(*) from t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}

// TestPhase2AdvisoryLock is DL-004's reconciler leadership, which is the
// one mechanism hosted mode cannot fall back from: exactly one holder, and the
// lock is released when its session ends.
func TestPhase2AdvisoryLock(t *testing.T) {
	ctx := context.Background()
	url := dsn(t)

	const key = "spov.reconciler"

	open := func() *pgx.Conn {
		c, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		return c
	}

	leader := open()
	defer leader.Close(context.WithoutCancel(ctx))
	follower := open()
	defer follower.Close(context.WithoutCancel(ctx))

	var got bool
	if err := leader.QueryRow(ctx, "select pg_try_advisory_lock(hashtext($1))", key).Scan(&got); err != nil {
		t.Fatalf("leader lock: %v", err)
	}
	if !got {
		t.Fatal("leader did not acquire the lock")
	}

	// A second connection must be refused, not blocked: pg_try_advisory_lock
	// returns immediately. This is what keeps one reconciler running.
	if err := follower.QueryRow(ctx, "select pg_try_advisory_lock(hashtext($1))", key).Scan(&got); err != nil {
		t.Fatalf("follower lock: %v", err)
	}
	if got {
		t.Fatal("follower acquired a lock the leader holds")
	}

	// Closing the leader's session must free the lock, which is how a dead
	// replica hands leadership over without a timeout.
	if err := leader.Close(ctx); err != nil {
		t.Fatalf("close leader: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := follower.QueryRow(ctx, "select pg_try_advisory_lock(hashtext($1))", key).Scan(&got); err != nil {
			t.Fatalf("follower relock: %v", err)
		}
		if got {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lock not released within 10s of the holder disconnecting")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestPhase2PartialUniqueIndex checks the constraint DL-003 uses to make
// "session names are unique per org among live sessions" structural rather
// than a read-then-write race.
func TestPhase2PartialUniqueIndex(t *testing.T) {
	ctx := context.Background()
	conn := connect(t)

	schema := fmt.Sprintf("spov_spike_idx_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c := context.WithoutCancel(ctx)
		if _, err := conn.Exec(c, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, "set search_path to "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("set search_path: %v", err)
	}

	if _, err := conn.Exec(ctx, `
		create table sessions (
			id text primary key,
			org_id text not null,
			name text not null,
			state text not null check (state in ('starting','running','stopped','failed'))
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		create unique index sessions_live_name
			on sessions (org_id, name)
			where state in ('starting','running')`); err != nil {
		t.Fatalf("create index: %v", err)
	}

	ins := func(id, name, state string) error {
		_, err := conn.Exec(ctx,
			"insert into sessions (id, org_id, name, state) values ($1, 'default', $2, $3)", id, name, state)
		return err
	}

	if err := ins("1", "alpha", "running"); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// A second live session with the same name must be refused.
	err := ins("2", "alpha", "starting")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("second live insert: got %v, want unique_violation (23505)", err)
	}

	// The same name is free once the first is no longer live, which is what
	// lets a user reuse a session name after stopping one.
	if _, err := conn.Exec(ctx, "update sessions set state = 'stopped' where id = '1'"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := ins("3", "alpha", "starting"); err != nil {
		t.Fatalf("insert after stop: %v", err)
	}
	// And stopped rows do not collide with each other.
	if err := ins("4", "alpha", "stopped"); err != nil {
		t.Fatalf("second stopped insert: %v", err)
	}
}

// TestPhase2SelectForUpdate covers DL-030's mint limit under Postgres:
// DL-003 claims it holds across replicas with no change because the count
// runs inside a transaction that locks the session row.
func TestPhase2SelectForUpdate(t *testing.T) {
	ctx := context.Background()
	url := dsn(t)

	setup, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Cleanups run last-registered-first, so closing is registered before
	// the drop that needs the connection still open.
	t.Cleanup(func() { _ = setup.Close(context.WithoutCancel(ctx)) })

	schema := fmt.Sprintf("spov_spike_fu_%d", time.Now().UnixNano())
	if _, err := setup.Exec(ctx, "create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c := context.WithoutCancel(ctx)
		if _, err := setup.Exec(c, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})

	q := func(sql string) string {
		return strings.ReplaceAll(sql, "{s}", pgx.Identifier{schema}.Sanitize())
	}
	if _, err := setup.Exec(ctx, q("create table {s}.sessions (id text primary key)")); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	if _, err := setup.Exec(ctx, q("insert into {s}.sessions values ('s1')")); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Two connections, both locking the same session row. The second must
	// wait for the first to commit rather than reading a stale count.
	a, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect a: %v", err)
	}
	defer a.Close(context.WithoutCancel(ctx))
	b, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect b: %v", err)
	}
	defer b.Close(context.WithoutCancel(ctx))

	txA, err := a.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a: %v", err)
	}
	var id string
	if err := txA.QueryRow(ctx, q("select id from {s}.sessions where id = 's1' for update")).Scan(&id); err != nil {
		t.Fatalf("a lock: %v", err)
	}

	blocked := make(chan error, 1)
	go func() {
		bctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		txB, err := b.Begin(bctx)
		if err != nil {
			blocked <- err
			return
		}
		var got string
		err = txB.QueryRow(bctx, q("select id from {s}.sessions where id = 's1' for update")).Scan(&got)
		_ = txB.Rollback(context.WithoutCancel(bctx))
		blocked <- err
	}()

	select {
	case err := <-blocked:
		t.Fatalf("second FOR UPDATE did not block, returned %v", err)
	case <-time.After(750 * time.Millisecond):
		// Blocking is the expected behaviour.
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit a: %v", err)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("second FOR UPDATE after commit: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("second FOR UPDATE still blocked 15s after the holder committed")
	}
}
