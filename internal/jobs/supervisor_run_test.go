package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// inboxTextRaw is an inbox frame carrying an item of a given type and
// text, in the measured shape: the text sits at data.item.payload.text,
// as in the captured frame in frames_test.go.
func inboxTextRaw(t *testing.T, itemType, text string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "session.inbox.enqueued",
		"data": map[string]any{"item": map[string]any{
			"type":     itemType,
			"payload":  map[string]any{"text": text},
			"delivery": "steer",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func stopMarkerText() string {
	return opencode.LoopStoppedMarker + ` {"reason":"limit","continuations":40,"turns":41}`
}

// The completion signal is the plugin's announcement and nothing else.
//
// @decision(DL-048)
func TestTheLoopsOwnAnnouncementEndsTheRun(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	f.put(t, EventInboxEnqueued, inboxTextRaw(t, "synthetic", stopMarkerText()))

	if err := f.sup.followOnce(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobDelivering {
		t.Fatalf("state = %v, want %v", got.State, store.JobDelivering)
	}
}

// A long turn produces no events for minutes and must never be read as
// a finished run.
//
// @decision(DL-048)
func TestSilenceIsNotCompletion(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	// A turn ended, then nothing at all for many passes: exactly what a
	// long-running turn looks like from the store.
	f.put(t, "session.execution.succeeded", nil)

	for i := 0; i < 20; i++ {
		if err := f.sup.followOnce(context.Background(), f.job); err != nil {
			t.Fatal(err)
		}
		got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != store.JobRunning {
			t.Fatalf("pass %d: state = %v, want the job still running;"+
				" silence must never be read as completion", i, got.State)
		}
		f.job = got
	}
}

// The marker must be spov's own, not anything that merely contains the
// token. The agent can write the string into its reply, and a human can
// type it; neither may end a job.
//
// @decision(DL-048)
func TestOnlyASyntheticCanAnnounceTheStop(t *testing.T) {
	cases := []struct {
		name     string
		itemType string
		text     string
		want     jobSignal
	}{
		{"the plugin's own announcement", "synthetic", stopMarkerText(), signalStopped},
		// A person's item is read at its delivery, never as a stop.
		{"a human typing the marker", "user", stopMarkerText(), signalNone},
		{"the marker inside a longer message", "synthetic", "I will print " + opencode.LoopStoppedMarker, signalNone},
		{"an ordinary continuation", "synthetic", "Continue working towards your goal.", signalNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := store.Event{
				Source: store.SourceOpenCode, Name: EventInboxEnqueued,
				Raw: inboxTextRaw(t, tc.itemType, tc.text),
			}
			if got := signalFor(ev); got != tc.want {
				t.Errorf("signal = %v, want %v", got, tc.want)
			}
		})
	}
}

// The Go constant and the plugin's own literal are the same string. If
// they drift, every job stops at `running` and nothing says why.
//
// @decision(DL-048)
func TestTheStopMarkerMatchesThePlugin(t *testing.T) {
	src, err := (&opencode.Plugin{Kind: "report", Goal: "g"}).Source()
	if err != nil {
		t.Fatal(err)
	}
	want := `const STOP_MARKER = "` + opencode.LoopStoppedMarker + `"`
	if !strings.Contains(src, want) {
		t.Fatalf("the plugin does not declare %s;"+
			" the Go constant and the JavaScript literal have drifted, which strands every job at running", want)
	}
}

// The backstop: a job whose sandbox is gone must not sit at `running`
// forever, even though nothing announced its stop.
//
// @decision(DL-045)
func TestASandboxThatStoppedConcludesTheRun(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	ctx := context.Background()
	sess, err := f.st.Sessions().Get(ctx, f.job.OrgID, f.job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State = store.StateStopped
	if err := f.st.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	if err := f.sup.followOnce(ctx, f.job); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobDelivering {
		t.Fatalf("state = %v, want %v: a job whose sandbox is gone must not sit at running",
			got.State, store.JobDelivering)
	}
	if got.Error == "" {
		t.Error("the row should say the sandbox stopped before the loop reported finishing")
	}
}

// The backstop must not fire while the sandbox is up, or it becomes the
// quiescence heuristic by another route.
//
// @decision(DL-048)
func TestTheBackstopLeavesALiveSandboxAlone(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	ctx := context.Background()
	// No events at all, and a running session: a long turn.
	if err := f.sup.followOnce(ctx, f.job); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobRunning {
		t.Fatalf("state = %v, want the job still running", got.State)
	}
}

// The run loop is resumable: each attempt does what the row says is
// outstanding, never restarting from the beginning.
//
// @decision(DL-045)
func TestTheRunLoopResumesRatherThanRestarting(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobRunning)

	var provisioned int
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Provision: func(context.Context, store.Job) error { provisioned++; return nil },
		Open:      func(context.Context, store.Job) error { return nil },
	}
	// A running job re-delivered by River must not provision again.
	if _, err := sup.step(ctx, job); err != nil {
		t.Fatal(err)
	}
	if provisioned != 0 {
		t.Fatalf("provisioned %d sandboxes for a job already running; a redelivery must adopt, never launch again",
			provisioned)
	}
}

// A queued job provisions; a provisioning job opens. Each state does
// its own step and no other.
//
// @decision(DL-045)
func TestEachStateDoesOnlyItsOwnStep(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		state           store.JobState
		wantProvisioned int
		wantOpened      int
	}{
		{store.JobQueued, 1, 0},
		{store.JobProvisioning, 0, 1},
		{store.JobRunning, 0, 0},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			st := store.NewMemory()
			job := jobstest.SeedJob(t, st, tc.state)
			var provisioned, opened int
			sup := &Supervisor{
				Store: st, Warn: func(string) {},
				Provision: func(context.Context, store.Job) error { provisioned++; return nil },
				Open:      func(context.Context, store.Job) error { opened++; return nil },
			}
			if _, err := sup.step(ctx, job); err != nil {
				t.Fatal(err)
			}
			if provisioned != tc.wantProvisioned {
				t.Errorf("provisioned = %d, want %d", provisioned, tc.wantProvisioned)
			}
			if opened != tc.wantOpened {
				t.Errorf("opened = %d, want %d", opened, tc.wantOpened)
			}
		})
	}
}

