//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

// TestTheAccountMigrationKeepsEveryAddressAndBindsNobody writes users and
// address entries in the schema before migration 39, migrates, and
// checks that the entries still admit their addresses, no user is bound
// to an account, an account entry can sit beside them, the table refuses
// an entry of both kinds, and the down migration runs.
//
// @decision(DL-128)
func TestTheAccountMigrationKeepsEveryAddressAndBindsNobody(t *testing.T) {
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
			return
		}
		defer conn.Close(c)
		_, _ = conn.Exec(c, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade")
	})
	dsn := schemaURL(t, base, schema)
	if err := postgres.MigrateUpTo(ctx, dsn, 38); err != nil {
		t.Fatalf("migrate to 38: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	for _, q := range []string{
		`insert into orgs (id, name) values ('org', 'org')`,
		`insert into users (id, org_id, email, github_login) values ('u1', 'org', 'ada@example.com', 'ada')`,
		`insert into allowlist_entries (org_id, email, admin, created_at) values
		   ('org', 'ada@example.com', true, now()), ('org', 'bob@example.com', false, now())`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := postgres.MigrateUp(ctx, dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	s, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if e, err := s.Allowlist().Get(ctx, "org", "ADA@example.com"); err != nil || !e.Admin || e.GithubID != 0 {
		t.Errorf("ada's entry after the migration = %+v, %v", e, err)
	}
	if u, err := s.Users().Get(ctx, "org", "u1"); err != nil || u.GithubID != 0 {
		t.Errorf("an old row was bound by the migration: %+v, %v", u, err)
	}
	if got, err := s.Users().ByGithubLogin(ctx, "org", "ada"); err != nil || len(got) != 0 {
		t.Errorf("an unbound row's login matched: %+v, %v", got, err)
	}
	if err := s.Allowlist().Put(ctx, store.AllowlistEntry{OrgID: "org", GithubID: 42, GithubLogin: "bob"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Allowlist().CountAdmins(ctx, "org"); err != nil || n != 1 {
		t.Errorf("admins = %d, %v", n, err)
	}
	// The table itself refuses an entry of both kinds or neither.
	for _, q := range []string{
		`insert into allowlist_entries (org_id, email, github_id, created_at) values ('org', 'x@example.com', 9, now())`,
		`insert into allowlist_entries (org_id, created_at) values ('org', now())`,
		`insert into allowlist_entries (org_id, github_id, created_at) values ('org', 42, now())`,
		`insert into allowlist_entries (org_id, email, created_at) values ('org', 'bob@example.com', now())`,
	} {
		if _, err := conn.Exec(ctx, q); err == nil {
			t.Errorf("the table accepted %s", q)
		}
	}
	// Two rows can never hold one account.
	if _, err := conn.Exec(ctx, `insert into users (id, org_id, email, github_id) values ('u2', 'org', 'c@example.com', 7)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Users().BindGithub(ctx, "org", "u1", 7); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a second row took a held account: %v", err)
	}

	if _, err := postgres.MigrateDown(ctx, dsn); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
}
