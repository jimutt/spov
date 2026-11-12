package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// measuringStore counts the recorder's measures of a session, and can
// inflate what a message write reports, as a count that drifted would.
type measuringStore struct {
	store.Store
	measures *atomic.Int64
	inflate  int64
}

func (s measuringStore) Conversations() store.ConversationStore {
	return measuringConvs{s.Store.Conversations(), s.measures, s.inflate}
}

type measuringConvs struct {
	store.ConversationStore
	measures *atomic.Int64
	inflate  int64
}

func (c measuringConvs) StoredSize(ctx context.Context, o, s string) (store.StoredSize, error) {
	c.measures.Add(1)
	return c.ConversationStore.StoredSize(ctx, o, s)
}

func (c measuringConvs) PutMessages(ctx context.Context, msgs []store.Message) (store.MessageWrite, error) {
	w, err := c.ConversationStore.PutMessages(ctx, msgs)
	if w.Changed > 0 {
		w.Bytes += c.inflate
	}
	return w, err
}

// TestTheSessionCountIsTheBytesAWriteAdded is DL-131: one new large
// message read in a page of small ones the store already holds counts
// at its own size, so the recording stops at the session's limit rather
// than at about a page's worth of times the limit.
func TestTheSessionCountIsTheBytesAWriteAdded(t *testing.T) {
	f := newRecorderFixture(t)
	f.rec.maxBytes = 64 << 10
	f.sb.listing = `{"data":[{"id":"ses_1"}]}`
	var tiny []string
	for i := range 49 {
		// Far-future times, so they are always the newest page.
		tiny = append(tiny, fmt.Sprintf(`{"id":"msg_t%02d","time":{"created":%d},"type":"user","text":"t"}`, i, 1888266000000+int64(i)))
	}
	var bigs []string
	body := func() string { return messagesBody(append(append([]string{}, bigs...), tiny...)) }
	f.sb.mu.Lock()
	f.sb.messages["ses_1"] = body()
	f.sb.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.attached(ctx)
	waitFor(t, "the catch-up", func() bool { return len(f.messages()) == 49 })
	pad := strings.Repeat("x", 16<<10)
	for i := 1; i <= 40; i++ {
		f.sb.mu.Lock()
		bigs = append(bigs, fmt.Sprintf(`{"id":"msg_b%03d","time":{"created":%d},"type":"user","text":%q}`, i, 1788266000000+int64(i)*1000, pad))
		f.sb.messages["ses_1"] = body()
		f.sb.mu.Unlock()
		f.sb.stream <- durableFrameFor("session.execution.succeeded", "ses_1", i, "")
		want := 49 + i
		waitFor(t, "the read", func() bool { return len(f.messages()) == want || len(f.markers(MarkerStopped)) > 0 })
		if len(f.markers(MarkerStopped)) > 0 {
			break
		}
	}
	if len(f.markers(MarkerStopped)) != 1 {
		t.Fatal("the recording never stopped")
	}
	size, err := f.st.Conversations().StoredSize(ctx, "org", f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The check comes before a write, so one page may land past it.
	if limit := f.rec.maxBytes; size.Bytes > limit+40<<10 {
		t.Errorf("stored %d bytes against a limit of %d", size.Bytes, limit)
	}
}

// rewritePage is fifty messages of about 600 bytes, each rewritten in
// place by a new generation of the same length.
func rewritePage(gen int) string {
	pad := strings.Repeat("x", 600)
	var ms []string
	for i := range 50 {
		ms = append(ms, fmt.Sprintf(`{"id":"msg_%02d","time":{"created":%d},"type":"user","text":"%s%03d"}`, i, 1788266000000+int64(i), pad, gen))
	}
	return messagesBody(ms)
}

// replayRewrites sends gens turn ends, each over a page rewritten in
// place, and returns once each is stored or the recording stopped.
func replayRewrites(t *testing.T, f *recorderFixture, gens int) {
	t.Helper()
	for gen := 1; gen <= gens; gen++ {
		f.sb.mu.Lock()
		f.sb.messages["ses_1"] = rewritePage(gen)
		f.sb.mu.Unlock()
		f.sb.stream <- durableFrameFor("session.execution.succeeded", "ses_1", 1, "")
		want := fmt.Sprintf("%03d", gen)
		waitFor(t, "the rewrite", func() bool {
			ms := f.messages()
			return len(f.markers(MarkerStopped)) > 0 || (len(ms) > 0 && strings.HasSuffix(ms[0].Text, want))
		})
		if len(f.markers(MarkerStopped)) > 0 {
			return
		}
	}
}

// caughtUpBytes is what a session stores once the recorder has caught up
// with rewritePage(0), measured on a fixture of its own, so a test can
// set its limit before its recorder starts.
func caughtUpBytes(t *testing.T) int64 {
	t.Helper()
	f := newRecorderFixture(t)
	f.sb.listing = `{"data":[{"id":"ses_1"}]}`
	f.sb.messages["ses_1"] = rewritePage(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.attached(ctx)
	waitFor(t, "the catch-up", func() bool { return len(f.messages()) == 50 })
	f.rec.stopAll()
	size, err := f.st.Conversations().StoredSize(ctx, "org", f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	return size.Bytes
}

// TestRewrittenMessagesNeitherGrowTheCountNorMeasureTheStore is DL-131:
// a session held just under its limit whose messages are rewritten at
// the same size, replayed turn end after turn end, is not measured
// again, and is not stopped.
func TestRewrittenMessagesNeitherGrowTheCountNorMeasureTheStore(t *testing.T) {
	limit := caughtUpBytes(t) + 2000
	f := newRecorderFixture(t)
	f.rec.maxBytes = limit
	var n atomic.Int64
	f.rec.store = measuringStore{Store: f.st, measures: &n}
	f.sb.listing = `{"data":[{"id":"ses_1"}]}`
	f.sb.mu.Lock()
	f.sb.messages["ses_1"] = rewritePage(0)
	f.sb.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.attached(ctx)
	waitFor(t, "the catch-up", func() bool { return len(f.messages()) == 50 })
	before := n.Load()
	replayRewrites(t, f, 100)
	if got := n.Load() - before; got != 0 {
		t.Errorf("100 rewrites measured the store %d times", got)
	}
	if len(f.markers(MarkerStopped)) != 0 {
		t.Error("rewrites at the same size stopped the recording")
	}
}

// TestTheStoreIsMeasuredAtMostOncePerInterval is DL-131: a count at the
// limit is checked against the store at most once per RemeasureInterval
// of a capture, and believed in between.
func TestTheStoreIsMeasuredAtMostOncePerInterval(t *testing.T) {
	limit := caughtUpBytes(t) + 2500
	f := newRecorderFixture(t)
	var n atomic.Int64
	// Every write claims a kilobyte it did not add, so the count reaches
	// the limit while the store stays under it.
	f.rec.store = measuringStore{Store: f.st, measures: &n, inflate: 1 << 10}
	f.rec.maxBytes = limit
	var mu sync.Mutex
	clock := time.Now()
	f.rec.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	f.sb.listing = `{"data":[{"id":"ses_1"}]}`
	f.sb.mu.Lock()
	f.sb.messages["ses_1"] = rewritePage(0)
	f.sb.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.attached(ctx)
	waitFor(t, "the catch-up", func() bool { return len(f.messages()) == 50 })
	before := n.Load()
	// Past the attach's measure: the first time the count reaches the
	// limit it is measured and found under it; the next, inside the same
	// interval, is believed.
	mu.Lock()
	clock = clock.Add(RemeasureInterval)
	mu.Unlock()
	replayRewrites(t, f, 10)
	if got := n.Load() - before; got != 1 {
		t.Errorf("the store was measured %d times inside one interval, want once", got)
	}
	if len(f.markers(MarkerStopped)) != 1 {
		t.Error("a count believed at the limit did not stop the recording")
	}
}

// TestACaptureThatPanicsIsGivenUpLikeOneOutOfRedials is DL-135 and
// DL-017: a capture that panics marks its session abandoned for that
// sandbox and says so once in the log, so the next sync does not attach
// again, and again, for as long as the sandbox lives.
func TestACaptureThatPanicsIsGivenUpLikeOneOutOfRedials(t *testing.T) {
	f := newRecorderFixture(t)
	f.rec.fetch = func(context.Context, Connection, string) ([]byte, error) {
		panic("a frame the recorder did not expect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the note", func() bool { return len(f.markers(MarkerStopped)) == 1 })
	waitFor(t, "the capture's end", func() bool {
		f.rec.mu.Lock()
		defer f.rec.mu.Unlock()
		_, on := f.rec.captures[f.sess.ID]
		return !on
	})
	_, dials, _, _ := f.sb.counts()
	for range 3 {
		if err := f.rec.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, d, _, _ := f.sb.counts(); d != dials {
		t.Errorf("a capture that panicked was dialled again (%d, then %d)", dials, d)
	}
	if n := len(f.markers(MarkerStopped)); n != 1 {
		t.Errorf("the log holds %d stop notes, want one", n)
	}
	if text := f.markers(MarkerStopped)[0].Text; !strings.Contains(text, "failed") {
		t.Errorf("the note = %q", text)
	}
}
