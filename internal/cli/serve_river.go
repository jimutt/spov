package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

// The job queue's startup: a thin adapter over River's client that only
// works against a real Postgres, so it sits outside the coverage gate.
// The tuning lives in internal/jobs/supervisor_config.go and the decision
// whether a supervisor exists in newSupervisor (serve.go), both tested.
//
// @decision(DL-045) @decision(DL-046)

// RiverStopTimeout bounds how long shutdown waits for the job queue to
// stop. The jobs are durable, so giving up loses nothing.
//
// @decision(DL-045)
const RiverStopTimeout = 10 * time.Second

// startRiver migrates River's own tables and starts the client that
// works the job queue, returning a function that stops it. A failed
// migration stops the server. The client runs on every replica; only the
// sweep is confined to the leader.
//
// @decision(DL-045) @decision(DL-046) @decision(DL-003)
func (e *Env) startRiver(ctx context.Context, st store.Store, sup *jobs.Supervisor, warn func(string)) (func(), error) {
	pg, ok := st.(*postgres.Store)
	if !ok {
		// Unreachable, since newSupervisor needs Postgres; checked to
		// avoid a nil-pool panic.
		return func() {}, fmt.Errorf("serve: the supervisor needs a database")
	}
	if err := jobs.MigrateRiver(ctx, pg.Pool()); err != nil {
		return func() {}, err
	}
	client, err := jobs.NewRiverClient(pg.Pool(), sup)
	if err != nil {
		return func() {}, err
	}
	if err := client.Start(ctx); err != nil {
		return func() {}, fmt.Errorf("serve: starting the job queue: %w", err)
	}
	// Set only after Start succeeded, or CreateJob would insert onto a
	// client that never came up and the job would never run.
	sup.Queue = func(ctx context.Context, args jobs.JobArgs) error {
		// A job already on the queue is skipped, not refused: JobArgs
		// is unique by args, and River reports a duplicate in the result
		// rather than as an error, so the sweep can re-queue freely.
		//
		// @decision(DL-045)
		_, err := client.Insert(ctx, args, nil)
		return err
	}
	return func() {
		// A stop that hangs must not hold the shutdown: the jobs are
		// durable and the next replica picks them up.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RiverStopTimeout)
		defer cancel()
		if err := client.Stop(sctx); err != nil {
			warn("stopping the job queue: " + err.Error())
		}
	}, nil
}
