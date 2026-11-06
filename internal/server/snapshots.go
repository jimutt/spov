package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// SnapshotSettings is how long exit snapshots are kept and how many.
//
// @decision(DL-086)
type SnapshotSettings struct {
	// Retention is how long after its run ended a snapshot is kept.
	Retention time.Duration
	// MaxPerUser is how many kept snapshots one user may have; 0 is no
	// cap.
	MaxPerUser int
}

// The bounds and defaults of the snapshot settings.
//
// @decision(DL-086)
const (
	DefaultSnapshotRetention  = 168 * time.Hour
	DefaultSnapshotsPerUser   = 20
	MaxSnapshotRetention      = 720 * time.Hour
	MinSnapshotRetention      = time.Hour
	SnapshotCollectInterval   = time.Minute
	SnapshotCollectWait       = 30 * time.Second
	SnapshotCollectPassBudget = 50 * time.Second
	SnapshotCollectParallel   = 8
	// SnapshotGiveUp is how long after its run ended a snapshot may stay
	// pending before it is marked failed.
	SnapshotGiveUp = 30 * time.Minute
	// SnapshotPruneInterval is how often pruning runs between the passes
	// a newly kept snapshot starts.
	SnapshotPruneInterval = time.Hour
)

// Validate refuses settings DL-086 rules out.
//
// @decision(DL-086)
func (c SnapshotSettings) Validate() error {
	if c.Retention < MinSnapshotRetention || c.Retention > MaxSnapshotRetention {
		return fmt.Errorf("snapshots.retention %s is outside %s to %s; Modal keeps an exit snapshot 30 days",
			c.Retention, MinSnapshotRetention, MaxSnapshotRetention)
	}
	if c.MaxPerUser < 0 {
		return fmt.Errorf("snapshots.max_per_user %d is negative; 0 means no cap", c.MaxPerUser)
	}
	return nil
}

