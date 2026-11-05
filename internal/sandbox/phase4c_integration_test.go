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
)

// Spike S5 (DL-045, DL-039, DL-046, DL-050, DL-017) is the standing
// proof of four facts:
//
//  1. POST /api/session/:id/synthetic {"text", "resume": true} answers
//     200, arrives as session.inbox.enqueued with item.type "synthetic"
//     and delivery "steer", and runs a turn — so the supervisor can
//     issue a job's opening goal this way.
//  2. A loop of synthetic continuations sustains: three continuations
//     produced four turns, each typed "synthetic".
//  3. A parked permission is answered over HTTP:
//     POST /api/session/:id/permission/:requestID/reply {"reply":
//     "once"} answers 204 and raises a permission.replied frame
//     carrying {sessionID, requestID, reply}. (The other published
//     route, POST /api/session/:id/permission, *creates* a request
//     rather than answering one, and wants {action, resources}.)
//  4. permission.asked and permission.replied carry no durable marker,
// confirming DL-017.
//
// It creates a billable sandbox and terminates what it creates. Ran
// 2026-09-02 on 0.0.0-beta-18684, not the 0.0.0-beta-18866 S1-S4 ran
// on; a finding that later disagrees with a phase-4b one could be a
// build difference rather than a real change.
//
//	go test -tags integration ./internal/sandbox/ -run S5 -v
//
// or against the earlier build:
//
//	SPOV_TEST_OPENCODE_VERSION=0.0.0-beta-18866 go test -tags integration ./internal/sandbox/ -run S5 -v
//
// comment-block: allow (a spike's findings, kept beside the test that proves them)

const (
	// s5GoalMarker is what the opening goal carries, so the session's
	// own messages show whether the endpoint's send was admitted.
	s5GoalMarker = "SPOV_S5_OPENING_GOAL"
	// s5LoopMarker is what each plugin continuation carries.
	s5LoopMarker = "SPOV_S5_CONTINUE"
	// s5AskMarker is the shell command the plugin parks on, chosen so
	// the evaluation can be recognised by what it asks about rather
	// than by arrival order (S3's lesson).
	s5AskMarker = "spov-s5-ask"
	// s5Continuations is how many turns the plugin drives before it
	// stops. Three is the smallest number that distinguishes a loop
	// from S4's single continuation: it proves the second continuation
	// was itself continued from.
	s5Continuations = 3
)

// s5PluginJS drives the loop with synthetic and parks a permission.
//
// It answers questions 2, 3 and 4. Question 1 is the server's side and
// needs no plugin, but the plugin is what observes its arrival.
//
// Import-free apart from node:fs, for S1's reason: the published
// @opencode-ai/plugin exports a narrower V2 context than the in-repo
// one, so a plugin spov ships as a file must not need it.
var s5PluginJS = spikePreambleJS + `
export default {
  id: "spov.spike",
  async setup(ctx) {
    say("setup", {})

    // Every permission and execution frame, raw enough to answer
    // question 4 from the trace: whether the frame carries a durable
    // object (the marker DL-017 keys on, and which S3 never checked).
    // "durability" is read too, because the phase-3 tail was silenced
    // once by filtering on that string when the field is an object.
    try {
      ;(async () => {
        for await (const ev of ctx.event.subscribe()) {
          const type = ev?.type ?? ev?.event
          if (typeof type !== "string") continue
          if (type.startsWith("permission")) {
            const d = ev?.data ?? ev?.properties
            say("permission.frame", {
              type,
              durable: ev?.durable ?? d?.durable ?? null,
              durableType: typeof (ev?.durable ?? d?.durable),
              durability: ev?.durability ?? d?.durability ?? null,
              seq: ev?.durable?.seq ?? d?.durable?.seq ?? null,
              topKeys: Object.keys(ev ?? {}).sort(),
              sample: peek(ev, 700),
            })
          }
          if (type.startsWith("session.execution")) {
            say("execution", { type, sessionID: d0(ev) })
          }
          if (type === "session.inbox.enqueued") {
            const d = ev?.data ?? ev?.properties
            say("inbox", {
              itemType: d?.item?.type ?? null,
              sample: peek(d, 300),
            })
          }
        }
      })().catch((e) => say("event.loop.failed", { error: String(e) }))
    } catch (e) {
      say("event.failed", { error: String(e) })
    }

    // Question 3: park on a permission so the test can answer it and
    // watch what the answer looks like. Everything else is allowed —
    // the run has work to do and only this one command should park.
    try {
      ctx.permission.hook("evaluate", (evaluation) => {
        const asked = peek(evaluation?.resources, 400) + " " + String(evaluation?.action)
        if (asked.includes("` + s5AskMarker + `")) {
          try { evaluation.effect = "ask" } catch (e) {}
          say("permission.parked", { sample: peek(evaluation, 400) })
        }
      })
      say("permission.hooked", {})
    } catch (e) {
      say("permission.failed", { error: String(e) })
    }

    // Question 2: a loop of several synthetic continuations. Bounded,
    // because nothing else stops it and the model's budget is real.
    //
    // Reserve-then-send, so a refused send does not consume one of the
    // three and read as a loop that stopped on its own — the shape
    // DL-049 asks the plugin's iteration budget to have.
    let sent = 0
    try {
      ;(async () => {
        for await (const ev of ctx.event.subscribe()) {
          const type = ev?.type ?? ev?.event
          if (type !== "session.execution.succeeded") continue
          const sessionID = d0(ev)
          if (!sessionID) continue
          if (sent >= ` + fmt.Sprint(s5Continuations) + `) continue
          const n = sent + 1
          sent = n
          try {
            await ctx.session.synthetic({
              sessionID,
              text: "` + s5LoopMarker + ` " + n + ": reply with the single word: " + n + ".",
              resume: true,
            })
            say("continue.sent", { n, sessionID })
          } catch (e) {
            sent = n - 1
            say("continue.failed", { n, error: String(e) })
          }
        }
      })().catch((e) => say("continue.loop.failed", { error: String(e) }))
    } catch (e) {
      say("continue.failed.outer", { error: String(e) })
    }
  },
}

function d0(ev) {
  return ev?.data?.sessionID ?? ev?.properties?.sessionID ?? null
}
`

