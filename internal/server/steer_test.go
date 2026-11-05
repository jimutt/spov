package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// prompted is what the fake sandbox recorded about one request: a
// POST /prompt, or for a cancel a DELETE of an inbox item.
type prompted struct {
	method string
	path   string
	query  string
	body   map[string]any
}

type steerFix struct {
	svc  *Service
	sess store.Session
	job  store.Job
	p    auth.Principal
	fake *sandbox.Fake

	mu   sync.Mutex
	sent []prompted
}

func (f *steerFix) calls() []prompted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]prompted(nil), f.sent...)
}

// newSteerFixture gives a running session whose sandbox answers
// POST /prompt the way S-LS1 measured, plus `conversations` recorded
// conversation rows.
func newSteerFixture(t *testing.T, conversations ...string) *steerFix {
	t.Helper()
	return newSteerFixtureAt(t, store.JobRunning, conversations...)
}

// newSteerFixtureAt is newSteerFixture with the session's job left in a
// chosen state, for the rules that turn on it (DL-052).
func newSteerFixtureAt(t *testing.T, state store.JobState, conversations ...string) *steerFix {
	t.Helper()
	f := &steerFix{}
	svc, job, fake := jobSandbox(t, func(w http.ResponseWriter, r *http.Request) {
		record := func(body map[string]any) {
			f.mu.Lock()
			f.sent = append(f.sent, prompted{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: body})
			f.mu.Unlock()
		}
		switch {
		case r.Method == http.MethodDelete && (strings.Contains(r.URL.Path, "/inbox/") || strings.Contains(r.URL.Path, "/form/")):
			record(nil)
			w.WriteHeader(http.StatusNoContent)
			return
		// An answer, a created conversation and its model, as S-F1 and
		// the job path measured them.
		case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/reply") || strings.HasSuffix(r.URL.Path, "/model")):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			record(body)
			w.WriteHeader(http.StatusNoContent)
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/session"):
			record(nil)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"ses_created"}}`))
			return
		case r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/prompt"):
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		record(body)
		// The measured answer, echoing back the caller's own id.
		id, _ := body["id"].(string)
		if id == "" {
			id = "msg_minted_by_the_sandbox"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"` + id + `","sessionID":"ses","time":{"created":1},` +
			`"type":"user","payload":{"text":"x"},"delivery":"steer"}}`))
	})
	ctx := context.Background()
	sess, err := svc.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// A RUNNING job, which is the ordinary case for a session somebody
	// is talking to. seedJobSession leaves it `queued`, which DL-052
	// refuses — so a fixture left at the default would test the refusal
	// on every send.
	f.job = job
	f.svc = svc
	setJobState(t, svc, job, state)
	for _, id := range conversations {
		if _, err := svc.deps.Store.Conversations().UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(svc.deps.Now()), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.svc, f.sess, f.fake = svc, sess, fake
	f.p = auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	return f
}

// setJobState walks a job to a state through the machine's own edges,
// so a test cannot pin behaviour on a transition the store refuses.
//
// The route is found rather than written down: store.CanTransition is
// the machine, and a hand-written path goes stale the moment an edge
// moves.
func setJobState(t *testing.T, svc *Service, job store.Job, to store.JobState) {
	t.Helper()
	ctx := context.Background()
	cur, err := svc.deps.Store.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range routeTo(cur.State, to) {
		cur, err = svc.deps.Store.Jobs().Get(ctx, job.OrgID, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		from := cur.State
		cur.State = step
		if err := svc.deps.Store.Jobs().Update(ctx, cur); err != nil {
			t.Fatalf("moving the job %s -> %s: %v", from, step, err)
		}
	}
	got, err := svc.deps.Store.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != to {
		t.Fatalf("the job is %s, want %s: no route through the machine", got.State, to)
	}
}

// routeTo is the shortest path of legal transitions from one state to
// another, breadth-first over store.CanTransition.
func routeTo(from, to store.JobState) []store.JobState {
	if from == to {
		return nil
	}
	prev := map[store.JobState]store.JobState{from: from}
	queue := []store.JobState{from}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		for _, next := range store.JobStates() {
			if next == at || !store.CanTransition(at, next) {
				continue
			}
			if _, seen := prev[next]; seen {
				continue
			}
			prev[next] = at
			if next == to {
				var path []store.JobState
				for s := to; s != from; s = prev[s] {
					path = append([]store.JobState{s}, path...)
				}
				return path
			}
			queue = append(queue, next)
		}
	}
	return nil
}

