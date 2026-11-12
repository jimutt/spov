package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

// stub is a scripted server: a handler per path.
func stub(t *testing.T, routes map[string]http.HandlerFunc) *Client {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "tok")
}

func problem(w http.ResponseWriter, status int, detail, typ string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "title": http.StatusText(status), "detail": detail, "type": typ})
}

// TestRequestsCarryTheTokenAndDecodeBodies covers the plain calls.
//
// @decision(DL-002)
func TestRequestsCarryTheTokenAndDecodeBodies(t *testing.T) {
	var seenAuth, seenAccept, seenUA string
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/me": func(w http.ResponseWriter, r *http.Request) {
			seenAuth, seenAccept, seenUA = r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("User-Agent")
			_ = json.NewEncoder(w).Encode(server.MeView{UserID: "local", Admin: true})
		},
		"POST /api/v1/sessions": func(w http.ResponseWriter, r *http.Request) {
			var req server.SessionRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Name != "n" || r.Header.Get("Content-Type") != "application/json" {
				problem(w, 422, "bad body", "")
				return
			}
			w.WriteHeader(202)
			_, _ = fmt.Fprint(w, `{"id":"01ABC"}`)
		},
		"GET /api/v1/sessions": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"sessions":[{"id":"1","name":"a"},{"id":"2","name":"b"}]}`)
		},
		"GET /api/v1/sessions/{ref}": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(server.SessionView{ID: "1", Name: r.PathValue("ref")})
		},
		"DELETE /api/v1/sessions/{ref}": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) },
		"POST /api/v1/sessions/{ref}/connect": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(server.ConnectionView{URL: "https://edge", Token: "t", IdleTimeoutSecs: 60})
		},
		"GET /api/v1/repos": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"repos":[{"platform":"cf","owner":"o","name":"n","private":true}],"warnings":["gh: token expired"]}`)
		},
	})
	c.UserAgent = "spov/test"
	ctx := context.Background()

	me, err := c.Me(ctx)
	if err != nil || me.UserID != "local" || !me.Admin {
		t.Fatalf("me = %+v, %v", me, err)
	}
	if seenAuth != "Bearer tok" || seenAccept != "application/json" || seenUA != "spov/test" {
		t.Errorf("headers: auth=%q accept=%q ua=%q", seenAuth, seenAccept, seenUA)
	}
	id, err := c.CreateSession(ctx, server.SessionRequest{Name: "n"})
	if err != nil || id != "01ABC" {
		t.Errorf("create = %q, %v", id, err)
	}
	list, err := c.ListSessions(ctx, Page{})
	if err != nil || len(list.Sessions) != 2 {
		t.Errorf("list = %+v, %v", list, err)
	}
	got, err := c.GetSession(ctx, "a b")
	if err != nil || got.Name != "a b" {
		t.Errorf("get = %+v, %v; the ref must be path-escaped and decoded", got, err)
	}
	if err := c.DeleteSession(ctx, "1"); err != nil {
		t.Errorf("delete: %v", err)
	}
	conn, err := c.Connect(ctx, "1")
	if err != nil || conn.URL != "https://edge" || conn.IdleTimeoutSecs != 60 {
		t.Errorf("connect = %+v, %v", conn, err)
	}
	repos, warnings, err := c.ListRepos(ctx)
	if err != nil || len(repos) != 1 || !repos[0].Private || len(warnings) != 1 {
		t.Errorf("repos = %+v, warnings = %v, %v", repos, warnings, err)
	}
}

// TestProblemsBecomeErrors: a problem document is the error the caller
// sees, with its status and type available for mapping.
//
// @decision(DL-005)
func TestProblemsBecomeErrors(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}": func(w http.ResponseWriter, r *http.Request) {
			switch r.PathValue("ref") {
			case "gone":
				problem(w, 404, "no such session", "")
			case "stopped":
				problem(w, 409, "the session is not running", server.ProblemTypeSessionStopped)
			case "plain":
				http.Error(w, "nope", http.StatusServiceUnavailable)
			case "html":
				w.WriteHeader(502)
				_, _ = fmt.Fprint(w, "<html>bad gateway</html>")
			default:
				w.WriteHeader(401)
			}
		},
	})
	ctx := context.Background()

	_, err := c.GetSession(ctx, "gone")
	if !IsNotFound(err) || err.Error() != "no such session" {
		t.Errorf("gone: %v", err)
	}
	_, err = c.GetSession(ctx, "stopped")
	if !IsSessionStopped(err) || StatusOf(err) != 409 {
		t.Errorf("stopped: %v", err)
	}
	_, err = c.GetSession(ctx, "plain")
	if StatusOf(err) != 503 || !strings.Contains(err.Error(), "nope") {
		t.Errorf("plain: %v", err)
	}
	_, err = c.GetSession(ctx, "html")
	if StatusOf(err) != 502 || strings.Contains(err.Error(), "<html>") {
		t.Errorf("html: %v; markup must not become the message", err)
	}
	_, err = c.GetSession(ctx, "other")
	if StatusOf(err) != 401 || err.Error() != "Unauthorized" {
		t.Errorf("empty 401: %v", err)
	}
	if StatusOf(errors.New("x")) != 0 || IsNotFound(nil) {
		t.Error("non-problem errors have a status")
	}
}

