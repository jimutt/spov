//go:build integration

package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

// jobOpenCode is a job sandbox's OpenCode for the scenario suites. It
// answers the calls spov makes into a job's sandbox, each checked against
// the pinned contract, and serves files from what a test put there: the
// reports in /workspace/out and the plugin's evidence and outcome files.
// A path in fail answers that status instead.
//
// @decision(DL-022)
type jobOpenCode struct {
	srv *httptest.Server

	mu         sync.Mutex
	files      map[string][]byte
	fail       map[string]int
	listStatus int
	calls      []string
	held       map[string]heldRead
}

// heldRead is a read that waits: arrived closes when it reaches the
// sandbox, and it answers once release closes or its caller gives up.
type heldRead struct{ arrived, release chan struct{} }

func newJobOpenCode(t *testing.T) *jobOpenCode {
	t.Helper()
	o := &jobOpenCode{files: map[string][]byte{}, fail: map[string]int{}, held: map[string]heldRead{}}
	o.srv = httptest.NewServer(contract.Checked(t, http.HandlerFunc(o.serve)))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *jobOpenCode) put(p string, body []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.files[p] = body
}

// reset forgets every file and failure, for the next job.
func (o *jobOpenCode) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.files, o.fail, o.listStatus, o.held = map[string][]byte{}, map[string]int{}, 0, map[string]heldRead{}
}

func (o *jobOpenCode) failRead(p string, status int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail[p] = status
}

// hold makes the next reads of p wait, for a test that cuts a delivery
// off partway; the returned channel closes when one arrives, and release
// lets them answer.
//
// @decision(DL-022)
func (o *jobOpenCode) hold(p string) (arrived <-chan struct{}, release func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	h := heldRead{arrived: make(chan struct{}), release: make(chan struct{})}
	o.held[p] = h
	var once sync.Once
	return h.arrived, func() {
		once.Do(func() {
			o.mu.Lock()
			delete(o.held, p)
			o.mu.Unlock()
			close(h.release)
		})
	}
}

func (o *jobOpenCode) failList(status int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.listStatus = status
}

// sent is the requests that contain part, as "METHOD path?query body".
func (o *jobOpenCode) sent(part string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for _, c := range o.calls {
		if strings.Contains(c, part) {
			out = append(out, c)
		}
	}
	return out
}

