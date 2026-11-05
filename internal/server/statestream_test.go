package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// recorder collects what a stream handler sent.
type recorder struct {
	mu       sync.Mutex
	data     []any
	comments int
	err      error
}

func (r *recorder) Data(v any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.data = append(r.data, v)
	return nil
}

func (r *recorder) Comment(string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.comments++
	return nil
}

func (r *recorder) snapshot() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.data...)
}

func sess(id string, state store.SessionState, updated time.Time) store.Session {
	return store.Session{ID: id, State: state, UpdatedAt: updated}
}

// TestTheFeedSendsOnlyWhatChanged: the browser holds one stream for the
// whole list, so a feed that re-sent an unchanged list every two seconds
// would be a poll with extra steps.
//
// @decision(DL-018)
func TestTheFeedSendsOnlyWhatChanged(t *testing.T) {
	at := time.Unix(1700000000, 0)
	var mu sync.Mutex
	current := []store.Session{sess("a", store.StateStarting, at)}
	feed := pollFeed{
		interval: time.Millisecond,
		list: func(context.Context, auth.Principal, int) ([]store.Session, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]store.Session(nil), current...), nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := feed.Subscribe(ctx, auth.Principal{}, DefaultPageSize)
	if err != nil {
		t.Fatal(err)
	}
	// The first send is immediate: a stream that waited an interval
	// before saying anything would render an empty list first.
	first := <-ch
	if len(first) != 1 || first[0].State != store.StateStarting {
		t.Fatalf("first = %+v", first)
	}
	// Nothing changes, so nothing is sent.
	select {
	case got := <-ch:
		t.Fatalf("an unchanged list was sent again: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
	mu.Lock()
	current = []store.Session{sess("a", store.StateRunning, at.Add(time.Second))}
	mu.Unlock()
	select {
	case got := <-ch:
		if len(got) != 1 || got[0].State != store.StateRunning {
			t.Errorf("after the change = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the change was never sent")
	}
}

// A first read that fails is the caller's answer, not an empty list:
// "you have no sessions" and "the store is unreachable" must not look
// the same.
//
// @decision(DL-018)
func TestTheFeedRefusesToOpenOnAFailedFirstRead(t *testing.T) {
	feed := pollFeed{interval: time.Millisecond, list: func(context.Context, auth.Principal, int) ([]store.Session, error) {
		return nil, errors.New("store down")
	}}
	if _, err := feed.Subscribe(context.Background(), auth.Principal{}, DefaultPageSize); err == nil {
		t.Fatal("Subscribe opened a stream over a failed read")
	}
}

// A later read that fails is transient: the stream survives it and the
// next tick carries the truth.
//
// @decision(DL-018)
func TestTheFeedSurvivesATransientReadFailure(t *testing.T) {
	at := time.Unix(1700000000, 0)
	var calls int
	var mu sync.Mutex
	feed := pollFeed{
		interval: time.Millisecond,
		warn:     func(string) {},
		list: func(context.Context, auth.Principal, int) ([]store.Session, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			switch {
			case calls == 1:
				return []store.Session{sess("a", store.StateStarting, at)}, nil
			case calls < 5:
				return nil, errors.New("blip")
			}
			return []store.Session{sess("a", store.StateRunning, at.Add(time.Second))}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := feed.Subscribe(ctx, auth.Principal{}, DefaultPageSize)
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	select {
	case got := <-ch:
		if got[0].State != store.StateRunning {
			t.Errorf("got %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the feed did not recover from a transient failure")
	}
}

// Cancelling the stream stops the goroutine reading for it.
//
// @decision(DL-018)
func TestTheFeedStopsWithItsContext(t *testing.T) {
	feed := pollFeed{interval: time.Millisecond, list: func(context.Context, auth.Principal, int) ([]store.Session, error) {
		return nil, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := feed.Subscribe(ctx, auth.Principal{}, DefaultPageSize)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed: the goroutine returned
			}
		case <-deadline:
			t.Fatal("the feed goroutine outlived its context")
		}
	}
}

// fingerprint decides what "changed" means, so every field the list
// renders has to be in it.
//
// @decision(DL-018)
func TestFingerprintNoticesWhatTheListShows(t *testing.T) {
	at := time.Unix(1700000000, 0)
	base := []store.Session{{ID: "a", State: store.StateStarting, UpdatedAt: at}}
	same := []store.Session{{ID: "a", State: store.StateStarting, UpdatedAt: at}}
	if fingerprint(base) != fingerprint(same) {
		t.Error("two identical lists fingerprinted differently")
	}
	for name, mut := range map[string]func(*store.Session){
		"state":     func(s *store.Session) { s.State = store.StateRunning },
		"updatedAt": func(s *store.Session) { s.UpdatedAt = at.Add(time.Nanosecond) },
		"sandbox":   func(s *store.Session) { s.SandboxID = "sb-1" },
		"error":     func(s *store.Session) { s.Error = "boom" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := []store.Session{{ID: "a", State: store.StateStarting, UpdatedAt: at}}
			mut(&changed[0])
			if fingerprint(base) == fingerprint(changed) {
				t.Errorf("a change to %s did not move the fingerprint", name)
			}
		})
	}
}

// The log feed drains what is stored in pages before it waits: a client
// that opens from the beginning of a long session gets it all without
// one page every two seconds.
//
// @decision(DL-018)
func TestTheLogFeedDrainsInPagesThenTails(t *testing.T) {
	rows := make([]store.Event, 5)
	for i := range rows {
		rows[i] = store.Event{ID: string(rune('a' + i)), Pos: int64(i + 1)}
	}
	var cursors []int64
	f := pollEvents{page: 2, interval: time.Hour, list: func(_ context.Context, _, _ string, after int64, limit int) ([]store.Event, error) {
		cursors = append(cursors, after)
		var out []store.Event
		for _, r := range rows {
			if r.Pos > after && len(out) < limit {
				out = append(out, r)
			}
		}
		return out, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := f.Subscribe(ctx, "org", "s1", 0)
	if err != nil {
		t.Fatal(err)
	}

	var seen []store.Event
	for i := 0; i < 3; i++ {
		batch := <-ch
		seen = append(seen, batch.Events...)
		// The interval is an hour, so a full page must not have waited.
		want := i == 2
		if batch.CaughtUp != want {
			t.Errorf("batch %d caught up = %v, want %v", i, batch.CaughtUp, want)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("read %d rows, want all five", len(seen))
	}
	if cursors[1] != 2 || cursors[2] != 4 {
		t.Errorf("cursors = %v, want each page to resume after the last row", cursors)
	}
}

// A read that failed is not the end of the log. Reporting it as one
// would end a running session's stream, because being caught up is
// exactly what the handler ends on.
//
// @decision(DL-018)
func TestAFailedLogReadIsNotTheEndOfTheLog(t *testing.T) {
	var calls atomic.Int32
	f := pollEvents{page: 2, interval: time.Millisecond, warn: func(string) {},
		list: func(_ context.Context, _, _ string, after int64, _ int) ([]store.Event, error) {
			switch calls.Add(1) {
			case 1:
				return []store.Event{{ID: "a", Pos: 1}, {ID: "b", Pos: 2}}, nil
			case 2:
				return nil, errors.New("store down")
			default:
				return []store.Event{{ID: "c", Pos: 3}}, nil
			}
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := f.Subscribe(ctx, "org", "s1", 0)
	if err != nil {
		t.Fatal(err)
	}

	first := <-ch
	if len(first.Events) != 2 || first.CaughtUp {
		t.Fatalf("first batch = %+v", first)
	}
	// The failed read produced nothing at all; the next good one
	// carries the row after the cursor.
	second := <-ch
	if len(second.Events) != 1 || second.Events[0].ID != "c" || !second.CaughtUp {
		t.Errorf("second batch = %+v, want the row after the failure", second)
	}
}

// A first read that fails is reported before the stream opens, so a
// caller does not mistake a broken store for an empty log.
//
// @decision(DL-018)
func TestTheLogFeedRefusesToOpenOnAFirstReadThatFails(t *testing.T) {
	f := pollEvents{list: func(context.Context, string, string, int64, int) ([]store.Event, error) {
		return nil, errors.New("store down")
	}}
	if _, err := f.Subscribe(context.Background(), "org", "s1", 0); err == nil {
		t.Error("the feed opened over a store it could not read")
	}
}

// A recorder writing a message touches no session row, so without the
// stamp an open list would show a last message that never changes.
//
// @decision(DL-018)
func TestTheFeedNoticesANewLastMessage(t *testing.T) {
	f := newAPIFixture(t)
	talky := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "talky", store.StateRunning)
	conv := seedConversation(t, f, talky, "ses_1", "one")
	seedMessage(t, f, conv, "msg_a", "user", "first", recordedAt)

	rec := &recorder{}
	ctx, cancel := context.WithCancel(principalCtx(auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID, Admin: true}))
	defer cancel()
	f.srv.feed = pollFeed{list: f.srv.svc.StreamList, stamp: f.srv.decorationStamp, interval: time.Millisecond}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.srv.streamSessions(ctx, nil, rec)
	}()
	waitFor(t, "the first list", func() bool { return len(rec.snapshot()) > 0 })

	// A reply that arrives in two reads is one row upserted twice, so
	// the stamp has to see the text and not only the identity.
	seedMessage(t, f, conv, "msg_a", "user", "first, corrected", recordedAt)
	waitFor(t, "the list to notice an edited message", func() bool {
		for _, ev := range rec.snapshot() {
			s, ok := ev.(SessionsEvent)
			if !ok || len(s.Sessions) == 0 {
				continue
			}
			if m := s.Sessions[0].LastMessage; m != nil && m.Text == "first, corrected" {
				return true
			}
		}
		return false
	})

	seedMessage(t, f, conv, "msg_b", "assistant", "the newer thing", recordedAt.Add(time.Minute))
	waitFor(t, "the list to notice the new message", func() bool {
		for _, ev := range rec.snapshot() {
			s, ok := ev.(SessionsEvent)
			if !ok || len(s.Sessions) == 0 {
				continue
			}
			if m := s.Sessions[0].LastMessage; m != nil && m.Text == "the newer thing" {
				return true
			}
		}
		return false
	})
	cancel()
	<-done
}
