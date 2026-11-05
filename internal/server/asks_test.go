package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
)

// askOn stores an ask frame in the fixture's session as the recorder
// would: ephemeral, in its first conversation, in the shape S-F1
// measured.
func askOn(t *testing.T, svc *Service, sess store.Session, name, id string, source store.EventSource) {
	t.Helper()
	ctx := context.Background()
	convs, err := svc.deps.Store.Conversations().ListConversations(ctx, sess.OrgID, sess.ID)
	if err != nil || len(convs) == 0 {
		t.Fatalf("no conversation: %v", err)
	}
	raw := `{"type":"permission.asked","data":{"id":"` + id + `","sessionID":"ses_1","action":"glob","resources":["*.txt"]}}`
	if name == eventFormCreated {
		raw = `{"type":"form.created","data":{"form":{"id":"` + id + `","sessionID":"ses_1","title":"Questions",` +
			`"fields":[{"key":"q0","type":"string","options":[{"value":"Blue","label":"Blue"}]}]}}}`
	}
	ev := store.Event{
		ID: store.NewID(svc.deps.Now()), OrgID: sess.OrgID, SessionID: sess.ID, ConversationID: convs[0].ID,
		Source: source, Name: name, At: svc.deps.Now(), Raw: []byte(raw),
	}
	if source == store.SourceOpenCode {
		ev.Seq = 99
	}
	if _, err := svc.deps.Store.Conversations().PutEvents(ctx, []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
}

// Allowing once relays OpenCode's reply route in the conversation the
// request was asked in, records who answered, and on a job writes the
// marker the supervisor releases a blocked job on.
//
// @decision(DL-020) @decision(DL-054)
func TestAllowingOnceRelaysTheReply(t *testing.T) {
	f := newSteerFixtureAt(t, store.JobBlocked, "ses_1")
	askOn(t, f.svc, f.sess, eventPermissionAsked, "per_a", store.SourceEphemeral)
	got, err := f.svc.ReplyPermission(context.Background(), f.p, f.sess.ID, "per_a", PermissionReply{Decision: "once"})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 1 || !strings.HasSuffix(calls[0].path, "/api/session/ses_1/permission/per_a/reply") ||
		calls[0].body["decision"] != "once" || calls[0].body["message"] != nil {
		t.Fatalf("the sandbox saw %+v", calls)
	}
	if got.Conversation != "ses_1" || got.Decision != "once" {
		t.Errorf("answered %+v", got)
	}
	if marks := markersOn(t, f.svc, f.sess, MarkerPermissionReplied); len(marks) != 1 ||
		!strings.Contains(marks[0].Text, "allowed once by "+f.p.UserID) {
		t.Errorf("audit markers %+v", marks)
	}
	marks := markersOn(t, f.svc, f.sess, jobs.MarkerAnswered)
	if len(marks) != 1 || jobs.AnsweredRequest(marks[0].Raw) != "per_a" {
		t.Errorf("job markers %+v", marks)
	}
}

// A reject carries the reader's message to the model; allow-once
// carries none, and "always" is refused before anything is sent.
//
// @decision(DL-020)
func TestOnlyAllowOnceAndRejectAreRelayed(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	askOn(t, f.svc, f.sess, eventPermissionAsked, "per_a", store.SourceEphemeral)
	ctx := context.Background()
	if _, err := f.svc.ReplyPermission(ctx, f.p, f.sess.ID, "per_a",
		PermissionReply{Decision: "reject", Message: "  use read instead "}); err != nil {
		t.Fatal(err)
	}
	if body := f.calls()[0].body; body["decision"] != "reject" || body["message"] != "use read instead" {
		t.Errorf("the sandbox saw %v", body)
	}
	for _, bad := range []PermissionReply{
		{Decision: "always"},
		{Decision: ""},
		{Decision: "once", Message: "a message with an allow"},
		{Decision: "reject", Message: strings.Repeat("x", MaxMessageBytes+1)},
	} {
		if _, err := f.svc.ReplyPermission(ctx, f.p, f.sess.ID, "per_a", bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%q: err = %v, want a refusal", bad.Decision, err)
		}
	}
	if n := len(f.calls()); n != 1 {
		t.Errorf("a refused reply reached the sandbox: %d requests", n)
	}
	// The audit marker never carries the reader's words.
	for _, m := range markersOn(t, f.svc, f.sess, MarkerPermissionReplied) {
		if strings.Contains(m.Text, "read instead") {
			t.Errorf("the marker carries the message: %q", m.Text)
		}
	}
}

// Only an ask the store holds from the sandbox can be answered: not an
// unknown id, not one in another session, not a frame of another kind
// or source, not a malformed id.
//
// @decision(DL-020)
func TestOnlyAStoredAskCanBeAnswered(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	ctx := context.Background()
	other := interactiveFixture(t, f, "ses_2")
	askOn(t, f.svc, other, eventPermissionAsked, "per_elsewhere", store.SourceEphemeral)
	askOn(t, f.svc, f.sess, eventFormCreated, "frm_a", store.SourceEphemeral)
	askOn(t, f.svc, f.sess, eventPermissionAsked, "per_durable", store.SourceOpenCode)
	for _, id := range []string{"per_unknown", "per_elsewhere", "per_durable"} {
		if _, err := f.svc.ReplyPermission(ctx, f.p, f.sess.ID, id, PermissionReply{Decision: "once"}); !errors.Is(err, ErrNoSuchAsk) {
			t.Errorf("%s: err = %v, want no such request", id, err)
		}
	}
	if _, err := f.svc.AnswerForm(ctx, f.p, f.sess.ID, "frm_b", FormAnswer{Answer: map[string]any{"q0": "Blue"}}); !errors.Is(err, ErrNoSuchAsk) {
		t.Errorf("an unknown form: err = %v", err)
	}
	for _, id := range []string{"frm_a", "per_../x", "evt_1"} {
		if _, err := f.svc.ReplyPermission(ctx, f.p, f.sess.ID, id, PermissionReply{Decision: "once"}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want a malformed id", id, err)
		}
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("an unknown ask reached the sandbox: %d requests", n)
	}
}

// A form is answered keyed by field and dismissed with a DELETE; each
// leaves its marker, and an interactive session gets no job marker.
//
// @decision(DL-020)
func TestAFormIsAnsweredOrDismissed(t *testing.T) {
	f := newSteerFixture(t)
	inter := interactiveFixture(t, f, "ses_1")
	askOn(t, f.svc, inter, eventFormCreated, "frm_a", store.SourceEphemeral)
	ctx := context.Background()
	answer := map[string]any{"q0": "Blue", "q1": []any{"No"}, "q2": true, "q3": 3.0}
	if _, err := f.svc.AnswerForm(ctx, f.p, inter.ID, "frm_a", FormAnswer{Answer: answer}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.DismissForm(ctx, f.p, inter.ID, "frm_a"); err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 2 || !strings.HasSuffix(calls[0].path, "/api/session/ses_1/form/frm_a/reply") ||
		calls[1].method != http.MethodDelete || !strings.HasSuffix(calls[1].path, "/api/session/ses_1/form/frm_a") {
		t.Fatalf("the sandbox saw %+v", calls)
	}
	sent, _ := calls[0].body["answer"].(map[string]any)
	if sent["q0"] != "Blue" || sent["q2"] != true {
		t.Errorf("the sandbox saw %v", calls[0].body)
	}
	if len(markersOn(t, f.svc, inter, MarkerFormAnswered)) != 1 || len(markersOn(t, f.svc, inter, MarkerFormDismissed)) != 1 {
		t.Error("an act left no audit marker")
	}
	if n := len(markersOn(t, f.svc, inter, jobs.MarkerAnswered)); n != 0 {
		t.Errorf("an interactive session got %d job markers", n)
	}
	for _, bad := range []map[string]any{nil, {"q0": map[string]any{}}, {"q1": []any{1.0}}} {
		if _, err := f.svc.AnswerForm(ctx, f.p, inter.ID, "frm_a", FormAnswer{Answer: bad}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%v: err = %v, want a refusal", bad, err)
		}
	}
}

// Answers take from the same per-session ceiling as sends, and need a
// running session.
//
// @decision(DL-020) @decision(DL-019)
func TestAnsweringIsBoundedLikeSending(t *testing.T) {
	f := newSteerFixture(t, "ses_1")
	askOn(t, f.svc, f.sess, eventPermissionAsked, "per_a", store.SourceEphemeral)
	ctx := context.Background()
	var err error
	for i := 0; i <= MessagesPerMinute && err == nil; i++ {
		_, err = f.svc.ReplyPermission(ctx, f.p, f.sess.ID, "per_a", PermissionReply{Decision: "once"})
	}
	if !errors.Is(err, ErrTooManyMessages) {
		t.Errorf("err = %v, want the message ceiling", err)
	}
	stopped := f.sess
	stopped.State = store.StateStopped
	if err := f.svc.deps.Store.Sessions().Update(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.DismissForm(ctx, f.p, f.sess.ID, "frm_a"); !errors.Is(err, ErrSessionNotRunning) {
		t.Errorf("a stopped session: err = %v", err)
	}
}

// Over HTTP: the routes are registered, an unknown ask is a 404 that
// says so, and "always" is a 422.
//
// @decision(DL-020)
func TestTheAnswerRoutesCarryTheirStatus(t *testing.T) {
	f := newAPIFixture(t)
	sess := runningSession(t, f)
	seedConversation(t, f, sess, "ses_1", "one")
	base := "/api/v1/sessions/" + sess.ID
	res, body := f.do(http.MethodPost, base+"/permissions/per_x", PermissionReply{Decision: "once"})
	if res.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "no such request") {
		t.Errorf("an unknown request = %d %s", res.StatusCode, body)
	}
	res, body = f.do(http.MethodPost, base+"/permissions/per_x", map[string]any{"decision": "always"})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("always = %d %s, want 422", res.StatusCode, body)
	}
	res, _ = f.do(http.MethodPost, base+"/forms/frm_x", FormAnswer{Answer: map[string]any{"q0": "a"}})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown form = %d", res.StatusCode)
	}
	res, _ = f.do(http.MethodDelete, base+"/forms/frm_x", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("dismissing an unknown form = %d", res.StatusCode)
	}
}

