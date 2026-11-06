package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/store"
)

// fakeSandbox scripts what a sandbox answers and counts every request
// the recorder makes of it, which is what the recorder's rule is about.
type fakeSandbox struct {
	// t checks each read against the pinned build's document.
	t        testing.TB
	mu       sync.Mutex
	connects int
	dials    int
	reads    map[string]int
	// listing and messages are what /api/session and each
	// conversation's /message answer.
	listing  string
	messages map[string]string
	// stream is the current span's frames; closing it ends the stream.
	stream chan string
	// dialErr, when set, makes the next dial fail.
	dialErr error
	// listErr and connectErr break one read rather than all of them.
	listErr    error
	connectErr error
	// connectAs records the principal of every connect.
	connectAs []auth.Principal
}

func newFakeSandbox(t testing.TB) *fakeSandbox {
	return &fakeSandbox{t: t, reads: map[string]int{}, listing: `{"data":[]}`, messages: map[string]string{}, stream: make(chan string, 64)}
}

func (f *fakeSandbox) connect(_ context.Context, p auth.Principal, _ store.Session) (Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects++
	f.connectAs = append(f.connectAs, p)
	if f.connectErr != nil {
		return Connection{}, f.connectErr
	}
	return Connection{URL: "http://sandbox.test", Token: "tok", User: "opencode", Password: "pw"}, nil
}

func (f *fakeSandbox) events(ctx context.Context, _ Connection) (io.ReadCloser, error) {
	f.mu.Lock()
	f.dials++
	err := f.dialErr
	stream := f.stream
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	// Stops feeding when the span closes the body, so an ended span's
	// goroutine cannot steal frames from the next one on the same
	// channel.
	dctx, stop := context.WithCancel(ctx)
	go func() {
		defer pw.Close()
		for {
			select {
			case <-dctx.Done():
				return
			case raw, ok := <-stream:
				if !ok {
					return
				}
				if _, err := fmt.Fprintf(pw, "data: %s\n\n", raw); err != nil {
					return
				}
			}
		}
	}()
	return &stoppingBody{Reader: pr, stop: func() { stop(); _ = pr.Close() }}, nil
}

type stoppingBody struct {
	io.Reader
	stop func()
}

func (b *stoppingBody) Close() error { b.stop(); return nil }

func (f *fakeSandbox) fetch(_ context.Context, _ Connection, path string) ([]byte, error) {
	contract.CheckRequest(f.t, http.MethodGet, path, nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[path]++
	if path == "/api/session" {
		if f.listErr != nil {
			return nil, f.listErr
		}
		return []byte(f.listing), nil
	}
	route, query, _ := strings.Cut(path, "?")
	for id, body := range f.messages {
		if route == "/api/session/"+id+"/message" {
			return pageOfMessages(f.t, body, query), nil
		}
	}
	return nil, errors.New("no such conversation")
}

// pageOfMessages answers a /message read as the pinned build does
// (spike S-MP1): limit, order or cursor, and a next cursor even on the
// last page. A body that is not a {data} envelope is answered whole.
//
// @decision(DL-069)
func pageOfMessages(t testing.TB, body, rawQuery string) []byte {
	var all struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal([]byte(body), &all) != nil || all.Data == nil {
		return []byte(body)
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Errorf("the message read's query: %v", err)
	}
	limit, order, from := 50, q.Get("order"), 0
	if l := q.Get("limit"); l != "" {
		if limit, err = strconv.Atoi(l); err != nil || limit < 1 || limit > 200 {
			t.Errorf("limit %q is outside what OpenCode accepts", l)
		}
	}
	if c := q.Get("cursor"); c != "" {
		if order != "" {
			t.Error("a cursor was combined with an order, which OpenCode refuses")
		}
		parts := strings.Split(c, ":")
		n, err := 0, error(nil)
		if len(parts) == 3 && parts[0] == "fake" {
			n, err = strconv.Atoi(parts[2])
		}
		if len(parts) != 3 || parts[0] != "fake" || err != nil {
			t.Errorf("a cursor the fake never issued: %q", c)
		}
		order, from = parts[len(parts)-2], n
	}
	if order == "" {
		order = "desc"
	}
	items := slices.Clone(all.Data)
	if order == "desc" {
		slices.Reverse(items)
	}
	to := min(from+limit, len(items))
	from = min(from, to)
	next := fmt.Sprintf("fake:%s:%d", order, to)
	out, _ := json.Marshal(map[string]any{"data": items[from:to], "cursor": map[string]any{"next": next, "previous": nil}})
	return out
}

// counts is a snapshot for assertions.
func (f *fakeSandbox) counts() (connects, dials, sessionReads, messageReads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for path, n := range f.reads {
		if path == "/api/session" {
			sessionReads += n
		} else {
			messageReads += n
		}
	}
	return f.connects, f.dials, sessionReads, messageReads
}

// breakStream ends the current stream and installs a fresh one for the
// next dial to read.
func (f *fakeSandbox) breakStream() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.stream)
	f.stream = make(chan string, 64)
}

// durableFrameFor is a frame in the measured shape, for conversation
// ocid at sequence seq.
func durableFrameFor(name, ocid string, seq int, text string) string {
	payload := ""
	if text != "" {
		payload = fmt.Sprintf(`,"item":{"type":"user","payload":{"text":%q}}`, text)
	}
	return fmt.Sprintf(`{"id":"evt_%d","created":%d,"type":%q,"data":{"sessionID":%q%s},"durable":{"aggregateID":%q,"seq":%d,"version":1}}`,
		seq, 1788265400000+int64(seq)*1000, name, ocid, payload, ocid, seq)
}

const twoMessages = `{"data":[
  {"id":"msg_u","time":{"created":1788266065984},"text":"hi","type":"user"},
  {"id":"msg_a","time":{"created":1788266066749},"type":"assistant","content":[{"type":"text","text":"hello"}]}]}`

// recorderFixture is a recorder over a memory store and a fake sandbox,
// with a running session to capture and no real waiting anywhere.
type recorderFixture struct {
	t     *testing.T
	st    *store.Memory
	sb    *fakeSandbox
	rec   *Recorder
	sess  store.Session
	warns []string
	// slept records every backoff the recorder asked for.
	slept []time.Duration
	mu    sync.Mutex
}

func newRecorderFixture(t *testing.T) *recorderFixture {
	t.Helper()
	f := &recorderFixture{t: t, st: store.NewMemory(), sb: newFakeSandbox(t)}
	f.sess = store.Session{ID: store.NewID(time.Now()), OrgID: "org", OwnerID: "u1", Name: "alpha",
		State: store.StateRunning, SandboxID: "sb-1"}
	if err := f.st.Sessions().Create(context.Background(), f.sess); err != nil {
		t.Fatal(err)
	}
	f.rec = NewRecorder(nil, f.st, time.Now, func(s string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.warns = append(f.warns, s)
	})
	f.rec.connect, f.rec.events, f.rec.fetch = f.sb.connect, f.sb.events, f.sb.fetch
	// NewRecorder was handed a nil Service, so the production drop
	// closure would panic rather than fail the moment a fake returned
	// a refusal. Stubbed here; the tests that care replace it.
	f.rec.drop = func(string, string) {}
	f.rec.backoff = []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond, 5 * time.Millisecond}
	f.rec.sleep = func(ctx context.Context, d time.Duration) error {
		f.mu.Lock()
		f.slept = append(f.slept, d)
		f.mu.Unlock()
		return ctx.Err()
	}
	// No capture outlives its test: every goroutine the recorder
	// started is cancelled and waited for.
	t.Cleanup(f.rec.stopAll)
	return f
}

