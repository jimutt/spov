package server

import (
	"context"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/store"
)

// A subagent's report in the shapes spike S-SA3 captured at 2.3.0: the
// inbox frame the parent's log stores, and the message OpenCode keeps.
const (
	reportFrame = `{"id":"evt_1eb8c5946001DjTG5yazwZqcVG","created":1794953206086,"type":"session.inbox.enqueued",` +
		`"durable":{"aggregateID":"ses_e1473beb4ffedd3YJoFLafA1CP","seq":23,"version":1},` +
		`"data":{"sessionID":"ses_e1473beb4ffedd3YJoFLafA1CP","inboxID":"msg_1eb8c436b001YVKsPvTHC0r6Z5",` +
		`"item":{"type":"synthetic","payload":{"text":"<subagent sessionID=\"ses_e1473bc9affePssI1WmxBG7Ksh\" state=\"completed\" description=\"probe child a\">\nfinal\n</subagent>",` +
		`"description":"probe child a","metadata":{"source":"subagent","childID":"ses_e1473bc9affePssI1WmxBG7Ksh","agent":"General","state":"completed"}},"delivery":"steer"}}}`
	reportMessage = `{"id":"msg_1eb8e2096001rsTFIhAbeAfYj3","metadata":{"source":"subagent","childID":"ses_e1471df72ffecZP6FsFzTsQbBa",` +
		`"agent":"General","state":"completed"},"time":{"created":1794953325005},` +
		`"text":"<subagent sessionID=\"ses_e1471df72ffecZP6FsFzTsQbBa\" state=\"completed\" description=\"probe child fg\">\nfinal\n</subagent>","type":"synthetic"}`
	continuation = `{"sessionID":"ses_p","inboxID":"msg_c",` +
		`"item":{"type":"synthetic","payload":{"text":"Continue working"},"delivery":"queue"}}`
)

// The wire says which synthetic item is a subagent's report, and whose,
// from the stored frame and message; nothing else is one.
//
// @decision(DL-144)
func TestASubagentsReportIsNamedOnTheWire(t *testing.T) {
	ev := store.Event{Source: store.SourceOpenCode, Name: jobs.EventInboxEnqueued, Raw: contract.Frame(t, reportFrame)}
	got := storedReport(ev, storedRole(ev))
	if got == nil || got.Child != "ses_e1473bc9affePssI1WmxBG7Ksh" || got.State != "completed" {
		t.Errorf("the report frame read as %+v", got)
	}
	m := messageViewOf(store.Message{OpenCodeID: "msg_1eb8e2096001rsTFIhAbeAfYj3", Role: "synthetic", Text: "x", Raw: []byte(reportMessage)})
	if m.Report == nil || m.Report.Child != "ses_e1471df72ffecZP6FsFzTsQbBa" || m.Report.State != "completed" {
		t.Errorf("the report message read as %+v", m.Report)
	}

	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"a continuation", contract.Event(t, jobs.EventInboxEnqueued, 24, continuation)},
		{"a person's item claiming to be a report", contract.Frame(t, replaceOnce(replaceOnce(reportFrame,
			`"type":"synthetic"`, `"type":"user"`), `"description":"probe child a",`, ``))},
		{"a report naming no child", contract.Frame(t, replaceOnce(reportFrame, `"childID":"ses_e1473bc9affePssI1WmxBG7Ksh",`, ``))},
		{"another source", contract.Frame(t, replaceOnce(reportFrame, `"source":"subagent"`, `"source":"plugin"`))},
		{"unreadable", contract.Unchecked(`{"data":{"item":`, "cut off")},
	} {
		ev := store.Event{Source: store.SourceOpenCode, Name: jobs.EventInboxEnqueued, Raw: tc.raw}
		if got := storedReport(ev, storedRole(ev)); got != nil {
			t.Errorf("%s read as a report: %+v", tc.name, got)
		}
	}
	if got := storedReport(store.Event{Source: store.SourceSpov, Name: jobs.EventInboxEnqueued, Raw: []byte(reportFrame)}, "synthetic"); got != nil {
		t.Errorf("spov's own row read as a report: %+v", got)
	}
	if m := messageViewOf(store.Message{Role: "synthetic", Raw: []byte(`{"type":"synthetic","text":"continue"}`)}); m.Report != nil {
		t.Errorf("a continuation message read as a report: %+v", m.Report)
	}
	if m := messageViewOf(store.Message{Role: "user", Raw: []byte(replaceOnce(reportMessage, `"type":"synthetic"`, `"type":"user"`))}); m.Report != nil {
		t.Errorf("a person's message read as a report: %+v", m.Report)
	}
}

func replaceOnce(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	panic("replaceOnce: " + old + " is not in the text")
}

// Each event of a subagent's conversation carries that conversation's
// parent, so a client learns a child from the log itself; a root's
// carries none.
//
// @decision(DL-144) @decision(DL-111)
func TestTheLogSaysWhoseChildAnEventsConversationIs(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.streamLifetime = time.Hour
	f.srv.heartbeat = time.Hour
	sess := seedSession(t, f, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "mine", store.StateRunning)
	root := seedConversation(t, f, sess, "ses_root", "root")
	child, err := f.store.Conversations().UpsertConversation(context.Background(), store.Conversation{
		ID: store.NewID(seedTime()), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: "ses_kid", ParentOpenCodeID: "ses_root",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, conv := range []store.Conversation{root, child} {
		seedEvent(t, f, store.Event{
			OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: conv.ID,
			Source: store.SourceOpenCode, Seq: int64(i + 1), Name: "session.execution.started", At: recordedAt,
			Raw: contract.Event(t, "session.execution.started", 1, `{"sessionID":"`+conv.OpenCodeID+`"}`),
		})
	}
	got := map[string]string{}
	for _, ev := range runLog(t, f, &logInput{Ref: "mine"}) {
		if l, ok := ev.(LogEvent); ok && l.Name == "session.execution.started" {
			got[l.Session] = l.Parent
		}
	}
	if len(got) != 2 || got["ses_kid"] != "ses_root" || got["ses_root"] != "" {
		t.Errorf("the log's parents are %v, want ses_kid's to be ses_root and the root's none", got)
	}
}