// The page reads an ask from the log: each of the five frames, in the
// shapes S-F1 measured, projects what drawing and answering it need,
// and nothing else does.
//
// @decision(DL-020)
func TestTheLogCarriesTheAsks(t *testing.T) {
	ephemeral := func(name, raw string) store.Event {
		return store.Event{Source: store.SourceEphemeral, Name: name, Raw: []byte(raw)}
	}
	asked := storedAsk(ephemeral(eventPermissionAsked, `{"type":"permission.asked","data":{"id":"per_1","sessionID":"ses",`+
		`"action":"glob","resources":["*.txt"],"save":["*"],"metadata":{"root":"."},"source":{"type":"tool","messageID":"msg_1","id":"call_1"}}}`))
	if asked == nil || asked.ID != "per_1" || asked.Kind != "permission" || asked.Action != "glob" ||
		len(asked.Resources) != 1 || asked.Call != "call_1" {
		t.Errorf("permission.asked = %+v", asked)
	}
	replied := storedAsk(ephemeral("permission.replied", `{"data":{"sessionID":"ses","requestID":"per_1","reply":"reject"}}`))
	if replied == nil || replied.ID != "per_1" || replied.Reply != "reject" {
		t.Errorf("permission.replied = %+v", replied)
	}
	created := storedAsk(ephemeral(eventFormCreated, `{"data":{"form":{"id":"frm_1","sessionID":"ses","title":"Questions",`+
		`"metadata":{"kind":"question","tool":{"messageID":"msg_1","id":"call_2"}},"fields":[{"key":"q0","title":"Colour",`+
		`"description":"Which colour?","type":"string","options":[{"value":"Red","label":"Red","description":"warm"}],"custom":true},`+
		`{"key":"q1","type":"multiselect","options":[{"value":"No","label":"No"}]}]}}}`))
	if created == nil || created.ID != "frm_1" || created.Kind != "form" || created.Call != "call_2" || len(created.Fields) != 2 ||
		!created.Fields[0].Custom || created.Fields[0].Options[0].Description != "warm" || created.Fields[1].Type != "multiselect" {
		t.Errorf("form.created = %+v", created)
	}
	answered := storedAsk(ephemeral(eventFormReplied, `{"data":{"id":"frm_1","sessionID":"ses","answer":{"q0":"Blue","q1":["No"]}}}`))
	if answered == nil || answered.ID != "frm_1" || answered.Answer["q0"] != "Blue" {
		t.Errorf("form.replied = %+v", answered)
	}
	if got := storedAsk(ephemeral(eventFormCancelled, `{"data":{"id":"frm_1","sessionID":"ses"}}`)); got == nil || got.ID != "frm_1" {
		t.Errorf("form.cancelled = %+v", got)
	}
	// A durable frame of the same name is the sandbox's word on
	// something else, and an unrelated ephemeral row carries no ask.
	durable := ephemeral(eventPermissionAsked, `{"data":{"id":"per_1"}}`)
	durable.Source = store.SourceOpenCode
	for _, ev := range []store.Event{durable, ephemeral("session.status", `{"data":{"id":"x"}}`),
		ephemeral(eventPermissionAsked, `{"data":{}}`)} {
		if got := storedAsk(ev); got != nil {
			t.Errorf("%s from %s carried %+v", ev.Name, ev.Source, got)
		}
	}
}