// waitFor polls until cond holds or the test's patience runs out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *recorderFixture) events() []store.Event {
	evs, err := f.st.Conversations().ListEvents(context.Background(), "org", f.sess.ID, 0, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return evs
}

// openCodeEvents is the sandbox's own rows alone. A test about what the
// stream stored says so with this rather than by counting every row:
// the recorder writes markers of its own beside them.
func (f *recorderFixture) openCodeEvents() []store.Event {
	var out []store.Event
	for _, e := range f.events() {
		if e.Source == store.SourceOpenCode {
			out = append(out, e)
		}
	}
	return out
}

func (f *recorderFixture) messages() []store.Message {
	msgs, err := f.st.Conversations().ListMessages(context.Background(), "org", f.sess.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return msgs
}

// conversationOf is n messages in the measured shape, the oldest first,
// numbered from first. Each one's time is its number in seconds, so a
// store listing orders them as the sandbox does.
func conversationOf(first, n int) []string {
	var out []string
	for i := first; i < first+n; i++ {
		out = append(out, fmt.Sprintf(`{"id":"msg_%05d","time":{"created":%d},"type":"user","text":"m%d"}`, i, 1788266000000+int64(i)*1000, i))
	}
	return out
}

func messagesBody(msgs []string) string { return `{"data":[` + strings.Join(msgs, ",") + `]}` }

// messageReadsOf counts the recorder's reads of one conversation's
// messages, a page each.
func (f *fakeSandbox) messageReadsOf(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for path, c := range f.reads {
		if strings.HasPrefix(path, "/api/session/"+id+"/message") {
			n += c
		}
	}
	return n
}

// A conversation longer than one page is stored whole at attach, and a
// turn end afterwards reads what is new and one page already stored,
// not the whole conversation again.
//
// @decision(DL-069)
func TestALongConversationIsReadPageByPage(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.listing = `{"data":[{"id":"ses_1"}]}`
	msgs := conversationOf(0, 2*MessagePageSize+30)
	f.sb.messages["ses_1"] = messagesBody(msgs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the catch-up", func() bool { return len(f.messages()) == len(msgs) })
	if n := f.sb.messageReadsOf("ses_1"); n != 3 {
		t.Errorf("the catch-up read %d pages, want 3", n)
	}
	if got := f.messages(); got[0].OpenCodeID != "msg_00000" || got[len(got)-1].OpenCodeID != "msg_00129" {
		t.Errorf("stored from %s to %s", got[0].OpenCodeID, got[len(got)-1].OpenCodeID)
	}

	// Sixty new messages: two pages hold something new, the third holds
	// nothing new and ends the read.
	msgs = append(msgs, conversationOf(len(msgs), 60)...)
	f.sb.messages["ses_1"] = messagesBody(msgs)
	f.sb.stream <- durableFrameFor("session.execution.succeeded", "ses_1", 1, "")
	waitFor(t, "the turn-end read", func() bool { return len(f.messages()) == len(msgs) })
	waitFor(t, "the read to stop", func() bool { return f.sb.messageReadsOf("ses_1") == 6 })

	// Two new messages: the first page, then one page with nothing new.
	msgs = append(msgs, conversationOf(len(msgs), 2)...)
	f.sb.messages["ses_1"] = messagesBody(msgs)
	f.sb.stream <- durableFrameFor("session.execution.succeeded", "ses_1", 2, "")
	waitFor(t, "the second turn-end read", func() bool { return len(f.messages()) == len(msgs) })
	waitFor(t, "the read to stop", func() bool { return f.sb.messageReadsOf("ses_1") == 8 })
	time.Sleep(20 * time.Millisecond)
	if n := f.sb.messageReadsOf("ses_1"); n != 8 {
		t.Errorf("after two turn ends the recorder read %d pages, want 8", n)
	}

	// A redial's catch-up finds the store holding everything: one page.
	f.sb.breakStream()
	waitFor(t, "the second catch-up", func() bool { _, _, s, _ := f.sb.counts(); return s == 2 })
	waitFor(t, "its read", func() bool { return f.sb.messageReadsOf("ses_1") == 9 })
	time.Sleep(20 * time.Millisecond)
	if n := f.sb.messageReadsOf("ses_1"); n != 9 {
		t.Errorf("a catch-up over a stored conversation read %d pages, want 1", n-8)
	}

	// The final read on a stop takes the same one page.
	cancel()
	f.rec.stopAll()
	f.rec.Final(context.Background(), auth.Principal{UserID: "u1", OrgID: "org"}, f.sess)
	if n := f.sb.messageReadsOf("ses_1"); n != 10 {
		t.Errorf("the final read took %d pages, want 1", n-9)
	}
}

// A page cut short at the read bound is not JSON. The read stops there
// and says so, rather than losing the older pages in silence.
//
// @decision(DL-069)
func TestAPageCutShortIsSaid(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.listing = `{"data":[{"id":"ses_1"}]}`
	f.sb.messages["ses_1"] = `{"data":[{"id":"msg_1","type":"user","text":"cut`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the warning", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.warns) == 1 && strings.Contains(f.warns[0], "is not JSON")
	})
}

// A message stored early in a span and changed later, such as a reply
// read while it was still being written, is stored again while it is
// within one page of the newest message the span had stored. Here it
// lies past the first page that holds a stored message, which a read
// stopping there would miss.
//
// @decision(DL-069)
func TestAMessageReadMidTurnIsReadAgainAtTheTurnEnd(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.listing = `{"data":[{"id":"ses_1"}]}`
	msgs := conversationOf(0, 60)
	f.sb.messages["ses_1"] = messagesBody(msgs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the catch-up", func() bool { return len(f.messages()) == 60 })

	msgs[50] = strings.Replace(msgs[50], `"text":"m50"`, `"text":"m50, finished"`, 1)
	msgs = append(msgs, conversationOf(60, MessagePageSize-5)...)
	f.sb.messages["ses_1"] = messagesBody(msgs)
	f.sb.stream <- durableFrameFor("session.execution.succeeded", "ses_1", 1, "")
	waitFor(t, "the finished message", func() bool {
		for _, m := range f.messages() {
			if m.OpenCodeID == "msg_00050" {
				return m.Text == "m50, finished"
			}
		}
		return false
	})
}

// A conversation past the page bound is read to it, newest first, and
// said.
//
// @decision(DL-069)
func TestAConversationPastThePageBoundIsSaid(t *testing.T) {
	f := newRecorderFixture(t)
	f.rec.maxPages = 3
	f.sb.listing = `{"data":[{"id":"ses_1"}]}`
	f.sb.messages["ses_1"] = messagesBody(conversationOf(0, 3*MessagePageSize+7))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the warning", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.warns) == 1 && strings.Contains(f.warns[0], "bound of 3 pages")
	})
	if n := f.sb.messageReadsOf("ses_1"); n != 3 {
		t.Errorf("read %d pages past a bound of 3", n)
	}
	got := f.messages()
	if len(got) != 3*MessagePageSize || got[0].OpenCodeID != "msg_00007" {
		t.Errorf("stored %d messages from %s, want the newest %d", len(got), got[0].OpenCodeID, 3*MessagePageSize)
	}
}

