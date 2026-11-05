package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// seedJob records a job of the given state on a seeded session, which is
// what `jobs create` leaves behind for attach to find.
func (f *apiFixture) seedJob(name string, state store.JobState) store.Job {
	f.t.Helper()
	sess := f.session(name)
	job := store.Job{
		ID: store.NewID(time.Now()), OrgID: sess.OrgID, OwnerID: sess.OwnerID, SessionID: sess.ID,
		Kind: store.JobKindReport, State: state, Goal: "write a report",
	}
	if err := f.deps.Store.Jobs().Create(context.Background(), job); err != nil {
		f.t.Fatal(err)
	}
	return job
}

// moveJob advances a seeded job through the states given, in order.
func (f *apiFixture) moveJob(job *store.Job, states ...store.JobState) {
	for _, st := range states {
		job.State = st
		if err := f.deps.Store.Jobs().Update(context.Background(), *job); err != nil {
			f.t.Errorf("moving the job to %s: %v", st, err)
			return
		}
	}
}

// hookedClient is the fixture's client with hook called before each
// request it sends, counting reads of the session and of its
// conversations, so a test can move the job between attach's polls.
func (f *apiFixture) hookedClient(hook func(r *http.Request, sessionReads, conversationReads int)) *client.Client {
	c := *f.env.Client
	var mu sync.Mutex
	sessions, convs := 0, 0
	c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		switch {
		case r.Method != http.MethodGet:
		case strings.HasSuffix(r.URL.Path, "/messages"):
			convs++
		case strings.HasPrefix(r.URL.Path, "/api/v1/sessions/"):
			sessions++
		}
		hook(r, sessions, convs)
		mu.Unlock()
		return http.DefaultTransport.RoundTrip(r)
	})}
	return &c
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The command `jobs create` prints works at once: attach waits while the
// job is queued and provisioned, then joins the job's own conversation
// once it is recorded.
//
// @decision(DL-025)
func TestAttachWaitsForAQueuedJob(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("job-1", time.Minute)
	job := f.seedJob("job-1", store.JobQueued)
	env := f.env
	env.AttachPoll = time.Millisecond
	env.Client = f.hookedClient(func(r *http.Request, sessions, convs int) {
		if r.Method != http.MethodGet || strings.HasSuffix(r.URL.Path, "/messages") {
			return
		}
		switch sessions {
		case 2:
			f.moveJob(&job, store.JobProvisioning, store.JobRunning)
		case 3:
			if _, err := f.deps.Store.Conversations().UpsertConversation(context.Background(), store.Conversation{
				ID: store.NewID(time.Now()), OrgID: job.OrgID, SessionID: job.SessionID, OpenCodeID: "ses_theJobsOwn",
			}); err != nil {
				t.Error(err)
			}
		}
	})
	// The timeout only stops a broken test from hanging.
	_, stderr, err := runWith(t, env, "attach", "job-1", "--timeout", "10s")
	if err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if f.tuiSeen.Session != "ses_theJobsOwn" {
		t.Errorf("the TUI was given session %q, want the job's conversation", f.tuiSeen.Session)
	}
	want := "Waiting for job-1 (Ctrl-C stops waiting; it keeps going).\n" +
		"  the job is queued\n" +
		"  waiting for the job's conversation to be recorded\n"
	if !strings.HasPrefix(stderr, want) || !strings.Contains(stderr, "Attached to job-1") {
		t.Errorf("stderr:\n%s\nwant it to start:\n%s", stderr, want)
	}
}

