//go:build integration

package cli

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// TestScenario6 is scenario 6's control-plane layer (DL-022): one report
// job parked on a permission, answered, taken over, resumed and ended at
// its wall clock, on the scenario's clock. A job's plugin never asks at
// the pin, so the park is the frame a parked OpenCode would send. The
// steering a reader meets in the browser is not exercised here.
//
//	SPOV_TEST_DATABASE_URL=... go test -tags integration ./internal/cli/ -run TestScenario6 -v
//
// @decision(DL-022) @decision(DL-050) @decision(DL-049) @decision(DL-054) @decision(DL-052)
func TestScenario6(t *testing.T) {
	s := newScenarioServer(t)
	s.storeApp()
	oc := newJobOpenCode(t)
	s.withJobs(oc)
	step := s.steps(t)
	const allowance = time.Hour

	created := s.createJob(map[string]any{"kind": "report", "goal": "Audit the repository.", "max_duration": "1h"})
	id := created.ID
	j := s.passUntil(id, store.JobRunning)
	s.worked(j)
	conv := s.conversationOf(j)
	used := func() time.Duration { return s.jobRow(id).ClockElapsed(s.clock.Now()) }
	ask := func(req string) {
		s.frame(j, store.SourceEphemeral, `{"type":"permission.asked","data":{"id":"`+req+`","sessionID":"`+
			conv.OpenCodeID+`","action":"read","resources":["/etc/os-release"],"save":["/etc/*"]}}`)
	}
	answer := func(req, decision string) (int, []byte) {
		return s.call("member", http.MethodPost, "/api/v1/sessions/"+j.SessionID+"/permissions/"+req,
			map[string]string{"decision": decision})
	}
	item := func(inbox, after string) {
		meta := ""
		if after != "" {
			meta = `,"metadata":{"spov":{"after":"` + after + `"}}`
		}
		s.frame(j, store.SourceOpenCode, `{"type":"session.inbox.enqueued","data":{"inboxID":"`+inbox+`","item":{"type":"user",`+
			`"payload":{"text":"a word from the reader"`+meta+`},"delivery":"steer"}}}`)
		s.frame(j, store.SourceOpenCode, `{"type":"session.inbox.delivered","data":{"inboxID":"`+inbox+`"}}`)
	}

	step("6.1 a job parked on a permission is blocked, and its clock keeps counting", func(t *testing.T) {
		s.clock.advance(5 * time.Minute)
		ask("per_one")
		if got := s.pass(id); got.State != store.JobBlocked {
			t.Fatalf("state %s, want blocked", got.State)
		}
		before := used()
		s.clock.advance(10 * time.Minute)
		if d := used() - before; d != 10*time.Minute {
			t.Errorf("ten blocked minutes counted %v", d)
		}
		if v := s.job(id); v.State != store.JobBlocked {
			t.Errorf("the job view says %s", v.State)
		}
	})

	step("6.2 a blocked job takes no message, no standing grant, and can be attached to", func(t *testing.T) {
		if code, raw := s.call("member", http.MethodPost, "/api/v1/sessions/"+j.SessionID+"/messages",
			map[string]string{"text": "carry on"}); code != http.StatusConflict {
			t.Errorf("a message to a blocked job: %d %s, want 409", code, raw)
		}
		if code, raw := answer("per_one", "always"); code < 400 || code >= 500 {
			t.Errorf("an always grant: %d %s, want it refused", code, raw)
		}
		var v server.SessionView
		s.must(http.StatusOK, "member", http.MethodGet, "/api/v1/sessions/"+j.SessionID, nil, &v)
		if why, err := joinable(v); err != nil || why != "" {
			t.Errorf("attach would wait or refuse: %q, %v", why, err)
		}
	})

	step("6.3 an answer releases the job only once nothing else is asked, and frees its loop", func(t *testing.T) {
		ask("per_two")
		if code, raw := answer("per_one", "once"); code != http.StatusAccepted {
			t.Fatalf("allow once: %d %s", code, raw)
		}
		if got := s.pass(id); got.State != store.JobBlocked {
			t.Fatalf("with per_two still asked: state %s, want blocked", got.State)
		}
		loops := len(oc.sent("/command"))
		if code, raw := answer("per_two", "once"); code != http.StatusAccepted {
			t.Fatalf("allow once: %d %s", code, raw)
		}
		if got := s.pass(id); got.State != store.JobRunning {
			t.Fatalf("both answered: state %s, want running", got.State)
		}
		if sent := oc.sent("/command"); len(sent) != loops+1 || !strings.Contains(sent[len(sent)-1], server.LoopResume) {
			t.Errorf("the release sent %v, want one resume", sent[loops:])
		}
		if replies := oc.sent("/permission/"); len(replies) != 2 {
			t.Errorf("the sandbox got %d replies, want 2: %v", len(replies), replies)
		}
	})

	step("6.4 a continue message leaves the job running and counting", func(t *testing.T) {
		item("inb_continue", "continue")
		if got := s.pass(id); got.State != store.JobRunning {
			t.Fatalf("state %s, want running", got.State)
		}
		before := used()
		s.clock.advance(5 * time.Minute)
		if d := used() - before; d != 5*time.Minute {
			t.Errorf("five running minutes counted %v", d)
		}
	})

	step("6.5 a handover steers the job and stops its clock; Resume continues it", func(t *testing.T) {
		item("inb_handover", "handover")
		if got := s.pass(id); got.State != store.JobSteered {
			t.Fatalf("state %s, want steered", got.State)
		}
		paused := used()
		s.clock.advance(20 * time.Minute)
		if d := used() - paused; d != 0 {
			t.Errorf("twenty steered minutes counted %v", d)
		}
		s.must(http.StatusNoContent, "member", http.MethodPost, "/api/v1/jobs/"+id+"/resume", nil, nil)
		if got := s.jobRow(id); got.State != store.JobRunning {
			t.Fatalf("after Resume: state %s, want running", got.State)
		}
		if d := used() - paused; d != 0 {
			t.Errorf("Resume added %v", d)
		}
	})

	step("6.6 the job ends at its wall clock with exactly its allowance used", func(t *testing.T) {
		oc.put(opencode.OutDir+"/audit.md", []byte("# Audit\n"))
		s.clock.advance(allowance - used() + time.Second)
		if got := s.pass(id); got.State != store.JobDelivering {
			t.Fatalf("past its allowance: state %s, want delivering", got.State)
		}
		final := s.passUntilDone(id)
		if final.ClockUsed < allowance || final.ClockUsed > allowance+2*time.Second {
			t.Errorf("the job used %v of a %v allowance", final.ClockUsed, allowance)
		}
		v := s.job(id)
		if g := gate(v, jobs.GateClassification); g.Outcome != jobs.GatePassed {
			t.Errorf("gate 3 = %+v; the answered requests must not read as a park", g)
		}
		if v.State != store.JobSucceeded || !v.Partial || !strings.Contains(gate(v, jobs.GateDeliverable).Detail, "wall clock") {
			t.Errorf("state %s, partial %v, gate 2 %+v: want a partial success at the wall clock",
				v.State, v.Partial, gate(v, jobs.GateDeliverable))
		}
	})

	step("6.7 nothing secret reached a response or the log", func(t *testing.T) {
		s.scan(t, s.appKeys, "")
	})
}
