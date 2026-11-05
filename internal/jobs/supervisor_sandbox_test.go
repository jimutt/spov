package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/sandbox"
)

// jobSandbox creates a sandbox tagged for a job at a chosen time and
// returns its handle and id.
func jobSandbox(t *testing.T, f *sandbox.Fake, jobID, name string, at time.Time) (sandbox.Handle, string) {
	t.Helper()
	f.Now = func() time.Time { return at }
	h, err := f.Create(context.Background(), sandbox.Spec{
		Name:        name,
		Image:       "img",
		Command:     []string{"true"},
		ReadyPort:   4096,
		CPU:         0.5,
		MemoryMiB:   1024,
		Timeout:     time.Hour,
		IdleTimeout: 10 * time.Minute,
		Egress:      sandbox.EgressOpen(),
		Tags:        sandbox.BuildTags(sandbox.TagSet{Name: name, Job: jobID, CreatedAt: at}),
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := h.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return h, info.ID
}

func TestFindJobSandboxFindsNothingWhenThereIsNothing(t *testing.T) {
	f := sandbox.NewFake()
	_, ok, err := findJobSandbox(context.Background(), f, "job_1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("found a sandbox for a job that has none")
	}
}

// The tag is what makes adoption possible, so a sandbox tagged for
// another job must not be adopted for this one.
func TestFindJobSandboxIgnoresAnotherJobsSandbox(t *testing.T) {
	f := sandbox.NewFake()
	_, _ = jobSandbox(t, f, "job_other", "other", time.Now())

	_, ok, err := findJobSandbox(context.Background(), f, "job_1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("adopted a sandbox belonging to another job")
	}
}

// An interactive session's sandbox carries no job tag and must never be
// adopted: it belongs to a person who is using it.
func TestFindJobSandboxIgnoresAnInteractiveSandbox(t *testing.T) {
	f := sandbox.NewFake()
	if _, err := f.Create(context.Background(), sandbox.Spec{
		Name: "interactive", Image: "img", Command: []string{"true"}, ReadyPort: 4096,
		CPU: 0.5, MemoryMiB: 1024, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Egress: sandbox.EgressOpen(),
		Tags:   sandbox.BuildTags(sandbox.TagSet{Name: "interactive", CreatedAt: time.Now()}),
	}); err != nil {
		t.Fatal(err)
	}
	_, ok, err := findJobSandbox(context.Background(), f, "job_1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("adopted an interactive session's sandbox")
	}
}

func TestFindJobSandboxAdoptsALiveOne(t *testing.T) {
	f := sandbox.NewFake()
	_, hID := jobSandbox(t, f, "job_1", "mine", time.Now())

	got, ok, err := findJobSandbox(context.Background(), f, "job_1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("did not adopt the job's own live sandbox")
	}
	if got.ID != hID {
		t.Errorf("adopted %q, want %q", got.ID, hID)
	}
}

// A stopped sandbox cannot be re-attached to. Adopting one would point
// the job at something that will never answer.
func TestFindJobSandboxIgnoresAStoppedOne(t *testing.T) {
	f := sandbox.NewFake()
	h, _ := jobSandbox(t, f, "job_1", "gone", time.Now())
	if err := h.Terminate(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, ok, err := findJobSandbox(context.Background(), f, "job_1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("adopted a stopped sandbox")
	}
}

// Two live sandboxes for one job is a duplicate that slipped through.
// The OLDEST is kept: its opening turn is furthest along and a recorder
// is already streaming it.
//
// The fake lists newest-first, so this fails if the selection relies on
// the provider's ordering rather than choosing explicitly.
func TestFindJobSandboxKeepsTheOldestDuplicate(t *testing.T) {
	f := sandbox.NewFake()
	base := time.Now()
	_, oldestID := jobSandbox(t, f, "job_1", "first", base)
	_, _ = jobSandbox(t, f, "job_1", "second", base.Add(time.Minute))
	_, _ = jobSandbox(t, f, "job_1", "third", base.Add(2*time.Minute))

	got, ok, err := findJobSandbox(context.Background(), f, "job_1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("found no sandbox among three")
	}
	if got.ID != oldestID {
		t.Errorf("kept %q, want the oldest %q", got.ID, oldestID)
	}
}

// The one thing worse than not adopting is provisioning a duplicate
// because the question could not be asked.
func TestAListingFailureNeverBecomesASecondSandbox(t *testing.T) {
	f := sandbox.NewFake()
	f.ListErr = errors.New("modal is unreachable")
	sup := &Supervisor{Provider: f, Warn: func(string) {}}

	created := false
	_, _, err := sup.adoptOrCreate(context.Background(), "job_1", func(context.Context) (string, error) {
		created = true
		return "sb-new", nil
	})
	if err == nil {
		t.Error("a listing failure was not reported")
	}
	if created {
		t.Error("a listing failure provisioned a second sandbox")
	}
}

func TestAdoptOrCreateCreatesWhenThereIsNothingToAdopt(t *testing.T) {
	f := sandbox.NewFake()
	sup := &Supervisor{Provider: f, Warn: func(string) {}}

	id, adopted, err := sup.adoptOrCreate(context.Background(), "job_1", func(context.Context) (string, error) {
		return "sb-new", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if adopted {
		t.Error("reported an adoption when it created")
	}
	if id != "sb-new" {
		t.Errorf("id = %q, want sb-new", id)
	}
}

// A River retry or rescue can deliver a job whose sandbox is already up.
// Creating a second bills to the same cap with nothing watching the
// first.
func TestAdoptOrCreateReusesALiveSandbox(t *testing.T) {
	f := sandbox.NewFake()
	_, hID := jobSandbox(t, f, "job_1", "running", time.Now())
	sup := &Supervisor{Provider: f, Warn: func(string) {}}

	created := false
	id, adopted, err := sup.adoptOrCreate(context.Background(), "job_1", func(context.Context) (string, error) {
		created = true
		return "sb-second", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("provisioned a second sandbox for a job that already had one")
	}
	if !adopted {
		t.Error("did not report the adoption")
	}
	if id != hID {
		t.Errorf("id = %q, want the existing %q", id, hID)
	}
}

// An empty job id would list every spov sandbox and adopt an unrelated
// one, so it is refused rather than queried.
func TestFindJobSandboxRefusesAnEmptyJobID(t *testing.T) {
	f := sandbox.NewFake()
	_, _ = jobSandbox(t, f, "job_other", "other", time.Now())
	if _, _, err := findJobSandbox(context.Background(), f, ""); err == nil {
		t.Error("an empty job id was queried instead of refused")
	}
}
