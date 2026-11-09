package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// SyncState is what a sync job's repository measured at delivery: where
// it is, how much changed since the seed's base, and the size its export
// would have. It carries no contents.
//
// @decision(DL-101)
type SyncState struct {
	Branch     string
	HeadCommit string
	Commits    int
	Files      int
	Bytes      int64
	// Rewritten says the base is no longer an ancestor of HEAD.
	Rewritten bool
}

// DefaultSyncHold is how long a finished sync job's sandbox is kept for
// its state to be taken when the request names no hold, and
// MinSyncHold the shortest hold a request may ask for.
//
// @decision(DL-101)
const (
	DefaultSyncHold = 30 * time.Minute
	MinSyncHold     = time.Minute
)

// InspectSync reads a sync job's repository in its sandbox, once, at
// delivery.
//
// @decision(DL-101)
type InspectSync func(ctx context.Context, job store.Job) (SyncState, error)

// endSyncRun stops a sync job's loop and turn before its repository is
// read, so the state judged is the state taken. Best-effort, as at the
// wall clock: a sandbox that cannot be reached is read as it is.
//
// @decision(DL-101)
func (s *Supervisor) endSyncRun(ctx context.Context, job store.Job) {
	if s.EndRun == nil {
		return
	}
	if err := s.EndRun(ctx, job); err != nil {
		s.warn("supervisor: job %s: stopping its run before reading the repository: %s", job.ID, redact.Error(err))
	}
}

// collectSync measures a sync job's repository and writes its one
// member. Gated by MarkerCollected, so a pass that broke off reads again.
//
// @decision(DL-101)
func (s *Supervisor) collectSync(ctx context.Context, job store.Job) error {
	if job.Kind != store.JobKindSync {
		return nil
	}
	events, err := s.jobEvents(ctx, job)
	if err != nil {
		return err
	}
	if spovNote(events, MarkerCollected) != "" {
		return nil
	}
	member := store.Deliverable{JobID: job.ID, Key: store.SyncDeliverableKey, Kind: store.DeliverableSync}
	if sess, err := s.Store.Sessions().Get(ctx, job.OrgID, job.SessionID); err == nil && len(sess.Request.Repos) > 0 {
		member.Platform = sess.Request.Repos[0].Platform
		member.Owner, member.Name = sess.Request.Repos[0].Owner, sess.Request.Repos[0].Name
	}
	if s.InspectSync == nil {
		return nil
	}
	state, err := s.InspectSync(ctx, job)
	if err != nil {
		why := err
		if errors.Is(err, ErrSandboxGone) {
			why = errors.New("the sandbox had already stopped, so the repository could not be read")
		} else {
			s.warn("supervisor: job %s: reading the repository: %s", job.ID, redact.Error(err))
		}
		member.Status, member.Detail = store.DeliverableUnverified, redact.Error(why)
		s.putMember(ctx, job, member)
		s.markCollectionFailed(ctx, job, why)
		return nil
	}
	member = syncMember(member, state)
	s.putMember(ctx, job, member)
	if s.Mark != nil {
		if err := s.Mark(ctx, job, MarkerCollected, "Read the repository: "+member.Detail); err != nil {
			s.warn("supervisor: job %s: recording the collection: %s", job.ID, redact.Error(err))
		}
	}
	return nil
}

// syncMember is the member a measured repository makes: verified when
// something changed within the export's bound, over_limit past it, and
// missing when nothing changed.
//
// @decision(DL-101)
func syncMember(m store.Deliverable, st SyncState) store.Deliverable {
	m.Branch, m.HeadCommit = st.Branch, st.HeadCommit
	m.Commits, m.Files, m.Size = st.Commits, st.Files, st.Bytes
	switch {
	case st.Commits == 0 && st.Files == 0 && !st.Rewritten:
		m.Status, m.Detail = store.DeliverableMissing, "the repository is unchanged since the job started"
	case st.Bytes > git.MaxSyncExportBytes:
		m.Status = store.DeliverableOverLimit
		m.Detail = fmt.Sprintf("the changes are %s, over the %d MiB a sync can take",
			mebibytes(st.Bytes), git.MaxSyncExportBytes>>20)
	default:
		m.Status, m.Detail = store.DeliverableVerified, syncChange(st)
	}
	return m
}

// syncChange says what changed, for a person.
func syncChange(st SyncState) string {
	var parts []string
	if st.Rewritten {
		parts = append(parts, "the history was rewritten past the base")
	} else if st.Commits > 0 {
		parts = append(parts, plural(st.Commits, "commit"))
	}
	if st.Files > 0 {
		parts = append(parts, plural(st.Files, "uncommitted changed file"))
	}
	out := strings.Join(parts, " and ")
	if st.Branch != "" {
		out += " on " + st.Branch
	}
	return out + ", " + mebibytes(st.Bytes) + " to take"
}

// mebibytes is a size as a person reads it.
func mebibytes(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KiB", (n+1023)>>10)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// judgeSync is gate 2 for a sync job, over its one member.
//
// @decision(DL-101) @decision(DL-050)
func judgeSync(job store.Job, events []store.Event, members []store.Deliverable) GateResult {
	res := GateResult{Gate: GateDeliverable}
	var member *store.Deliverable
	for i := range members {
		if members[i].Kind == store.DeliverableSync {
			member = &members[i]
		}
	}
	why, broke := collectionFailed(events)
	if member == nil || member.Status == store.DeliverableUnverified {
		res.Outcome, res.Detail = GateUnverified, "the repository could not be read"
		if broke {
			res.Detail += ": " + why
		}
		return res
	}
	parts := []string{member.Detail}
	switch member.Status {
	case store.DeliverableVerified:
		res.Outcome = GatePassed
		if job.Unfinished != "" {
			parts = append(parts, "the agent left unfinished: "+truncateRunes(oneLine(job.Unfinished), 300))
			res.Outcome = GatePartial
		}
		if boundNote(events) != "" {
			parts = append(parts, "the run ended at its wall clock before declaring itself done")
			res.Outcome = GatePartial
		}
	default:
		res.Outcome = GateFailed
	}
	res.Detail = strings.Join(parts, "; ")
	return res
}
