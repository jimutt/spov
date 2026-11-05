//go:build integration

// A job's whole life, against a real Postgres and a real River queue,
// with the sandbox faked so it costs nothing:
//
//	SPOV_TEST_DATABASE_URL=postgres://... go test -tags integration ./internal/server/ -run JobLifecycle -v
//
// Everything else covering the run loop is a unit test over fakes, and
// the pieces this puts together — the queue, the state machine, the
// store's transitions, the snooze that brings an attempt back — are
// exactly the ones a fake cannot tell you are wired correctly.
//
// It needs a database and nothing else: no Modal credentials, no
// GitHub OAuth app, and it creates no sandbox. Every test here carries
// "JobLifecycle" in its name so that selector runs all of them.
//
// @decision(DL-045)
package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

// jobLifecycleFixture is a supervisor whose sandbox work is recorded
// rather than done, over a real store and a real queue.
type jobLifecycleFixture struct {
	sup        *jobs.Supervisor
	st         store.Store
	job        store.Job
	provisions int
	opens      int
	collects   int
	marks      int
	terminates int
}

func newJobLifecycleFixture(t *testing.T, opts ...func(*store.Job)) *jobLifecycleFixture {
	t.Helper()
	pool := jobPool(t)
	ctx := context.Background()
	st := postgres.NewWithPool(pool)

	now := time.Now()
	owner := store.NewID(now)
	if err := st.Users().Create(ctx, store.User{
		ID: owner, OrgID: "default", Email: owner + "@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	sess := store.Session{
		ID: store.NewID(now), OrgID: "default", OwnerID: owner,
		Name: "job-" + store.NewID(now), State: store.StateStarting,
		Request: store.SessionRequest{Egress: "agent"},
	}
	job := store.Job{
		ID: store.NewID(now), OrgID: "default", OwnerID: owner,
		SessionID: sess.ID, Kind: store.JobKindReport,
		State: store.JobQueued, Goal: "write the report",
	}
	for _, o := range opts {
		o(&job)
	}
	if err := st.Tx(ctx, func(tx store.Store) error {
		if err := tx.Sessions().Create(ctx, sess); err != nil {
			return err
		}
		return tx.Jobs().Create(ctx, job)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The rows outlive the test otherwise, and the next run's
		// listings see them. Terminal first, so the machine allows it.
		clean := context.WithoutCancel(ctx)
		if j, err := st.Jobs().Get(clean, job.OrgID, job.ID); err == nil && !j.State.Terminal() {
			j.State = store.JobCancelled
			_ = st.Jobs().Update(clean, j)
		}
	})

	f := &jobLifecycleFixture{st: st, job: job}
	f.sup = &jobs.Supervisor{
		Store: st,
		Warn:  func(string) {},
		// The sandbox is faked: this test is about the sequence, and a
		// real one would cost money to prove something Modal's own
		// integration tests already prove.
		Provision: func(ctx context.Context, j store.Job) error {
			f.provisions++
			// What a real launch commits: a running session with a
			// sandbox. The loop reads this back to decide it may open.
			s, err := st.Sessions().Get(ctx, j.OrgID, j.SessionID)
			if err != nil {
				return err
			}
			s.State = store.StateRunning
			s.SandboxID = "sb-" + j.ID
			return st.Sessions().Update(ctx, s)
		},
		Open: func(context.Context, store.Job) error { f.opens++; return nil },
		// @decision(DL-051)
		OpenOut:  func(context.Context, store.Job) (jobs.OutDir, error) { f.collects++; return lifecycleOut{}, nil },
		Evidence: func(context.Context, store.Job) ([]byte, error) { return []byte(`{"event":"loop.stopped"}`), nil },
		// The sandbox is stopped when the job ends. Counted, because
		// nothing else stops it: a delivered job would otherwise keep
		// its sandbox to Modal's 24h cap.
		//
		// @decision(DL-050)
		Terminate: func(ctx context.Context, j store.Job) error {
			f.terminates++
			sess, err := st.Sessions().Get(ctx, j.OrgID, j.SessionID)
			if err != nil {
				return err
			}
			if !sess.State.Live() {
				return nil
			}
			sess.State = store.StateStopped
			return st.Sessions().Update(ctx, sess)
		},
		Mark: func(ctx context.Context, j store.Job, name, text string) error {
			f.marks++
			_, err := st.Conversations().PutEvents(ctx, []store.Event{{
				ID: store.NewID(time.Now()), OrgID: j.OrgID, SessionID: j.SessionID,
				Source: store.SourceSpov, Name: name, Text: text, At: time.Now(),
			}})
			return err
		},
	}
	return f
}

// pass drives one attempt and returns the job as the store now has it.
func (f *jobLifecycleFixture) pass(t *testing.T) store.Job {
	t.Helper()
	ctx := context.Background()
	if err := f.sup.Run(ctx, jobs.JobArgs{JobID: f.job.ID, OrgID: f.job.OrgID}, 1); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.job = got
	return got
}

// A job walks queued -> provisioning -> running -> delivering, one step
// per attempt, against the real store's own transition rules.
//
// @decision(DL-045)
func TestIntegrationJobLifecycleWalksTheStateMachine(t *testing.T) {
	f := newJobLifecycleFixture(t)
	ctx := context.Background()

	if got := f.pass(t); got.State != store.JobProvisioning {
		t.Fatalf("after the first pass: state = %v, want %v", got.State, store.JobProvisioning)
	}
	if f.provisions != 1 {
		t.Fatalf("provisioned %d times, want 1", f.provisions)
	}

	if got := f.pass(t); got.State != store.JobRunning {
		t.Fatalf("after the second pass: state = %v, want %v", got.State, store.JobRunning)
	}
	if f.opens != 1 {
		t.Fatalf("opened the goal %d times, want 1", f.opens)
	}

	// A running job stays running until the plugin says its loop has
	// stopped. This is the pass that would fail if silence were being
	// read as completion.
	if got := f.pass(t); got.State != store.JobRunning {
		t.Fatalf("a quiet pass moved the job to %v; silence is not completion", got.State)
	}

	// The plugin's own announcement, as a real frame.
	conv := store.NewID(time.Now())
	if _, err := f.st.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: conv, OrgID: f.job.OrgID, SessionID: f.job.SessionID, OpenCodeID: "ses_1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		ConversationID: conv, Source: store.SourceOpenCode, Seq: 1,
		Name: jobs.EventInboxEnqueued, At: time.Now(),
		Raw: []byte(`{"type":"session.inbox.enqueued","data":{"item":{"type":"synthetic",` +
			`"payload":{"text":"SPOV_LOOP_STOPPED {\"reason\":\"limit\"}"},"delivery":"steer"}}}`),
	}}); err != nil {
		t.Fatal(err)
	}

	if got := f.pass(t); got.State != store.JobDelivering {
		t.Fatalf("after the loop announced its stop: state = %v, want %v", got.State, store.JobDelivering)
	}

	// Delivery collects once and JUDGES, so the job reaches an end
	// rather than resting at delivering.
	got := f.pass(t)
	if f.collects != 1 {
		t.Fatalf("collected %d times, want exactly 1", f.collects)
	}
	if body := collectedReport(t, f.st, got); body != "# report" {
		t.Errorf("report = %q, want the collected one", body)
	}
	// The report exists, so gate 2 passed; nothing was refused, so gate
	// 3 did not veto. A job that stopped at delivering here is one
	// nothing will ever conclude — and whose sandbox nothing will stop.
	//
	// @decision(DL-050)
	if got.State != store.JobSucceeded {
		t.Fatalf("after delivery: state = %v (%q), want %v",
			got.State, got.Verdict, store.JobSucceeded)
	}
	// The verdict carries its reasoning, not only its conclusion.
	for _, want := range []string{jobs.GateMechanical, jobs.GateDeliverable, jobs.GateClassification} {
		if !strings.Contains(got.Verdict, want) {
			t.Errorf("the row does not record gate %q:\n%s", want, got.Verdict)
		}
	}
	// The sandbox is stopped. Nothing else stops it, and a delivered
	// job would otherwise bill to Modal's 24h cap.
	if f.terminates != 1 {
		t.Fatalf("terminated %d times, want 1: the job ended with its sandbox still running", f.terminates)
	}
	sess, err := f.st.Sessions().Get(ctx, f.job.OrgID, f.job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State.Live() {
		t.Errorf("session state = %v after the job ended; it is still billing", sess.State)
	}

	// And a further attempt does not deliver again: that would be a
	// sandbox read on a timer.
	f.pass(t)
	if f.collects != 1 {
		t.Fatalf("collected %d times after a redelivery; delivery must happen once", f.collects)
	}
	if f.terminates != 1 {
		t.Errorf("terminated %d times after a redelivery; a terminal job is left alone", f.terminates)
	}
}

