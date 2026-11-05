//go:build integration

package cli

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// TestScenario1 is scenario 1's control-plane layer (DL-022): pr jobs
// across several repositories, created through the API and judged
// against scenarioGitHub's pull requests. One coordinated job is attached
// to at once, corrected without pausing, taken over and resumed three
// times, and corrected again just before it completes; two more show a
// three-repository launch that changes one and a recorded pull request
// that was closed. Real pull requests on a real forge are not exercised
// here.
//
//	SPOV_TEST_DATABASE_URL=... go test -tags integration ./internal/cli/ -run TestScenario1 -v
//
// @decision(DL-022) @decision(DL-051) @decision(DL-050) @decision(DL-049) @decision(DL-048)
func TestScenario1(t *testing.T) {
	s := newScenarioServer(t)
	s.gh.installRepo("docs")
	s.storeApp()
	oc := newJobOpenCode(t)
	s.withJobs(oc)
	step := s.steps(t)
	const allowance = time.Hour

	repos := func(names ...string) []map[string]string {
		out := make([]map[string]string, len(names))
		for i, n := range names {
			out[i] = map[string]string{"platform": "github", "owner": s.gh.org, "name": n}
		}
		return out
	}
	recorded := func(repo, branch string) string {
		return `{"event":"` + opencode.DeliverableEvent + `","platform":"github","repository":"` + s.gh.org + "/" + repo + `","branch":"` + branch + `"}`
	}
	pr := func(v server.JobView, repo, branch string) server.DeliverableView {
		for _, d := range v.Deliverables {
			if d.Repository == s.gh.org+"/"+repo && d.Branch == branch {
				return d
			}
		}
		s.t.Fatalf("job %s has no pull request from %s in %s: %+v", v.ID, branch, repo, v.Deliverables)
		return server.DeliverableView{}
	}

	created := s.createJob(map[string]any{
		"kind": "pr", "goal": "Change the API in shared-only and its client in both.", "max_duration": "1h",
		"repos": repos("shared-only", "both"), "check": "make check",
	})
	id := created.ID
	var j store.Job
	used := func() time.Duration { return s.jobRow(id).ClockElapsed(s.clock.Now()) }
	// counted is the running time the steps below let pass.
	var counted time.Duration
	run := func(d time.Duration) {
		s.clock.advance(d)
		counted += d
	}
	item := func(inbox, after string, delivered bool) {
		s.frame(j, store.SourceOpenCode, `{"type":"session.inbox.enqueued","data":{"inboxID":"`+inbox+`","item":{"type":"user",`+
			`"payload":{"text":"a correction","metadata":{"spov":{"after":"`+after+`"}}},"delivery":"steer"}}}`)
		if delivered {
			s.frame(j, store.SourceOpenCode, `{"type":"session.inbox.delivered","data":{"inboxID":"`+inbox+`"}}`)
		}
	}

	step("1.1 attach waits for the queued job and joins it once it runs", func(t *testing.T) {
		var v server.SessionView
		s.must(http.StatusOK, "member", http.MethodGet, "/api/v1/sessions/"+created.SessionID, nil, &v)
		if why, err := joinable(v); err != nil || why == "" {
			t.Errorf("a queued job: attach would join (%q, %v), want it to wait", why, err)
		}
		j = s.passUntil(id, store.JobRunning)
		s.worked(j)
		s.must(http.StatusOK, "member", http.MethodGet, "/api/v1/sessions/"+created.SessionID, nil, &v)
		if why, err := joinable(v); err != nil || why != "" {
			t.Errorf("a running job: attach would wait or refuse (%q, %v)", why, err)
		}
		if got := s.row(j.SessionID).Request.Repos; len(got) != 2 {
			t.Errorf("the session clones %v, want both repositories", got)
		}
	})

	step("1.2 a correction sent to continue is read without pausing the job", func(t *testing.T) {
		run(5 * time.Minute)
		item("inb_fix", "continue", true)
		if got := s.pass(id); got.State != store.JobRunning {
			t.Fatalf("state %s, want running", got.State)
		}
		run(5 * time.Minute)
		if d := used(); d != counted {
			t.Errorf("the clock reads %v after %v of running", d, counted)
		}
	})

	step("1.3 three takeovers and resumes never add time", func(t *testing.T) {
		for i, inbox := range []string{"inb_take1", "inb_take2", "inb_take3"} {
			item(inbox, "handover", true)
			if got := s.pass(id); got.State != store.JobSteered {
				t.Fatalf("takeover %d: state %s, want steered", i+1, got.State)
			}
			s.clock.advance(20 * time.Minute)
			s.must(http.StatusNoContent, "member", http.MethodPost, "/api/v1/jobs/"+id+"/resume", nil, nil)
			if got := s.pass(id); got.State != store.JobRunning {
				t.Fatalf("resume %d: state %s, want running", i+1, got.State)
			}
			run(3 * time.Minute)
		}
		if d := used(); d != counted {
			t.Errorf("after an hour steered over three takeovers the clock reads %v, want the %v it ran", d, counted)
		}
		if left := allowance - used(); left != allowance-counted {
			t.Errorf("%v left of the allowance, want %v", left, allowance-counted)
		}
	})

	step("1.4 a correction queued as the agent completes holds the job until it is read", func(t *testing.T) {
		// The agent tries to complete with the message still waiting:
		// the plugin records the attempt as deferred and announces
		// nothing, so the job runs on.
		item("inb_late", "continue", false)
		if got := s.pass(id); got.State != store.JobRunning {
			t.Fatalf("state %s, want running", got.State)
		}
		run(2 * time.Minute)
		s.frame(j, store.SourceOpenCode, `{"type":"session.inbox.delivered","data":{"inboxID":"inb_late"}}`)
		if got := s.pass(id); got.State != store.JobRunning {
			t.Fatalf("after the correction was read: state %s, want running", got.State)
		}
		run(4 * time.Minute)
	})

	step("1.5 the revised set is judged: every pull request verified, two in one repository", func(t *testing.T) {
		s.gh.openPull("shared-only", "spov/api", 11)
		s.gh.openPull("shared-only", "spov/api-docs", 12)
		s.gh.openPull("both", "spov/client", 21)
		mints := len(s.gh.mintLog())
		oc.evidence(
			recorded("shared-only", "spov/api"),
			recorded("shared-only", "spov/api-docs"),
			`{"event":"check.finished","command":"make check","exit":1,"output":"FAIL: client"}`,
			`{"event":"completion.deferred","pending":1}`,
			// What the correction asked for: the supporting change in
			// both, and the API's branch recorded again.
			recorded("both", "spov/client"),
			recorded("shared-only", "spov/api"),
			// A repository the job was not given is not part of it.
			recorded("personal-only", "spov/x"),
			`{"event":"check.finished","command":"make check","exit":0,"output":"ok"}`,
			`{"event":"completion.declared","summary":"API, docs and client changed"}`,
		)
		s.stopped(j)
		final := s.passUntilDone(id)
		v := s.job(id)
		if final.State != store.JobSucceeded || v.Partial {
			t.Fatalf("state %s, partial %v (%q), want a full success", final.State, v.Partial, final.Error)
		}
		if g := gate(v, jobs.GateDeliverable); g.Outcome != jobs.GatePassed || !strings.Contains(g.Detail, "3 of 3 pull requests verified") {
			t.Errorf("gate 2 = %+v", g)
		}
		if g := gate(v, jobs.GateMechanical); g.Outcome != jobs.GatePassed {
			t.Errorf("gate 1 = %+v; the check that decides is the last one, run after the correction", g)
		}
		if len(v.Deliverables) != 3 {
			t.Errorf("deliverables %+v, want the three in the job's repositories, once each", v.Deliverables)
		}
		for _, want := range []struct {
			repo, branch string
			number       int
		}{{"shared-only", "spov/api", 11}, {"shared-only", "spov/api-docs", 12}, {"both", "spov/client", 21}} {
			d := pr(v, want.repo, want.branch)
			if d.Status != store.DeliverableVerified || d.Number != want.number || !strings.HasSuffix(d.URL, "/pull/"+strconv.Itoa(want.number)) {
				t.Errorf("%s %s: %+v", want.repo, want.branch, d)
			}
		}
		// Each pull request is asked about with a token for its own
		// repository alone.
		for _, m := range s.gh.mintLog()[mints:] {
			if len(m.Repos) != 1 {
				t.Errorf("gate 2 minted a token for %v, want one repository", m.Repos)
			}
		}
		if final.ClockUsed != counted {
			t.Errorf("the job used %v, want the %v it ran", final.ClockUsed, counted)
		}
	})

	step("1.6 a three-repository launch that changes one is a full success", func(t *testing.T) {
		oc.reset()
		mints := len(s.gh.mintLog())
		three := s.createJob(map[string]any{
			"kind": "pr", "goal": "Fix the typo wherever it is.", "repos": repos("shared-only", "both", "docs"),
		})
		tj := s.passUntil(three.ID, store.JobRunning)
		s.worked(tj)
		var launch []string
		for _, m := range s.gh.mintLog()[mints:] {
			if len(m.Repos) > len(launch) {
				launch = m.Repos
			}
		}
		slices.Sort(launch)
		if strings.Join(launch, ",") != "both,docs,shared-only" {
			t.Errorf("the sandbox's token reaches %v, want all three repositories", launch)
		}
		s.gh.openPull("docs", "spov/typo", 31)
		oc.evidence(recorded("docs", "spov/typo"), `{"event":"completion.declared","summary":"only docs had the typo"}`)
		s.stopped(tj)
		s.passUntilDone(three.ID)
		v := s.job(three.ID)
		if v.State != store.JobSucceeded || v.Partial {
			t.Fatalf("state %s, partial %v, want a full success on one pull request", v.State, v.Partial)
		}
		if g := gate(v, jobs.GateDeliverable); !strings.Contains(g.Detail, "1 of 1 pull request verified") {
			t.Errorf("gate 2 = %+v", g)
		}
	})

	// prJob runs a pr job on shared-only whose evidence is lines, with a
	// person's message first when asked, and returns its view.
	prJob := func(goal string, person bool, lines ...string) server.JobView {
		s.t.Helper()
		oc.reset()
		created := s.createJob(map[string]any{"kind": "pr", "goal": goal, "repos": repos("shared-only")})
		pj := s.passUntil(created.ID, store.JobRunning)
		s.worked(pj)
		if person {
			s.clock.advance(time.Minute)
			s.frame(pj, store.SourceOpenCode, `{"type":"session.inbox.enqueued","data":{"inboxID":"inb_drop","item":{"type":"user",`+
				`"payload":{"text":"drop the second one","metadata":{"spov":{"after":"continue"}}},"delivery":"steer"}}}`)
		}
		oc.evidence(append(lines, `{"event":"completion.declared","summary":"done"}`)...)
		s.stopped(pj)
		s.passUntilDone(created.ID)
		return s.job(created.ID)
	}
	// at stamps a record line with the scenario's clock, as the plugin
	// stamps its own.
	at := func(line string, when time.Time) string {
		return strings.TrimSuffix(line, "}") + `,"at":"` + when.Format(time.RFC3339Nano) + `"}`
	}
	withdrawn := func(repo, branch string) string {
		return strings.TrimSuffix(recorded(repo, branch), "}") + `,"withdrawn":true}`
	}

	step("1.7 a recorded pull request closed without merging makes the job partial", func(t *testing.T) {
		s.gh.openPull("shared-only", "spov/one", 41)
		s.gh.openPull("shared-only", "spov/two", 42)
		s.gh.closePull("shared-only", "spov/two")
		v := prJob("Split the change in two.", false, recorded("shared-only", "spov/one"), recorded("shared-only", "spov/two"))
		if v.State != store.JobSucceeded || !v.Partial {
			t.Fatalf("state %s, partial %v, want a partial success", v.State, v.Partial)
		}
		if d := pr(v, "shared-only", "spov/two"); d.Status != store.DeliverableMissing || d.Detail != "closed without merging" || d.Number != 42 {
			t.Errorf("the closed one: %+v", d)
		}
		if g := gate(v, jobs.GateDeliverable); !strings.Contains(g.Detail, "1 of 2 pull requests verified") {
			t.Errorf("gate 2 = %+v", g)
		}
	})

	step("1.7b a pull request someone merged before the job ended is verified", func(t *testing.T) {
		s.gh.openPull("shared-only", "spov/merged", 43)
		s.gh.mergePull("shared-only", "spov/merged")
		v := prJob("Fix it.", false, recorded("shared-only", "spov/merged"))
		if v.State != store.JobSucceeded || v.Partial {
			t.Fatalf("state %s, partial %v, want a full success", v.State, v.Partial)
		}
		if d := pr(v, "shared-only", "spov/merged"); d.Status != store.DeliverableVerified || d.Detail != "merged" || d.Number != 43 {
			t.Errorf("the merged one: %+v", d)
		}
	})

	step("1.7c a pull request withdrawn after a person wrote is left out; unasked, it is judged", func(t *testing.T) {
		s.gh.openPull("shared-only", "spov/keep", 44)
		s.gh.openPull("shared-only", "spov/drop", 45)
		s.gh.closePull("shared-only", "spov/drop")
		// Recorded before the person's message (stamped at the job's
		// pass, a minute on), withdrawn after it.
		now := s.clock.Now()
		lines := []string{at(recorded("shared-only", "spov/keep"), now), at(recorded("shared-only", "spov/drop"), now),
			at(withdrawn("shared-only", "spov/drop"), now.Add(2*time.Minute))}
		v := prJob("Two pull requests, then fold them.", true, lines...)
		if v.State != store.JobSucceeded || v.Partial {
			t.Fatalf("asked: state %s, partial %v, want a full success", v.State, v.Partial)
		}
		if d := pr(v, "shared-only", "spov/drop"); d.Status != store.DeliverableWithdrawn {
			t.Errorf("the withdrawn one: %+v", d)
		}
		if g := gate(v, jobs.GateDeliverable); !strings.Contains(g.Detail, "1 of 1 pull request verified") || !strings.Contains(g.Detail, "1 withdrawn") {
			t.Errorf("gate 2 = %+v", g)
		}
		v = prJob("Two pull requests.", false, lines...)
		if v.State != store.JobSucceeded || !v.Partial || pr(v, "shared-only", "spov/drop").Status != store.DeliverableMissing {
			t.Fatalf("unasked: state %s, partial %v, %+v, want it judged and missing", v.State, v.Partial, v.Deliverables)
		}
		if g := gate(v, jobs.GateDeliverable); !strings.Contains(g.Detail, "nobody asked it to drop") {
			t.Errorf("gate 2 = %+v", g)
		}
	})

	step("1.8 nothing secret reached a response or the log", func(t *testing.T) {
		s.scan(t, s.appKeys, "")
	})
}
