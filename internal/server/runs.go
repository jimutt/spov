package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// runPlan is which run of its session a launch is. The zero value is a
// first launch, run 1.
//
// @decision(DL-085)
type runPlan struct {
	N      int
	Origin store.RunOrigin
	// From is the exit snapshot a resume boots, by its image ID.
	From string
}

func (p runPlan) number() int { return max(p.N, 1) }

func (p runPlan) origin() store.RunOrigin {
	if p.Origin == "" {
		return store.RunFromLaunch
	}
	return p.Origin
}

// runOf is the run a launch that reached running records.
//
// @decision(DL-085)
func (s *Service) runOf(sess store.Session, plan runPlan, sandboxID, image string, snapshot bool) store.Run {
	now := s.deps.Now()
	state := store.SnapshotNone
	if snapshot {
		state = store.SnapshotPending
	}
	return store.Run{
		OrgID: sess.OrgID, SessionID: sess.ID, OwnerID: sess.OwnerID, N: plan.number(),
		SandboxID: sandboxID, Booted: image, BootedImageID: plan.From, OpenCode: s.deps.Defaults.OpenCodeVersion,
		Origin: plan.origin(), StartedAt: sess.CreatedAt, RunningAt: &now, SnapshotState: state,
	}
}

// recordRun writes a launched run: over the row a resume inserted for
// it, keeping that row's start, or as a new run.
//
// @decision(DL-085)
func recordRun(ctx context.Context, tx store.Store, run store.Run) error {
	cur, err := tx.Runs().Current(ctx, run.OrgID, run.SessionID)
	switch {
	case err == nil && cur.N == run.N:
		run.StartedAt = cur.StartedAt
		return tx.Runs().Update(ctx, run)
	case err == nil || errors.Is(err, store.ErrNotFound):
		return tx.Runs().Insert(ctx, run)
	}
	return err
}

// endFailedRun ends the run a resume inserted, when its launch failed
// before the session's row could say so through MarkStopped.
//
// @decision(DL-085)
func (s *Service) endFailedRun(ctx context.Context, sess store.Session, plan runPlan, at time.Time) {
	cur, err := s.deps.Store.Runs().Current(ctx, sess.OrgID, sess.ID)
	if err != nil || cur.N != plan.number() || cur.EndedAt != nil {
		return
	}
	cur.EndedAt = &at
	if cur.SandboxID == "" {
		cur.SnapshotState = store.SnapshotNone
	}
	if err := s.deps.Store.Runs().Update(ctx, cur); err != nil {
		s.deps.Warn(fmt.Sprintf("launch %s: end its run: %s", sess.ID, redact.Error(err)))
	}
}