// A running session is joined at once, with no waiting line.
//
// @decision(DL-025)
func TestAttachToAJoinableJobPrintsNoWait(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("job-1", time.Minute)
	job := f.seedJob("job-1", store.JobQueued)
	f.moveJob(&job, store.JobProvisioning, store.JobRunning)
	f.seedConversations("job-1", "ses_theJobsOwn")

	_, stderr, err := runWith(t, f.env, "attach", "job-1")
	if err != nil {
		t.Fatalf("attach: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "Waiting") {
		t.Errorf("a joinable session printed a wait:\n%s", stderr)
	}
}

// Ctrl-C ends the wait and nothing else: the job and its sandbox carry
// on, and the message says how to come back or cancel.
//
// @decision(DL-025)
func TestInterruptingTheWaitLeavesTheJobAlone(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("job-1", time.Minute)
	job := f.seedJob("job-1", store.JobQueued)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := f.env
	env.AttachPoll = time.Millisecond
	env.Err = io.Discard
	// Ctrl-C arrives during the first poll after the wait began.
	env.Client = f.hookedClient(func(r *http.Request, sessions, _ int) {
		if sessions == 2 {
			cancel()
		}
	})

	err := runAttach(ctx, &env, attachOptions{Ref: "job-1"})
	if err == nil {
		t.Fatal("an interrupted wait attached")
	}
	for _, want := range []string{"stopped waiting for job-1 (the job is queued)", "nothing was stopped", "spov attach job-1", "spov jobs cancel " + job.ID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q: %v", want, err)
		}
	}
	got, gerr := f.deps.Store.Jobs().Get(context.Background(), job.OrgID, job.ID)
	if gerr != nil || got.State != store.JobQueued {
		t.Errorf("the job is %s (%v), want still queued", got.State, gerr)
	}
	if f.provider.Running() != 1 || f.proxyStarts != 0 {
		t.Errorf("the wait touched the sandbox: %d running, %d proxies", f.provider.Running(), f.proxyStarts)
	}
}

// --timeout bounds the wait and --no-wait refuses at once; both leave
// the job running.
//
// @decision(DL-025)
func TestTheWaitCanBeBoundedOrSkipped(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("job-1", time.Minute)
	f.seedJob("job-1", store.JobQueued)
	f.env.AttachPoll = 5 * time.Millisecond

	_, _, err := runWith(t, f.env, "attach", "job-1", "--timeout", "30ms")
	if err == nil || !strings.Contains(err.Error(), "gave up after 30ms") || !strings.Contains(err.Error(), "the job is queued") {
		t.Errorf("--timeout: %v", err)
	}
	_, stderr, err := runWith(t, f.env, "attach", "job-1", "--no-wait")
	if err == nil || !strings.Contains(err.Error(), "cannot be attached to yet: the job is queued") {
		t.Errorf("--no-wait: %v", err)
	}
	if strings.Contains(stderr, "Waiting") {
		t.Errorf("--no-wait waited:\n%s", stderr)
	}
	if f.proxyStarts != 0 {
		t.Error("a proxy was started for a session that could not be joined")
	}
}

// A job that has ended is not an attach to retry: the message points at
// the job's outcome.
//
// @decision(DL-025)
func TestAttachToAFinishedJobPointsAtItsOutcome(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("job-1", time.Minute)
	job := f.seedJob("job-1", store.JobQueued)
	f.moveJob(&job, store.JobCancelled)

	_, _, err := runWith(t, f.env, "attach", "job-1")
	if err == nil || !strings.Contains(err.Error(), "has cancelled") || !strings.Contains(err.Error(), "spov jobs show "+job.ID) {
		t.Errorf("err = %v", err)
	}
	if f.proxyStarts != 0 {
		t.Error("a proxy was started for a finished job")
	}
}

// joinable decides every combination of session and job state.
//
// @decision(DL-025)
func TestJoinable(t *testing.T) {
	job := func(st store.JobState) *server.SessionJobView {
		return &server.SessionJobView{ID: "job_1", State: st}
	}
	cases := []struct {
		name    string
		view    server.SessionView
		wait    string
		errWant string
	}{
		{"running", server.SessionView{State: store.StateRunning}, "", ""},
		{"starting", server.SessionView{State: store.StateStarting}, "starting the sandbox", ""},
		{"stopped", server.SessionView{State: store.StateStopped}, "", "spov fly"},
		{"queued job", server.SessionView{State: store.StateStarting, Job: job(store.JobQueued)}, "the job is queued", ""},
		{"provisioning job", server.SessionView{State: store.StateStarting, Job: job(store.JobProvisioning)}, "starting the job's sandbox", ""},
		{"running job, starting session", server.SessionView{State: store.StateStarting, Job: job(store.JobRunning)}, "starting the job's sandbox", ""},
		{"running job", server.SessionView{State: store.StateRunning, Job: job(store.JobRunning)}, "", ""},
		{"blocked job", server.SessionView{State: store.StateRunning, Job: job(store.JobBlocked)}, "", ""},
		{"steered job", server.SessionView{State: store.StateRunning, Job: job(store.JobSteered)}, "", ""},
		{"delivering job", server.SessionView{State: store.StateRunning, Job: job(store.JobDelivering)}, "the job is finishing", ""},
		{"finished job", server.SessionView{State: store.StateStopped, Job: job(store.JobSucceeded)}, "", "spov jobs show job_1"},
		{"failed session", server.SessionView{State: store.StateFailed, Error: "clone failed", Job: job(store.JobProvisioning)}, "", "failed: clone failed"},
		{"unknown job state", server.SessionView{State: store.StateRunning, Job: job("pondering")}, "the job is pondering", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wait, err := joinable(tc.view)
			if wait != tc.wait {
				t.Errorf("wait = %q, want %q", wait, tc.wait)
			}
			switch {
			case tc.errWant == "" && err != nil:
				t.Errorf("err = %v", err)
			case tc.errWant != "" && (err == nil || !strings.Contains(err.Error(), tc.errWant)):
				t.Errorf("err = %v, want one naming %q", err, tc.errWant)
			}
		})
	}
}