// A refusal's Retry-After reaches the error, in seconds or as a date,
// so a caller can wait as long as the server asked.
//
// @decision(DL-133)
func TestARefusalCarriesItsRetryAfter(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", r.PathValue("ref"))
			problem(w, 429, "Too Many Requests", "")
		},
	})
	var e *Error
	if _, err := c.GetSession(context.Background(), "7"); !errors.As(err, &e) || e.RetryAfter != 7*time.Second {
		t.Errorf("Retry-After 7 = %v, want 7s on the error", err)
	}
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"30":                            30 * time.Second,
		"":                              0,
		"soon":                          0,
		"-5":                            0,
		"Thu, 12 Nov 2026 09:01:00 GMT": time.Minute,
		"Thu, 12 Nov 2026 08:00:00 GMT": 0,
	} {
		if got := retryAfter(v, now); got != want {
			t.Errorf("retryAfter(%q) = %s, want %s", v, got, want)
		}
	}
}

func TestNoServerIsAnError(t *testing.T) {
	if _, err := New("", "t").Me(context.Background()); !errors.Is(err, ErrNoServer) {
		t.Errorf("err = %v", err)
	}
}

// TestTransportErrorsRedactCredentials: a server URL configured with
// userinfo must not echo it through a transport error.
func TestTransportErrorsRedactCredentials(t *testing.T) {
	c := New("http://user:secret@127.0.0.1:1", "t")
	_, err := c.Me(context.Background())
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the error carries the password: %v", err)
	}
}

// recorder collects what Follow reports.
type recorder struct {
	steps  []string
	notes  []string
	ready  *server.ReadyEvent
	failed *server.ErrorEvent
}

func (r *recorder) Step(ev server.StepEvent)    { r.steps = append(r.steps, ev.Step+":"+ev.State) }
func (r *recorder) Note(ev server.NoteEvent)    { r.notes = append(r.notes, ev.Message) }
func (r *recorder) Ready(ev server.ReadyEvent)  { r.ready = &ev }
func (r *recorder) Failed(ev server.ErrorEvent) { r.failed = &ev }

func sse(w http.ResponseWriter, name string, data any) {
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// TestFollowStreamsToReady is the happy path: steps, a note, ready.
//
// @decision(DL-002) @decision(DL-005)
func TestFollowStreamsToReady(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}/events": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept") != "text/event-stream" {
				problem(w, 406, "want a stream", "")
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			sse(w, "step", server.StepEvent{Step: "preparing tokens", State: "started"})
			sse(w, "step", server.StepEvent{Step: "preparing tokens", State: "done"})
			sse(w, "note", server.NoteEvent{Message: "heads up"})
			sse(w, "future", map[string]int{"x": 1})
			sse(w, "ready", server.ReadyEvent{Session: server.SessionView{ID: "1", State: store.StateRunning}})
			sse(w, "step", server.StepEvent{Step: "after", State: "started"})
		},
	})
	var rec recorder
	if err := c.Follow(context.Background(), "1", &rec, FollowOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.steps, " ") != "preparing tokens:started preparing tokens:done" {
		t.Errorf("steps = %v; nothing after ready must be delivered", rec.steps)
	}
	if len(rec.notes) != 1 || rec.notes[0] != "heads up" {
		t.Errorf("notes = %v", rec.notes)
	}
	if rec.ready == nil || rec.ready.Session.ID != "1" {
		t.Errorf("ready = %+v", rec.ready)
	}
}

