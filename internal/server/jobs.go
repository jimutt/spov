package server

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// The job service: creating, listing, cancelling and resuming jobs. The
// supervisor drives a job once it is queued.
//
// @decision(DL-045)

// MaxGoalBytes bounds a job's goal. The goal reaches the sandbox through
// a shell heredoc and a JavaScript literal and is re-asserted on every
// request, so its size is also a per-turn token cost.
//
// @decision(DL-045) @decision(DL-039)
const MaxGoalBytes = 16 << 10

// MaxModelBytes bounds a model identifier, which reaches a shell heredoc
// in the sandbox. It is checked before DL-013's catalogue check, so an
// unbounded string never reaches that refusal's error message.
//
// @decision(DL-014) @decision(DL-013)
const MaxModelBytes = 256

// MaxCheckBytes bounds a job's check command. It is a command line;
// anything longer is a script, which belongs in the repository.
//
// @decision(DL-050)
const MaxCheckBytes = 1 << 10

// ErrJobNotCancellable is a cancel of a job that has already finished.
//
// @decision(DL-045)
var ErrJobNotCancellable = errors.New("server: the job has already finished")

// ErrJobNotResumable is a resume of a job that is not suspended.
//
// @decision(DL-054)
var ErrJobNotResumable = errors.New("server: the job is not suspended")

// CreateJob records a job and the fresh session it runs in, in one
// transaction, then enqueues the job. The session is recorded starting
// with no sandbox; the supervisor launches it.
//
// @decision(DL-045)
func (s *Service) CreateJob(ctx context.Context, p auth.Principal, req JobRequest) (store.Job, error) {
	if s.deps.Jobs == nil {
		return store.Job{}, jobs.ErrJobsNeedAServer
	}
	now := s.deps.Now()
	jobID := store.NewID(now)
	// @decision(DL-120)
	tmpl, err := s.templateFor(ctx, p, req.Template)
	if err != nil {
		return store.Job{}, err
	}
	sreq, err := s.jobSessionRequest(req, jobID, tmpl)
	if err != nil {
		return store.Job{}, inTemplate(tmpl, err)
	}
	sreq = withAccountAuthor(p, sreq)
	// One read of the profile answers both of the next two questions.
	//
	// @decision(DL-014)
	profile := s.profileFor(ctx, p.OrgID, p.UserID)
	// Resolved onto the session request before either row is written.
	// The job row copies its model from here, so the two cannot
	// disagree about what the run was asked for.
	//
	// @decision(DL-014)
	model, src := resolveModel(sreq.Model, profile)
	sreq.Model = model
	// Before the quota and before any row exists, so a bad model is a
	// refused request rather than a failed job.
	//
	// @decision(DL-013)
	modelNote, err := s.checkModel(ctx, p, sreq.Model, src)
	if err != nil {
		return store.Job{}, err
	}
	// A job never takes the interactive default.
	//
	// @decision(DL-043)
	sreq.GitAccess, sreq.GitAccessPreferred = resolveGitAccess(sreq.GitAccess, sreq.GitAccessPreferred, profile.DefaultJobGitAccess)
	if err := s.refuseGitAccess(ctx, p.OrgID, p.UserID, sreq); err != nil {
		return store.Job{}, err
	}
	// @decision(DL-075)
	if err := s.refuseRoutedNames(ctx, p.OrgID, p.UserID, sreq.Routed); err != nil {
		return store.Job{}, inTemplate(tmpl, err)
	}
	// @decision(DL-077)
	if err := s.refuseRoutesWithoutProxy(ctx, p.OrgID, p.UserID, sreq.Routed); err != nil {
		return store.Job{}, inTemplate(tmpl, err)
	}
	// The caller's own limits, checked before anything billable. The
	// size limits refuse here; the concurrency limit is counted inside
	// the insert's transaction, since two creates on different replicas
	// would otherwise both read one short of it.
	quota := s.quotaOf(profile)
	// The clock decides the sandbox's timeout, so it is resolved first
	// and the caller's own timeout is not used.
	//
	// @decision(DL-049)
	clock, timeout, err := jobClockFor(req, profile.DefaultJobDuration, quota)
	if err != nil {
		// @decision(DL-011)
		var over overLimit
		if errors.As(err, &over) {
			s.deps.Metrics.QuotaRefused(ctx, telemetry.QuotaDuration)
		}
		return store.Job{}, err
	}
	sreq.Sandbox.Timeout = timeout
	// A job is idle only by its own bounds, never by Modal's clock,
	// which no request of a working job's resets.
	//
	// @decision(DL-049)
	sreq.Sandbox.IdleTimeout = timeout
	if err := quota.checkRequest(sreq); err != nil {
		// @decision(DL-011)
		s.deps.Metrics.QuotaRefused(ctx, telemetry.QuotaSize)
		return store.Job{}, err
	}
	// @decision(DL-134)
	if err := s.refuseBuildOverLimit(ctx, p.UserID, sreq); err != nil {
		return store.Job{}, err
	}

	sess := store.Session{
		ID:      store.NewID(now),
		OrgID:   p.OrgID,
		OwnerID: p.UserID,
		Name:    sreq.Name,
		State:   store.StateStarting,
		Request: sreq,
	}
	job := store.Job{
		ID:        jobID,
		OrgID:     p.OrgID,
		OwnerID:   p.UserID,
		SessionID: sess.ID,
		Kind:      req.Kind,
		State:     store.JobQueued,
		// Trimmed: what was validated is what is stored. The untrimmed
		// text would carry unbounded leading and trailing whitespace
		// past MaxGoalBytes and into the sandbox heredoc.
		Goal: strings.TrimSpace(req.Goal),
		// Trimmed like the goal: the value reaches a shell inside the
		// sandbox, and surrounding whitespace is never meaningful.
		//
		// @decision(DL-050)
		CheckCommand: strings.TrimSpace(req.Check),
		// From the session request rather than the raw one, so the row
		// and its session cannot disagree about what was asked for.
		//
		// @decision(DL-014)
		Model: sreq.Model,
		// @decision(DL-049)
		MaxDuration: clock,
		// Stored empty for spov's own policy.
		//
		// @decision(DL-137)
		PRPolicy: storedPRPolicy(req.PRPolicy),
		// @decision(DL-138)
		SkipPrep: req.SkipPrep,
	}
	if err := job.Validate(); err != nil {
		return store.Job{}, errors.Join(ErrInvalidRequest, err)
	}
	// The bundle is not part of the stored request; it waits in its own
	// row until the launch applies it.
	//
	// @decision(DL-100)
	var seed []byte
	if sreq.Sync != nil {
		seed = sreq.Sync.Bundle
	}
	job, err = s.createJobWithinQuota(ctx, sess, job, seed, quota.MaxConcurrentJobs, p.Service)
	if err != nil {
		return store.Job{}, err
	}
	// @decision(DL-013)
	if modelNote != "" {
		s.markModelNote(ctx, sess, modelNote)
	}

	// Enqueued after the rows commit, so the supervisor is never handed
	// an id that does not resolve. A failed enqueue keeps the queued row,
	// which stays visible and cancellable and which the sweep re-queues
	// within SweepInterval; the error names the job id.
	if err := s.deps.Jobs.Enqueue(ctx, job); err != nil {
		s.deps.Warn(fmt.Sprintf("job %s: enqueue: %s", job.ID, redact.Error(err)))
		return job, fmt.Errorf(
			"job %s was recorded but could not be queued, so nothing will run it:"+
				" cancel it with `spov jobs cancel %s`: %w", job.ID, job.ID, err)
	}
	return job, nil
}

