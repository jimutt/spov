//go:build integration

package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// TestScenario7 is scenario 7's control-plane layer (DL-022): one report
// job taken through every state a restart can meet it in (running,
// steered, blocked, past its wall clock, and a delivery cut off partway),
// each time on a new server process over the same database and
// sandboxes, then a second job's pages and the metrics read with no
// request reaching its sandbox. A restart with a real sandbox and a
// sandbox idling out while it is watched are not exercised here.
//
//	SPOV_TEST_DATABASE_URL=... go test -tags integration ./internal/cli/ -run TestScenario7 -v
//
// @decision(DL-022) @decision(DL-049) @decision(DL-011) @decision(DL-051) @decision(DL-018)
func TestScenario7(t *testing.T) {
	s := newScenarioServer(t)
	s.storeApp()
	oc := newJobOpenCode(t)
	s.withJobs(oc)
	step := s.steps(t)
	const allowance = time.Hour
	// A new process gets a new store, which must keep the scenario's
	// clock too.
	restart := func() {
		s.t.Helper()
		s.restart(nil)
		s.withJobs(oc)
	}

	created := s.createJob(map[string]any{"kind": "report", "goal": "Write two reports.", "max_duration": "1h"})
	id := created.ID
	j := s.passUntil(id, store.JobRunning)
	s.worked(j)
	conv := s.conversationOf(j)
	sandboxID := s.row(j.SessionID).SandboxID
	used := func() time.Duration { return s.jobRow(id).ClockElapsed(s.clock.Now()) }
	alive := func(t *testing.T) {
		t.Helper()
		if h := s.provider.Handles()[sandboxID]; h == nil || h.WasTerminated() {
			t.Fatalf("the job's sandbox %s did not survive the restart", sandboxID)
		}
		if st := s.row(j.SessionID).State; st != store.StateRunning {
			t.Errorf("the job's session is %s after the restart, want running", st)
		}
	}
	loops := func(arg string) int {
		n := 0
		for _, c := range oc.sent("/command") {
			if strings.Contains(c, arg) {
				n++
			}
		}
		return n
	}

	step("7.1 a running job runs on across a restart, its clock neither reset nor counted twice", func(t *testing.T) {
		s.clock.advance(10 * time.Minute)
		before := used()
		// The minutes the server is down are the sandbox's minutes too.
		s.clock.advance(2 * time.Minute)
		restart()
		alive(t)
		if got := s.jobRow(id); got.State != store.JobRunning {
			t.Fatalf("after the restart: state %s, want running", got.State)
		}
		if d := used() - before; d != 2*time.Minute {
			t.Errorf("two minutes across the restart counted %v", d)
		}
		if got := s.pass(id); got.State != store.JobRunning {
			t.Fatalf("the new process's pass: state %s, want running", got.State)
		}
		if d := used() - before; d != 2*time.Minute {
			t.Errorf("after the new process's pass the clock moved %v, want 2m0s", d)
		}
	})

	step("7.2 a steered job stays paused across a restart, and Resume on the new process continues its clock", func(t *testing.T) {
		s.frame(j, store.SourceOpenCode, `{"type":"session.inbox.enqueued","data":{"inboxID":"inb_take","item":{"type":"user",`+
			`"payload":{"text":"let me look","metadata":{"spov":{"after":"handover"}}},"delivery":"steer"}}}`)
		s.frame(j, store.SourceOpenCode, `{"type":"session.inbox.delivered","data":{"inboxID":"inb_take"}}`)
		if got := s.pass(id); got.State != store.JobSteered {
			t.Fatalf("state %s, want steered", got.State)
		}
		paused := used()
		s.clock.advance(15 * time.Minute)
		restart()
		alive(t)
		if got := s.jobRow(id); got.State != store.JobSteered {
			t.Fatalf("after the restart: state %s, want steered", got.State)
		}
		if got := s.pass(id); got.State != store.JobSteered {
			t.Fatalf("the new process's pass: state %s, want steered", got.State)
		}
		s.clock.advance(5 * time.Minute)
		if d := used() - paused; d != 0 {
			t.Errorf("twenty steered minutes across a restart counted %v", d)
		}
		resumes := loops(server.LoopResume)
		s.must(http.StatusNoContent, "member", http.MethodPost, "/api/v1/jobs/"+id+"/resume", nil, nil)
		if got := s.jobRow(id); got.State != store.JobRunning {
			t.Fatalf("after Resume: state %s, want running", got.State)
		}
		if d := used() - paused; d != 0 {
			t.Errorf("Resume added %v", d)
		}
		if n := loops(server.LoopResume) - resumes; n != 1 {
			t.Errorf("Resume on the new process released the loop %d times, want once", n)
		}
	})

	step("7.3 a blocked job stays blocked across a restart, counting, and an answer on the new process releases it", func(t *testing.T) {
		s.frame(j, store.SourceEphemeral, `{"type":"permission.asked","data":{"id":"per_restart","sessionID":"`+
			conv.OpenCodeID+`","action":"read","resources":["/etc/hosts"],"save":["/etc/*"]}}`)
		if got := s.pass(id); got.State != store.JobBlocked {
			t.Fatalf("state %s, want blocked", got.State)
		}
		before := used()
		s.clock.advance(5 * time.Minute)
		restart()
		alive(t)
		if got := s.pass(id); got.State != store.JobBlocked {
			t.Fatalf("after the restart: state %s, want blocked", got.State)
		}
		if d := used() - before; d != 5*time.Minute {
			t.Errorf("five blocked minutes across a restart counted %v", d)
		}
		resumes := loops(server.LoopResume)
		if code, raw := s.call("member", http.MethodPost, "/api/v1/sessions/"+j.SessionID+"/permissions/per_restart",
			map[string]string{"decision": "once"}); code != http.StatusAccepted {
			t.Fatalf("allow once: %d %s", code, raw)
		}
		if got := s.pass(id); got.State != store.JobRunning {
			t.Fatalf("answered: state %s, want running", got.State)
		}
		if n := loops(server.LoopResume) - resumes; n != 1 {
			t.Errorf("the answer released the loop %d times, want once", n)
		}
	})

	// The second process's scrape, taken before the delivery that is cut
	// off, and the job's spov markers as they stand then.
	var beforeDelivery string
	step("7.4 a job past its allowance while the server was down is bounded on the new process's first pass", func(t *testing.T) {
		oc.put(opencode.OutDir+"/a.md", []byte("# First\n"))
		oc.put(opencode.OutDir+"/b.md", []byte("# Second\n"))
		oc.evidence()
		s.clock.advance(allowance - used() + time.Minute)
		restart()
		alive(t)
		if got := s.pass(id); got.State != store.JobDelivering {
			t.Fatalf("past its allowance: state %s, want delivering", got.State)
		}
		if n := s.markers(j, jobs.MarkerBounded); n != 1 {
			t.Errorf("the wall clock was marked %d times, want once", n)
		}
		beforeDelivery = s.scrape()
		if n := metricSum(beforeDelivery, "spov_job_endings_total"); n != 0 {
			t.Errorf("before delivery this process counts %v endings", n)
		}
	})

	step("7.5 a delivery cut off partway is finished by the next process, each report read and stored once", func(t *testing.T) {
		arrived, release := oc.hold(opencode.OutDir + "/b.md")
		defer release()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.jobs.Run(ctx, jobs.JobArgs{JobID: id, OrgID: server.DefaultOrgID}, 1) }()
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the delivery never reached the second report")
		}
		// The process dies while it reads b.md: a.md is stored, and
		// nothing after it is written.
		cancel()
		if err := <-done; err == nil || !errors.Is(err, context.Canceled) {
			t.Logf("the cut-off pass returned %v", err)
		}
		if got := s.jobRow(id); got.State != store.JobDelivering {
			t.Fatalf("after the cut-off pass: state %s, want delivering", got.State)
		}
		if files := s.jobFiles(id); strings.Join(files, ",") != "a.md" {
			t.Fatalf("the cut-off pass stored %v, want a.md alone", files)
		}
		if n := s.markers(j, jobs.MarkerCollected); n != 0 {
			t.Fatalf("the cut-off pass marked its collection done")
		}
		interrupts := len(oc.sent("/interrupt"))
		release()

		restart()
		final := s.passUntilDone(id)
		if final.State != store.JobSucceeded {
			t.Fatalf("state %s (%q), want succeeded", final.State, final.Error)
		}
		if files := s.jobFiles(id); strings.Join(files, ",") != "a.md,b.md" {
			t.Errorf("stored files %v, want a.md,b.md once each", files)
		}
		if n := len(oc.sent("/api/fs/read/a.md")); n != 1 {
			t.Errorf("a.md was read %d times, want once: the new process must take it from the store", n)
		}
		if n := len(oc.sent("/interrupt")) - interrupts; n != 0 {
			t.Errorf("the new process stopped the run again (%d interrupts)", n)
		}
		for name, want := range map[string]int{jobs.MarkerEvidence: 1, jobs.MarkerCollected: 1, jobs.MarkerBounded: 1} {
			if n := s.markers(j, name); n != want {
				t.Errorf("%s written %d times, want %d", name, n, want)
			}
		}
		v := s.job(id)
		if g := gate(v, jobs.GateDeliverable); !strings.Contains(g.Detail, "2 of 2 reports verified") || !strings.Contains(g.Detail, "wall clock") {
			t.Errorf("gate 2 = %+v, want both reports verified, partial at the wall clock", g)
		}
		if len(v.Deliverables) != 2 || !v.Partial {
			t.Errorf("deliverables %+v, partial %v", v.Deliverables, v.Partial)
		}
		if final.ClockUsed < allowance || final.ClockUsed > allowance+2*time.Minute {
			t.Errorf("the job used %v of a %v allowance", final.ClockUsed, allowance)
		}
		if h := s.provider.Handles()[sandboxID]; h == nil || !h.WasTerminated() {
			t.Errorf("the delivered job's sandbox was not stopped")
		}
	})

	step("7.6 the ending is counted once, by the process that wrote it", func(t *testing.T) {
		m := s.scrape()
		if n := metricSum(m, "spov_job_endings_total", `outcome="succeeded"`, `ended_by="wall-clock"`, `kind="report"`); n != 1 {
			t.Errorf("this process counts %v endings, want 1:\n%s", n, grepLines(m, "spov_job_endings_total"))
		}
		if n := metricSum(m, "spov_job_gates_total"); n != 3 {
			t.Errorf("this process counts %v gate results, want 3", n)
		}
		if n := metricSum(m, "spov_job_deliverables_total", `status="verified"`); n != 2 {
			t.Errorf("this process counts %v verified deliverables, want 2", n)
		}
		// A redelivery of the finished job, on this process and the next,
		// counts nothing.
		s.pass(id)
		if n := metricSum(s.scrape(), "spov_job_endings_total"); n != 1 {
			t.Errorf("a redelivery raised the endings to %v", n)
		}
		restart()
		s.pass(id)
		if n := metricSum(s.scrape(), "spov_job_endings_total"); n != 0 {
			t.Errorf("a redelivery on a third process counted %v endings", n)
		}
	})

	step("7.7 after a restart, reading a job's pages and scraping the metrics never reaches its sandbox", func(t *testing.T) {
		oc.reset()
		live := s.createJob(map[string]any{"kind": "report", "goal": "Keep working.", "max_duration": "1h"})
		lj := s.passUntil(live.ID, store.JobRunning)
		s.worked(lj)
		restart()
		h := s.provider.Handles()[s.row(lj.SessionID).SandboxID]
		calls, execs := len(oc.sent("")), len(h.ExecLog())
		for range 3 {
			s.view(lj)
			s.scrape()
		}
		if n := len(oc.sent("")) - calls; n != 0 {
			t.Errorf("the reads sent %d requests to the sandbox: %v", n, oc.sent("")[calls:])
		}
		if n := len(h.ExecLog()) - execs; n != 0 {
			t.Errorf("the reads ran %d commands in the sandbox", n)
		}
	})

	step("7.8 nothing secret reached a response or the log", func(t *testing.T) {
		s.scan(t, s.appKeys, "")
	})
}