// A launch that fails must not be retried by River: each retry
// provisions again, which is three sandboxes for one broken goal.
//
// @decision(DL-045)
func TestAFailedLaunchFailsTheJobRatherThanRetrying(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobQueued)
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Provision: func(context.Context, store.Job) error { return errors.New("modal said no") },
	}
	if _, err := sup.step(ctx, job); err != nil {
		t.Fatalf("step returned %v; a failed launch must not be returned as an error River would retry", err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobFailed {
		t.Fatalf("state = %v, want %v", got.State, store.JobFailed)
	}
	if !strings.Contains(got.Error, "modal said no") {
		t.Errorf("Error = %q, want it to name what failed", got.Error)
	}
}

// A failed opening turn IS worth retrying: the sandbox is up and paid
// for, and the failure is one request into it.
//
// @decision(DL-045)
func TestAFailedOpeningTurnIsRetried(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobProvisioning)
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Open: func(context.Context, store.Job) error { return errors.New("connection reset") },
	}
	if _, err := sup.step(ctx, job); err == nil {
		t.Fatal("step returned nil; a failed opening turn should be retried, not swallowed")
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobProvisioning {
		t.Fatalf("state = %v, want the job left at %v so a retry finds its sandbox by tag",
			got.State, store.JobProvisioning)
	}
}

// A cancel that lands mid-loop wins: the run loop must never resurrect
// a job someone has already stopped.
//
// @decision(DL-045)
func TestACancelledJobIsNotResurrected(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobRunning)

	cancelled := job
	cancelled.State = store.JobCancelled
	if err := st.Jobs().Update(ctx, cancelled); err != nil {
		t.Fatal(err)
	}
	sup := &Supervisor{Store: st, Warn: func(string) {}}
	// The stale row this pass holds still says running.
	if err := sup.move(ctx, job, store.JobDelivering, ""); err != nil {
		t.Fatal(err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobCancelled {
		t.Fatalf("state = %v, want the cancel to stand", got.State)
	}
}

// The evidence read is best-effort and never fails a job that ran.
//
// @decision(DL-045) @decision(DL-050)
func TestEvidenceThatCannotBeCollectedDoesNotFailTheJob(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Evidence: func(context.Context, store.Job) ([]byte, error) {
			return nil, errors.New("the sandbox is gone")
		},
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatalf("deliver returned %v; missing evidence is missing evidence, not a failure", err)
	}
}

// Evidence lands as a spov marker on the session's own event stream, so
// the transcript a human reads and the rows the verdict reads are the
// same thing.
//
// @decision(DL-045) @decision(DL-018)
func TestEvidenceIsRecordedAsASpovMarker(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	var gotName, gotText string
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Evidence: func(context.Context, store.Job) ([]byte, error) {
			return []byte(`{"event":"loop.stopped","reason":"limit"}`), nil
		},
		Mark: func(_ context.Context, _ store.Job, name, text string) error {
			gotName, gotText = name, text
			return nil
		},
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatal(err)
	}
	if gotName != MarkerEvidence {
		t.Errorf("marker = %q, want %q", gotName, MarkerEvidence)
	}
	if !strings.Contains(gotText, "loop.stopped") {
		t.Errorf("text = %q, want the plugin's own record", gotText)
	}
}

// The sweep re-queues every unfinished job and no finished one.
//
// @decision(DL-045)
func TestTheSweepRequeuesUnfinishedJobsOnly(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	running := jobstest.SeedJob(t, st, store.JobRunning)
	done := jobstest.SeedJob(t, st, store.JobFailed)

	var queued []string
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Queue: func(_ context.Context, args JobArgs) error {
			queued = append(queued, args.JobID)
			return nil
		},
	}
	if err := sup.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0] != running.ID {
		t.Fatalf("queued = %v, want just the unfinished job %s (and never %s)", queued, running.ID, done.ID)
	}
}

// One job that cannot be queued must not stop the rest being recovered.
//
// @decision(DL-045)
func TestTheSweepKeepsGoingPastOneFailure(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	for i := 0; i < 3; i++ {
		jobstest.SeedJob(t, st, store.JobRunning)
	}
	var attempts int
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Queue: func(context.Context, JobArgs) error {
			attempts++
			if attempts == 1 {
				return errors.New("the queue is down")
			}
			return nil
		},
	}
	if err := sup.Sweep(ctx); err == nil {
		t.Error("Sweep should report that a job could not be re-queued")
	}
	if attempts != 3 {
		t.Fatalf("attempted %d of 3; one failure must not abandon the sweep", attempts)
	}
}

// The follow interval is a store read, not a sandbox read, so it costs
// no sandbox time. Guarding the default keeps that true by accident
// rather than by intention.
//
// @decision(DL-045) @decision(DL-018)
func TestTheFollowIntervalIsOverridable(t *testing.T) {
	sup := &Supervisor{}
	if got := sup.followInterval(); got != FollowInterval {
		t.Errorf("default = %v, want %v", got, FollowInterval)
	}
	sup.FollowInterval = time.Millisecond
	if got := sup.followInterval(); got != time.Millisecond {
		t.Errorf("override = %v, want 1ms", got)
	}
}

