package server

import (
	"context"
	"errors"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// Frames in the shape spike S-SA1 measured at the pin: a subagent's
// session.created names its parent, and a root's names none.
const (
	rootCreated = `{"id":"evt_1bc9866100013xf4x4q6LthqVA","created":1794165466640,"type":"session.created",` +
		`"location":{"directory":"/workspace"},"data":{"sessionID":"ses_root0000000000000000000","slug":"crisp-forest",` +
		`"version":"2.2.5","projectID":"7eb7","subpath":""},` +
		`"durable":{"aggregateID":"ses_root0000000000000000000","seq":0,"version":1}}`
	childCreated = `{"id":"evt_1bc9869af001rQ6eLthN0avb1q","created":1794165467567,"type":"session.created",` +
		`"location":{"directory":"/workspace"},"data":{"sessionID":"ses_child000000000000000000","slug":"lucky-orchid",` +
		`"version":"2.2.5","projectID":"7eb7","parentID":"ses_root0000000000000000000","subpath":"",` +
		`"title":"probe child fg","agent":"general","model":{"id":"m","providerID":"probe","variant":"default"}},` +
		`"durable":{"aggregateID":"ses_child000000000000000000","seq":0,"version":1}}`
	childEnqueued = `{"id":"evt_1bc9869b2002A4fvDfIO05z6BN","created":1794165467570,"type":"session.inbox.enqueued",` +
		`"location":{"directory":"/workspace"},"data":{"inboxID":"msg_1bc9869b20018Wf1Z1H8kMNdk7",` +
		`"sessionID":"ses_child000000000000000000","item":{"type":"user","payload":{"text":"You are a subagent spawned by another session.\nrun one glob"},"delivery":"steer"}},` +
		`"durable":{"aggregateID":"ses_child000000000000000000","seq":1,"version":1}}`
	childStarted = `{"id":"evt_1bc9869b5001qQlF3MX3LnC8lK","created":1794165467573,"type":"session.execution.started",` +
		`"data":{"sessionID":"ses_child000000000000000000"},"durable":{"aggregateID":"ses_child000000000000000000","seq":2,"version":1}}`
)

func (f *recorderFixture) conversationOf(t *testing.T, opencodeID string) (store.Conversation, bool) {
	t.Helper()
	convs, err := f.st.Conversations().ListConversations(context.Background(), "org", f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range convs {
		if c.OpenCodeID == opencodeID {
			return c, true
		}
	}
	return store.Conversation{}, false
}

// A subagent's row carries its parent from its created frame, before any
// of its frames is stored, and neither conversation is read for it.
//
// @decision(DL-110)
func TestASubagentsCreatedFrameRecordsItsParent(t *testing.T) {
	f := newRecorderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attach", func() bool { _, _, s, _ := f.sb.counts(); return s == 1 })
	f.sb.stream <- rootCreated
	f.sb.stream <- childCreated
	f.sb.stream <- childEnqueued
	f.sb.stream <- childStarted
	waitFor(t, "the child's frames", func() bool { return len(f.openCodeEvents()) == 2 })
	child, ok := f.conversationOf(t, "ses_child000000000000000000")
	if !ok || child.ParentOpenCodeID != "ses_root0000000000000000000" {
		t.Errorf("the child's row is %+v, want its parent", child)
	}
	if root, ok := f.conversationOf(t, "ses_root0000000000000000000"); !ok || !root.IsRoot() {
		t.Errorf("the root's row is %+v, %v", root, ok)
	}
	for _, id := range []string{"ses_root0000000000000000000", "ses_child000000000000000000"} {
		if n := f.sb.infoReads(id); n != 0 {
			t.Errorf("%s was read %d times; its created frame said enough", id, n)
		}
	}
}

// A conversation first seen mid-log is read once, before its first frame
// is stored, so a subagent never has a frame stored as a root's; later
// frames read nothing more.
//
// @decision(DL-110)
func TestAConversationFirstSeenMidLogIsReadOnceForItsParent(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.infos["ses_child000000000000000000"] = `{"id":"ses_child000000000000000000","title":"probe child fg","parentID":"ses_root0000000000000000000"}`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attach", func() bool { _, _, s, _ := f.sb.counts(); return s == 1 })
	f.sb.stream <- childEnqueued
	f.sb.stream <- childStarted
	waitFor(t, "the child's frames", func() bool { return len(f.openCodeEvents()) == 2 })
	child, _ := f.conversationOf(t, "ses_child000000000000000000")
	if child.ParentOpenCodeID != "ses_root0000000000000000000" || child.Title != "probe child fg" || len(child.Info) == 0 {
		t.Errorf("the child's row is %+v, want the info its read gave", child)
	}
	if n := f.sb.infoReads("ses_child000000000000000000"); n != 1 {
		t.Errorf("the child was read %d times, want once", n)
	}
}

// A read that fails leaves the row a root, and the next catch-up's
// listing gives it its parent.
//
// @decision(DL-110)
func TestAFailedReadLeavesARootThatTheNextCatchUpCorrects(t *testing.T) {
	f := newRecorderFixture(t)
	f.sb.infoErr = errors.New("sandbox busy")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attach", func() bool { _, _, s, _ := f.sb.counts(); return s == 1 })
	f.sb.stream <- childEnqueued
	waitFor(t, "the child's frame", func() bool { return len(f.openCodeEvents()) == 1 })
	if child, _ := f.conversationOf(t, "ses_child000000000000000000"); !child.IsRoot() {
		t.Fatalf("with no read the child is %+v, want a root", child)
	}

	f.sb.mu.Lock()
	f.sb.listing = `{"data":[{"id":"ses_root0000000000000000000"},` +
		`{"id":"ses_child000000000000000000","parentID":"ses_root0000000000000000000"}]}`
	f.sb.mu.Unlock()
	f.sb.breakStream()
	waitFor(t, "a catch-up", func() bool { _, _, s, _ := f.sb.counts(); return s >= 2 })
	waitFor(t, "the parent", func() bool {
		child, _ := f.conversationOf(t, "ses_child000000000000000000")
		return child.ParentOpenCodeID == "ses_root0000000000000000000"
	})
}
