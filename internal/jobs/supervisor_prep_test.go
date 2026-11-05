package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/store"
)

// The preparation pass's half in the supervisor: when it runs, what it
// is allowed to change, and — mostly — that a failed pass is not a
// failed job.
//
// @decision(DL-047)

// prepJob is a queued job with the given goal and caller's check.
func prepJob(t *testing.T, st store.Store, goal, check string) store.Job {
	t.Helper()
	job := jobstest.SeedJob(t, st, store.JobQueued)
	job.Goal = goal
	job.CheckCommand = check
	if err := st.Jobs().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

// prepSup wires a supervisor whose pass answers with a fixed
// assessment, and reports how many times it was called.
func prepSup(st store.Store, a prep.Assessment, err error) (*Supervisor, *int, *[]string) {
	calls := 0
	var warnings []string
	sup := &Supervisor{
		Store: st,
		Warn:  func(m string) { warnings = append(warnings, m) },
		Prepare: func(context.Context, string) (prep.Assessment, error) {
			calls++
			return a, err
		},
	}
	return sup, &calls, &warnings
}

// A caller's check always wins. An assessment that could overwrite an
// explicit instruction is the rewritten goal DL-047 refuses, applied to
// a command rather than to prose — and spov EXECUTES this command.
//
// @decision(DL-047) @decision(DL-050)
func TestACallersCheckIsNeverOverwrittenByAProposedOne(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "fix the build", "./scripts/check.sh")
	sup, _, _ := prepSup(st, prep.Assessment{Summary: "S", Check: "go test ./..."}, nil)

	got := sup.prepare(context.Background(), job)
	if got.CheckCommand != "./scripts/check.sh" {
		t.Fatalf("CheckCommand = %q, want the caller's; a proposal overwrote an explicit instruction",
			got.CheckCommand)
	}
	if got.CheckProposed {
		t.Error("CheckProposed is set on a check the caller gave, which would weaken a gate that is not weak")
	}
	// The assessment still lands: the check being the caller's says
	// nothing about the summary.
	if got.Assessment != "S" {
		t.Errorf("Assessment = %q, want it recorded", got.Assessment)
	}
}

// A job with no check of its own takes the proposal, and the row
// records that spov chose it.
//
// @decision(DL-047)
func TestAProposedCheckIsTakenAndMarkedAsProposed(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "fix the build", "")
	sup, _, _ := prepSup(st, prep.Assessment{Summary: "S", Check: "go test ./..."}, nil)

	got := sup.prepare(context.Background(), job)
	if got.CheckCommand != "go test ./..." {
		t.Errorf("CheckCommand = %q, want the proposal", got.CheckCommand)
	}
	if !got.CheckProposed {
		t.Error("CheckProposed is not set; a reader could not tell spov chose the command")
	}
	// And it is durable: provisioning may happen on another replica.
	stored, err := st.Jobs().Get(context.Background(), job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.CheckProposed || stored.CheckCommand != "go test ./..." {
		t.Errorf("the stored row does not carry the proposal: %+v", stored)
	}
}

// NO CHECK IS A CORRECT ANSWER. Most requests name none and have no
// conventional one, and DL-050 records that a guessed check is worse
// than none. Gate 1 is then unverified, which is not fatal.
//
// @decision(DL-047) @decision(DL-050)
func TestAnAssessmentWithNoCheckLeavesTheJobWithout(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "investigate the slow deploy", "")
	sup, _, _ := prepSup(st, prep.Assessment{Summary: "Investigate a slow deploy."}, nil)

	got := sup.prepare(context.Background(), job)
	if got.CheckCommand != "" {
		t.Errorf("CheckCommand = %q, want none: the pass proposed none", got.CheckCommand)
	}
	if got.CheckProposed {
		t.Error("CheckProposed is set with no check to be proposed")
	}
	if got.Assessment == "" {
		t.Error("the assessment was dropped because there was no check beside it")
	}
}