// A supervisor wired with Evidence but no Mark, a wiring fault since
// serve sets both, must not panic and take a River worker with it.
//
// @decision(DL-045)
func TestCollectingEvidenceWithNowhereToPutItDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Evidence: func(context.Context, store.Job) ([]byte, error) {
			return []byte("some evidence"), nil
		},
		// Mark deliberately nil.
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatal(err)
	}
}

// A report over the size limit is recorded as such, and nothing of it
// is stored: a cut report would read as a complete one.
//
// @decision(DL-051)
func TestAReportOverTheLimitIsNotCut(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	out := &fakeOut{files: map[string][]byte{"big.md": make([]byte, MaxFileBytes+1)}}
	sup := &Supervisor{Store: st, Warn: func(string) {}, OpenOut: out.open}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatal(err)
	}
	got := members(t, st, job)
	if len(got) != 1 || got[0].Status != store.DeliverableOverLimit {
		t.Fatalf("members = %+v, want the report over the limit", got)
	}
	if _, err := st.Jobs().JobFile(ctx, job.OrgID, job.ID, "big.md"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("part of a report over the limit was stored: %v", err)
	}
}

// A redelivered attempt adopts the sandbox already running the job and
// never launches a second one.
//
// @decision(DL-045) @decision(DL-046)
func TestARedeliveredAttemptAdoptsRatherThanLaunchingAgain(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobQueued)

	var launched int
	var adopted string
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Provider: &fakeJobProvider{running: []string{"sb-already-up"}},
		Provision: func(context.Context, store.Job) error {
			launched++
			return nil
		},
		Adopt: func(_ context.Context, _ store.Job, id string) error {
			adopted = id
			return nil
		},
	}
	if _, err := sup.step(ctx, job); err != nil {
		t.Fatal(err)
	}
	if launched != 0 {
		t.Fatalf("launched %d sandboxes while one was already tagged for this job;"+
			" a duplicate bills to the same cap and nothing watches the abandoned one", launched)
	}
	// The id must be recorded, not merely not-launched: a sandbox with
	// no session row pointing at it is adopted by the reconciler as an
	// ordinary session and counts against the owner's SESSION quota
	// while the job counts against the job one.
	if adopted != "sb-already-up" {
		t.Fatalf("adopted = %q, want the sandbox recorded onto the job's session", adopted)
	}
}

// A provider that cannot answer must not fall through to a launch: the
// one thing worse than not adopting is buying a duplicate because the
// question could not be asked.
//
// @decision(DL-045) @decision(DL-046)
func TestAListingFailureDoesNotBecomeASecondSandbox(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobQueued)

	var launched int
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Provider: &fakeJobProvider{err: errors.New("modal is unreachable")},
		Provision: func(context.Context, store.Job) error {
			launched++
			return nil
		},
	}
	if _, err := sup.step(ctx, job); err != nil {
		t.Fatal(err)
	}
	if launched != 0 {
		t.Fatalf("launched %d sandboxes after the listing failed;"+
			" an unanswerable question must not become a duplicate", launched)
	}
}

// A launch that concluded without a sandbox, as the orphan sweep leaves
// one when a replica dies mid-provision, fails the job with the
// session's own reason instead of leaving it in `provisioning`.
//
// @decision(DL-045) @decision(DL-009)
func TestASessionThatFailedToLaunchFailsTheJob(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobProvisioning)

	sess, err := st.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State = store.StateFailed
	sess.Error = "the image could not be resolved"
	if err := st.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if _, err := sup.step(ctx, job); err != nil {
		t.Fatal(err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobFailed {
		t.Fatalf("state = %v, want %v", got.State, store.JobFailed)
	}
	if !strings.Contains(got.Error, "image could not be resolved") {
		t.Errorf("Error = %q, want the session's own reason carried onto the job", got.Error)
	}
}

// A session still starting is waited on, not concluded: the bound is
// the reconciler's orphan sweep, and a second one here would race it.
//
// @decision(DL-045)
func TestAStartingSessionIsWaitedOn(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobProvisioning)

	sess, err := st.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State = store.StateStarting
	if err := st.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	var opened int
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Open: func(context.Context, store.Job) error { opened++; return nil },
	}
	if _, err := sup.step(ctx, job); err != nil {
		t.Fatal(err)
	}
	if opened != 0 {
		t.Errorf("opened the goal %d times against a sandbox that is not up", opened)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobProvisioning {
		t.Fatalf("state = %v, want the job left at %v", got.State, store.JobProvisioning)
	}
}

// fakeJobProvider answers the tag query and nothing else: the run loop
// only ever lists.
type fakeJobProvider struct {
	running []string
	err     error
}

func (f *fakeJobProvider) ExitSnapshot(context.Context, string, time.Duration) (string, error) {
	return "", sandbox.ErrNoExitSnapshot
}

func (f *fakeJobProvider) Create(context.Context, sandbox.Spec) (sandbox.Handle, error) {
	return nil, errors.New("the run loop must not create through the provider")
}

func (f *fakeJobProvider) Get(context.Context, string) (sandbox.Handle, error) {
	return nil, errors.New("not used")
}

func (f *fakeJobProvider) List(context.Context, map[string]string) ([]sandbox.Info, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]sandbox.Info, 0, len(f.running))
	for _, id := range f.running {
		out = append(out, sandbox.Info{ID: id, State: sandbox.StateRunning, CreatedAt: time.Now()})
	}
	return out, nil
}

