package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// TestATranscriptHoldsOnlyItsNewestMessages is DL-131: the transcript
// endpoint assembles at most MaxTranscriptMessages of the session's
// newest messages, and says when it left older ones out.
func TestATranscriptHoldsOnlyItsNewestMessages(t *testing.T) {
	f := newAPIFixture(t)
	sess := seedSession(t, f, store.NewID(time.Now()), "alpha", store.StateRunning)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	msgs := make([]store.Message, 0, MaxTranscriptMessages+1)
	for i := range MaxTranscriptMessages + 1 {
		msgs = append(msgs, store.Message{
			ID: store.NewID(recordedAt), OrgID: conv.OrgID, SessionID: conv.SessionID, ConversationID: conv.ID,
			OpenCodeID: fmt.Sprintf("msg_%06d", i), Role: "user", Text: "x", At: recordedAt.Add(time.Duration(i) * time.Second),
		})
	}
	if _, err := f.store.Conversations().PutMessages(context.Background(), msgs); err != nil {
		t.Fatal(err)
	}
	convs, warnings, truncated, err := f.srv.transcript(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(warnings) != 1 || warnings[0] != transcriptTruncated {
		t.Errorf("truncated %v, warnings %v", truncated, warnings)
	}
	got := convs[0].Messages
	if len(got) != MaxTranscriptMessages || got[0].ID != "msg_000001" || got[len(got)-1].ID != fmt.Sprintf("msg_%06d", MaxTranscriptMessages) {
		t.Errorf("got %d messages, from %s", len(got), got[0].ID)
	}

	// One fewer is the whole transcript, and says nothing.
	seedMessage(t, f, seedConversation(t, f, seedSession(t, f, store.NewID(time.Now()), "beta", store.StateRunning), "ses_2", ""), "m", "user", "hi", recordedAt)
	small, _ := f.store.Sessions().List(context.Background(), sess.OrgID, store.SessionFilter{})
	for _, s := range small {
		if s.Name != "beta" {
			continue
		}
		if _, warnings, truncated, _ := f.srv.transcript(context.Background(), s); truncated || len(warnings) != 0 {
			t.Errorf("a short transcript: truncated %v, warnings %v", truncated, warnings)
		}
	}
}

// TestOnlyTheTranscriptImportReadsEveryMessage is DL-131: a reader that
// a request or a pass reaches reads a bounded RecentMessages, never a
// session's whole history. The transcript import, once per resume, is
// the one reader of every message.
func TestOnlyTheTranscriptImportReadsEveryMessage(t *testing.T) {
	allowed := map[string]bool{filepath.Join("..", "server", "transcriptimport.go"): true}
	for _, dir := range []string{filepath.Join("..", "server"), filepath.Join("..", "jobs")} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") || allowed[path] {
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(src), ".ListMessages(") {
				t.Errorf("%s reads every message of a session", path)
			}
		}
	}
}