// storedPRPolicy is the policy as a job row holds it: empty for spov's.
//
// @decision(DL-137)
func storedPRPolicy(p store.PRPolicy) store.PRPolicy {
	if p == store.PRPolicySpov {
		return ""
	}
	return p
}

// createJobWithinQuota counts the owner's unfinished jobs and inserts
// the session and the job in one transaction, so the count a create
// decides on is the count it commits against. A max of zero or less is
// no limit. It returns the job as stored, with the store's timestamps.
// A service account's create checks first that the account is not
// deleted.
//
// @decision(DL-045) @decision(DL-009) @decision(DL-140)
func (s *Service) createJobWithinQuota(ctx context.Context, sess store.Session, job store.Job, seed []byte, max int, service bool) (store.Job, error) {
	stored := job
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		if service {
			if err := liveServiceOwner(ctx, tx, job.OrgID, job.OwnerID); err != nil {
				return err
			}
		}
		if max > 0 {
			n, err := tx.Jobs().CountLive(ctx, job.OrgID, job.OwnerID)
			if err != nil {
				return err
			}
			if n >= max {
				return fmt.Errorf("%w: you have %d of %d jobs running;"+
					" cancel one with `spov jobs cancel` or ask an admin to raise your limit",
					ErrQuotaExceeded, n, max)
			}
		}
		if err := tx.Sessions().Create(ctx, sess); err != nil {
			return err
		}
		if err := tx.Jobs().Create(ctx, job); err != nil {
			return err
		}
		// @decision(DL-100)
		if len(seed) > 0 {
			if err := tx.Jobs().PutSeed(ctx, job.OrgID, job.ID, seed); err != nil {
				return err
			}
		}
		var err error
		stored, err = tx.Jobs().Get(ctx, job.OrgID, job.ID)
		return err
	})
	if errors.Is(err, store.ErrConflict) {
		return store.Job{}, ErrNameInUse
	}
	// @decision(DL-011)
	if errors.Is(err, ErrQuotaExceeded) {
		s.deps.Metrics.QuotaRefused(ctx, telemetry.QuotaJobs)
	}
	if err != nil {
		return store.Job{}, err
	}
	return stored, nil
}