// An attempt advances the job one step and asks to be redelivered,
// rather than holding a worker slot for the job's whole life.
//
// @decision(DL-045)
func TestAnAttemptSnoozesRatherThanHoldingItsWorker(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobRunning)

	var snoozed time.Duration
	var snoozes int
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		FollowInterval: 7 * time.Second,
		Snooze: func(d time.Duration) error {
			snoozes++
			snoozed = d
			return nil
		},
	}
	if err := sup.Run(ctx, JobArgs{JobID: job.ID, OrgID: job.OrgID}, 1); err != nil {
		t.Fatal(err)
	}
	if snoozes != 1 {
		t.Fatalf("snoozed %d times, want exactly 1: an unfinished job must be redelivered", snoozes)
	}
	if snoozed != 7*time.Second {
		t.Errorf("snoozed for %v, want the follow interval", snoozed)
	}
}

// A job that has reached its end must not ask to be redelivered, or the
// queue would carry it forever.
//
// @decision(DL-045)
func TestAFinishedJobDoesNotSnooze(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	var snoozes int
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Snooze: func(time.Duration) error { snoozes++; return nil },
	}
	if err := sup.Run(ctx, JobArgs{JobID: job.ID, OrgID: job.OrgID}, 1); err != nil {
		t.Fatal(err)
	}
	if snoozes != 0 {
		t.Fatalf("snoozed %d times for a job that has finished delivering", snoozes)
	}
}

// The sweep runs on a timer and stops with its context.
//
// @decision(DL-045)
func TestRunSweepStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := store.NewMemory()
	jobstest.SeedJob(t, st, store.JobRunning)

	swept := make(chan struct{}, 4)
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		SweepInterval: time.Millisecond,
		Queue: func(context.Context, JobArgs) error {
			select {
			case swept <- struct{}{}:
			default:
			}
			return nil
		},
	}
	done := make(chan struct{})
	go func() { defer close(done); sup.RunSweep(ctx) }()

	// The first sweep is immediate: a replica that has just taken the
	// lock is the one most likely to have just restarted.
	select {
	case <-swept:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep did not run promptly; a restart would leave jobs stalled")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunSweep did not stop with its context")
	}
}

// The sweep interval is overridable, and defaults to the constant.
//
// @decision(DL-045)
func TestTheSweepIntervalIsOverridable(t *testing.T) {
	sup := &Supervisor{}
	if got := sup.sweepInterval(); got != SweepInterval {
		t.Errorf("default = %v, want %v", got, SweepInterval)
	}
	sup.SweepInterval = time.Second
	if got := sup.sweepInterval(); got != time.Second {
		t.Errorf("override = %v, want 1s", got)
	}
}

// A sweep with no queue does nothing rather than erroring: local mode
// has no supervisor at all, so this is only reachable mid-wiring.
//
// @decision(DL-045)
func TestASweepWithNoQueueIsANoOp(t *testing.T) {
	st := store.NewMemory()
	jobstest.SeedJob(t, st, store.JobRunning)
	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if err := sup.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep with no queue returned %v, want nil", err)
	}
}

// A job whose session has vanished must not sit at running forever.
//
// @decision(DL-045)
func TestAJobWhoseSessionIsGoneConcludes(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobRunning)
	// Point the job at a session that does not exist.
	job.SessionID = store.NewID(time.Now())

	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if err := sup.checkAbandoned(ctx, job); err != nil {
		t.Fatal(err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobDelivering {
		t.Fatalf("state = %v, want %v", got.State, store.JobDelivering)
	}
}

// A report job with no report is not failed by the collection, which
// only reports the absence; it is failed by the verdict, where gate 2
// classifies it. The test holds both halves.
//
// @decision(DL-045) @decision(DL-050) @decision(DL-051)
func TestAReportJobWithNoReportIsClassifiedNotCollected(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	out := &fakeOut{files: map[string][]byte{"notes.txt": []byte("not a report")}}
	sup := &Supervisor{Store: st, Warn: func(string) {}, OpenOut: out.open}
	// The collection itself does not fail the job.
	if err := sup.collectOut(ctx, job); err != nil {
		t.Fatalf("collectOut returned %v; a job that produced no file still ran", err)
	}
	mid, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mid.State != store.JobDelivering {
		t.Errorf("after collection state = %v, want it still delivering: the absence is the verdict's to classify",
			mid.State)
	}

	// The verdict is what fails it, and on gate 2 specifically.
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatalf("deliver returned %v", err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobFailed {
		t.Errorf("state = %v, want failed: a report job that produced no report did not deliver", got.State)
	}
	if !strings.Contains(got.Error, "left no report") {
		t.Errorf("error = %q, want it to name the missing report", got.Error)
	}
}

// A pr job has no out directory to read: its deliverables are pull
// requests, which gate 2 checks from spov's own side.
//
// @decision(DL-045) @decision(DL-050)
func TestAPRJobReadsNoOutDirectory(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	job.Kind = store.JobKindPR

	opened := 0
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		OpenOut: func(context.Context, store.Job) (OutDir, error) { opened++; return &fakeOut{}, nil },
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatal(err)
	}
	if opened != 0 {
		t.Errorf("opened the out directory %d times for a pr job", opened)
	}
}

// A listing that fails outright is a warning, not a failed job: the run
// happened, and gate 2 is unverified rather than failed.
//
// @decision(DL-045) @decision(DL-050) @decision(DL-051)
func TestAFailedCollectionDoesNotFailTheJob(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)
	var warned []string
	out := &fakeOut{listErr: errors.New("the sandbox refused")}
	sup := &Supervisor{
		Store: st, Warn: func(s string) { warned = append(warned, s) },
		OpenOut: out.open, Mark: markInto(st),
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatalf("deliver returned %v, want the failure warned about instead", err)
	}
	if len(warned) == 0 {
		t.Error("a failed collection should be said out loud")
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g := gateNamed(Verdict{Gates: ParseGatesNote(got.Verdict)}, GateDeliverable); g.Outcome != GateUnverified {
		t.Errorf("gate 2 = %+v, want unverified: spov's read broke, not the job", g)
	}
}

// A job with no queue to be redelivered by makes one pass and stops,
// which is what a test driving Run directly wants.
//
// @decision(DL-045)
func TestRunWithoutASnoozeMakesOnePass(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobRunning)
	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if err := sup.Run(ctx, JobArgs{JobID: job.ID, OrgID: job.OrgID}, 1); err != nil {
		t.Fatal(err)
	}
}