// A job past its run, or not yet in one, takes no answer; a blocked job
// does.
//
// @decision(DL-020) @decision(DL-052)
func TestAnAnswerFollowsTheJobsState(t *testing.T) {
	f := newSteerFixtureAt(t, store.JobDelivering, "ses_1")
	askOn(t, f.svc, f.sess, eventPermissionAsked, "per_a", store.SourceEphemeral)
	if _, err := f.svc.ReplyPermission(context.Background(), f.p, f.sess.ID, "per_a", PermissionReply{Decision: "once"}); !errors.Is(err, ErrJobNotSteerable) {
		t.Errorf("a delivering job: err = %v", err)
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("the refused answer reached the sandbox: %d requests", n)
	}
}

// Only a web address survives as a form field's link: it reaches a page
// as an href.
//
// @decision(DL-020)
func TestAFormLinkIsOnlyAWebAddress(t *testing.T) {
	for raw, kept := range map[string]bool{
		"https://example.com/x": true, "http://example.com": true,
		"javascript:alert(1)": false, "data:text/html,x": false, "/relative": false, "": false,
		"JavaScript:alert(1)": false,
	} {
		ev := store.Event{Source: store.SourceEphemeral, Name: eventFormCreated, Raw: []byte(
			`{"data":{"form":{"id":"frm_1","fields":[{"key":"k","type":"external","url":` + strconvQuote(raw) + `}]}}}`)}
		got := storedAsk(ev)
		if got == nil || len(got.Fields) != 1 {
			t.Fatalf("%q: %+v", raw, got)
		}
		if (got.Fields[0].URL != "") != kept {
			t.Errorf("%q: kept as %q", raw, got.Fields[0].URL)
		}
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
