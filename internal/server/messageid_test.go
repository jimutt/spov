package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
)

// realInboxFrame is the captured session.inbox.enqueued frame that
// recorder_test.go and frames_test.go both hold. It is the one shape
// that carries BOTH ids, which is what makes it the right fixture
// here: the frame's own `id` is the event's (evt_…) and the message's
// is `data.inboxID` (msg_…).
const realInboxFrame = `{"id":"evt_000000000006SyntheticId006","created":1788267414303,` +
	`"type":"session.inbox.enqueued","location":{"directory":"/workspace/spov"},` +
	`"data":{"inboxID":"msg_000000000005SyntheticId005","sessionID":"ses_000000000019SyntheticId019",` +
	`"item":{"type":"user","payload":{"text":"reply with ok"},"delivery":"steer"}},` +
	`"durable":{"aggregateID":"ses_000000000019SyntheticId019","seq":30,"version":1}}`

const (
	realInboxMessageID = "msg_000000000005SyntheticId005"
	realInboxEventID   = "evt_000000000006SyntheticId006"
)

// The log carries the id of the message an inbox event enqueued, all
// the way to a client.
//
// @decision(DL-019)
func TestTheLogCarriesTheEnqueuedMessageId(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.streamLifetime = time.Hour
	f.srv.heartbeat = time.Hour
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
	conv := seedConversation(t, f, sess, "ses_000000000019SyntheticId019", "one")
	seedEvent(t, f, store.Event{
		OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID,
		Source: store.SourceOpenCode, Seq: 30, Name: jobs.EventInboxEnqueued,
		Text: "reply with ok", At: recordedAt, Raw: []byte(realInboxFrame),
	})

	var seen int
	for _, ev := range runLog(t, f, &logInput{Ref: "mine"}) {
		l, ok := ev.(LogEvent)
		if !ok || l.Name != jobs.EventInboxEnqueued {
			continue
		}
		seen++
		if l.Message == realInboxEventID {
			t.Errorf("the log carries the EVENT's id (%s) as the message id: a"+
				" breadth-first search for `id` finds the frame's own before the"+
				" item's, so every event would reconcile against a key no message"+
				" will ever have", l.Message)
		}
		if l.Message != realInboxMessageID {
			t.Errorf("message = %q, want %q from data.inboxID", l.Message, realInboxMessageID)
		}
	}
	if seen == 0 {
		t.Fatal("the inbox event never reached the log")
	}
}

// Only an inbox event names a message; the other spellings a frame can
// carry name a different message from the one their event is about.
//
// @decision(DL-019)
func TestOnlyAnInboxEventNamesAMessage(t *testing.T) {
	for _, raw := range []string{"", "not json at all", `{"data":{}}`, `{"id":"evt_1","data":{}}`} {
		if got := storedMessage(store.Event{Name: jobs.EventInboxEnqueued, Raw: []byte(raw)}); got != "" {
			t.Errorf("storedMessage(%q) = %q, want empty", raw, got)
		}
	}

	const stepStarted = `{"id":"evt_000000000003SyntheticId003","created":1788265407953,` +
		`"type":"session.step.started","location":{"directory":"/workspace/spov"},` +
		`"data":{"sessionID":"ses_000000000020SyntheticId020","agent":"build",` +
		`"assistantMessageID":"msg_000000000001SyntheticId001","snapshot":"0badc0de"},` +
		`"durable":{"aggregateID":"ses_000000000020SyntheticId020","seq":46,"version":1}}`
	if got := storedMessage(store.Event{Name: "session.step.started", Raw: []byte(stepStarted)}); got != "" {
		t.Errorf("storedMessage named a message on a step event: %q", got)
	}
	// And the guard is the NAME, not the absence of the field: a frame
	// carrying an inboxID under another name is still not read.
	if got := storedMessage(store.Event{Name: "session.text.ended", Raw: []byte(realInboxFrame)}); got != "" {
		t.Errorf("storedMessage parsed a non-inbox event: %q", got)
	}
}

// The key list holds inboxID alone. Widening it is a change nothing else
// would catch: an inbox frame carrying another spelling and no inboxID
// would start reporting the id of a different message. An id spov
// cannot find is a message the client draws afresh; a wrong id is one
// it silently merges into an unrelated message.
//
// @decision(DL-019)
func TestTheMessageIdIsReadFromInboxIDAndNothingElse(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"the assistant turn a step frame names", `{"id":"evt_1","type":"session.inbox.enqueued",` +
			`"data":{"sessionID":"ses_1","assistantMessageID":"msg_assistant",` +
			`"item":{"type":"user","payload":{"text":"hi"}}}}`},
		{"the tool call a permission frame names", `{"id":"evt_2","type":"session.inbox.enqueued",` +
			`"data":{"sessionID":"ses_1","item":{"type":"user","payload":{"text":"hi"}},` +
			`"source":{"type":"tool","messageID":"msg_toolcall","id":"call_1"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := storedMessage(store.Event{Name: jobs.EventInboxEnqueued, Raw: []byte(tc.raw)}); got != "" {
				t.Errorf("message = %q, want empty: that id names another message entirely,"+
					" so a client reconciling on it merges the reader's own message into"+
					" one they never sent", got)
			}
		})
	}
}

// The stored transcript carries the same id, so a client that reloads
// the page recognises the messages it had already drawn from the
// stream.
//
// @decision(DL-019)
func TestTheTranscriptCarriesTheMessageId(t *testing.T) {
	f := newAPIFixture(t)
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alpha", store.StateRunning)
	conv := seedConversation(t, f, sess, "ses_1", "one")
	seedMessage(t, f, conv, realInboxMessageID, "user", "reply with ok", recordedAt)

	convs, _, err := f.srv.transcript(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || len(convs[0].Messages) != 1 {
		t.Fatalf("got %+v, want one conversation holding one message", convs)
	}
	if got := convs[0].Messages[0].ID; got != realInboxMessageID {
		t.Errorf("message id = %q, want OpenCode's own %q; the row has carried it"+
			" all along and only the view did not", got, realInboxMessageID)
	}
}

// parseMessages puts the id on the view rather than beside it, which
// is what makes the transcript's id and the recorder's key the same
// field rather than two that agree by convention.
//
// @decision(DL-019) @decision(DL-017)
func TestTheParsedMessageCarriesItsIdOnTheView(t *testing.T) {
	const captured = `{"data":[{"id":"msg_synthetic_capture",` +
		`"time":{"created":1788534620484},"text":"hello","type":"assistant"}]}`
	msgs := parseMessages([]byte(captured))
	if len(msgs) != 1 {
		t.Fatalf("parsed %d messages, want 1", len(msgs))
	}
	// Taken as a view on its own, which is how the transcript gets it:
	// the id has to survive being detached from the parsed wrapper.
	view := msgs[0].MessageView
	if view.ID != "msg_synthetic_capture" {
		t.Errorf("the view's id = %q, want the message's own", view.ID)
	}
	// The promoted spelling, which is the one recorder.go reads.
	if !strings.HasPrefix(msgs[0].ID, "msg_") {
		t.Errorf("id = %q, want OpenCode's msg_ space", msgs[0].ID)
	}
}