// A job that is gone is not retried: nothing will bring it back, and
// River would re-run this to its attempt limit provisioning nothing.
//
// @decision(DL-046)
func TestAMissingJobEndsTheAttempt(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	sup := &Supervisor{Store: st, Warn: func(string) {}}
	err := sup.Run(ctx, JobArgs{JobID: store.NewID(time.Now()), OrgID: "org_1"}, 1)
	if err != nil {
		t.Fatalf("Run returned %v for a job that does not exist; River would retry that", err)
	}
}

// A failure that cannot itself be written is warned about rather than
// returned: the caller has already decided not to retry, and turning a
// store hiccup into a River retry would provision again.
//
// @decision(DL-045)
func TestAnUnwritableFailureIsWarnedNotRetried(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobQueued)
	// Already cancelled, so the move to failed is refused.
	cancelled := job
	cancelled.State = store.JobCancelled
	if err := st.Jobs().Update(ctx, cancelled); err != nil {
		t.Fatal(err)
	}
	var warned []string
	sup := &Supervisor{
		Store: st, Warn: func(s string) { warned = append(warned, s) },
		Provision: func(context.Context, store.Job) error { return errors.New("modal said no") },
	}
	// The stale row this pass holds still says queued.
	if _, err := sup.step(ctx, job); err != nil {
		t.Fatalf("step returned %v; a refused failure write must not become a retry", err)
	}
	got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobCancelled {
		t.Errorf("state = %v, want the cancel to stand", got.State)
	}
	if len(warned) == 0 {
		t.Error("the refused write should be said out loud")
	}
}

// A supervisor with no way to open the goal is a wiring fault and says
// so, rather than leaving the job at provisioning forever.
//
// @decision(DL-045)
func TestNoOpenSeamIsAWiringFault(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobProvisioning)
	sup := &Supervisor{Store: st, Warn: func(string) {}}
	if _, err := sup.step(ctx, job); err == nil {
		t.Error("step returned nil with no Open wired; the job would sit at provisioning forever")
	}
}

// The stop marker is found in the real frame shape: the captured frame
// from frames_test.go, with the item's type and text changed to what the
// plugin sends.
//
// @decision(DL-048) @decision(DL-017)
func TestTheStopMarkerIsFoundInARealFrameShape(t *testing.T) {
	raw := `{"id":"evt_0000000000sampleevent00001","created":1788267414303,` +
		`"type":"session.inbox.enqueued","location":{"directory":"/workspace/spov"},` +
		`"data":{"inboxID":"msg_0000000000sampleinbox00001","sessionID":"ses_0000000000samplesession000",` +
		`"item":{"type":"synthetic","payload":{"text":"` + opencode.LoopStoppedMarker +
		` {\"reason\":\"limit\"}"},"delivery":"steer"}},` +
		`"durable":{"aggregateID":"ses_0000000000samplesession000","seq":30,"version":1}}`

	ev := store.Event{Source: store.SourceOpenCode, Name: EventInboxEnqueued, Raw: []byte(raw)}
	if got := signalFor(ev); got != signalStopped {
		t.Fatalf("signal = %v, want signalStopped;"+
			" the marker was not found at data.item.payload.text, which is where a real frame carries it", got)
	}

	// And a real human's frame, unchanged from the capture, must still
	// read as steering rather than as a stop.
	human := `{"id":"evt_1","created":1788267414303,` +
		`"type":"session.inbox.enqueued","data":{"sessionID":"ses_1",` +
		`"item":{"type":"user","payload":{"text":"reply with ok"},"delivery":"steer"}},` +
		`"durable":{"aggregateID":"ses_1","seq":31,"version":1}}`
	ev = store.Event{Source: store.SourceOpenCode, Name: EventInboxEnqueued, Raw: []byte(human)}
	if got := signalFor(ev); got != signalNone || !takesOver(ev) {
		t.Errorf("signal = %v, takesOver = %v; want no signal of its own and a takeover on delivery", got, takesOver(ev))
	}
}

// The sweep does not re-deliver a delivering job, which would read its
// sandbox on every sweep.
//
// @decision(DL-045) @decision(DL-018)
func TestTheSweepLeavesADeliveringJobAlone(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	running := jobstest.SeedJob(t, st, store.JobRunning)
	jobstest.SeedJob(t, st, store.JobDelivering)

	var queued []string
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Queue: func(_ context.Context, args JobArgs) error {
			queued = append(queued, args.JobID)
			return nil
		},
	}
	if err := sup.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0] != running.ID {
		t.Fatalf("queued = %v, want only the running job;"+
			" re-delivering a delivering job reads its sandbox on a timer", queued)
	}
}

