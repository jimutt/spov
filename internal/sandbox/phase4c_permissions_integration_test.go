//go:build integration

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// Spike S6 (DL-046, DL-050) is the standing proof of three facts:
//
//  1. permission.replied carries {sessionID, requestID, reply} and no
//     principal, so a human's answer and spov's own are
//     indistinguishable on the stream.
//  2. An answer un-parks the run: after a 204 reply the turn resumed
//     and finished.
//  3. Permission.Reply is ["once", "always", "reject"], read from the
//     server's own /openapi.json. A rejection through the reply
//     endpoint leaves a tool part with state.error.type "aborted",
//     distinct from the "permission.rejected" a policy deny leaves
//     through the hook.
//
// It creates a billable sandbox and terminates what it creates.
//
//	go test -tags integration ./internal/sandbox/ -run S6 -v

const (
	// s6AcceptMarker and s6RejectMarker are the shell commands the
	// plugin parks on. Two, so one run measures both an accepted and a
	// rejected reply — and recognised by what they ask about rather
	// than by arrival order, which is S3's lesson.
	s6AcceptMarker = "spov-s6-accept"
	s6RejectMarker = "spov-s6-reject"
)

// s6PluginJS parks on two named commands and records everything about
// the permissions and the tool parts around them.
//
// The captures are deliberately wide: S5's 700-byte peek is what left
// question 1 open, since a principal beyond that would have been
// truncated away with nothing to show it had been.
var s6PluginJS = spikePreambleJS + `
export default {
  id: "spov.spike",
  async setup(ctx) {
    say("setup", {})

    try {
      ;(async () => {
        for await (const ev of ctx.event.subscribe()) {
          const type = ev?.type ?? ev?.event
          if (typeof type !== "string") continue
          if (type.startsWith("permission")) {
            // Whole, not peeked: question 1 is precisely whether a
            // field nobody has looked at is in here.
            let whole = ""
            try { whole = JSON.stringify(ev) } catch (e) { whole = String(e) }
            say("permission.frame", {
              type,
              topKeys: Object.keys(ev ?? {}).sort(),
              dataKeys: Object.keys(ev?.data ?? ev?.properties ?? {}).sort(),
              durable: ev?.durable ?? null,
              whole,
            })
          }
          if (type.startsWith("session.execution")) {
            say("execution", {
              type,
              sessionID: ev?.data?.sessionID ?? ev?.properties?.sessionID ?? null,
            })
          }
        }
      })().catch((e) => say("event.loop.failed", { error: String(e) }))
    } catch (e) {
      say("event.failed", { error: String(e) })
    }

    // Both markers park. Everything else is allowed: the run has work
    // to do and only these two should stop it.
    try {
      ctx.permission.hook("evaluate", (evaluation) => {
        const asked = peek(evaluation?.resources, 400) + " " + String(evaluation?.action)
        if (asked.includes("` + s6AcceptMarker + `") || asked.includes("` + s6RejectMarker + `")) {
          try { evaluation.effect = "ask" } catch (e) {}
          say("permission.parked", { asked: peek(evaluation?.resources, 200) })
        }
      })
      say("permission.hooked", {})
    } catch (e) {
      say("permission.failed", { error: String(e) })
    }
  },
}
`