// TestIntegrationS5SyntheticLoopAndPermissionAnswer is spike S5.
//
// Failures are reported rather than fatal wherever the run can
// continue, so one billable run yields the whole picture instead of the
// first broken claim — the habit S1-S4 kept and the reason they were
// worth keeping.
func TestIntegrationS5SyntheticLoopAndPermissionAnswer(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-s5", 18*time.Minute,
		map[string]string{spikePluginPath: s5PluginJS})
	client := &http.Client{Timeout: 0}
	base := creds.URL
	if !waitForPlugin(ctx, t, client, base, creds.Token, password) {
		t.Fatal("the plugin never loaded; S1 covers that claim, so this run measures nothing")
	}
	session := createSession(ctx, t, client, base, creds.Token, password)

	// Question 1, and it runs first because everything after it needs a
	// turn to have happened: if the endpoint can open the session, the
	// loop has something to continue from.
	openedBySynthetic := t.Run("a synthetic over HTTP schedules execution", func(t *testing.T) {
		body, err := json.Marshal(map[string]any{
			"text":   s5GoalMarker + ": reply with the single word: one.",
			"resume": true,
		})
		if err != nil {
			t.Fatalf("goal body: %v", err)
		}
		code, raw := post(ctx, t, client, base+"/api/session/"+session+"/synthetic",
			creds.Token, password, string(body))
		if code == http.StatusNotFound {
			t.Fatalf("POST /api/session/:id/synthetic does not exist (404: %s): DL-046 has the supervisor open a job this way, so the opening goal needs another mechanism", truncate(raw, 200))
		}
		if code < 200 || code >= 300 {
			t.Fatalf("POST /api/session/:id/synthetic -> %d (%s): the endpoint refused the send", code, truncate(raw, 200))
		}
		entries := waitForTraceFunc(ctx, t, sb, 4*time.Minute, func(es []map[string]any) bool {
			return countSucceeded(es) > 0
		})
		for _, e := range ofKind(entries, "inbox") {
			t.Logf("inbox item: %v", e)
		}
		// The type matters as much as the scheduling: DL-045's whole
		// steered invariant is that nothing of spov's is a "user" item,
		// and the opening goal is spov's.
		var opening map[string]any
		for _, e := range ofKind(entries, "inbox") {
			if strings.Contains(fmt.Sprint(e["sample"]), s5GoalMarker) {
				opening = e
			}
		}
		switch {
		case opening == nil:
			t.Error("the opening goal raised no session.inbox.enqueued: it cannot be told apart from a human's prompt because it cannot be seen at all")
		case opening["itemType"] != "synthetic":
			t.Errorf(`the opening goal arrived as item.type %v, not "synthetic": issuing it over HTTP does not uphold DL-045's invariant and every job would enter steered on its own first turn`, opening["itemType"])
		default:
			t.Logf(`confirmed: the opening goal arrives as item.type "synthetic"`)
		}
		if countSucceeded(entries) == 0 {
			t.Error("no turn finished within 4m of the synthetic: resume over HTTP does not schedule execution, so the supervisor cannot open a job with it")
		} else {
			t.Logf("confirmed: a synthetic over HTTP started and finished a turn")
		}
	})

	if !openedBySynthetic {
		// The loop needs a first turn to continue from. Fall back to a
		// prompt so questions 2-4 are still measured on this sandbox
		// rather than lost with question 1.
		t.Log("opening with a prompt instead, so the rest of the run still measures something")
		prompt(ctx, t, client, base, creds.Token, password, session, "SPOV_S5_FALLBACK: reply with the single word: one.")
	}

	t.Run("a loop of synthetic continuations sustains", func(t *testing.T) {
		entries := waitForTraceFunc(ctx, t, sb, 8*time.Minute, func(es []map[string]any) bool {
			return kindCount(es, "continue.sent") >= s5Continuations &&
				countSucceeded(es) >= s5Continuations+1
		})
		for _, e := range ofKind(entries, "continue.sent") {
			t.Logf("%v", e)
		}
		for _, e := range ofKind(entries, "continue.failed") {
			t.Logf("%v", e)
		}
		sent := kindCount(entries, "continue.sent")
		if sent < s5Continuations {
			t.Errorf("the plugin sent %d of %d synthetic continuations: synthetic drives one turn (S4) but does not carry a loop, and DL-039's continuation mechanism needs rewriting", sent, s5Continuations)
		}
		// The turn count is what separates a loop from a single
		// continuation: the opening turn plus one per continuation.
		if got, want := countSucceeded(entries), s5Continuations+1; got < want {
			t.Errorf("%d turns completed, want %d: a continuation started a turn but the loop did not sustain past it", got, want)
		} else {
			t.Logf("confirmed: %d turns, %d of them started by the plugin's own synthetic continuations", got, sent)
		}
		// Every continuation must stay typed apart, not just the first:
		// one arriving as "user" would strand its job in steered.
		for _, e := range ofKind(entries, "inbox") {
			if !strings.Contains(fmt.Sprint(e["sample"]), s5LoopMarker) {
				continue
			}
			if e["itemType"] != "synthetic" {
				t.Errorf(`a continuation arrived as item.type %v, not "synthetic": %v`, e["itemType"], e)
			}
		}
	})

	// Questions 3 and 4 share a parked permission, so they share a
	// subtest: parking twice would cost another turn for nothing.
	t.Run("a permission parks, and what its answer looks like", func(t *testing.T) {
		prompt(ctx, t, client, base, creds.Token, password, session,
			"Run this exact shell command and tell me what it printed: echo "+s5AskMarker)

		entries := waitForTraceFunc(ctx, t, sb, 5*time.Minute, func(es []map[string]any) bool {
			for _, e := range ofKind(es, "permission.frame") {
				if fmt.Sprint(e["type"]) == "permission.asked" {
					return true
				}
			}
			return false
		})
		asked := ofKind(entries, "permission.frame")
		for _, e := range asked {
			t.Logf("permission frame: %v", e)
		}
		var ask map[string]any
		for _, e := range asked {
			if fmt.Sprint(e["type"]) == "permission.asked" {
				ask = e
			}
		}
		if ask == nil {
			dumpStdout(ctx, t, sb)
			t.Fatal(`no permission.asked arrived: the hook's "ask" effect did not park the session, so DL-046 has no signal for blocked at all`)
		}

		// Question 4, answered off the frame already in hand. DL-017
		// asserts these are ephemeral; DL-017's schema amendment is
		// sized on that being true.
		if ask["durable"] == nil {
			t.Logf(`confirmed: permission.asked carries no durable marker (durability=%v), so DL-017 is right and DL-017's amendment is the full schema change it claims`, ask["durability"])
		} else {
			t.Errorf("permission.asked DOES carry a durable marker (%v, type %v, seq %v): DL-017 asserts the opposite and DL-017's schema amendment is sized on it — the recorder may already keep these and the change shrinks to a filter", ask["durable"], ask["durableType"], ask["seq"])
		}

		// Question 3. The id is what an answer is addressed to; S3 saw
		// it as a per_… on the frame.
		id := permissionID(ask)
		if id == "" {
			t.Fatalf("no permission id on the frame, so there is nothing to answer: %v", ask["sample"])
		}
		t.Logf("permission id: %s", id)

		// The endpoint's shape is itself unverified, and a wrong guess
		// looks exactly like a missing mechanism. So enumerate the
		// server's own routes first and try what it actually publishes:
		// a 404 is only evidence once it is a 404 at the right URL.
		routes, bodies := permissionRoutes(ctx, t, client, base, creds.Token, password)
		if len(routes) == 0 {
			t.Log("the server's route list named no permission path; falling back to the shapes the research assumes")
			routes = []string{
				"/api/session/" + session + "/permission/" + id,
				"/api/session/" + session + "/permission",
				"/api/permission/" + id,
			}
		}
		// A published path is templated ("/api/session/{id}/permission/
		// {permissionID}"); fill the placeholders in rather than
		// POSTing to a literal brace.
		for i, r := range routes {
			r = strings.NewReplacer(
				"{id}", session,
				"{sessionID}", session,
				"{requestID}", id,
				"{permissionID}", id,
				"{permissionId}", id,
			).Replace(r)
			routes[i] = r
			t.Logf("candidate permission route: %s", r)
		}

		before := len(ofKind(entries, "permission.frame"))
		answered := false
		for _, route := range routes {
			// The bodies the document itself describes lead; the
			// guesses are the fallback for a route with no usable
			// schema. The first run's 400 named "action" as the
			// missing key, which is why that shape heads the guesses.
			// The document names the key on the /reply route as
			// "reply", referencing Permission.Reply; its values are the
			// fallbacks below. The schema-derived candidates lead
			// anyway, so a build that renames them still works.
			candidates := bodies[route]
			candidates = append(candidates,
				`{"decision":"once"}`,
				`{"reply":"once"}`,
				`{"reply":"always"}`,
				`{"reply":"reject"}`,
				`{"reply":"allow"}`,
				`{"action":"once"}`)
			for _, body := range candidates {
				code, raw := post(ctx, t, client, base+route, creds.Token, password, body)
				if code == http.StatusNotFound {
					// Wrong URL: the other bodies will 404 too.
					break
				}
				t.Logf("answering at %s with %s -> %d %s", route, body, code, truncate(raw, 200))
				if code >= 200 && code < 300 {
					t.Logf("confirmed: a parked permission is answered by POST %s with %s", route, body)
					answered = true
					break
				}
			}
			if answered {
				break
			}
		}
		if !answered {
			t.Errorf("no published permission route accepted an answer: answering a parked permission has no measured mechanism over HTTP, so DL-046's rule that a job leaves blocked only by an explicit human act stands by necessity rather than by choice")
		}

		after := waitForTraceFunc(ctx, t, sb, 4*time.Minute, func(es []map[string]any) bool {
			return len(ofKind(es, "permission.frame")) > before || countSucceeded(es) > countSucceeded(entries)
		})
		var answers []map[string]any
		for _, e := range ofKind(after, "permission.frame") {
			if fmt.Sprint(e["type"]) != "permission.asked" {
				answers = append(answers, e)
			}
		}
		for _, e := range answers {
			t.Logf("post-answer permission frame: %v", e)
		}
		if len(answers) == 0 {
			t.Errorf("no permission frame other than permission.asked arrived within 4m of answering: an answer produces no event the supervisor could read, so DL-046's blocked state has no measured exit and a job must be released by hand")
		} else {
			t.Logf("confirmed: answering produces %d frame(s); the first is %v, which is what the supervisor would key a blocked exit on", len(answers), answers[0]["type"])
		}
		if countSucceeded(after) > countSucceeded(entries) {
			t.Logf("the turn also resumed after the answer (%d -> %d succeeded), so an answered permission continues the run", countSucceeded(entries), countSucceeded(after))
		}
	})
}

