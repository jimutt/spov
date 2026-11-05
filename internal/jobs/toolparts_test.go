package jobs

import (
	"testing"
)

// The tool-part reader, against the shape spike S6 captured verbatim.
//
// @decision(DL-050)

// The exact bytes S6 logged, twice, identically. Kept as one string
// rather than reformatted, so what is asserted is what was measured.
const s6CapturedRefusal = `{"type":"tool","name":"shell","executed":false,` +
	`"state":{"status":"error","error":{"type":"aborted",` +
	`"message":"The user declined this tool call"}}}`

// TestToolPartsAgainstTheCapturedShape reads the part out of a message
// envelope shaped as the sandbox sends one.
//
// @decision(DL-050)
func TestToolPartsAgainstTheCapturedShape(t *testing.T) {
	raw := []byte(`{"id":"msg_a","type":"assistant","agent":"build",` +
		`"content":[` +
		`{"type":"reasoning","text":"I should run the tests"},` +
		s6CapturedRefusal + `,` +
		`{"type":"text","text":"The shell command was denied, so I could not run the tests."}` +
		`],"finish":"stop"}`)

	parts := toolParts(raw)
	if len(parts) != 1 {
		t.Fatalf("found %d tool parts, want 1: %+v", len(parts), parts)
	}
	got := parts[0]
	if got.Name != "shell" {
		t.Errorf("name = %q, want shell", got.Name)
	}
	if got.Executed {
		t.Error("executed = true on a refused call: the part cannot then distinguish a refusal from a run")
	}
	if got.Status != "error" {
		t.Errorf("status = %q, want error", got.Status)
	}
	if got.ErrorType != refusalByHuman {
		t.Errorf("error type = %q, want %q", got.ErrorType, refusalByHuman)
	}
	if !got.Refused() {
		t.Error("the captured refusal does not read as refused")
	}
}

// TestNarrationIsNotAToolPart checks the agent's narration of a denial
// is never read as a tool part: it is model prose, the thing being
// judged, not an input to the verdict.
//
// @decision(DL-050)
func TestNarrationIsNotAToolPart(t *testing.T) {
	raw := []byte(`{"id":"msg_a","type":"assistant","content":[` +
		`{"type":"text","text":"The shell command was denied: permission.rejected, executed false."}]}`)
	if parts := toolParts(raw); len(parts) != 0 {
		t.Errorf("prose was read as %d tool parts: %+v", len(parts), parts)
	}
}

// A part that ran is not a refusal, whatever else it carries.
//
// @decision(DL-050)
func TestASuccessfulCallIsNotARefusal(t *testing.T) {
	cases := map[string]toolPart{
		"a completed call":                 {Executed: true, Status: "completed"},
		"a call that ran and then errored": {Executed: true, Status: "error", ErrorType: "timeout"},
		"a call still running":             {Executed: false, Status: "running"},
		"a pending call":                   {Executed: false, Status: ""},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if p.Refused() {
				t.Errorf("%+v read as refused", p)
			}
		})
	}
}

// TestAnUnknownMarkerIsStillARefusal checks an unrecognised error type
// is still read as a refusal, only with a vaguer reason.
//
// @decision(DL-050)
func TestAnUnknownMarkerIsStillARefusal(t *testing.T) {
	p := toolPart{Executed: false, Status: "error", ErrorType: "policy.blocked"}
	if !p.Refused() {
		t.Fatal("a third refusal marker was not read as a refusal; gate 3 would miss it entirely")
	}
	if reason := refusalReason(p); reason == "" {
		t.Error("an unknown marker produced no reason at all")
	}
}

// TestAnUntypedErrorIsNotARefusal checks a part carrying no error type
// is not a refusal: keying on the shape alone would turn every failed
// shell command into one.
//
// @decision(DL-050)
func TestAnUntypedErrorIsNotARefusal(t *testing.T) {
	for name, p := range map[string]toolPart{
		"no error object at all": {Executed: false, Status: "error"},
		"an empty type":          {Executed: false, Status: "error", ErrorType: ""},
		"whitespace":             {Executed: false, Status: "error", ErrorType: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			if p.Refused() {
				t.Errorf("%+v read as a refusal; an ordinary failing command would block the job", p)
			}
		})
	}
}

// Tool parts are found wherever the message puts them: the measured
// shape calls the array `content`, and another spelling is `parts`. A
// reader tied to one key would silently see no tool calls at all.
//
// @decision(DL-050)
func TestToolPartsAreFoundUnderEitherArrayName(t *testing.T) {
	for _, key := range []string{"content", "parts"} {
		t.Run(key, func(t *testing.T) {
			raw := []byte(`{"id":"m","type":"assistant","` + key + `":[` + s6CapturedRefusal + `]}`)
			parts := toolParts(raw)
			if len(parts) != 1 {
				t.Fatalf("found %d parts under %q, want 1", len(parts), key)
			}
			if !parts[0].Refused() {
				t.Error("the part did not read as refused")
			}
		})
	}
}

