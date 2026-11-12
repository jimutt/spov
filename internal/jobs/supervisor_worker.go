package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/riverqueue/river"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// JobArgs is what River carries for one supervised job. It holds the ids
// and nothing else: the row is the truth, and a goal copied into the
// queue would be a second place to keep it in step.
//
// @decision(DL-046)
type JobArgs struct {
	JobID string `json:"job_id"`
	OrgID string `json:"org_id"`
}

// Kind identifies the job type to River. Renaming it strands jobs
// already queued under the old name.
func (JobArgs) Kind() string { return "spov_supervise" }

// InsertOpts pins the queue and the retry budget to this job type, so a
// caller cannot insert one that River then works under its own defaults.
//
// @decision(DL-046)
func (JobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       SupervisorQueue,
		MaxAttempts: SupervisorMaxAttempts,
		// One queue entry per job, enforced by the queue itself. Without
		// it the sweep's periodic re-queue would insert a second delivery
		// for a job already being worked, racing inside provision; the
		// tag query alone only catches a sequential duplicate.
		//
		// @decision(DL-045)
		UniqueOpts: river.UniqueOpts{ByArgs: true},
	}
}

// Supervisor drives autonomous jobs. It is nil in local mode.
//
// @decision(DL-046)
type Supervisor struct {
	Store    store.Store
	Provider sandbox.Provider
	// Queue inserts a job on River's queue. A function rather than the
	// client itself so a test does not need a real River type, and so
	// local mode can leave it nil.
	//
	// @decision(DL-045)
	Queue func(ctx context.Context, args JobArgs) error
	// Provision launches a job's session and returns once its sandbox
	// is running. Nil means the supervisor cannot start a job, which is
	// a configuration fault rather than a mode.
	//
	// @decision(DL-045)
	Provision Provision
	// Open issues a job's opening goal into its running sandbox.
	//
	// @decision(DL-045)
	Open Open
	// Adopt records a sandbox found by tag onto the job's session row,
	// for a sandbox an earlier attempt created but never committed.
	//
	// @decision(DL-045)
	Adopt Adopt
	// Wake tells the recorder a session is worth recording now, rather
	// than at its next poll. Nil means nothing is nudged and the poll
	// remains the only path.
	//
	// @decision(DL-017)
	Wake func(sess store.Session)
	// OpenOut opens a job's out directory in its sandbox, at job end, for
	// collecting a report job's reports and their images. Nil collects
	// nothing.
	//
	// @decision(DL-045) @decision(DL-051)
	OpenOut OpenOut
	// InspectSync measures a sync job's repository at delivery. Nil
	// leaves its deliverable unread.
	//
	// @decision(DL-101)
	InspectSync InspectSync
	// Evidence reads the plugin's own record of the run out of its
	// sandbox, at job end. Nil collects none.
	//
	// @decision(DL-045)
	Evidence Collect
	// Outcome reads the plugin's outcome file, the few lines that decide
	// a job's outcome, kept apart from the evidence file's size bound.
	// Nil collects none.
	//
	// @decision(DL-051) @decision(DL-050)
	Outcome Collect
	// CatchUp asks the recorder to read the job's session once more
	// before the gates are read, so the verdict sees the run's final
	// turn — the one turn nothing re-reads on its own, since the
	// recorder reads /message when a turn ends. Nil judges on what was
	// already stored.
	//
	// @decision(DL-050) @decision(DL-017)
	CatchUp func(ctx context.Context, job store.Job) error
	// Terminate stops a finished job's sandbox. Nil leaves it running,
	// which is the wrong default in production but right for a test
	// with no provider.
	//
	// @decision(DL-050)
	Terminate func(ctx context.Context, job store.Job) error
	// FindPullRequest asks the forge whether a pull request is open from
	// one recorded member's branch in its repository. A function rather
	// than a git.Platform because the platform is resolved from
	// configuration the supervisor does not otherwise read. Nil leaves
	// every pull request unverified.
	//
	// @decision(DL-050) @decision(DL-051)
	FindPullRequest func(ctx context.Context, job store.Job, d store.Deliverable) (*git.PullRequest, error)
	// Prepare runs the control plane's own model pass over a job's goal,
	// before its sandbox exists. Nil skips it, and the job opens on its
	// original text.
	//
	// @decision(DL-047)
	Prepare func(ctx context.Context, goal string) (prep.Assessment, error)
	// Mark writes a spov marker onto a job's session event stream.
	//
	// @decision(DL-045)
	Mark func(ctx context.Context, job store.Job, name, text string) error
	// Loop holds or releases the plugin's continuation loop in a job's
	// sandbox. The supervisor calls it in one direction only, pausing
	// when it writes `steered`, so the row's state always implies the
	// plugin's; the resume half is the service's, driven by a human's
	// request. Nil means the loop is never held.
	//
	// The principal is who asked, so the connection mint this causes is
	// audited against them; the supervisor's own pause passes a zero
	// principal, which falls back to the session's owner.
	//
	// @decision(DL-054)
	Loop func(ctx context.Context, p auth.Principal, job store.Job, paused bool) error
	// EndRun stops a job's loop for good and interrupts its turn, so
	// what delivery collects is at rest. Used at the wall clock, and
	// before every sync job's repository is read.
	//
	// @decision(DL-049) @decision(DL-101)
	EndRun func(ctx context.Context, job store.Job) error
	// Now is the supervisor's clock, for the wall clock; time.Now when
	// nil.
	//
	// @decision(DL-049)
	Now func() time.Time
	// Snooze asks the queue to redeliver the job after a delay,
	// releasing the worker in the meantime. Nil means one pass and no
	// redelivery, which is what a test driving Run directly wants.
	//
	// @decision(DL-045)
	Snooze func(time.Duration) error
	// FollowInterval overrides the wait between passes. Zero means
	// FollowInterval; a test sets it small.
	//
	// @decision(DL-045)
	FollowInterval time.Duration
	// MaxVerdictEvents overrides how much of a run the verdict holds.
	// Zero means MaxVerdictEvents.
	//
	// @decision(DL-050)
	MaxVerdictEvents int
	// MaxVerdictBytes overrides the size of what the verdict holds.
	// Zero means MaxVerdictBytes.
	//
	// @decision(DL-131)
	MaxVerdictBytes int64
	// SweepInterval overrides how often unfinished jobs are re-queued.
	// Zero means SweepInterval.
	//
	// @decision(DL-045)
	SweepInterval time.Duration
	// Warn receives one line per problem; nil discards.
	Warn func(string)
	// Metrics records passes, waits and endings; nil records nothing.
	//
	// @decision(DL-011)
	Metrics *telemetry.Metrics
	// onRun, when set, is called with the args of every job worked. It
	// exists so a test can observe that the queue delivered work
	// without needing a provider or a sandbox behind it.
	onRun func(JobArgs)
}

