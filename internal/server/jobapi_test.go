package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// The job API. Every test here drives the real handlers over HTTP, so
// what is asserted is what a client sees.
//
// @decision(DL-045)

// jobFixture is an apiFixture with a supervisor whose queue records
// what it was handed rather than reaching Postgres. The queue is the
// only part of the supervisor this slice's surface touches.
func jobFixture(t *testing.T) (*apiFixture, *recordingQueue) {
	return jobFixtureWith(t, nil)
}

// jobFixtureWith is jobFixture with a chance to adjust Deps, for the
// tests that need a provider key configured.
//
// adjust runs AFTER the supervisor is built from d.Store and
// d.Provider, so replacing either in an adjust leaves the supervisor
// pointing at the old one. Nothing does today; if something needs to,
// build the supervisor after the call rather than working around it.
func jobFixtureWith(t *testing.T, adjust func(*Deps)) (*apiFixture, *recordingQueue) {
	t.Helper()
	q := &recordingQueue{}
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Jobs = &jobs.Supervisor{Store: d.Store, Provider: d.Provider, Queue: q.insert, Warn: d.Warn}
		if adjust != nil {
			adjust(d)
		}
	})
	return f, q
}

type recordingQueue struct {
	args []jobs.JobArgs
	err  error
}

func (q *recordingQueue) insert(_ context.Context, a jobs.JobArgs) error {
	if q.err != nil {
		return q.err
	}
	q.args = append(q.args, a)
	return nil
}

func jobRequest() JobRequest {
	return JobRequest{
		Goal:  "fix the failing test in internal/cli",
		Kind:  store.JobKindPR,
		Repos: []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}},
	}
}

// The happy path, and the two things a job's creation must produce: a
// queued row, and a session of its own for it to run in.
func TestCreateJobRecordsAQueuedJobWithItsOwnSession(t *testing.T) {
	f, q := jobFixture(t)
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var view JobView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.State != store.JobQueued {
		t.Errorf("a new job is %s, want queued", view.State)
	}
	if view.SessionID == "" {
		t.Fatal("the job has no session, so nothing can run it")
	}
	if view.CreatedAt.IsZero() || view.UpdatedAt.IsZero() {
		t.Errorf("the create answered without the stored times: created %v, updated %v", view.CreatedAt, view.UpdatedAt)
	}
	// The session is real, is the job's own, and has no sandbox yet:
	// provisioning is the supervisor's.
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, view.SessionID)
	if err != nil {
		t.Fatalf("the job's session was not recorded: %v", err)
	}
	if sess.SandboxID != "" {
		t.Errorf("the create provisioned a sandbox %q; that is the supervisor's job", sess.SandboxID)
	}
	if sess.State != store.StateStarting {
		t.Errorf("the job's session is %s, want starting", sess.State)
	}
	// And it reached the queue, exactly once, carrying the ids and
	// nothing else.
	if len(q.args) != 1 {
		t.Fatalf("the queue was handed %d jobs, want 1", len(q.args))
	}
	if q.args[0].JobID != view.ID || q.args[0].OrgID != sess.OrgID {
		t.Errorf("queued %+v, want the job's own ids", q.args[0])
	}
}

// The row is committed before the enqueue, so a queue failure leaves a
// visible queued job rather than losing the work. The caller is told,
// because their job will not start on its own.
func TestCreateJobKeepsTheRowWhenTheQueueRefuses(t *testing.T) {
	f, q := jobFixture(t)
	q.err = errors.New("the queue is down")

	resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if resp.StatusCode == http.StatusAccepted {
		t.Fatalf("a failed enqueue was reported as success: %s", body)
	}
	// The recorded job still holds a quota slot and nothing will run
	// it, so the error has to name it — otherwise the user cannot find
	// what to cancel, and two such failures make every later `spov jobs create`
	// a 429 for no visible reason.
	stored, err := f.store.Jobs().List(context.Background(), auth.LocalID, store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 1 && !strings.Contains(string(body), stored[0].ID) {
		t.Errorf("the error does not name the job that was left behind:\n%s", body)
	}
	// The job exists even though the request failed: losing it silently
	// would be worse than an error the user can act on.
	jobs, err := f.store.Jobs().List(context.Background(), auth.LocalID, store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs recorded, want the one whose enqueue failed", len(jobs))
	}
	if jobs[0].State != store.JobQueued {
		t.Errorf("the job is %s, want queued so a sweep can pick it up", jobs[0].State)
	}
}

// A server with no supervisor is local mode, which cannot run jobs. It
// says so rather than answering 404, which a client would read as a bad
// URL rather than as the wrong kind of server.
//
// @decision(DL-046)
func TestCreateJobRefusesInLocalMode(t *testing.T) {
	f := newAPIFixture(t) // no Deps.Jobs
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("create in local mode: %d %s, want 501", resp.StatusCode, body)
	}
	// Nothing was written: a refused create must not leave a session
	// behind for the reconciler to puzzle over.
	sessions, err := f.store.Sessions().List(context.Background(), auth.LocalID, store.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("a refused create left %d sessions", len(sessions))
	}
}

