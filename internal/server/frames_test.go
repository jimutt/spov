package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
)

// frame wraps one payload as an SSE data frame.
func frame(data string) string { return "data: " + data + "\n\n" }

const durableFrame = `{"event":"session.text.ended","durability":"durable","sessionID":"ses_1","data":{"text":"hello"}}`

// projectString decodes a frame and projects it, which is what the
// recorder does for every frame that arrives.
func projectString(t *testing.T, raw string) (LogEvent, bool) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return LogEvent{}, false
	}
	return projectFrame(decoded, time.Unix(1700000000, 0).UTC())
}

// The listing is wrapped in a data envelope, and some builds answer with
// a bare array; neither shape has been pinned against a running sandbox,
// so both are read.
//
// @decision(DL-018)
func TestConversationsReadsEitherEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"data envelope", `{"data":[{"id":"ses_1","title":"one"},{"id":"ses_2"}]}`, []string{"ses_1", "ses_2"}},
		{"bare array", `[{"id":"ses_1","title":"one"}]`, []string{"ses_1"}},
		{"an entry with no id is skipped", `{"data":[{"title":"orphan"},{"id":"ses_1"}]}`, []string{"ses_1"}},
		{"not JSON", `<html>`, nil},
		{"empty", `{"data":[]}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, titles, _, _ := conversations([]byte(tc.body))
			if len(ids) != len(tc.want) {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
			for i, id := range ids {
				if id != tc.want[i] {
					t.Errorf("ids[%d] = %q, want %q", i, id, tc.want[i])
				}
			}
			if len(ids) > 0 && ids[0] == "ses_1" && tc.name != "an entry with no id is skipped" && titles["ses_1"] != "one" {
				t.Errorf("title = %q", titles["ses_1"])
			}
		})
	}
}

// A reply is split across parts, so the transcript wants every text a
// message carries, in the order it appears.
//
// @decision(DL-018)
func TestMessagesGatherEveryPartInOrder(t *testing.T) {
	body := `{"data":[
	  {"role":"user","parts":[{"type":"text","text":"first"}]},
	  {"role":"assistant","parts":[{"type":"text","text":"one"},{"type":"tool","name":"bash"},{"type":"text","text":"two"}]},
	  {"role":"assistant","parts":[]}
	]}`
	got := parseMessages([]byte(body))
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	if got[0].Role != "user" || got[0].Text != "first" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Text != "one\ntwo" {
		t.Errorf("the parts were not joined in order: %q", got[1].Text)
	}
	// A message with a role and no text is still a turn that happened.
	if got[2].Role != "assistant" || got[2].Text != "" {
		t.Errorf("third = %+v", got[2])
	}
}

// The same bytes must project the same way every time: a map's iteration
// order is not stable, and a transcript that reshuffles itself between
// two reads is worse than one that is occasionally incomplete.
//
// @decision(DL-018)
func TestTheProjectionIsStableAcrossReads(t *testing.T) {
	body := []byte(`{"data":[{"role":"assistant","alpha":{"text":"a"},"beta":{"text":"b"},"gamma":{"text":"c"},"delta":{"text":"d"}}]}`)
	first := parseMessages(body)
	for i := 0; i < 50; i++ {
		if got := parseMessages(body); got[0].Text != first[0].Text {
			t.Fatalf("read %d gave %q, first gave %q", i, got[0].Text, first[0].Text)
		}
	}
	// Sorted by field name — alpha, beta, delta, gamma — not by the
	// order they appear in the bytes, which encoding/json does not keep.
	if first[0].Text != "a\nb\nd\nc" {
		t.Errorf("text = %q, want the fields in field-name order", first[0].Text)
	}
}

// A message nested past the bound stops the walk rather than recursing
// through whatever the sandbox sent.
//
// @decision(DL-018)
func TestTheWalkIsBounded(t *testing.T) {
	deep := `{"text":"deep"}`
	for i := 0; i < maxCollectDepth+3; i++ {
		deep = `{"a":` + deep + `}`
	}
	if got := collectText(mustJSON(t, deep), 0); len(got) != 0 {
		t.Errorf("the walk went past its bound and found %v", got)
	}
	shallow := mustJSON(t, `{"a":{"text":"near"}}`)
	if got := collectText(shallow, 0); len(got) != 1 || got[0] != "near" {
		t.Errorf("a shallow text was missed: %v", got)
	}
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestMessagesFromRealSandboxShapes reads message envelopes captured
// verbatim from a running sandbox (opencode2 0.0.0-beta-18743), and pins
// that the speaker comes from `type`, not `role`.
//
// @decision(DL-018)
func TestMessagesFromRealSandboxShapes(t *testing.T) {
	const body = `{"data":[
      {"id":"msg_a","time":{"created":1788266066749,"completed":1788266067262},
       "type":"assistant","agent":"build",
       "model":{"id":"muse-spark-1.2","providerID":"opencode"},
       "content":[
         {"type":"reasoning","text":"the user greeted me, I should greet back",
          "state":{"itemId":"rs_1","reasoningEncryptedContent":"OPAQUE"}},
         {"type":"text","text":"Hi! I'm ready to help."}],
       "finish":"stop"},
      {"id":"msg_b","time":{"created":1788266065984},"text":"hi",
       "files":[],"agents":[],"type":"user"}]}`

	got := parseMessages([]byte(body))
	if len(got) != 2 {
		t.Fatalf("parsed %d messages, want 2: %+v", len(got), got)
	}

	if got[0].Role != "assistant" {
		t.Errorf("role = %q, want assistant — the speaker is `type`, not `role`", got[0].Role)
	}
	if got[1].Role != "user" {
		t.Errorf("role = %q, want user; every message reading as the agent is the bug", got[1].Role)
	}

	// The reply, and only the reply. A reasoning part is the model
	// thinking; prepending it to what the model said is not the
	// transcript, and leaks reasoning the user never saw.
	if got[0].Text != "Hi! I'm ready to help." {
		t.Errorf("assistant text = %q, want only the text part", got[0].Text)
	}
	if strings.Contains(got[0].Text, "greeted me") {
		t.Error("a reasoning part leaked into the reply")
	}
	if strings.Contains(got[0].Text, "OPAQUE") {
		t.Error("opaque part state leaked into the reply")
	}
	if got[1].Text != "hi" {
		t.Errorf("user text = %q, want the top-level text", got[1].Text)
	}

	// Timestamps, so a client can interleave these with live events
	// instead of showing two disconnected lists.
	if got[0].At == nil || got[1].At == nil {
		t.Fatalf("a message has no timestamp: %+v", got)
	}
	if !got[1].At.Before(*got[0].At) {
		t.Errorf("the user's message is not before the reply: %s vs %s", got[1].At, got[0].At)
	}
	if want := time.UnixMilli(1788266066749).UTC(); !got[0].At.Equal(want) {
		t.Errorf("At = %s, want %s", got[0].At, want)
	}
}

// TestProjectionFiltersAndShapes pins that only durable session events
// are projected, onto spov's own event, across the envelope layouts the
// by-name lookup is meant to survive.
//
// @decision(DL-018)
func TestProjectionFiltersAndShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want *LogEvent
	}{
		{
			name: "a durable session event, flat",
			raw:  `{"event":"session.text.ended","durability":"durable","sessionID":"ses_1","text":"hi"}`,
			want: &LogEvent{Name: "session.text.ended", Session: "ses_1", Text: "hi"},
		},
		{
			name: "the same, with the payload nested",
			raw:  `{"event":"session.step.ended","durability":"durable","data":{"sessionID":"ses_2","text":"step"}}`,
			want: &LogEvent{Name: "session.step.ended", Session: "ses_2", Text: "step"},
		},
		{
			name: "a type field rather than event",
			raw:  `{"type":"session.execution.succeeded","durability":"durable","sessionId":"ses_3"}`,
			want: &LogEvent{Name: "session.execution.succeeded", Session: "ses_3"},
		},
		// Ephemeral events are deltas and status, not a transcript.
		{name: "an ephemeral event", raw: `{"event":"session.text.delta","durability":"ephemeral","sessionID":"ses_1"}`},
		{name: "no durability at all", raw: `{"event":"session.text.ended","sessionID":"ses_1"}`},
		// Server-level events belong to nobody's transcript.
		{name: "a server event", raw: `{"event":"server.connected","durability":"durable"}`},
		{name: "not JSON", raw: `<html>`},
		{name: "no name", raw: `{"durability":"durable","sessionID":"ses_1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := projectString(t, tc.raw)
			if tc.want == nil {
				if ok {
					t.Fatalf("relayed %+v, want it filtered out", got)
				}
				return
			}
			if !ok {
				t.Fatal("filtered out an event the transcript needs")
			}
			if got.Name != tc.want.Name || got.Session != tc.want.Session || got.Text != tc.want.Text {
				t.Errorf("got %+v, want %+v", got, *tc.want)
			}
			if got.At.IsZero() {
				t.Error("the event carries no timestamp")
			}
		})
	}
}

