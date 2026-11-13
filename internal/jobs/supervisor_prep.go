package jobs

import (
	"context"
	"strings"

	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// prepare runs the control plane's preparation pass over a job's goal
// and records what it found. It returns the job as it should now be
// read, so the caller carries on with the assessment rather than
// re-reading the row. A failed pass is never a failed job: every path
// out of here that is not a successful pass leaves the job opening on
// its original text.
//
// @decision(DL-047)
func (s *Supervisor) prepare(ctx context.Context, job store.Job) store.Job {
	// A job that asked to skip the pass is returned before any call, on
	// every attempt.
	//
	// @decision(DL-138)
	if s.Prepare == nil || job.SkipPrep {
		return job
	}
	// A redelivered attempt must not pay for a second call. Both fields
	// are checked, since a pass may propose a check with no summary.
	if strings.TrimSpace(job.Assessment) != "" || job.CheckProposed {
		return job
	}
	if strings.TrimSpace(job.Goal) == "" {
		return job
	}

	a, err := s.Prepare(ctx, job.Goal)
	if err != nil {
		// Warned about rather than swallowed: this is the half of
		// DL-047's logging split that says a call was attempted and
		// failed. No provider configured at all is a nil Prepare and
		// says nothing here.
		s.warn("supervisor: job %s: preparing the goal: %s", job.ID, redact.Error(err))
		return job
	}

	summary := strings.TrimSpace(a.Summary)
	check := strings.TrimSpace(a.Check)
	if summary == "" && check == "" {
		// The pass ran and had nothing to say. Writing the row anyway
		// would cost a write and change nothing.
		return job
	}

	// Re-read inside the transaction and change only the fields this
	// pass owns. Writing the caller's stale copy back instead would
	// write `queued` over `provisioning`, undoing the move that guards
	// against a second sandbox being bought.
	//
	// @decision(DL-047) @decision(DL-045)
	updated := job
	err = s.Store.Tx(ctx, func(tx store.Store) error {
		current, err := tx.Jobs().Get(ctx, job.OrgID, job.ID)
		if err != nil {
			return err
		}
		current.Assessment = summary
		// A caller's check always wins, re-checked against the current
		// row rather than the copy this pass started from.
		if check != "" && strings.TrimSpace(current.CheckCommand) == "" {
			current.CheckCommand = check
			current.CheckProposed = true
		}
		updated = current
		return tx.Jobs().Update(ctx, current)
	})
	if err != nil {
		// The pass succeeded but could not be recorded, so the job opens
		// on its goal alone: a half-applied assessment must not reach a
		// prompt another replica reads.
		s.warn("supervisor: job %s: recording the assessment: %s", job.ID, redact.Error(err))
		return job
	}
	return updated
}