// interactiveFixture is a running session with NO job: a sandbox
// somebody launched with `spov fly`, which is where queueing is
// offered.
func interactiveFixture(t *testing.T, f *steerFix, conversations ...string) store.Session {
	t.Helper()
	ctx := context.Background()
	sess := store.Session{
		ID: store.NewID(time.Now()), OrgID: f.sess.OrgID, OwnerID: f.sess.OwnerID,
		Name: "interactive-" + store.NewID(time.Now()), State: store.StateRunning,
		SandboxID: f.sess.SandboxID, Request: store.SessionRequest{Egress: "agent"},
	}
	if err := f.svc.deps.Store.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for _, id := range conversations {
		if _, err := f.svc.deps.Store.Conversations().UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(f.svc.deps.Now()), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return sess
}

// A message reaches the sandbox's own prompt endpoint, and what spov
// sends is explicit about both things S-LS1 measured a default for.
//
// @decision(DL-019)
func TestASendReachesTheSandbox(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	sent, err := f.svc.Steer(context.Background(), f.p, f.sess.ID,
		SteerRequest{Text: "  stop and summarise  ", ID: "msg_mine"})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("the sandbox saw %d prompts, want 1", len(calls))
	}
	got := calls[0]
	if !strings.HasSuffix(got.path, "/api/session/ses_1/prompt") {
		t.Errorf("path = %q, want the conversation's own prompt endpoint", got.path)
	}
	// Trimmed, like a job's goal: the untrimmed text would carry
	// unbounded surrounding whitespace past the bound and into the
	// sandbox.
	if got.body["text"] != "stop and summarise" {
		t.Errorf("text = %#v, want it trimmed", got.body["text"])
	}
	// resume: without it the item is enqueued and nothing runs.
	if got.body["resume"] != true {
		t.Errorf("resume = %#v, want true; the message would sit unread", got.body["resume"])
	}
	// delivery: an omitted one is steer, sent explicitly (DL-019).
	if got.body["delivery"] != DeliverySteer {
		t.Errorf("delivery = %#v, want %q", got.body["delivery"], DeliverySteer)
	}
	if got.body["id"] != "msg_mine" {
		t.Errorf("id = %#v, want the caller's own; it is the idempotency key", got.body["id"])
	}
	if sent.ID != "msg_mine" || sent.Conversation != "ses_1" || sent.Delivery != DeliverySteer {
		t.Errorf("sent = %+v", sent)
	}
}

// The conversation is escaped into the path: the sandbox chose the
// string, and an id carrying a `?` would otherwise truncate the path.
//
// @decision(DL-019) @decision(DL-018)
func TestTheConversationIsEscapedIntoThePath(t *testing.T) {
	const odd = "ses_1?x=1"
	f := newSteerFixture(t, odd)
	if _, err := f.svc.Steer(context.Background(), f.p, f.sess.ID,
		SteerRequest{Conversation: odd, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("the sandbox saw %d prompts, want 1", len(calls))
	}
	if calls[0].query != "" {
		t.Errorf("the sandbox saw the query %q: the id escaped the path segment it was"+
			" meant to be", calls[0].query)
	}
	if !strings.HasSuffix(calls[0].path, "/api/session/"+odd+"/prompt") {
		t.Errorf("the sandbox saw %q, want the whole id in one segment", calls[0].path)
	}
}

// A conversation is resolved against the session's own rows, so a
// caller cannot name another session's conversation.
//
// @decision(DL-019)
func TestAForeignConversationIsRefused(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	ctx := context.Background()
	// A real conversation, belonging to somebody else's session.
	other := seedJobSession(t, f.svc, store.StateRunning, "sb_other")
	if _, err := f.svc.deps.Store.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(f.svc.deps.Now()), OrgID: other.OrgID, SessionID: other.SessionID,
		OpenCodeID: "ses_theirs",
	}); err != nil {
		t.Fatal(err)
	}

	_, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Conversation: "ses_theirs", Text: "hello"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want an invalid request", err)
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("the sandbox saw %d prompts, want none: the check must happen before the send", n)
	}
	if mintsOf(f.fake) != 0 {
		t.Error("a refused conversation spent a mint")
	}
}