// RunSnapshots collects ended runs' exit snapshots every
// SnapshotCollectInterval, and prunes, until ctx ends. It runs where
// the reconciler runs.
//
// @decision(DL-085) @decision(DL-086)
func (s *Service) RunSnapshots(ctx context.Context) {
	if s.deps.Provider == nil {
		return
	}
	var pruned time.Time
	for {
		kept := s.CollectSnapshots(ctx)
		if now := s.deps.Now(); kept > 0 || now.Sub(pruned) >= SnapshotPruneInterval {
			s.PruneSnapshots(ctx)
			pruned = now
		}
		jitter := time.Duration(rand.Int64N(int64(SnapshotCollectInterval / 10)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(SnapshotCollectInterval + jitter):
		}
	}
}

// CollectSnapshots asks the provider for the exit snapshot of every
// ended run still pending, and reports how many it kept.
//
// @decision(DL-085)
func (s *Service) CollectSnapshots(ctx context.Context) int {
	runs, err := s.deps.Store.Runs().Snapshots(ctx, store.SnapshotPending)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("snapshots: list pending: %s", redact.Error(err)))
		return 0
	}
	pass, cancel := context.WithTimeout(ctx, SnapshotCollectPassBudget)
	defer cancel()
	var (
		mu   sync.Mutex
		kept int
		wg   sync.WaitGroup
		gate = make(chan struct{}, SnapshotCollectParallel)
	)
	for _, run := range runs {
		select {
		case gate <- struct{}{}:
		case <-pass.Done():
			wg.Wait()
			return kept
		}
		wg.Go(func() {
			defer func() { <-gate }()
			if s.collect(pass, run) {
				mu.Lock()
				kept++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return kept
}

// collect settles one pending run, and reports whether it kept a
// snapshot that pruning may now supersede with.
func (s *Service) collect(ctx context.Context, run store.Run) bool {
	outcome, kept := s.settleSnapshot(ctx, &run, SnapshotCollectWait)
	if outcome == "" {
		return false
	}
	if err := s.deps.Store.Runs().Update(context.WithoutCancel(ctx), run); err != nil {
		s.deps.Warn(fmt.Sprintf("snapshots: session %s run %d: record: %s", run.SessionID, run.N, redact.Error(err)))
		return false
	}
	s.deps.Metrics.SnapshotCollected(ctx, outcome)
	// A run that never reached running holds no work.
	if kept && !run.Ran() {
		s.deleteSnapshots(ctx, []store.Run{run}, "unused")
		return false
	}
	return kept
}

// settleSnapshot asks for run's snapshot, waiting at most wait, and
// moves run on: kept, none or failed, with the outcome's metric label.
// An empty outcome leaves the run pending.
//
// @decision(DL-085)
func (s *Service) settleSnapshot(ctx context.Context, run *store.Run, wait time.Duration) (outcome string, kept bool) {
	now := s.deps.Now()
	if run.SandboxID == "" {
		run.SnapshotState = store.SnapshotNone
		return "none", false
	}
	id, err := s.deps.Provider.ExitSnapshot(ctx, run.SandboxID, wait)
	switch {
	case err == nil:
		run.Snapshot, run.SnapshotState, run.SnapshotAt = id, store.SnapshotKept, &now
		return "kept", true
	case errors.Is(err, sandbox.ErrNoExitSnapshot):
		run.SnapshotState = store.SnapshotNone
		return "none", false
	case errors.Is(err, sandbox.ErrSnapshotFailed), errors.Is(err, sandbox.ErrNotFound):
		run.SnapshotState = store.SnapshotFailed
		return "failed", false
	}
	if !errors.Is(err, sandbox.ErrSnapshotPending) && ctx.Err() == nil {
		s.deps.Warn(fmt.Sprintf("snapshots: session %s run %d: %s", run.SessionID, run.N, redact.Error(err)))
	}
	if run.EndedAt != nil && now.Sub(*run.EndedAt) > SnapshotGiveUp {
		run.SnapshotState = store.SnapshotFailed
		return "failed", false
	}
	return "", false
}

// PruneSnapshots marks deleted every kept snapshot past the retention,
// superseded by a later run's, or beyond its owner's cap, then deletes
// the images of every run so marked, retrying earlier failures.
//
// @decision(DL-086)
func (s *Service) PruneSnapshots(ctx context.Context) {
	settings := s.deps.Snapshots
	runs, err := s.deps.Store.Runs().Snapshots(ctx, store.SnapshotKept, store.SnapshotDeleted)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("snapshots: list kept: %s", redact.Error(err)))
		return
	}
	var kept, retry []store.Run
	for _, r := range runs {
		switch {
		case r.SnapshotState == store.SnapshotDeleted && r.Snapshot != "":
			retry = append(retry, r)
		case r.SnapshotState == store.SnapshotKept:
			kept = append(kept, r)
		}
	}
	s.deleteImages(ctx, retry)
	if settings == nil {
		// Off: what is kept ages out under the default retention.
		settings = &SnapshotSettings{Retention: DefaultSnapshotRetention}
	}
	reasons := pruneReasons(kept, *settings, s.deps.Now())
	for reason, rs := range reasons {
		s.deleteSnapshots(ctx, rs, reason)
	}
}

// pruneReasons sorts kept (oldest end first) into what pruning removes,
// by reason.
//
// @decision(DL-086)
func pruneReasons(kept []store.Run, settings SnapshotSettings, now time.Time) map[string][]store.Run {
	out := map[string][]store.Run{}
	newest := map[string]int{}
	for _, r := range kept {
		newest[r.SessionID] = max(newest[r.SessionID], r.N)
	}
	perOwner := map[string][]store.Run{}
	for _, r := range kept {
		switch {
		case now.Sub(*r.EndedAt) > settings.Retention:
			out["retention"] = append(out["retention"], r)
		case r.N < newest[r.SessionID]:
			out["superseded"] = append(out["superseded"], r)
		default:
			perOwner[r.OwnerID] = append(perOwner[r.OwnerID], r)
		}
	}
	if settings.MaxPerUser > 0 {
		for _, rs := range perOwner {
			sort.SliceStable(rs, func(i, j int) bool { return rs[i].EndedAt.Before(*rs[j].EndedAt) })
			if over := len(rs) - settings.MaxPerUser; over > 0 {
				out["cap"] = append(out["cap"], rs[:over]...)
			}
		}
	}
	return out
}

// deleteSnapshots marks each run's snapshot deleted, then deletes its
// image.
//
// @decision(DL-086)
func (s *Service) deleteSnapshots(ctx context.Context, runs []store.Run, reason string) {
	var marked []store.Run
	for _, r := range runs {
		r.SnapshotState = store.SnapshotDeleted
		if err := s.deps.Store.Runs().Update(ctx, r); err != nil {
			s.deps.Warn(fmt.Sprintf("snapshots: session %s run %d: mark deleted: %s", r.SessionID, r.N, redact.Error(err)))
			continue
		}
		s.deps.Metrics.SnapshotPruned(ctx, reason)
		marked = append(marked, r)
	}
	s.deleteImages(ctx, marked)
}

// deleteImages deletes the images of runs marked deleted, and forgets
// each image once it is gone.
func (s *Service) deleteImages(ctx context.Context, runs []store.Run) {
	del, ok := s.deps.Provider.(sandbox.ImageDeleter)
	if !ok {
		return
	}
	for _, r := range runs {
		if err := del.DeleteImage(ctx, r.Snapshot); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			s.deps.Warn(fmt.Sprintf("snapshots: delete %s: %s", r.Snapshot, redact.Error(err)))
			continue
		}
		r.Snapshot = ""
		if err := s.deps.Store.Runs().Update(ctx, r); err != nil {
			s.deps.Warn(fmt.Sprintf("snapshots: session %s run %d: forget the deleted image: %s", r.SessionID, r.N, redact.Error(err)))
		}
	}
}
