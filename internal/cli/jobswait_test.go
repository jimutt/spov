package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// jobScript serves GET /api/v1/jobs/job_1 from a list of states, one per
// read and the last repeating, and records every request that is not
// that read, so a test can say nothing else happened to the job.
type jobScript struct {
	mu      sync.Mutex
	states  []string
	partial bool
	reads   int
	others  []string
}

func (s *jobScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet || r.URL.Path != "/api/v1/jobs/job_1" {
		s.others = append(s.others, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"title":"Not Found","detail":"no such job"}`)
		return
	}
	i := min(s.reads, len(s.states)-1)
	s.reads++
	_, _ = fmt.Fprintf(w, `{"id":"job_1","session_id":"sess_1","kind":"pr","state":%q,"goal":"fix it","partial":%t}`,
		s.states[i], s.partial && s.states[i] == "succeeded")
}

func (s *jobScript) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func waitEnv(t *testing.T, s *jobScript) Env {
	t.Helper()
	api := httptest.NewServer(s)
	t.Cleanup(api.Close)
	return Env{Client: client.New(api.URL, "tok"), JobPoll: time.Millisecond}
}

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the error is %v, not an ExitError", err)
	}
	return exit.Code
}

// `jobs wait` ends with a code for how the job ended, prints the job
// first, and goes on through blocked and steered. A wait that spov itself
// could not finish (an unknown job) is a plain error, which exits 1.
//
// @decision(DL-103)
func TestJobsWaitExitsByOutcome(t *testing.T) {
	cases := []struct {
		name    string
		states  []string
		partial bool
		args    []string
		code    int // 0 is a nil error
	}{
		{name: "succeeded", states: []string{"running", "delivering", "succeeded"}, code: 0},
		{name: "succeeded partly", states: []string{"running", "succeeded"}, partial: true, code: 3},
		{name: "failed", states: []string{"running", "failed"}, code: 4},
		{name: "cancelled", states: []string{"queued", "cancelled"}, code: 5},
		{name: "blocked and steered are not the end", states: []string{
			"running", "blocked", "steered", "running", "succeeded"}, code: 0},
		{name: "timed out", states: []string{"running"}, args: []string{"--timeout", "30ms"}, code: 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &jobScript{states: c.states, partial: c.partial}
			env := waitEnv(t, s)
			out, _, err := runWith(t, env, append([]string{"jobs", "wait", "job_1"}, c.args...)...)
			if c.code == 0 {
				if err != nil {
					t.Fatalf("wait: %v", err)
				}
			} else if got := exitCodeOf(t, err); got != c.code {
				t.Fatalf("exit code %d, want %d (%v)", got, c.code, err)
			}
			// The job is printed whatever the code, a timed-out wait
			// printing the last view it read.
			if !strings.Contains(out, "job_1") || !strings.Contains(out, "fix it") {
				t.Errorf("the job was not printed:\n%s", out)
			}
			if len(s.others) != 0 {
				t.Errorf("a wait made other calls: %v", s.others)
			}
		})
	}

	t.Run("an unknown job", func(t *testing.T) {
		env := waitEnv(t, &jobScript{states: []string{"running"}})
		_, _, err := runWith(t, env, "jobs", "wait", "job_nope")
		var exit *ExitError
		if err == nil || errors.As(err, &exit) {
			t.Fatalf("an unknown job: %v, want a plain error (exit 1)", err)
		}
	})

	t.Run("a bad timeout", func(t *testing.T) {
		env := waitEnv(t, &jobScript{states: []string{"succeeded"}})
		_, _, err := runWith(t, env, "jobs", "wait", "job_1", "--timeout", "-1s")
		var exit *ExitError
		if err == nil || errors.As(err, &exit) {
			t.Fatalf("a negative timeout: %v, want a plain error (exit 1)", err)
		}
	})
}

// `--json` prints the final view and nothing else on stdout; progress
// lines go to stderr and only when stderr is a terminal.
//
// @decision(DL-103)
func TestJobsWaitJSONAndProgress(t *testing.T) {
	s := &jobScript{states: []string{"queued", "running", "running", "succeeded"}}
	env := waitEnv(t, s)
	env.StderrIsTerminal = func() bool { return true }
	out, stderr, err := runWith(t, env, "jobs", "wait", "job_1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var final server.JobView
	decodeOnly(t, out, &final)
	if final.State != store.JobSucceeded {
		t.Errorf("--json printed %+v", final)
	}
	for _, want := range []string{"job job_1: queued", "job job_1: running", "job job_1: succeeded"} {
		if strings.Count(stderr, want) != 1 {
			t.Errorf("stderr should have %q once (a line per change):\n%s", want, stderr)
		}
	}

	s = &jobScript{states: []string{"queued", "succeeded"}}
	env = waitEnv(t, s)
	_, stderr, err = runWith(t, env, "jobs", "wait", "job_1")
	if err != nil || stderr != "" {
		t.Errorf("off a terminal: err %v, stderr %q; want no progress lines", err, stderr)
	}
}

// Ctrl-C ends the wait with 130 and sends the job nothing: the only
// requests are reads of it.
//
// @decision(DL-103) @decision(DL-025)
func TestInterruptingAJobsWaitLeavesTheJobAlone(t *testing.T) {
	s := &jobScript{states: []string{"running"}}
	env := waitEnv(t, s)
	env.Out, env.Err = io.Discard, io.Discard
	env.Workdir, env.StateDir = t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for s.readCount() < 3 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	root := NewRoot(env)
	root.SetArgs([]string{"jobs", "wait", "job_1"})
	err := root.ExecuteContext(ctx)
	if got := exitCodeOf(t, err); got != 130 {
		t.Fatalf("exit code %d, want 130 (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "it keeps going") {
		t.Errorf("the message does not say the job goes on: %v", err)
	}
	if len(s.others) != 0 {
		t.Errorf("an interrupted wait made other calls: %v", s.others)
	}
}

// `jobs create --wait` creates, prints the job at once, then waits. With
// --json that is two documents, one per line; a create that fails exits
// 1 with no wait.
//
// @decision(DL-103)
func TestJobsCreateWaitPrintsTheJobFirst(t *testing.T) {
	f := newAPIFixture(t)
	f.deps.Jobs = &jobs.Supervisor{
		Store: f.deps.Store, Provider: f.provider,
		Queue: func(context.Context, jobs.JobArgs) error { return nil },
		Warn:  func(string) {},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(f.deps, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	// The job is cancelled in the store when it is read a third time,
	// as a supervisor would end it.
	var reads int
	hs := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/jobs/") {
			if reads++; reads == 3 {
				id := strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/")
				j, err := f.deps.Store.Jobs().Get(r.Context(), auth.LocalID, id)
				if err != nil {
					t.Error(err)
				} else {
					j.State = store.JobCancelled
					if err := f.deps.Store.Jobs().Update(r.Context(), j); err != nil {
						t.Error(err)
					}
				}
			}
		}
		srv.Handler().ServeHTTP(w, r)
	}))
	hs.Listener = ln
	hs.Start()
	t.Cleanup(hs.Close)
	env := f.env
	env.Client = client.New(hs.URL, f.token)
	env.JobPoll = time.Millisecond

	out, _, err := runWith(t, env, "jobs", "create", "fix the failing test", "--repo", "alice/spov", "--wait", "--json")
	if got := exitCodeOf(t, err); got != 5 {
		t.Fatalf("exit code %d, want 5 for a cancelled job (%v)", got, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("--wait --json printed %d lines, want 2:\n%s", len(lines), out)
	}
	var created, final server.JobView
	decodeOnly(t, lines[0], &created)
	decodeOnly(t, lines[1], &final)
	if created.State != store.JobQueued || final.ID != created.ID || final.State != store.JobCancelled {
		t.Errorf("created %+v, final %+v", created, final)
	}

	// Without --json: the queued line, then the job as `show` prints it.
	reads = 0
	out, _, err = runWith(t, env, "jobs", "create", "second goal", "--repo", "alice/spov", "--wait")
	if got := exitCodeOf(t, err); got != 5 {
		t.Fatalf("exit code %d, want 5 (%v)", got, err)
	}
	if !strings.HasPrefix(out, "job ") || !strings.Contains(out, "queued") || !strings.Contains(out, "cancelled") {
		t.Errorf("--wait printed:\n%s", out)
	}

	// A create that fails exits 1 and never waits.
	reads = 0
	_, _, err = runWith(t, env, "jobs", "create", "", "--wait")
	var exit *ExitError
	if err == nil || errors.As(err, &exit) || reads != 0 {
		t.Errorf("a failed create: %v after %d reads", err, reads)
	}

	// --timeout means nothing without --wait.
	_, _, err = runWith(t, env, "jobs", "create", "third", "--timeout", "1m")
	if err == nil || !strings.Contains(err.Error(), "--wait") {
		t.Errorf("--timeout without --wait: %v", err)
	}
}

// The outcome codes and the message `cmd/spov` prints are one table.
//
// @decision(DL-103)
func TestOutcomeCodes(t *testing.T) {
	for _, c := range []struct {
		v    server.JobView
		code int
	}{
		{server.JobView{State: store.JobSucceeded}, 0},
		{server.JobView{State: store.JobSucceeded, Partial: true}, 3},
		{server.JobView{State: store.JobFailed}, 4},
		{server.JobView{State: store.JobCancelled}, 5},
	} {
		err := outcomeCode(c.v)
		if c.code == 0 {
			if err != nil {
				t.Errorf("%+v: %v", c.v, err)
			}
			continue
		}
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != c.code || exit.Err != nil {
			t.Errorf("%+v: %v, want a silent exit %d", c.v, err, c.code)
		}
	}
	if (&ExitError{Code: 6}).Error() == "" {
		t.Error("an ExitError with no cause has no text")
	}
}

// A read that fails in transit or with a server error is retried five
// times in a row with a growing delay, and a successful read resets the
// count; a sixth failure in a row ends the wait with exit 1.
//
// @decision(DL-103)
func TestJobsWaitRetriesAFailedRead(t *testing.T) {
	serve := func(t *testing.T, failFirst int) (Env, func() int) {
		var mu sync.Mutex
		reads := 0
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			reads++
			if reads <= failFirst {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"status":503,"title":"Service Unavailable"}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"job_1","session_id":"sess_1","kind":"pr","state":"succeeded","goal":"fix it"}`)
		}))
		t.Cleanup(api.Close)
		count := func() int { mu.Lock(); defer mu.Unlock(); return reads }
		return Env{Client: client.New(api.URL, "tok"), JobPoll: time.Millisecond}, count
	}

	env, reads := serve(t, waitRetries)
	_, stderr, err := runWith(t, env, "jobs", "wait", "job_1")
	if err != nil {
		t.Fatalf("five failed reads then a finished job: %v\n%s", err, stderr)
	}
	if got := strings.Count(stderr, "retrying in"); got != waitRetries || reads() != waitRetries+1 {
		t.Errorf("%d retry lines and %d reads, want %d and %d:\n%s", got, reads(), waitRetries, waitRetries+1, stderr)
	}
	if !strings.Contains(stderr, "retrying in 1ms (1 of 5)") || !strings.Contains(stderr, "retrying in 16ms (5 of 5)") {
		t.Errorf("the delay does not double from the poll interval:\n%s", stderr)
	}

	env, reads = serve(t, waitRetries+1)
	_, _, err = runWith(t, env, "jobs", "wait", "job_1")
	var exit *ExitError
	if err == nil || errors.As(err, &exit) || client.StatusOf(err) != http.StatusServiceUnavailable {
		t.Errorf("six failed reads in a row: %v, want the read's own error (exit 1)", err)
	}
	if reads() != waitRetries+1 {
		t.Errorf("%d reads, want %d", reads(), waitRetries+1)
	}
}

func TestTransientRead(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errors.New("connection refused"), true},
		{&client.Error{Status: http.StatusBadGateway}, true},
		{&client.Error{Status: http.StatusInternalServerError}, true},
		{&client.Error{Status: http.StatusNotImplemented}, false},
		{&client.Error{Status: http.StatusNotFound}, false},
		{&client.Error{Status: http.StatusUnauthorized}, false},
	} {
		if got := transientRead(c.err); got != c.want {
			t.Errorf("transientRead(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