// A redelivered attempt does only what the row says is outstanding,
// against the real store rather than a fake that cannot refuse an
// illegal transition.
//
// @decision(DL-045)
func TestIntegrationJobLifecycleResumesRatherThanRestarting(t *testing.T) {
	f := newJobLifecycleFixture(t)

	f.pass(t) // queued -> provisioning
	f.pass(t) // provisioning -> running

	// Five more deliveries of a job already running.
	for range 5 {
		if got := f.pass(t); got.State != store.JobRunning {
			t.Fatalf("a redelivery moved the job to %v", got.State)
		}
	}
	if f.provisions != 1 {
		t.Fatalf("provisioned %d times across 7 attempts; a redelivery must never launch again",
			f.provisions)
	}
	if f.opens != 1 {
		t.Fatalf("opened the goal %d times; a redelivery must not re-issue it", f.opens)
	}
}

// A cancel lands mid-flight and the loop must not resurrect the job.
// The real store enforces the machine, so this is where a transition
// the fake would have allowed is actually refused.
//
// @decision(DL-045)
func TestIntegrationJobLifecycleRespectsACancel(t *testing.T) {
	f := newJobLifecycleFixture(t)
	ctx := context.Background()

	f.pass(t) // queued -> provisioning

	cancelled := f.job
	cancelled.State = store.JobCancelled
	if err := f.st.Jobs().Update(ctx, cancelled); err != nil {
		t.Fatal(err)
	}

	before := f.opens
	got := f.pass(t)
	if got.State != store.JobCancelled {
		t.Fatalf("state = %v, want the cancel to stand", got.State)
	}
	if f.opens != before {
		t.Error("the loop opened a goal for a cancelled job")
	}
}