// jobSessionRequest builds the request for the fresh session a job runs
// in. A job never joins a live session.
//
// @decision(DL-045)
func (s *Service) jobSessionRequest(req JobRequest, jobID string, tmpl *store.Template) (store.SessionRequest, error) {
	goal := strings.TrimSpace(req.Goal)
	var problems []string
	if goal == "" {
		problems = append(problems, "a job needs a goal")
	}
	if len(goal) > MaxGoalBytes {
		problems = append(problems, fmt.Sprintf("the goal is %d bytes; the limit is %d", len(goal), MaxGoalBytes))
	}
	// The check reaches a shell inside the sandbox and a JavaScript
	// literal in the plugin's config, exactly as the goal does, so it
	// is bounded for the same reason.
	//
	// @decision(DL-050)
	if check := strings.TrimSpace(req.Check); len(check) > MaxCheckBytes {
		problems = append(problems, fmt.Sprintf(
			"the check command is %d bytes; the limit is %d", len(check), MaxCheckBytes))
	}
	switch req.Kind {
	case store.JobKindPR, store.JobKindReport, store.JobKindSync:
	default:
		problems = append(problems, fmt.Sprintf("unknown kind %q; it is one of pr, report or sync", req.Kind))
	}
	// @decision(DL-100) @decision(DL-101)
	if req.Kind == store.JobKindSync {
		if req.Sync == nil {
			problems = append(problems, "a sync job needs a seed: run `spov jobs create --kind sync` inside a checkout")
		}
	} else {
		if req.Sync != nil {
			problems = append(problems, "only a sync job takes a seed")
		}
		if strings.TrimSpace(req.Hold) != "" {
			problems = append(problems, "only a sync job takes a hold")
		}
	}
	// @decision(DL-137)
	switch req.PRPolicy {
	case "", store.PRPolicySpov, store.PRPolicyGoal:
		if req.PRPolicy != "" && req.Kind != store.JobKindPR {
			problems = append(problems, "only a pr job takes a pr_policy")
		}
	default:
		problems = append(problems, fmt.Sprintf("unknown pr_policy %q; it is goal or spov", req.PRPolicy))
	}
	if len(problems) > 0 {
		return store.SessionRequest{}, fmt.Errorf("%w: %s", ErrInvalidRequest, strings.Join(problems, "; "))
	}

	repos := make([]store.RepoRef, len(req.Repos))
	for i, r := range req.Repos {
		repos[i] = store.RepoRef(r)
	}
	// A sync job lands in its one repository, as a synced session does,
	// so its check runs there.
	//
	// @decision(DL-100)
	landing := req.Landing
	if req.Kind == store.JobKindSync && len(repos) == 1 {
		dir := git.SingleRepoDir(repos[0].Owner, repos[0].Name)
		if landing != "" && strings.Trim(landing, "/") != dir {
			return store.SessionRequest{}, fmt.Errorf("%w: a sync job works in its repository's directory, %s, not %s",
				ErrInvalidRequest, dir, landing)
		}
		landing = dir
	}
	seed, err := SessionRequest{Sync: req.Sync}.syncToStore()
	if err != nil {
		return store.SessionRequest{}, err
	}
	sreq := store.SessionRequest{
		// The job's own id, not a timestamp; see jobSessionName.
		Name:    jobSessionName(jobID),
		Repos:   repos,
		Landing: landing,
		// @decision(DL-100)
		Sync:   seed,
		Image:  req.Image,
		Egress: req.Egress,
		// @decision(DL-068)
		EgressExtraDomains: req.EgressExtraDomains,
		// A job's timeout follows from its clock (jobClock), so the
		// request's own is not validated against anything.
		//
		// @decision(DL-049)
		Sandbox:  withoutTimeout(req.Sandbox.toStore()),
		GitName:  req.Git.Name,
		GitEmail: req.Git.Email,
		// Copied onto the session's request exactly as the goal is
		// copied onto the job row: the launch reads the session, the
		// redelivery reads the job. withDefaults trims it.
		//
		// @decision(DL-014)
		Model: req.Model,
		// @decision(DL-043)
		GitAccess:          req.GitAccess,
		GitAccessPreferred: req.GitAccessPreferred,
		// @decision(DL-042)
		CredentialPreference: req.CredentialPreference,
		// @decision(DL-075)
		Routed: routedRequest(req.Credentials, req.NoCredentials, req.CredentialsPreferred, req.Needs),
		// @decision(DL-033)
		Addons: req.Addons,
		Apt:    req.Apt,
		// @decision(DL-096)
		Env: req.Env,
	}
	// @decision(DL-120)
	if tmpl != nil {
		sreq = applyTemplate(sreq, *tmpl)
	}
	sreq = s.withDefaults(sreq)
	if err := s.validateRequest(sreq); err != nil {
		return store.SessionRequest{}, err
	}
	return sreq, nil
}

