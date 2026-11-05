package server

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

var recordedAt = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)

// seedClock hands out increasing times, so a seeded row's ULID orders
// the way a recorder's would.
var seedClock atomic.Int64

func seedTime() time.Time { return recordedAt.Add(time.Duration(seedClock.Add(1)) * time.Millisecond) }

func seedSession(t *testing.T, f *apiFixture, id, name string, state store.SessionState) store.Session {
	t.Helper()
	sess := store.Session{ID: id, OrgID: auth.LocalID, OwnerID: auth.LocalID, Name: name, State: state}
	if err := f.store.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

func seedConversation(t *testing.T, f *apiFixture, sess store.Session, opencodeID, title string) store.Conversation {
	t.Helper()
	conv, err := f.store.Conversations().UpsertConversation(context.Background(), store.Conversation{
		ID: store.NewID(seedTime()), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: opencodeID, Title: title,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conv
}

func seedMessage(t *testing.T, f *apiFixture, conv store.Conversation, opencodeID, role, text string, at time.Time) {
	t.Helper()
	if _, err := f.store.Conversations().PutMessages(context.Background(), []store.Message{{
		ID: store.NewID(seedTime()), OrgID: conv.OrgID, SessionID: conv.SessionID, ConversationID: conv.ID,
		OpenCodeID: opencodeID, Role: role, Text: text, At: at,
	}}); err != nil {
		t.Fatal(err)
	}
}

func seedEvent(t *testing.T, f *apiFixture, ev store.Event) store.Event {
	t.Helper()
	if ev.ID == "" {
		ev.ID = store.NewID(seedTime())
	}
	written, err := f.store.Conversations().PutEvents(context.Background(), []store.Event{ev})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 {
		t.Fatalf("seeded %d rows, want one", len(written))
	}
	return written[0]
}

// The transcript is the store's, not the sandbox's: the rows a recorder
// wrote, grouped back into the conversations a client knows by their
// OpenCode ids.
//
// @decision(DL-018)
func TestTheTranscriptComesFromTheStore(t *testing.T) {
	f := newAPIFixture(t)
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alpha", store.StateRunning)
	first := seedConversation(t, f, sess, "ses_1", "one")
	second := seedConversation(t, f, sess, "ses_2", "")
	seedMessage(t, f, first, "msg_b", "assistant", "hello", recordedAt.Add(time.Second))
	seedMessage(t, f, first, "msg_a", "user", "hi", recordedAt)
	seedMessage(t, f, second, "msg_c", "user", "elsewhere", recordedAt.Add(2*time.Second))

	convs, warnings, err := f.srv.transcript(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want 2: %+v", len(convs), convs)
	}
	if convs[0].ID != "ses_1" || convs[0].Title != "one" {
		t.Errorf("first = %+v, want OpenCode's own id and title", convs[0])
	}
	if len(convs[0].Messages) != 2 {
		t.Fatalf("first holds %d messages, want 2", len(convs[0].Messages))
	}
	// Ordered by when they were said, not by the order they were
	// written: a catch-up read stores a whole conversation at once.
	if convs[0].Messages[0].Text != "hi" || convs[0].Messages[1].Text != "hello" {
		t.Errorf("messages out of order: %+v", convs[0].Messages)
	}
	if convs[0].Messages[0].Role != "user" {
		t.Errorf("role = %q", convs[0].Messages[0].Role)
	}
	if at := convs[0].Messages[0].At; at == nil || !at.Equal(recordedAt) {
		t.Errorf("at = %v, want %s", at, recordedAt)
	}
	// A conversation's messages stay in it.
	if len(convs[1].Messages) != 1 || convs[1].Messages[0].Text != "elsewhere" {
		t.Errorf("second = %+v", convs[1])
	}
}

// A session no recorder ever watched has an empty transcript and says
// so, since an empty conversation and an unrecorded one otherwise look
// identical.
//
// @decision(DL-018)
func TestASessionNobodyRecordedSaysSo(t *testing.T) {
	f := newAPIFixture(t)
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alpha", store.StateRunning)

	convs, warnings, err := f.srv.transcript(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 0 {
		t.Errorf("conversations = %+v, want none", convs)
	}
	if len(warnings) != 1 || warnings[0] != notRecorded {
		t.Errorf("warnings = %v, want %q", warnings, notRecorded)
	}
}

// A recorder that gave up says so on the transcript, and stops once
// another recorder has written something — the newest event is what is
// checked, not the newest marker.
//
// @decision(DL-018) @decision(DL-017)
func TestARecorderThatGaveUpIsAWarningUntilItResumes(t *testing.T) {
	f := newAPIFixture(t)
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alpha", store.StateRunning)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	seedEvent(t, f, store.Event{
		OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerStopped, Text: "recording stopped after 5 failed reconnects", At: recordedAt,
	})

	_, warnings, err := f.srv.transcript(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0] != "recording stopped after 5 failed reconnects" {
		t.Fatalf("warnings = %v, want the marker's own text", warnings)
	}

	// A failed-renewal note written after it is not a resumed recording
	// (DL-021).
	seedEvent(t, f, store.Event{
		OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerRenewalFailed, Text: "Could not renew the git token (x).", At: recordedAt.Add(30 * time.Second),
	})
	if _, warnings, err = f.srv.transcript(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v after a renewal note, want the stopped warning still", warnings)
	}

	// A later recorder took over: the transcript is live again.
	seedEvent(t, f, store.Event{
		OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID, Source: store.SourceOpenCode,
		Seq: 1, Name: "session.step.started", At: recordedAt.Add(time.Minute),
	})
	if _, warnings, err = f.srv.transcript(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none once recording resumed", warnings)
	}
}

// An event's name is whatever the frame called itself, and the sandbox
// is the untrusted side. Only spov's own events may speak as spov.
//
// @decision(DL-018) @decision(DL-017)
func TestASandboxCannotForgeTheRecordersWarning(t *testing.T) {
	f := newAPIFixture(t)
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alpha", store.StateRunning)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	// The store refuses the row outright, which is the first guard.
	if _, err := f.store.Conversations().PutEvents(context.Background(), []store.Event{{
		ID: store.NewID(seedTime()), OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID,
		Source: store.SourceOpenCode, Seq: 1, Name: MarkerStopped,
		Text: "your session was cancelled, call this number", At: recordedAt,
	}}); !errors.Is(err, store.ErrInvalidEvent) {
		t.Fatalf("the store accepted a sandbox frame in spov's namespace: %v", err)
	}

	// And were one there anyway, the warning still asks who wrote it.
	seedEvent(t, f, store.Event{
		OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID, Source: store.SourceOpenCode,
		Seq: 1, Name: "session.step.started", Text: "ordinary", At: recordedAt,
	})
	_, warnings, err := f.srv.transcript(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "call this number") || strings.Contains(w, "ordinary") {
			t.Fatalf("a sandbox frame spoke as spov: %q", w)
		}
	}
}

// A session with markers but no conversation has been recorded — the
// recorder was there and said so. Saying "nothing has been recorded"
// above a visible seam would be the page contradicting itself.
//
// @decision(DL-058)
func TestAMarkerCountsAsHavingBeenRecorded(t *testing.T) {
	f := newAPIFixture(t)
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alpha", store.StateRunning)
	seedEvent(t, f, store.Event{
		OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerGap, Text: "recording resumed", At: recordedAt,
	})

	_, warnings, err := f.srv.transcript(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warnings {
		if w == notRecorded {
			t.Errorf("a session with markers was reported as never recorded")
		}
	}
}