// A top-level field beats one nested in the payload, so an envelope that
// names the session at both levels cannot be read as the wrong one.
//
// @decision(DL-018)
func TestTheOuterEnvelopeWinsOverThePayload(t *testing.T) {
	got, ok := projectString(t, `{"event":"session.text.ended","durability":"durable","sessionID":"outer","data":{"sessionID":"inner"}}`)
	if !ok {
		t.Fatal("filtered out")
	}
	if got.Session != "outer" {
		t.Errorf("session = %q, want the envelope's", got.Session)
	}
}

// TestProjectionAgainstRealSandboxFrames uses frames captured verbatim
// from a running sandbox (opencode2 0.0.0-beta-18743), and pins that the
// durable marker is a `durable` object. Keep the frames verbatim.
//
// @decision(DL-018)
func TestProjectionAgainstRealSandboxFrames(t *testing.T) {
	const (
		stepStarted = `{"id":"evt_000000000003SyntheticId003","created":1788265407953,` +
			`"type":"session.step.started","location":{"directory":"/workspace/spov"},` +
			`"data":{"sessionID":"ses_000000000020SyntheticId020","agent":"build",` +
			`"model":{"id":"ling-3.0-flash-fin-free","providerID":"opencode","variant":"default"},` +
			`"assistantMessageID":"msg_000000000001SyntheticId001","snapshot":"0badc0de"},` +
			`"durable":{"aggregateID":"ses_000000000020SyntheticId020","seq":46,"version":1}}`

		retryScheduled = `{"id":"evt_000000000004SyntheticId004","created":1788265407955,` +
			`"type":"session.retry.scheduled","location":{"directory":"/workspace/spov"},` +
			`"data":{"sessionID":"ses_000000000020SyntheticId020",` +
			`"assistantMessageID":"msg_000000000001SyntheticId001","attempt":3,"at":1788265412097,` +
			`"error":{"type":"provider.internal","message":"Upstream request failed","status":503}},` +
			`"durable":{"aggregateID":"ses_000000000020SyntheticId020","seq":47,"version":1}}`

		// No `durable` member and no session: server-level, and the
		// first frame every connection receives.
		serverConnected = `{"id":"evt_000000000002SyntheticId002","type":"server.connected","data":{}}`
	)

	got, ok := projectString(t, stepStarted)
	if !ok {
		t.Fatal("a real durable frame was dropped; the live tail would be silent")
	}
	if got.Name != "session.step.started" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Session != "ses_000000000020SyntheticId020" {
		t.Errorf("session = %q, want the id from data.sessionID", got.Session)
	}

	// The retry carries the provider's reason three levels down, which
	// is inside maxProjectDepth and is exactly what a watcher wants to
	// see when a session is producing nothing.
	got, ok = projectString(t, retryScheduled)
	if !ok {
		t.Fatal("a real retry frame was dropped")
	}
	if got.Text != "Upstream request failed" {
		t.Errorf("text = %q, want the provider's message", got.Text)
	}

	if _, ok := projectString(t, serverConnected); ok {
		t.Error("a server-level frame was relayed into a session's transcript")
	}

	// The user's own prompt, which is how the live view learns what was
	// asked without re-reading the sandbox. It sits four levels down,
	// inside maxProjectDepth, so the inbox event relays its Text and a
	// session shows both halves of the conversation.
	const inboxEnqueued = `{"id":"evt_000000000006SyntheticId006","created":1788267414303,` +
		`"type":"session.inbox.enqueued","location":{"directory":"/workspace/spov"},` +
		`"data":{"inboxID":"msg_000000000005SyntheticId005","sessionID":"ses_000000000019SyntheticId019",` +
		`"item":{"type":"user","payload":{"text":"reply with ok"},"delivery":"steer"}},` +
		`"durable":{"aggregateID":"ses_000000000019SyntheticId019","seq":30,"version":1}}`

	got, ok = projectString(t, inboxEnqueued)
	if !ok {
		t.Fatal("a user's prompt was dropped")
	}
	if got.Text != "reply with ok" {
		t.Errorf("text = %q, want the prompt from data.item.payload.text", got.Text)
	}
	if got.Session != "ses_000000000019SyntheticId019" {
		t.Errorf("session = %q", got.Session)
	}
	if got.Name != "session.inbox.enqueued" {
		t.Errorf("name = %q", got.Name)
	}

	// Real permission frames from spike S5 (0.0.0-beta-18684). Neither
	// carries a `durable` object, so both must be kept by name.
	//
	// @decision(DL-017)
	const permissionAsked = `{"id":"evt_000000000009SyntheticId009","created":1788384403589,` +
		`"type":"permission.asked","location":{"directory":"/root"},` +
		`"data":{"id":"per_000000000008SyntheticId008","sessionID":"ses_000000000018SyntheticId018",` +
		`"action":"shell","resources":["echo spov-s5-ask"],"save":["echo *"],` +
		`"source":{"type":"tool","messageID":"msg_000000000007SyntheticId007","id":"call_00000000000000000000000000000001"}}}`

	const permissionReplied = `{"id":"evt_000000000010SyntheticId010","created":1788384408259,` +
		`"type":"permission.replied","location":{"directory":"/root"},` +
		`"data":{"sessionID":"ses_000000000018SyntheticId018",` +
		`"requestID":"per_000000000008SyntheticId008","reply":"once"}}`

	// The form frames from spike S-F1 (2.0.22), which the page reads a
	// pending question from.
	//
	// @decision(DL-020)
	const formCreated = `{"id":"evt_000000000015SyntheticId015","created":1792353120018,"type":"form.created",` +
		`"location":{"directory":"/tmp/w"},"data":{"form":{"id":"frm_150922f12001d32I6jdH0V4l5T",` +
		`"sessionID":"ses_000000000018SyntheticId018","title":"Questions",` +
		`"metadata":{"kind":"question","tool":{"messageID":"msg_000000000014SyntheticId014","id":"call_1"}},` +
		`"fields":[{"key":"q0","title":"Colour","description":"Which colour?","type":"string",` +
		`"options":[{"value":"Red","label":"Red","description":"warm"},{"value":"Blue","label":"Blue","description":"cool"}],"custom":true}]}}}`
	const formReplied = `{"id":"evt_000000000016SyntheticId016","created":1792353121971,"type":"form.replied",` +
		`"location":{"directory":"/tmp/w"},"data":{"id":"frm_150922f12001d32I6jdH0V4l5T",` +
		`"sessionID":"ses_000000000018SyntheticId018","answer":{"q0":"Blue"}}}`
	const formCancelled = `{"id":"evt_000000000017SyntheticId017","created":1792353126024,"type":"form.cancelled",` +
		`"location":{"directory":"/tmp/w"},"data":{"id":"frm_150923efe001dAn1331x2mOet7",` +
		`"sessionID":"ses_000000000018SyntheticId018"}}`

	for _, tc := range []struct{ name, frame string }{
		{"permission.asked", permissionAsked},
		{"permission.replied", permissionReplied},
		{eventFormCreated, formCreated},
		{eventFormReplied, formReplied},
		{eventFormCancelled, formCancelled},
	} {
		got, ok = projectString(t, tc.frame)
		if !ok {
			t.Errorf("a real %s frame was dropped: neither the supervisor nor the page can see the ask", tc.name)
			continue
		}
		if got.Name != tc.name {
			t.Errorf("name = %q, want %q", got.Name, tc.name)
		}
		if got.Session != "ses_000000000018SyntheticId018" {
			t.Errorf("%s session = %q", tc.name, got.Session)
		}
		// Measured: neither carries a durable marker, which is what
		// DL-017's schema change is sized on. If this starts failing,
		// the frames became durable and the ephemeral source may no
		// longer be needed.
		if durableSeq(decodeFrame(t, tc.frame)) != 0 {
			t.Errorf("%s now carries a durable.seq; DL-017 is sized on it not having one", tc.name)
		}
	}

	// And the frames spov does not keep are still dropped: storing
	// every non-durable frame would put deltas and status into the
	// busiest table spov has, which is what DL-017 exists to prevent.
	const messageDelta = `{"id":"evt_1","type":"message.part.delta","data":{"sessionID":"ses_x","text":"pa"}}`
	if _, ok := projectString(t, messageDelta); ok {
		t.Error("an ephemeral delta was kept: only the named permission frames are")
	}
}