// jobClock resolves how long a job may run unattended, and the sandbox
// timeout that follows from it. The duration is the request's, else the
// profile's default, else DefaultWallClock. An explicit duration that
// exceeds the caller's limit, or does not fit under their sandbox cap
// with the launch and delivery margin, is refused; a configured or
// default one is clamped. The timeout is the clock, the margin and the
// steering headroom, cut to the cap.
//
// @decision(DL-049)
func jobClock(asked string, preferred bool, profileDefault time.Duration, q Quotas) (clock, timeout time.Duration, err error) {
	ceiling := q.MaxTimeout
	room := ceiling - jobs.WallClockMargin
	if room < jobs.MinWallClock {
		return 0, 0, fmt.Errorf("%w: your sandbox limit of %s leaves no time for a job after its launch;"+
			" ask an admin to raise it to at least %s", ErrInvalidRequest, ceiling, jobs.WallClockMargin+jobs.MinWallClock)
	}
	if q.MaxJobDuration < jobs.MinWallClock {
		return 0, 0, fmt.Errorf("%w: your job duration limit of %s is below the %s minimum;"+
			" ask an admin to raise it", ErrInvalidRequest, q.MaxJobDuration, jobs.MinWallClock)
	}
	limit := min(q.MaxJobDuration, room)
	switch asked = strings.TrimSpace(asked); {
	case asked != "":
		d, perr := parseJobDuration("max_duration", asked)
		if perr != nil {
			if preferred {
				return 0, 0, fmt.Errorf("%w (from sandbox.job_max_duration in your config)", perr)
			}
			return 0, 0, perr
		}
		switch {
		case preferred:
			clock = min(d, limit)
		case d > q.MaxJobDuration:
			return 0, 0, overLimit{fmt.Errorf("%w: max_duration %s exceeds your limit of %s", ErrInvalidRequest, d, q.MaxJobDuration)}
		case d > room:
			return 0, 0, overLimit{fmt.Errorf("%w: max_duration %s does not fit in your %s sandbox limit, which also covers"+
				" the launch and the delivery; ask for at most %s", ErrInvalidRequest, d, ceiling, room)}
		default:
			clock = d
		}
	case profileDefault > 0:
		clock = min(profileDefault, limit)
	default:
		clock = min(jobs.DefaultWallClock, limit)
	}
	return clock, min(clock+jobs.WallClockMargin+jobs.SteerHeadroom, ceiling), nil
}