// A failed pass is never a failed job. The job opens on its original
// text.
//
// @decision(DL-047)
func TestAFailedPassLeavesTheJobExactlyAsItWas(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "fix the build", "./check.sh")
	sup, _, warnings := prepSup(st, prep.Assessment{}, errors.New("dial tcp: refused"))

	got := sup.prepare(context.Background(), job)
	if got.Assessment != "" || got.CheckCommand != "./check.sh" || got.CheckProposed {
		t.Errorf("a failed pass changed the job: %+v", got)
	}
	// It is warned about: this is the half of the logging split that
	// says a call was ATTEMPTED and failed, as against no provider
	// being configured at all.
	if len(*warnings) == 0 {
		t.Error("a failed pass said nothing; a deployment whose model was retired" +
			" would look exactly like one that never configured a key")
	}
}

// No provider configured is a nil seam, and it says nothing here: it is
// an ordinary zero-configuration state reported once at startup, not a
// per-job fault.
//
// @decision(DL-047)
func TestNoPassConfiguredIsSilentAndHarmless(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "fix the build", "")
	var warnings []string
	sup := &Supervisor{Store: st, Warn: func(m string) { warnings = append(warnings, m) }}

	if got := sup.prepare(context.Background(), job); got.Assessment != "" {
		t.Errorf("a nil Prepare produced an assessment: %q", got.Assessment)
	}
	if len(warnings) != 0 {
		t.Errorf("a deployment with no provider warned per job: %v", warnings)
	}
}

// River redelivers and provisioning may happen on another replica. A
// job that already has an assessment must not pay for a second call.
//
// @decision(DL-047)
func TestARedeliveredJobDoesNotPayForASecondPass(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "fix the build", "")
	job.Assessment = "already assessed"
	if err := st.Jobs().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	sup, calls, _ := prepSup(st, prep.Assessment{Summary: "a second opinion"}, nil)

	got := sup.prepare(context.Background(), job)
	if *calls != 0 {
		t.Errorf("made %d model calls for a job that was already assessed", *calls)
	}
	if got.Assessment != "already assessed" {
		t.Errorf("Assessment = %q, want the stored one", got.Assessment)
	}
}

// A pass that had nothing to say costs no write.
//
// @decision(DL-047)
func TestAPassWithNothingToSayWritesNothing(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "do the thing", "")
	sup, calls, _ := prepSup(st, prep.Assessment{}, nil)

	got := sup.prepare(context.Background(), job)
	if *calls != 1 {
		t.Fatalf("made %d calls, want 1", *calls)
	}
	if got.Assessment != "" || got.CheckProposed {
		t.Errorf("an empty assessment was recorded: %+v", got)
	}
}

// An empty goal has nothing to prepare and paying for a call about it
// is waste.
//
// @decision(DL-047)
func TestAnEmptyGoalIsNotPrepared(t *testing.T) {
	st := store.NewMemory()
	// Built rather than stored: the store refuses a job with no goal,
	// so this guard is defence against a row that should not exist
	// rather than against one that routinely does.
	job := jobstest.SeedJob(t, st, store.JobQueued)
	job.Goal = "   "
	sup, calls, _ := prepSup(st, prep.Assessment{Summary: "S"}, nil)

	sup.prepare(context.Background(), job)
	if *calls != 0 {
		t.Error("paid for a call about an empty goal")
	}
}

// Gate 1 must say when the check was spov's own, on EVERY outcome —
// including the unverified ones, which is where a reader most needs it.
//
// @decision(DL-047) @decision(DL-050)
func TestGateOneSaysWhenTheCheckWasSpovsOwn(t *testing.T) {
	proposed := store.Job{CheckCommand: "go test ./...", CheckProposed: true}
	given := store.Job{CheckCommand: "go test ./...", CheckProposed: false}

	// The unverified path: no evidence at all. This is the early return
	// in judgeMechanical, and a note applied only at the end would miss
	// it entirely.
	if got := judgeMechanical(proposed, nil); !strings.Contains(got.Detail, "spov proposed") {
		t.Errorf("an unverified gate 1 does not say the check was proposed: %q", got.Detail)
	}
	if got := judgeMechanical(given, nil); strings.Contains(got.Detail, "spov proposed") {
		t.Errorf("a caller's check was described as proposed: %q", got.Detail)
	}
	// And a job with no check at all is not described as having a
	// proposed one.
	none := store.Job{}
	if got := judgeMechanical(none, nil); strings.Contains(got.Detail, "spov proposed") {
		t.Errorf("a job with no check was described as having a proposed one: %q", got.Detail)
	}
}

// failingJobs is a JobStore whose Update always fails.
type failingJobs struct {
	store.JobStore
	err error
}

