package postgres

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// pgJobs is the Postgres JobStore.
//
// @decision(DL-045)
type pgJobs struct{ s *Store }

func (p pgJobs) Create(ctx context.Context, j store.Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	now := p.s.now()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now
	}
	j.UpdatedAt = now
	return mapErr("create job", p.s.q.CreateJob(ctx, sqlcgen.CreateJobParams{
		ID:        j.ID,
		OrgID:     j.OrgID,
		OwnerID:   j.OwnerID,
		SessionID: j.SessionID,
		Kind:      string(j.Kind),
		State:     string(j.State),
		Goal:      j.Goal,
		// The caller's check, when they gave one. UpdateJob carries it
		// too (DL-047): the preparation pass may propose one for a
		// job created without it, and that pass runs after create.
		//
		// @decision(DL-050) @decision(DL-047)
		CheckCommand:  j.CheckCommand,
		CheckProposed: j.CheckProposed,
		Unfinished:    j.Unfinished,
		Error:         j.Error,
		Attempt:       int32(j.Attempt),
		CreatedAt:     ts(j.CreatedAt),
		UpdatedAt:     ts(j.UpdatedAt),
		StartedAt:     tsPtr(j.StartedAt),
		StoppedAt:     tsPtr(j.StoppedAt),
		// Written even though the one production caller leaves all four
		// zero: a Create that dropped them did so silently, since every
		// column defaults to a zero value rather than refusing.
		Cursor:     j.Cursor,
		Verdict:    j.Verdict,
		Assessment: j.Assessment,
		Model:      j.Model,
		// @decision(DL-049)
		MaxDurationSeconds: int64(j.MaxDuration / time.Second),
		ClockStartedAt:     tsPtr(j.ClockStartedAt),
		// @decision(DL-049)
		ClockUsedSeconds: int64(j.ClockUsed / time.Second),
		// @decision(DL-102)
		SyncAppliedAt: tsPtr(j.SyncAppliedAt),
		// @decision(DL-137) @decision(DL-138)
		PrPolicy: string(j.PRPolicy),
		SkipPrep: j.SkipPrep,
	}))
}

func (p pgJobs) Get(ctx context.Context, orgID, id string) (store.Job, error) {
	row, err := p.s.q.GetJob(ctx, sqlcgen.GetJobParams{OrgID: orgID, ID: id})
	if err != nil {
		return store.Job{}, mapErr("get job", err)
	}
	return jobFromRow(row), nil
}

// GetForUpdate reads the row with `for update`, which holds it until the
// enclosing transaction ends.
//
// @decision(DL-049)
func (p pgJobs) GetForUpdate(ctx context.Context, orgID, id string) (store.Job, error) {
	row, err := p.s.q.GetJobForUpdate(ctx, sqlcgen.GetJobForUpdateParams{OrgID: orgID, ID: id})
	if err != nil {
		return store.Job{}, mapErr("get job for update", err)
	}
	return jobFromRow(row), nil
}

func (p pgJobs) BySession(ctx context.Context, orgID, sessionID string) (store.Job, error) {
	row, err := p.s.q.GetJobBySession(ctx, sqlcgen.GetJobBySessionParams{
		OrgID: orgID, SessionID: sessionID,
	})
	if err != nil {
		return store.Job{}, mapErr("get job by session", err)
	}
	return jobFromRow(row), nil
}

// BySessions decorates a page of sessions in one query.
//
// An empty list is answered without a request: `any('{}')` matches
// nothing, so the round trip would be spent proving it.
//
// @decision(DL-060)
func (p pgJobs) BySessions(ctx context.Context, orgID string, sessionIDs []string) (map[string]store.Job, error) {
	if len(sessionIDs) == 0 {
		return map[string]store.Job{}, nil
	}
	rows, err := p.s.q.JobsBySessions(ctx, sqlcgen.JobsBySessionsParams{
		OrgID: orgID, SessionIds: sessionIDs,
	})
	if err != nil {
		return nil, mapErr("jobs by sessions", err)
	}
	out := make(map[string]store.Job, len(rows))
	for _, r := range rows {
		j := jobFromRow(r)
		out[j.SessionID] = j
	}
	return out, nil
}