// decodeFrame parses a frame the way the recorder does, so a test can
// ask the same questions of it.
func decodeFrame(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// A tool part's output is not message text: collecting it would render a
// whole file the agent read as though the agent had typed it. The fixture
// is a real stored message.
//
// @decision(DL-058) @decision(DL-018)
func TestAToolPartsOutputIsNotTheMessageText(t *testing.T) {
	const body = `{"data":[{
	  "type":"assistant",
	  "content":[
	    {"type":"reasoning","text":"thinking about it"},
	    {"type":"tool","name":"read","state":{"status":"completed",
	      "content":[{"type":"text","text":"# spov\n\nthe entire readme, sixteen kilobytes of it"}]}},
	    {"type":"text","text":"Found the project README."}
	  ]}]}`

	msgs := parseMessages([]byte(body))
	if len(msgs) != 1 {
		t.Fatalf("parsed %d messages, want 1", len(msgs))
	}
	got := msgs[0].Text

	if !strings.Contains(got, "Found the project README.") {
		t.Errorf("the agent's own words were dropped: %q", got)
	}
	if strings.Contains(got, "sixteen kilobytes") {
		t.Errorf("a tool's output was collected into the message text, so the transcript "+
			"renders the file as though the agent had typed it: %q", got)
	}
	// The reasoning exclusion must still hold; this adds to it rather
	// than replacing it.
	if strings.Contains(got, "thinking about it") {
		t.Errorf("a reasoning part was collected: %q", got)
	}
}

// The stop marker's message shape, captured from a real job, parses to
// what loopStoppedInMessages matches on: role "synthetic" and a text that
// starts with the marker. A synthetic message has no `content` array (its
// text is at the top level) and no `role` key (the role comes from `type`).
//
// @decision(DL-048) @decision(DL-018)
func TestTheStopMarkerIsFoundInARealMessageShape(t *testing.T) {
	const captured = `{"data":[{"id":"msg_synthetic_capture",` +
		`"time":{"created":1788534620484},` +
		`"text":"SPOV_LOOP_STOPPED {\"reason\":\"declared\",\"continuations\":0,` +
		`\"maxContinuations\":40,\"turns\":0}",` +
		`"type":"synthetic"}]}`

	msgs := parseMessages([]byte(captured))
	if len(msgs) != 1 {
		t.Fatalf("parsed %d messages, want 1", len(msgs))
	}
	got := msgs[0]

	// The role the recovery filters on.
	if got.Role != "synthetic" {
		t.Errorf("role = %q, want %q: the recovery filters on it, so anything else means "+
			"a lost stop marker is never recovered and the job strands", got.Role, "synthetic")
	}
	// The text the recovery prefix-matches.
	if !strings.HasPrefix(got.Text, opencode.LoopStoppedMarker) {
		t.Errorf("text = %q, want it to START WITH %q: the recovery matches on the prefix",
			got.Text, opencode.LoopStoppedMarker)
	}
}

// TestTheTurnStartedFrameReachesTheStore pins that
// `session.execution.started`, which DL-059's Stop control depends on,
// survives projectFrame under its own name. No constant names it in this
// package, so nothing else would notice it being dropped.
//
// @decision(DL-059) @decision(DL-018)
func TestTheTurnStartedFrameReachesTheStore(t *testing.T) {
	const turnStarted = `{"id":"evt_000000000003SyntheticId003","created":1788265407953,` +
		`"type":"session.execution.started","location":{"directory":"/workspace"},` +
		`"data":{"sessionID":"ses_000000000020SyntheticId020"},` +
		`"durable":{"aggregateID":"ses_000000000020SyntheticId020","seq":45,"version":1}}`

	got, ok := projectString(t, turnStarted)
	if !ok {
		t.Fatal("session.execution.started was dropped: DL-059's Stop control has no signal," +
			" and it would never be drawn with nothing logged either")
	}
	if got.Name != "session.execution.started" {
		t.Errorf("name = %q: the browser matches on this exact string (web/src/lib/steer.ts"+
			" TurnStarted), so anything else leaves Stop permanently hidden", got.Name)
	}
	if got.Session != "ses_000000000020SyntheticId020" {
		t.Errorf("session = %q, want the id from data.sessionID", got.Session)
	}

	// The page clears in-flight on all three enders. `.succeeded` is
	// covered by the supervisor's tests; these two are not.
	for _, name := range []string{"session.execution.interrupted", "session.execution.failed"} {
		raw := `{"id":"evt_1","created":1788265407953,"type":"` + name + `",` +
			`"data":{"sessionID":"ses_1","reason":"user"},` +
			`"durable":{"aggregateID":"ses_1","seq":46,"version":1}}`
		ended, ok := projectString(t, raw)
		if !ok {
			t.Errorf("%s was dropped: the page never learns the turn ended and Stop stays"+
				" on screen with nothing to stop", name)
			continue
		}
		if ended.Name != name {
			t.Errorf("name = %q, want %q", ended.Name, name)
		}
	}
}

// TestTheTurnEventNamesAgreeWithTheBrowser checks steer.ts's turn events
// against turnEnded: the three enders in both directions, and that the
// start event does not read as an ending. The start frame itself is held
// by TestTheTurnStartedFrameReachesTheStore.
//
// @decision(DL-059) @decision(DL-017)
func TestTheTurnEventNamesAgreeWithTheBrowser(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "steer.ts"))
	if err != nil {
		t.Fatalf("reading the browser's copy: %v", err)
	}
	browser := string(src)

	started := quotedAfter(browser, "export const TurnStarted = ")
	if started != "session.execution.started" {
		t.Fatalf("the browser starts a turn on %q; the recorder keeps that frame because it is"+
			" durable rather than because it is named, so a rename here leaves Stop"+
			" permanently hidden with nothing logged in either tree", started)
	}
	if turnEnded(started) {
		t.Errorf("turnEnded(%q) is true: a turn BEGINNING would be read as one ending,"+
			" and the recorder would re-read the conversation on every one", started)
	}

	enders := quotedBlock(browser, "export const TurnEnders: readonly string[] = [", "];")
	if len(enders) != 3 {
		t.Fatalf("the browser lists %d ending events, want the three turnEnded knows: %v",
			len(enders), enders)
	}
	for _, name := range enders {
		if !turnEnded(name) {
			t.Errorf("turnEnded(%q) is false. The browser clears its in-flight flag on this"+
				" frame and the recorder does not treat it as ending a turn — one of the two"+
				" has drifted, and the browser's half leaves Stop stuck on screen with"+
				" nothing to stop", name)
		}
	}
}