// jobClockFor is jobClock for the request's kind. A sync job's sandbox
// also lives through its hold: the clock is resolved leaving room for
// the shortest hold, and the timeout is the clock, the margin, the
// steering headroom and the hold, with the headroom cut first and then
// the hold when the sum is over the cap. An explicit hold that cannot
// fit is refused; a configured or default one is cut.
//
// @decision(DL-049) @decision(DL-101)
func jobClockFor(req JobRequest, profileDefault time.Duration, q Quotas) (clock, timeout time.Duration, err error) {
	if req.Kind != store.JobKindSync {
		return jobClock(req.MaxDuration, req.MaxDurationPreferred, profileDefault, q)
	}
	hold := jobs.DefaultSyncHold
	asked := strings.TrimSpace(req.Hold)
	if asked != "" {
		d, perr := time.ParseDuration(asked)
		if perr != nil || d < jobs.MinSyncHold {
			err := fmt.Errorf("%w: hold %q is not a duration of at least %s, such as 30m", ErrInvalidRequest, asked, jobs.MinSyncHold)
			if req.HoldPreferred {
				err = fmt.Errorf("%w (from sandbox.job_sync_hold in your config)", err)
			}
			return 0, 0, err
		}
		hold = d
	}
	reserved := q
	reserved.MaxTimeout -= jobs.MinSyncHold
	if clock, _, err = jobClock(req.MaxDuration, req.MaxDurationPreferred, profileDefault, reserved); err != nil {
		// The real cap's refusal, when it has one, names the limit the
		// caller knows.
		if _, _, real := jobClock(req.MaxDuration, req.MaxDurationPreferred, profileDefault, q); real != nil {
			return 0, 0, real
		}
		return 0, 0, err
	}
	room := q.MaxTimeout - clock - jobs.WallClockMargin
	if hold > room {
		if asked != "" && !req.HoldPreferred {
			return 0, 0, overLimit{fmt.Errorf("%w: a hold of %s does not fit in your %s sandbox limit after the job's"+
				" %s and its launch and delivery; ask for at most %s", ErrInvalidRequest, hold, q.MaxTimeout, clock, room)}
		}
		hold = room
	}
	headroom := min(jobs.SteerHeadroom, room-hold)
	return clock, clock + jobs.WallClockMargin + headroom + hold, nil
}

// parseJobDuration reads a job duration as the API and the profile take
// it: a Go duration of at least MinWallClock. Empty is zero.
//
// @decision(DL-049)
func parseJobDuration(field, text string) (time.Duration, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(text)
	if err != nil || d < jobs.MinWallClock {
		return 0, fmt.Errorf("%w: %s %q is not a duration of at least %s, such as 45m",
			ErrInvalidRequest, field, text, jobs.MinWallClock)
	}
	return d, nil
}

// overLimit marks a refusal by the caller's own duration limits, for
// the quota refusal metric.
//
// @decision(DL-011)
type overLimit struct{ error }

func (o overLimit) Unwrap() error { return o.error }

// withoutTimeout clears a job request's sandbox timeout and idle
// timeout, which jobClock's timeout replaces.
//
// @decision(DL-049)
func withoutTimeout(sb store.SandboxConfig) store.SandboxConfig {
	// @decision(DL-049)
	sb.Timeout, sb.IdleTimeout = 0, 0
	return sb
}

// jobSessionName names a job's session job-<id>, unique because the id
// is. Do not derive it from the clock: two jobs started in the same
// second would collide.
//
// @decision(DL-045)
func jobSessionName(jobID string) string { return "job-" + strings.ToLower(jobID) }

// GetJob resolves a job the principal may see.
//
// @decision(DL-045)
func (s *Service) GetJob(ctx context.Context, p auth.Principal, id string) (store.Job, error) {
	job, err := s.deps.Store.Jobs().Get(ctx, p.OrgID, id)
	if err != nil {
		return store.Job{}, err
	}
	if !p.CanAccess(job.OwnerID) {
		// Not found rather than forbidden: the existence of another
		// user's job is not the caller's to learn.
		return store.Job{}, store.ErrNotFound
	}
	return job, nil
}

// JobDeliverables reads the inventories of jobs the caller has already
// been allowed to see; it scopes by org only.
//
// @decision(DL-051)
func (s *Service) JobDeliverables(ctx context.Context, p auth.Principal, ids []string) (map[string][]store.Deliverable, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.deps.Store.Jobs().Deliverables(ctx, p.OrgID, ids)
}

// syncJobSessions reads the sessions of a page's sync jobs and their
// runs, one query each, for the hold on each job's view.
//
// @decision(DL-101)
func (s *Service) syncJobSessions(ctx context.Context, p auth.Principal, ids []string) (map[string]store.Session, map[string][]store.Run, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	sessions, err := s.deps.Store.Sessions().ByIDs(ctx, p.OrgID, ids)
	if err != nil {
		return nil, nil, err
	}
	runs, err := s.deps.Store.Runs().List(ctx, p.OrgID, ids)
	if err != nil {
		return nil, nil, err
	}
	return sessions, runs, nil
}

// GetJobFile reads one file a job delivered, under the same rules as
// reading the job.
//
// @decision(DL-051)
func (s *Service) GetJobFile(ctx context.Context, p auth.Principal, jobID, path string) (store.JobFile, error) {
	if _, err := s.GetJob(ctx, p, jobID); err != nil {
		return store.JobFile{}, err
	}
	return s.deps.Store.Jobs().JobFile(ctx, p.OrgID, jobID, path)
}