// The request checks. Each is a 422 rather than a 500 or a job that
// fails minutes later inside a sandbox.
func TestCreateJobRefusesABadRequest(t *testing.T) {
	for name, mutate := range map[string]func(*JobRequest){
		"no goal":      func(r *JobRequest) { r.Goal = "" },
		"blank goal":   func(r *JobRequest) { r.Goal = "   \n\t " },
		"unknown kind": func(r *JobRequest) { r.Kind = "sculpture" },
		"no repos":     func(r *JobRequest) { r.Repos = nil },
		"oversized goal": func(r *JobRequest) {
			r.Goal = strings.Repeat("x", MaxGoalBytes+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, q := jobFixture(t)
			req := jobRequest()
			mutate(&req)
			resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("= %d %s, want 422", resp.StatusCode, body)
			}
			if len(q.args) != 0 {
				t.Error("a refused request still reached the queue")
			}
		})
	}
}

// A job's concurrency quota, separate from the session limit.
func TestCreateJobIsBoundedByItsOwnQuota(t *testing.T) {
	f, _ := jobFixture(t)
	f.srv.deps.Quotas.MaxConcurrentJobs = 1
	f.srv.svc.deps.Quotas.MaxConcurrentJobs = 1

	if resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest()); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the first job: %d %s", resp.StatusCode, body)
	}
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the second job: %d %s, want 429", resp.StatusCode, body)
	}
}

// The job quota and the session quota are separate pools. A queue of
// jobs must not lock its owner out of `spov fly`.
func TestAJobDoesNotConsumeTheSessionQuota(t *testing.T) {
	f, _ := jobFixture(t)
	f.srv.deps.Quotas.MaxConcurrentSessions = 1
	f.srv.svc.deps.Quotas.MaxConcurrentSessions = 1

	if resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest()); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the job: %d %s", resp.StatusCode, body)
	}
	// With a session limit of one, an ordinary session must still be
	// creatable: the job's own session is counted against the job
	// quota, not this one.
	resp, body := f.do(http.MethodPost, "/api/v1/sessions", SessionRequest{
		Repos: []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a session alongside one job: %d %s — the job took the session slot", resp.StatusCode, body)
	}
}

