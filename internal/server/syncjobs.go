package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// A sync job's state is read from its live sandbox: measured by the
// supervisor at delivery, exported over the API, and released once
// taken.
//
// @decision(DL-101) @decision(DL-102)

// ErrSyncHeld is a stop of a held sync job's session, whose work would
// be lost with it.
//
// @decision(DL-102)
var ErrSyncHeld = errors.New("server: the job's sandbox is held for its work to be taken")

// ErrJobFinished is a connect to a finished job's session.
//
// @decision(DL-101)
var ErrJobFinished = errors.New("server: the job has finished")

// ErrSyncUnavailable is an export or a release of a sync job whose
// state cannot be taken, or not yet.
//
// @decision(DL-102)
var ErrSyncUnavailable = errors.New("server: the sync job's state cannot be taken now")

// heldError names the two ways a hold ends.
//
// @decision(DL-102)
func heldError(job store.Job) error {
	return fmt.Errorf("%w: take it with `spov jobs apply %s`, or discard it with `spov jobs release %s`",
		ErrSyncHeld, job.ID, job.ID)
}

// finishedJobError refuses a connect to a finished job's session.
//
// @decision(DL-101)
func finishedJobError(job store.Job) error {
	if job.Kind == store.JobKindSync {
		return fmt.Errorf("%w: job %s is %s; take its work with `spov jobs apply %s`", ErrJobFinished, job.ID, job.State, job.ID)
	}
	return fmt.Errorf("%w: job %s is %s, so its session cannot be joined", ErrJobFinished, job.ID, job.State)
}

// syncJob resolves a sync job the principal may see, with its session.
// Any other kind is not found, as another user's job is.
//
// @decision(DL-102)
func (s *Service) syncJob(ctx context.Context, p auth.Principal, id string) (store.Job, store.Session, error) {
	job, err := s.GetJob(ctx, p, id)
	if err != nil {
		return store.Job{}, store.Session{}, err
	}
	if job.Kind != store.JobKindSync {
		return store.Job{}, store.Session{}, store.ErrNotFound
	}
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return store.Job{}, store.Session{}, err
	}
	return job, sess, nil
}

// syncDir is the workspace directory a synced session's repository is
// in.
//
// @decision(DL-027)
func syncDir(sess store.Session, owner, name string) string {
	dir := git.SingleRepoDir(owner, name)
	if sess.Request.Landing != "" {
		dir = sess.Request.Landing
	} else if rel := strings.TrimPrefix(sess.Landing, opencode.WorkspaceRoot+"/"); rel != "" && rel != sess.Landing {
		dir = rel
	}
	return dir
}

// syncBase is the commit a synced session was seeded from.
func syncBase(sess store.Session) string {
	if sess.Request.Sync != nil {
		return sess.Request.Sync.BaseCommit
	}
	return ""
}

// InspectJobSync measures a sync job's repository for the supervisor's
// gate 2, through one exec. A session no longer running is
// jobs.ErrSandboxGone.
//
// @decision(DL-101)
func (s *Service) InspectJobSync(ctx context.Context, job store.Job) (jobs.SyncState, error) {
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return jobs.SyncState{}, fmt.Errorf("reading the job's session: %w", err)
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return jobs.SyncState{}, jobs.ErrSandboxGone
	}
	owner, name, ok := singleSessionRepo(sess)
	if !ok {
		return jobs.SyncState{}, errors.New("a sync job has one repository")
	}
	h, err := s.handle(ctx, sess)
	if err != nil {
		return jobs.SyncState{}, err
	}
	sum, err := session.InspectSync(ctx, h, syncDir(sess, owner, name), syncBase(sess))
	if err != nil {
		return jobs.SyncState{}, err
	}
	return jobs.SyncState{
		Branch: sum.Branch, HeadCommit: sum.HeadCommit, Commits: sum.Commits,
		Files: sum.Files, Bytes: sum.Bytes, Rewritten: sum.Rewritten,
	}, nil
}