// ReportImages lists, without bodies, the collected images a report
// links, in link order. None when f is not a report.
//
// @decision(DL-051)
func (s *Service) ReportImages(ctx context.Context, p auth.Principal, jobID string, f store.JobFile) ([]store.JobFile, error) {
	if !strings.HasPrefix(f.ContentType, "text/markdown") {
		return nil, nil
	}
	stored, err := s.deps.Store.Jobs().JobFiles(ctx, p.OrgID, jobID)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]store.JobFile, len(stored))
	for _, file := range stored {
		byPath[file.Path] = file
	}
	var out []store.JobFile
	for _, link := range jobs.ImageLinks(f.Body) {
		if img, ok := byPath[link]; ok && strings.HasPrefix(img.ContentType, "image/") {
			out = append(out, img)
		}
	}
	return out, nil
}

// WriteReportBundle writes a zip of a report and its images to w, at
// their paths under the out directory, reading one image at a time.
//
// @decision(DL-051)
func (s *Service) WriteReportBundle(ctx context.Context, p auth.Principal, jobID string, report store.JobFile, images []store.JobFile, w io.Writer) error {
	zw := zip.NewWriter(w)
	add := func(f store.JobFile) error {
		// @decision(DL-135)
		if !jobs.SafeEntryName(f.Path) {
			return fmt.Errorf("%w: %q is not a file name that can be bundled", ErrInvalidRequest, f.Path)
		}
		zf, err := zw.CreateHeader(&zip.FileHeader{Name: f.Path, Method: zip.Deflate, Modified: f.CreatedAt})
		if err != nil {
			return err
		}
		_, err = zf.Write(f.Body)
		return err
	}
	if err := add(report); err != nil {
		return err
	}
	for _, img := range images {
		full, err := s.deps.Store.Jobs().JobFile(ctx, p.OrgID, jobID, img.Path)
		if err != nil {
			return err
		}
		if err := add(full); err != nil {
			return err
		}
	}
	return zw.Close()
}

// ListJobs returns the jobs the principal may see.
//
// @decision(DL-045)
func (s *Service) ListJobs(ctx context.Context, p auth.Principal, f store.JobFilter) ([]store.Job, error) {
	// The owner is a filter on the query rather than a pass over the
	// result, so a non-admin never reads the whole org's job history.
	// The same shape List does for sessions.
	if !p.Admin {
		f.OwnerID = p.UserID
	}
	return s.deps.Store.Jobs().List(ctx, p.OrgID, f)
}

// CountJobs counts what ListJobs would return without its page.
//
// @decision(DL-012)
func (s *Service) CountJobs(ctx context.Context, p auth.Principal, f store.JobFilter) (int, error) {
	if !p.Admin {
		f.OwnerID = p.UserID
	}
	return s.deps.Store.Jobs().Count(ctx, p.OrgID, f)
}

// ResumeJob hands a suspended job back to its loop: it clears the
// plugin's pause, then moves the row to running. Both halves are needed.
// Resuming is idempotent, and a resume that returns an error must be
// retried.
//
// @decision(DL-054) @decision(DL-045)
func (s *Service) ResumeJob(ctx context.Context, p auth.Principal, id string) error {
	job, err := s.GetJob(ctx, p, id)
	if err != nil {
		return err
	}
	switch job.State {
	case store.JobSteered, store.JobBlocked:
	default:
		// Named rather than described, for the same reason a cancel of
		// a finished job is: a caller resuming a running job has
		// misunderstood what they are looking at.
		return fmt.Errorf("%w: it is %s", ErrJobNotResumable, job.State)
	}
	// A job whose loop already announced its stop is delivered, not
	// resumed: running above a stopped loop would never continue.
	//
	// @decision(DL-054) @decision(DL-048)
	stopped, err := s.jobLoopAnnouncedStop(ctx, job)
	if err != nil {
		// Not fatal: an ordinary resume is the safe direction, since a
		// stopped loop stays stopped and the backstop concludes it.
		s.deps.Warn(fmt.Sprintf("job %s: looking for an announced stop before resuming: %s",
			job.ID, redact.Error(err)))
	}
	if stopped {
		return s.resumeIntoDelivery(ctx, job)
	}
	// The plugin first. A steered job's loop is always held, so a failed
	// release fails the resume. A blocked job's is held only if a human
	// wrote to it while it was parked (DL-052), so the release is
	// best-effort there: most blocked jobs have nothing to release, and
	// one left held is still ended by its wall clock (DL-049).
	//
	// @decision(DL-054) @decision(DL-052)
	switch err := s.SetJobLoopPaused(ctx, p, job, false); {
	case err == nil:
	case job.State == store.JobSteered:
		return fmt.Errorf("resuming the job's loop: %w", err)
	default:
		s.deps.Warn(fmt.Sprintf("job %s: releasing its loop on resume: %s", job.ID, redact.Error(err)))
	}
	return s.moveJobToRunning(ctx, p, job)
}

