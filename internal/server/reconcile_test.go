package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

var t0 = time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)

func fixture(t *testing.T) (*Reconciler, *sandbox.Fake, *store.Memory, *[]string) {
	t.Helper()
	fake := sandbox.NewFake()
	mem := store.NewMemory()
	mem.Now = func() time.Time { return t0 }
	var warned []string
	r := &Reconciler{Store: mem, Provider: fake, Now: func() time.Time { return t0 },
		Warn: func(s string) { warned = append(warned, s) }, DefaultOrg: "org", DefaultOwner: "u1"}
	return r, fake, mem, &warned
}

func create(t *testing.T, fake *sandbox.Fake, name string, repos []string) sandbox.Handle {
	t.Helper()
	h, err := fake.Create(context.Background(), sandbox.Spec{Name: name, Image: "img", Command: []string{"x"}, CPU: 0.5, MemoryMiB: 2048, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: name, User: "someone", Repos: repos, CreatedAt: t0}), Egress: sandbox.EgressOpen()})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestReconcileAdoptsUnknownRunningSandboxes: a server restart, or a
// sandbox another spov started, shows up in the listing without a live
// provider call from the handler.
//
// @decision(DL-004)
func TestReconcileAdoptsUnknownRunningSandboxes(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	h := create(t, fake, "alpha", []string{"o/r", "o/s"})
	info, _ := h.Info(ctx)

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := mem.Sessions().Get(ctx, "org", "alpha")
	if err != nil {
		t.Fatalf("not adopted: %v", err)
	}
	if got.SandboxID != info.ID || got.State != store.StateRunning || got.OwnerID != "u1" || len(got.Clones) != 2 {
		t.Errorf("adopted as %+v", got)
	}
	if !store.ValidID(got.ID) {
		t.Errorf("id %q is not a ULID", got.ID)
	}
	// A second pass adopts nothing twice.
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if all, _ := mem.Sessions().List(ctx, "org", store.SessionFilter{}); len(all) != 1 {
		t.Errorf("%d sessions after two passes", len(all))
	}
}

// TestReconcileStopsSessionsWhoseSandboxIsGone.
//
// @decision(DL-004)
func TestReconcileStopsSessionsWhoseSandboxIsGone(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	h := create(t, fake, "alpha", nil)
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.Terminate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := mem.Sessions().Get(ctx, "org", "alpha")
	if got.ID != "" {
		t.Fatalf("a stopped session still resolves by name: %+v", got)
	}
	all, _ := mem.Sessions().List(ctx, "org", store.SessionFilter{States: []store.SessionState{store.StateStopped}})
	if len(all) != 1 || all[0].StoppedAt == nil || !all[0].StoppedAt.Equal(t0) {
		t.Errorf("stopped = %+v", all)
	}
}

// TestReconcileResolvesSessionsLeftStarting: after a restart, a session
// whose sandbox is running becomes running; one with no sandbox and no
// recent activity failed.
//
// @decision(DL-004)
func TestReconcileResolvesSessionsLeftStarting(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	h := create(t, fake, "alpha", nil)
	info, _ := h.Info(ctx)

	withSandbox := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "alpha", State: store.StateStarting, SandboxID: info.ID}
	stale := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "beta", State: store.StateStarting}
	fresh := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "gamma", State: store.StateStarting}
	// Past LaunchTimeout plus the orphan grace, which is when a starting
	// row stops being a launch in flight.
	mem.Now = func() time.Time { return t0.Add(-config.DefaultLaunchTimeout - 2*time.Minute) }
	for _, s := range []store.Session{withSandbox, stale} {
		if err := mem.Sessions().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	mem.Now = func() time.Time { return t0 }
	if err := mem.Sessions().Create(ctx, fresh); err != nil {
		t.Fatal(err)
	}

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Sessions().Get(ctx, "org", withSandbox.ID); got.State != store.StateRunning {
		t.Errorf("session with a running sandbox: %s", got.State)
	}
	if got, _ := mem.Sessions().Get(ctx, "org", stale.ID); got.State != store.StateFailed || !strings.Contains(got.Error, "server stopped") {
		t.Errorf("stale starting session: %+v", got)
	}
	if got, _ := mem.Sessions().Get(ctx, "org", fresh.ID); got.State != store.StateStarting {
		t.Errorf("a launch in flight was touched: %s", got.State)
	}
	if all, _ := mem.Sessions().List(ctx, "org", store.SessionFilter{}); len(all) != 3 {
		t.Errorf("%d sessions; the known sandbox was adopted twice", len(all))
	}
}