// ExportJobSync exports a sync job's state from its sandbox: a held
// job's, or an unfinished one's whose sandbox is live.
//
// @decision(DL-102)
func (s *Service) ExportJobSync(ctx context.Context, p auth.Principal, id string) (git.ExportedState, error) {
	job, sess, err := s.syncJob(ctx, p, id)
	if err != nil {
		return git.ExportedState{}, err
	}
	live := sess.State == store.StateRunning && sess.SandboxID != ""
	switch {
	case store.SyncHeld(job, sess):
	case live && (job.State == store.JobRunning || job.State == store.JobBlocked || job.State == store.JobSteered):
	case job.State.Terminal() || (job.State != store.JobQueued && job.State != store.JobProvisioning && !live):
		return git.ExportedState{}, fmt.Errorf("%w: job %s is %s and its sandbox has ended", ErrSyncUnavailable, job.ID, job.State)
	case job.State == store.JobDelivering:
		return git.ExportedState{}, fmt.Errorf("%w: job %s is being judged; try again once it has finished", ErrSyncUnavailable, job.ID)
	default:
		return git.ExportedState{}, fmt.Errorf("%w: job %s is %s; try again once it is running", ErrSyncUnavailable, job.ID, job.State)
	}
	owner, name, ok := singleSessionRepo(sess)
	if !ok {
		return git.ExportedState{}, fmt.Errorf("%w: a sync job has one repository", ErrSyncUnavailable)
	}
	h, err := s.handle(ctx, sess)
	if err != nil {
		return git.ExportedState{}, err
	}
	return session.ExportSync(ctx, h, syncDir(sess, owner, name), syncBase(sess))
}

// ReleaseJob ends a held sync job's sandbox, recording an apply when
// applied. A sync job already released, or cancelled, answers as it is.
//
// @decision(DL-102)
func (s *Service) ReleaseJob(ctx context.Context, p auth.Principal, id string, applied bool) (store.Job, error) {
	job, sess, err := s.syncJob(ctx, p, id)
	if err != nil {
		return store.Job{}, err
	}
	if !store.SyncHeld(job, sess) {
		if job.State.Terminal() {
			return job, nil
		}
		return store.Job{}, fmt.Errorf("%w: job %s is %s, not finished; cancel it with `spov jobs cancel %s`",
			ErrSyncUnavailable, job.ID, job.State, job.ID)
	}
	if applied {
		if err := s.deps.Store.Jobs().SetSyncApplied(ctx, job.OrgID, job.ID, s.deps.Now()); err != nil {
			return store.Job{}, err
		}
	}
	if err := s.deleteFor(ctx, p, sess.ID, telemetry.TerminationReleased); err != nil {
		return store.Job{}, err
	}
	return s.deps.Store.Jobs().Get(ctx, job.OrgID, job.ID)
}

type releaseJobInput struct {
	ID string `path:"id" doc:"Job ID"`
	// A pointer, so the body may be left out.
	Body *ReleaseJobRequest
}

// ReleaseJobRequest says whether the released state was applied.
//
// @decision(DL-102)
type ReleaseJobRequest struct {
	Applied bool `json:"applied,omitempty" doc:"The state was applied somewhere: recorded as applied_at"`
}

// registerSyncJobs adds the two routes that take a sync job's state.
// Both are POSTs, per-act reads or writes of a sandbox (DL-018).
//
// @decision(DL-102)
func (s *Server) registerSyncJobs() {
	register(s.api, huma.Operation{
		OperationID: "export-job-sync", Method: http.MethodPost, Path: "/api/v1/jobs/{id}/sync-export",
		Summary: "Export a sync job's git commits and working-tree changes from its sandbox", Tags: []string{"jobs"},
		Errors: []int{http.StatusConflict},
	}, func(ctx context.Context, in *jobIDInput) (*syncExportOutput, error) {
		state, err := s.svc.ExportJobSync(ctx, principal(ctx), in.ID)
		if err != nil {
			return nil, toJobHumaError(err)
		}
		return &syncExportOutput{Body: syncExportViewOf(state)}, nil
	})
	register(s.api, huma.Operation{
		OperationID: "release-job", Method: http.MethodPost, Path: "/api/v1/jobs/{id}/release",
		Summary: "End a held sync job's sandbox", Tags: []string{"jobs"},
		Errors: []int{http.StatusConflict},
	}, func(ctx context.Context, in *releaseJobInput) (*jobOutput, error) {
		job, err := s.svc.ReleaseJob(ctx, principal(ctx), in.ID, in.Body != nil && in.Body.Applied)
		if err != nil {
			return nil, toJobHumaError(err)
		}
		view, err := s.jobView(ctx, job)
		if err != nil {
			return nil, toJobHumaError(err)
		}
		return &jobOutput{Body: view}, nil
	})
}

// syncExportViewOf is an exported state as the API carries it.
//
// @decision(DL-027)
func syncExportViewOf(state git.ExportedState) SyncExportView {
	v := SyncExportView{
		Branch: state.Branch, BaseCommit: state.BaseCommit, HeadCommit: state.HeadCommit, Deleted: state.Deleted,
	}
	if len(state.Bundle) > 0 {
		v.Bundle = base64.StdEncoding.EncodeToString(state.Bundle)
	}
	if len(state.WorktreeTar) > 0 {
		v.WorktreeTar = base64.StdEncoding.EncodeToString(state.WorktreeTar)
	}
	return v
}
