package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// The restart sweep re-queues unfinished jobs River does not know about:
// one whose enqueue failed after its rows were committed (CreateJob keeps
// the row so the user can see and cancel it), or one queued before the
// River tables were reset. Re-queueing is safe because the run loop is
// resumable and adoption is by tag.
//
// @decision(DL-045) @decision(DL-046)

// SweepInterval is how often the supervisor looks for unfinished jobs
// nothing is working.
//
// @decision(DL-045)
const SweepInterval = 5 * time.Minute

// Sweep re-queues every unfinished job. It crosses orgs through ListLive,
// since a restarted replica has no principal to scope by, and runs on the
// leader alone.
//
// @decision(DL-045)
func (s *Supervisor) Sweep(ctx context.Context) error {
	if s.Queue == nil {
		return nil
	}
	jobs, err := s.Store.Jobs().ListLive(ctx)
	if err != nil {
		return fmt.Errorf("supervisor: listing unfinished jobs: %w", err)
	}
	var failed int
	for _, job := range jobs {
		// ListLive returns delivering jobs, which are not re-queued:
		// re-delivering one would read its sandbox on every sweep.
		//
		// @decision(DL-045) @decision(DL-018)
		if job.State == store.JobDelivering {
			continue
		}
		if err := s.Enqueue(ctx, job); err != nil {
			// Reported per job rather than abandoning the sweep: one
			// job that cannot be queued must not stop the rest from
			// being recovered.
			failed++
			s.warn("supervisor: re-queueing job %s: %s", job.ID, redact.Error(err))
		}
	}
	if failed > 0 {
		return fmt.Errorf("supervisor: %d of %d jobs could not be re-queued", failed, len(jobs))
	}
	return nil
}

// RunSweep sweeps on a timer until the context ends. The first sweep is
// immediate, since a replica that has just taken the leader lock has
// most likely just restarted.
//
// @decision(DL-045)
func (s *Supervisor) RunSweep(ctx context.Context) {
	for {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.warn("%s", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.sweepInterval()):
		}
	}
}

func (s *Supervisor) sweepInterval() time.Duration {
	if s.SweepInterval > 0 {
		return s.SweepInterval
	}
	return SweepInterval
}