// The quota counts a job in every unfinished state, which is what stops
// a caller queueing a thousand jobs against a limit of two. Counted
// against the real SQL rather than the memory store's own arithmetic.
//
// @decision(DL-045) @decision(DL-009)
func TestIntegrationJobLifecycleCountsAgainstTheQuotaUntilItEnds(t *testing.T) {
	f := newJobLifecycleFixture(t)
	ctx := context.Background()

	for _, want := range []store.JobState{store.JobQueued, store.JobProvisioning, store.JobRunning} {
		if f.job.State != want {
			f.pass(t)
		}
		n, err := f.st.Jobs().CountLive(ctx, f.job.OrgID, f.job.OwnerID)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("a %v job counts %d against the quota, want 1", f.job.State, n)
		}
	}

	// Terminal, and it stops counting.
	done := f.job
	done.State = store.JobCancelled
	if err := f.st.Jobs().Update(ctx, done); err != nil {
		t.Fatal(err)
	}
	n, err := f.st.Jobs().CountLive(ctx, f.job.OrgID, f.job.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a cancelled job still counts %d against the quota", n)
	}
}

// putRaw appends one frame to the job's conversation.
func (f *jobLifecycleFixture) putRaw(t *testing.T, conv string, seq int64, raw string) {
	t.Helper()
	if _, err := f.st.Conversations().PutEvents(context.Background(), []store.Event{{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		ConversationID: conv, Source: store.SourceOpenCode, Seq: seq,
		Name: frameType(raw), At: time.Now(), Raw: []byte(raw),
	}}); err != nil {
		t.Fatal(err)
	}
}