func TestFollowReportsAFailedLaunch(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}/events": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			sse(w, "step", server.StepEvent{Step: "starting sandbox", State: "failed"})
			sse(w, "error", server.ErrorEvent{Problem: server.ProblemView{Title: "Launch failed", Status: 502, Detail: "no capacity"}})
		},
	})
	var rec recorder
	err := c.Follow(context.Background(), "1", &rec, FollowOptions{})
	if !errors.Is(err, ErrLaunchFailed) {
		t.Fatalf("err = %v", err)
	}
	if rec.failed == nil || rec.failed.Problem.Detail != "no capacity" {
		t.Errorf("failed = %+v", rec.failed)
	}
}

// TestFollowRetriesOnceThenPolls: a stream that drops is reopened from
// the beginning; a second drop falls back to polling the session.
//
// @decision(DL-002)
func TestFollowRetriesOnceThenPolls(t *testing.T) {
	var streams, gets atomic.Int32
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}/events": func(w http.ResponseWriter, _ *http.Request) {
			streams.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			sse(w, "step", server.StepEvent{Step: "preparing tokens", State: "started"})
			// Drop the connection mid-stream.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("no hijacker")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		},
		"GET /api/v1/sessions/{ref}": func(w http.ResponseWriter, _ *http.Request) {
			state := store.StateStarting
			if gets.Add(1) >= 3 {
				state = store.StateRunning
			}
			_ = json.NewEncoder(w).Encode(server.SessionView{ID: "1", State: state})
		},
	})
	var rec recorder
	err := c.Follow(context.Background(), "1", &rec, FollowOptions{PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if streams.Load() != 2 {
		t.Errorf("the stream was opened %d times, want 2", streams.Load())
	}
	if gets.Load() < 3 {
		t.Errorf("polled %d times", gets.Load())
	}
	if rec.ready == nil || rec.ready.Session.State != store.StateRunning {
		t.Errorf("ready = %+v", rec.ready)
	}
	// The observer is told why it stopped seeing steps.
	if len(rec.notes) == 0 || !strings.Contains(rec.notes[0], "polling") {
		t.Errorf("notes = %v", rec.notes)
	}
	// Steps from the replayed stream arrive twice; the view tolerates
	// that, and a plain caller sees the replay for what it is.
	if len(rec.steps) != 2 {
		t.Errorf("steps = %v", rec.steps)
	}
}

func TestFollowPollingReportsAFailedSession(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}/events": func(w http.ResponseWriter, _ *http.Request) {
			hj := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		},
		"GET /api/v1/sessions/{ref}": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(server.SessionView{ID: "1", State: store.StateFailed, Error: "boom"})
		},
	})
	var rec recorder
	err := c.Follow(context.Background(), "1", &rec, FollowOptions{PollInterval: time.Millisecond})
	if !errors.Is(err, ErrLaunchFailed) || rec.failed == nil || rec.failed.Problem.Detail != "boom" {
		t.Errorf("err = %v, failed = %+v", err, rec.failed)
	}
}

// TestFollowDoesNotRetryAProblem: a 404 or 401 on the stream is the
// server's answer, not a broken connection.
func TestFollowDoesNotRetryAProblem(t *testing.T) {
	var streams atomic.Int32
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}/events": func(w http.ResponseWriter, _ *http.Request) {
			streams.Add(1)
			problem(w, 404, "no such session", "")
		},
	})
	err := c.Follow(context.Background(), "1", &recorder{}, FollowOptions{})
	if !IsNotFound(err) || streams.Load() != 1 {
		t.Errorf("err = %v, streams = %d", err, streams.Load())
	}
}

func TestFollowStopsWithItsContext(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}/events": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			sse(w, "step", server.StepEvent{Step: "starting sandbox", State: "started"})
			<-r.Context().Done()
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Follow(ctx, "1", &recorder{}, FollowOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

func TestFollowRejectsAMalformedEvent(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}/events": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: ready\ndata: {not json\n\n")
		},
	})
	err := c.Follow(context.Background(), "1", &recorder{}, FollowOptions{StreamAttempts: 1, PollInterval: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "bad ready event") {
		t.Errorf("err = %v", err)
	}
}

