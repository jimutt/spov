//go:build integration

// The Postgres store against a real server. Set SPOV_TEST_DATABASE_URL:
//
//	SPOV_TEST_DATABASE_URL=postgres://... go test -tags integration ./internal/store/postgres/ -v
//
// Each test runs in its own schema, created and dropped by the test, so
// the package parallelises against one database.
//
// @decision(DL-003)
package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
	"github.com/jimutt/spov/internal/store/storetest"
)

var schemaSeq struct {
	sync.Mutex
	n int
}

func nextSchema() string {
	schemaSeq.Lock()
	defer schemaSeq.Unlock()
	schemaSeq.n++
	return fmt.Sprintf("spov_test_%d_%d", time.Now().UnixNano(), schemaSeq.n)
}

func baseURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("SPOV_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("SPOV_TEST_DATABASE_URL not set")
	}
	return u
}

// schemaURL returns a URL whose connections land in their own schema, so
// the migrations and every query see only this test's tables.
func schemaURL(t *testing.T, base, schema string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// newStore migrates a fresh schema and returns a store on it.
func newStore(t *testing.T) *postgres.Store {
	t.Helper()
	ctx := context.Background()
	base := baseURL(t)
	schema := nextSchema()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(context.WithoutCancel(ctx))
	if _, err := admin.Exec(ctx, "create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(c, base)
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer conn.Close(c)
		if _, err := conn.Exec(c, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})

	dsn := schemaURL(t, base, schema)
	if _, err := postgres.MigrateUp(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)

	// storetest's sessions reference orgs that the contract never
	// creates, so the foreign key would reject them. The orgs the suite
	// uses are inserted here rather than weakening the schema.
	pool := s.Pool()
	for _, org := range []string{"org", "org-2", "org-a", "org-b", "other", "other-org", "org-z"} {
		if _, err := pool.Exec(ctx,
			"insert into orgs (id, name) values ($1, $1) on conflict do nothing", org); err != nil {
			t.Fatalf("seed org %s: %v", org, err)
		}
	}
	return s
}

// newStoreWithMaxConns is newStore with a deliberately small pool, so a
// leaked connection is a failure rather than a slow drift.
func newStoreWithMaxConns(t *testing.T, max int) *postgres.Store {
	t.Helper()
	ctx := context.Background()
	base := baseURL(t)
	schema := nextSchema()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(context.WithoutCancel(ctx))
	if _, err := admin.Exec(ctx, "create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c := context.WithoutCancel(ctx)
		conn, err := pgx.Connect(c, base)
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer conn.Close(c)
		if _, err := conn.Exec(c, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})

	// pool_max_conns is understood by pgxpool, not by the database/sql
	// handle the migrations use, which forwards it to the server as a
	// setting and is refused. So migrate on the plain DSN and keep the
	// option for the pool.
	dsn := schemaURL(t, base, schema)
	if _, err := postgres.MigrateUp(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := postgres.Open(ctx, dsn+fmt.Sprintf("&pool_max_conns=%d", max))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// TestPostgresMeetsTheContract is the acceptance test: the same suite the
// memory store passes.
//
// @decision(DL-003)
func TestPostgresMeetsTheContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return newStore(t) })
}

// TestPostgresImagesMeetTheContract runs the image registry's contract.
//
// @decision(DL-016)
func TestPostgresImagesMeetTheContract(t *testing.T) {
	storetest.RunImages(t, func(t *testing.T) store.ImageStore { return newStore(t).Images() })
}

// TestPostgresTemplatesMeetTheContract runs the sandbox template
// store's contract.
//
// @decision(DL-119)
func TestPostgresTemplatesMeetTheContract(t *testing.T) {
	storetest.RunTemplates(t, "org", func(t *testing.T) store.TemplateStore { return newStore(t).Templates() })
}