// jobLoopAnnouncedStop reports whether the plugin already announced the
// end of this job's run. It reads the transcript with the same reader
// loopStoppedInMessages uses, because the announcement can be missing
// from the event log: the newest messages of the job's own conversation.
//
// @decision(DL-054) @decision(DL-045) @decision(DL-131)
func (s *Service) jobLoopAnnouncedStop(ctx context.Context, job store.Job) (bool, error) {
	cs := s.deps.Store.Conversations()
	convs, err := cs.ListConversations(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return false, err
	}
	primary, _ := store.Primary(convs)
	return jobs.LoopStoppedInRecent(ctx, cs, job, primary.ID)
}

// resumeIntoDelivery concludes a job whose loop announced its stop
// while the job was suspended.
//
// @decision(DL-054)
func (s *Service) resumeIntoDelivery(ctx context.Context, job store.Job) error {
	return s.deps.Store.Tx(ctx, func(tx store.Store) error {
		current, err := tx.Jobs().Get(ctx, job.OrgID, job.ID)
		if err != nil {
			return err
		}
		if current.State != store.JobSteered && current.State != store.JobBlocked {
			// Decided or released while this ran. That outcome stands,
			// and the caller is told the resume did not happen.
			return fmt.Errorf("%w: it is %s", ErrJobNotResumable, current.State)
		}
		// A blocked job goes via running, in two edges committed
		// together, as any resume of it does.
		//
		// @decision(DL-054) @decision(DL-045)
		if current.State == store.JobBlocked {
			current.State = store.JobRunning
			current.Error = ""
			if err := tx.Jobs().Update(ctx, current); err != nil {
				return err
			}
			current, err = tx.Jobs().Get(ctx, job.OrgID, job.ID)
			if err != nil {
				return err
			}
		}
		current.State = store.JobDelivering
		// The suspension's note goes with the suspension, exactly as
		// it does on the ordinary resume path.
		current.Error = ""
		return tx.Jobs().Update(ctx, current)
	})
}

// moveJobToRunning writes the row half of a resume and its boundary
// marker. The marker must be written before the row moves, so a follow
// pass that reads running always finds the boundary that neutralises the
// stale user item.
//
// @decision(DL-054) @decision(DL-045)
func (s *Service) moveJobToRunning(ctx context.Context, p auth.Principal, job store.Job) error {
	if err := s.MarkJobEvent(ctx, job, jobs.MarkerResumed, "resumed by "+cancelledBy(p)); err != nil {
		// Fatal, unlike most marker writes: without the boundary the
		// next follow pass steers the job straight back. The caller
		// retries.
		return fmt.Errorf("recording the resume: %w", err)
	}
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		current, err := tx.Jobs().Get(ctx, job.OrgID, job.ID)
		if err != nil {
			return err
		}
		if current.State == store.JobRunning {
			// Already resumed, by a retry or by another caller.
			return nil
		}
		if current.State != store.JobSteered && current.State != store.JobBlocked {
			return fmt.Errorf("%w: it is %s", ErrJobNotResumable, current.State)
		}
		current.State = store.JobRunning
		// Clears the suspension's note and any gapNote: the job is no
		// longer waiting, and clients render Error as a failure.
		current.Error = ""
		return tx.Jobs().Update(ctx, current)
	})
	if errors.Is(err, store.ErrInvalidTransition) {
		return fmt.Errorf("%w: it was decided while the resume was in flight", ErrJobNotResumable)
	}
	return err
}