// TestFollowSkipsAMalformedProgressEvent: a step that cannot be decoded
// is cosmetic; the launch is decided by the terminal event.
func TestFollowSkipsAMalformedProgressEvent(t *testing.T) {
	var streams atomic.Int32
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}/events": func(w http.ResponseWriter, _ *http.Request) {
			streams.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: step\ndata: {not json\n\nevent: note\ndata: 42\n\n")
			sse(w, "ready", server.ReadyEvent{Session: server.SessionView{ID: "1", State: store.StateRunning}})
		},
	})
	var rec recorder
	if err := c.Follow(context.Background(), "1", &rec, FollowOptions{}); err != nil {
		t.Fatal(err)
	}
	if streams.Load() != 1 || rec.ready == nil || len(rec.steps) != 0 || len(rec.notes) != 0 {
		t.Errorf("streams = %d, ready = %v, steps = %v, notes = %v", streams.Load(), rec.ready, rec.steps, rec.notes)
	}
}

// TestFollowBoundsAnEvent: an event that never ends must not grow the
// client without limit.
func TestFollowBoundsAnEvent(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"GET /api/v1/sessions/{ref}/events": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: step\n")
			line := "data: " + strings.Repeat("x", 64<<10) + "\n"
			for i := 0; i < 64; i++ {
				if _, err := fmt.Fprint(w, line); err != nil {
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			<-r.Context().Done()
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Follow(ctx, "1", &recorder{}, FollowOptions{StreamAttempts: 1, PollInterval: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v", err)
	}
}

// The job methods: the paths they call, the state query they build, and
// the ids they escape.
//
// @decision(DL-045)
func TestJobMethodsCallTheRightPaths(t *testing.T) {
	var listQuery, method, path string
	c := stub(t, map[string]http.HandlerFunc{
		"POST /api/v1/jobs": func(w http.ResponseWriter, r *http.Request) {
			var req server.JobRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req.Goal != "do the thing" || req.Kind != store.JobKindPR {
				t.Errorf("the request body lost the goal or the kind: %+v", req)
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(server.JobView{ID: "01JOB", State: store.JobQueued})
		},
		"GET /api/v1/jobs": func(w http.ResponseWriter, r *http.Request) {
			listQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []server.JobView{{ID: "01JOB", State: store.JobRunning}},
			})
		},
		"GET /api/v1/jobs/{id}": func(w http.ResponseWriter, r *http.Request) {
			method, path = r.Method, r.PathValue("id")
			_ = json.NewEncoder(w).Encode(server.JobView{ID: r.PathValue("id")})
		},
		"DELETE /api/v1/jobs/{id}": func(w http.ResponseWriter, r *http.Request) {
			method, path = r.Method, r.PathValue("id")
			w.WriteHeader(http.StatusNoContent)
		},
	})
	ctx := context.Background()

	job, err := c.CreateJob(ctx, server.JobRequest{Goal: "do the thing", Kind: store.JobKindPR})
	if err != nil || job.ID != "01JOB" {
		t.Fatalf("CreateJob = %+v, %v", job, err)
	}

	// Every state joins one comma-separated parameter, which is what
	// huma parses; one parameter per state would silently drop all but
	// the first.
	jobs, err := c.ListJobs(ctx, Page{States: []string{"queued", "running"}})
	if err != nil || len(jobs.Jobs) != 1 {
		t.Fatalf("ListJobs = %+v, %v", jobs, err)
	}
	// One comma-separated parameter: the server keeps only the first of
	// a repeated one, so the repeated form would filter on `queued`
	// alone and look like an empty result rather than a client bug.
	if listQuery != "state=queued%2Crunning" {
		t.Errorf("the state filter went out as %q, want one comma-separated parameter", listQuery)
	}
	// No states means no query at all, not an empty one.
	if _, err := c.ListJobs(ctx, Page{}); err != nil {
		t.Fatal(err)
	}
	if listQuery != "" {
		t.Errorf("an unfiltered listing sent %q", listQuery)
	}
	// A page names its size and cursor beside the states.
	//
	// @decision(DL-012)
	if _, err := c.ListJobs(ctx, Page{States: []string{"failed"}, Limit: 5, Before: "01OLD"}); err != nil {
		t.Fatal(err)
	}
	if listQuery != "before=01OLD&limit=5&state=failed" {
		t.Errorf("a page went out as %q", listQuery)
	}

	if _, err := c.GetJob(ctx, "01JOB"); err != nil || method != http.MethodGet || path != "01JOB" {
		t.Errorf("GetJob sent %s %q: %v", method, path, err)
	}
	if err := c.CancelJob(ctx, "01JOB"); err != nil || method != http.MethodDelete || path != "01JOB" {
		t.Errorf("CancelJob sent %s %q: %v", method, path, err)
	}
	// Path escaping is deliberately not asserted here. net/http
	// normalises the URL before it goes out, so a request built with an
	// unescaped id arrives at the same route as one built with an
	// escaped one and a test cannot tell them apart.
}

// Steer POSTs to the session's own messages route, the same address the
// GET reads from. Nothing else in the tree calls this method, so a
// wrong path here would otherwise surface only at its first caller.
//
// @decision(DL-019)
func TestSteerPostsToTheSessionsMessages(t *testing.T) {
	var sent server.SteerRequest
	var ref string
	c := stub(t, map[string]http.HandlerFunc{
		"POST /api/v1/sessions/{ref}/messages": func(w http.ResponseWriter, r *http.Request) {
			ref = r.PathValue("ref")
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(server.SteeredMessage{
				ID: "msg_echo", Conversation: "ses_1", Delivery: "steer"})
		},
	})

	out, err := c.Steer(context.Background(), "alpha",
		server.SteerRequest{Conversation: "ses_1", Text: "stop and summarise", ID: "msg_mine"})
	if err != nil {
		t.Fatal(err)
	}
	if ref != "alpha" {
		t.Errorf("the session went out as %q", ref)
	}
	if sent.Text != "stop and summarise" || sent.Conversation != "ses_1" || sent.ID != "msg_mine" {
		t.Errorf("the request body lost something: %+v", sent)
	}
	if out.ID != "msg_echo" {
		t.Errorf("the answer lost the message id: %+v", out)
	}
}

// A refusal comes back as a *client.Error with its status, which is
// what the CLI switches on to explain a 501.
//
// @decision(DL-045) @decision(DL-046)
func TestJobRefusalsCarryTheirStatus(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		"POST /api/v1/jobs": func(w http.ResponseWriter, _ *http.Request) {
			problem(w, http.StatusNotImplemented, "autonomous jobs need a hosted deployment", "")
		},
	})
	_, err := c.CreateJob(context.Background(), server.JobRequest{Goal: "g", Kind: store.JobKindPR})
	if err == nil {
		t.Fatal("a 501 was not reported as an error")
	}
	if got := StatusOf(err); got != http.StatusNotImplemented {
		t.Errorf("StatusOf = %d, want 501", got)
	}
	if !strings.Contains(err.Error(), "hosted") {
		t.Errorf("the server's explanation was lost: %v", err)
	}
}