// Delivery happens once. A redelivered job must not read the sandbox
// again — that is a timed sandbox read by another name.
//
// @decision(DL-045) @decision(DL-018)
func TestDeliveryDoesNotRepeat(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobDelivering)

	var reads int
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Evidence: func(context.Context, store.Job) ([]byte, error) {
			reads++
			return []byte(`{"event":"loop.stopped"}`), nil
		},
		// The seam is implemented directly rather than through
		// Service.MarkJobEvent: what this asserts is that deliver runs
		// once, and the marker's own write is that method's test.
		Mark: func(ctx context.Context, j store.Job, name, text string) error {
			now := time.Now()
			_, err := st.Conversations().PutEvents(ctx, []store.Event{{
				ID: store.NewID(now), OrgID: j.OrgID, SessionID: j.SessionID,
				Source: store.SourceSpov, Name: name, Text: text, At: now,
			}})
			return err
		},
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := sup.deliver(ctx, job); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("read the sandbox %d times; delivery must happen once", reads)
	}
}

// A cancel landing between the read and the move must stop the launch.
// Provisioning anyway buys a sandbox for a job nobody wants.
//
// @decision(DL-045)
func TestACancelBeforeTheMoveStopsTheLaunch(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobQueued)

	cancelled := job
	cancelled.State = store.JobCancelled
	if err := st.Jobs().Update(ctx, cancelled); err != nil {
		t.Fatal(err)
	}

	var launched int
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Provision: func(context.Context, store.Job) error { launched++; return nil },
	}
	// The stale row this pass holds still says queued.
	if err := sup.provision(ctx, job); err != nil {
		t.Fatal(err)
	}
	if launched != 0 {
		t.Fatalf("launched %d sandboxes for a job cancelled under the pass", launched)
	}
}

// A suspended job whose sandbox died is concluded rather than left
// holding its owner's quota: it goes to delivering and is judged.
//
// @decision(DL-045)
func TestASuspendedJobWhoseSandboxDiedIsConcluded(t *testing.T) {
	for _, tc := range []struct {
		from store.JobState
		want store.JobState
	}{
		{store.JobRunning, store.JobDelivering},
		{store.JobSteered, store.JobDelivering},
		{store.JobBlocked, store.JobDelivering},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			job := jobstest.SeedJob(t, st, tc.from)
			sess, err := st.Sessions().Get(ctx, job.OrgID, job.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			sess.State = store.StateStopped
			if err := st.Sessions().Update(ctx, sess); err != nil {
				t.Fatal(err)
			}

			sup := &Supervisor{Store: st, Warn: func(string) {}}
			if err := sup.checkAbandoned(ctx, job); err != nil {
				t.Fatal(err)
			}
			got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.want {
				t.Fatalf("state = %v, want %v: a suspension with no sandbox left holds quota forever",
					got.State, tc.want)
			}
		})
	}
}

// A job whose session row is gone is routed exactly like one whose
// session merely stopped.
//
// @decision(DL-045)
func TestAJobWhoseSessionVanishedIsRoutedByItsState(t *testing.T) {
	for _, tc := range []struct {
		from store.JobState
		want store.JobState
	}{
		{store.JobRunning, store.JobDelivering},
		{store.JobSteered, store.JobDelivering},
		{store.JobBlocked, store.JobDelivering},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			job := jobstest.SeedJob(t, st, tc.from)
			// SessionStore has no Delete, so a job pointed at a
			// session ID nothing answers for is how this branch is
			// reached at all — the same shape
			// TestAJobWhoseSessionIsGoneConcludes uses.
			job.SessionID = store.NewID(time.Now())

			sup := &Supervisor{Store: st, Warn: func(string) {}}
			if err := sup.checkAbandoned(ctx, job); err != nil {
				t.Fatal(err)
			}
			got, err := st.Jobs().Get(ctx, job.OrgID, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.want {
				t.Fatalf("state = %v, want %v", got.State, tc.want)
			}
		})
	}
}

// abandonedState only picks edges DL-045's machine has, and picks
// delivering wherever the machine allows it. The two are pinned to each
// other rather than each to a literal.
//
// @decision(DL-045)
func TestTheAbandonedRoutingIsAnEdgeTheMachineHas(t *testing.T) {
	for _, from := range []store.JobState{store.JobRunning, store.JobBlocked, store.JobSteered} {
		to := abandonedState(from)
		if !store.CanTransition(from, to) {
			t.Errorf("%v -> %v: the machine has no such edge, so the backstop writes nothing", from, to)
		}
		// Delivering is taken exactly where it is reachable. A state
		// that gains the edge and keeps being failed here is the
		// quieter half of the drift.
		if want := store.CanTransition(from, store.JobDelivering); (to == store.JobDelivering) != want {
			t.Errorf("%v: routed to %v, but the machine %s a delivering edge",
				from, to, map[bool]string{true: "has", false: "has no"}[want])
		}
	}
}

// The backstop routes on the row the pass left, not the row it found,
// so a job suspended in this pass and one suspended a pass earlier
// reach the same state. Both suspensions reach delivering, so this pins
// that a job suspended in the same pass is concluded.
//
// @decision(DL-045) @decision(DL-049)
func TestTheBackstopReadsTheRowThePassLeft(t *testing.T) {
	for _, tc := range []struct {
		name    string
		suspend func(f *followFixture)
		want    store.JobState
	}{
		{
			name: "steered",
			suspend: func(f *followFixture) {
				f.putHuman(t)
			},
			want: store.JobDelivering,
		},
		{
			name: "blocked",
			suspend: func(f *followFixture) {
				f.put(t, "session.execution.succeeded", nil)
				f.putEphemeral(t, "permission.asked")
			},
			want: store.JobDelivering,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFollowFixture(t, store.JobRunning)
			sess, err := f.st.Sessions().Get(ctx, f.job.OrgID, f.job.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			sess.State = store.StateStopped
			if err := f.st.Sessions().Update(ctx, sess); err != nil {
				t.Fatal(err)
			}
			tc.suspend(f)

			if err := f.sup.followOnce(ctx, f.job); err != nil {
				t.Fatal(err)
			}
			got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.want {
				t.Fatalf("state = %v, want %v: the pass suspended the job and then asked the"+
					" backstop about the state it no longer had", got.State, tc.want)
			}
		})
	}
}