// quotedAfter returns the first double-quoted string following marker.
func quotedAfter(src, marker string) string {
	at := strings.Index(src, marker)
	if at < 0 {
		return ""
	}
	rest := src[at+len(marker):]
	open := strings.Index(rest, `"`)
	if open < 0 {
		return ""
	}
	rest = rest[open+1:]
	close := strings.Index(rest, `"`)
	if close < 0 {
		return ""
	}
	return rest[:close]
}

// quotedBlock returns every double-quoted string between open and the
// first close after it.
func quotedBlock(src, open, close string) []string {
	at := strings.Index(src, open)
	if at < 0 {
		return nil
	}
	rest := src[at+len(open):]
	end := strings.Index(rest, close)
	if end < 0 {
		return nil
	}
	// Split on the quote character: the odd-numbered pieces are what
	// sat between a pair of them.
	var out []string
	parts := strings.Split(rest[:end], `"`)
	for i := 1; i < len(parts); i += 2 {
		out = append(out, parts[i])
	}
	return out
}

// A /message page as GA 2.0.22 answered it (captured 2026-10-02 from a
// local server against a fake model), newest first. GA adds an "idle"
// message at the end of every turn, with no text: it is kept as its own
// role, carries nothing for the transcript, and is not a stop marker.
//
// @decision(DL-035)
func TestTheIdleMessageGAAddsIsKeptAndSaysNothing(t *testing.T) {
	body := []byte(`{"data":[{"id":"msg_000000000013SyntheticId013","time":{"created":1790969388006},"type":"idle","outcome":"succeeded"},{"id":"msg_000000000012SyntheticId012","time":{"created":1790969387739,"streamed":1790969388002,"completed":1790969388003},"type":"assistant","agent":"build","model":{"id":"m","providerID":"probe"},"content":[{"type":"text","text":"ok reply"}],"finish":"stop","rawFinish":"stop","cost":0,"tokens":{"input":5,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}},{"id":"msg_000000000011SyntheticId011","time":{"created":1790969387704},"text":"say hi","type":"user"}],"cursor":{"previous":"eyJpZCI6Im1zZ18wMDAwMDAwMDAwMTNTeW50aGV0aWNJZDAxMyIsIm9yZGVyIjoiZGVzYyIsImRpcmVjdGlvbiI6InByZXZpb3VzIn0","next":"eyJpZCI6Im1zZ18wMDAwMDAwMDAwMTFTeW50aGV0aWNJZDAxMSIsIm9yZGVyIjoiZGVzYyIsImRpcmVjdGlvbiI6Im5leHQifQ"}}`)
	got := parseMessages(body)
	if len(got) != 3 {
		t.Fatalf("parsed %d messages, want 3: %+v", len(got), got)
	}
	idle, reply, prompt := got[0], got[1], got[2]
	if idle.Role != "idle" || idle.Text != "" || idle.ID == "" || idle.At == nil {
		t.Errorf("idle message = %+v, want role idle, no text, its id and time", idle.MessageView)
	}
	if reply.Role != "assistant" || reply.Text != "ok reply" {
		t.Errorf("reply = %+v", reply.MessageView)
	}
	if prompt.Role != "user" || prompt.Text != "say hi" {
		t.Errorf("prompt = %+v", prompt.MessageView)
	}
	var msgs []store.Message
	for _, m := range got {
		msgs = append(msgs, store.Message{Role: m.Role, Text: m.Text})
	}
	if jobs.StoppedIn(msgs) {
		t.Error("an idle message was read as the loop's stop marker")
	}
}