func (s *Supervisor) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Supervisor) warn(format string, args ...any) {
	if s.Warn != nil {
		s.Warn(fmt.Sprintf(format, args...))
	}
}

// ErrJobsNeedAServer is what a local server answers a job request with.
//
// @decision(DL-046)
var ErrJobsNeedAServer = errors.New(
	"autonomous jobs need a hosted deployment: the supervisor is a durable queue on Postgres, " +
		"and this server runs against no database")

// Enqueue hands a created job to the queue. A supervisor with no queue is
// a configuration fault: local mode has no supervisor at all, and
// CreateJob refuses before reaching here.
//
// @decision(DL-045)
func (s *Supervisor) Enqueue(ctx context.Context, job store.Job) error {
	if s.Queue == nil {
		return errors.New("supervisor: no queue to insert on")
	}
	return s.Queue(ctx, JobArgs{JobID: job.ID, OrgID: job.OrgID})
}

// SupervisorWorker is River's side of the supervisor.
//
// @decision(DL-046)
type SupervisorWorker struct {
	river.WorkerDefaults[JobArgs]
	Supervisor *Supervisor
}

// Timeout overrides River's one-minute default per attempt. An attempt
// is one pass (DL-045), so this is the launch budget and a margin —
// not the sandbox's life, which the attempt no longer spans.
//
// @decision(DL-046) @decision(DL-045)
func (w *SupervisorWorker) Timeout(*river.Job[JobArgs]) time.Duration {
	return SupervisorJobTimeout
}

// Work supervises one job.
//
// @decision(DL-046)
func (w *SupervisorWorker) Work(ctx context.Context, rj *river.Job[JobArgs]) error {
	return w.Supervisor.Run(ctx, rj.Args, rj.Attempt)
}

// NewSupervisorWorker builds River's side of the supervisor and wires the
// snooze it uses to be redelivered between passes. Set here rather than
// in serve because it is River's own sentinel error, and threading it out
// to the CLI would spread that dependency for nothing.
//
// @decision(DL-045)
func NewSupervisorWorker(sup *Supervisor) *SupervisorWorker {
	sup.Snooze = river.JobSnooze
	return &SupervisorWorker{Supervisor: sup}
}

// isTerminalJob reports whether a job has reached an end state.
//
// @decision(DL-045)
func isTerminalJob(s store.JobState) bool {
	switch s {
	case store.JobSucceeded, store.JobFailed, store.JobCancelled:
		return true
	}
	return false
}