// TestTheRecorderNeverReadsOnATimer drives one capture through attach, a
// run of frames, a turn end and a break, and checks that every request
// the recorder made had one of those as its cause — and that nothing at
// all happens while the sandbox is quiet.
//
// @decision(DL-017)
func TestTheRecorderNeverReadsOnATimer(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.listing = `{"data":[{"id":"ses_1","title":"first"}]}`
	f.sb.messages["ses_1"] = twoMessages
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// Attach: one mint, one dial, one listing, one read per conversation.
	waitFor(t, "the catch-up", func() bool { return len(f.messages()) == 2 })
	if c, d, s, m := f.sb.counts(); c != 1 || d != 1 || s != 1 || m != 1 {
		t.Fatalf("after attach: connects=%d dials=%d listings=%d message reads=%d", c, d, s, m)
	}
	convs, _ := f.st.Conversations().ListConversations(ctx, "org", f.sess.ID)
	if len(convs) != 1 || convs[0].Title != "first" || convs[0].OpenCodeID != "ses_1" {
		t.Fatalf("conversations = %+v", convs)
	}
	// The listing's item is kept whole, with no read of its own.
	// @decision(DL-089)
	if string(convs[0].Info) != `{"id":"ses_1","title":"first"}` {
		t.Errorf("the conversation's info = %q, want the listing's item", convs[0].Info)
	}
	if evs := f.events(); len(evs) != 0 {
		t.Fatalf("a first attach wrote a marker: %+v", evs)
	}

	// A hundred frames: a hundred rows and no reads.
	for i := 1; i <= 100; i++ {
		f.sb.stream <- durableFrameFor("session.step.started", "ses_1", i, "")
	}
	waitFor(t, "a hundred rows", func() bool { return len(f.events()) == 100 })
	if c, d, s, m := f.sb.counts(); c != 1 || d != 1 || s != 1 || m != 1 {
		t.Fatalf("after frames: connects=%d dials=%d listings=%d message reads=%d", c, d, s, m)
	}
	first := f.events()[0]
	if first.Seq != 1 || first.Source != store.SourceOpenCode || first.ConversationID != convs[0].ID ||
		!strings.Contains(string(first.Raw), `"seq":1`) || first.At.IsZero() {
		t.Errorf("first row = %+v", first)
	}
	if !first.At.Equal(time.UnixMilli(1788265401000).UTC()) {
		t.Errorf("at = %v, want the frame's created time", first.At)
	}

	// A turn end: exactly one read of that conversation's messages.
	f.sb.messages["ses_1"] = strings.Replace(twoMessages, `"hello"`, `"hello again"`, 1)
	f.sb.stream <- durableFrameFor("session.execution.succeeded", "ses_1", 101, "")
	waitFor(t, "the turn-end read", func() bool { _, _, _, m := f.sb.counts(); return m == 2 })
	waitFor(t, "the updated message", func() bool {
		msgs := f.messages()
		return len(msgs) == 2 && msgs[1].Text == "hello again"
	})
	if c, d, s, _ := f.sb.counts(); c != 1 || d != 1 || s != 1 {
		t.Fatalf("a turn end minted or dialled: connects=%d dials=%d listings=%d", c, d, s)
	}

	// Quiet: nothing happens.
	time.Sleep(50 * time.Millisecond)
	if c, d, s, m := f.sb.counts(); c != 1 || d != 1 || s != 1 || m != 2 {
		t.Fatalf("something read the sandbox while it was quiet: connects=%d dials=%d listings=%d message reads=%d", c, d, s, m)
	}

	// A break while the session still runs: one redial, then attach
	// again. The log resumes where it stopped, so nothing is marked.
	f.sb.breakStream()
	waitFor(t, "the redial", func() bool { _, d, _, _ := f.sb.counts(); return d == 2 })
	waitFor(t, "the second catch-up", func() bool { _, _, s, _ := f.sb.counts(); return s == 2 })
	f.mu.Lock()
	slept := append([]time.Duration(nil), f.slept...)
	f.mu.Unlock()
	if len(slept) != 1 || slept[0] != f.rec.backoff[0] {
		t.Errorf("waited %v before redialling, want %v once", slept, f.rec.backoff[0])
	}
	f.sb.stream <- durableFrameFor("session.step.started", "ses_1", 102, "")
	// 100 frames, the turn end, then this one, with no gap marker.
	waitFor(t, "a frame after the redial", func() bool { return len(f.events()) == 102 })
	if got := f.events()[101]; got.Seq != 102 || got.Pos <= f.events()[100].Pos {
		t.Errorf("after the redial: %+v", got)
	}
	if m := f.gapMarkers(); len(m) != 0 {
		t.Errorf("a redial that resumed at the next sequence marked %q", m[0].Text)
	}
}

// TestABreakIsBoundedAndMarked: a sandbox whose stream keeps ending
// while the store says the session runs is redialled RecorderRedials
// times, then given up on with a stored marker, and never again while
// this process leads.
//
// @decision(DL-017)
func TestABreakIsBoundedAndMarked(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.dialErr = errors.New("connection refused")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stopped marker", func() bool {
		evs := f.events()
		return len(evs) == 1 && evs[0].Name == MarkerStopped
	})
	waitFor(t, "the capture to end", func() bool { return !f.rec.Recording(f.sess.ID) })
	if _, d, _, _ := f.sb.counts(); d != 1+RecorderRedials {
		t.Errorf("dials = %d, want the first plus %d redials", d, RecorderRedials)
	}
	f.mu.Lock()
	slept := append([]time.Duration(nil), f.slept...)
	f.mu.Unlock()
	if len(slept) != RecorderRedials || slept[RecorderRedials-1] != f.rec.backoff[RecorderRedials-1] {
		t.Errorf("waits = %v, want the whole backoff schedule", slept)
	}
	marker := f.events()[0]
	if marker.Source != store.SourceSpov || !strings.Contains(marker.Text, "5 failed reconnects") || marker.ConversationID != "" {
		t.Errorf("marker = %+v", marker)
	}

	// A later sync does not try again.
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, d, _, _ := f.sb.counts(); d != 1+RecorderRedials {
		t.Errorf("a sync after giving up dialled again: %d", d)
	}
	if f.rec.Recording(f.sess.ID) {
		t.Error("an abandoned session is being recorded")
	}
}

