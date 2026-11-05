package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

func principalCtx(p auth.Principal) context.Context {
	return auth.WithPrincipal(context.Background(), p)
}

// The state stream carries Service.List's output for this caller, so a
// user sees their own sessions and an admin the org's — authorisation is
// inherited rather than written again.
//
// @decision(DL-018)
func TestTheStateStreamIsScopedToItsCaller(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	for _, s := range []store.Session{
		{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", OrgID: auth.LocalID, OwnerID: auth.LocalID, Name: "mine", State: store.StateRunning},
		{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", OrgID: auth.LocalID, OwnerID: "someone-else", Name: "theirs", State: store.StateRunning},
	} {
		if err := f.store.Sessions().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	rec := &recorder{}
	sctx, cancel := context.WithCancel(principalCtx(auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}))
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	f.srv.streamSessions(sctx, nil, rec)

	sent := rec.snapshot()
	if len(sent) == 0 {
		t.Fatal("the stream sent nothing")
	}
	ev, ok := sent[0].(SessionsEvent)
	if !ok {
		t.Fatalf("first event is %T, want SessionsEvent", sent[0])
	}
	if len(ev.Sessions) != 1 || ev.Sessions[0].Name != "mine" {
		t.Errorf("a user saw %+v, want only their own", ev.Sessions)
	}
}

// The lifetime bound ends the stream as an orderly close, not an error:
// the client reopens and says nothing to the user.
//
// @decision(DL-018) @decision(DL-007)
func TestTheStateStreamEndsOnItsLifetimeBound(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.streamLifetime = 30 * time.Millisecond
	f.srv.heartbeat = time.Hour

	rec := &recorder{}
	f.srv.streamSessions(principalCtx(auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}), nil, rec)

	sent := rec.snapshot()
	if len(sent) == 0 {
		t.Fatal("nothing was sent")
	}
	end, ok := sent[len(sent)-1].(EndEvent)
	if !ok {
		t.Fatalf("the stream ended with %T, want EndEvent", sent[len(sent)-1])
	}
	if end.Reason != "lifetime" {
		t.Errorf("reason = %q", end.Reason)
	}
}

// An idle stream sends comments, because huma sends nothing of its own
// and a silent connection looks dead to a proxy.
//
// @decision(DL-018)
func TestAnIdleStreamHeartbeats(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.streamLifetime = 120 * time.Millisecond
	f.srv.heartbeat = 10 * time.Millisecond

	rec := &recorder{}
	f.srv.streamSessions(principalCtx(auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}), nil, rec)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.comments == 0 {
		t.Error("an idle stream sent no keepalive")
	}
}

// A store the stream cannot read is reported, not rendered as an empty
// session list.
//
// @decision(DL-018)
func TestTheStateStreamReportsAFeedItCannotOpen(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.feed = pollFeed{interval: time.Millisecond, list: func(context.Context, auth.Principal, int) ([]store.Session, error) {
		return nil, errors.New("store down")
	}}
	rec := &recorder{}
	f.srv.streamSessions(principalCtx(auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}), nil, rec)
	sent := rec.snapshot()
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want one problem", len(sent))
	}
	if _, ok := sent[0].(ErrorEvent); !ok {
		t.Errorf("sent %T, want ErrorEvent", sent[0])
	}
}

// A session someone does not own is not theirs to watch, and the answer
// is the 404 the launch stream already gives rather than a 403 that
// confirms it exists.
//
// @decision(DL-018) @decision(DL-006)
func TestWatchingASessionYouDoNotOwnIsANotFound(t *testing.T) {
	f := newAPIFixture(t)
	if err := f.store.Sessions().Create(context.Background(), store.Session{
		ID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", OrgID: auth.LocalID, OwnerID: "someone-else",
		Name: "theirs", State: store.StateRunning,
	}); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	ctx := principalCtx(auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID})
	f.srv.streamLog(ctx, &logInput{Ref: "theirs"}, rec)

	sent := rec.snapshot()
	if len(sent) != 1 {
		t.Fatalf("sent %d events", len(sent))
	}
	ev, ok := sent[0].(ErrorEvent)
	if !ok {
		t.Fatalf("sent %T, want ErrorEvent", sent[0])
	}
	if ev.Problem.Status != 404 {
		t.Errorf("status = %d, want 404", ev.Problem.Status)
	}
}

