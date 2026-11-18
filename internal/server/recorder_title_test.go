package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/opencode/contract"
)

// A rename during a live capture reaches the conversation's title when
// its frame arrives, cleaned to one bounded line, and the frame is
// stored like any other.
//
// @decision(DL-149)
func TestARenameIsRecordedWhenItsFrameArrives(t *testing.T) {
	f := newRecorderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attach", func() bool { _, _, s, _ := f.sb.counts(); return s == 1 })
	f.sb.send(rootCreated)
	f.sb.send(string(contract.Event(t, "session.renamed", 1,
		`{"sessionID":"`+capturedRoot+`","title":"Fix the flaky login test"}`)))
	waitFor(t, "the rename", func() bool {
		c, ok := f.conversationOf(t, capturedRoot)
		return ok && c.Title == "Fix the flaky login test"
	})
	if evs := f.openCodeEvents(); len(evs) != 1 || evs[0].Name != "session.renamed" {
		t.Errorf("stored %+v, want the renamed frame", evs)
	}

	hostile := "Line one\nline\u202e two\x1b[2J " + strings.Repeat("long ", 60)
	f.sb.send(string(contract.Event(t, "session.renamed", 2,
		`{"sessionID":"`+capturedRoot+`","title":`+quoteJSON(t, hostile)+`}`)))
	waitFor(t, "the second rename", func() bool {
		c, _ := f.conversationOf(t, capturedRoot)
		return strings.HasPrefix(c.Title, "Line one")
	})
	c, _ := f.conversationOf(t, capturedRoot)
	if strings.ContainsAny(c.Title, "\n\x1b\u202e") || len([]rune(c.Title)) > 120 || !strings.HasPrefix(c.Title, "Line one line two [2J long") {
		t.Errorf("the stored title is %q, want one clean line of at most 120 characters", c.Title)
	}
	if n := f.sb.infoReads(capturedRoot); n != 0 {
		t.Errorf("the conversation was read %d times; the frame said enough", n)
	}
}

// quoteJSON is s as a JSON string.
func quoteJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
