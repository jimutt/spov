package server

import (
	"context"
	"time"

	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// What the server records about sessions, and the shared-state snapshot
// the reconciler lock holder publishes.
//
// @decision(DL-011)

// SnapshotInterval is how often the lock holder refreshes shared state.
const SnapshotInterval = 30 * time.Second

// launchStep names the launcher step a launch was in when it failed;
// setup when it had not reached one.
func launchStep(running string) telemetry.LaunchStep {
	switch session.Step(running) {
	case session.StepTokens:
		return telemetry.StepTokens
	case session.StepSandbox:
		return telemetry.StepSandbox
	case session.StepServer:
		return telemetry.StepServer
	default:
		return telemetry.StepSetup
	}
}

// recordStop records a session's end at stopped, by the caller whose
// MarkStopped took it. A job's session is told apart by its job, never
// by its name, and its lifetime runs from the job's start, since its row
// is created when the job is queued. A job that never started records
// no lifetime.
//
// @decision(DL-011)
func recordStop(ctx context.Context, m *telemetry.Metrics, st store.Store, sess store.Session, stopped time.Time, reason telemetry.Termination) {
	if m == nil {
		return
	}
	// The row is written; a caller that has gone does not undo it.
	ctx = context.WithoutCancel(ctx)
	kind, from := telemetry.KindInteractive, sess.CreatedAt
	// A resumed session's lifetime is its run's.
	//
	// @decision(DL-085)
	if run, err := st.Runs().Current(ctx, sess.OrgID, sess.ID); err == nil && run.N > 1 {
		from = run.StartedAt
	}
	if job, err := st.Jobs().BySession(ctx, sess.OrgID, sess.ID); err == nil {
		kind, from = telemetry.KindJob, time.Time{}
		if job.StartedAt != nil {
			from = *job.StartedAt
		}
	}
	var lifetime time.Duration
	if !from.IsZero() {
		lifetime = stopped.Sub(from)
	}
	m.SessionStopped(ctx, kind, reason, lifetime)
}

// RunMetricsSnapshot publishes shared state for as long as ctx lasts,
// which is as long as this process holds the reconciler lock, and
// withdraws it when ctx ends. It reads only the store.
//
// @decision(DL-011) @decision(DL-018)
func (s *Service) RunMetricsSnapshot(ctx context.Context) {
	m := s.deps.Metrics
	if m == nil {
		return
	}
	defer m.ClearShared()
	t := time.NewTicker(SnapshotInterval)
	defer t.Stop()
	for {
		if snap, err := s.metricsSnapshot(ctx); err == nil {
			m.SetShared(snap)
		} else if ctx.Err() == nil {
			s.deps.Warn("metrics: reading shared state: " + err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// metricsSnapshot counts unfinished jobs by state, live sessions by
// state and kind, and the oldest queued job's wait.
func (s *Service) metricsSnapshot(ctx context.Context) (telemetry.Snapshot, error) {
	jobs, err := s.deps.Store.Jobs().ListLive(ctx)
	if err != nil {
		return telemetry.Snapshot{}, err
	}
	sessions, err := s.deps.Store.Sessions().ListLive(ctx)
	if err != nil {
		return telemetry.Snapshot{}, err
	}
	now := s.deps.Now()
	snap := telemetry.Snapshot{Jobs: map[store.JobState]int64{}, Sessions: map[telemetry.SessionKey]int64{}}
	jobSessions := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		snap.Jobs[j.State]++
		jobSessions[j.SessionID] = true
		if j.State == store.JobQueued {
			snap.OldestQueued = max(snap.OldestQueued, now.Sub(j.CreatedAt))
		}
	}
	// A live session whose job has finished is still a job's: a held
	// sync job's, or one whose terminate failed. Read once per org.
	//
	// @decision(DL-101) @decision(DL-011)
	others := map[string][]string{}
	for _, sess := range sessions {
		if !jobSessions[sess.ID] {
			others[sess.OrgID] = append(others[sess.OrgID], sess.ID)
		}
	}
	for org, ids := range others {
		finished, err := s.deps.Store.Jobs().BySessions(ctx, org, ids)
		if err != nil {
			return telemetry.Snapshot{}, err
		}
		for id := range finished {
			jobSessions[id] = true
		}
	}
	for _, sess := range sessions {
		kind := telemetry.KindInteractive
		if jobSessions[sess.ID] {
			kind = telemetry.KindJob
		}
		snap.Sessions[telemetry.SessionKey{State: sess.State, Kind: kind}]++
	}
	return snap, nil
}