// CancelJob writes cancelled, then terminates the job's sandbox. The row
// goes first, so a terminate that fails leaves a cancelled job and a
// sandbox the reconciler reaps.
//
// @decision(DL-045) @decision(DL-004)
func (s *Service) CancelJob(ctx context.Context, p auth.Principal, id string) error {
	job, err := s.GetJob(ctx, p, id)
	if err != nil {
		return err
	}
	if job.State.Terminal() {
		// Already cancelled is not an error; already finished is.
		if job.State == store.JobCancelled {
			return nil
		}
		return fmt.Errorf("%w: it is %s", ErrJobNotCancellable, job.State)
	}
	note := "cancelled by " + cancelledBy(p)
	// Re-read inside the transaction and change only what a cancel owns.
	// Update writes the whole row, so a stale copy would undo the
	// assessment and proposed check of a prep pass still in flight.
	//
	// @decision(DL-045) @decision(DL-047)
	var cancelled *store.Job
	if err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		current, err := tx.Jobs().Get(ctx, job.OrgID, job.ID)
		if err != nil {
			return err
		}
		if current.State.Terminal() {
			if current.State == store.JobCancelled {
				return nil
			}
			return fmt.Errorf("%w: it is %s", ErrJobNotCancellable, current.State)
		}
		current.State = store.JobCancelled
		// Appended, not replaced: a blocked or steered job's Error says
		// what it was waiting on.
		if current.Error == "" {
			current.Error = note
		} else {
			current.Error += "; " + note
		}
		if err := tx.Jobs().Update(ctx, current); err != nil {
			return err
		}
		cancelled = &current
		return nil
	}); err != nil {
		// A job that finished between the read and the write has been
		// decided by someone else; that outcome stands.
		if errors.Is(err, store.ErrInvalidTransition) {
			return fmt.Errorf("%w: it finished while the cancel was in flight", ErrJobNotCancellable)
		}
		return err
	}
	// Counted only by the call that wrote the cancel.
	//
	// @decision(DL-011)
	if cancelled != nil {
		s.deps.Metrics.JobEnded(ctx, cancelled.Kind, store.JobCancelled, telemetry.EndedCancelled,
			cancelled.ClockElapsed(s.deps.Now()))
	}
	// The session goes with the job. A failure is warned, not returned:
	// the job is already cancelled and the reconciler reaps the sandbox.
	if err := s.stopJobSession(ctx, p, job.SessionID); err != nil {
		s.deps.Warn(fmt.Sprintf("job %s: stopping its session: %s", job.ID, redact.Error(err)))
	}
	return nil
}

// stopJobSession stops the session of a cancelled job. Delete terminates
// the sandbox or records the durable cancel request, but does not conclude
// a starting row, because an ordinary launch owns that. A job's session
// may have no launch, so this concludes the row, but only while it is
// still starting with no sandbox.
//
// @decision(DL-045) @decision(DL-003)
func (s *Service) stopJobSession(ctx context.Context, p auth.Principal, sessionID string) error {
	if err := s.Delete(ctx, p, sessionID); err != nil {
		return err
	}
	sess, err := s.deps.Store.Sessions().Get(ctx, p.OrgID, sessionID)
	if err != nil {
		return err
	}
	if sess.State != store.StateStarting || sess.SandboxID != "" {
		// Either Delete already settled it, or a launch committed a
		// sandbox in the meantime and owns the row.
		return nil
	}
	t := s.deps.Now()
	s.forgetSession(sess.ID)
	// @decision(DL-011)
	took, err := s.deps.Store.Sessions().MarkStopped(ctx, sess.OrgID, sess.ID, store.StateStopped, t, sess.Error)
	if err != nil {
		return err
	}
	if took {
		recordStop(ctx, s.deps.Metrics, s.deps.Store, sess, t, telemetry.TerminationStopped)
	}
	return nil
}

// DeleteSession is what `DELETE /api/v1/sessions/:ref` calls: Delete,
// except that a session owned by an unfinished job is cancelled through
// the job. A plain Delete would leave the job queued, to run later
// against a session the caller believed stopped.
//
// @decision(DL-045)
func (s *Service) DeleteSession(ctx context.Context, p auth.Principal, ref string) error {
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return err
	}
	job, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return s.Delete(ctx, p, ref)
	case err != nil:
		return err
	// A held sync job's work is lost with its sandbox, so only apply
	// or release ends it.
	//
	// @decision(DL-102)
	case store.SyncHeld(job, sess):
		return heldError(job)
	case job.State.Terminal():
		// The job is done with; the session is an ordinary one now.
		return s.Delete(ctx, p, ref)
	}
	// CancelJob stops the session itself, by whichever of the two paths
	// the job's state calls for.
	return s.CancelJob(ctx, p, job.ID)
}

// cancelledBy names who cancelled or resumed a job, by user id rather
// than e-mail, since the text is shown back over the API.
//
// @decision(DL-045)
func cancelledBy(p auth.Principal) string {
	if p.UserID != "" {
		return p.UserID
	}
	return auth.LocalID
}