// The log is the store's, replayed from wherever the client left off:
// every frame carries its position, and a reopen with that position
// resumes rather than repeating.
//
// @decision(DL-018)
func TestTheLogReplaysFromAPositionAndCarriesIt(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.streamLifetime = time.Hour
	f.srv.heartbeat = time.Hour
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	var written []store.Event
	for seq := int64(1); seq <= 3; seq++ {
		written = append(written, seedEvent(t, f, store.Event{
			OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID, Source: store.SourceOpenCode,
			Seq: seq, Name: "session.step.started", Text: "step", At: recordedAt,
		}))
	}

	sent := runLog(t, f, &logInput{Ref: "mine", After: written[0].Pos})
	var logs []LogEvent
	for _, ev := range sent {
		if l, ok := ev.(LogEvent); ok {
			logs = append(logs, l)
		}
	}
	if len(logs) != 2 {
		t.Fatalf("sent %d log events, want the two after the cursor: %+v", len(logs), sent)
	}
	if logs[0].Pos != written[1].Pos || logs[1].Pos != written[2].Pos {
		t.Errorf("positions = %d, %d, want %d, %d", logs[0].Pos, logs[1].Pos, written[1].Pos, written[2].Pos)
	}
	// The session a client sees is OpenCode's, which is what the
	// transcript's conversations are keyed by.
	if logs[0].Session != "ses_1" {
		t.Errorf("session = %q, want the OpenCode id", logs[0].Session)
	}
	if !logs[0].At.Equal(recordedAt) {
		t.Errorf("at = %s, want the recorded time, not the read time", logs[0].At)
	}
}

// spov's own markers ride the same stream, so the page draws the seam
// where the recorder was not listening rather than where the browser
// was not. A marker belongs to the session, not to a conversation.
//
// @decision(DL-018) @decision(DL-017)
func TestAMarkerIsAnOrdinaryFrameOnTheLog(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.streamLifetime = time.Hour
	f.srv.heartbeat = time.Hour
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
	seedEvent(t, f, store.Event{
		OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerGap, Text: "recording resumed", At: recordedAt,
	})

	sent := runLog(t, f, &logInput{Ref: "mine"})
	if len(sent) == 0 {
		t.Fatal("the stream sent nothing")
	}
	ev, ok := sent[0].(LogEvent)
	if !ok {
		t.Fatalf("first event is %T, want LogEvent", sent[0])
	}
	if ev.Name != MarkerGap || ev.Text != "recording resumed" {
		t.Errorf("marker = %+v", ev)
	}
	if ev.Session != "" {
		t.Errorf("session = %q, want none: a marker belongs to no conversation", ev.Session)
	}
}

// A session that can no longer produce events ends the stream once its
// rows are exhausted.
//
// @decision(DL-018)
func TestAFinishedSessionsLogEndsRatherThanBeingReopened(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.streamLifetime = time.Hour
	f.srv.heartbeat = time.Hour
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "gone", store.StateStopped)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	seedEvent(t, f, store.Event{
		OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID, Source: store.SourceOpenCode,
		Seq: 1, Name: "session.step.started", At: recordedAt,
	})

	sent := runLog(t, f, &logInput{Ref: sess.ID})
	if len(sent) != 2 {
		t.Fatalf("sent %d events, want the row and the end: %+v", len(sent), sent)
	}
	end, ok := sent[1].(EndEvent)
	if !ok {
		t.Fatalf("ended with %T, want EndEvent", sent[1])
	}
	if end.Reason != "stopped" {
		t.Errorf("reason = %q, want stopped", end.Reason)
	}
}

