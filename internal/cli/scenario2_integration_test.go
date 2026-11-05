//go:build integration

package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// pngBody is the smallest bytes collection recognises as a PNG.
var pngBody = []byte("\x89PNG\r\n\x1a\nscenario-image")

// TestScenario2 is scenario 2's control-plane layer (DL-022): report jobs
// created through the API, run by the supervisor wired as `spov serve`
// wires it, against a job sandbox whose OpenCode serves the reports a
// step put in /workspace/out, and judged on Postgres. What only a real
// sandbox and the page show is not exercised here.
//
//	SPOV_TEST_DATABASE_URL=... go test -tags integration ./internal/cli/ -run TestScenario2 -v
//
// @decision(DL-022) @decision(DL-051) @decision(DL-050) @decision(DL-063)
func TestScenario2(t *testing.T) {
	s := newScenarioServer(t)
	s.storeApp()
	oc := newJobOpenCode(t)
	s.withJobs(oc)
	step := s.steps(t)

	// run takes a report job from creation to its verdict: the reports
	// are in place, the agent did some work, the plugin wrote its
	// evidence and the loop stopped.
	run := func(req map[string]any, files map[string][]byte, evidence ...string) server.JobView {
		s.t.Helper()
		// Whatever a step set up is for this job alone.
		defer oc.reset()
		for p, body := range files {
			oc.put(opencode.OutDir+"/"+p, body)
		}
		created := s.createJob(merge(map[string]any{"kind": "report", "goal": "Write the reports."}, req))
		j := s.passUntil(created.ID, store.JobRunning)
		s.worked(j)
		oc.evidence(evidence...)
		s.stopped(j)
		s.passUntilDone(created.ID)
		return s.job(created.ID)
	}
	deliverable := func(v server.JobView, path string) server.DeliverableView {
		for _, d := range v.Deliverables {
			if d.Path == path {
				return d
			}
		}
		s.t.Fatalf("job %s has no deliverable %s: %+v", v.ID, path, v.Deliverables)
		return server.DeliverableView{}
	}

	var full server.JobView
	step("2.1 several reports and their images are collected and pass", func(t *testing.T) {
		full = run(nil, map[string][]byte{
			"a.md":      []byte("# Findings\n\n![chart](chart.png)\n"),
			"b.md":      []byte("# Appendix\n"),
			"chart.png": pngBody,
		}, `{"event":"completion.declared"}`)
		if full.State != store.JobSucceeded || full.Partial {
			t.Fatalf("state %s, partial %v (%q), want a full success", full.State, full.Partial, full.Error)
		}
		if g := gate(full, jobs.GateDeliverable); g.Outcome != jobs.GatePassed || !strings.Contains(g.Detail, "2 of 2 reports verified") {
			t.Errorf("gate 2 = %+v", g)
		}
		for _, p := range []string{"a.md", "b.md"} {
			if d := deliverable(full, p); d.Status != store.DeliverableVerified {
				t.Errorf("%s is %s (%s)", p, d.Status, d.Detail)
			}
		}
	})

	step("2.2 a mixed set is judged partial, each fault named", func(t *testing.T) {
		oc.failRead(opencode.OutDir+"/broken.md", http.StatusBadGateway)
		v := run(nil, map[string][]byte{
			"good.md":   []byte("# Good\n"),
			"big.md":    bytes.Repeat([]byte("x"), jobs.MaxFileBytes+1),
			"broken.md": []byte("# Never read\n"),
			"pics.md":   []byte("# Pictures\n\n![here](here.png) ![gone](gone.png)\n"),
			"here.png":  pngBody,
		}, `{"event":"completion.declared"}`)
		if v.State != store.JobSucceeded || !v.Partial {
			t.Fatalf("state %s, partial %v (%q), want a partial success", v.State, v.Partial, v.Error)
		}
		g := gate(v, jobs.GateDeliverable)
		for _, want := range []string{"2 of 4 reports verified", "1 could not be checked", "1 over the size limit", "1 report lost some linked images"} {
			if !strings.Contains(g.Detail, want) {
				t.Errorf("gate 2 detail %q does not say %q", g.Detail, want)
			}
		}
		for path, want := range map[string]store.DeliverableStatus{
			"good.md": store.DeliverableVerified, "pics.md": store.DeliverableVerified,
			"big.md": store.DeliverableOverLimit, "broken.md": store.DeliverableUnverified,
		} {
			if d := deliverable(v, path); d.Status != want {
				t.Errorf("%s is %s (%s), want %s", path, d.Status, d.Detail, want)
			}
		}
		if d := deliverable(v, "pics.md"); !strings.Contains(d.Detail, "gone.png was not found") {
			t.Errorf("pics.md's detail %q does not name the lost image", d.Detail)
		}
		if d := deliverable(v, "broken.md"); !strings.Contains(d.Detail, "could not be read") {
			t.Errorf("broken.md's detail %q does not say it could not be read", d.Detail)
		}
	})

	step("2.2b a report whose only fault is a lost image is partial", func(t *testing.T) {
		v := run(nil, map[string][]byte{"pics.md": []byte("# Pictures\n\n![gone](gone.png)\n")},
			`{"event":"completion.declared"}`)
		if v.State != store.JobSucceeded || !v.Partial {
			t.Fatalf("state %s, partial %v, want a partial success", v.State, v.Partial)
		}
		if g := gate(v, jobs.GateDeliverable); g.Outcome != jobs.GatePartial {
			t.Errorf("gate 2 = %+v, want partial", g)
		}
	})

	step("2.3 an out directory that cannot be listed is unverified, not a failed delivery", func(t *testing.T) {
		oc.failList(http.StatusBadGateway)
		v := run(nil, map[string][]byte{"a.md": []byte("# A\n")}, `{"event":"completion.declared"}`)
		if g := gate(v, jobs.GateDeliverable); g.Outcome != jobs.GateUnverified || !strings.Contains(g.Detail, "could not be read") {
			t.Errorf("gate 2 = %+v, want unverified naming the listing", g)
		}
		if v.State == store.JobSucceeded {
			t.Errorf("a job whose reports could not be read succeeded")
		}
	})

	step("2.4 work the agent declared unfinished makes a success partial", func(t *testing.T) {
		v := run(nil, map[string][]byte{"a.md": []byte("# A\n")},
			`{"event":"completion.declared","unfinished":"the second report"}`)
		if v.State != store.JobSucceeded || !v.Partial {
			t.Fatalf("state %s, partial %v, want a partial success", v.State, v.Partial)
		}
		if g := gate(v, jobs.GateDeliverable); !strings.Contains(g.Detail, "the agent left unfinished: the second report") {
			t.Errorf("gate 2 detail %q", g.Detail)
		}
	})

	step("2.5 a failed check fails a job with verified reports, and one with a partial set", func(t *testing.T) {
		failed := `{"event":"check.finished","command":"make check","exit":1,"output":"FAIL: lint"}`
		whole := run(map[string]any{"check": "make check"}, map[string][]byte{"a.md": []byte("# A\n")},
			failed, `{"event":"completion.declared"}`)
		oc.failRead(opencode.OutDir+"/broken.md", http.StatusBadGateway)
		part := run(map[string]any{"check": "make check"},
			map[string][]byte{"a.md": []byte("# A\n"), "broken.md": []byte("x")},
			failed, `{"event":"completion.declared"}`)
		for name, v := range map[string]server.JobView{"verified reports": whole, "a partial set": part} {
			if v.State != store.JobFailed {
				t.Errorf("%s: state %s, want failed on the check", name, v.State)
			}
			if g := gate(v, jobs.GateMechanical); g.Outcome != jobs.GateFailed {
				t.Errorf("%s: gate 1 = %+v", name, g)
			}
			if g := gate(v, jobs.GateDeliverable); g.Outcome == jobs.GateFailed {
				t.Errorf("%s: gate 2 failed too, so the check was not what failed it", name)
			}
		}
	})

	step("2.6 a passed check with no verified report is not a success", func(t *testing.T) {
		oc.failRead(opencode.OutDir+"/only.md", http.StatusBadGateway)
		v := run(map[string]any{"check": "make check"}, map[string][]byte{"only.md": []byte("x")},
			`{"event":"check.finished","command":"make check","exit":0,"output":"ok"}`, `{"event":"completion.declared"}`)
		if v.State != store.JobFailed || v.Partial {
			t.Errorf("state %s, partial %v, want failed", v.State, v.Partial)
		}
		if g := gate(v, jobs.GateMechanical); g.Outcome != jobs.GatePassed {
			t.Errorf("gate 1 = %+v, want the check passed", g)
		}
	})

	step("2.7 collected reports are read after the sandbox has gone, and after a restart", func(t *testing.T) {
		sess := s.row(full.SessionID)
		if sess.State.Live() {
			t.Fatalf("the finished job's session is still %s", sess.State)
		}
		read := func() {
			body := s.file(full.ID, "a.md", false)
			if !strings.HasPrefix(string(body), "# Findings") {
				t.Errorf("a.md = %q", body)
			}
			if body := s.file(full.ID, "chart.png", false); !bytes.Equal(body, pngBody) {
				t.Errorf("chart.png = %q", body)
			}
			zipped := s.file(full.ID, "a.md", true)
			r, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
			if err != nil {
				t.Fatalf("the download is not a zip: %v", err)
			}
			var names []string
			for _, f := range r.File {
				names = append(names, f.Name)
			}
			if strings.Join(names, ",") != "a.md,chart.png" {
				t.Errorf("the bundle holds %v", names)
			}
		}
		if h := s.provider.Handles()[sess.SandboxID]; h == nil || !h.WasTerminated() {
			t.Errorf("the finished job's sandbox %s was not terminated", sess.SandboxID)
		}
		reads := len(oc.sent("/api/fs/"))
		read()
		s.restart(nil)
		read()
		if n := len(oc.sent("/api/fs/")) - reads; n != 0 {
			t.Errorf("reading the collected files asked the sandbox %d times", n)
		}
	})

	step("2.8 nothing secret reached a response or the log", func(t *testing.T) {
		s.scan(t, s.appKeys, "")
	})
}

// file reads one of a job's collected files through the API.
func (s *scenarioServer) file(id, path string, download bool) []byte {
	s.t.Helper()
	q := "/api/v1/jobs/" + id + "/file?path=" + path
	if download {
		q += "&download=true"
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.base+q, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.users["member"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("GET %s: %d %s", q, resp.StatusCode, body)
	}
	return body
}