func (f failingJobs) Update(context.Context, store.Job) error { return f.err }

// failingStore is a store whose job updates fail, inside a transaction
// as well as outside one. Wrapping only the outer Jobs() would leave the
// transactional path — which is the one prepare uses — writing happily.
type failingStore struct {
	store.Store
	err error
}

func (f failingStore) Jobs() store.JobStore { return failingJobs{f.Store.Jobs(), f.err} }

func (f failingStore) Tx(ctx context.Context, fn func(store.Store) error) error {
	return f.Store.Tx(ctx, func(tx store.Store) error {
		return fn(failingStore{tx, f.err})
	})
}

// TestAnAssessmentThatCannotBeRecordedIsNotUsed checks a pass that
// succeeded but could not be recorded leaves the job on its goal alone,
// not on an assessment no row holds.
//
// @decision(DL-047)
func TestAnAssessmentThatCannotBeRecordedIsNotUsed(t *testing.T) {
	base := store.NewMemory()
	job := prepJob(t, base, "fix the build", "")
	st := failingStore{base, errors.New("the database went away")}

	var warnings []string
	sup := &Supervisor{
		Store: st,
		Warn:  func(m string) { warnings = append(warnings, m) },
		Prepare: func(context.Context, string) (prep.Assessment, error) {
			return prep.Assessment{Summary: "S", Check: "go test ./..."}, nil
		},
	}

	got := sup.prepare(context.Background(), job)
	if got.Assessment != "" || got.CheckProposed {
		t.Errorf("used an assessment no row holds: %+v", got)
	}
	// Asserted on the row rather than through openingText, which is the
	// server's: what this test owns is that a pass whose write failed
	// leaves nothing behind for the opening turn to pick up.
	if got.Assessment != "" {
		t.Error("the opening turn would carry an assessment the row does not")
	}
	if len(warnings) == 0 {
		t.Error("a write that failed said nothing")
	}
}

// TestThePassSurvivesTheMoveToProvisioning drives the real call order —
// prepare after the job has moved to provisioning — rather than calling
// prepare directly on a still-queued row, which every other test here
// does and which passed even when prepare wrote back a stale copy the
// store refused as a backwards transition.
//
// @decision(DL-047) @decision(DL-045)
func TestThePassSurvivesTheMoveToProvisioning(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "fix the build", "")
	sup, _, warnings := prepSup(st, prep.Assessment{Summary: "S", Check: "go test ./..."}, nil)

	took, err := sup.moved(context.Background(), job, store.JobProvisioning, "")
	if err != nil || !took {
		t.Fatalf("moved = %v, %v", took, err)
	}
	// The caller's copy is now stale, exactly as it is in provision.
	got := sup.prepare(context.Background(), job)

	if got.Assessment != "S" {
		t.Errorf("Assessment = %q; the pass was paid for and its result dropped", got.Assessment)
	}
	if !got.CheckProposed || got.CheckCommand != "go test ./..." {
		t.Errorf("the proposal was dropped: check=%q proposed=%v", got.CheckCommand, got.CheckProposed)
	}
	if len(*warnings) != 0 {
		t.Errorf("a working pass warned: %v", *warnings)
	}
	// The move must survive too: writing the stale copy back would
	// return the job to `queued` and undo the guard against a second
	// sandbox being bought.
	stored, err := st.Jobs().Get(context.Background(), job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != store.JobProvisioning {
		t.Errorf("State = %s, want provisioning; the pass wrote a stale row back over the move", stored.State)
	}
	if stored.Assessment != "S" {
		t.Errorf("the stored row carries no assessment: %+v", stored)
	}
}

// A check proposed with no summary still counts as having been asked,
// so a redelivery does not pay again. Keying the guard on the summary
// alone would miss this.
//
// @decision(DL-047)
func TestAProposalWithNoSummaryStillStopsASecondPass(t *testing.T) {
	st := store.NewMemory()
	job := prepJob(t, st, "fix the build", "")
	job.CheckProposed = true
	job.CheckCommand = "go test ./..."
	if err := st.Jobs().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	sup, calls, _ := prepSup(st, prep.Assessment{Summary: "a second opinion"}, nil)

	sup.prepare(context.Background(), job)
	if *calls != 0 {
		t.Errorf("made %d calls for a job that had already been prepared", *calls)
	}
}