// A store that cannot be read is reported before the stream opens,
// rather than looking like a session that has said nothing.
//
// @decision(DL-018)
func TestTheLogReportsAStoreItCannotRead(t *testing.T) {
	f := newAPIFixture(t)
	seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
	f.srv.events = pollEvents{interval: time.Millisecond, list: func(context.Context, string, string, int64, int) ([]store.Event, error) {
		return nil, errors.New("store down")
	}}
	rec := &recorder{}
	f.srv.streamLog(principalCtx(auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}), &logInput{Ref: "mine"}, rec)
	sent := rec.snapshot()
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want one problem", len(sent))
	}
	if _, ok := sent[0].(ErrorEvent); !ok {
		t.Errorf("sent %T, want ErrorEvent", sent[0])
	}
}

// runLog reads one session's log until the handler returns, which it
// does on its own once a finished session is caught up and otherwise on
// the deadline below.
func runLog(t *testing.T, f *apiFixture, in *logInput) []any {
	t.Helper()
	// The real cadence is two seconds, which is a real test's whole
	// budget; the interval is the only thing shortened.
	f.srv.events = pollEvents{interval: time.Millisecond, page: LogPageSize, warn: func(string) {},
		list: func(ctx context.Context, orgID, sessionID string, after int64, limit int) ([]store.Event, error) {
			return f.store.Conversations().ListEvents(ctx, orgID, sessionID, after, limit)
		}}
	rec := &recorder{}
	ctx, cancel := context.WithTimeout(principalCtx(auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.srv.streamLog(ctx, in, rec)
	}()
	// A running session's stream does not end by itself: it is read
	// until it has caught up, then cancelled.
	waitFor(t, "the log to send something", func() bool { return len(rec.snapshot()) > 0 })
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		cancel()
		<-done
	}
	return rec.snapshot()
}

// A session named "events" is shadowed by the state stream's literal
// route at that path, and is still reachable by ID.
//
// @decision(DL-018)
func TestTheStateStreamPathShadowsASessionNamedEvents(t *testing.T) {
	f := newAPIFixture(t)
	// Bounded, because the request below opens a real stream and the
	// fixture's client reads the body to completion.
	f.srv.streamLifetime = 50 * time.Millisecond
	f.srv.heartbeat = time.Hour
	id := f.create(repoReq("events"))
	f.events(id)

	// By name: the literal route wins, so this is the stream, not the
	// session — and a stream is not JSON with an id.
	resp, body := f.do(http.MethodGet, "/api/v1/sessions/events", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want the stream: the literal route must win", ct)
	}
	// By ID, the session is still there.
	resp, body = f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("by id = %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), id) {
		t.Errorf("by id returned %s", body)
	}
}

// Reading the transcript is gated by Service.Get: another org's session,
// and one the caller neither owns nor administers, is 404 rather than 403.
//
// @decision(DL-018) @decision(DL-006)
func TestReadingSomeoneElsesTranscriptIsANotFound(t *testing.T) {
	f := newAPIFixture(t)
	elsewhere := store.Session{
		ID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", OrgID: "another-org", OwnerID: "someone-else",
		Name: "theirs", State: store.StateRunning, SandboxID: "sb-1",
	}
	if err := f.store.Sessions().Create(context.Background(), elsewhere); err != nil {
		t.Fatal(err)
	}
	resp, body := f.do(http.MethodGet, "/api/v1/sessions/"+elsewhere.ID+"/messages", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET = %d, want 404: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "forbidden") {
		t.Errorf("the answer confirms the session exists: %s", body)
	}

	// And within one org, a user who does not own it.
	mine := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
	if _, err := f.srv.svc.Get(context.Background(), auth.Principal{OrgID: auth.LocalID, UserID: "nobody"}, mine.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a non-owner resolved the session: %v", err)
	}
}