// putMessage appends one stored message.
func (f *followFixture) putMessage(t *testing.T, role, text string) {
	t.Helper()
	if _, err := f.st.Conversations().PutMessages(context.Background(), []store.Message{{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		ConversationID: f.conv, OpenCodeID: store.NewID(time.Now()),
		Role: role, Text: text, At: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
}

// A stop marker whose event a recording gap lost is recovered from the
// message store, where the recorder also wrote it (DL-048). The backstop
// would not fire while the sandbox is alive.
//
// @decision(DL-045) @decision(DL-048)
func TestAStopMarkerLostInAGapIsRecoveredFromTheTranscript(t *testing.T) {
	f := newFollowFixture(t, store.JobRunning)
	// The gap stands where the marker's event would have been, and the
	// message is what the recorder stored for the same announcement.
	f.putGap(t)
	f.putMessage(t, "synthetic", stopMarkerText())

	if err := f.sup.followOnce(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobDelivering {
		t.Fatalf("state = %v, want %v: a job whose completion signal fell in a gap stays "+
			"at running with a live sandbox, billing until it is cancelled by hand",
			got.State, store.JobDelivering)
	}
}

// The transcript recovery keeps isLoopStopped's guarantees: neither a
// human typing the marker nor the agent quoting it in a longer reply
// ends a job.
//
// @decision(DL-045) @decision(DL-048)
func TestTheGapRecoveryCannotBeForged(t *testing.T) {
	for _, tc := range []struct {
		name string
		role string
		text string
	}{
		{"a human typing the marker", "user", stopMarkerText()},
		{"the agent quoting the marker", "assistant", stopMarkerText()},
		{"the marker inside a longer reply", "synthetic", "I will now write " + stopMarkerText()},
		{"an ordinary synthetic", "synthetic", "Continue working towards your standing goal."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFollowFixture(t, store.JobRunning)
			f.putGap(t)
			f.putMessage(t, tc.role, tc.text)

			if err := f.sup.followOnce(context.Background(), f.job); err != nil {
				t.Fatal(err)
			}
			got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State == store.JobDelivering {
				t.Errorf("%s ended the job: the recovery must keep isLoopStopped's own "+
					"conditions, or a gap turns the marker into something anyone can forge",
					tc.name)
			}
		})
	}
}

// The transcript recovery is bounded by the job's state, not by whether
// a gap was seen (DL-048): a marker lost in the opening window before the
// recorder attaches leaves no gap, and only a `running` job is recovered.
//
// @decision(DL-045) @decision(DL-048)
func TestTheTranscriptRecoveryIsBoundedByStateNotByAGap(t *testing.T) {
	// No gap anywhere: the marker's event was never recorded because
	// nothing was listening when it went past.
	t.Run("a running job is recovered with no gap", func(t *testing.T) {
		f := newFollowFixture(t, store.JobRunning)
		f.putMessage(t, "synthetic", stopMarkerText())

		if err := f.sup.followOnce(context.Background(), f.job); err != nil {
			t.Fatal(err)
		}
		got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != store.JobDelivering {
			t.Errorf("state = %v, want %v: a job whose marker never reached the event log "+
				"stays at running with a live sandbox, billing until it is cancelled by hand",
				got.State, store.JobDelivering)
		}
	})

	// The states that must NOT be reconsidered. Each is suspended by
	// something a human has to release, and the plugin's loop ending
	// does not release it.
	for _, state := range []store.JobState{store.JobBlocked, store.JobSteered} {
		t.Run("a "+string(state)+" job is left alone", func(t *testing.T) {
			f := newFollowFixture(t, state)
			f.putMessage(t, "synthetic", stopMarkerText())

			if err := f.sup.followOnce(context.Background(), f.job); err != nil {
				t.Fatal(err)
			}
			got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != state {
				t.Errorf("state = %v, want %v: a parked job was concluded from the "+
					"transcript under the human who parked it", got.State, state)
			}
		})
	}
}

// The recorder is nudged before the goal is posted, so it dials while
// the agent starts rather than after a short run is over.
//
// @decision(DL-017)
func TestTheRecorderIsNudgedBeforeTheGoalIsPosted(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobProvisioning)
	// The session has to be running for open to reach the goal.
	sess, err := st.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State = store.StateRunning
	sess.SandboxID = "sb-1"
	if err := st.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	var order []string
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Wake: func(s store.Session) {
			if s.ID != job.SessionID {
				t.Errorf("nudged session %q, want the job's %q", s.ID, job.SessionID)
			}
			order = append(order, "wake")
		},
		Open: func(context.Context, store.Job) error {
			order = append(order, "open")
			return nil
		},
	}
	if _, err := sup.step(ctx, job); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "wake" || order[1] != "open" {
		t.Errorf("order = %v, want [wake open]: nudging after the goal loses the frames "+
			"the recorder exists to capture", order)
	}
}

// A supervisor with no recorder opens the goal all the same: a nil
// Wake is local mode, not a configuration fault.
//
// @decision(DL-017)
func TestASupervisorWithNoRecorderStillOpensTheGoal(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	job := jobstest.SeedJob(t, st, store.JobProvisioning)
	sess, err := st.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State = store.StateRunning
	sess.SandboxID = "sb-1"
	if err := st.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	opened := 0
	sup := &Supervisor{
		Store: st, Warn: func(string) {},
		Open: func(context.Context, store.Job) error { opened++; return nil },
	}
	if _, err := sup.step(ctx, job); err != nil {
		t.Fatalf("a nil Wake broke the run loop: %v", err)
	}
	if opened != 1 {
		t.Errorf("opened = %d, want 1", opened)
	}
}

// declaredMarkerText is the marker as the plugin writes it when the
// agent called the completion tool.
func declaredMarkerText() string {
	return opencode.LoopStoppedMarker +
		` {"reason":"declared","continuations":0,"maxContinuations":40,"turns":2}`
}

// The reason separates a statement from an ending. Only `declared` is
// the agent saying the goal is met; every other reason is the loop
// giving out, and only a running job moves on those.
//
// @decision(DL-048)
func TestTheStopReasonSeparatesADeclarationFromAnEnding(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want jobSignal
	}{
		{"the agent declared the goal met", declaredMarkerText(), signalDeclared},
		{"the loop was stopped", opencode.LoopStoppedMarker + ` {"reason":"stopped"}`, signalStopped},
		{"no completion tool", opencode.LoopStoppedMarker + ` {"reason":"no-completion-tool"}`, signalStopped},
		{"a continuation could not be sent", opencode.LoopStoppedMarker + ` {"reason":"continue-failed"}`, signalStopped},
		// Fail soft, in the direction that does not release a
		// suspension: a marker this build cannot read is still a
		// marker, but it is not a declaration.
		{"an unparseable payload", opencode.LoopStoppedMarker + " {not json", signalStopped},
		{"no payload at all", opencode.LoopStoppedMarker, signalStopped},
		{"a reason this build does not know", opencode.LoopStoppedMarker + ` {"reason":"invented"}`, signalStopped},
		// Anything after the payload makes it unreadable, and the
		// fallback must be the conservative one. The plugin writes no
		// trailing text today; this pins the direction of the failure
		// rather than the shape of the input.
		{"text after the payload", opencode.LoopStoppedMarker + ` {"reason":"declared"} and more`, signalStopped},
		{"extra whitespace around the payload", opencode.LoopStoppedMarker + `  {"reason":"declared"}  `, signalDeclared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := store.Event{
				Source: store.SourceOpenCode, Name: EventInboxEnqueued,
				Raw: inboxTextRaw(t, "synthetic", tc.text),
			}
			if got := signalFor(ev); got != tc.want {
				t.Errorf("signal = %v, want %v", got, tc.want)
			}
		})
	}
}