// permissionRoutes asks the server which permission paths it publishes.
// The endpoint that answers a parked permission is not documented
// anywhere spov has measured, and guessing at it makes a 404 mean two
// different things — wrong URL, or no such mechanism. Reading the
// server's own route list separates them.
func permissionRoutes(ctx context.Context, t *testing.T, c *http.Client, base, token, password string) ([]string, map[string][]string) {
	t.Helper()
	var out []string
	bodies := map[string][]string{}
	for _, doc := range []string{"/doc", "/openapi.json", "/api/doc", "/docs/json"} {
		raw := get(ctx, t, c, base+doc, token, password)
		if raw == "" || !strings.Contains(raw, "permission") {
			continue
		}
		t.Logf("route document at %s (%d bytes)", doc, len(raw))
		var spec struct {
			Paths      map[string]map[string]json.RawMessage `json:"paths"`
			Components struct {
				Schemas map[string]json.RawMessage `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			t.Logf("%s is not an OpenAPI document: %v", doc, err)
			continue
		}
		for path, methods := range spec.Paths {
			post, ok := methods["post"]
			if !ok || !strings.Contains(path, "permission") {
				continue
			}
			out = append(out, path)
			// The request schema is the answer to "what body does this
			// want", and reading it beats guessing: two sandboxes went
			// on learning one key name at a time from 400s.
			t.Logf("POST %s schema: %s", path, truncate(string(post), 1200))
			bodies[path] = append(bodies[path], bodiesFromSchema(post, spec.Components.Schemas)...)
		}
		if len(out) > 0 {
			for p, b := range bodies {
				t.Logf("bodies the document describes for %s: %v", p, b)
			}
			return out, bodies
		}
	}
	return out, bodies
}

// bodiesFromSchema builds candidate request bodies from an operation's
// declared enums: an OpenAPI enum on a property is exactly the set of
// values the endpoint accepts, so each becomes one body to try.
//
// A property is often a $ref rather than an inline enum — Permission.Reply
// is — so refs are resolved one level against components/schemas. Not
// resolving them is what made the second run guess wrongly at a body the
// document had already described.
func bodiesFromSchema(op json.RawMessage, schemas map[string]json.RawMessage) []string {
	var o struct {
		RequestBody struct {
			Content map[string]struct {
				Schema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
	}
	if err := json.Unmarshal(op, &o); err != nil {
		return nil
	}
	var out []string
	for _, c := range o.RequestBody.Content {
		for name, prop := range c.Schema.Properties {
			for _, v := range enumValues(prop, schemas) {
				out = append(out, fmt.Sprintf("{%q:%q}", name, v))
			}
		}
	}
	return out
}

// enumValues reads a property's enum, following one $ref into
// components/schemas. A union of refs (anyOf/oneOf) contributes every
// branch's values.
func enumValues(prop json.RawMessage, schemas map[string]json.RawMessage) []string {
	var p struct {
		Ref   string            `json:"$ref"`
		Enum  []string          `json:"enum"`
		Const string            `json:"const"`
		AnyOf []json.RawMessage `json:"anyOf"`
		OneOf []json.RawMessage `json:"oneOf"`
	}
	if err := json.Unmarshal(prop, &p); err != nil {
		return nil
	}
	var out []string
	out = append(out, p.Enum...)
	if p.Const != "" {
		out = append(out, p.Const)
	}
	for _, branch := range append(p.AnyOf, p.OneOf...) {
		out = append(out, enumValues(branch, schemas)...)
	}
	if p.Ref != "" {
		name := p.Ref[strings.LastIndex(p.Ref, "/")+1:]
		if target, ok := schemas[name]; ok {
			// One level only: a self-referential document would
			// otherwise recur forever, and nothing here needs two.
			out = append(out, enumValues(target, nil)...)
		}
	}
	return out
}

// permissionID digs the permission's own id out of the frame. S3 saw it
// as a per_… value; the exact path is not settled, so this reads the
// sample rather than assuming a shape — the frame is logged in full
// beside it either way.
func permissionID(frame map[string]any) string {
	sample := fmt.Sprint(frame["sample"])
	i := strings.Index(sample, "per_")
	if i < 0 {
		return ""
	}
	rest := sample[i:]
	end := strings.IndexFunc(rest, func(r rune) bool {
		return r != '_' &&
			(r < '0' || r > '9') &&
			(r < 'a' || r > 'z') &&
			(r < 'A' || r > 'Z')
	})
	if end < 0 {
		return rest
	}
	return rest[:end]
}
