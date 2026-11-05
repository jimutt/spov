package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// River's schema is River's own, and spov's is goose's: goose owns every
// domain table and River owns every river_* table, and neither migrates
// the other's.
//
// @decision(DL-003) @decision(DL-046)

// MigrateRiver brings River's own tables up to date. It is separate from
// `spov migrate`, which must never touch them.
//
// @decision(DL-046)
func MigrateRiver(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("supervisor: river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("supervisor: river migrate: %w", err)
	}
	return nil
}

// NewRiverClient builds the queue the supervisor works jobs on.
//
// Every default River would otherwise supply is set explicitly here;
// see supervisor_config.go for why each one is wrong for this.
//
// @decision(DL-046)
func NewRiverClient(pool *pgxpool.Pool, sup *Supervisor) (*river.Client[pgx.Tx], error) {
	if sup == nil {
		return nil, errors.New("supervisor: no supervisor to work jobs with")
	}
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, NewSupervisorWorker(sup)); err != nil {
		return nil, fmt.Errorf("supervisor: registering the worker: %w", err)
	}
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			SupervisorQueue: {MaxWorkers: SupervisorMaxWorkers},
		},
		Workers:              workers,
		JobTimeout:           SupervisorJobTimeout,
		RescueStuckJobsAfter: SupervisorRescueAfter,
		MaxAttempts:          SupervisorMaxAttempts,
	})
}