// TestReconcileLeavesAnInFlightLaunchWithASandboxAlone: the server
// wrote the sandbox id after the reconciler's provider listing.
//
// @decision(DL-004)
func TestReconcileLeavesAnInFlightLaunchWithASandboxAlone(t *testing.T) {
	r, _, mem, _ := fixture(t)
	ctx := context.Background()
	in := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "alpha", State: store.StateStarting, SandboxID: "sb-not-listed-yet"}
	if err := mem.Sessions().Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Sessions().Get(ctx, "org", in.ID); got.State != store.StateStarting {
		t.Errorf("a fresh launch with a sandbox id was marked %s", got.State)
	}
}

// TestReconcileResurrectsASessionWhoseSandboxStillRuns: a transient
// listing miss must not hide a billing sandbox forever.
//
// @decision(DL-004)
func TestReconcileResurrectsASessionWhoseSandboxStillRuns(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	h := create(t, fake, "alpha", nil)
	info, _ := h.Info(ctx)
	stopped := t0
	in := store.Session{ID: store.NewID(t0), OrgID: "org-b", OwnerID: "u9", Name: "alpha", State: store.StateStopped, SandboxID: info.ID, StoppedAt: &stopped}
	if err := mem.Sessions().Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := mem.Sessions().Get(ctx, "org-b", in.ID)
	if got.State != store.StateRunning || got.StoppedAt != nil || got.OwnerID != "u9" {
		t.Errorf("got %+v", got)
	}
	if live, _ := mem.Sessions().ListLive(ctx); len(live) != 1 {
		t.Errorf("%d live sessions; the sandbox was adopted a second time", len(live))
	}
}

// TestReconcileStopsSessionsInEveryOrg.
//
// @decision(DL-004)
func TestReconcileStopsSessionsInEveryOrg(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	h := create(t, fake, "alpha", nil)
	info, _ := h.Info(ctx)
	in := store.Session{ID: store.NewID(t0), OrgID: "org-b", OwnerID: "u9", Name: "alpha", State: store.StateRunning, SandboxID: info.ID}
	if err := mem.Sessions().Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := h.Terminate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Sessions().Get(ctx, "org-b", in.ID); got.State != store.StateStopped {
		t.Errorf("another org's session was left %s", got.State)
	}
}

// TestReconcileAdoptsANamelessSandboxUnderItsID.
//
// @decision(DL-004)
func TestReconcileAdoptsANamelessSandboxUnderItsID(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	h := create(t, fake, "", nil)
	info, _ := h.Info(ctx)
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := mem.Sessions().Get(ctx, "org", info.ID); err != nil || got.SandboxID != info.ID {
		t.Errorf("got %+v, %v", got, err)
	}
}

// TestReconcileLeavesALaunchInFlightUnadopted: a launch has created its
// sandbox but not yet recorded the ID. Adopting it would file the same
// sandbox twice, once under its name and once under its ID.
//
// @decision(DL-004)
func TestReconcileLeavesALaunchInFlightUnadopted(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	h := create(t, fake, "alpha", nil)
	info, _ := h.Info(ctx)
	launching := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "alpha", State: store.StateStarting}
	if err := mem.Sessions().Create(ctx, launching); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Sessions().BySandbox(ctx, info.ID); err == nil {
		t.Fatal("the in-flight launch's sandbox was adopted")
	}
	// The launch finishes and records its sandbox; the next cycle has
	// nothing to adopt.
	launching.State, launching.SandboxID = store.StateRunning, info.ID
	if err := mem.Sessions().Update(ctx, launching); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	all, _ := mem.Sessions().List(ctx, "org", store.SessionFilter{})
	if len(all) != 1 {
		t.Errorf("%d sessions for one sandbox", len(all))
	}
}