// TestIntegrationS6PermissionRepliesAndTheirEffects is spike S6.
//
// Failures are reported rather than fatal wherever the run can
// continue, so one billable run yields the whole picture instead of the
// first broken claim.
func TestIntegrationS6PermissionRepliesAndTheirEffects(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-s6", 18*time.Minute,
		map[string]string{spikePluginPath: s6PluginJS})
	client := &http.Client{Timeout: 0}
	base := creds.URL
	if !waitForPlugin(ctx, t, client, base, creds.Token, password) {
		t.Fatal("the plugin never loaded; S1 covers that claim, so this run measures nothing")
	}
	session := createSession(ctx, t, client, base, creds.Token, password)

	// Question 3's first half is a read, not an experiment. Done first
	// because what it says narrows what the replies below need to try.
	replies := t.Run("the reply vocabulary is what the document says it is", func(t *testing.T) {
		values := schemaEnum(ctx, t, client, base, creds.Token, password, "Permission.Reply")
		if len(values) == 0 {
			t.Error("Permission.Reply named no values: the reply vocabulary stays unmeasured and the arms below fall back to guesses")
			return
		}
		t.Logf("confirmed: Permission.Reply accepts %v", values)
		// The two the job model needs: something that lets the call
		// through, and something that refuses it.
		var accepts, rejects bool
		for _, v := range values {
			switch v {
			case "once", "always", "allow":
				accepts = true
			case "reject", "deny", "never":
				rejects = true
			}
		}
		if !accepts || !rejects {
			t.Errorf("the vocabulary %v has no clear accept/reject pair: DL-050's verdict needs a refusal that is distinguishable from an approval", values)
		}
	})
	if !replies {
		t.Log("continuing with the values S5 measured working, so the rest of the run still measures something")
	}

	t.Run("an accepted reply names no actor, and resumes the run", func(t *testing.T) {
		before := countSucceeded(readTrace(ctx, t, sb))
		id := parkOn(ctx, t, client, sb, base, creds.Token, password, session, s6AcceptMarker)
		if id == "" {
			t.Fatal("nothing parked, so there is no permission to answer")
		}

		code, raw := post(ctx, t, client,
			base+"/api/session/"+session+"/permission/"+id+"/reply",
			creds.Token, password, `{"decision":"once"}`)
		if code < 200 || code >= 300 {
			t.Fatalf("answering -> %d %s", code, truncate(raw, 200))
		}

		entries := waitForTraceFunc(ctx, t, sb, 5*time.Minute, func(es []map[string]any) bool {
			return repliedFor(es, id) != nil && countSucceeded(es) > before
		})

		// Question 1, off the whole frame rather than a peek at it.
		replied := repliedFor(entries, id)
		if replied == nil {
			t.Fatal("no permission.replied arrived for the answer")
		}
		t.Logf("permission.replied, whole: %v", replied["whole"])
		t.Logf("permission.replied data keys: %v", replied["dataKeys"])
		if actor := namesAnActor(replied); actor != "" {
			t.Logf("the frame DOES name an actor (%s): the supervisor can tell a human's answer from its own, and DL-046's blocked exit can be automatic", actor)
		} else {
			t.Log(`confirmed: permission.replied names no actor. A human's answer and spov's own are identical on the stream, so "a human decided" cannot be inferred from it — DL-046's rule that blocked is left only by an explicit act is permanent, not provisional`)
		}

		// Question 2. The whole point of parking rather than failing.
		if got := countSucceeded(entries); got > before {
			t.Logf("confirmed: the run resumed after the answer (%d -> %d turns finished), so blocked has a real exit", before, got)
		} else {
			t.Errorf("no turn finished within 5m of answering (%d, unchanged): an answered permission does not resume the run, so blocked cannot be released and the supervisor must terminate and retry — which makes DL-049's \"a bound parks, it does not fail\" a much weaker story than it reads as", before)
		}
	})

	t.Run("a rejected reply is distinguishable, and marks the tool part", func(t *testing.T) {
		id := parkOn(ctx, t, client, sb, base, creds.Token, password, session, s6RejectMarker)
		if id == "" {
			t.Fatal("nothing parked, so there is no permission to reject")
		}
		before := countSucceeded(readTrace(ctx, t, sb))

		// The document's own refusal value, falling back to the two
		// spellings a refusal usually takes.
		values := schemaEnum(ctx, t, client, base, creds.Token, password, "Permission.Reply")
		refusal := ""
		for _, v := range values {
			if v == "reject" || v == "deny" || v == "never" {
				refusal = v
				break
			}
		}
		answered := false
		for _, v := range []string{refusal, "reject", "deny"} {
			if v == "" {
				continue
			}
			code, raw := post(ctx, t, client,
				base+"/api/session/"+session+"/permission/"+id+"/reply",
				creds.Token, password, fmt.Sprintf(`{"decision":%q}`, v))
			t.Logf("rejecting with %q -> %d %s", v, code, truncate(raw, 200))
			if code >= 200 && code < 300 {
				t.Logf("confirmed: a permission is refused with reply %q", v)
				answered = true
				break
			}
		}
		if !answered {
			t.Fatal("no refusal value was accepted: a permission can be approved but not refused over HTTP, which the verdict needs")
		}

		entries := waitForTraceFunc(ctx, t, sb, 5*time.Minute, func(es []map[string]any) bool {
			return repliedFor(es, id) != nil && countSucceeded(es) > before
		})
		if replied := repliedFor(entries, id); replied != nil {
			t.Logf("the refusal's frame, whole: %v", replied["whole"])
		} else {
			t.Error("a refused permission raised no permission.replied: the supervisor cannot see that the question was settled")
		}

		// The half that matters to DL-050. S3 saw permission.rejected
		// in a tool part from a `deny` in the *hook*; this is the reply
		// path, and if the two differ then gate 3 must read both.
		//
		// Read after the turn ends, not straight after the reply: the
		// part is written as the turn streams, and reading early
		// measures the test's timing rather than the sandbox's
		// behaviour. The recorder has the same constraint — DL-017
		// reads /message only when a turn ends.
		if countSucceeded(entries) <= before {
			t.Logf("the turn had not finished when the trace was last read; waiting for it before reading the messages")
			entries = waitForTraceFunc(ctx, t, sb, 4*time.Minute, func(es []map[string]any) bool {
				return countSucceeded(es) > before
			})
		}
		if countSucceeded(entries) <= before {
			// Not fatal: the part below carries its own completion
			// time, so whether it is written is visible in the part
			// rather than inferred from the turn count.
			t.Log("no further turn was traced after the refusal; the tool part's own completed time is what says whether it was written")
		}
		body := get(ctx, t, client, base+"/api/session/"+session+"/message", creds.Token, password)
		// The body carries encrypted reasoning blobs kilobytes long, so
		// the tool parts are pulled out rather than dumped: what gate 3
		// reads is the part's own state, and that is what to show.
		for _, part := range toolParts(t, body) {
			t.Logf("tool part: %s", truncate(part, 900))
		}
		// What gate 3 must key on: a field, not a substring.
		refused := refusedPart(t, body)
		if refused == nil {
			t.Fatal("no tool part records the refusal at all: DL-050's gate 3 cannot see a refusal that came through the reply path")
		}
		t.Logf("the refused call: executed=%v status=%q error.type=%q error.message=%q",
			refused.Executed, refused.State.Status, refused.State.Error.Type, refused.State.Error.Message)
		if refused.Executed {
			t.Error("the refused call reports executed:true, so the part cannot distinguish a refusal from a run")
		}
		if refused.State.Status != "error" {
			t.Errorf("state.status = %q, want \"error\"", refused.State.Status)
		}
		// The reply path and the hook's own deny mark a refusal differently.
		switch refused.State.Error.Type {
		case "aborted":
			t.Logf(`confirmed: the reply path marks a refusal state.error.type %q (%q), NOT "permission.rejected" — that is the hook's marker. DL-050's gate 3 must read both, or it misses every refusal a human made through the reply endpoint`,
				refused.State.Error.Type, refused.State.Error.Message)
		case "permission.rejected":
			t.Log(`the reply path now marks a refusal "permission.rejected", the same as the hook: the two markers have converged and gate 3 can read one shape after all`)
		default:
			t.Errorf(`a refusal through the reply path marks state.error.type %q, which is neither the "aborted" this spike measured nor the "permission.rejected" S3 measured from the hook. Gate 3 keys on a set of markers, and this is a third one`,
				refused.State.Error.Type)
		}
	})
}