// The rows move while the session runs, so the transcript is never a
// cacheable representation of the session.
//
// @decision(DL-018)
func TestTheTranscriptIsNotCacheable(t *testing.T) {
	f := newAPIFixture(t)
	conv := seedConversation(t, f, runningSession(t, f), "ses_1", "one")
	seedMessage(t, f, conv, "msg_a", "assistant", "hi", recordedAt)
	resp, body := f.do(http.MethodGet, "/api/v1/sessions/alpha/messages", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if !strings.Contains(string(body), "hi") {
		t.Errorf("body = %s", body)
	}
}

// A stopped session still has its transcript: the rows outlive the
// sandbox, which is most of the point of recording them.
//
// @decision(DL-018)
func TestAStoppedSessionStillHasItsTranscript(t *testing.T) {
	f := newAPIFixture(t)
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "gone", store.StateStopped)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	seedMessage(t, f, conv, "msg_a", "assistant", "what I said", recordedAt)

	// By id: a stopped session is no longer resolvable by name.
	resp, body := f.do(http.MethodGet, "/api/v1/sessions/"+sess.ID+"/messages", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "what I said") {
		t.Errorf("body = %s", body)
	}
}

// TestNoReadPathTouchesTheSandbox drives the list, /messages and /log
// against a sandbox that fails the test if it is contacted at all. If a
// read was added to one of these paths, the read is the bug.
//
// @decision(DL-018)
func TestNoReadPathTouchesTheSandbox(t *testing.T) {
	var reached atomic.Int32
	sb := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		t.Errorf("a read path contacted the sandbox: %s", r.URL)
	}))
	defer sb.Close()

	f := newAPIFixtureWith(t, func(d *Deps) { d.Provider.(*sandbox.Fake).ConnectBaseURL = sb.URL })
	f.srv.streamLifetime = 100 * time.Millisecond
	f.srv.heartbeat = time.Hour
	sess := runningSession(t, f)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	seedMessage(t, f, conv, "msg_a", "assistant", "hi", recordedAt)
	seedEvent(t, f, store.Event{
		OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID, Source: store.SourceOpenCode,
		Seq: 1, Name: "session.step.started", At: recordedAt,
	})

	for _, path := range []string{"/api/v1/sessions", "/api/v1/sessions/alpha/messages", "/api/v1/sessions/alpha/log"} {
		if resp, body := f.do(http.MethodGet, path, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, body)
		}
	}
	if reached.Load() != 0 {
		t.Errorf("the sandbox was read %d times", reached.Load())
	}
}