// TestMigrateUpIsIdempotentAndReversible.
//
// @decision(DL-003)
func TestMigrateUpIsIdempotentAndReversible(t *testing.T) {
	ctx := context.Background()
	base := baseURL(t)
	schema := nextSchema()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Cleanups run last-registered-first, so the close is registered
	// before the drop that needs the connection open.
	t.Cleanup(func() { _ = admin.Close(context.WithoutCancel(ctx)) })
	if _, err := admin.Exec(ctx, "create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c := context.WithoutCancel(ctx)
		if _, err := admin.Exec(c, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	dsn := schemaURL(t, base, schema)

	applied, err := postgres.MigrateUp(ctx, dsn)
	if err != nil || len(applied) == 0 {
		t.Fatalf("first up: %v applied %v", err, applied)
	}
	if head, err := postgres.AtHead(ctx, dsn); err != nil || !head {
		t.Fatalf("at head after up: %v, %v", head, err)
	}
	again, err := postgres.MigrateUp(ctx, dsn)
	if err != nil || len(again) != 0 {
		t.Fatalf("second up applied %v, %v; want nothing", again, err)
	}
	if _, err := postgres.MigrateDown(ctx, dsn); err != nil {
		t.Fatalf("down: %v", err)
	}
	if head, err := postgres.AtHead(ctx, dsn); err != nil || head {
		t.Fatalf("at head after down: %v, %v", head, err)
	}
}

// TestLeaderElectsOneHolder is the leadership DL-004 rests on: two
// replicas, one leader, and the follower takes over when it goes.
//
// @decision(DL-004)
func TestLeaderElectsOneHolder(t *testing.T) {
	ctx := context.Background()
	base := baseURL(t)
	key := "spov.test." + nextSchema()

	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	first := &postgres.Leader{URL: base, Key: key, Retry: 200 * time.Millisecond}
	firstWorking := make(chan struct{})
	var once sync.Once
	go first.Run(firstCtx, func(ctx context.Context) {
		once.Do(func() { close(firstWorking) })
		<-ctx.Done()
	})
	select {
	case <-firstWorking:
	case <-time.After(15 * time.Second):
		t.Fatal("the first replica never became leader")
	}

	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	second := &postgres.Leader{URL: base, Key: key, Retry: 200 * time.Millisecond}
	secondWorking := make(chan struct{})
	var onceSecond sync.Once
	go second.Run(secondCtx, func(ctx context.Context) {
		onceSecond.Do(func() { close(secondWorking) })
		<-ctx.Done()
	})

	select {
	case <-secondWorking:
		t.Fatal("two replicas held the lock at once")
	case <-time.After(1500 * time.Millisecond):
	}
	if !first.Held() || second.Held() {
		t.Fatalf("held: first=%v second=%v", first.Held(), second.Held())
	}

	// The leader goes; the follower must take over on its own.
	stopFirst()
	select {
	case <-secondWorking:
	case <-time.After(15 * time.Second):
		t.Fatal("the follower never took over")
	}
	if !second.Held() {
		t.Error("the follower ran work without holding the lock")
	}
}

// TestTxRollsBackAcrossSubStores: a transaction that fails must undo
// writes to every table it touched, not only sessions.
//
// @decision(DL-003)
func TestTxRollsBackAcrossSubStores(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	boom := errors.New("boom")

	err := s.Tx(ctx, func(tx store.Store) error {
		if err := tx.Users().Create(ctx, store.User{ID: "u-tx", OrgID: "org", Email: "tx@example.com"}); err != nil {
			return err
		}
		if err := tx.Tokens().Create(ctx, store.APIToken{ID: "t-tx", OrgID: "org", UserID: "u-tx", Hash: []byte("tx")}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Users().Get(ctx, "org", "u-tx"); !errors.Is(err, store.ErrNotFound) {
		t.Error("a rolled-back user is visible")
	}
	if _, err := s.Tokens().ByHash(ctx, []byte("tx")); !errors.Is(err, store.ErrNotFound) {
		t.Error("a rolled-back token is visible")
	}
}

// TestLiveNameUniquenessIsEnforcedByTheDatabase: two replicas creating
// the same name concurrently must not both win, whatever the application
// checks.
//
// @decision(DL-003)
func TestLiveNameUniquenessIsEnforcedByTheDatabase(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pool := s.Pool()

	insert := func(id string) error {
		_, err := pool.Exec(ctx, `insert into sessions (id, org_id, owner_id, name, state)
			values ($1, 'org', 'u1', 'race', 'running')`, id)
		return err
	}
	if err := insert("s-1"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert("s-2"); err == nil {
		t.Fatal("the index allowed a second live session with the same name")
	}
}

// The two git access defaults hold only what the server can mean, so a
// row written around the API cannot store a value a launch would read
// as repos without anyone noticing.
//
// @decision(DL-043)
func TestGitAccessDefaultsAreConstrainedByTheDatabase(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, tc := range []struct {
		access, job store.GitAccess
		ok          bool
	}{
		{"", "", true},
		{store.GitAccessAll, store.GitAccessRepos, true},
		{"everything", "", false},
		{"", "ALL", false},
	} {
		id := store.NewID(time.Now())
		u := store.User{ID: id, OrgID: "org", Email: id + "@example.com", DefaultGitAccess: tc.access, DefaultJobGitAccess: tc.job}
		if err := s.Users().Create(ctx, u); (err == nil) != tc.ok {
			t.Errorf("%q/%q: create err = %v", tc.access, tc.job, err)
		}
	}
}

// TestPoolIsUsableAfterTx guards the rollback path: a leaked transaction
// would exhaust the pool.
//
// @decision(DL-003)
func TestPoolIsUsableAfterTx(t *testing.T) {
	ctx := context.Background()
	s := newStoreWithMaxConns(t, 2)

	for i := range 20 {
		_ = s.Tx(ctx, func(tx store.Store) error {
			return fmt.Errorf("rollback %d", i)
		})
	}
	// With only two connections, a single leak among twenty rollbacks
	// makes this time out.
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for range 4 {
		if err := s.Ping(c); err != nil {
			t.Fatalf("the pool is exhausted after rolled-back transactions: %v", err)
		}
	}
}

// TestLeaderNoticesTheLockReleasedUnderIt: a Ping succeeds on a
// connection whose lock someone else released, so the holder re-asserts
// ownership instead. Without that, a second replica could take the lock
// while the first kept working.
//
// @decision(DL-004)
func TestLeaderNoticesTheLockReleasedUnderIt(t *testing.T) {
	ctx := context.Background()
	base := baseURL(t)
	key := "spov.test." + nextSchema()

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	l := &postgres.Leader{URL: base, Key: key, Retry: 200 * time.Millisecond}
	working := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once
	go func() {
		l.Run(runCtx, func(ctx context.Context) {
			once.Do(func() { close(working) })
			<-ctx.Done()
			select {
			case <-stopped:
			default:
				close(stopped)
			}
		})
	}()
	select {
	case <-working:
	case <-time.After(15 * time.Second):
		t.Fatal("never became leader")
	}

	// Release the lock from another session, as an admin would.
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(context.WithoutCancel(ctx))
	var pid int
	if err := admin.QueryRow(ctx, `select pid from pg_locks
		where locktype = 'advisory' and objid = hashtext($1)::bigint & 4294967295
		and granted limit 1`, key).Scan(&pid); err != nil {
		t.Fatalf("find the holder: %v", err)
	}
	if _, err := admin.Exec(ctx, "select pg_terminate_backend($1)", pid); err != nil {
		t.Fatalf("terminate the holder: %v", err)
	}

	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the leader kept working after its lock went")
	}
	if l.Held() {
		t.Error("the leader still reports the lock held")
	}
}
