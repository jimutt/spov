package jobs

import (
	"context"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// What the supervisor records. An ending is recorded only where this
// process's write moved the job into a terminal state, so a redelivered
// attempt counts nothing.
//
// @decision(DL-011)

// endedBy names what ended a judged run, from spov's wall-clock marker
// and the loop's own stop announcement. A run with neither was
// concluded by the backstop.
func endedBy(events []store.Event) telemetry.EndedBy {
	if boundNote(events) != "" {
		return telemetry.EndedWallClock
	}
	reason, found := "", false
	for _, e := range events {
		if isLoopStopped(e) {
			reason, found = stopReason(e), true
		}
	}
	if !found {
		return telemetry.EndedAbandoned
	}
	switch reason {
	case opencode.CompletionReason:
		return telemetry.EndedDeclared
	case opencode.DeniedReason:
		return telemetry.EndedDenied
	case "wall-clock":
		return telemetry.EndedWallClock
	default:
		return telemetry.EndedBy(reason)
	}
}

// endedBeforeJudging names what ended a job the supervisor moved to a
// terminal state itself, outside a verdict: a failed launch, or a
// cancel.
func endedBeforeJudging(from, to store.JobState) telemetry.EndedBy {
	switch {
	case to == store.JobCancelled:
		return telemetry.EndedCancelled
	case from == store.JobQueued || from == store.JobProvisioning:
		return telemetry.EndedLaunchFailed
	default:
		return telemetry.EndedOther
	}
}

// recordEnding records a job's terminal state, its clock and, for a
// judged run, its gates and how long delivery took.
func (s *Supervisor) recordEnding(ctx context.Context, job store.Job, by telemetry.EndedBy, v *Verdict, delivered time.Duration) {
	if s.Metrics == nil {
		return
	}
	s.Metrics.JobEnded(ctx, job.Kind, job.State, by, job.ClockElapsed(s.now()))
	if v == nil {
		return
	}
	for _, g := range v.Gates {
		s.Metrics.JobGate(ctx, g.Gate, string(g.Outcome))
	}
	// @decision(DL-050)
	for _, d := range v.Deliverables {
		s.Metrics.JobDeliverable(ctx, d.Kind, d.Status)
	}
	s.Metrics.JobDelivered(ctx, delivered)
}