// frameType is a raw frame's own name, as the recorder stores it.
func frameType(raw string) string {
	var frame struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(raw), &frame)
	return frame.Type
}

// conversation creates the job's conversation, once.
func (f *jobLifecycleFixture) conversation(t *testing.T) string {
	t.Helper()
	conv := store.NewID(time.Now())
	if _, err := f.st.Conversations().UpsertConversation(context.Background(), store.Conversation{
		ID: conv, OrgID: f.job.OrgID, SessionID: f.job.SessionID, OpenCodeID: "ses_1",
	}); err != nil {
		t.Fatal(err)
	}
	return conv
}

// humanItem is a person's message that hands the job over, enqueued,
// and humanDelivered its delivery, which is where the takeover happens
// (DL-052).
func humanItem(id string) string {
	return `{"type":"session.inbox.enqueued","data":{"inboxID":"` + id + `","item":{"type":"user",` +
		`"payload":{"text":"hold on, let me look"},"delivery":"steer"}}}`
}

func humanDelivered(id string) string {
	return `{"type":"session.inbox.delivered","data":{"inboxID":"` + id + `"}}`
}

const stopFrame = `{"type":"session.inbox.enqueued","data":{"item":{"type":"synthetic",` +
	`"payload":{"text":"SPOV_LOOP_STOPPED {\"reason\":\"declared\"}"},"delivery":"steer"}}}`