// @decision(DL-012)
func (p pgJobs) List(ctx context.Context, orgID string, f store.JobFilter) ([]store.Job, error) {
	session, owner, states := jobFilterArgs(f)
	var before *string
	if f.Before != "" {
		before = &f.Before
	}
	var limit *int32
	if f.Limit > 0 {
		n := int32(min(f.Limit, math.MaxInt32))
		limit = &n
	}
	rows, err := p.s.q.ListJobs(ctx, sqlcgen.ListJobsParams{
		OrgID: orgID, SessionID: session, OwnerID: owner, States: states,
		Before: before, RowLimit: limit,
	})
	if err != nil {
		return nil, mapErr("list jobs", err)
	}
	return jobsFromRows(rows), nil
}

// Count counts what List would return without its page.
//
// @decision(DL-012)
func (p pgJobs) Count(ctx context.Context, orgID string, f store.JobFilter) (int, error) {
	session, owner, states := jobFilterArgs(f)
	n, err := p.s.q.CountJobs(ctx, sqlcgen.CountJobsParams{
		OrgID: orgID, SessionID: session, OwnerID: owner, States: states,
	})
	if err != nil {
		return 0, mapErr("count jobs", err)
	}
	return int(n), nil
}

// jobFilterArgs is a filter's conditions as the queries bind them.
func jobFilterArgs(f store.JobFilter) (session, owner *string, states []string) {
	states = make([]string, 0, len(f.States))
	for _, st := range f.States {
		states = append(states, string(st))
	}
	if f.SessionID != "" {
		session = &f.SessionID
	}
	if f.OwnerID != "" {
		owner = &f.OwnerID
	}
	return session, owner, states
}

// jobQuotaLockPrefix namespaces the job quota's advisory lock away from
// the session quota's, which is taken on the bare (org, owner) pair.
// Without it a user's job create and session create would serialise on
// each other for no reason — the two quotas are separate.
//
// @decision(DL-045)
const jobQuotaLockPrefix = "job:"

// CountLive counts a user's unfinished jobs, through the partial index
// migration 00010 adds for it.
//
// The advisory lock is taken for the same reason pgSessions.CountLive
// takes one: count(*) holds no row locks, so without it two
// transactions both find room under the limit and both insert.
//
// @decision(DL-045) @decision(DL-009)
func (p pgJobs) CountLive(ctx context.Context, orgID, ownerID string) (int, error) {
	if p.s.inTx {
		if err := p.s.q.LockOwnerForJobQuota(ctx, sqlcgen.LockOwnerForJobQuotaParams{
			Hashtext: orgID, Hashtext_2: jobQuotaLockPrefix + ownerID,
		}); err != nil {
			return 0, mapErr("lock owner for job quota", err)
		}
	}
	n, err := p.s.q.CountLiveJobsForOwner(ctx, sqlcgen.CountLiveJobsForOwnerParams{
		OrgID: orgID, OwnerID: ownerID,
	})
	if err != nil {
		return 0, mapErr("count live jobs", err)
	}
	return int(n), nil
}

func (p pgJobs) ListLive(ctx context.Context) ([]store.Job, error) {
	rows, err := p.s.q.ListLiveJobs(ctx)
	if err != nil {
		return nil, mapErr("list live jobs", err)
	}
	return jobsFromRows(rows), nil
}

// Update replaces the stored job, refusing a move the machine does not
// allow.
//
// Read and write are one transaction with the row locked, so the state
// the transition is checked against is the state it is written over. The
// supervisor runs behind DL-004's leader lock, but a restart or a
// failover can leave two replicas briefly both believing they hold it,
// and this is what makes the machine hold anyway.
//
// @decision(DL-045) @decision(DL-003)
func (p pgJobs) Update(ctx context.Context, j store.Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	if p.s.inTx {
		return p.update(ctx, p.s, j)
	}
	// Tx returns what the callback returned, wrapped or not, so the
	// sentinels stay reachable through errors.Is either way.
	return p.s.Tx(ctx, func(tx store.Store) error {
		return p.update(ctx, tx.(*Store), j)
	})
}