func (o *jobOpenCode) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	o.calls = append(o.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(body))
	o.mu.Unlock()
	dir := r.URL.Query().Get("location[directory]")
	// The fake sandbox's connection URL carries /connect/<id>/<port>.
	if rest, ok := strings.CutPrefix(r.URL.Path, "/connect/"); ok {
		if parts := strings.SplitN(rest, "/", 3); len(parts) == 3 {
			r.URL.Path = "/" + parts[2]
		}
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/session":
		jobJSON(w, `{"data":{"id":"ses_job"}}`)
	case r.Method == http.MethodGet && r.URL.Path == "/api/fs/list":
		o.mu.Lock()
		status := o.listStatus
		var names []string
		for p := range o.files {
			if rel, ok := strings.CutPrefix(p, dir+"/"); ok && !strings.Contains(rel, "/") {
				names = append(names, rel)
			}
		}
		o.mu.Unlock()
		if status != 0 {
			http.Error(w, "listing failed", status)
			return
		}
		sort.Strings(names)
		entries := make([]map[string]string, 0, len(names))
		for _, n := range names {
			entries = append(entries, map[string]string{"path": n, "type": "file"})
		}
		raw, _ := json.Marshal(map[string]any{"location": map[string]string{"directory": dir}, "data": entries})
		jobJSON(w, string(raw))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/fs/read/"):
		p := path.Join(dir, strings.TrimPrefix(r.URL.Path, "/api/fs/read/"))
		o.mu.Lock()
		body, ok := o.files[p]
		status := o.fail[p]
		h, held := o.held[p]
		if held {
			select {
			case <-h.arrived:
			default:
				close(h.arrived)
			}
		}
		o.mu.Unlock()
		if held {
			select {
			case <-h.release:
			case <-r.Context().Done():
				return
			}
		}
		switch {
		case status != 0:
			http.Error(w, "read failed", status)
		case !ok:
			http.Error(w, "not found", http.StatusNotFound)
		default:
			_, _ = w.Write(body)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/interrupt"):
		jobJSON(w, `{"data":true}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt"):
		jobJSON(w, `{"data":{"id":"msg_scenario","sessionID":"ses_job","time":{"created":1},`+
			`"type":"user","payload":{"text":"x"},"delivery":"steer"}}`)
	case r.Method == http.MethodPost || r.Method == http.MethodDelete:
		// /model, /synthetic, /command, a permission's or a form's
		// reply: each answers with no body.
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func jobJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

// evidence writes the plugin's two files as a run that ended would leave
// them: each line in both, as the plugin copies the deciding ones.
func (o *jobOpenCode) evidence(lines ...string) {
	body := []byte(strings.Join(append([]string{`{"event":"loop.stopped","reason":"declared"}`}, lines...), "\n") + "\n")
	o.put(opencode.PluginEvidencePath, body)
	o.put(opencode.PluginOutcomePath, body)
}

// withJobs points every job sandbox at o and lets the store keep the
// scenario's clock, so the wall-clock ledger and the supervisor read one
// time.
func (s *scenarioServer) withJobs(o *jobOpenCode) {
	s.t.Helper()
	s.provider.ConnectBaseURL = o.srv.URL
	if s.provider.ExecResults == nil {
		s.provider.ExecResults = map[string]sandbox.ExecResult{}
	}
	s.provider.ExecResults["printenv "+opencode.PasswordEnvVar] = sandbox.ExecResult{Stdout: "scenario-password\n"}
	pg, ok := s.store.(*postgres.Store)
	if !ok {
		s.t.Fatalf("the scenario store is %T, not Postgres", s.store)
	}
	pg.Now = s.clock.Now
}

// createJob creates a job as member through the API.
func (s *scenarioServer) createJob(req map[string]any) server.JobView {
	s.t.Helper()
	body := map[string]any{
		"repos": []map[string]string{{"platform": "github", "owner": s.gh.org, "name": "shared-only"}},
	}
	for k, v := range req {
		body[k] = v
	}
	var job server.JobView
	s.must(http.StatusAccepted, "member", http.MethodPost, "/api/v1/jobs", body, &job)
	return job
}

// pass runs one supervisor attempt on a job, as River would deliver it,
// and returns the row.
func (s *scenarioServer) pass(id string) store.Job {
	s.t.Helper()
	ctx := context.Background()
	if err := s.jobs.Run(ctx, jobs.JobArgs{JobID: id, OrgID: server.DefaultOrgID}, 1); err != nil {
		s.t.Fatalf("supervisor pass on %s: %v", id, err)
	}
	return s.jobRow(id)
}

func (s *scenarioServer) jobRow(id string) store.Job {
	s.t.Helper()
	j, err := s.store.Jobs().Get(context.Background(), server.DefaultOrgID, id)
	if err != nil {
		s.t.Fatal(err)
	}
	return j
}

// passUntil runs passes until the job reaches want, failing after a few.
func (s *scenarioServer) passUntil(id string, want store.JobState) store.Job {
	s.t.Helper()
	var j store.Job
	for range 6 {
		if j = s.pass(id); j.State == want {
			return j
		}
	}
	s.t.Fatalf("job %s is %v after six passes, want %v (error %q)", id, j.State, want, j.Error)
	return j
}

// passUntilDone runs passes until the job has a verdict.
func (s *scenarioServer) passUntilDone(id string) store.Job {
	s.t.Helper()
	var j store.Job
	for range 6 {
		if j = s.pass(id); j.State.Terminal() {
			return j
		}
	}
	s.t.Fatalf("job %s is %v after six passes, want it finished (error %q)", id, j.State, j.Error)
	return j
}

// storeApp has the admin store the organisation's GitHub App, which
// reaches the repositories every job here lands in.
func (s *scenarioServer) storeApp() {
	s.t.Helper()
	key := s.gh.newKey(s.t, "jobs")
	s.appKeys = append(s.appKeys, key)
	s.must(http.StatusOK, "admin", http.MethodPut, "/api/v1/org/github/github",
		map[string]string{"app_id": s.gh.appID, "private_key": key}, nil)
}

// steps returns a runner for a scenario's numbered steps: each fails on
// its own, and a failed one ends the run, since later steps build on it.
func (s *scenarioServer) steps(t *testing.T) func(string, func(*testing.T)) {
	return func(name string, f func(*testing.T)) {
		ok := t.Run(name, func(st *testing.T) {
			s.t = st
			defer func() { s.t = t }()
			f(st)
		})
		if !ok {
			t.FailNow()
		}
	}
}

func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// conversationOf is the job's conversation row, which opening the goal
// recorded.
func (s *scenarioServer) conversationOf(j store.Job) store.Conversation {
	s.t.Helper()
	convs, err := s.store.Conversations().ListConversations(context.Background(), j.OrgID, j.SessionID)
	if err != nil || len(convs) == 0 {
		s.t.Fatalf("job %s has no conversation: %v", j.ID, err)
	}
	return convs[0]
}

// frame stores one of the sandbox's frames in the job's conversation, as
// the recorder would, from source opencode or ephemeral.
func (s *scenarioServer) frame(j store.Job, source store.EventSource, raw string) {
	s.t.Helper()
	conv := s.conversationOf(j)
	now := s.clock.Now()
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(raw), &head)
	ev := store.Event{
		ID: store.NewID(now), OrgID: j.OrgID, SessionID: j.SessionID, ConversationID: conv.ID,
		Source: source, Name: head.Type, At: now, Raw: []byte(raw),
	}
	// Only the sandbox's durable frames carry a sequence.
	if source == store.SourceOpenCode {
		ev.Seq = time.Now().UnixNano()
	}
	if _, err := s.store.Conversations().PutEvents(context.Background(), []store.Event{ev}); err != nil {
		s.t.Fatal(err)
	}
}

// worked stores an assistant message whose tool call ran, which is what
// gate 3 needs to see before it lets anything succeed.
func (s *scenarioServer) worked(j store.Job) {
	s.t.Helper()
	conv := s.conversationOf(j)
	now := s.clock.Now()
	if _, err := s.store.Conversations().PutMessages(context.Background(), []store.Message{{
		ID: store.NewID(now), OrgID: j.OrgID, SessionID: j.SessionID, ConversationID: conv.ID,
		OpenCodeID: "msg_work_" + strings.ToLower(randomAlnum(6)), Role: "assistant", At: now,
		Raw: []byte(`{"id":"msg_work","type":"assistant","content":[{"type":"tool","name":"read",` +
			`"executed":true,"state":{"status":"completed"}}]}`),
	}}); err != nil {
		s.t.Fatal(err)
	}
}

// stopped stores the plugin's stop marker, which ends the run.
func (s *scenarioServer) stopped(j store.Job) {
	s.t.Helper()
	s.frame(j, store.SourceOpenCode, `{"type":"session.inbox.enqueued","data":{"item":{"type":"synthetic",`+
		`"payload":{"text":"SPOV_LOOP_STOPPED {\"reason\":\"declared\"}"},"delivery":"steer"}}}`)
}

// job is the API's view of a job, as member.
func (s *scenarioServer) job(id string) server.JobView {
	s.t.Helper()
	var v server.JobView
	s.must(http.StatusOK, "member", http.MethodGet, "/api/v1/jobs/"+id, nil, &v)
	return v
}

// gate is one of a job view's gates by name.
func gate(v server.JobView, name string) jobs.GateResult {
	for _, g := range v.Gates {
		if g.Gate == name {
			return g
		}
	}
	return jobs.GateResult{}
}
