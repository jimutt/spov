package store_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/storetest"
)

// TestMemoryMeetsTheContract.
//
// @decision(DL-003)
func TestMemoryMeetsTheContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}

// TestMemoryTxViewDiesWithTheTransaction: a retained view would bypass
// the lock, so it panics instead.
//
// @decision(DL-003)
func TestMemoryTxViewDiesWithTheTransaction(t *testing.T) {
	m := store.NewMemory()
	var leaked store.Store
	if err := m.Tx(context.Background(), func(tx store.Store) error { leaked = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Error("a view used after its transaction did not panic")
		}
	}()
	_, _ = leaked.Sessions().List(context.Background(), "org", store.SessionFilter{})
}

// TestNewIDIsAULIDThatSortsByTime.
//
// @decision(DL-003)
func TestNewIDIsAULIDThatSortsByTime(t *testing.T) {
	t0 := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	a, b := store.NewID(t0), store.NewID(t0.Add(time.Second))
	if !store.ValidID(a) || !store.ValidID(b) {
		t.Fatalf("not ULIDs: %s %s", a, b)
	}
	if a >= b {
		t.Errorf("%s does not sort before %s", a, b)
	}
	if store.ValidID("not-an-id") || store.ValidID("") {
		t.Error("garbage validated")
	}
}

func TestMemoryImages(t *testing.T) {
	storetest.RunImages(t, func(*testing.T) store.ImageStore { return store.NewMemoryImages() })
}

// TestMemoryIndexesFollowARolledBackTransaction: a failed transaction's
// writes are undone, indexes included, so an event or message written in
// it can be written again and is listed once, an updated message reads
// as it was, and a conversation it created is gone.
//
// @decision(DL-071)
func TestMemoryIndexesFollowARolledBackTransaction(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	now := time.Now()
	sess := store.Session{ID: store.NewID(now), OrgID: "org", OwnerID: "u", Name: "s", State: store.StateRunning}
	if err := m.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	conv, err := m.Conversations().UpsertConversation(ctx, store.Conversation{ID: store.NewID(now), OrgID: "org", SessionID: sess.ID, OpenCodeID: "ses_1"})
	if err != nil {
		t.Fatal(err)
	}
	event := func(seq int64) store.Event {
		return store.Event{ID: store.NewID(time.Now()), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
			Source: store.SourceOpenCode, Seq: seq, Name: "session.step.started", At: now}
	}
	message := store.Message{ID: store.NewID(now), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID, OpenCodeID: "msg_1", Role: "user", Text: "hi", At: now}
	if _, err := m.Conversations().PutEvents(ctx, []store.Event{event(1)}); err != nil {
		t.Fatal(err)
	}
	kept := store.Message{ID: store.NewID(now), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID, OpenCodeID: "msg_0", Role: "user", Text: "before", At: now}
	if _, err := m.Conversations().PutMessages(ctx, []store.Message{kept}); err != nil {
		t.Fatal(err)
	}
	failed := m.Tx(ctx, func(tx store.Store) error {
		changed := kept
		changed.ID, changed.Text = store.NewID(time.Now()), "during"
		if _, err := tx.Conversations().PutMessages(ctx, []store.Message{changed}); err != nil {
			return err
		}
		if _, err := tx.Conversations().UpsertConversation(ctx, store.Conversation{ID: store.NewID(time.Now()), OrgID: "org", SessionID: sess.ID, OpenCodeID: "ses_2"}); err != nil {
			return err
		}
		if _, err := tx.Conversations().PutEvents(ctx, []store.Event{event(2)}); err != nil {
			return err
		}
		if _, err := tx.Conversations().PutMessages(ctx, []store.Message{message}); err != nil {
			return err
		}
		return context.Canceled
	})
	if !errors.Is(failed, context.Canceled) {
		t.Fatalf("the transaction returned %v", failed)
	}
	if stored, err := m.Conversations().PutEvents(ctx, []store.Event{event(2)}); err != nil || len(stored) != 1 {
		t.Fatalf("the rolled-back sequence was not written again: %v, %d stored", err, len(stored))
	}
	if n, err := m.Conversations().PutMessages(ctx, []store.Message{{ID: store.NewID(time.Now()), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID, OpenCodeID: "msg_1", Role: "user", Text: "hi", At: now}}); err != nil || n.Changed != 1 {
		t.Fatalf("the rolled-back message was not written again: %v, %+v", err, n)
	}
	evs, err := m.Conversations().ListEvents(ctx, "org", sess.ID, 0, 0)
	if err != nil || len(evs) != 2 || evs[0].Seq != 1 || evs[1].Seq != 2 {
		t.Fatalf("listed %+v, %v", evs, err)
	}
	if after, _ := m.Conversations().ListEvents(ctx, "org", sess.ID, evs[0].Pos, 1); len(after) != 1 || after[0].Seq != 2 {
		t.Errorf("after the first position: %+v", after)
	}
	if last, err := m.Conversations().LastEvent(ctx, "org", sess.ID); err != nil || last.Seq != 2 {
		t.Errorf("last event %+v, %v", last, err)
	}
	if past, _ := m.Conversations().ListEvents(ctx, "org", sess.ID, math.MaxInt64, 0); len(past) != 0 {
		t.Errorf("past the largest cursor: %d events", len(past))
	}
	msgs, _ := m.Conversations().ListMessages(ctx, "org", sess.ID)
	texts := map[string]string{}
	for _, msg := range msgs {
		texts[msg.OpenCodeID] = msg.Text
	}
	if len(msgs) != 2 || texts["msg_0"] != "before" {
		t.Errorf("messages after the rollback: %v", texts)
	}
	if convs, _ := m.Conversations().ListConversations(ctx, "org", sess.ID); len(convs) != 1 {
		t.Errorf("%d conversations after the rollback", len(convs))
	}
	// The session's message index follows the rollback too (DL-131).
	if size, _ := m.Conversations().StoredSize(ctx, "org", sess.ID); size.Rows != 4 || size.Bytes != int64(len("before")+len("hi")) {
		t.Errorf("stored size after the rollback: %+v", size)
	}
	if recent, more, _ := m.Conversations().RecentMessages(ctx, "org", sess.ID, "", 10, 0); len(recent) != 2 || more {
		t.Errorf("recent messages after the rollback: %+v, more %v", recent, more)
	}
}

// The contract holds for a memory store that journals every write, and
// each case's store reopens from its journal without error.
//
// @decision(DL-091)
func TestJournaledMemoryMeetsTheContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		dir := t.TempDir()
		m := store.NewMemory()
		j, err := store.OpenJournal(dir, m, store.JournalOptions{Warn: func(s string) { t.Error(s) }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = j.Close()
			again, err := store.OpenJournal(dir, store.NewMemory(), store.JournalOptions{Warn: func(s string) { t.Error(s) }})
			if err != nil {
				t.Errorf("reopen: %v", err)
				return
			}
			_ = again.Close()
		})
		return m
	})
}