// The list carries each session's last message, filled by one query for
// the whole page.
//
// @decision(DL-018)
func TestTheListCarriesEachSessionsLastMessage(t *testing.T) {
	f := newAPIFixture(t)
	talky := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "talky", store.StateRunning)
	seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAW", "quiet", store.StateRunning)
	conv := seedConversation(t, f, talky, "ses_1", "one")
	seedMessage(t, f, conv, "msg_a", "user", "first", recordedAt)
	seedMessage(t, f, conv, "msg_b", "assistant", "the newest thing said", recordedAt.Add(time.Minute))

	resp, body := f.do(http.MethodGet, "/api/v1/sessions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, body)
	}
	var got struct {
		Sessions []SessionView `json:"sessions"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("listed %d sessions", len(got.Sessions))
	}
	byName := map[string]SessionView{}
	for _, v := range got.Sessions {
		byName[v.Name] = v
	}
	last := byName["talky"].LastMessage
	if last == nil {
		t.Fatal("the session with a conversation has no last message")
	}
	if last.Text != "the newest thing said" || last.Role != "assistant" {
		t.Errorf("last message = %+v", last)
	}
	// A session nobody has recorded carries none, rather than an empty
	// one a client would have to render.
	if byName["quiet"].LastMessage != nil {
		t.Errorf("a session with no conversation carried %+v", byName["quiet"].LastMessage)
	}

	// The state stream is the same projection, so an open list updates
	// as the recorder writes.
	rec := &recorder{}
	ctx, cancel := context.WithCancel(principalCtx(auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID, Admin: true}))
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	f.srv.streamSessions(ctx, nil, rec)
	sent := rec.snapshot()
	if len(sent) == 0 {
		t.Fatal("the stream sent nothing")
	}
	ev := sent[0].(SessionsEvent)
	for _, v := range ev.Sessions {
		if v.Name == "talky" && (v.LastMessage == nil || v.LastMessage.Text != "the newest thing said") {
			t.Errorf("the stream's view carried %+v", v.LastMessage)
		}
	}
}

// sessionEnded is what ends a stream, so a read that failed must not be
// mistaken for a session that finished — and a session deleted while
// somebody watched it must be.
//
// @decision(DL-018)
func TestWhatCountsAsAFinishedSession(t *testing.T) {
	f := newAPIFixture(t)
	p := auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}
	ctx := context.Background()
	running := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
	stopped := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAW", "gone", store.StateStopped)

	if f.srv.sessionEnded(ctx, p, running.ID) {
		t.Error("a running session was reported finished")
	}
	if !f.srv.sessionEnded(ctx, p, stopped.ID) {
		t.Error("a stopped session was not reported finished")
	}
	// Deleted while being watched.
	if !f.srv.sessionEnded(ctx, p, "01ARZ3NDEKTSV4RRFFQ69G5FAX") {
		t.Error("a session that is gone was not reported finished")
	}
	// A store that cannot answer says nothing about the session: the
	// stream keeps its lifetime bound instead of ending on a blip, and
	// says so once.
	var warned int
	f.srv.deps.Warn = func(string) { warned++ }
	f.srv.svc.deps.Store = downStore{Store: f.store, err: errors.New("the database is down")}
	if f.srv.sessionEnded(ctx, p, running.ID) {
		t.Error("a failed read ended the stream")
	}
	if warned != 1 {
		t.Errorf("a failed read warned %d times, want one", warned)
	}
}

// downStore is a store whose sessions cannot be read.
type downStore struct {
	store.Store
	err error
}

func (d downStore) Sessions() store.SessionStore { return downSessions{d.Store.Sessions(), d.err} }

type downSessions struct {
	store.SessionStore
	err error
}

func (d downSessions) Get(context.Context, string, string) (store.Session, error) {
	return store.Session{}, d.err
}

// The list says whether a job is driving a session, so a reader can
// tell an autonomous run from a session someone opened by hand without
// clicking into each one.
//
// @decision(DL-060)
func TestTheListSaysWhichSessionsAreJobs(t *testing.T) {
	f := newAPIFixture(t)
	driven := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "driven", store.StateRunning)
	seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAW", "byhand", store.StateRunning)

	job := store.Job{
		ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID,
		SessionID: driven.ID, Kind: store.JobKindPR, State: store.JobQueued,
		Goal: "open a pull request", Unfinished: "a very long note on what is left",
	}
	if err := f.store.Jobs().Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	byName := listByName(t, f)
	got := byName["driven"].Job
	if got == nil {
		t.Fatal("the session driven by a job carries none")
	}
	if got.ID != job.ID || got.Kind != store.JobKindPR || got.State != store.JobQueued {
		t.Errorf("job = %+v, want the row's id, kind and state", got)
	}
	// A session nobody supervises carries none, rather than an empty
	// one a client would have to test the fields of.
	if byName["byhand"].Job != nil {
		t.Errorf("an ordinary session carried a job: %+v", byName["byhand"].Job)
	}

	// A session row carries the job's id, kind and state and nothing
	// else the job holds.
	raw := listRaw(t, f)
	if strings.Contains(raw, "a very long note on what is left") {
		t.Error("the listing carried the job's unfinished work; a list says what a row is, not what happened")
	}
	if strings.Contains(raw, "open a pull request") {
		t.Error("the listing carried the job's goal; a list says what a row is, not what happened")
	}
}

// TestTheListStampMovesWithAJobsState pins that the feed's stamp moves
// with a job's state and not with its cursor.
//
// @decision(DL-060)
func TestTheListStampMovesWithAJobsState(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "driven", store.StateRunning)
	job := store.Job{
		ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: auth.LocalID,
		SessionID: sess.ID, Kind: store.JobKindPR, State: store.JobQueued, Goal: "g",
	}
	if err := f.store.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	sessions, err := f.store.Sessions().List(ctx, auth.LocalID, store.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}

	before := f.srv.decorationStamp(ctx, p, sessions)
	job.State = store.JobProvisioning
	if err := f.store.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	after := f.srv.decorationStamp(ctx, p, sessions)
	if before == after {
		t.Fatal("the stamp did not move when the job's state did; the badge would go stale")
	}

	// A cursor advance changes nothing a reader can see, so it must not
	// wake every open list in the org.
	quiet := f.srv.decorationStamp(ctx, p, sessions)
	job.Cursor = 4242
	if err := f.store.Jobs().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if got := f.srv.decorationStamp(ctx, p, sessions); got != quiet {
		t.Error("the stamp moved on a cursor advance, which shows nothing")
	}
}

// listByName lists the sessions and keys them by name.
func listByName(t *testing.T, f *apiFixture) map[string]SessionView {
	t.Helper()
	var got struct {
		Sessions []SessionView `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(listRaw(t, f)), &got); err != nil {
		t.Fatal(err)
	}
	byName := map[string]SessionView{}
	for _, v := range got.Sessions {
		byName[v.Name] = v
	}
	return byName
}

