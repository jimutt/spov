package server

import (
	"testing"

	"github.com/jimutt/spov/internal/jobs"
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
	continuationFrame = `{"type":"session.inbox.enqueued","data":{"sessionID":"ses_p","inboxID":"msg_c",` +
		`"item":{"type":"synthetic","payload":{"text":"Continue working"},"delivery":"queue"}}}`
)

// The wire says which synthetic item is a subagent's report, and whose,
// from the stored frame and message; nothing else is one.
//
// @decision(DL-144)
func TestASubagentsReportIsNamedOnTheWire(t *testing.T) {
	ev := store.Event{Source: store.SourceOpenCode, Name: jobs.EventInboxEnqueued, Raw: []byte(reportFrame)}
	got := storedReport(ev, storedRole(ev))
	if got == nil || got.Child != "ses_e1473bc9affePssI1WmxBG7Ksh" || got.State != "completed" {
		t.Errorf("the report frame read as %+v", got)
	}
	m := messageViewOf(store.Message{OpenCodeID: "msg_1eb8e2096001rsTFIhAbeAfYj3", Role: "synthetic", Text: "x", Raw: []byte(reportMessage)})
	if m.Report == nil || m.Report.Child != "ses_e1471df72ffecZP6FsFzTsQbBa" || m.Report.State != "completed" {
		t.Errorf("the report message read as %+v", m.Report)
	}

	for _, tc := range []struct{ name, raw string }{
		{"a continuation", continuationFrame},
		{"a person's item claiming to be a report", replaceOnce(reportFrame, `"type":"synthetic"`, `"type":"user"`)},
		{"a report naming no child", replaceOnce(reportFrame, `"childID":"ses_e1473bc9affePssI1WmxBG7Ksh",`, ``)},
		{"another source", replaceOnce(reportFrame, `"source":"subagent"`, `"source":"plugin"`)},
		{"unreadable", `{"data":{"item":`},
	} {
		ev := store.Event{Source: store.SourceOpenCode, Name: jobs.EventInboxEnqueued, Raw: []byte(tc.raw)}
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
