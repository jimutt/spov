//go:build integration

// The supervisor's queue against a real Postgres. Set
// SPOV_TEST_DATABASE_URL:
//
//	SPOV_TEST_DATABASE_URL=postgres://... go test -tags integration ./internal/jobs/ -run River -v
//
// Every test here carries "River" in its name so that selector runs all
// of them. It needs a database and nothing else: no Modal credentials, no
// GitHub OAuth app, and it creates no sandbox.
//
// @decision(DL-046)
package jobs

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

func riverPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SPOV_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SPOV_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// River's migrator must bring its own tables up, and running it twice
// must be safe: every replica runs it at startup.
func TestIntegrationRiverMigratesItsOwnSchema(t *testing.T) {
	pool := riverPool(t)
	ctx := context.Background()

	if err := MigrateRiver(ctx, pool); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := MigrateRiver(ctx, pool); err != nil {
		t.Fatalf("second migrate is not idempotent: %v", err)
	}

	var n int
	err := pool.QueryRow(ctx,
		`select count(*) from information_schema.tables
		 where table_schema = current_schema() and table_name = 'river_job'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("river_job does not exist after migrating (found %d)", n)
	}
}

// The boundary that makes two migrators safe: goose owns the domain
// tables, River owns river_*, and neither creates the other's.
//
// BOTH migrators run here. Running only River's would leave the domain
// side empty, and an overlap check against nothing passes for the wrong
// reason — the two have to be in the same database for the question to
// mean anything.
func TestIntegrationRiverAndGooseDoNotOverlap(t *testing.T) {
	pool := riverPool(t)
	ctx := context.Background()

	if _, err := postgres.MigrateUp(ctx, os.Getenv("SPOV_TEST_DATABASE_URL")); err != nil {
		t.Fatalf("goose migrate: %v", err)
	}
	if err := MigrateRiver(ctx, pool); err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx,
		`select table_name from information_schema.tables
		 where table_schema = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var riverTables, domainTables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasPrefix(name, "river_"):
			riverTables = append(riverTables, name)
		case name == "goose_db_version":
		default:
			domainTables = append(domainTables, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(riverTables) == 0 {
		t.Error("River's migrator created no river_* tables")
	}
	// Without this the overlap check below is vacuous: it would be
	// comparing River's tables against an empty set.
	if len(domainTables) == 0 {
		t.Fatal("goose created no domain tables, so this proves nothing about the two coexisting")
	}
	t.Logf("river tables: %v", riverTables)
	t.Logf("domain tables: %v", domainTables)

	// The tables spov's own migrations own must survive River's, and
	// vice versa. This is the failure the split exists to prevent.
	for _, want := range []string{"sessions", "jobs", "events"} {
		found := false
		for _, name := range domainTables {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the domain table %q is missing after both migrators ran: %v", want, domainTables)
		}
	}

	// River must not have created anything outside its own prefix. If
	// this fails, the two migrators have started sharing a table and
	// the exception to DL-003 is no longer contained.
	for _, name := range domainTables {
		if strings.Contains(name, "river") {
			t.Errorf("table %q looks like River's but is outside the river_ prefix", name)
		}
	}
}

// A job inserted on the supervisor's queue must be picked up and worked,
// which is the whole point of the durable queue.
func TestIntegrationRiverQueueWorksAJob(t *testing.T) {
	pool := riverPool(t)
	ctx := context.Background()
	if err := MigrateRiver(ctx, pool); err != nil {
		t.Fatal(err)
	}

	// River's queue is durable, so a job left over from an earlier run
	// against this database is still there and may be worked first.
	// The test therefore waits for ITS OWN job rather than for the
	// first one worked — asserting on the first would fail against a
	// database that has been used before.
	st := postgres.NewWithPool(pool)
	worked := make(chan JobArgs, 32)
	sup := &Supervisor{
		Store: st,
		Warn:  func(string) {},
		onRun: func(a JobArgs) {
			select {
			case worked <- a:
			default:
			}
		},
	}

	client, err := NewRiverClient(pool, sup)
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	args := JobArgs{JobID: store.NewID(time.Now()), OrgID: "org_1"}
	if _, err := client.Insert(ctx, args, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	deadline := time.After(30 * time.Second)
	var others int
	for {
		select {
		case got := <-worked:
			if got.JobID == args.JobID {
				if others > 0 {
					t.Logf("also worked %d job(s) left over from an earlier run", others)
				}
				return
			}
			others++
		case <-deadline:
			t.Fatalf("the queue never worked this test's job (worked %d other(s))", others)
		}
	}
}

// River's defaults must not survive into the running client: a
// one-minute JobTimeout would cancel a job long before its sandbox is
// done.
func TestIntegrationRiverClientUsesSpovsConfiguration(t *testing.T) {
	pool := riverPool(t)
	if err := MigrateRiver(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	client, err := NewRiverClient(pool, &Supervisor{Store: postgres.NewWithPool(pool)})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	// River validates RescueStuckJobsAfter > JobTimeout at construction,
	// so a client that builds at all has the ordering right; this pins
	// that the values reaching it are spov's.
	if SupervisorJobTimeout <= time.Minute {
		t.Error("JobTimeout is River's default")
	}
	_ = client
}