// Unreadable input yields no parts rather than a panic. A message the
// recorder could not store raw is the ordinary case for an old row.
//
// @decision(DL-050)
func TestUnreadableMessagesYieldNoParts(t *testing.T) {
	for name, raw := range map[string][]byte{
		"nil":              nil,
		"empty":            {},
		"not json":         []byte("<html>"),
		"a bare array":     []byte(`[1,2,3]`),
		"a bare string":    []byte(`"hello"`),
		"null":             []byte(`null`),
		"an empty message": []byte(`{}`),
	} {
		t.Run(name, func(t *testing.T) {
			if parts := toolParts(raw); len(parts) != 0 {
				t.Errorf("got %d parts from %s", len(parts), name)
			}
		})
	}
}

// TestASuccessfulCallAlsoCarriesExecutedFalse checks that a successful
// tool call also carries executed: false, so the error-type check, not
// executed, is what decides a refusal.
//
// @decision(DL-050)
func TestASuccessfulCallAlsoCarriesExecutedFalse(t *testing.T) {
	const raw = `{"content":[
	  {"type":"tool","name":"glob","executed":false,"state":{"status":"completed"}},
	  {"type":"tool","name":"shell","executed":false,"state":{"status":"completed"}},
	  {"type":"tool","name":"read","executed":false,"state":{"status":"error",
	     "error":{"type":"permission.rejected","message":"Permission denied: external_directory"}}}
	]}`

	parts := toolParts([]byte(raw))
	if len(parts) != 3 {
		t.Fatalf("parsed %d parts, want 3", len(parts))
	}
	for _, p := range parts {
		if p.Executed {
			t.Errorf("%s decoded executed=true; the measured shape is false on every part, "+
				"successes included", p.Name)
		}
	}
	// Only the refused one is a refusal, and it is the error TYPE that
	// makes it so — the other two are identical in every other field
	// this function reads.
	if parts[0].Refused() || parts[1].Refused() {
		t.Error("a completed call was read as a refusal: gate 3 would block every job that " +
			"ran a tool successfully")
	}
	if !parts[2].Refused() {
		t.Error("the refused call was not read as a refusal")
	}

	// The load-bearing half, stated as its own assertion: strip the
	// error type from the refusal and it must stop being one. That is
	// what makes an ordinary runtime error different from a refusal,
	// and it is the check a tidy-up would delete as redundant.
	untyped := parts[2]
	untyped.ErrorType = ""
	if untyped.Refused() {
		t.Error("a status-error part with no error type was read as a refusal: every " +
			"ordinary tool failure would block its job")
	}
}

// TestARuntimeToolErrorIsNotARefusal checks a tool that ran and failed
// (tool.execution) is not a refusal: the type is excluded by name,
// keeping DL-050's rule that an unrecognised type is still a refusal.
//
// @decision(DL-050)
func TestARuntimeToolErrorIsNotARefusal(t *testing.T) {
	// The captured shape, from that job's stored message.
	const raw = `{"content":[
	  {"type":"tool","name":"edit","executed":false,"state":{"status":"error",
	    "error":{"type":"tool.execution",
	             "message":"No changes to apply: oldString and newString are identical."}}},
	  {"type":"tool","name":"shell","executed":false,"state":{"status":"error",
	    "error":{"type":"permission.rejected","message":"Permission denied: shell"}}},
	  {"type":"tool","name":"read","executed":false,"state":{"status":"error",
	    "error":{"type":"some.future.marker","message":"denied by something new"}}}
	]}`

	parts := toolParts([]byte(raw))
	if len(parts) != 3 {
		t.Fatalf("parsed %d parts, want 3", len(parts))
	}

	if parts[0].Refused() {
		t.Error("a tool.execution error was read as a refusal: an agent's harmless mistake " +
			"fails a job that went on to do its work, and no other gate catches it")
	}
	if !parts[1].Refused() {
		t.Error("a permission.rejected part is no longer a refusal: gate 3 would miss every " +
			"run the policy blocked")
	}
	// The unrecognised type must STILL be a refusal — the exclusion is
	// one name, not a switch to an allowlist.
	if !parts[2].Refused() {
		t.Error("an unrecognised error type is no longer a refusal: a build that invents a " +
			"third refusal marker would go unnoticed")
	}
}