// parkOn makes the agent run a marked command, waits for the permission
// it parks on, and returns that permission's id.
func parkOn(ctx context.Context, t *testing.T, c *http.Client, sb *modal.Sandbox, base, token, password, session, marker string) string {
	t.Helper()
	seen := map[string]bool{}
	for _, e := range askedFrames(readTrace(ctx, t, sb)) {
		seen[permissionIDOf(e)] = true
	}
	prompt(ctx, t, c, base, token, password, session,
		"Run this exact shell command and tell me what it printed: echo "+marker)
	entries := waitForTraceFunc(ctx, t, sb, 5*time.Minute, func(es []map[string]any) bool {
		for _, e := range askedFrames(es) {
			if id := permissionIDOf(e); id != "" && !seen[id] {
				return true
			}
		}
		return false
	})
	for _, e := range askedFrames(entries) {
		if id := permissionIDOf(e); id != "" && !seen[id] {
			t.Logf("parked on %s for %s", id, marker)
			return id
		}
	}
	dumpStdout(ctx, t, sb)
	return ""
}

// askedFrames keeps the permission.asked entries of a trace.
func askedFrames(entries []map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range ofKind(entries, "permission.frame") {
		if fmt.Sprint(e["type"]) == "permission.asked" {
			out = append(out, e)
		}
	}
	return out
}