// TestReconcileLeavesAHostedLaunchInFlightUnadopted: on a hosted server
// the reconciler's default org is the local one, while a launch's
// session and its sandbox's org tag name the deployment's org. The
// in-flight launch is still found, so its sandbox is not filed twice.
//
// @decision(DL-004)
func TestReconcileLeavesAHostedLaunchInFlightUnadopted(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	r.DefaultOrg, r.DefaultOwner = auth.LocalID, auth.LocalID
	ctx := context.Background()
	h, err := fake.Create(ctx, sandbox.Spec{Name: "alpha", Image: "img", Command: []string{"x"}, CPU: 0.5, MemoryMiB: 2048,
		Timeout: time.Hour, IdleTimeout: 10 * time.Minute, Egress: sandbox.EgressOpen(),
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "alpha", User: auth.LocalID, Org: DefaultOrgID, CreatedAt: t0})})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(ctx)
	launching := store.Session{ID: store.NewID(t0), OrgID: DefaultOrgID, OwnerID: auth.LocalID, Name: "alpha", State: store.StateStarting}
	if err := mem.Sessions().Create(ctx, launching); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if adopted, err := mem.Sessions().BySandbox(ctx, info.ID); err == nil {
		t.Fatalf("the hosted launch's sandbox was adopted as %q in %q", adopted.Name, adopted.OrgID)
	}
}

// TestReconcileAdoptsUnderTheSandboxIDWhenTheNameIsTaken: a genuinely
// different sandbox with a name a running session already holds is
// still adopted, under its ID.
//
// @decision(DL-004)
func TestReconcileAdoptsUnderTheSandboxIDWhenTheNameIsTaken(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	// A running session holds the name "alpha" for a differently named
	// sandbox; a new sandbox named "alpha" is not its.
	first := create(t, fake, "alpha-old", nil)
	firstInfo, _ := first.Info(ctx)
	h := create(t, fake, "alpha", nil)
	info, _ := h.Info(ctx)
	if err := mem.Sessions().Create(ctx, store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "alpha", State: store.StateRunning, SandboxID: firstInfo.ID}); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := mem.Sessions().BySandbox(ctx, info.ID)
	if err != nil || got.Name != info.ID {
		t.Errorf("adopted as %+v, %v", got, err)
	}
}

// TestReconcileReportsAProviderFailure.
func TestReconcileReportsAProviderFailure(t *testing.T) {
	r, fake, _, warned := fixture(t)
	fake.ListErr = context.DeadlineExceeded
	if err := r.Once(context.Background()); err == nil {
		t.Fatal("a provider failure was swallowed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
	if len(*warned) != 0 {
		t.Errorf("a cancelled run warned: %v", *warned)
	}
}

// TestReconcileLeavesASlowLaunchAlone: a launch still within its budget
// must not be concluded, whatever the provider reports. The health wait
// alone can take minutes, so a starting row that is merely older than the
// reconcile interval is not evidence of anything.
//
// @decision(DL-004)
func TestReconcileLeavesASlowLaunchAlone(t *testing.T) {
	r, _, mem, _ := fixture(t)
	ctx := context.Background()

	// 90 seconds old, with a sandbox the provider does not list yet.
	slow := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "slow",
		State: store.StateStarting, SandboxID: "sb-not-listed"}
	mem.Now = func() time.Time { return t0.Add(-90 * time.Second) }
	if err := mem.Sessions().Create(ctx, slow); err != nil {
		t.Fatal(err)
	}
	mem.Now = func() time.Time { return t0 }

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := mem.Sessions().Get(ctx, "org", slow.ID)
	if got.State != store.StateStarting {
		t.Errorf("a launch %v into a %v budget became %s", 90*time.Second, config.DefaultLaunchTimeout, got.State)
	}
}

// TestReconcileOrphansALaunchWhoseReplicaDied: past LaunchTimeout the
// launching replica is gone. A row with a sandbox recorded is failed as
// orphaned, which is distinguishable from a server that stopped before
// recording one.
//
// @decision(DL-004)
func TestReconcileOrphansALaunchWhoseReplicaDied(t *testing.T) {
	r, _, mem, _ := fixture(t)
	var forgotten []string
	r.Forget = func(id string) { forgotten = append(forgotten, id) }
	ctx := context.Background()

	orphan := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "orphan",
		State: store.StateStarting, SandboxID: "sb-gone"}
	mem.Now = func() time.Time { return t0.Add(-config.DefaultLaunchTimeout - 2*time.Minute) }
	if err := mem.Sessions().Create(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	mem.Now = func() time.Time { return t0 }

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := mem.Sessions().Get(ctx, "org", orphan.ID)
	if got.State != store.StateFailed || got.Error != "launch orphaned" {
		t.Errorf("got state %s error %q", got.State, got.Error)
	}
	if got.StoppedAt == nil {
		t.Error("an orphaned launch has no stopped time")
	}
	if !slices.Contains(forgotten, orphan.ID) {
		t.Error("an orphaned launch's cached password was not forgotten")
	}
}

// TestReconcileAdoptsAnOrphanWhoseSandboxSurvived: the replica died but
// the sandbox is still running, so the session is running, not failed.
//
// @decision(DL-004)
func TestReconcileAdoptsAnOrphanWhoseSandboxSurvived(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	h := create(t, fake, "survivor", nil)
	info, _ := h.Info(ctx)

	orphan := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "survivor",
		State: store.StateStarting, SandboxID: info.ID}
	mem.Now = func() time.Time { return t0.Add(-config.DefaultLaunchTimeout - 2*time.Minute) }
	if err := mem.Sessions().Create(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	mem.Now = func() time.Time { return t0 }

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := mem.Sessions().Get(ctx, "org", orphan.ID)
	if got.State != store.StateRunning || got.Error != "" {
		t.Errorf("got state %s error %q; a live sandbox outranks the deadline", got.State, got.Error)
	}
}

// TestReconcileHonoursAConfiguredLaunchTimeout: a deployment that allows
// longer launches must not have its own launches orphaned.
//
// @decision(DL-004)
func TestReconcileHonoursAConfiguredLaunchTimeout(t *testing.T) {
	r, _, mem, _ := fixture(t)
	r.LaunchTimeout = 30 * time.Minute
	ctx := context.Background()

	// Past the default budget, well inside the configured one.
	sess := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "long",
		State: store.StateStarting, SandboxID: "sb-slow"}
	mem.Now = func() time.Time { return t0.Add(-config.DefaultLaunchTimeout - 2*time.Minute) }
	if err := mem.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	mem.Now = func() time.Time { return t0 }

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Sessions().Get(ctx, "org", sess.ID); got.State != store.StateStarting {
		t.Errorf("a launch inside a configured %v budget became %s", r.LaunchTimeout, got.State)
	}
}