// The wait reads only the server, and each change of what it waits on
// prints one line, in order.
//
// @decision(DL-025) @decision(DL-018)
func TestTheWaitReportsEachPhaseOnce(t *testing.T) {
	views := []string{
		`{"id":"s1","name":"job-1","state":"starting","job":{"id":"job_1","kind":"report","state":"queued"}}`,
		`{"id":"s1","name":"job-1","state":"starting","job":{"id":"job_1","kind":"report","state":"queued"}}`,
		`{"id":"s1","name":"job-1","state":"starting","job":{"id":"job_1","kind":"report","state":"provisioning"}}`,
		`{"id":"s1","name":"job-1","state":"running","job":{"id":"job_1","kind":"report","state":"running"}}`,
	}
	var mu sync.Mutex
	gets, convReads := 0, 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/sessions/s1":
			_, _ = io.WriteString(w, views[min(gets, len(views)-1)])
			gets++
		case "/api/v1/sessions/s1/messages":
			convReads++
			if convReads == 1 {
				_, _ = io.WriteString(w, `{"conversations":[]}`)
				return
			}
			_, _ = io.WriteString(w, `{"conversations":[{"id":"ses_1","messages":[]}]}`)
		default:
			t.Errorf("the wait asked for %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	c := client.New(api.URL, "tok")
	var out strings.Builder
	env := Env{Err: &out, AttachPoll: time.Millisecond}

	first, err := c.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	got, conv, err := awaitJoinable(context.Background(), &env, c, first, attachOptions{Ref: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	if conv != "ses_1" {
		t.Errorf("returned conversation %q, want the recorded one", conv)
	}
	if got.Job.State != store.JobRunning {
		t.Errorf("returned a view of a %s job", got.Job.State)
	}
	want := "Waiting for job-1 (Ctrl-C stops waiting; it keeps going).\n" +
		"  the job is queued\n" +
		"  starting the job's sandbox\n" +
		"  waiting for the job's conversation to be recorded\n"
	if out.String() != want {
		t.Errorf("printed:\n%s\nwant:\n%s", out.String(), want)
	}
}

// A failed read ends the wait with the same hints as giving up: the job
// keeps going.
//
// @decision(DL-025)
func TestAFailedReadEndsTheWaitWithTheWayBack(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("job-1", time.Minute)
	job := f.seedJob("job-1", store.JobQueued)
	env := f.env
	env.AttachPoll = time.Millisecond
	c := *f.env.Client
	gets := 0
	c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if gets++; gets > 1 {
			return nil, errors.New("connection reset")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	env.Client = &c

	_, _, err := runWith(t, env, "attach", "job-1")
	for _, want := range []string{"reading job-1 failed", "connection reset", "nothing was stopped", "spov jobs cancel " + job.ID} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q: %v", want, err)
		}
	}
	if _, _, err := runWith(t, f.env, "attach", "job-1", "--timeout", "-1s"); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("a negative --timeout: %v", err)
	}
}

// Stopping a job's session names the job it cancelled, which the
// single session's view carries.
//
// @decision(DL-025) @decision(DL-023)
func TestStoppingAJobsSessionNamesTheJob(t *testing.T) {
	f := newAPIFixture(t)
	f.seedSession("job-1", time.Minute)
	job := f.seedJob("job-1", store.JobQueued)
	out, _, err := runWith(t, f.env, "stop", "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cancelled its job "+job.ID) {
		t.Errorf("stop said %q", out)
	}
}
