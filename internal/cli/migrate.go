package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/store/postgres"
)

// DatabaseURLEnvVar names the database hosted mode connects to. It is
// environment-only: a database URL carries a password, and config files
// are shared and copied.
//
// @decision(DL-003)
const DatabaseURLEnvVar = "SPOV_DATABASE_URL"

// MigrateOnStartEnvVar makes the server migrate before listening. It is
// for compose and development; a real deployment migrates in a step that
// can fail the deploy.
//
// @decision(DL-003)
const MigrateOnStartEnvVar = "SPOV_MIGRATE_ON_START"

// newMigrateCmd is `spov serve migrate`.
//
// @decision(DL-003)
func newMigrateCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply, roll back or report database migrations",
		Long: "Apply, roll back or report the migrations embedded in this binary.\n\n" +
			"The database comes from " + DatabaseURLEnvVar + ". Local mode has no database and needs no migrations.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newMigrateUpCmd(env),
		newMigrateDownCmd(env),
		newMigrateStatusCmd(env),
	)
	return cmd
}

// databaseURL reads the database URL or explains that it is missing.
//
// @decision(DL-003)
func databaseURL(env *Env) (string, error) {
	url := env.Environ.Getenv(DatabaseURLEnvVar)
	if url == "" {
		return "", fmt.Errorf("no database configured: set %s", DatabaseURLEnvVar)
	}
	return url, nil
}

func newMigrateUpCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Apply every pending migration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, err := databaseURL(env)
			if err != nil {
				return err
			}
			applied, err := postgres.MigrateUp(cmd.Context(), url)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprint(env.Out, formatApplied(applied))
			return nil
		},
	}
}

func newMigrateDownCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Roll back the most recent migration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, err := databaseURL(env)
			if err != nil {
				return err
			}
			v, err := postgres.MigrateDown(cmd.Context(), url)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(env.Out, "rolled back %d\n", v)
			return nil
		},
	}
}

func newMigrateStatusCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report which migrations are applied",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, err := databaseURL(env)
			if err != nil {
				return err
			}
			stats, err := postgres.MigrationsStatus(cmd.Context(), url)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprint(env.Out, formatStatus(stats))
			return nil
		},
	}
}

// formatApplied renders what `migrate up` did. Nothing applied is a
// normal outcome, not silence.
//
// @decision(DL-003)
func formatApplied(applied []int64) string {
	if len(applied) == 0 {
		return "already up to date\n"
	}
	var b strings.Builder
	for _, v := range applied {
		fmt.Fprintf(&b, "applied %d\n", v)
	}
	return b.String()
}

// formatStatus renders one line per migration, applied or pending.
//
// @decision(DL-003)
func formatStatus(stats []postgres.MigrationStatus) string {
	var b strings.Builder
	for _, s := range stats {
		state := "pending"
		if s.Applied {
			state = "applied"
		}
		fmt.Fprintf(&b, "%d\t%s\t%s\n", s.Version, state, s.Source)
	}
	return b.String()
}

// migrateOnStart applies migrations before the server listens, when
// SPOV_MIGRATE_ON_START says to. It is for compose and development: a
// real deployment migrates in a step that can fail the deploy before any
// new container serves. Local mode has no database, so asking for it
// there is a mistake worth reporting rather than ignoring.
//
// @decision(DL-003)
func migrateOnStart(ctx context.Context, env *Env, opts serveOptions) error {
	if !truthy(env.Environ.Getenv(MigrateOnStartEnvVar)) {
		return nil
	}
	if opts.Local {
		return fmt.Errorf("%s is set, but --local runs without a database", MigrateOnStartEnvVar)
	}
	url, err := databaseURL(env)
	if err != nil {
		return fmt.Errorf("%s is set but %w", MigrateOnStartEnvVar, err)
	}
	applied, err := postgres.MigrateUp(ctx, url)
	if err != nil {
		return err
	}
	if len(applied) > 0 {
		_, _ = fmt.Fprintf(env.Err, "spov serve: applied %d migration(s)\n", len(applied))
	}
	return nil
}

// truthy reads the spellings people actually write in a compose file.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// migrationsAtHead reports not-ready until every embedded migration has
// been applied, so a replica running ahead of its schema does not serve.
//
// The answer is cached once true: migrations do not go backwards under a
// running replica, and each check otherwise opens a fresh database/sql
// handle — a new connection every time a load balancer probes.
//
// @decision(DL-010)
func migrationsAtHead(url string) func(context.Context) error {
	var mu sync.Mutex
	var reached bool
	return func(ctx context.Context) error {
		mu.Lock()
		done := reached
		mu.Unlock()
		if done {
			return nil
		}
		head, err := postgres.AtHead(ctx, url)
		if err != nil {
			return fmt.Errorf("migrations: %w", err)
		}
		if !head {
			return fmt.Errorf("migrations are behind the embedded set; run `spov serve migrate up`")
		}
		mu.Lock()
		reached = true
		mu.Unlock()
		return nil
	}
}