// TestReconcileLeavesAQueuedJobsSessionAlone: the comparison session is
// the point — an ordinary starting session of exactly the same age is
// still failed, so this asserts the exemption rather than the
// reconciler having stopped sweeping.
//
// @decision(DL-004)
func TestReconcileLeavesAQueuedJobsSessionAlone(t *testing.T) {
	r, _, mem, _ := fixture(t)
	ctx := context.Background()

	// Both older than any launch could be.
	mem.Now = func() time.Time { return t0.Add(-config.DefaultLaunchTimeout - 2*time.Minute) }
	jobSess := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "job-sess", State: store.StateStarting}
	plain := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "plain", State: store.StateStarting}
	for _, s := range []store.Session{jobSess, plain} {
		if err := mem.Sessions().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	job := store.Job{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", SessionID: jobSess.ID,
		Kind: store.JobKindPR, State: store.JobQueued, Goal: "do the thing"}
	if err := mem.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	mem.Now = func() time.Time { return t0 }

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Sessions().Get(ctx, "org", jobSess.ID); got.State != store.StateStarting {
		t.Errorf("a queued job's session was swept to %s: the job dies before it runs", got.State)
	}
	// The exemption is for queued jobs only, not for starting sessions
	// in general.
	if got, _ := mem.Sessions().Get(ctx, "org", plain.ID); got.State != store.StateFailed {
		t.Errorf("an ordinary orphaned launch is %s, want failed: the sweep has stopped working", got.State)
	}
}

// TestReconcileSweepsTheSessionOfAJobThatHasMovedOn: the exemption is
// tied to `queued` and not to "has a job".
//
// @decision(DL-004)
func TestReconcileSweepsTheSessionOfAJobThatHasMovedOn(t *testing.T) {
	r, _, mem, _ := fixture(t)
	ctx := context.Background()

	mem.Now = func() time.Time { return t0.Add(-config.DefaultLaunchTimeout - 2*time.Minute) }
	sess := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "provisioning", State: store.StateStarting}
	if err := mem.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	job := store.Job{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", SessionID: sess.ID,
		Kind: store.JobKindPR, State: store.JobQueued, Goal: "do the thing"}
	if err := mem.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	mem.Now = func() time.Time { return t0 }
	// The supervisor picked it up and the replica then died.
	job.State = store.JobProvisioning
	if err := mem.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Sessions().Get(ctx, "org", sess.ID); got.State != store.StateFailed {
		t.Errorf("the session of a provisioning job is %s, want failed: a dead provision is never concluded", got.State)
	}
}