// listRaw is the listing's body as it goes over the wire, for
// assertions about what is NOT in it.
func listRaw(t *testing.T, f *apiFixture) string {
	t.Helper()
	resp, body := f.do(http.MethodGet, "/api/v1/sessions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, body)
	}
	return string(body)
}

// The live stream and the stored transcript agree on who spoke. This
// drives a frame through the store and out as the browser receives it,
// since a role set in projectFrame alone would reach nobody.
//
// @decision(DL-058) @decision(DL-018) @decision(DL-045)
func TestTheLogSaysWhoSpoke(t *testing.T) {
	for _, tc := range []struct {
		name string
		item string
		want string
	}{
		{"a person typing", `{"type":"user","payload":{"text":"reply with ok"}}`, "user"},
		{"one of spov's own", `{"type":"synthetic","payload":{"text":"SPOV_LOOP_STOPPED {}"}}`, "synthetic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			f.srv.streamLifetime = time.Hour
			f.srv.heartbeat = time.Hour
			sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
			conv := seedConversation(t, f, sess, "ses_1", "one")
			raw := `{"id":"evt_1","created":1,"type":"session.inbox.enqueued",` +
				`"data":{"sessionID":"ses_1","item":` + tc.item + `},` +
				`"durable":{"aggregateID":"ses_1","seq":1,"version":1}}`
			seedEvent(t, f, store.Event{
				OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID,
				Source: store.SourceOpenCode, Seq: 1, Name: jobs.EventInboxEnqueued,
				Text: "x", At: recordedAt, Raw: []byte(raw),
			})

			var seen int
			for _, ev := range runLog(t, f, &logInput{Ref: "mine"}) {
				l, ok := ev.(LogEvent)
				if !ok || l.Name != jobs.EventInboxEnqueued {
					continue
				}
				seen++
				if l.Role != tc.want {
					t.Errorf("the log says role %q, want %q; the client would have to guess"+
						" from the event name, which cannot tell these two apart", l.Role, tc.want)
				}
			}
			if seen == 0 {
				t.Fatal("the inbox event never reached the log")
			}
		})
	}
}

// A row whose raw frame cannot be read carries no role, so the client
// falls back to its own guess rather than to a wrong speaker.
//
// @decision(DL-058)
func TestAnUnreadableFrameCarriesNoRole(t *testing.T) {
	for _, raw := range []string{"", "not json at all", `{"data":{}}`} {
		got := storedRole(store.Event{Name: jobs.EventInboxEnqueued, Raw: []byte(raw)})
		if got != "" {
			t.Errorf("storedRole(%q) = %q, want empty", raw, got)
		}
	}
	// And a name that is not the inbox is never parsed: it is the one
	// name where the client's fallback is wrong, and every other would
	// cost an unmarshal per row per watcher to learn nothing.
	full := `{"data":{"item":{"type":"synthetic"}}}`
	if got := storedRole(store.Event{Name: "session.text.ended", Raw: []byte(full)}); got != "" {
		t.Errorf("storedRole parsed a non-inbox event: %q", got)
	}
}