// A stream that ends because the session stopped is not a break: no
// redial, no marker.
//
// @decision(DL-017)
func TestAStoppedSessionEndsQuietly(t *testing.T) {
	f := newRecorderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attach", func() bool { _, _, s, _ := f.sb.counts(); return s == 1 })

	stopped := f.sess
	stopped.State = store.StateStopped
	if err := f.st.Sessions().Update(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	f.sb.breakStream()
	waitFor(t, "the capture to end", func() bool { return !f.rec.Recording(f.sess.ID) })
	if _, d, _, _ := f.sb.counts(); d != 1 {
		t.Errorf("a stopped session was redialled: %d dials", d)
	}
	if evs := f.events(); len(evs) != 0 {
		t.Errorf("a stopped session got a marker: %+v", evs)
	}
}

// Sync starts captures for running sessions and cancels the ones whose
// session is no longer running.
//
// @decision(DL-017)
func TestSyncStartsAndStopsCaptures(t *testing.T) {
	f := newRecorderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	other := store.Session{ID: store.NewID(time.Now()), OrgID: "org", OwnerID: "u1", Name: "beta",
		State: store.StateRunning, SandboxID: "sb-2"}
	starting := store.Session{ID: store.NewID(time.Now()), OrgID: "org", OwnerID: "u1", Name: "gamma",
		State: store.StateStarting}
	for _, s := range []store.Session{other, starting} {
		if err := f.st.Sessions().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.rec.Recording(f.sess.ID) || !f.rec.Recording(other.ID) || f.rec.Recording(starting.ID) {
		t.Fatal("captures do not match the running sessions")
	}
	waitFor(t, "both attaches", func() bool { c, _, _, _ := f.sb.counts(); return c == 2 })
	// A second sync starts nothing new.
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if c, _, _, _ := f.sb.counts(); c != 2 {
		t.Errorf("a second sync connected again: %d", c)
	}

	other.State = store.StateStopped
	if err := f.st.Sessions().Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stopped session's capture to end", func() bool { return !f.rec.Recording(other.ID) })
	if !f.rec.Recording(f.sess.ID) {
		t.Error("the running session's capture was cancelled too")
	}
}

// failingConversations wraps a store so its writes fail on demand.
type failingConversations struct {
	store.ConversationStore
	fail atomic.Bool
}

func (w *failingConversations) PutEvents(ctx context.Context, evs []store.Event) ([]store.Event, error) {
	if w.fail.Load() {
		return nil, errors.New("disk full")
	}
	return w.ConversationStore.PutEvents(ctx, evs)
}

type storeWith struct {
	store.Store
	convs store.ConversationStore
}

func (s storeWith) Conversations() store.ConversationStore { return s.convs }

// A store that cannot be written ends the span like a broken stream,
// and the capture comes back under the same bounded redial — here the
// store recovers by the first backoff, and recording resumes with a
// gap marker where the failed write was.
//
// @decision(DL-017)
func TestAWriteFailureIsABreak(t *testing.T) {
	f := newRecorderFixture(t)
	failing := &failingConversations{ConversationStore: f.st.Conversations()}
	f.rec.store = storeWith{Store: f.st, convs: failing}
	inner := f.rec.sleep
	f.rec.sleep = func(ctx context.Context, d time.Duration) error {
		failing.fail.Store(false)
		return inner(ctx, d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attach", func() bool { _, _, s, _ := f.sb.counts(); return s == 1 })
	failing.fail.Store(true)
	f.sb.stream <- durableFrameFor("session.step.started", "ses_1", 1, "")
	waitFor(t, "the redial", func() bool { _, d, _, _ := f.sb.counts(); return d >= 2 })
	f.mu.Lock()
	warned := strings.Join(f.warns, "\n")
	f.mu.Unlock()
	if !strings.Contains(warned, "disk full") {
		t.Errorf("the write failure was not reported: %q", warned)
	}
	f.sb.stream <- durableFrameFor("session.step.started", "ses_1", 2, "")
	waitFor(t, "a row after recovery", func() bool {
		evs := f.events()
		return len(evs) == 2 && evs[0].Name == MarkerGap && evs[1].Seq == 2
	})
	if _, d, _, _ := f.sb.counts(); d != 2 {
		t.Errorf("dials = %d, want exactly one redial", d)
	}
}

// The real frames captured on 2026-09-01 become rows keyed on their
// durable sequence, at their own created time.
//
// @decision(DL-017)
func TestRealFramesBecomeRows(t *testing.T) {
	f := newRecorderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attach", func() bool { _, _, s, _ := f.sb.counts(); return s == 1 })

	const (
		stepStarted = `{"id":"evt_000000000003SyntheticId003","created":1788265407953,` +
			`"type":"session.step.started","location":{"directory":"/workspace/spov"},` +
			`"data":{"sessionID":"ses_000000000020SyntheticId020","agent":"build",` +
			`"model":{"id":"ling-3.0-flash-fin-free","providerID":"opencode","variant":"default"},` +
			`"assistantMessageID":"msg_000000000001SyntheticId001","snapshot":"0badc0de"},` +
			`"durable":{"aggregateID":"ses_000000000020SyntheticId020","seq":46,"version":1}}`
		inboxEnqueued = `{"id":"evt_000000000006SyntheticId006","created":1788267414303,` +
			`"type":"session.inbox.enqueued","location":{"directory":"/workspace/spov"},` +
			`"data":{"inboxID":"msg_000000000005SyntheticId005","sessionID":"ses_000000000019SyntheticId019",` +
			`"item":{"type":"user","payload":{"text":"reply with ok"},"delivery":"steer"}},` +
			`"durable":{"aggregateID":"ses_000000000019SyntheticId019","seq":30,"version":1}}`
		serverConnected = `{"id":"evt_000000000002SyntheticId002","type":"server.connected","data":{}}`
	)
	f.sb.stream <- serverConnected
	f.sb.stream <- stepStarted
	f.sb.stream <- inboxEnqueued
	waitFor(t, "two rows", func() bool { return len(f.openCodeEvents()) == 2 })
	evs := f.openCodeEvents()
	if evs[0].Seq != 46 || evs[0].Name != "session.step.started" || !evs[0].At.Equal(time.UnixMilli(1788265407953).UTC()) {
		t.Errorf("step = %+v", evs[0])
	}
	if evs[1].Seq != 30 || evs[1].Text != "reply with ok" {
		t.Errorf("prompt = %+v", evs[1])
	}
	convs, _ := f.st.Conversations().ListConversations(ctx, "org", f.sess.ID)
	if len(convs) != 2 || evs[0].ConversationID == evs[1].ConversationID {
		t.Errorf("two conversations expected, one per aggregate: %+v", convs)
	}
	if string(evs[0].Raw) != stepStarted {
		t.Error("the raw frame was not kept verbatim")
	}
}

// Final reads every conversation once as the given principal and never
// opens a stream.
//
// @decision(DL-017)
func TestFinalReadsOnceAsTheDeletingPrincipal(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.listing = `{"data":[{"id":"ses_1","title":"last words"}]}`
	f.sb.messages["ses_1"] = twoMessages
	who := auth.Principal{UserID: "u1", OrgID: "org"}
	f.rec.Final(context.Background(), who, f.sess)
	if c, d, s, m := f.sb.counts(); c != 1 || d != 0 || s != 1 || m != 1 {
		t.Errorf("final read: connects=%d dials=%d listings=%d message reads=%d", c, d, s, m)
	}
	if len(f.sb.connectAs) != 1 || f.sb.connectAs[0].UserID != "u1" {
		t.Errorf("connected as %+v, want the deleting principal", f.sb.connectAs)
	}
	if msgs := f.messages(); len(msgs) != 2 {
		t.Errorf("messages = %+v", msgs)
	}
}

// Delete runs OnStop before the sandbox is terminated, and only for a
// running session with a sandbox.
//
// @decision(DL-017)
func TestDeleteReadsBeforeItTerminates(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	var sawRunning atomic.Bool
	f.srv.Service().OnStop = func(ctx context.Context, p auth.Principal, s store.Session) {
		if s.ID != sess.ID || p.UserID != auth.LocalID {
			t.Errorf("OnStop got %s as %s", s.ID, p.UserID)
		}
		h, err := f.provider.Get(ctx, sess.SandboxID)
		if err != nil {
			t.Errorf("the sandbox was gone before OnStop ran: %v", err)
			return
		}
		info, _ := h.Info(ctx)
		sawRunning.Store(info.State == "running")
	}
	if err := f.srv.Service().Delete(context.Background(), auth.Local(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if !sawRunning.Load() {
		t.Error("OnStop did not see a running sandbox")
	}
}

// The recorder mints under its own principal, so the audit row names it.
//
// @decision(DL-017)
func TestTheRecorderMintsAsItself(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	rec := NewRecorder(f.srv.Service(), f.store, time.Now, nil)
	conn, err := rec.connect(context.Background(), auth.Recorder(sess.OrgID), sess)
	if err != nil {
		t.Fatal(err)
	}
	if conn.URL == "" || conn.Token == "" {
		t.Errorf("connection = %+v", conn)
	}
	mints, err := f.store.Mints().ListBySession(context.Background(), sess.OrgID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mints) == 0 || mints[len(mints)-1].UserID != auth.RecorderID {
		t.Errorf("mints = %+v, want the last one by %q", mints, auth.RecorderID)
	}
}

// Run syncs on its interval and stops every capture when its context
// ends.
//
// @decision(DL-017)
func TestRunSyncsAndStopsOnCancel(t *testing.T) {
	f := newRecorderFixture(t)
	f.rec.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.rec.Run(ctx) }()
	waitFor(t, "the first sync's attach", func() bool { c, _, _, _ := f.sb.counts(); return c == 1 })

	late := store.Session{ID: store.NewID(time.Now()), OrgID: "org", OwnerID: "u1", Name: "late",
		State: store.StateRunning, SandboxID: "sb-3"}
	if err := f.st.Sessions().Create(ctx, late); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the late session to be picked up", func() bool { return f.rec.Recording(late.ID) })

	// Many more syncs: a session already captured is never touched
	// again. This is where a poll of the sandbox would be added, and
	// the count is what would move.
	time.Sleep(20 * f.rec.interval)
	if c, d, _, _ := f.sb.counts(); c != 2 || d != 2 {
		t.Errorf("repeated syncs reached the sandbox: connects=%d dials=%d, want 2 and 2", c, d)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if f.rec.Recording(f.sess.ID) || f.rec.Recording(late.ID) {
		t.Error("captures survived Run's end")
	}
}

// Frames that arrive while the catch-up read is in flight are queued
// and written after it, and a span that ends during the catch-up takes
// its reader with it.
//
// @decision(DL-017)
func TestFramesDuringCatchUpAreKept(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.listing = `{"data":[{"id":"ses_1"}]}`
	f.sb.messages["ses_1"] = twoMessages
	// The listing read blocks until a frame has been sent on the
	// stream, so the frame necessarily arrives mid-catch-up.
	release := make(chan struct{})
	inner := f.rec.fetch
	f.rec.fetch = func(ctx context.Context, conn Connection, path string) ([]byte, error) {
		if path == "/api/session" {
			<-release
		}
		return inner(ctx, conn, path)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the dial", func() bool { _, d, _, _ := f.sb.counts(); return d == 1 })
	f.sb.stream <- durableFrameFor("session.step.started", "ses_1", 7, "")
	time.Sleep(20 * time.Millisecond)
	if evs := f.openCodeEvents(); len(evs) != 0 {
		t.Fatalf("a frame was written before the catch-up finished: %+v", evs)
	}
	close(release)
	waitFor(t, "the queued frame", func() bool { return len(f.openCodeEvents()) == 1 })
	if msgs := f.messages(); len(msgs) != 2 {
		t.Errorf("messages = %+v", msgs)
	}

	// A span that ends while frames are queued releases its reader.
	before := runtime.NumGoroutine()
	f.sb.breakStream()
	waitFor(t, "the redial", func() bool { _, d, _, _ := f.sb.counts(); return d == 2 })
	cancel()
	f.rec.stopAll()
	waitFor(t, "goroutines to settle", func() bool { return runtime.NumGoroutine() <= before })
}

// A sandbox that produces faster than the recorder can store during a
// catch-up ends the span as a break rather than growing without bound.
//
// @decision(DL-017)
func TestAFloodDuringCatchUpIsABreak(t *testing.T) {
	f := newRecorderFixture(t)
	release := make(chan struct{})
	inner := f.rec.fetch
	f.rec.fetch = func(ctx context.Context, conn Connection, path string) ([]byte, error) {
		<-release
		return inner(ctx, conn, path)
	}
	q := newFrameQueue()
	big := strings.Repeat("x", RecorderBufferBytes/2+1)
	if !q.push(big) {
		t.Fatal("the first half-buffer frame was refused")
	}
	if q.push(big) {
		t.Fatal("a frame past the bound was accepted")
	}
	q.close(nil)
	if raw, ok, _ := q.next(context.Background()); !ok || raw != big {
		t.Error("the queued frame was lost")
	}
	if _, ok, err := q.next(context.Background()); ok || err != nil {
		t.Errorf("after close: ok=%v err=%v", ok, err)
	}
	close(release)
}

// unreadableConversations wraps a store so one read fails on demand.
type unreadableConversations struct {
	store.ConversationStore
	events bool
}

func (u unreadableConversations) LastSeqs(ctx context.Context, org, sess string) (map[string]int64, error) {
	if u.events {
		return nil, errors.New("db down")
	}
	return u.ConversationStore.LastSeqs(ctx, org, sess)
}

func (u unreadableConversations) ListEvents(ctx context.Context, org, sess string, after int64, limit int) ([]store.Event, error) {
	if u.events {
		return nil, errors.New("db down")
	}
	return u.ConversationStore.ListEvents(ctx, org, sess, after, limit)
}

// captureOf is a capture as attach and catchUp receive one, for the
// tests that drive those directly rather than through a span.
func (f *recorderFixture) captureOf() *capture {
	return newCapture(f.sess,
		Connection{URL: "http://sandbox.test", Token: "tok", User: "opencode", Password: "pw"})
}

func (f *recorderFixture) warnings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.warns...)
}

// sleepFor is the real backoff, which every other test replaces. It
// waits, and a cancelled context cuts the wait short rather than
// letting a stopped recorder sit through its longest backoff.
//
// @decision(DL-017)
func TestSleepForWaitsUnlessTheContextEnds(t *testing.T) {
	if err := sleepFor(context.Background(), time.Millisecond); err != nil {
		t.Errorf("a completed sleep returned %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepFor(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled sleep returned %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("a cancelled sleep took %s: it waited out the duration", d)
	}
}

// A conversation the sandbox will not hand over is skipped with a
// warning, and the ones that can be read are still stored: one
// unreadable conversation must not cost the whole catch-up.
//
// @decision(DL-017)
func TestAConversationThatCannotBeReadIsSkipped(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.listing = `{"data":[{"id":"ses_good","title":"kept"},{"id":"ses_bad","title":"gone"}]}`
	f.sb.messages["ses_good"] = twoMessages

	if err := f.rec.catchUp(context.Background(), f.captureOf()); err != nil {
		t.Fatalf("catchUp stopped on one unreadable conversation: %v", err)
	}
	if msgs := f.messages(); len(msgs) != 2 {
		t.Errorf("stored %d messages, want the readable conversation's 2", len(msgs))
	}
	var said bool
	for _, w := range f.warnings() {
		if strings.Contains(w, "ses_bad") {
			said = true
		}
	}
	if !said {
		t.Errorf("nothing warned about the unreadable conversation: %v", f.warnings())
	}
}

// The conversation count comes from the sandbox, which is the untrusted
// side, so it is capped rather than believed.
//
// @decision(DL-017)
func TestTheSandboxesConversationListIsCapped(t *testing.T) {
	f := newRecorderFixture(t)
	var entries []string
	for i := 0; i < MaxConversations+4; i++ {
		id := fmt.Sprintf("ses_%d", i)
		entries = append(entries, fmt.Sprintf(`{"id":%q,"title":"c"}`, id))
		f.sb.messages[id] = twoMessages
	}
	f.sb.listing = `{"data":[` + strings.Join(entries, ",") + `]}`

	if err := f.rec.catchUp(context.Background(), f.captureOf()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, reads := f.sb.counts(); reads != MaxConversations {
		t.Errorf("read %d conversations, want the cap of %d", reads, MaxConversations)
	}
	var said bool
	for _, w := range f.warnings() {
		if strings.Contains(w, "conversations") {
			said = true
		}
	}
	if !said {
		t.Errorf("the cap was applied silently: %v", f.warnings())
	}
}

// A sandbox that will not list its conversations ends the catch-up
// rather than leaving the store looking complete.
//
// @decision(DL-017)
func TestCatchUpStopsWhenTheSandboxCannotBeListed(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.listErr = errors.New("connection refused")
	err := f.rec.catchUp(context.Background(), f.captureOf())
	if err == nil {
		t.Fatal("catchUp reported success when it could not list anything")
	}
	if !strings.Contains(err.Error(), "list conversations") {
		t.Errorf("error %q does not say what failed", err)
	}
}

// attach reads where the store's recording stopped before it judges a
// gap, and a store it cannot read ends the span rather than judging the
// recording continuous.
//
// @decision(DL-017)
func TestAttachReportsAStoreItCannotRead(t *testing.T) {
	f := newRecorderFixture(t)
	convs := unreadableConversations{events: true}
	convs.ConversationStore = f.st.Conversations()
	f.rec.store = storeWith{Store: f.st, convs: convs}
	err := f.rec.attach(context.Background(), f.captureOf())
	if err == nil {
		t.Fatal("attach reported success over an unreadable store")
	}
	if !strings.Contains(err.Error(), "read the store") {
		t.Errorf("error %q does not say what failed", err)
	}
}

// A span that begins mid-log marks a gap and names how many events it
// missed, judged from the sandbox's durable sequence rather than the store.
//
// @decision(DL-017)
func TestASpanThatBeginsMidLogMarksAGap(t *testing.T) {
	f := newRecorderFixture(t)
	c := f.captureOf()
	if err := f.rec.attach(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.events() {
		if e.Name == MarkerGap {
			t.Fatal("a gap was marked at attach; nothing at that point can know one")
		}
	}
	if err := f.rec.frame(context.Background(), c,
		durableFrameFor("session.reasoning.ended", "oc-1", 59, "")); err != nil {
		t.Fatal(err)
	}
	var marker store.Event
	for _, e := range f.events() {
		if e.Name == MarkerGap {
			marker = e
		}
	}
	if marker.ID == "" {
		t.Fatal("no gap was marked for a span whose first frame sat at sequence 59:" +
			" 58 events are lost for good, and a reader would take the transcript as complete")
	}
	// The marker names the count, not only that something was missed.
	if !strings.Contains(marker.Text, "58") {
		t.Errorf("the marker says %q; it does not say how many events were missed", marker.Text)
	}
}

// A span that begins at the start of the log marks no gap, as for a job
// whose recorder was nudged before the goal went out.
//
// @decision(DL-017)
func TestASpanThatBeginsAtTheStartOfTheLogMarksNoGap(t *testing.T) {
	f := newRecorderFixture(t)
	c := f.captureOf()
	// The conversation row exists, as a job's does: the supervisor
	// opens it before the recorder attaches.
	if _, err := f.st.Conversations().UpsertConversation(context.Background(), store.Conversation{
		ID: store.NewID(time.Now()), OrgID: f.sess.OrgID, SessionID: f.sess.ID, Title: "c",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.attach(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	// Two frames: the check must be recorded as done before the
	// sequence is judged, or the second frame (seq 2) reads as a gap.
	for i, name := range []string{"session.model.selected", "session.inbox.enqueued"} {
		if err := f.rec.frame(context.Background(), c,
			durableFrameFor(name, "oc-1", i+1, "")); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range f.events() {
		if e.Name == MarkerGap {
			t.Fatalf("a gap was marked for a span that carried the log from its first"+
				" event: %q, on a session whose conversation row was merely already there", e.Text)
		}
	}
}

// An ephemeral frame, whose sequence the recorder forces to zero, does
// not spend a conversation's one gap check.
//
// @decision(DL-017)
func TestAnEphemeralFrameDoesNotSpendTheGapCheck(t *testing.T) {
	f := newRecorderFixture(t)
	c := f.captureOf()
	if err := f.rec.attach(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ephemeral := `{"type":"permission.asked","data":{"sessionID":"oc-1"}}`
	if err := f.rec.frame(context.Background(), c, ephemeral); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.frame(context.Background(), c,
		durableFrameFor("session.reasoning.ended", "oc-1", 59, "")); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.events() {
		if e.Name == MarkerGap {
			return
		}
	}
	t.Error("an ephemeral frame arriving first suppressed the gap check for its conversation")
}

// One marker per span, however many conversations began mid-log: the
// marker is the session's, not a conversation's.
//
// @decision(DL-017)
func TestASpanMarksAtMostOneGap(t *testing.T) {
	f := newRecorderFixture(t)
	c := f.captureOf()
	if err := f.rec.attach(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, ocid := range []string{"oc-1", "oc-2"} {
		if err := f.rec.frame(context.Background(), c,
			durableFrameFor("session.reasoning.ended", ocid, 40, "")); err != nil {
			t.Fatal(err)
		}
	}
	var markers int
	for _, e := range f.events() {
		if e.Name == MarkerGap {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("a span with two mid-log conversations wrote %d markers, want 1", markers)
	}
}

// The count agrees with its number.
//
// @decision(DL-017)
func TestTheGapCountAgreesWithItsNumber(t *testing.T) {
	if got := missedCount(1); got != "1 earlier event" {
		t.Errorf("missedCount(1) = %q", got)
	}
	if got := missedCount(58); got != "58 earlier events" {
		t.Errorf("missedCount(58) = %q", got)
	}
}

// The gap marker is stamped with the attach time, not the time it is
// written, so it sorts before the frames this span had already stored.
//
// @decision(DL-017)
func TestTheGapMarkerIsStampedAtTheAttachTime(t *testing.T) {
	f := newRecorderFixture(t)
	c := f.captureOf()
	c.t0 = time.Now().Add(-90 * time.Second).UTC()
	if err := f.rec.attach(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.frame(context.Background(), c,
		durableFrameFor("session.reasoning.ended", "oc-1", 12, "")); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.events() {
		if e.Name != MarkerGap {
			continue
		}
		if !e.At.Equal(c.t0) {
			t.Errorf("the marker is stamped %s; the attach was at %s, and a marker"+
				" stamped later sorts below the messages the catch-up recovered", e.At, c.t0)
		}
		return
	}
	t.Fatal("no gap was marked")
}

// recordedUpTo stores durable frames 1 to n of one conversation, as an
// earlier span would have, and returns the conversation.
func (f *recorderFixture) recordedUpTo(t *testing.T, ocid string, n int) store.Conversation {
	t.Helper()
	conv, err := f.st.Conversations().UpsertConversation(context.Background(), store.Conversation{
		ID: store.NewID(time.Now()), OrgID: f.sess.OrgID, SessionID: f.sess.ID, OpenCodeID: ocid,
	})
	if err != nil {
		t.Fatal(err)
	}
	for seq := 1; seq <= n; seq++ {
		if _, err := f.st.Conversations().PutEvents(context.Background(), []store.Event{{
			ID: store.NewID(time.Now()), OrgID: f.sess.OrgID, SessionID: f.sess.ID,
			ConversationID: conv.ID, Source: store.SourceOpenCode, Seq: int64(seq),
			Name: "session.step.started", At: time.Now(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return conv
}

func (f *recorderFixture) gapMarkers() []store.Event {
	var out []store.Event
	for _, e := range f.events() {
		if e.Name == MarkerGap {
			out = append(out, e)
		}
	}
	return out
}

// A span after a recorded one, as after a restart, marks nothing at
// attach, and nothing when its first frame follows the stored log.
//
// @decision(DL-017)
func TestASpanThatContinuesTheStoredLogMarksNoGap(t *testing.T) {
	f := newRecorderFixture(t)
	f.recordedUpTo(t, "oc-1", 20)
	c := f.captureOf()
	if err := f.rec.attach(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if m := f.gapMarkers(); len(m) != 0 {
		t.Fatalf("a gap was marked at attach: %q", m[0].Text)
	}
	if err := f.rec.frame(context.Background(), c, durableFrameFor("session.tool.success", "oc-1", 21, "")); err != nil {
		t.Fatal(err)
	}
	if m := f.gapMarkers(); len(m) != 0 {
		t.Errorf("a span that resumed at sequence 21 after 20 stored marked %q; nothing was missed", m[0].Text)
	}
}

// A span after a recorded one whose first frame skips past the stored
// log marks a gap, names how many were missed, and stamps it at attach.
//
// @decision(DL-017)
func TestASpanThatSkipsPastTheStoredLogMarksAGap(t *testing.T) {
	f := newRecorderFixture(t)
	f.recordedUpTo(t, "oc-1", 20)
	c := f.captureOf()
	c.t0 = time.Now().Add(-time.Minute).UTC()
	if err := f.rec.attach(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.frame(context.Background(), c, durableFrameFor("session.tool.success", "oc-1", 26, "")); err != nil {
		t.Fatal(err)
	}
	m := f.gapMarkers()
	if len(m) != 1 {
		t.Fatalf("%d gap markers, want 1: five frames were lost between the spans", len(m))
	}
	if !strings.Contains(m[0].Text, "5 earlier events") || !m[0].At.Equal(c.t0) {
		t.Errorf("the marker says %q at %v; want 5 earlier events at the attach time %v", m[0].Text, m[0].At, c.t0)
	}
}

// A conversation that began while nobody listened is judged from
// sequence 1, beside one that was recorded.
//
// @decision(DL-017)
func TestANewConversationAfterARestartIsJudgedFromItsStart(t *testing.T) {
	f := newRecorderFixture(t)
	f.recordedUpTo(t, "oc-1", 5)
	c := f.captureOf()
	if err := f.rec.attach(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.frame(context.Background(), c, durableFrameFor("session.step.started", "oc-1", 6, "")); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.frame(context.Background(), c, durableFrameFor("session.step.started", "oc-2", 3, "")); err != nil {
		t.Fatal(err)
	}
	if m := f.gapMarkers(); len(m) != 1 || !strings.Contains(m[0].Text, "2 earlier events") {
		t.Errorf("markers %v; want one naming the 2 frames of oc-2 nobody recorded", m)
	}
}

// A first attach to a session with nothing recorded marks no gap at
// attach.
//
// @decision(DL-017)
func TestAFirstAttachToAnUntouchedSessionMarksNoGap(t *testing.T) {
	f := newRecorderFixture(t)
	if err := f.rec.attach(context.Background(), f.captureOf()); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.events() {
		if e.Name == MarkerGap {
			t.Fatal("a gap was marked for a session with nothing recorded and no conversation")
		}
	}
}

// The final read is best effort: a sandbox that will not connect is a
// warning, never a reason to hold up the delete that asked for it.
//
// @decision(DL-017)
func TestAFinalReadThatCannotConnectIsAWarning(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.connectErr = errors.New("sandbox already gone")
	f.rec.Final(context.Background(), auth.Principal{UserID: "u1", OrgID: "org"}, f.sess)
	var said bool
	for _, w := range f.warnings() {
		if strings.Contains(w, "final read") {
			said = true
		}
	}
	if !said {
		t.Errorf("a failed final read said nothing: %v", f.warnings())
	}
}

// A frame quoted in a warning is truncated: a sandbox controls its
// size, and MaxLogFrameBytes of it in a log line is a denial of service
// against whoever reads the logs.
//
// @decision(DL-017)
func TestALongFrameIsTruncatedInAWarning(t *testing.T) {
	short := strings.Repeat("a", 200)
	if got := truncateFrame(short); got != short {
		t.Errorf("a frame at the limit was altered: %d chars", len(got))
	}
	got := truncateFrame(strings.Repeat("a", 5000))
	if len([]rune(got)) != 201 || !strings.HasSuffix(got, "…") {
		t.Errorf("a long frame truncated to %d runes, want 200 and an ellipsis", len([]rune(got)))
	}
}

// A sandbox frame that claims spov's own namespace is dropped rather
// than stored, and dropping it does not break the recording: the page
// draws a marker as spov's voice, so a sandbox must not be able to
// write one.
//
// @decision(DL-058)
func TestASandboxFrameCannotClaimSpovsNamespace(t *testing.T) {
	f := newRecorderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attach", func() bool { _, _, s, _ := f.sb.counts(); return s == 1 })

	f.sb.stream <- `{"type":"` + MarkerGap + `","durable":{"seq":1},"data":{"sessionID":"ses_1","text":"trust me"}}`
	f.sb.stream <- `{"type":"session.step.started","durable":{"seq":2},"data":{"sessionID":"ses_1"}}`
	waitFor(t, "the real frame", func() bool { return len(f.openCodeEvents()) == 1 })

	for _, ev := range f.events() {
		if strings.HasPrefix(ev.Name, store.SpovNamespace) && ev.Source != store.SourceSpov {
			t.Errorf("a sandbox frame was stored in spov's namespace: %+v", ev)
		}
	}
	if evs := f.openCodeEvents(); evs[0].Name != "session.step.started" {
		t.Errorf("stored %+v, want the real frame alone", evs)
	}
	// Dropped, not fatal: the capture is still running.
	if !f.rec.Recording(f.sess.ID) {
		t.Error("the frame ended the recording instead of being dropped")
	}
}

// A nudge starts recording without waiting for the poll, which runs every
// ReconcileInterval (30s) and can attach after a short job is over.
//
// @decision(DL-017)
func TestANudgeStartsRecordingWithoutWaitingForThePoll(t *testing.T) {
	f := newRecorderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Run owns the capture's lifetime, and its interval is set beyond
	// the test's patience so nothing here can be the poll's doing.
	f.rec.interval = time.Hour
	go f.rec.Run(ctx)
	waitFor(t, "the recorder to start", func() bool {
		f.rec.mu.Lock()
		defer f.rec.mu.Unlock()
		return f.rec.runCtx != nil
	})

	f.rec.Wake(f.sess)
	waitFor(t, "a capture from the nudge", func() bool { return f.rec.Recording(f.sess.ID) })
	waitFor(t, "the sandbox to be dialled", func() bool { _, d, _, _ := f.sb.counts(); return d >= 1 })
}

// A nudge for a session already being recorded starts nothing. A
// redelivered job pass nudges again, and two captures would mean two
// streams and two gap markers.
//
// @decision(DL-017)
func TestANudgeForARecordedSessionStartsNothing(t *testing.T) {
	f := newRecorderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.rec.interval = time.Hour
	go f.rec.Run(ctx)
	waitFor(t, "the recorder to start", func() bool {
		f.rec.mu.Lock()
		defer f.rec.mu.Unlock()
		return f.rec.runCtx != nil
	})

	f.rec.Wake(f.sess)
	waitFor(t, "the first capture", func() bool { _, d, _, _ := f.sb.counts(); return d >= 1 })
	_, dialsAfterFirst, _, _ := f.sb.counts()

	for range 5 {
		f.rec.Wake(f.sess)
	}
	// Nothing to wait for, so the assertion is that the count did not
	// move while the extra nudges had every chance to act.
	time.Sleep(50 * time.Millisecond)
	if _, d, _, _ := f.sb.counts(); d != dialsAfterFirst {
		t.Errorf("dials = %d after five extra nudges, want %d: a repeated nudge started a "+
			"second capture, which means two streams and two gap markers", d, dialsAfterFirst)
	}
}

// A nudge before Run does nothing. A capture hangs from Run's context,
// not the caller's (a job pass that ends in seconds), so without Run there
// is nothing to own it. Local mode and most tests are in this state.
//
// @decision(DL-017)
func TestANudgeBeforeRunStartsNothing(t *testing.T) {
	f := newRecorderFixture(t)
	f.rec.Wake(f.sess)
	if f.rec.Recording(f.sess.ID) {
		t.Error("a nudge started a capture with no Run to own it; it would be cancelled " +
			"as soon as the caller's pass returned")
	}
	if _, d, _, _ := f.sb.counts(); d != 0 {
		t.Errorf("the sandbox was dialled %d times with no Run", d)
	}
}

// A nudge for a session that is not running, or has no sandbox, does
// nothing, as Sync would not record it.
//
// @decision(DL-017)
func TestANudgeForAnUnrecordableSessionStartsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		sess store.Session
	}{
		{"not running", store.Session{ID: "s1", OrgID: "org", State: store.StateStarting, SandboxID: "sb-1"}},
		{"no sandbox", store.Session{ID: "s2", OrgID: "org", State: store.StateRunning}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecorderFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.rec.interval = time.Hour
			go f.rec.Run(ctx)
			waitFor(t, "the recorder to start", func() bool {
				f.rec.mu.Lock()
				defer f.rec.mu.Unlock()
				return f.rec.runCtx != nil
			})

			f.rec.Wake(tc.sess)
			time.Sleep(50 * time.Millisecond)
			if f.rec.Recording(tc.sess.ID) {
				t.Errorf("a %s session was recorded", tc.name)
			}
		})
	}
}

// A launch nudges the recorder, so a short interactive session does not
// finish inside the poll window.
//
// @decision(DL-017)
func TestALaunchNudgesTheRecorder(t *testing.T) {
	f := newAPIFixture(t)
	var woke []string
	f.srv.Service().OnStart = func(s store.Session) { woke = append(woke, s.ID) }

	sess := runningSession(t, f)

	if len(woke) != 1 || woke[0] != sess.ID {
		t.Fatalf("the launch woke %v, want just %s: without this the recorder waits"+
			" for its next poll, which is longer than a short session lives", woke, sess.ID)
	}
}

// The nudge carries a committed session: `running` with a sandbox id,
// which is what Sync requires before it attaches.
//
// @decision(DL-017)
func TestTheNudgeCarriesACommittedSession(t *testing.T) {
	f := newAPIFixture(t)
	var got store.Session
	f.srv.Service().OnStart = func(s store.Session) { got = s }

	runningSession(t, f)

	if got.State != store.StateRunning {
		t.Errorf("the nudge carried state %q, want running", got.State)
	}
	if got.SandboxID == "" {
		t.Error("the nudge carried no sandbox id, so the recorder would have nothing to attach to")
	}
}

// A deployment with no recorder launches without one. Nil is a
// working configuration: local mode, and every test that has neither.
//
// @decision(DL-017)
func TestALaunchWithNoRecorderIsUnaffected(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.Service().OnStart = nil
	if sess := runningSession(t, f); sess.State != store.StateRunning {
		t.Errorf("state = %s, want running", sess.State)
	}
}

// A steering marker is spov's own row, not part of the sandbox's log, so
// it moves no conversation's stored sequence.
//
// @decision(DL-017) @decision(DL-019)
func TestASteeringMarkerIsNotEvidenceOfAnEarlierRecorder(t *testing.T) {
	f := newRecorderFixture(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := f.st.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: f.sess.OrgID, SessionID: f.sess.ID,
		Source: store.SourceSpov, Name: MarkerSteered, Text: "message sent by user_1", At: now,
	}}); err != nil {
		t.Fatal(err)
	}

	c := f.captureOf()
	if err := f.rec.attach(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.frame(ctx, c, durableFrameFor("session.model.selected", "oc-1", 1, "")); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.events() {
		if e.Name == MarkerGap {
			t.Fatalf("a gap was marked for a span that carried the log from its first"+
				" event: %q, on a session whose only stored row was spov's own record"+
				" of a message somebody sent", e.Text)
		}
	}
}