// What the caller sent is checked before anything else happens.
//
// @decision(DL-019)
func TestAMessageIsBounded(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"empty", ""},
		{"whitespace only", "   \n\t "},
		{"over the bound", strings.Repeat("x", MaxMessageBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSteerFixture(t, "ses_1")
			_, err := f.svc.Steer(context.Background(), f.p, f.sess.ID, SteerRequest{Text: tc.text})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want an invalid request", err)
			}
			if n := len(f.calls()); n != 0 {
				t.Errorf("the sandbox saw %d prompts, want none", n)
			}
		})
	}
	// The bound is on the TRIMMED text, which is what is sent: exactly
	// at the limit with whitespace around it is a message, not an
	// oversized one.
	f := newSteerFixture(t, "ses_1")
	at := "  " + strings.Repeat("x", MaxMessageBytes) + "  "
	if _, err := f.svc.Steer(context.Background(), f.p, f.sess.ID, SteerRequest{Text: at}); err != nil {
		t.Errorf("a message at the bound was refused: %v", err)
	}
}

// A client-minted id is checked here rather than discovered in the
// sandbox: it becomes a primary key there and the recorder's key here,
// so an unchecked one is a caller choosing what goes in both.
//
// @decision(DL-059) @decision(DL-019)
func TestAMintedIdIsChecked(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"not OpenCode's space", "ses_1"},
		{"no prefix at all", "hello"},
		{"too long", MessageIDPrefix + strings.Repeat("x", MaxMessageIDBytes)},
		{"a path separator", "msg_../../etc"},
		{"a space", "msg_one two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSteerFixture(t, "ses_1")
			_, err := f.svc.Steer(context.Background(), f.p, f.sess.ID,
				SteerRequest{Text: "hello", ID: tc.id})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want an invalid request", err)
			}
			if n := len(f.calls()); n != 0 {
				t.Errorf("the sandbox saw %d prompts, want none", n)
			}
		})
	}
}

// An unknown delivery is refused rather than passed through to find
// out. The enum is the whole of what the sandbox accepts (S-LS1).
//
// @decision(DL-019)
func TestDeliveryIsOneOfTwoThings(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	_, err := f.svc.Steer(context.Background(), f.p, f.sess.ID,
		SteerRequest{Text: "hello", Delivery: "later"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want an invalid request", err)
	}
	// Queueing is an INTERACTIVE-session thing (DL-052): on a job it is
	// refused, and that half is tested below.
	inter := interactiveFixture(t, f, "ses_i")
	if _, err := f.svc.Steer(context.Background(), f.p, inter.ID,
		SteerRequest{Text: "hello", Delivery: DeliveryQueue}); err != nil {
		t.Fatalf("queue was refused on an interactive session: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].body["delivery"] != DeliveryQueue {
		t.Errorf("the sandbox was sent %+v, want one queued delivery", calls)
	}
}

// The ceiling refuses before anything reaches the sandbox.
//
// @decision(DL-019) @decision(DL-030)
func TestTheCeilingRefusesBeforeTheSandboxIsTouched(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	ctx := context.Background()
	for i := range MessagesPerMinute {
		if _, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "hello"}); err != nil {
			t.Fatalf("message %d of %d was refused: %v", i+1, MessagesPerMinute, err)
		}
	}
	before := len(f.calls())
	_, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "one too many"})
	if !errors.Is(err, ErrTooManyMessages) {
		t.Fatalf("err = %v, want the steering ceiling", err)
	}
	if got := len(f.calls()); got != before {
		t.Errorf("the sandbox saw %d prompts after the refusal, want %d", got, before)
	}
	// And it is the STEERING ceiling, not the mint one: a reader told
	// their messages were limited must not be sent looking at
	// connection credentials.
	if errors.Is(err, ErrTooManyMints) {
		t.Error("the steering ceiling reports itself as the mint limit")
	}
}

