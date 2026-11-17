package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

// A frame is checked against its own type's schema in the event union,
// and every kind of drift is named.
//
// @decision(DL-145)
func TestEventFramesAreCheckedAgainstTheirTypesSchema(t *testing.T) {
	const enqueued = `{"id":"evt_1","created":1794958241104,"type":"session.inbox.enqueued",` +
		`"location":{"directory":"/workspace"},"data":{"inboxID":"msg_1","sessionID":"ses_1",` +
		`"item":{"type":"user","payload":{"text":"hi"},"delivery":"steer"}},` +
		`"durable":{"aggregateID":"ses_1","seq":2,"version":1}}`
	for _, c := range []struct {
		name, frame, wantErr string
	}{
		{"a captured enqueue", enqueued, ""},
		{"the stream's greeting", `{"id":"evt_2","type":"server.connected","data":{}}`, ""},
		{"a plugin's call, typed by pattern", `{"id":"evt_3","created":1,"type":"rpc.spov.ping","location":{"directory":"/w"},"data":{}}`, ""},
		{"spov's own marker", `{"type":"spov.recorder.gap","anything":true}`, ""},
		{"a type the build does not have", `{"id":"evt_4","type":"message.part.delta","data":{}}`, `has no event type "message.part.delta"`},
		{"no type", `{"id":"evt_5","data":{}}`, "has no type"},
		{"a type that is not text", `{"type":7}`, "has no type"},
		{"not JSON", `{`, "not JSON"},
		{"not an object", `[1]`, "not an object"},
		{"two values", enqueued + ` {}`, "after its JSON value"},
		{"a missing envelope field", strings.Replace(enqueued, `"created":1794958241104,`, ``, 1), `missing required field "created"`},
		{"a missing data field", strings.Replace(enqueued, `"inboxID":"msg_1",`, ``, 1), `data: missing required field "inboxID"`},
		{"an id of the wrong shape", strings.Replace(enqueued, `"msg_1"`, `"inb_1"`, 1), `data.inboxID: "inb_1" does not match ^msg_`},
		{"a field the type does not have", strings.Replace(enqueued, `"delivery":"steer"}`, `"delivery":"steer","extra":1}`, 1), `unknown field "extra"`},
		{"a durable version the type does not have", strings.Replace(enqueued, `"version":1`, `"version":2`, 1), `durable.version: 2 is not one of [1]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckEvent([]byte(c.frame))
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.wantErr != "" && err == nil:
				t.Errorf("accepted, want an error containing %q", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("error %q, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

// The session log has a union of its own: the durable events and the
// sync point, and none of the stream's ephemeral events.
//
// @decision(DL-145)
func TestLogItemsAreCheckedAgainstTheLogsUnion(t *testing.T) {
	if err := CheckLogItem([]byte(`{"type":"log.synced","aggregateID":"ses_1","seq":4}`)); err != nil {
		t.Errorf("the sync point was refused: %v", err)
	}
	if err := CheckLogItem([]byte(`{"type":"log.synced","seq":4}`)); err == nil {
		t.Error("a sync point with no aggregate was accepted")
	}
	delta := `{"id":"evt_1","created":1,"type":"session.text.delta","data":{"sessionID":"ses_1","assistantMessageID":"msg_1","ordinal":0,"delta":"x"}}`
	if err := CheckEvent([]byte(delta)); err != nil {
		t.Errorf("a delta was refused on the stream: %v", err)
	}
	if err := CheckLogItem([]byte(delta)); err == nil || !strings.Contains(err.Error(), "has no event type") {
		t.Errorf("a delta was accepted as a log item: %v", err)
	}
}

// A union's branches are found through $ref and nested anyOf, and a
// document whose union cannot be read says so.
//
// @decision(DL-145)
func TestAUnionIsReadThroughItsReferences(t *testing.T) {
	d, err := Parse([]byte(`{"components":{"schemas":{
	  "U": {"anyOf": [{"$ref": "#/components/schemas/Inner"}, {"$ref": "#/components/schemas/b"}]},
	  "Inner": {"anyOf": [{"$ref": "#/components/schemas/a"}]},
	  "a": {"type": "object", "properties": {"type": {"enum": ["a"]}, "n": {"type": "integer"}}, "required": ["type", "n"]},
	  "b": {"type": "object", "properties": {"type": {"type": "string", "pattern": "^b\\."}}},
	  "Untyped": {"anyOf": [{"type": "object", "properties": {"type": {"type": "string"}}}]},
	  "NotASchema": {"anyOf": [3]},
	  "BadRef": {"anyOf": [{"$ref": "#/elsewhere"}]},
	  "BadPattern": {"anyOf": [{"type": "object", "properties": {"type": {"type": "string", "pattern": "("}}}]},
	  "Deep": {"anyOf": [{"$ref": "#/components/schemas/Deep"}]}
	}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	for frame, wantErr := range map[string]string{
		`{"type":"a","n":1}`:   "",
		`{"type":"a","n":"x"}`: "a.n: string is not of type integer",
		`{"type":"b.any"}`:     "",
		`{"type":"c"}`:         `has no event type "c"`,
	} {
		err := d.CheckFrame("U", []byte(frame))
		if (err == nil) != (wantErr == "") || (err != nil && !strings.Contains(err.Error(), wantErr)) {
			t.Errorf("%s: error %v, want %q", frame, err, wantErr)
		}
	}
	for union, wantErr := range map[string]string{
		"Missing":    `no schema "Missing"`,
		"Untyped":    "neither an enum nor a pattern",
		"NotASchema": "not a schema",
		"BadRef":     "unsupported $ref",
		"BadPattern": "pattern",
		"Deep":       "too deep",
	} {
		if err := d.CheckFrame(union, []byte(`{"type":"a","n":1}`)); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: error %v, want %q", union, err, wantErr)
		}
	}
}

// Event builds the envelope the document asks of a type: a durable
// position only where the type has one, at the version it allows.
//
// @decision(DL-145)
func TestEventBuildsTheEnvelopeOfItsType(t *testing.T) {
	var frame struct {
		ID      string         `json:"id"`
		Durable map[string]any `json:"durable"`
	}
	raw := Event(t, "session.tool.success", 9, `{"sessionID":"ses_1","assistantMessageID":"msg_1","id":"call_1",`+
		`"content":[{"type":"text","text":"ok"}],"executed":true}`)
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Durable["aggregateID"] != "ses_1" || frame.Durable["seq"] != float64(9) || frame.Durable["version"] != float64(2) {
		t.Errorf("durable = %v, want ses_1 at 9, version 2", frame.Durable)
	}
	if !strings.HasPrefix(frame.ID, "evt_") {
		t.Errorf("id = %q", frame.ID)
	}
	raw = Event(t, "permission.asked", 0, `{"id":"per_1","sessionID":"ses_1","action":"read","resources":[]}`)
	if strings.Contains(string(raw), "durable") {
		t.Errorf("an ephemeral type was given a durable position: %s", raw)
	}

	rec := &recorder{TB: t}
	Event(rec, "session.inbox.enqueued", 1, `{"sessionID":"ses_1"}`)
	if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], `missing required field "inboxID"`) {
		t.Errorf("reported %q, want the missing inboxID", rec.errors)
	}
	for _, c := range []struct{ typ, data, want string }{
		{"no.such.type", `{}`, "has no event type"},
		{"session.idle", `[`, "not a JSON object"},
	} {
		fatal := &fatalRecorder{recorder: recorder{TB: t}}
		func() {
			defer func() { _ = recover() }()
			Event(fatal, c.typ, 1, c.data)
		}()
		if len(fatal.errors) != 1 || !strings.Contains(fatal.errors[0], c.want) {
			t.Errorf("%s: reported %q, want %q", c.typ, fatal.errors, c.want)
		}
	}
}

// Frame reports a refused fixture, and Unchecked insists on a reason.
//
// @decision(DL-145)
func TestAFixtureIsCheckedUnlessItSaysWhyNot(t *testing.T) {
	rec := &recorder{TB: t}
	if got := Frame(rec, `{"type":"nothing.here"}`); string(got) != `{"type":"nothing.here"}` {
		t.Errorf("Frame returned %s", got)
	}
	CheckLogFrame(rec, []byte(`{"type":"log.synced","note":"`+strings.Repeat("x", 700)+`"}`))
	if len(rec.errors) != 2 || !strings.Contains(rec.errors[1], "…") {
		t.Errorf("reported %q, want both fixtures, the long one clipped", rec.errors)
	}
	if got := Unchecked(`{`, "cut off"); string(got) != `{` {
		t.Errorf("Unchecked returned %s", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("an unchecked frame with no reason was accepted")
		}
	}()
	Unchecked(`{`, " ")
}

// fatalRecorder is a recorder whose Fatalf records and stops the call.
type fatalRecorder struct{ recorder }

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	panic("fatal")
}
