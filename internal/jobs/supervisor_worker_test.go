package jobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/store"
)

// newTestSupervisor is a supervisor over a memory store.
func newTestSupervisor(t *testing.T) (*Supervisor, store.Store) {
	t.Helper()
	st := store.NewMemory()
	return &Supervisor{Store: st, Warn: func(string) {}}, st
}

// A job that no longer exists must not be retried. River would re-run
// the attempt to its limit, and nothing will bring the row back.
func TestSupervisorTreatsAMissingJobAsDone(t *testing.T) {
	sup, _ := newTestSupervisor(t)
	err := sup.Run(context.Background(), JobArgs{JobID: "nope", OrgID: "org_1"}, 1)
	if err != nil {
		t.Errorf("a missing job returned %v, want nil so River stops retrying", err)
	}
}

// A job whose outcome was decided while the attempt was in flight is
// left alone: River's retries and rescues can both deliver one.
func TestSupervisorLeavesAFinishedJobAlone(t *testing.T) {
	for _, state := range []store.JobState{
		store.JobSucceeded, store.JobFailed, store.JobCancelled,
	} {
		sup, st := newTestSupervisor(t)
		job := jobstest.SeedJob(t, st, state)
		if err := sup.Run(context.Background(), JobArgs{JobID: job.ID, OrgID: job.OrgID}, 1); err != nil {
			t.Errorf("a %v job returned %v, want nil", state, err)
		}
		after, err := st.Jobs().Get(context.Background(), job.OrgID, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.State != state {
			t.Errorf("a %v job was moved to %v", state, after.State)
		}
	}
}

func TestIsTerminalJob(t *testing.T) {
	terminal := map[store.JobState]bool{
		store.JobSucceeded: true, store.JobFailed: true, store.JobCancelled: true,
	}
	for _, s := range []store.JobState{
		store.JobQueued, store.JobProvisioning, store.JobRunning,
		store.JobDelivering, store.JobSucceeded, store.JobFailed,
		store.JobBlocked, store.JobSteered, store.JobCancelled,
	} {
		if got := isTerminalJob(s); got != terminal[s] {
			t.Errorf("isTerminalJob(%v) = %v, want %v", s, got, terminal[s])
		}
	}
}

// Local mode has no supervisor, and the refusal has to say why rather
// than reading as a missing route or a bug.
func TestTheLocalRefusalExplainsItself(t *testing.T) {
	msg := ErrJobsNeedAServer.Error()
	for _, want := range []string{"hosted", "Postgres", "database"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q: %q", want, msg)
		}
	}
}

// River identifies work by kind across deploys, so renaming one strands
// every job already queued under the old name.
func TestJobKindIsStable(t *testing.T) {
	if got := (JobArgs{}).Kind(); got != "spov_supervise" {
		t.Errorf("the job kind is %q; renaming it strands queued jobs", got)
	}
}

// A caller must not be able to insert work that River then runs under
// its own defaults.
func TestInsertOptsPinTheQueueAndRetries(t *testing.T) {
	opts := (JobArgs{}).InsertOpts()
	if opts.Queue != SupervisorQueue {
		t.Errorf("queue = %q, want %q", opts.Queue, SupervisorQueue)
	}
	if opts.MaxAttempts != SupervisorMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", opts.MaxAttempts, SupervisorMaxAttempts)
	}
}

// NewSupervisorWorker wires the snooze River's worker uses to be
// redelivered between passes; without it a job would provision and stall.
//
// @decision(DL-045)
func TestNewSupervisorWorkerWiresTheSnooze(t *testing.T) {
	sup := &Supervisor{Store: store.NewMemory(), Warn: func(string) {}}
	w := NewSupervisorWorker(sup)
	if w.Supervisor != sup {
		t.Error("the worker does not hold the supervisor it was built from")
	}
	if sup.Snooze == nil {
		t.Fatal("Snooze was not wired; an unfinished job would never be redelivered")
	}
	// River's sentinel is an error that carries the delay.
	if err := sup.Snooze(time.Minute); err == nil {
		t.Error("the snooze returned nil rather than River's sentinel")
	}
}

// One attempt is one pass, so its timeout bounds a launch rather than
// the job's whole life.
//
// @decision(DL-045)
func TestTheWorkerTimeoutIsThePassBudget(t *testing.T) {
	w := &SupervisorWorker{}
	if got := w.Timeout(nil); got != SupervisorJobTimeout {
		t.Errorf("Timeout = %v, want %v", got, SupervisorJobTimeout)
	}
}

// Work is River's entry point and must reach Run.
//
// @decision(DL-046)
func TestWorkSupervisesTheJob(t *testing.T) {
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobRunning)
	var ran []JobArgs
	sup := &Supervisor{Store: st, Warn: func(string) {}, onRun: func(a JobArgs) { ran = append(ran, a) }}
	w := &SupervisorWorker{Supervisor: sup}
	if err := w.Work(context.Background(), &river.Job[JobArgs]{
		// JobRow is embedded as a pointer and Work reads Attempt off
		// it, so a bare literal would panic.
		JobRow: &rivertype.JobRow{Attempt: 1},
		Args:   JobArgs{JobID: job.ID, OrgID: job.OrgID},
	}); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0].JobID != job.ID {
		t.Fatalf("Work did not supervise the job it was given: %v", ran)
	}
}