// Steering fails before recording does, and the DURABLE mint rows are
// what make that true. The in-memory ceiling cannot: it is per replica
// while the mint limit is shared, so no value of it bounds the budget.
//
// @decision(DL-019) @decision(DL-030)
func TestSteeringFailsBeforeRecordingDoes(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	ctx := context.Background()
	now := f.svc.deps.Now()

	// Spent up to the floor: one more send could cost two mints and
	// still leave the recorder its share.
	spend := MintsPerMinute - RecordingMintReserve - MintsPerSteer
	for i := range spend {
		mintRow(t, f, now.Add(time.Duration(i)*time.Millisecond))
	}
	if _, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "hello"}); err != nil {
		t.Fatalf("a send with %d of %d mints spent was refused: %v", spend, MintsPerMinute, err)
	}

	// One past it. What is left must still cover the recorder's own
	// worst case inside the window.
	mintRow(t, f, now)
	_, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "hello"})
	if !errors.Is(err, ErrTooManyMessages) {
		t.Fatalf("err = %v, want the send refused: with %d mints spent, two more would"+
			" leave the recorder fewer than %d and its redial is what gets starved",
			err, spend+1, RecordingMintReserve)
	}
	if n := len(f.calls()); n != 1 {
		t.Errorf("the sandbox saw %d prompts, want 1: the budget is read before the send", n)
	}
}

// A send costs up to TWO mints, not one, and the reserve is sized for
// it: withConnection mints on a cache miss AND mints again when the
// sandbox refuses the credential it was given.
//
// @decision(DL-019) @decision(DL-030)
func TestTheReserveIsSizedForWhatASendCanCost(t *testing.T) {
	if MintsPerSteer < 2 {
		t.Errorf("MintsPerSteer is %d: withConnection mints on a miss and again on a"+
			" refusal, so a bound of one understates what a send can spend", MintsPerSteer)
	}
	if RecordingMintReserve <= 0 || RecordingMintReserve >= MintsPerMinute {
		t.Errorf("RecordingMintReserve (%d) leaves the recorder nothing or leaves steering"+
			" nothing, against a budget of %d", RecordingMintReserve, MintsPerMinute)
	}
	// The recorder's own worst case inside one window: RecorderBackoff
	// waits 5s, 10s, 20s and 40s, so the redials that BEGIN within a
	// minute are what the reserve has to cover.
	var elapsed, within time.Duration
	for _, w := range RecorderBackoff {
		elapsed += w
		if elapsed < time.Minute {
			within++
		}
	}
	if RecordingMintReserve < int(within) {
		t.Errorf("the reserve is %d but %d redials begin inside a minute, so the recorder"+
			" can still be refused one", RecordingMintReserve, int(within))
	}
}

