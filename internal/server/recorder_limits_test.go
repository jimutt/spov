package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// freshFrame is a small durable frame naming a conversation of its own.
func freshFrame(n int) string {
	id := fmt.Sprintf("ses_flood%018d", n)
	return fmt.Sprintf(`{"id":"evt_%d","created":1794165467570,"type":"session.execution.started",`+
		`"data":{"sessionID":%q},"durable":{"aggregateID":%q,"seq":1,"version":1}}`, n, id, id)
}

func (f *recorderFixture) markers(name string) []store.Event {
	var out []store.Event
	for _, e := range f.events() {
		if e.Source == store.SourceSpov && e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

func (f *recorderFixture) attached(ctx context.Context) {
	f.t.Helper()
	if err := f.rec.Sync(ctx); err != nil {
		f.t.Fatal(err)
	}
	waitFor(f.t, "attach", func() bool { _, _, s, _ := f.sb.counts(); return s >= 1 })
}

// TestASessionStoresOnlySoManyConversations is DL-131: a sandbox that
// names a new conversation in every frame fills the session's limit and
// no more, each further one costing no read, and the log says so once.
func TestASessionStoresOnlySoManyConversations(t *testing.T) {
	f := newRecorderFixture(t)
	f.rec.maxConversations = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.attached(ctx)
	for i := 1; i <= 8; i++ {
		f.sb.stream <- freshFrame(i)
	}
	// A frame of a conversation the session holds is still recorded.
	f.sb.stream <- strings.Replace(freshFrame(1), `"seq":1`, `"seq":2`, 1)
	waitFor(t, "the frames", func() bool { return len(f.openCodeEvents()) == 4 })

	convs, err := f.st.Conversations().ListConversations(ctx, "org", f.sess.ID)
	if err != nil || len(convs) != 3 {
		t.Fatalf("stored %d conversations (%v), want the limit of 3", len(convs), err)
	}
	for i := 4; i <= 8; i++ {
		if n := f.sb.infoReads(fmt.Sprintf("ses_flood%018d", i)); n != 0 {
			t.Errorf("conversation %d past the limit was read %d times", i, n)
		}
	}
	if n := len(f.markers(MarkerConversationLimit)); n != 1 {
		t.Errorf("the log holds %d limit notes, want one", n)
	}

	// A later span of the same session is refused quietly: the note is
	// in the log already.
	close(f.sb.stream)
	f.sb.mu.Lock()
	f.sb.stream = make(chan string, 64)
	f.sb.mu.Unlock()
	waitFor(t, "the redial", func() bool { _, d, _, _ := f.sb.counts(); return d >= 2 })
	f.sb.stream <- freshFrame(9)
	f.sb.stream <- strings.Replace(freshFrame(2), `"seq":1`, `"seq":2`, 1)
	waitFor(t, "the second span", func() bool { return len(f.openCodeEvents()) == 5 })
	if n := len(f.markers(MarkerConversationLimit)); n != 1 {
		t.Errorf("after a redial the log holds %d limit notes, want one", n)
	}
	if convs, _ := f.st.Conversations().ListConversations(ctx, "org", f.sess.ID); len(convs) != 3 {
		t.Errorf("after a redial the session holds %d conversations", len(convs))
	}
}

// TestAnOversizedConversationInfoIsNotStored is DL-131: the conversation
// is recorded, its info is not, and no listing carries any info.
func TestAnOversizedConversationInfoIsNotStored(t *testing.T) {
	f := newRecorderFixture(t)
	big := fmt.Sprintf(`{"id":"ses_flood%018d","title":"big","pad":%q}`, 1, strings.Repeat("x", MaxConversationInfoBytes))
	f.sb.infos[fmt.Sprintf("ses_flood%018d", 1)] = big
	small := fmt.Sprintf(`{"id":"ses_flood%018d","title":"small"}`, 2)
	f.sb.infos[fmt.Sprintf("ses_flood%018d", 2)] = small
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.attached(ctx)
	f.sb.stream <- freshFrame(1)
	f.sb.stream <- freshFrame(2)
	waitFor(t, "the frames", func() bool { return len(f.openCodeEvents()) == 2 })

	convs, _ := f.st.Conversations().ListConversations(ctx, "org", f.sess.ID)
	if len(convs) != 2 {
		t.Fatalf("stored %d conversations, want both", len(convs))
	}
	for _, c := range convs {
		if len(c.Info) != 0 {
			t.Errorf("the listing carries %d bytes of info for %s", len(c.Info), c.OpenCodeID)
		}
	}
	infos, err := f.st.Conversations().ConversationInfos(ctx, "org", f.sess.ID)
	if err != nil || len(infos) != 1 {
		t.Fatalf("stored %d infos (%v), want only the small one", len(infos), err)
	}
	for _, info := range infos {
		if string(info) != small {
			t.Errorf("stored info = %.80q", info)
		}
	}
}

// TestRecordingStopsAtTheSessionsStorageLimit is DL-131: a sandbox that
// never stops writing fills the session's limit and no more, the log
// says why once, and the session is not dialled again.
func TestRecordingStopsAtTheSessionsStorageLimit(t *testing.T) {
	f := newRecorderFixture(t)
	f.rec.maxBytes = 4 << 10
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.attached(ctx)
	id := fmt.Sprintf("ses_flood%018d", 1)
	pad := strings.Repeat("x", 1<<10)
	for seq := 1; seq <= 40; seq++ {
		select {
		case f.sb.stream <- fmt.Sprintf(`{"id":"evt_%d","created":1794165467570,"type":"session.execution.started","pad":%q,`+
			`"data":{"sessionID":%q},"durable":{"aggregateID":%q,"seq":%d,"version":1}}`, seq, pad, id, id, seq):
		case <-ctx.Done():
		}
	}
	waitFor(t, "the stop", func() bool { return len(f.markers(MarkerStopped)) == 1 })
	size, err := f.st.Conversations().StoredSize(ctx, "org", f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// One frame may land past the limit: the check is before the write.
	if size.Bytes > 4<<10+2<<10 {
		t.Errorf("stored %d bytes against a limit of %d", size.Bytes, 4<<10)
	}
	if text := f.markers(MarkerStopped)[0].Text; !strings.Contains(text, "limit") {
		t.Errorf("the note = %q", text)
	}
	if got := len(f.slept); got != 0 {
		t.Errorf("the recorder redialled %d times after the limit", got)
	}
	_, dials, _, _ := f.sb.counts()

	// The next sync leaves it alone, and so does a new process, which
	// finds the note and adds none.
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, d, _, _ := f.sb.counts(); d != dials {
		t.Errorf("a session at its limit was dialled again (%d, then %d)", dials, d)
	}
	f.rec.stopAtStorageLimit(ctx, f.sess)
	if n := len(f.markers(MarkerStopped)); n != 1 {
		t.Errorf("the log holds %d stop notes, want one", n)
	}
}