// createForJob is a sandbox tagged as a job's, which is how the
// supervisor creates one (sandbox.JobTags).
func createForJob(t *testing.T, fake *sandbox.Fake, name, jobID string) sandbox.Handle {
	t.Helper()
	h, err := fake.Create(context.Background(), sandbox.Spec{
		Name: name, Image: "img", Command: []string{"x"}, CPU: 0.5, MemoryMiB: 2048,
		Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags:   sandbox.BuildTags(sandbox.TagSet{Name: name, User: "someone", Job: jobID, CreatedAt: t0}),
		Egress: sandbox.EgressOpen()})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A provisioning job's sandbox is not adopted as a second session.
//
// The window: the job is `provisioning`, its sandbox exists and is
// tagged, and its session row does not yet carry the id.
//
// @decision(DL-004)
func TestReconcileDoesNotAdoptAProvisioningJobsSandbox(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()

	jobSess := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1",
		Name: "job-abc", State: store.StateStarting}
	if err := mem.Sessions().Create(ctx, jobSess); err != nil {
		t.Fatal(err)
	}
	job := store.Job{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", SessionID: jobSess.ID,
		Kind: store.JobKindPR, State: store.JobQueued, Goal: "do the thing"}
	if err := mem.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.State = store.JobProvisioning
	if err := mem.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}

	// The sandbox exists and is tagged; the session does not know its
	// id yet. The name is the sandbox's own, as Modal reports it —
	// deliberately NOT the session's name, which is the whole trap.
	createForJob(t, fake, "sb-01ABC", job.ID)

	before, err := mem.Sessions().ListLive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := mem.Sessions().ListLive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		for _, s := range after {
			t.Logf("session %s name=%q sandbox=%q", s.ID, s.Name, s.SandboxID)
		}
		t.Fatalf("the reconciler filed %d sessions for %d: a provisioning job's sandbox was"+
			" adopted as a second session, so it counts against both quotas and is"+
			" recorded twice", len(after), len(before))
	}
}