// markers counts the job's spov markers of one name.
func (s *scenarioServer) markers(j store.Job, name string) int {
	s.t.Helper()
	events, err := s.store.Conversations().ListEvents(context.Background(), j.OrgID, j.SessionID, 0, 10_000)
	if err != nil {
		s.t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Source == store.SourceSpov && e.Name == name {
			n++
		}
	}
	return n
}

// jobFiles is the paths of a job's collected files, in the store's order.
func (s *scenarioServer) jobFiles(id string) []string {
	s.t.Helper()
	files, err := s.store.Jobs().JobFiles(context.Background(), server.DefaultOrgID, id)
	if err != nil {
		s.t.Fatal(err)
	}
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// scrape reads this process's /metrics.
func (s *scenarioServer) scrape() string {
	s.t.Helper()
	resp, err := http.Get("http://" + s.metrics + "/metrics")
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("GET /metrics: %d", resp.StatusCode)
	}
	s.bodies = append(s.bodies, string(body))
	return string(body)
}

// view reads what the session page and the listings read for a job, as
// a browser would: the job, its session, its messages, the two live
// streams for a moment each, the listings, and the pages themselves.
func (s *scenarioServer) view(j store.Job) {
	s.t.Helper()
	for _, p := range []string{
		"/api/v1/jobs/" + j.ID, "/api/v1/jobs", "/api/v1/sessions", "/api/v1/me",
		"/api/v1/sessions/" + j.SessionID, "/api/v1/sessions/" + j.SessionID + "/messages",
	} {
		s.must(http.StatusOK, "member", http.MethodGet, p, nil, nil)
	}
	for _, p := range []string{"/api/v1/sessions/" + j.SessionID + "/log", "/api/v1/sessions/events", "/", "/jobs/", "/session/?id=" + j.SessionID} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.base+p, nil)
		req.Header.Set("Authorization", "Bearer "+s.users["member"])
		if resp, err := http.DefaultClient.Do(req); err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			s.bodies = append(s.bodies, string(body))
		}
		cancel()
	}
}

// metricSum adds up a metric's samples whose labels include every one
// of want.
func metricSum(text, name string, want ...string) float64 {
	var sum float64
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(line, w)
		}
		if !ok {
			continue
		}
		if v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64); err == nil {
			sum += v
		}
	}
	return sum
}

func grepLines(text, part string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, part) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