// A job steered by a human, handed back, and finishing on its own —
// against the real store, the real transition rules and the real Pos
// ordering, which the memory store and Postgres each assign their own
// way. It also proves the pause is enacted where the row is written,
// since the supervisor's Loop seam is driven by the real followOnce
// rather than called directly.
//
// @decision(DL-054) @decision(DL-045)
func TestIntegrationJobLifecycleSteersResumesAndFinishes(t *testing.T) {
	f := newJobLifecycleFixture(t)
	ctx := context.Background()
	var paused []bool
	f.sup.Loop = func(_ context.Context, _ auth.Principal, _ store.Job, p bool) error {
		paused = append(paused, p)
		return nil
	}

	f.pass(t) // queued -> provisioning
	f.pass(t) // provisioning -> running

	conv := f.conversation(t)
	f.putRaw(t, conv, 1, humanItem("msg_first"))
	f.putRaw(t, conv, 2, humanDelivered("msg_first"))

	if got := f.pass(t); got.State != store.JobSteered {
		t.Fatalf("a human's message left the job %v, want steered", got.State)
	}
	// The row reaching steered enacts the pause, so the plugin cannot
	// keep working under a suspended row and lose its deliverables.
	if len(paused) != 1 || !paused[0] {
		t.Fatalf("the loop was told %v; writing steered must pause it", paused)
	}

	// The resume: the boundary marker, then the row. A second human
	// item is added FIRST and left unread, which is the race the marker
	// exists for — the cursor advances only on a pass, so a message
	// typed just before the resume is still unconsumed when the row
	// moves.
	f.putRaw(t, conv, 3, humanItem("msg_second"))
	f.putRaw(t, conv, 4, humanDelivered("msg_second"))
	if _, err := f.st.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(time.Now()), OrgID: f.job.OrgID, SessionID: f.job.SessionID,
		Source: store.SourceSpov, Name: jobs.MarkerResumed, Text: "resumed by a person", At: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	job, err := f.st.Jobs().Get(ctx, f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.State = store.JobRunning
	job.Error = ""
	if err := f.st.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	f.job = job

	if got := f.pass(t); got.State != store.JobRunning {
		t.Fatalf("the pass after the resume left the job %v; the stale message undid it", got.State)
	}

	// And the agent finishes on its own, as it would have if nobody
	// had taken it over. That is the whole promise: a steered job is
	// suspended, not ended.
	f.putRaw(t, conv, 5, stopFrame)
	if got := f.pass(t); got.State != store.JobDelivering {
		t.Fatalf("the resumed job did not finish: state = %v, want delivering", got.State)
	}
	// One pause across the whole life. The resume is the service's, not
	// the supervisor's, so nothing here should have written to the loop
	// again.
	if len(paused) != 1 {
		t.Errorf("the loop was written to %v; the supervisor pauses and never resumes", paused)
	}
}

// A job runs out its wall clock against the real store: the locked read,
// the move and the marker commit together, and delivery then stops the
// run before collecting and opens the verdict with the bound.
//
// @decision(DL-049)
func TestIntegrationJobLifecycleEndsAtItsWallClock(t *testing.T) {
	f := newJobLifecycleFixture(t, func(j *store.Job) { j.MaxDuration = time.Minute })
	var endRuns []store.JobState
	f.sup.EndRun = func(ctx context.Context, j store.Job) error {
		cur, err := f.st.Jobs().Get(ctx, j.OrgID, j.ID)
		if err != nil {
			return err
		}
		endRuns = append(endRuns, cur.State)
		return nil
	}
	for _, want := range []store.JobState{store.JobProvisioning, store.JobRunning} {
		if got := f.pass(t); got.State != want {
			t.Fatalf("state = %v, want %v", got.State, want)
		}
	}
	running := f.job
	if running.MaxDuration != time.Minute || running.ClockStartedAt == nil {
		t.Fatalf("the stored clock is %s from %v; want 1m, started", running.MaxDuration, running.ClockStartedAt)
	}

	// Inside the clock, a pass leaves it running.
	f.sup.Now = func() time.Time { return running.ClockStartedAt.Add(30 * time.Second) }
	if got := f.pass(t); got.State != store.JobRunning {
		t.Fatalf("inside the clock: state = %v", got.State)
	}

	f.sup.Now = func() time.Time { return running.ClockStartedAt.Add(2 * time.Minute) }
	bounded := f.pass(t)
	if bounded.State != store.JobDelivering || !strings.Contains(bounded.Error, "1m0s wall clock") {
		t.Fatalf("at the clock: state %v, note %q", bounded.State, bounded.Error)
	}
	if len(endRuns) != 0 {
		t.Errorf("the run was stopped in the pass that moved it: %v", endRuns)
	}

	done := f.pass(t)
	if done.State.Terminal() == false {
		t.Fatalf("after delivery: state %v", done.State)
	}
	if len(endRuns) != 1 || endRuns[0] != store.JobDelivering {
		t.Errorf("EndRun saw %v, want one call while delivering", endRuns)
	}
	if !strings.HasPrefix(done.Verdict, bounded.Error) {
		t.Errorf("the verdict %q does not open with the bound", done.Verdict)
	}
	if body := collectedReport(t, f.st, done); body != "# report" {
		t.Errorf("report = %q; the report written before the bound was not kept", body)
	}
}

// jobPool is a pool against SPOV_TEST_DATABASE_URL, skipping when there
// is none.
//
// internal/jobs has the same helper for its own integration tests.
// Duplicated rather than shared: it is four lines of setup with no
// behaviour to drift, and the alternative is a non-test package
// existing only to hold it.
//
// @decision(DL-046)
func jobPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SPOV_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SPOV_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// lifecycleOut is an out directory holding one report.
//
// @decision(DL-051)
type lifecycleOut struct{}

func (lifecycleOut) List(context.Context, string) ([]jobs.OutEntry, error) {
	return []jobs.OutEntry{{Path: "report.md"}}, nil
}

func (lifecycleOut) Read(context.Context, string, int64) ([]byte, error) {
	return []byte("# report"), nil
}

// collectedReport is the body of the job's verified report.md.
func collectedReport(t *testing.T, st store.Store, job store.Job) string {
	t.Helper()
	ctx := context.Background()
	got, err := st.Jobs().Deliverables(ctx, job.OrgID, []string{job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[job.ID]) != 1 || got[job.ID][0].Status != store.DeliverableVerified {
		t.Fatalf("deliverables = %+v, want report.md verified", got[job.ID])
	}
	f, err := st.Jobs().JobFile(ctx, job.OrgID, job.ID, "report.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(f.Body)
}
