package jobs

import (
	"context"
	"fmt"
	"sort"

	"github.com/jimutt/spov/internal/sandbox"
)

// Modal offers no lifecycle events, so a tag query is the only way to ask
// what is still running: a River retry or rescue can deliver a job whose
// sandbox is already up, and a restarted server has no other way to find
// the jobs it left behind.
//
// @decision(DL-046)

// findJobSandbox returns the live sandbox running this job, if there is
// one. A stopped sandbox is not a candidate: it cannot be re-attached to.
//
// @decision(DL-046)
func findJobSandbox(ctx context.Context, p sandbox.Provider, jobID string) (sandbox.Info, bool, error) {
	if jobID == "" {
		return sandbox.Info{}, false, fmt.Errorf("supervisor: no job id to look up")
	}
	found, err := p.List(ctx, sandbox.JobTags(jobID))
	if err != nil {
		return sandbox.Info{}, false, fmt.Errorf("supervisor: listing sandboxes for job %s: %w", jobID, err)
	}
	var live []sandbox.Info
	for _, info := range found {
		if info.State == sandbox.StateRunning {
			live = append(live, info)
		}
	}
	if len(live) == 0 {
		return sandbox.Info{}, false, nil
	}
	// More than one means a duplicate slipped through. The oldest is kept
	// since its opening turn is furthest along and any recorder is
	// already streaming it.
	sort.Slice(live, func(i, j int) bool { return live[i].CreatedAt.Before(live[j].CreatedAt) })
	return live[0], true, nil
}

// adoptOrCreate returns the sandbox for this job, reusing a live one
// rather than provisioning a second. It deliberately does not terminate a
// duplicate it finds; deciding which of two live sandboxes is the real
// one is the reconciler's job (DL-004).
//
// @decision(DL-046)
func (s *Supervisor) adoptOrCreate(ctx context.Context, jobID string, create func(context.Context) (string, error)) (id string, adopted bool, err error) {
	if s.Provider != nil {
		existing, ok, err := findJobSandbox(ctx, s.Provider, jobID)
		if err != nil {
			// A listing failure must not become a second sandbox.
			return "", false, err
		}
		if ok {
			return existing.ID, true, nil
		}
	}
	id, err = create(ctx)
	if err != nil {
		return "", false, err
	}
	return id, false, nil
}