// A human typing the declaration marker steers the job; it does not end
// it. The synthetic requirement is what carries this, and it must hold
// for the declaring reason exactly as it does for the others.
//
// @decision(DL-048) @decision(DL-045)
func TestADeclarationCannotBeForged(t *testing.T) {
	ev := store.Event{
		Source: store.SourceOpenCode, Name: EventInboxEnqueued,
		Raw: inboxTextRaw(t, "user", declaredMarkerText()),
	}
	if got := signalFor(ev); got != signalNone || !takesOver(ev) {
		t.Errorf("signal = %v, takesOver = %v: a human typing the marker must not end a job, and takes it over", got, takesOver(ev))
	}
}

// A steered job whose agent declares the goal met reaches delivering
// through the real follow pass, so what it delivered is collected.
//
// @decision(DL-048)
func TestASteeredJobIsEndedByADeclaration(t *testing.T) {
	f := newFollowFixture(t, store.JobSteered)
	f.put(t, EventInboxEnqueued, inboxTextRaw(t, "synthetic", declaredMarkerText()))

	if err := f.sup.followOnce(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobDelivering {
		t.Fatalf("state = %v, want %v: a declared completion must release a steered job,"+
			" or what it delivered is never collected", got.State, store.JobDelivering)
	}
}

// A steered job is not concluded because the loop merely gave out (a
// stop, no completion tool, a failed continuation); only a declaration
// releases it.
//
// @decision(DL-048) @decision(DL-045)
func TestASteeredJobSurvivesTheLoopMerelyStopping(t *testing.T) {
	for _, reason := range []string{"stopped", "no-completion-tool", "continue-failed", opencode.TurnFailedReason} {
		t.Run(reason, func(t *testing.T) {
			f := newFollowFixture(t, store.JobSteered)
			f.put(t, EventInboxEnqueued, inboxTextRaw(t, "synthetic",
				opencode.LoopStoppedMarker+` {"reason":"`+reason+`"}`))

			if err := f.sup.followOnce(context.Background(), f.job); err != nil {
				t.Fatal(err)
			}
			got, err := f.st.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != store.JobSteered {
				t.Errorf("state = %v, want the job still steered: only a declaration releases it", got.State)
			}
		})
	}
}

// A declaration read after the sandbox has gone is still delivered:
// followOnce skips checkAbandoned when the pass saw a stop, so the
// backstop cannot overwrite `delivering` in the same pass.
//
// @decision(DL-048) @decision(DL-045)
func TestADeclarationSurvivesADeadSandbox(t *testing.T) {
	for _, from := range []store.JobState{store.JobRunning, store.JobSteered} {
		t.Run(string(from), func(t *testing.T) {
			f := newFollowFixture(t, from)
			ctx := context.Background()
			// The sandbox is gone by the time the pass reads the marker.
			sess, err := f.st.Sessions().Get(ctx, f.job.OrgID, f.job.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			sess.State = store.StateStopped
			if err := f.st.Sessions().Update(ctx, sess); err != nil {
				t.Fatal(err)
			}
			f.put(t, EventInboxEnqueued, inboxTextRaw(t, "synthetic", declaredMarkerText()))

			if err := f.sup.followOnce(ctx, f.job); err != nil {
				t.Fatal(err)
			}
			got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != store.JobDelivering {
				t.Errorf("state = %v, want %v: the run declared completion, so it is delivered"+
					" rather than failed for a sandbox that is gone", got.State, store.JobDelivering)
			}
		})
	}
}
