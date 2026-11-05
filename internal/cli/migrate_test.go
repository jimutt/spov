package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/store/postgres"
)

// TestMigrateWithoutADatabaseSaysWhatToSet: the commands must name the
// variable rather than fail inside pgx with a parse error.
//
// @decision(DL-003)
func TestMigrateWithoutADatabaseSaysWhatToSet(t *testing.T) {
	for _, sub := range []string{"up", "down", "status"} {
		t.Run(sub, func(t *testing.T) {
			_, _, err := run(t, Env{}, "serve", "migrate", sub)
			if err == nil {
				t.Fatal("a migration without a database succeeded")
			}
			if !strings.Contains(err.Error(), DatabaseURLEnvVar) {
				t.Errorf("error %q does not name %s", err, DatabaseURLEnvVar)
			}
		})
	}
}

// TestMigrateReportsAnUnreachableDatabase: the URL is present but wrong,
// which must fail with the connection's reason rather than hang.
//
// @decision(DL-003)
func TestMigrateReportsAnUnreachableDatabase(t *testing.T) {
	env := Env{Environ: config.MapEnv{Vars: map[string]string{
		DatabaseURLEnvVar: "://not a database url",
	}}}
	_, _, err := run(t, env, "serve", "migrate", "status")
	if err == nil {
		t.Fatal("an unparseable database url succeeded")
	}
}

// TestMigrateIsUnderServe keeps the command where the decision puts it,
// since `spov migrate` at the top level would suggest local mode needs it.
//
// @decision(DL-003)
func TestMigrateIsUnderServe(t *testing.T) {
	out, _, err := run(t, Env{}, "serve", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "migrate") {
		t.Error("serve --help does not mention migrate")
	}
	if _, _, err := run(t, Env{}, "migrate", "up"); err == nil {
		t.Error("spov migrate exists at the top level")
	}
}

// TestMigrateHelpListsEverySubcommand.
//
// @decision(DL-003)
func TestMigrateHelpListsEverySubcommand(t *testing.T) {
	out, _, err := run(t, Env{}, "serve", "migrate", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"up", "down", "status"} {
		if !strings.Contains(out, sub) {
			t.Errorf("migrate --help does not mention %q", sub)
		}
	}
	if !strings.Contains(out, DatabaseURLEnvVar) {
		t.Errorf("migrate --help does not name %s", DatabaseURLEnvVar)
	}
}

// TestFormatApplied.
//
// @decision(DL-003)
func TestFormatApplied(t *testing.T) {
	if got := formatApplied(nil); got != "already up to date\n" {
		t.Errorf("nothing applied printed %q", got)
	}
	got := formatApplied([]int64{1, 2})
	if got != "applied 1\napplied 2\n" {
		t.Errorf("got %q", got)
	}
}

// TestFormatStatus.
//
// @decision(DL-003)
func TestFormatStatus(t *testing.T) {
	if got := formatStatus(nil); got != "" {
		t.Errorf("no migrations printed %q", got)
	}
	got := formatStatus([]postgres.MigrationStatus{
		{Version: 1, Source: "00001_init.sql", Applied: true},
		{Version: 2, Source: "00002_next.sql"},
	})
	want := "1\tapplied\t00001_init.sql\n2\tpending\t00002_next.sql\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestMigrateWithNoSubcommandPrintsHelp: `spov serve migrate` alone must
// explain itself rather than doing something.
//
// @decision(DL-003)
func TestMigrateWithNoSubcommandPrintsHelp(t *testing.T) {
	out, _, err := run(t, Env{}, "serve", "migrate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "up") || !strings.Contains(out, "status") {
		t.Errorf("bare migrate printed %q", out)
	}
}

// TestTruthy covers the spellings a compose file actually contains.
//
// @decision(DL-003)
func TestTruthy(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		if !truthy(v) {
			t.Errorf("truthy(%q) = false", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "maybe"} {
		if truthy(v) {
			t.Errorf("truthy(%q) = true", v)
		}
	}
}

// TestMigrateOnStartIsRefusedInLocalMode: local mode has no database, so
// asking for a migration there is a mistake rather than a no-op.
//
// @decision(DL-003)
func TestMigrateOnStartIsRefusedInLocalMode(t *testing.T) {
	env := Env{Environ: config.MapEnv{Vars: map[string]string{MigrateOnStartEnvVar: "true"}}}
	err := migrateOnStart(context.Background(), &env, serveOptions{Local: true})
	if err == nil {
		t.Fatal("migrate-on-start was accepted with --local")
	}
	if !strings.Contains(err.Error(), "--local") {
		t.Errorf("error %q does not mention --local", err)
	}
}

// TestMigrateOnStartIsOffByDefault: a server with no database must start
// normally, which is every local run.
//
// @decision(DL-003)
func TestMigrateOnStartIsOffByDefault(t *testing.T) {
	env := Env{Environ: config.MapEnv{Vars: map[string]string{}}}
	if err := migrateOnStart(context.Background(), &env, serveOptions{}); err != nil {
		t.Errorf("an unset %s failed the start: %v", MigrateOnStartEnvVar, err)
	}
	// Explicitly off is also off.
	env.Environ = config.MapEnv{Vars: map[string]string{MigrateOnStartEnvVar: "false"}}
	if err := migrateOnStart(context.Background(), &env, serveOptions{}); err != nil {
		t.Errorf("%s=false failed the start: %v", MigrateOnStartEnvVar, err)
	}
}

// TestMigrateOnStartNeedsADatabase: set but with nothing to connect to
// must name the missing variable.
//
// @decision(DL-003)
func TestMigrateOnStartNeedsADatabase(t *testing.T) {
	env := Env{Environ: config.MapEnv{Vars: map[string]string{MigrateOnStartEnvVar: "1"}}}
	err := migrateOnStart(context.Background(), &env, serveOptions{})
	if err == nil {
		t.Fatal("migrate-on-start succeeded with no database")
	}
	if !strings.Contains(err.Error(), DatabaseURLEnvVar) {
		t.Errorf("error %q does not name %s", err, DatabaseURLEnvVar)
	}
}

// TestMigrationsAtHeadCachesSuccess: a readiness probe runs every few
// seconds and each uncached check opens a fresh database handle.
// Migrations do not go backwards under a running replica, so the answer
// is asked for once.
//
// @decision(DL-010)
func TestMigrationsAtHeadCachesSuccess(t *testing.T) {
	// An unreachable database, so every real check fails. The first
	// call must report it rather than caching a false success.
	check := migrationsAtHead("postgres://spov:spov@127.0.0.1:1/spov?sslmode=disable&connect_timeout=1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := check(ctx); err == nil {
		t.Fatal("an unreachable database reported ready")
	}
	// And it keeps reporting it: a failure is never cached, or a
	// replica that came up before its migrations would serve forever.
	if err := check(ctx); err == nil {
		t.Error("a second check reported ready after a failure")
	}
}