// A tool event names its call, and the input.started event its tool, so
// the page can join a call's events into one line. The frames are the
// measured shapes from a real job on OpenCode 2.0.22.
//
// @decision(DL-058)
func TestAToolEventNamesItsCall(t *testing.T) {
	const output = `"content":[{"type":"text","text":"total 15\n-rw-r--r-- 1 root root 3598 .envrc\n"}],` +
		`"metadata":{"status":"completed","truncated":false,"exit":0}`
	for _, tc := range []struct {
		name, event, raw string
		source           store.EventSource
		call, tool       string
	}{
		{"input started", "session.tool.input.started",
			`{"id":"evt_1","type":"session.tool.input.started","location":{"directory":"/workspace"},` +
				`"data":{"sessionID":"ses_1","assistantMessageID":"msg_1","id":"call_a","name":"shell"},` +
				`"durable":{"aggregateID":"ses_1","seq":7,"version":1}}`,
			store.SourceOpenCode, "call_a", "shell"},
		{"input ended", "session.tool.input.ended",
			`{"id":"evt_2","type":"session.tool.input.ended","data":{"sessionID":"ses_1","id":"call_a",` +
				`"text":"{\"command\":\"ls\"}"}}`,
			store.SourceOpenCode, "call_a", ""},
		{"success, id before a large output", "session.tool.success",
			`{"id":"evt_3","type":"session.tool.success","data":{"sessionID":"ses_1","id":"call_a",` + output + `}}`,
			store.SourceOpenCode, "call_a", ""},
		{"success, id after its output", "session.tool.success",
			`{"id":"evt_3","type":"session.tool.success","data":{` + output + `,"id":"call_a"}}`,
			store.SourceOpenCode, "call_a", ""},
		{"a name on any other tool event is not the tool", "session.tool.called",
			`{"id":"evt_4","data":{"id":"call_a","name":"not-a-tool"}}`,
			store.SourceOpenCode, "call_a", ""},
		{"the frame's own id is not the call", "session.tool.called",
			`{"id":"evt_5","data":{"sessionID":"ses_1"}}`,
			store.SourceOpenCode, "", ""},
		{"not a tool event", "session.text.ended",
			`{"id":"evt_6","data":{"id":"call_a","name":"shell"}}`,
			store.SourceOpenCode, "", ""},
		{"spov's own row is never parsed", "session.tool.called",
			`{"data":{"id":"call_a"}}`,
			store.SourceSpov, "", ""},
		{"unreadable", "session.tool.success", `{"data":{"id":`, store.SourceOpenCode, "", ""},
		{"not an object", "session.tool.success", `[1,2]`, store.SourceOpenCode, "", ""},
		{"a data that is not an object", "session.tool.success", `{"data":"call_a"}`, store.SourceOpenCode, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, tool := storedToolCall(store.Event{Name: tc.event, Source: tc.source, Raw: []byte(tc.raw)})
			if call != tc.call || tool != tc.tool {
				t.Errorf("storedToolCall = %q, %q; want %q, %q", call, tool, tc.call, tc.tool)
			}
		})
	}
}

// The stream carries the call and the tool, not just the helper.
//
// @decision(DL-058)
func TestTheLogCarriesTheToolCall(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.streamLifetime = time.Hour
	f.srv.heartbeat = time.Hour
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	seedEvent(t, f, store.Event{
		OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID,
		Source: store.SourceOpenCode, Seq: 1, Name: "session.tool.input.started", At: recordedAt,
		Raw: []byte(`{"type":"session.tool.input.started","data":{"sessionID":"ses_1","id":"call_a","name":"read"},` +
			`"durable":{"aggregateID":"ses_1","seq":1,"version":1}}`),
	})
	for _, ev := range runLog(t, f, &logInput{Ref: "mine"}) {
		if l, ok := ev.(LogEvent); ok && l.Name == "session.tool.input.started" {
			if l.Call != "call_a" || l.Tool != "read" {
				t.Errorf("the log carries call %q tool %q, want call_a read", l.Call, l.Tool)
			}
			return
		}
	}
	t.Fatal("the tool event never reached the log")
}
