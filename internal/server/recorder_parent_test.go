package server

import (
	"context"
	"errors"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// A subagent's session.created names its parent, and a root's names
// none (capture_test.go holds the frames).
const (
	rootCreated   = capturedRootCreated
	childCreated  = capturedChildCreated
	childEnqueued = capturedChildEnqueued
	childStarted  = capturedChildStarted
)

func (f *recorderFixture) conversationOf(t *testing.T, opencodeID string) (store.Conversation, bool) {
	t.Helper()
	convs, err := f.st.Conversations().ListConversations(context.Background(), "org", f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The listing leaves Info out (DL-131); a test reads the row whole.
	infos, err := f.st.Conversations().ConversationInfos(context.Background(), "org", f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range convs {
		if c.OpenCodeID == opencodeID {
			c.Info = infos[c.ID]
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
	f.sb.send(rootCreated)
	f.sb.send(childCreated)
	f.sb.send(childEnqueued)
	f.sb.send(childStarted)
	waitFor(t, "the child's frames", func() bool { return len(f.openCodeEvents()) == 2 })
	child, ok := f.conversationOf(t, capturedChild)
	if !ok || child.ParentOpenCodeID != capturedRoot {
		t.Errorf("the child's row is %+v, want its parent", child)
	}
	if root, ok := f.conversationOf(t, capturedRoot); !ok || !root.IsRoot() {
		t.Errorf("the root's row is %+v, %v", root, ok)
	}
	for _, id := range []string{capturedRoot, capturedChild} {
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
	f.sb.infos[capturedChild] = `{"id":"` + capturedChild + `","title":"probe child fg","parentID":"` + capturedRoot + `"}`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attach", func() bool { _, _, s, _ := f.sb.counts(); return s == 1 })
	f.sb.send(childEnqueued)
	f.sb.send(childStarted)
	waitFor(t, "the child's frames", func() bool { return len(f.openCodeEvents()) == 2 })
	child, _ := f.conversationOf(t, capturedChild)
	if child.ParentOpenCodeID != capturedRoot || child.Title != "probe child fg" || len(child.Info) == 0 {
		t.Errorf("the child's row is %+v, want the info its read gave", child)
	}
	if n := f.sb.infoReads(capturedChild); n != 1 {
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
	f.sb.send(childEnqueued)
	waitFor(t, "the child's frame", func() bool { return len(f.openCodeEvents()) == 1 })
	if child, _ := f.conversationOf(t, capturedChild); !child.IsRoot() {
		t.Fatalf("with no read the child is %+v, want a root", child)
	}

	f.sb.mu.Lock()
	f.sb.listing = `{"data":[{"id":"` + capturedRoot + `"},` +
		`{"id":"` + capturedChild + `","parentID":"` + capturedRoot + `"}]}`
	f.sb.mu.Unlock()
	f.sb.breakStream()
	waitFor(t, "a catch-up", func() bool { _, _, s, _ := f.sb.counts(); return s >= 2 })
	waitFor(t, "the parent", func() bool {
		child, _ := f.conversationOf(t, capturedChild)
		return child.ParentOpenCodeID == capturedRoot
	})
}