// repliedFor finds the permission.replied frame answering one request.
func repliedFor(entries []map[string]any, requestID string) map[string]any {
	for _, e := range ofKind(entries, "permission.frame") {
		if fmt.Sprint(e["type"]) != "permission.replied" {
			continue
		}
		if strings.Contains(fmt.Sprint(e["whole"]), requestID) {
			return e
		}
	}
	return nil
}

// permissionIDOf digs a permission's own id out of a captured frame.
func permissionIDOf(frame map[string]any) string {
	return permissionID(map[string]any{"sample": frame["whole"]})
}

// actorKeys are the names a field identifying who answered would
// plausibly take. Deliberately generous, since a false negative would
// record "no actor" for a field simply spelled differently.
var actorKeys = []string{
	"principal", "actor", "user", "userID", "userId", "by", "author",
	"source", "origin", "client", "identity", "account", "answeredBy",
	"repliedBy", "agent",
}

// namesAnActor reports which actor-ish key the frame carries, if any.
func namesAnActor(frame map[string]any) string {
	whole := fmt.Sprint(frame["whole"])
	var decoded map[string]any
	if err := json.Unmarshal([]byte(whole), &decoded); err != nil {
		return ""
	}
	data, _ := decoded["data"].(map[string]any)
	if data == nil {
		return ""
	}
	for _, k := range actorKeys {
		if v, ok := data[k]; ok && v != nil && fmt.Sprint(v) != "" {
			return fmt.Sprintf("%s=%v", k, v)
		}
	}
	return ""
}

// schemaEnum resolves one named schema's enum from the server's own
// OpenAPI document. S5 spent three sandboxes learning that guessing an
// endpoint's shape makes every error ambiguous; the document answers
// directly, and a $ref is one lookup away rather than a mystery.
func schemaEnum(ctx context.Context, t *testing.T, c *http.Client, base, token, password, name string) []string {
	t.Helper()
	raw := get(ctx, t, c, base+"/openapi.json", token, password)
	if raw == "" {
		t.Log("no /openapi.json to read")
		return nil
	}
	var spec struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Logf("the route document is not JSON: %v", err)
		return nil
	}
	target, ok := spec.Components.Schemas[name]
	if !ok {
		t.Logf("%s is not in components/schemas", name)
		return nil
	}
	t.Logf("%s = %s", name, truncate(string(target), 800))
	return enumValues(target, spec.Components.Schemas)
}

// toolParts pulls the tool-call parts out of a /message body. The
// messages carry encrypted reasoning blobs kilobytes long, so a raw
// dump buries what gate 3 actually reads.
func toolParts(t *testing.T, body string) []string {
	t.Helper()
	var envelope struct {
		Data []struct {
			Content []json.RawMessage `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Logf("the message body is not the shape this expects: %v", err)
		return nil
	}
	var out []string
	for _, m := range envelope.Data {
		for _, part := range m.Content {
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(part, &head); err != nil {
				continue
			}
			if head.Type == "reasoning" || head.Type == "text" {
				continue
			}
			out = append(out, string(part))
		}
	}
	return out
}

// toolPart is what gate 3 reads out of a message: whether the call ran,
// and what stopped it if it did not.
type toolPart struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Executed bool   `json:"executed"`
	State    struct {
		Status string `json:"status"`
		Error  struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"state"`
}

// refusedPart finds the tool part of a call that did not run. Read as
// fields rather than by substring: "the body mentions rejected
// somewhere" is not the same claim as "the part says the call was
// refused", and only the second is something a verdict can be built on.
func refusedPart(t *testing.T, body string) *toolPart {
	t.Helper()
	for _, raw := range toolParts(t, body) {
		var p toolPart
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			continue
		}
		if p.Type != "tool" || p.Executed {
			continue
		}
		if p.State.Status == "error" {
			return &p
		}
	}
	return nil
}