func (p pgJobs) update(ctx context.Context, s *Store, j store.Job) error {
	cur, err := s.q.GetJobForUpdate(ctx, sqlcgen.GetJobForUpdateParams{
		OrgID: j.OrgID, ID: j.ID,
	})
	if err != nil {
		return mapErr("lock job", err)
	}
	from := store.JobState(cur.State)
	if !store.CanTransition(from, j.State) {
		return fmt.Errorf("%w: %s -> %s", store.ErrInvalidTransition, from, j.State)
	}
	now := s.now()
	prev := jobFromRow(cur)
	stamped := j
	store.StampJobTimes(&stamped, prev, now)
	n, err := s.q.UpdateJob(ctx, sqlcgen.UpdateJobParams{
		OrgID:         stamped.OrgID,
		ID:            stamped.ID,
		State:         string(stamped.State),
		Goal:          stamped.Goal,
		Unfinished:    stamped.Unfinished,
		Error:         stamped.Error,
		Attempt:       int32(stamped.Attempt),
		Cursor:        stamped.Cursor,
		Verdict:       stamped.Verdict,
		Assessment:    stamped.Assessment,
		CheckCommand:  stamped.CheckCommand,
		CheckProposed: stamped.CheckProposed,
		UpdatedAt:     ts(now),
		StartedAt:     tsPtr(stamped.StartedAt),
		StoppedAt:     tsPtr(stamped.StoppedAt),
		// @decision(DL-049)
		ClockStartedAt: tsPtr(stamped.ClockStartedAt),
		// @decision(DL-049)
		ClockUsedSeconds: int64(stamped.ClockUsed / time.Second),
	})
	if err != nil {
		return mapErr("update job", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	// @decision(DL-100)
	if stamped.State.Terminal() {
		if err := s.q.DeleteJobSeed(ctx, sqlcgen.DeleteJobSeedParams{OrgID: j.OrgID, JobID: j.ID}); err != nil {
			return mapErr("delete job seed", err)
		}
	}
	return nil
}

// PutSeed writes a sync job's seed; a job that is not the org's is
// ErrNotFound.
//
// @decision(DL-100)
func (p pgJobs) PutSeed(ctx context.Context, orgID, jobID string, bundle []byte) error {
	if bundle == nil {
		bundle = []byte{}
	}
	n, err := p.s.q.PutJobSeed(ctx, sqlcgen.PutJobSeedParams{
		Bundle: bundle, Now: ts(p.s.now()), OrgID: orgID, JobID: jobID,
	})
	if err != nil {
		return mapErr("put job seed", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// @decision(DL-100)
func (p pgJobs) Seed(ctx context.Context, orgID, jobID string) ([]byte, error) {
	b, err := p.s.q.GetJobSeed(ctx, sqlcgen.GetJobSeedParams{OrgID: orgID, JobID: jobID})
	if err != nil {
		return nil, mapErr("get job seed", err)
	}
	return b, nil
}

// @decision(DL-100)
func (p pgJobs) DeleteSeed(ctx context.Context, orgID, jobID string) error {
	return mapErr("delete job seed", p.s.q.DeleteJobSeed(ctx, sqlcgen.DeleteJobSeedParams{OrgID: orgID, JobID: jobID}))
}

// @decision(DL-102)
func (p pgJobs) SetSyncApplied(ctx context.Context, orgID, jobID string, at time.Time) error {
	n, err := p.s.q.SetJobSyncApplied(ctx, sqlcgen.SetJobSyncAppliedParams{
		AppliedAt: ts(at), Now: ts(p.s.now()), OrgID: orgID, ID: jobID,
	})
	if err != nil {
		return mapErr("set job sync applied", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// PutDeliverable inserts or improves a member; zero rows is a verified
// member left alone, or a job that is not the org's.
//
// @decision(DL-051)
func (p pgJobs) PutDeliverable(ctx context.Context, orgID string, d store.Deliverable) (bool, error) {
	n, err := p.s.q.PutDeliverable(ctx, sqlcgen.PutDeliverableParams{
		OrgID:      orgID,
		JobID:      d.JobID,
		Key:        d.Key,
		Kind:       string(d.Kind),
		Status:     string(d.Status),
		Detail:     d.Detail,
		Platform:   d.Platform,
		Owner:      d.Owner,
		Name:       d.Name,
		Branch:     d.Branch,
		Number:     int32(d.Number),
		Url:        d.URL,
		Title:      d.Title,
		Path:       d.Path,
		Size:       d.Size,
		Forge:      d.Forge,
		ImagesLost: d.ImagesLost,
		// @decision(DL-101)
		HeadCommit: d.HeadCommit,
		Commits:    int32(d.Commits),
		Files:      int32(d.Files),
		Now:        ts(p.s.now()),
	})
	if err != nil {
		return false, mapErr("put deliverable", err)
	}
	if n > 0 {
		return true, nil
	}
	return false, p.mustExist(ctx, orgID, d.JobID)
}

// mustExist is ErrNotFound for a job that is not the org's.
func (p pgJobs) mustExist(ctx context.Context, orgID, jobID string) error {
	ok, err := p.s.q.JobExists(ctx, sqlcgen.JobExistsParams{OrgID: orgID, ID: jobID})
	if err != nil {
		return mapErr("find job", err)
	}
	if !ok {
		return store.ErrNotFound
	}
	return nil
}

func (p pgJobs) Deliverables(ctx context.Context, orgID string, jobIDs []string) (map[string][]store.Deliverable, error) {
	rows, err := p.s.q.DeliverablesForJobs(ctx, sqlcgen.DeliverablesForJobsParams{OrgID: orgID, JobIds: jobIDs})
	if err != nil {
		return nil, mapErr("list deliverables", err)
	}
	out := map[string][]store.Deliverable{}
	for _, r := range rows {
		out[r.JobID] = append(out[r.JobID], deliverableFromRow(r))
	}
	return out, nil
}

// PutJobFile inserts once; zero rows is a stored file kept, or a job
// that is not the org's.
//
// @decision(DL-051)
func (p pgJobs) PutJobFile(ctx context.Context, orgID string, f store.JobFile) (bool, error) {
	body := f.Body
	if body == nil {
		body = []byte{}
	}
	n, err := p.s.q.PutJobFile(ctx, sqlcgen.PutJobFileParams{
		OrgID:       orgID,
		JobID:       f.JobID,
		Path:        f.Path,
		ContentType: f.ContentType,
		Size:        f.Size,
		Body:        body,
		Now:         ts(p.s.now()),
	})
	if err != nil {
		return false, mapErr("put job file", err)
	}
	if n > 0 {
		return true, nil
	}
	return false, p.mustExist(ctx, orgID, f.JobID)
}

func (p pgJobs) JobFile(ctx context.Context, orgID, jobID, path string) (store.JobFile, error) {
	r, err := p.s.q.GetJobFile(ctx, sqlcgen.GetJobFileParams{OrgID: orgID, JobID: jobID, Path: path})
	if err != nil {
		return store.JobFile{}, mapErr("get job file", err)
	}
	return store.JobFile{
		JobID: r.JobID, Path: r.Path, ContentType: r.ContentType, Size: r.Size,
		Body: r.Body, CreatedAt: goTime(r.CreatedAt),
	}, nil
}

func (p pgJobs) JobFiles(ctx context.Context, orgID, jobID string) ([]store.JobFile, error) {
	if err := p.mustExist(ctx, orgID, jobID); err != nil {
		return nil, err
	}
	rows, err := p.s.q.ListJobFiles(ctx, sqlcgen.ListJobFilesParams{OrgID: orgID, JobID: jobID})
	if err != nil {
		return nil, mapErr("list job files", err)
	}
	out := make([]store.JobFile, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.JobFile{
			JobID: r.JobID, Path: r.Path, ContentType: r.ContentType, Size: r.Size,
			CreatedAt: goTime(r.CreatedAt),
		})
	}
	return out, nil
}