// The exemption is tied to the JOB still counting, not to the tag being
// present. A sandbox whose job has finished is exactly what DL-004's
// adoption is for.
//
// @decision(DL-004)
func TestReconcileAdoptsTheSandboxOfAFinishedJob(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()

	jobSess := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1",
		Name: "job-done", State: store.StateStopped}
	if err := mem.Sessions().Create(ctx, jobSess); err != nil {
		t.Fatal(err)
	}
	job := store.Job{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", SessionID: jobSess.ID,
		Kind: store.JobKindPR, State: store.JobQueued, Goal: "do the thing"}
	if err := mem.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	for _, st := range []store.JobState{store.JobProvisioning, store.JobRunning, store.JobDelivering, store.JobSucceeded} {
		job.State = st
		if err := mem.Jobs().Update(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	h := createForJob(t, fake, "sb-01DONE", job.ID)
	info, err := h.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := mem.Sessions().BySandbox(ctx, info.ID)
	if err != nil {
		t.Fatalf("a finished job's leftover sandbox was not adopted (%v);"+
			" it bills with nothing tracking it", err)
	}
	if !got.State.Live() {
		t.Errorf("adopted as %s, want a live session", got.State)
	}
}

// An ordinary interactive sandbox is untagged, so it reads as "" and
// must fall through to the checks that were always there.
//
// @decision(DL-004)
func TestReconcileStillAdoptsAnUntaggedSandbox(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()

	h := create(t, fake, "alpha", nil)
	info, err := h.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Sessions().BySandbox(ctx, info.ID); err != nil {
		t.Fatalf("an ordinary unknown sandbox was not adopted (%v): DL-004's backstop is gone", err)
	}
}

// A session brought back from a listing miss has its run reopened, and a
// resume still starting is judged by when its run began, not by when
// the session was created.
//
// @decision(DL-085) @decision(DL-087)
func TestReconcileFollowsTheCurrentRun(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	h := create(t, fake, "alpha", nil)
	info, _ := h.Info(ctx)
	stopped := t0
	in := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "alpha", State: store.StateStopped, SandboxID: info.ID, StoppedAt: &stopped}
	if err := mem.Sessions().Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := mem.Runs().Insert(ctx, store.Run{OrgID: "org", SessionID: in.ID, OwnerID: "u1", N: 1, SandboxID: info.ID,
		Origin: store.RunFromLaunch, StartedAt: t0, EndedAt: &stopped, SnapshotState: store.SnapshotFailed}); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if run, _ := mem.Runs().Current(ctx, "org", in.ID); run.EndedAt != nil || run.SnapshotState != store.SnapshotPending {
		t.Errorf("the revived session's run = %+v", run)
	}

	// A resume of an old session, started a moment ago, is in flight.
	old := t0.Add(-72 * time.Hour)
	resumed := store.Session{ID: store.NewID(old), OrgID: "org", OwnerID: "u1", Name: "beta", State: store.StateStarting, CreatedAt: old}
	if err := mem.Sessions().Create(ctx, resumed); err != nil {
		t.Fatal(err)
	}
	ended := old.Add(time.Hour)
	for _, run := range []store.Run{
		{OrgID: "org", SessionID: resumed.ID, OwnerID: "u1", N: 1, Origin: store.RunFromLaunch, StartedAt: old, EndedAt: &ended, SnapshotState: store.SnapshotKept},
		{OrgID: "org", SessionID: resumed.ID, OwnerID: "u1", N: 2, Origin: store.RunFromSnapshot, StartedAt: t0, SnapshotState: store.SnapshotPending},
	} {
		if err := mem.Runs().Insert(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Sessions().Get(ctx, "org", resumed.ID); got.State != store.StateStarting {
		t.Errorf("a resume just begun was concluded: %s", got.State)
	}
}

// A later run's sandbox that no session records is ended, never adopted
// as a second session; one whose resume is in flight is left to it.
//
// @decision(DL-087) @decision(DL-004)
func TestReconcileEndsAStrayRunAndLeavesAnInFlightOne(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	mk := func(session string) sandbox.Handle {
		h, err := fake.Create(ctx, sandbox.Spec{Name: session + "-run2", Image: "img", Command: []string{"x"}, CPU: 0.5, MemoryMiB: 2048,
			Timeout: time.Hour, IdleTimeout: 10 * time.Minute, Egress: sandbox.EgressOpen(),
			Tags: sandbox.BuildTags(sandbox.TagSet{Name: session, User: "u1", Org: "org", CreatedAt: t0, Run: 2})})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	stray, flying := mk("gone"), mk("flying")
	in := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1", Name: "flying", State: store.StateStarting, CreatedAt: t0}
	if err := mem.Sessions().Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if !stray.(*sandbox.FakeHandle).WasTerminated() {
		t.Error("a stray later run was not ended")
	}
	if flying.(*sandbox.FakeHandle).WasTerminated() {
		t.Error("an in-flight resume's sandbox was ended")
	}
	if all, _ := mem.Sessions().List(ctx, "org", store.SessionFilter{}); len(all) != 1 {
		t.Errorf("%d sessions: a later run was adopted", len(all))
	}
}

// A held sync job's session is live and records its sandbox, so the
// reconciler leaves both alone; once the sandbox ends, the session is
// stopped as any other, which is the release.
//
// @decision(DL-101)
func TestReconcileLeavesAHeldSyncSandboxAlone(t *testing.T) {
	r, fake, mem, _ := fixture(t)
	ctx := context.Background()
	job := store.Job{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1",
		Kind: store.JobKindSync, State: store.JobQueued, Goal: "do the thing"}
	h := createForJob(t, fake, "sb-01HELD", job.ID)
	info, err := h.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sess := store.Session{ID: store.NewID(t0), OrgID: "org", OwnerID: "u1",
		Name: "job-held", State: store.StateRunning, SandboxID: info.ID}
	if err := mem.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	job.SessionID = sess.ID
	if err := mem.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	for _, st := range []store.JobState{store.JobProvisioning, store.JobRunning, store.JobDelivering, store.JobSucceeded} {
		job.State = st
		if err := mem.Jobs().Update(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Sessions().Get(ctx, "org", sess.ID); got.State != store.StateRunning || fake.Running() != 1 {
		t.Fatalf("after a pass the held session is %s with %d sandboxes running", got.State, fake.Running())
	}
	if err := h.Terminate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Sessions().Get(ctx, "org", sess.ID); got.State.Live() {
		t.Errorf("a held session whose sandbox ended is still %s", got.State)
	}
}