// mintRow records one mint against the session, as Connect would.
func mintRow(t *testing.T, f *steerFix, at time.Time) {
	t.Helper()
	if err := f.svc.deps.Store.Mints().Create(context.Background(), store.Mint{
		ID: store.NewID(at), OrgID: f.sess.OrgID, SessionID: f.sess.ID,
		UserID: f.sess.OwnerID, Port: 4096, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// The window is a window: it lets go.
//
// @decision(DL-019)
func TestTheCeilingIsASlidingWindow(t *testing.T) {
	l := newMessageLimiter(2)
	now := recordedAt
	for i := range 2 {
		if !l.take("a", now) {
			t.Fatalf("send %d was refused inside a ceiling of 2", i+1)
		}
	}
	if l.take("a", now) {
		t.Fatal("a third was admitted inside the window")
	}
	// Another session is unaffected, or one busy reader silences
	// everyone else's.
	if !l.take("b", now) {
		t.Error("a different session was refused on another's count")
	}
	if !l.take("a", now.Add(MessageWindow+time.Second)) {
		t.Error("the window never lets go")
	}
	// And a refusal records nothing, so hammering a full window does
	// not push its own expiry out.
	if n := len(l.byID["a"]); n != 1 {
		t.Errorf("the session holds %d times, want 1: a refused send was recorded", n)
	}
	// And a session whose window has emptied is dropped, or the map
	// grows with every session ever steered.
	l.take("c", now.Add(2*MessageWindow))
	if _, held := l.byID["b"]; held {
		t.Error("a session with nothing left in the window is still held")
	}
}

// Ownership is Service.Get's, org-scoped, so somebody else's session
// is not found rather than forbidden.
//
// @decision(DL-019)
func TestANonOwnerCannotSteer(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	stranger := auth.Principal{OrgID: "org_elsewhere", UserID: "user_2"}
	_, err := f.svc.Steer(context.Background(), stranger, f.sess.ID, SteerRequest{Text: "hello"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("the sandbox saw %d prompts from a stranger", n)
	}
}

// The recorder is nudged, for the reason DL-017 give: a
// message it is not listening for is written to the sandbox's own store
// and never to spov's, so the reader watches their own words vanish.
//
// @decision(DL-019) @decision(DL-017)
func TestASendNudgesTheRecorder(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	var woken []string
	f.svc.OnStart = func(sess store.Session) { woken = append(woken, sess.ID) }
	if _, err := f.svc.Steer(context.Background(), f.p, f.sess.ID, SteerRequest{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if len(woken) != 1 || woken[0] != f.sess.ID {
		t.Errorf("woke %v, want the steered session once", woken)
	}
}

// A nil nudge is a working configuration: it is an optimisation on a
// path that already works, and the poll is the mechanism.
//
// @decision(DL-019) @decision(DL-017)
func TestANilNudgeStillSends(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	f.svc.OnStart = nil
	if _, err := f.svc.Steer(context.Background(), f.p, f.sess.ID, SteerRequest{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls()); n != 1 {
		t.Errorf("the sandbox saw %d prompts, want 1", n)
	}
}

// A send writes a spov.session.steered marker naming who sent which
// message.
//
// @decision(DL-019) @decision(DL-030)
func TestSteeringRecordsItsOwnAct(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	ctx := context.Background()
	// An org admin, which is exactly the case this marker exists for:
	// Service.Get admits them to somebody else's session, and the
	// inbox item they cause is typed `user` like any other.
	admin := auth.Principal{OrgID: f.sess.OrgID, UserID: "user_admin", Admin: true}
	if _, err := f.svc.Steer(ctx, admin, f.sess.ID, SteerRequest{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	evs, err := f.svc.deps.Store.Conversations().ListEvents(ctx, f.sess.OrgID, f.sess.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var marks []store.Event
	for _, e := range evs {
		if e.Name == MarkerSteered {
			marks = append(marks, e)
		}
	}
	if len(marks) != 1 {
		t.Fatalf("found %d steering markers, want 1", len(marks))
	}
	if marks[0].Source != store.SourceSpov {
		t.Errorf("source = %q, want spov's own: a sandbox must not be able to write this",
			marks[0].Source)
	}
	if !strings.Contains(marks[0].Text, "user_admin") {
		t.Errorf("the marker says %q and does not name who sent it", marks[0].Text)
	}

	// It names the MESSAGE too, or with several in flight nothing ties
	// a marker to one of them — and it says how it was delivered, which
	// is a different act. Queueing is an interactive-session thing
	// (DL-052), so that half is asserted there.
	inter := interactiveFixture(t, f, "ses_i")
	if _, err := f.svc.Steer(ctx, admin, f.sess.ID,
		SteerRequest{Text: "hello", ID: "msg_second"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Steer(ctx, admin, inter.ID,
		SteerRequest{Text: "hello", ID: "msg_queued", Delivery: DeliveryQueue}); err != nil {
		t.Fatal(err)
	}
	evs, err = f.svc.deps.Store.Conversations().ListEvents(ctx, f.sess.OrgID, f.sess.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var second string
	for _, e := range evs {
		if e.Name == MarkerSteered && strings.Contains(e.Text, "msg_second") {
			second = e.Text
		}
	}
	if second == "" {
		t.Fatal("no marker names the second message, so nothing ties one to its own act")
	}
	ievs, err := f.svc.deps.Store.Conversations().ListEvents(ctx, inter.OrgID, inter.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var queued string
	for _, e := range ievs {
		if e.Name == MarkerSteered {
			queued = e.Text
		}
	}
	if !strings.Contains(queued, "queued") {
		t.Errorf("the marker says %q and does not say it was queued", queued)
	}
}

// The record survives the caller hanging up. It is the only
// attribution there is, so a client that cancels the moment its send is
// away must not get to choose whether its own act was recorded.
//
// @decision(DL-019)
func TestTheRecordSurvivesACancelledRequest(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	// The memory store ignores its context, so ctxBoundConversations
	// stands in for pgx, which refuses on a cancelled context.
	real := f.svc.deps.Store
	f.svc.deps.Store = storeWith{Store: real, convs: &ctxBoundConversations{
		ConversationStore: real.Conversations()}}

	// The state the service is in once the send is away and the client
	// has closed the connection. Driven at recordSteer rather than
	// through Steer, because cancelling mid-request races the POST
	// itself and would be measuring the transport.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.svc.recordSteer(ctx, f.p, f.sess, SteeredMessage{ID: "msg_mine", Delivery: DeliverySteer})

	evs, err := real.Conversations().ListEvents(
		context.Background(), f.sess.OrgID, f.sess.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Name == MarkerSteered {
			return
		}
	}
	t.Error("the act went unrecorded because the caller hung up: this is the only" +
		" attribution there is, so a client could choose whether its own act was recorded")
}

// ctxBoundConversations refuses a write on a cancelled context, which
// is what a real database driver does and what the memory store does
// not.
type ctxBoundConversations struct {
	store.ConversationStore
}

func (w *ctxBoundConversations) PutEvents(ctx context.Context, evs []store.Event) ([]store.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return w.ConversationStore.PutEvents(ctx, evs)
}

// A session with no conversation gets one on its first message, in its
// landing and on its model, recorded so the next message goes into the
// same one.
//
// @decision(DL-019)
func TestTheFirstMessageCreatesTheConversation(t *testing.T) {
	f := newSteerFixture(t)
	inter := interactiveFixture(t, f)
	ctx := context.Background()
	stored, err := f.svc.deps.Store.Sessions().Get(ctx, inter.OrgID, inter.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Request.Model = "anthropic/claude-sonnet-4-5"
	stored.Landing = "/workspace/repo"
	if err := f.svc.deps.Store.Sessions().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	sent, err := f.svc.Steer(ctx, f.p, inter.ID, SteerRequest{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Conversation != "ses_created" {
		t.Errorf("sent into %q, want the created conversation", sent.Conversation)
	}
	var paths []string
	for _, c := range f.calls() {
		paths = append(paths, c.method+" "+c.path[strings.Index(c.path, "/api/"):])
	}
	want := []string{"POST /api/session", "POST /api/session/ses_created/model", "POST /api/session/ses_created/prompt"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("the sandbox saw %v, want %v", paths, want)
	}
	if _, err := f.svc.Steer(ctx, f.p, inter.ID, SteerRequest{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls()); n != 4 {
		t.Errorf("%d requests; the second message created another conversation", n)
	}
	convs, _ := f.svc.deps.Store.Conversations().ListConversations(ctx, inter.OrgID, inter.ID)
	if len(convs) != 1 || convs[0].OpenCodeID != "ses_created" {
		t.Errorf("conversations = %+v", convs)
	}
}

// Concurrent first messages create one conversation between them.
//
// @decision(DL-019)
func TestConcurrentFirstMessagesCreateOneConversation(t *testing.T) {
	f := newSteerFixture(t)
	inter := interactiveFixture(t, f)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.svc.Steer(context.Background(), f.p, inter.ID, SteerRequest{Text: "hi"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	created := 0
	for _, c := range f.calls() {
		if strings.HasSuffix(c.path, "/api/session") {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d conversations created, want 1", created)
	}
}

// A job's conversation is jobConversation's to create: a message to a
// job with none recorded is still refused rather than opening another.
//
// @decision(DL-019) @decision(DL-045)
func TestAJobsFirstConversationIsNotAMessages(t *testing.T) {
	f := newSteerFixture(t)
	if _, err := f.svc.Steer(context.Background(), f.p, f.sess.ID, SteerRequest{Text: "hello"}); !errors.Is(err, ErrNoConversation) {
		t.Fatalf("err = %v, want no conversation", err)
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("the sandbox saw %d requests", n)
	}
}

// A cancel still needs a conversation: there is nothing to cancel in.
//
// @decision(DL-019)
func TestACancelInASessionWithNoConversationIsAConflict(t *testing.T) {
	f := newSteerFixture(t)
	_, err := f.svc.CancelMessage(context.Background(), f.p, f.sess.ID, "msg_x", "")
	if !errors.Is(err, ErrNoConversation) {
		t.Fatalf("err = %v, want no conversation", err)
	}
}

// With several conversations the caller names one. Choosing would be a
// guess, and the wrong guess puts a reader's message in front of an
// agent they were not talking to.
//
// @decision(DL-019)
func TestSeveralConversationsMustBeNamed(t *testing.T) {
	f := newSteerFixture(t, "ses_1", "ses_2")
	ctx := context.Background()
	_, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "hello"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want an invalid request", err)
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("the sandbox saw %d prompts, want none", n)
	}
	sent, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Conversation: "ses_2", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Conversation != "ses_2" {
		t.Errorf("went to %q, want the named one", sent.Conversation)
	}
}

// An answer this build cannot read must not lose an id the caller
// already holds: that id is what a client reconciles its own echo on,
// so dropping it renders the reader's message twice.
//
// @decision(DL-019) @decision(DL-059)
func TestAnUnreadableAnswerKeepsTheCallersId(t *testing.T) {
	for _, answer := range []string{"", "not json", `{"data":{}}`, `{}`} {
		if got := promptedID([]byte(answer), "msg_mine"); got != "msg_mine" {
			t.Errorf("promptedID(%q) = %q, want the caller's own", answer, got)
		}
	}
	if got := promptedID([]byte(`{"data":{"id":"msg_theirs"}}`), "msg_mine"); got != "msg_theirs" {
		t.Errorf("promptedID = %q, want the sandbox's answer when it has one", got)
	}
	// And the sandbox is held to the rule a caller is. This id becomes
	// the recorder's key for the row and what a client reconciles on,
	// so an id the sandbox invented is not exempt from the check a
	// caller's own would fail.
	for _, bad := range []string{"ses_1", "msg_one two", "msg_../../x", "évidemment"} {
		if got := promptedID([]byte(`{"data":{"id":"`+bad+`"}}`), "msg_mine"); got != "msg_mine" {
			t.Errorf("the sandbox answered %q and it was passed through as %q", bad, got)
		}
	}
}

// A session that is not running is a state conflict, and nothing
// reaches a sandbox that is not there.
//
// @decision(DL-019)
func TestAStoppedSessionCannotBeSteered(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	ctx := context.Background()
	stopped := f.sess
	stopped.State = store.StateStopped
	if err := f.svc.deps.Store.Sessions().Update(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "hello"})
	if !errors.Is(err, ErrSessionNotRunning) {
		t.Fatalf("err = %v, want the session not running", err)
	}

	// Running with no sandbox is the other half, and it is the one a
	// state check alone would miss: a job's session is recorded
	// `starting` with no sandbox, and a row that reached `running`
	// without one has nothing to send to.
	g := newSteerFixture(t, "ses_1")
	none := g.sess
	none.SandboxID = ""
	if err := g.svc.deps.Store.Sessions().Update(ctx, none); err != nil {
		t.Fatal(err)
	}
	if _, err := g.svc.Steer(ctx, g.p, g.sess.ID, SteerRequest{Text: "hello"}); !errors.Is(err, ErrSessionNotRunning) {
		t.Fatalf("err = %v, want the session not running", err)
	}
	if n := len(g.calls()); n != 0 {
		t.Errorf("the sandbox saw %d prompts from a session with none", n)
	}
	// And the refusal cost nothing: without the guard here the send
	// reaches the ceiling first and is refused by connectReusing
	// afterwards, so hammering a dead session would lock its owner out
	// of the one they revive.
	none.SandboxID = g.sess.SandboxID
	if err := g.svc.deps.Store.Sessions().Update(ctx, none); err != nil {
		t.Fatal(err)
	}
	for i := range MessagesPerMinute {
		if _, err := g.svc.Steer(ctx, g.p, g.sess.ID, SteerRequest{Text: "hello"}); err != nil {
			t.Fatalf("message %d of %d was refused, so the refusals above spent the ceiling: %v",
				i+1, MessagesPerMinute, err)
		}
	}
}

// Steering shares one mint across its sends. It is the only test that
// fails if withConnection is replaced by Connect plus a direct post.
//
// @decision(DL-019) @decision(DL-030)
func TestSteeringSharesOneMint(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	ctx := context.Background()
	for i := range 3 {
		if _, err := f.svc.Steer(ctx, f.p, f.sess.ID, SteerRequest{Text: "hello"}); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	if n := mintsOf(f.fake); n != 1 {
		t.Errorf("three messages issued %d connect tokens, want 1: each is unrevocable for"+
			" 24 h and comes out of the budget the recorder needs to redial", n)
	}
}

// The endpoint is registered, driven over HTTP; every test above calls
// Service.Steer directly.
//
// @decision(DL-019)
func TestTheSendEndpointIsRegistered(t *testing.T) {
	var got struct {
		mu   sync.Mutex
		path string
		body map[string]any
	}
	sb := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		got.path = r.URL.Path
		_ = json.Unmarshal(raw, &got.body)
		got.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"msg_echo"}}`))
	})))
	defer sb.Close()

	f := newAPIFixture(t)
	f.provider.ConnectBaseURL = sb.URL
	sess := runningSession(t, f)
	seedConversation(t, f, sess, "ses_1", "one")

	res, body := f.do(http.MethodPost, "/api/v1/sessions/"+sess.ID+"/messages",
		SteerRequest{Text: "hello", ID: "msg_mine"})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("POST messages = %d %s, want 202", res.StatusCode, body)
	}
	var out SteeredMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "msg_echo" || out.Conversation != "ses_1" {
		t.Errorf("answered %+v", out)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if !strings.HasSuffix(got.path, "/api/session/ses_1/prompt") {
		t.Errorf("the sandbox saw %q, want the prompt endpoint", got.path)
	}
	if got.body["text"] != "hello" {
		t.Errorf("the sandbox saw %#v", got.body)
	}
	// Credentials and a live conversation are never a cacheable
	// representation of anything.
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// The two refusals that are neither a bad request nor a missing
// session keep their own status, because a client switches on it.
//
// @decision(DL-019)
func TestTheSendRefusalsCarryTheirStatus(t *testing.T) {
	f := newAPIFixture(t)
	// A closed loopback port refuses at once. The fake's default host
	// costs a DNS lookup per send, which on a slow resolver stretched
	// the sends past the ceiling's minute.
	f.provider.BaseURL = "https://127.0.0.1:1"
	sess := runningSession(t, f)

	// No conversation to cancel in: a state conflict, not a 422.
	res, body := f.do(http.MethodDelete, "/api/v1/sessions/"+sess.ID+"/messages/msg_x", nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("a cancel in a session with no conversation = %d %s, want 409", res.StatusCode, body)
	}

	seedConversation(t, f, sess, "ses_1", "one")
	// The ceiling. The sandbox is unreachable here, so these fail — but
	// the ceiling is taken before the send, so it is exhausted all the
	// same, which is the behaviour a client meets on a bad day.
	for range MessagesPerMinute {
		f.do(http.MethodPost, "/api/v1/sessions/"+sess.ID+"/messages", SteerRequest{Text: "hello"})
	}
	res, body = f.do(http.MethodPost, "/api/v1/sessions/"+sess.ID+"/messages", SteerRequest{Text: "hello"})
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("past the ceiling = %d %s, want 429", res.StatusCode, body)
	}
}

// MessageIDPrefix is written twice, here and in web/src/lib/steer.ts.
// This reads the TypeScript source, since on drift validMessageID
// refuses every browser send with a 422.
//
// @decision(DL-059) @decision(DL-019)
func TestTheMessageIDPrefixMatchesTheBrowser(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "steer.ts"))
	if err != nil {
		t.Fatalf("reading the browser's copy: %v", err)
	}
	want := `export const MessageIDPrefix = "` + MessageIDPrefix + `";`
	if !strings.Contains(string(src), want) {
		t.Fatalf("web/src/lib/steer.ts does not declare %s;"+
			" the Go constant and the browser's have drifted, so every send from"+
			" the composer is refused by validMessageID and nothing says why", want)
	}
}
