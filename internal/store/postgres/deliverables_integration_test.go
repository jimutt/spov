//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

// TestTheDeliverablesMigrationMovesOldResults writes jobs in the schema
// before migration 24, migrates, and checks each one's result and branch
// became the member DL-051 says, then that the down migration runs.
//
// @decision(DL-051)
func TestTheDeliverablesMigrationMovesOldResults(t *testing.T) {
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
	if err := postgres.MigrateUpTo(ctx, dsn, 23); err != nil {
		t.Fatalf("migrate to 23: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, q := range []string{
		`insert into orgs (id, name) values ('org', 'org')`,
		`insert into sessions (id, org_id, owner_id, name, state, request) values
		   ('s1', 'org', 'u', 'job-1', 'stopped', '{"Repos":[{"Platform":"codefloe","Owner":"acme","Name":"app"},{"Platform":"github","Owner":"acme","Name":"lib"}]}'),
		   ('s2', 'org', 'u', 'job-2', 'stopped', '{"Repos":[{"Platform":"codefloe","Owner":"acme","Name":"app"}]}'),
		   ('s3', 'org', 'u', 'job-3', 'stopped', '{}'),
		   ('s4', 'org', 'u', 'job-4', 'stopped', '{"Repos":[{"Platform":"codefloe","Owner":"acme","Name":"app"}]}')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, j := range []struct{ id, sess, kind, result, branch string }{
		{"j1", "s1", "pr", "https://codefloe.com/acme/app/pulls/9", "fix"},
		{"j2", "s2", "pr", "", "half"},
		{"j3", "s3", "report", "intro\n  # The Title ##\r\n\nbody ü\n", ""},
		{"j4", "s4", "pr", "", ""},
	} {
		if _, err := conn.Exec(ctx, `insert into jobs (id, org_id, owner_id, session_id, kind, state, goal,
			result, branch, created_at, updated_at) values ($1, 'org', 'u', $2, $3, 'succeeded', 'g', $4, $5, $6, $6)`,
			j.id, j.sess, j.kind, j.result, j.branch, at); err != nil {
			t.Fatalf("insert %s: %v", j.id, err)
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
	got, err := s.Jobs().Deliverables(ctx, "org", []string{"j1", "j2", "j3", "j4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["j1"]) != 1 || len(got["j2"]) != 1 || len(got["j3"]) != 1 || len(got["j4"]) != 0 {
		t.Fatalf("migrated inventories = %+v", got)
	}
	if d := got["j1"][0]; d.Key != store.PullRequestKey("codefloe", "acme", "app", "fix") ||
		d.Status != store.DeliverableVerified || d.URL != "https://codefloe.com/acme/app/pulls/9" ||
		d.Platform != "codefloe" || d.Owner != "acme" || d.Name != "app" || d.Branch != "fix" {
		t.Errorf("the found pull request became %+v", d)
	}
	if d := got["j2"][0]; d.Status != store.DeliverableUnverified || d.Branch != "half" || d.URL != "" || d.Detail == "" {
		t.Errorf("the branch with no pull request became %+v", d)
	}
	body := "intro\n  # The Title ##\r\n\nbody ü\n"
	if d := got["j3"][0]; d.Kind != store.DeliverableReport || d.Path != "report.md" ||
		d.Title != "The Title" || d.Size != int64(len(body)) || d.Status != store.DeliverableVerified {
		t.Errorf("the report became %+v", d)
	}
	f, err := s.Jobs().JobFile(ctx, "org", "j3", "report.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Body) != body || f.ContentType != "text/markdown; charset=utf-8" {
		t.Errorf("the report's file = %q (%s)", f.Body, f.ContentType)
	}

	if _, err := postgres.MigrateDown(ctx, dsn); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
}