// The pin read sends no credential and asks outside the API prefix: a
// client wants this before it has a token, so a read that rode on
// newRequest would answer 401 instead.
//
// @decision(DL-035)
func TestThePinIsReadWithoutACredential(t *testing.T) {
	var authorization string
	var asked string
	c := stub(t, map[string]http.HandlerFunc{
		server.OpenCodePinPath: func(w http.ResponseWriter, r *http.Request) {
			authorization, asked = r.Header.Get("Authorization"), r.URL.Path
			_ = json.NewEncoder(w).Encode(server.OpenCodePin{Scheme: server.PinScheme, OpenCode: "beta-19151"})
		},
	})
	pin, err := c.OpenCodePin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pin.OpenCode != "beta-19151" {
		t.Errorf("pin = %+v", pin)
	}
	if authorization != "" {
		t.Error("the pin read sent a credential; it must work before the caller has one")
	}
	if asked != server.OpenCodePinPath {
		t.Errorf("asked %q, want %q — the API prefix would put it behind authentication", asked, server.OpenCodePinPath)
	}
}

// A scheme this build does not know is not an answer; the caller falls
// back to its own configuration instead.
//
// @decision(DL-035)
func TestAPinInAnUnknownSchemeIsRefused(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{
		server.OpenCodePinPath: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"scheme": server.PinScheme + 7, "opencode": "beta-99999",
			})
		},
	})
	pin, err := c.OpenCodePin(context.Background())
	if err == nil {
		t.Fatal("a pin in an unknown scheme was accepted")
	}
	if pin.OpenCode != "" {
		t.Errorf("pin = %+v; a refused answer must carry no version", pin)
	}
}

// A server with no pin endpoint is not an error the caller must handle
// specially — it is simply a failure, and the caller falls back.
//
// @decision(DL-035)
func TestAServerWithoutAPinAnswersNotFound(t *testing.T) {
	c := stub(t, map[string]http.HandlerFunc{})
	if _, err := c.OpenCodePin(context.Background()); !IsNotFound(err) {
		t.Errorf("err = %v, want a 404 an older server would give", err)
	}
}
