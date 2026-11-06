package store

import (
	"context"
	"time"
)

// RunOrigin is how a run's sandbox started.
//
// @decision(DL-085)
type RunOrigin string

const (
	RunFromLaunch     RunOrigin = "launch"
	RunFromSnapshot   RunOrigin = "snapshot"
	RunFromTranscript RunOrigin = "transcript"
)

// SnapshotState is where a run's exit snapshot is.
//
// @decision(DL-085)
type SnapshotState string

const (
	// SnapshotNone is a run whose sandbox was created without one.
	SnapshotNone SnapshotState = "none"
	// SnapshotPending is a run whose snapshot is not collected yet: its
	// sandbox is still running, or ended moments ago.
	SnapshotPending SnapshotState = "pending"
	SnapshotKept    SnapshotState = "kept"
	SnapshotFailed  SnapshotState = "failed"
	// SnapshotDeleted is marked before the image is deleted, and a run
	// marked so is never booted.
	SnapshotDeleted SnapshotState = "deleted"
)

// Run is one sandbox a session had, numbered from 1. A launch is run 1
// and each resume the next.
//
// @decision(DL-085)
type Run struct {
	OrgID     string
	SessionID string
	// OwnerID is the session's owner, so pruning can count a user's
	// snapshots without reading every session.
	OwnerID string
	N       int
	// SandboxID is empty until the run's sandbox exists.
	SandboxID string
	// Booted is the image name the run is recorded against, and
	// BootedImageID the snapshot it booted from, if it did.
	Booted        string
	BootedImageID string
	// OpenCode is the OpenCode version the run's sandbox ran.
	OpenCode  string
	Origin    RunOrigin
	StartedAt time.Time
	// RunningAt is when the run first reached running; nil if it never
	// did. EndedAt is nil while the run is live.
	RunningAt *time.Time
	EndedAt   *time.Time
	// Snapshot is the exit snapshot's image ID once it is kept.
	Snapshot      string
	SnapshotState SnapshotState
	SnapshotAt    *time.Time
}

// Ran reports whether the run ever reached running.
func (r Run) Ran() bool { return r.RunningAt != nil }

// Stretch is how long the run's sandbox ran inside [since, now].
//
// @decision(DL-085) @decision(DL-009)
func (r Run) Stretch(since, now time.Time) time.Duration {
	start, end := r.StartedAt, now
	if start.IsZero() {
		return 0
	}
	if start.Before(since) {
		start = since
	}
	if r.EndedAt != nil && r.EndedAt.Before(end) {
		end = *r.EndedAt
	}
	if !end.After(start) {
		return 0
	}
	return end.Sub(start)
}

// RunStore holds sessions' runs. A run is written by its launch or
// resume, ended by MarkStopped in the same write as the session, and
// its snapshot moved on by collection and pruning.
//
// @decision(DL-085)
type RunStore interface {
	// Insert adds a run. A run already stored for (SessionID, N), or a
	// session that does not exist in the org, is ErrConflict or
	// ErrNotFound.
	Insert(ctx context.Context, r Run) error
	// Update replaces the stored run (SessionID, N); OrgID, OwnerID,
	// N and StartedAt are kept.
	Update(ctx context.Context, r Run) error
	// Current is the session's newest run, or ErrNotFound.
	Current(ctx context.Context, orgID, sessionID string) (Run, error)
	// List is each session's runs, oldest first, keyed by session ID,
	// in one read. A session with none is absent.
	List(ctx context.Context, orgID string, sessionIDs []string) (map[string][]Run, error)
	// Snapshots is every ended run, across orgs, whose snapshot is in
	// one of states, oldest end first. Collection and pruning read it.
	Snapshots(ctx context.Context, states ...SnapshotState) ([]Run, error)
	// Reopen clears the current run's end, for a session the reconciler
	// found running again, and puts a snapshot the sandbox was created
	// with back to pending.
	Reopen(ctx context.Context, orgID, sessionID string) error
}