// Listing, its state filter, and the refusal of a state that does not
// exist — which would otherwise return an empty list and read as "no
// such jobs".
func TestListJobsFiltersByStateAndRefusesAnUnknownOne(t *testing.T) {
	f, _ := jobFixture(t)
	var created JobView
	_, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	// A second job, moved on, so the filter has something to exclude.
	// With only one job in one state the filter cannot be told from no
	// filter at all: every query returns the same row.
	var other JobView
	_, body = f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if err := json.Unmarshal(body, &other); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	moved, err := f.store.Jobs().Get(ctx, auth.LocalID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []store.JobState{store.JobProvisioning, store.JobRunning} {
		moved.State = st
		if err := f.store.Jobs().Update(ctx, moved); err != nil {
			t.Fatal(err)
		}
	}

	list := func(query string) []JobView {
		t.Helper()
		resp, body := f.do(http.MethodGet, "/api/v1/jobs"+query, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list%s: %d %s", query, resp.StatusCode, body)
		}
		var out struct {
			Jobs []JobView `json:"jobs"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out.Jobs
	}

	if all := list(""); len(all) != 2 {
		t.Fatalf("an unfiltered listing returned %d jobs, want both", len(all))
	}
	queued := list("?state=queued")
	if len(queued) != 1 || queued[0].ID != created.ID {
		t.Errorf("state=queued returned %+v, want only the queued job", queued)
	}
	running := list("?state=running")
	if len(running) != 1 || running[0].ID != other.ID {
		t.Errorf("state=running returned %+v, want only the running job", running)
	}
	// Two states are one comma-separated parameter. huma keeps only the
	// first of a repeated parameter; that form is asserted too, so a
	// change in huma's parsing fails here.
	if both := list("?state=queued,running"); len(both) != 2 {
		t.Errorf("state=queued,running returned %d jobs, want both", len(both))
	}
	if repeated := list("?state=queued&state=running"); len(repeated) != 1 {
		t.Errorf("the repeated form returned %d jobs; huma's parsing has changed"+
			" and client.ListJobs can go back to one parameter per state", len(repeated))
	}
	if none := list("?state=succeeded"); len(none) != 0 {
		t.Errorf("state=succeeded returned %+v, want nothing", none)
	}
	resp, body := f.do(http.MethodGet, "/api/v1/jobs?state=loitering", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("an unknown state = %d %s, want 422", resp.StatusCode, body)
	}
}

// Cancellation: the row moves, the session is stopped, and a second
// cancel is not an error.
func TestCancelJobStopsItAndItsSession(t *testing.T) {
	f, _ := jobFixture(t)
	var created JobView
	_, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	if resp, body := f.do(http.MethodDelete, "/api/v1/jobs/"+created.ID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", resp.StatusCode, body)
	}
	ctx := context.Background()
	job, err := f.store.Jobs().Get(ctx, auth.LocalID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.JobCancelled {
		t.Errorf("the job is %s after a cancel", job.State)
	}
	if job.StoppedAt == nil {
		t.Error("a cancelled job has no stop time")
	}
	// The session goes with it: a job whose sandbox outlived its
	// cancellation would keep billing.
	sess, err := f.store.Sessions().Get(ctx, auth.LocalID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State.Live() {
		t.Errorf("the job's session is still %s after the job was cancelled", sess.State)
	}
	// Cancelling again is what a retried request does, and it must not
	// be an error.
	if resp, body := f.do(http.MethodDelete, "/api/v1/jobs/"+created.ID, nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("a second cancel = %d %s", resp.StatusCode, body)
	}
}

// A job someone has already finished with cannot be cancelled: the
// caller is looking at something other than what they think.
func TestCancelRefusesAFinishedJob(t *testing.T) {
	f, _ := jobFixture(t)
	var created JobView
	_, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	job, err := f.store.Jobs().Get(ctx, auth.LocalID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []store.JobState{store.JobProvisioning, store.JobRunning, store.JobDelivering, store.JobSucceeded} {
		job.State = st
		if err := f.store.Jobs().Update(ctx, job); err != nil {
			t.Fatalf("moving to %s: %v", st, err)
		}
	}
	resp, body := f.do(http.MethodDelete, "/api/v1/jobs/"+created.ID, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cancelling a succeeded job = %d %s, want 409", resp.StatusCode, body)
	}
}

// A job of another org is not reachable, and a missing one is 404
// rather than 500.
func TestGetJobIsScopedAndAbsenceIs404(t *testing.T) {
	f, _ := jobFixture(t)
	if resp, _ := f.do(http.MethodGet, "/api/v1/jobs/01ABSENTABSENTABSENTABSENT", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown job = %d, want 404", resp.StatusCode)
	}
	if resp, _ := f.do(http.MethodDelete, "/api/v1/jobs/01ABSENTABSENTABSENTABSENT", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("cancelling an unknown job = %d, want 404", resp.StatusCode)
	}
}

// The view carries what a client needs and not the supervisor's own
// bookkeeping: the cursor is an implementation detail of DL-046 and
// would invite a client to reason about event positions.
func TestJobViewDoesNotLeakTheCursor(t *testing.T) {
	j := store.Job{ID: "j", Kind: store.JobKindPR, State: store.JobRunning, Cursor: 99}
	b, err := json.Marshal(jobViewOf(j, nil))
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatal(err)
	}
	if _, ok := generic["cursor"]; ok {
		t.Error("the job view carries the supervisor's cursor")
	}
}

// Access control, at the service rather than over HTTP: the fixture's
// principal is a local admin, so the non-admin path — which is the one
// that hides another user's work — needs a principal of its own.
//
// @decision(DL-045) @decision(DL-006)
func TestJobsAreScopedToTheirOwner(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()
	svc := f.srv.svc

	alice := auth.Principal{UserID: "u-alice", OrgID: auth.LocalID}
	bob := auth.Principal{UserID: "u-bob", OrgID: auth.LocalID}
	admin := auth.Principal{UserID: "u-admin", OrgID: auth.LocalID, Admin: true}

	hers, err := svc.CreateJob(ctx, alice, jobRequest())
	if err != nil {
		t.Fatal(err)
	}
	his, err := svc.CreateJob(ctx, bob, jobRequest())
	if err != nil {
		t.Fatal(err)
	}

	// Each sees only their own.
	mine, err := svc.ListJobs(ctx, alice, store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].ID != hers.ID {
		t.Errorf("alice sees %+v, want only her own job", mine)
	}
	// And cannot reach the other's by id: 404, not 403, so the
	// existence of another user's job is not learnable.
	if _, err := svc.GetJob(ctx, alice, his.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("alice reading bob's job = %v, want not-found", err)
	}
	if err := svc.CancelJob(ctx, alice, his.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("alice cancelling bob's job = %v, want not-found", err)
	}
	// An admin sees both.
	all, err := svc.ListJobs(ctx, admin, store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("an admin sees %d jobs, want both", len(all))
	}
}

// The cancel note records who did it, so a job that stopped for no
// apparent reason has an answer in the row.
func TestCancelRecordsWhoAskedForIt(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()
	svc := f.srv.svc
	p := auth.Principal{UserID: "u-alice", OrgID: auth.LocalID}
	job, err := svc.CreateJob(ctx, p, jobRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelJob(ctx, p, job.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Error, "u-alice") {
		t.Errorf("the cancel note is %q; it does not say who", got.Error)
	}
	// A principal with no user id is local mode's single user, and is
	// named rather than left blank.
	if got := cancelledBy(auth.Principal{}); got != auth.LocalID {
		t.Errorf("cancelledBy for an anonymous principal = %q", got)
	}
}

// A job cancelled after it was provisioned takes the ordinary session
// delete, which terminates the sandbox. This is the branch the queued
// case skips, and it is the one that stops the billing.
func TestCancellingARunningJobTerminatesItsSandbox(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()
	svc := f.srv.svc
	p := auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID}
	job, err := svc.CreateJob(ctx, p, jobRequest())
	if err != nil {
		t.Fatal(err)
	}
	// Stand the session up as the supervisor would: a real sandbox from
	// the fake provider, recorded on the row.
	h, err := f.provider.Create(ctx, sandbox.Spec{
		Name: "job-sandbox", Image: "img", Command: []string{"x"},
		CPU: 0.5, MemoryMiB: 2048, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags: sandbox.JobTags(job.ID), Egress: sandbox.EgressOpen(),
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := h.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := f.store.Sessions().Get(ctx, auth.LocalID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State, sess.SandboxID = store.StateRunning, info.ID
	if err := f.store.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}
	job.State = store.JobProvisioning
	if err := f.store.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.State = store.JobRunning
	if err := f.store.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}

	if err := svc.CancelJob(ctx, p, job.ID); err != nil {
		t.Fatal(err)
	}
	after, err := f.provider.Get(ctx, info.ID)
	if err != nil {
		t.Fatalf("the sandbox is gone from the provider entirely: %v", err)
	}
	got, err := after.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == sandbox.StateRunning {
		t.Error("a cancelled job's sandbox is still running, and still billing")
	}
}

// `spov stop` on a job's session must cancel the job, not leave it
// queued against a session someone believed they had stopped.
//
// @decision(DL-045)
func TestDeletingAJobsSessionCancelsTheJob(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()
	var created JobView
	_, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	if resp, body := f.do(http.MethodDelete, "/api/v1/sessions/"+created.SessionID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete session: %d %s", resp.StatusCode, body)
	}
	job, err := f.store.Jobs().Get(ctx, auth.LocalID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.JobCancelled {
		t.Errorf("the job is %s after its session was deleted; it will still be worked", job.State)
	}
	sess, err := f.store.Sessions().Get(ctx, auth.LocalID, created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State.Live() {
		t.Errorf("the session is still %s after being deleted", sess.State)
	}
}

// An ordinary session is unaffected: the job lookup must not change how
// a session with no job is deleted.
//
// @decision(DL-045)
func TestDeletingAnOrdinarySessionIsUnchanged(t *testing.T) {
	f, _ := jobFixture(t)
	id := f.create(SessionRequest{Repos: []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}}})
	// The launch is asynchronous; delete is only meaningful once it has
	// a sandbox to terminate.
	deadline := time.Now().Add(5 * time.Second)
	for {
		sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, id)
		if err != nil {
			t.Fatal(err)
		}
		if sess.State == store.StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session never reached running: %s", sess.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp, body := f.do(http.MethodDelete, "/api/v1/sessions/"+id, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, body)
	}
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State != store.StateStopped {
		t.Errorf("an ordinary session is %s after delete, want stopped", sess.State)
	}
}

// A job's session carries the requested git identity and the request's
// other session fields, so `spov jobs create` sends what `spov fly` would.
//
// @decision(DL-045) @decision(DL-044) @decision(DL-049)
func TestAJobsSessionCarriesTheRequestedIdentityAndShape(t *testing.T) {
	f, _ := jobFixture(t)
	req := jobRequest()
	req.Image = "custom-image"
	req.Egress = "open"
	req.Sandbox = SandboxConfigView{CPU: 2, MemoryMiB: 4096, TimeoutSecs: 1800, IdleSecs: 300}
	req.Git = GitIdentityView{Name: "Ada Lovelace", Email: "ada@example.com"}

	var view JobView
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, view.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Request.GitName != "Ada Lovelace" || sess.Request.GitEmail != "ada@example.com" {
		t.Errorf("the job's session authors as %q <%s>; the job's commits are not the requester's",
			sess.Request.GitName, sess.Request.GitEmail)
	}
	if sess.Request.Image != "custom-image" {
		t.Errorf("image = %q, want the requested one", sess.Request.Image)
	}
	if sess.Request.Sandbox.CPU != 2 || sess.Request.Sandbox.MemoryMiB != 4096 {
		t.Errorf("the sandbox shape was not carried: %+v", sess.Request.Sandbox)
	}
	// The timeout is not the request's: it follows from the job's clock
	// (DL-049).
	if want := time.Hour + jobs.WallClockMargin + jobs.SteerHeadroom; sess.Request.Sandbox.Timeout != want {
		t.Errorf("timeout = %s, want %s from the clock", sess.Request.Sandbox.Timeout, want)
	}
	// Nor is the idle timeout: a job idles out only at its timeout
	// (DL-049).
	if sess.Request.Sandbox.IdleTimeout != sess.Request.Sandbox.Timeout {
		t.Errorf("idle timeout = %s, want the sandbox timeout %s", sess.Request.Sandbox.IdleTimeout, sess.Request.Sandbox.Timeout)
	}
}

// Cancelling a job whose provision never produced a sandbox concludes
// the session rather than leaving it `starting` with no launch to
// resolve it.
//
// @decision(DL-045) @decision(DL-003)
func TestCancellingAProvisioningJobConcludesItsSession(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()
	svc := f.srv.svc
	p := auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID}
	job, err := svc.CreateJob(ctx, p, jobRequest())
	if err != nil {
		t.Fatal(err)
	}
	// The supervisor took it and then the replica died: provisioning,
	// with no sandbox recorded.
	job.State = store.JobProvisioning
	if err := f.store.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}

	if err := svc.CancelJob(ctx, p, job.ID); err != nil {
		t.Fatal(err)
	}
	sess, err := f.store.Sessions().Get(ctx, auth.LocalID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State.Live() {
		t.Errorf("the session is %s after its job was cancelled; it holds a slot nothing will free", sess.State)
	}
}

// The check command is bounded like the goal: it reaches a shell inside
// the sandbox and a JavaScript literal in the plugin's config.
//
// @decision(DL-050)
func TestCreateJobBoundsTheCheckCommand(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()
	p := auth.Principal{UserID: "u-alice", OrgID: auth.LocalID}

	req := jobRequest()
	req.Check = strings.Repeat("x", MaxCheckBytes+1)
	if _, err := f.srv.svc.CreateJob(ctx, p, req); err == nil {
		t.Fatal("an oversized check command was accepted")
	} else if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want an invalid-request error", err)
	}

	// An ordinary one is fine, and reaches the row.
	req.Check = "./scripts/check.sh"
	job, err := f.srv.svc.CreateJob(ctx, p, req)
	if err != nil {
		t.Fatal(err)
	}
	if job.CheckCommand != "./scripts/check.sh" {
		t.Errorf("the row carries check %q, want ./scripts/check.sh", job.CheckCommand)
	}
}

// TestListJobsFiltersBySession covers the parameter the session page
// asks with. Two jobs exist so the filter can be told from no filter.
//
// @decision(DL-057)
func TestListJobsFiltersBySession(t *testing.T) {
	f, _ := jobFixture(t)
	create := func() JobView {
		t.Helper()
		var j JobView
		_, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
		if err := json.Unmarshal(body, &j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	mine, other := create(), create()
	if mine.SessionID == "" || mine.SessionID == other.SessionID {
		t.Fatalf("the fixture gave the two jobs sessions this test cannot tell apart: %q and %q",
			mine.SessionID, other.SessionID)
	}

	list := func(query string) []JobView {
		t.Helper()
		resp, body := f.do(http.MethodGet, "/api/v1/jobs"+query, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list%s: %d %s", query, resp.StatusCode, body)
		}
		var out struct {
			Jobs []JobView `json:"jobs"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out.Jobs
	}

	if all := list(""); len(all) != 2 {
		t.Fatalf("an unfiltered listing returned %d jobs, want both", len(all))
	}
	got := list("?session=" + mine.SessionID)
	if len(got) != 1 || got[0].ID != mine.ID {
		t.Fatalf("filtering on a session returned %+v, want only %s", got, mine.ID)
	}
	// A session with no job answers empty rather than answering
	// everything, which is what an ignored parameter would do.
	if got := list("?session=ses_nothing_here"); len(got) != 0 {
		t.Fatalf("a session with no job returned %d jobs: %+v", len(got), got)
	}
}

// TestAJobViewCarriesItsGatesAsStructure holds the projection: the
// client draws one gate differently from another, so the gates must
// reach it as fields rather than only inside the verdict blob.
//
// @decision(DL-057) @decision(DL-050)
func TestAJobViewCarriesItsGatesAsStructure(t *testing.T) {
	v := jobViewOf(store.Job{
		ID:      "j1",
		Verdict: "conduct: an edit was refused\n- check: passed (exited 0)\n- conduct: failed (an edit was refused)",
	}, nil)
	if len(v.Gates) != 2 {
		t.Fatalf("the view carried %d gates: %+v", len(v.Gates), v.Gates)
	}
	if v.Gates[1].Outcome != jobs.GateFailed || v.Gates[1].Gate != "conduct" {
		t.Errorf("the deciding gate did not survive the projection: %+v", v.Gates[1])
	}
	// The blob is kept beside the structure, so a client can still show
	// a line this build's parser could not read.
	if v.Verdict == "" {
		t.Error("the view dropped the verdict text once it had the gates")
	}
}

// resumeFixture is a job driven to one suspended state, with the
// service and principal a resume needs.
//
// @decision(DL-054)
func resumeFixture(t *testing.T, state store.JobState) (*apiFixture, *Service, auth.Principal, store.Job) {
	t.Helper()
	f, _ := jobFixture(t)
	ctx := context.Background()
	svc := f.srv.svc
	p := auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID}
	job, err := svc.CreateJob(ctx, p, jobRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []store.JobState{store.JobProvisioning, store.JobRunning, state} {
		job.State = st
		if err := f.store.Jobs().Update(ctx, job); err != nil {
			t.Fatalf("moving to %s: %v", st, err)
		}
	}
	return f, svc, p, job
}

// Resuming a blocked job moves it back to `running`, even when the loop
// release cannot reach a sandbox: a blocked job's loop is usually not
// held, so that release is best-effort (DL-052).
//
// @decision(DL-054) @decision(DL-045) @decision(DL-052)
func TestResumingABlockedJobHandsItBackToItsLoop(t *testing.T) {
	f, svc, p, job := resumeFixture(t, store.JobBlocked)
	ctx := context.Background()
	if err := svc.ResumeJob(ctx, p, job.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobRunning {
		t.Errorf("state = %v, want running", got.State)
	}
	// Resuming again is what a retried request does. It must not be an
	// error: the record makes retry the thing that closes the window
	// between the plugin write and the row write.
	if err := svc.ResumeJob(ctx, p, job.ID); err == nil {
		t.Error("a second resume succeeded; it should refuse a running job by name")
	} else if !errors.Is(err, ErrJobNotResumable) {
		t.Errorf("a second resume = %v, want ErrJobNotResumable", err)
	}
}

// The boundary marker is written, and it is spov's own so a sandbox
// cannot forge one. Without it the next follow pass re-reads the
// human's message and steers the job straight back.
//
// @decision(DL-054)
func TestAResumeWritesItsBoundaryMarker(t *testing.T) {
	f, svc, p, job := resumeFixture(t, store.JobBlocked)
	ctx := context.Background()
	if err := svc.ResumeJob(ctx, p, job.ID); err != nil {
		t.Fatal(err)
	}
	events, err := f.store.Conversations().ListEvents(ctx, auth.LocalID, job.SessionID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found *store.Event
	for i, e := range events {
		if e.Name == jobs.MarkerResumed {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatal("no resume marker was written, so the next pass would steer the job back")
	}
	if found.Source != store.SourceSpov {
		t.Errorf("the marker's source is %q; a sandbox could forge one that is not spov's", found.Source)
	}
	if !strings.Contains(found.Text, auth.LocalID) {
		t.Errorf("the marker %q does not say who resumed the job", found.Text)
	}
}

// A job that is neither steered nor blocked has nothing to resume, and
// the refusal names the state.
//
// @decision(DL-054)
func TestResumeRefusesAJobThatIsNotSuspended(t *testing.T) {
	for _, st := range []store.JobState{store.JobRunning, store.JobQueued, store.JobDelivering} {
		f, _ := jobFixture(t)
		ctx := context.Background()
		svc := f.srv.svc
		p := auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID}
		job, err := svc.CreateJob(ctx, p, jobRequest())
		if err != nil {
			t.Fatal(err)
		}
		for _, to := range jobstest.PathTo(st) {
			job.State = to
			if err := f.store.Jobs().Update(ctx, job); err != nil {
				t.Fatalf("moving to %s: %v", to, err)
			}
		}
		err = svc.ResumeJob(ctx, p, job.ID)
		if !errors.Is(err, ErrJobNotResumable) {
			t.Errorf("resuming a %s job = %v, want ErrJobNotResumable", st, err)
		}
	}
}

// Over HTTP: the refusal is a 409, not a 404 or a 400. The job exists
// and the caller may see it; its state does not admit the act.
//
// @decision(DL-054)
func TestResumeOverHTTPAnswers409ForARunningJob(t *testing.T) {
	f, _ := jobFixture(t)
	var created JobView
	_, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	resp, body := f.do(http.MethodPost, "/api/v1/jobs/"+created.ID+"/resume", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("resuming a queued job = %d %s, want 409", resp.StatusCode, body)
	}
}

// A steered or blocked job whose loop has already announced its stop is
// delivered rather than resumed. From `blocked` it goes via `running`,
// since the machine has no blocked -> delivering edge.
//
// @decision(DL-054) @decision(DL-048)
// @decision(DL-045)
func TestResumingAJobWhoseLoopAlreadyStoppedDeliversIt(t *testing.T) {
	for _, from := range []store.JobState{store.JobSteered, store.JobBlocked} {
		t.Run(string(from), func(t *testing.T) { resumeAfterAnAnnouncedStop(t, from) })
	}
}

func resumeAfterAnAnnouncedStop(t *testing.T, from store.JobState) {
	t.Helper()
	f, svc, p, job := resumeFixture(t, from)
	ctx := context.Background()
	conv := store.NewID(time.Now())
	if _, err := f.store.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: conv, OrgID: job.OrgID, SessionID: job.SessionID, Title: "c",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Conversations().PutMessages(ctx, []store.Message{{
		ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID,
		ConversationID: conv, Role: jobs.SyntheticRole, Text: stopMarker(), At: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResumeJob(ctx, p, job.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobDelivering {
		t.Errorf("state = %v, want delivering; a job whose loop already stopped was restarted above it", got.State)
	}
	// The suspension's note goes with the suspension here too, or a
	// delivered job carries "blocked on …" into its verdict.
	if got.Error != "" {
		t.Errorf("the delivered job carries %q", got.Error)
	}
}

// A resume clears the suspension's note from Error, including a
// jobs.GapNote, which clients would render as a failure.
//
// @decision(DL-054) @decision(DL-046)
func TestAResumedJobCarriesNoSuspensionNote(t *testing.T) {
	f, svc, p, job := resumeFixture(t, store.JobBlocked)
	ctx := context.Background()
	job.Error = jobs.GapNote
	if err := f.store.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResumeJob(ctx, p, job.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error != "" {
		t.Errorf("a resumed job carries %q, which clients render as a failure", got.Error)
	}
}

// Resuming a steered job whose sandbox has gone is a 409 with the
// session-stopped problem type, not a connection failure, so a caller
// does not retry it; the backstop concludes such a job.
//
// @decision(DL-054) @decision(DL-045)
func TestResumingASteeredJobWithNoSandboxSaysSo(t *testing.T) {
	f, svc, p, job := resumeFixture(t, store.JobSteered)
	err := svc.ResumeJob(context.Background(), p, job.ID)
	if !errors.Is(err, ErrSessionNotRunning) {
		t.Fatalf("resume = %v, want ErrSessionNotRunning", err)
	}
	// And the row is untouched, so nothing has half-happened.
	got, gerr := f.store.Jobs().Get(context.Background(), auth.LocalID, job.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.State != store.JobSteered {
		t.Errorf("state = %v, want steered; a failed resume moved the row anyway", got.State)
	}
}

// The row half of a resume is idempotent, so a caller can retry after
// a database error that followed the plugin write. Driven at
// moveJobToRunning because ResumeJob's own state check answers first.
//
// @decision(DL-054)
func TestTheRowHalfOfAResumeIsIdempotent(t *testing.T) {
	f, svc, p, job := resumeFixture(t, store.JobBlocked)
	ctx := context.Background()
	for i := range 3 {
		if err := svc.moveJobToRunning(ctx, p, job); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	got, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobRunning {
		t.Errorf("state = %v, want running", got.State)
	}
}

// A cancel that lands mid-resume wins. The row is re-read inside the
// transaction, as every other job write is, so a resume drawn before
// the cancel cannot resurrect it.
//
// @decision(DL-054) @decision(DL-045)
func TestACancelDuringAResumeWins(t *testing.T) {
	f, svc, p, job := resumeFixture(t, store.JobBlocked)
	ctx := context.Background()
	// Decided between the read ResumeJob made and the write.
	current, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.State = store.JobCancelled
	if err := f.store.Jobs().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := svc.moveJobToRunning(ctx, p, job); !errors.Is(err, ErrJobNotResumable) {
		t.Fatalf("resume = %v, want ErrJobNotResumable", err)
	}
	got, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobCancelled {
		t.Errorf("state = %v; a resume resurrected a cancelled job", got.State)
	}
}

// The loop command reaches the sandbox as the caller, or as the owner
// for a zero principal, and an unreachable sandbox is reported. The
// fake sandbox has no recoverable password, so the call stops at Connect.
//
// @decision(DL-054) @decision(DL-030)
func TestTheLoopCommandReachesTheSandboxAsTheCaller(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()
	svc := f.srv.svc
	p := auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID}
	job, err := svc.CreateJob(ctx, p, jobRequest())
	if err != nil {
		t.Fatal(err)
	}
	h, err := f.provider.Create(ctx, sandbox.Spec{
		Name: "job-loop", Image: "img", Command: []string{"x"},
		CPU: 0.5, MemoryMiB: 2048, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags: sandbox.JobTags(job.ID), Egress: sandbox.EgressOpen(),
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := h.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := f.store.Sessions().Get(ctx, auth.LocalID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State, sess.SandboxID = store.StateRunning, info.ID
	if err := f.store.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// The caller's principal, and the supervisor's zero one, both get
	// past the session guards and fail at the same place.
	for _, caller := range []auth.Principal{p, {}} {
		for _, paused := range []bool{true, false} {
			err := svc.SetJobLoopPaused(ctx, caller, job, paused)
			if err == nil {
				t.Errorf("paused=%v: reported success against a sandbox it could not authenticate to", paused)
			}
			if errors.Is(err, ErrSessionNotRunning) {
				t.Errorf("paused=%v: stopped at the session guard; the session IS running", paused)
			}
		}
	}
}

// A job decided while a resume-into-delivery was in flight keeps the
// decision, and the caller is TOLD rather than getting a 204 for
// something that did not happen.
//
// @decision(DL-054) @decision(DL-045)
func TestAResumeIntoDeliveryOfADecidedJobReportsIt(t *testing.T) {
	f, svc, _, job := resumeFixture(t, store.JobBlocked)
	ctx := context.Background()
	current, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.State = store.JobCancelled
	if err := f.store.Jobs().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := svc.resumeIntoDelivery(ctx, job); !errors.Is(err, ErrJobNotResumable) {
		t.Errorf("resumeIntoDelivery of a cancelled job = %v, want ErrJobNotResumable", err)
	}
	got, err := f.store.Jobs().Get(ctx, auth.LocalID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.JobCancelled {
		t.Errorf("state = %v; the delivery overrode a decision", got.State)
	}
}

// A job's egress is the caller's choice and defaults to open. Asserted on
// the created session's request, where a drift from the preset table
// would show.
//
// @decision(DL-031)
func TestAJobsEgressIsTheCallersChoice(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		ask  string
		want string
	}{
		{"unset defaults to open", "", sandbox.PresetOpen},
		{"agent is selectable", sandbox.PresetAgent, sandbox.PresetAgent},
		{"open is selectable", sandbox.PresetOpen, sandbox.PresetOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := jobFixture(t)
			p := auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID}
			req := jobRequest()
			req.Egress = tc.ask
			job, err := f.srv.svc.CreateJob(ctx, p, req)
			if err != nil {
				t.Fatalf("creating a job with egress %q: %v", tc.ask, err)
			}
			sess, err := f.store.Sessions().Get(ctx, auth.LocalID, job.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			// Asserted on the SESSION REQUEST rather than on the preset
			// table, because a preset can be defined and correct and
			// reached by nothing. A test of the presets themselves
			// would pass regardless.
			if got := sess.Request.Egress; got != tc.want {
				t.Errorf("a job asking for %q got egress %q, want %q", tc.ask, got, tc.want)
			}
		})
	}
}

// A job with no verdict projects no gates. The parser is tested in
// internal/jobs; this pins that jobViewOf calls it.
//
// @decision(DL-057) @decision(DL-050) @decision(DL-046)
func TestAJobWithNoVerdictProjectsNoGates(t *testing.T) {
	if got := jobViewOf(store.Job{ID: "j1"}, nil).Gates; len(got) != 0 {
		t.Errorf("a job with no verdict produced %d gates: %+v", len(got), got)
	}
}

// stopMarker is the plugin's announcement as it reaches the store, built
// from opencode.LoopStoppedMarker rather than a literal.
//
// @decision(DL-045) @decision(DL-046)
func stopMarker() string {
	return opencode.LoopStoppedMarker + ` {"reason":"limit","continuations":40,"turns":41}`
}

// Every job view carries the inventory and never a report's body, which
// only the file endpoint serves, with the type it was stored as and
// headers that keep it from running in this origin.
//
// @decision(DL-051) @decision(DL-060) @decision(DL-063)
func TestJobViewsCarryTheInventoryAndNoBody(t *testing.T) {
	f, _ := jobFixture(t)
	ctx := context.Background()
	const body = "# Findings\n\nThe whole report, which no listing carries."
	req := jobRequest()
	req.Kind = store.JobKindReport
	resp, raw := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}
	var view JobView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view.Deliverables == nil || len(view.Deliverables) != 0 {
		t.Errorf("a new job's deliverables = %#v, want an empty list", view.Deliverables)
	}
	for _, file := range []store.JobFile{
		{JobID: view.ID, Path: "findings.md", ContentType: "text/markdown; charset=utf-8", Size: int64(len(body)), Body: []byte(body)},
		{JobID: view.ID, Path: "images/c.png", ContentType: "image/png", Size: 4, Body: []byte("\x89PNG")},
	} {
		if _, err := f.store.Jobs().PutJobFile(ctx, auth.LocalID, file); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.Jobs().PutDeliverable(ctx, auth.LocalID, store.Deliverable{
		JobID: view.ID, Key: store.ReportKey("findings.md"), Kind: store.DeliverableReport,
		Status: store.DeliverableVerified, Path: "findings.md", Title: "Findings", Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/v1/jobs", "/api/v1/jobs?session=" + view.SessionID, "/api/v1/jobs/" + view.ID} {
		resp, raw := f.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, raw)
		}
		if strings.Contains(string(raw), "which no listing carries") {
			t.Errorf("%s carries a report's body", path)
		}
		if !strings.Contains(string(raw), `"path":"findings.md"`) || !strings.Contains(string(raw), `"title":"Findings"`) {
			t.Errorf("%s does not carry the inventory: %s", path, raw)
		}
	}

	get := func(query string) (result, []byte) {
		t.Helper()
		return f.do(http.MethodGet, "/api/v1/jobs/"+view.ID+"/file?"+query, nil)
	}
	resp, raw = get("path=findings.md")
	if resp.StatusCode != http.StatusOK || string(raw) != body {
		t.Fatalf("the report: %d %q", resp.StatusCode, raw)
	}
	for header, want := range map[string]string{
		"Content-Type":            "text/markdown; charset=utf-8",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox; default-src 'none'",
		"Cache-Control":           "private, max-age=31536000, immutable",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if d := resp.Header.Get("Content-Disposition"); d != "" {
		t.Errorf("a read for viewing is an attachment: %q", d)
	}
	resp, _ = get("path=images%2Fc.png&download=true")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("the image: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if d := resp.Header.Get("Content-Disposition"); !strings.Contains(d, "attachment") || !strings.Contains(d, view.ID+"-images-c.png") {
		t.Errorf("a download's disposition = %q", d)
	}
	// The report links the collected image, so its download is a zip of
	// both, at their paths.
	report, err := f.store.Jobs().JobFile(ctx, auth.LocalID, view.ID, "findings.md")
	if err != nil {
		t.Fatal(err)
	}
	report.Path, report.Body = "linked.md", []byte("# L\n![c](images/c.png)\n![gone](images/gone.png)")
	if _, err := f.store.Jobs().PutJobFile(ctx, auth.LocalID, report); err != nil {
		t.Fatal(err)
	}
	resp, raw = get("path=linked.md&download=true")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/zip" ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "-linked.zip") {
		t.Fatalf("the bundle: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"))
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, zf := range zr.File {
		names = append(names, zf.Name)
	}
	if strings.Join(names, ",") != "linked.md,images/c.png" {
		t.Errorf("the bundle holds %v", names)
	}
	// A report without images downloads as itself.
	if resp, _ := get("path=findings.md&download=true"); resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" {
		t.Errorf("a report without images downloaded as %s", resp.Header.Get("Content-Type"))
	}
	for _, q := range []string{"path=missing.md", "path=..%2Fsecret"} {
		if resp, _ := get(q); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", q, resp.StatusCode)
		}
	}
	if resp, _ := f.do(http.MethodGet, "/api/v1/jobs/nope/file?path=findings.md", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another job's file: %d, want 404", resp.StatusCode)
	}
}

// A single session's view names its job, as the listing does, so
// `spov attach` can tell a job still queued from one it can join.
//
// @decision(DL-025) @decision(DL-060)
func TestTheSessionViewNamesItsJob(t *testing.T) {
	f, _ := jobFixture(t)
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var job JobView
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	resp, body = f.do(http.MethodGet, "/api/v1/sessions/"+job.SessionID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d %s", resp.StatusCode, body)
	}
	var view SessionView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Job == nil || view.Job.ID != job.ID || view.Job.State != store.JobQueued {
		t.Errorf("the session's job = %+v, want %s queued", view.Job, job.ID)
	}

	sess := f.create(repoReq("beta"))
	resp, body = f.do(http.MethodGet, "/api/v1/sessions/"+sess, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d %s", resp.StatusCode, body)
	}
	view = SessionView{}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Job != nil {
		t.Errorf("a session with no job names %+v", view.Job)
	}
}

// The session list's job badge says a success was partial, read from the
// verdict as JobView.Partial is, and only on a succeeded job.
//
// @decision(DL-060) @decision(DL-050)
func TestTheSessionBadgeSaysASuccessWasPartial(t *testing.T) {
	partial := "the deliverables are partly there\n- check: passed (ok)\n- deliverable: partial (1 of 2 reports verified)\n- conduct: passed (ok)"
	full := "the deliverables are there\n- check: passed (ok)\n- deliverable: passed (2 of 2 reports verified)\n- conduct: passed (ok)"
	for _, tc := range []struct {
		name    string
		state   store.JobState
		verdict string
		want    bool
	}{
		{"a partial success", store.JobSucceeded, partial, true},
		{"a full success", store.JobSucceeded, full, false},
		{"a failure is never partial", store.JobFailed, partial, false},
		{"no verdict yet", store.JobRunning, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := sessionJobView(store.Job{ID: "j", Kind: store.JobKindReport, State: tc.state, Verdict: tc.verdict}, store.Session{})
			if v.Partial != tc.want {
				t.Errorf("Partial = %v, want %v", v.Partial, tc.want)
			}
		})
	}
}
