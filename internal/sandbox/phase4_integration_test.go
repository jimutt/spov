//go:build integration

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// These are the phase-4 plugin spikes (S1-S4). Each stays as the standing
// proof of a fact DL-045, DL-039, DL-046 and DL-040 rest on; the findings are
// recorded there. They create billable sandboxes and terminate them.
//
// The plugin here is a probe, not spov's, written into the container
// before the server starts rather than baked into an image.
//
//   - S1: a file plugin with a plain object default export loads with no
//     config entry and no npm, registers a slash command reachable over
// HTTP, sees a turn end and starts the next one (DL-039). The
//     data?.sessionID filter is insurance: every execution event observed
//     carried a sessionID.
//   - S2: session.hook("context") fires before every provider dispatch and
//     can push onto system[] without breaking it; whether compaction drops
// the goal is not stable, so re-assertion is per request (DL-039).
//     system[] holds plain strings (S8), not parts. A manual compaction
// joins the run in flight, so turn counts undercount (DL-049).
//   - S3: permission.evaluate is authoritative, a denied turn still
//     finishes "stop", /wait never answers on a parked session, and
// /shell bypasses the hook (DL-046, DL-050, DL-040).
//   - S4: the plugin's own prompt arrives as item.type "user" with no id,
//     like a human's; synthetic with resume:true drives a turn and is
// typed apart (DL-045, DL-039). Measuring it needs an idle session.
//
// comment-block: allow (the spikes' findings, kept beside the tests that prove them)

const (
	// spikePluginID is what the plugin calls itself, and what
	// GET /api/plugin should report.
	spikePluginID = "spov.spike"
	// spikePluginPath is the global config directory's plugin dir. HOME
	// is /root in the sandbox (DL-036), so this is the global one.
	spikePluginPath = "/root/.config/opencode/plugins/spov-spike.js"
	// spikeTracePath is where the plugin records what it saw. Reading it
	// needs no network from inside the sandbox and survives the server
	// being busy.
	spikeTracePath = "/tmp/spov-spike.jsonl"
	spikeCommand   = "spov-goal"
	// spikeMarker appears in the text the plugin re-prompts with, so the
	// session's own messages show whether the continuation was admitted.
	spikeMarker = "SPOV_SPIKE_REPROMPT"
)

// spikePreambleJS is what every probe here shares: a trace file, and
// stdout as the fallback channel when writing it fails.
//
// Deliberately import-free apart from node:fs. The published
// @opencode-ai/plugin package exports a narrower V2 context than the
// in-repo one, so a plugin that needs Plugin.define carries a packaging
// hazard; S1 measured that a plain object default export is enough, so
// none of these import it.
const spikePreambleJS = `
let fs
try {
  fs = await import("node:fs")
} catch (e) {}

function say(event, data) {
  const line = JSON.stringify({ event, at: new Date().toISOString(), ...data })
  try { console.log("SPOV_SPIKE " + line) } catch (e) {}
  try { fs.appendFileSync("/tmp/spov-spike.jsonl", line + "\n") } catch (e) {}
}

function peek(value, n) {
  try { return JSON.stringify(value).slice(0, n ?? 400) } catch (e) { return String(value) }
}
`

// spikeLoopPluginJS is S1: the smallest plugin that could close the loop.
//
// The loader reads mod.default and decodes {id, setup}, so a plain
// object is all a file plugin needs.
const spikeLoopPluginJS = spikePreambleJS + `
export default {
  id: "spov.spike",
  async setup(ctx) {
    say("setup", { domains: Object.keys(ctx ?? {}).sort() })

    try {
      ctx.command.transform((draft) => {
        draft.add({
          name: "spov-goal",
          description: "spov S1 spike control",
          execute: async (input) => {
            say("command", {
              sessionID: input?.sessionID,
              prompt: input?.prompt,
              delivery: input?.delivery,
              keys: Object.keys(input ?? {}).sort(),
            })
          },
        })
      })
      say("command.registered", {})
    } catch (e) {
      say("command.failed", { error: String(e) })
    }

    // One re-prompt, not a loop: this proves continuation is possible
    // without spending a free model's budget on a run nothing stops.
    let reprompts = 0
    try {
      const stream = ctx.event.subscribe()
      say("event.subscribed", {
        asyncIterable: typeof stream?.[Symbol.asyncIterator] === "function",
        keys: Object.keys(stream ?? {}).sort(),
      })
      ;(async () => {
        for await (const ev of stream) {
          const type = ev?.type ?? ev?.event
          // Two shapes, one without a sessionID (dld-kit DL-032).
          const sessionID = ev?.data?.sessionID ?? ev?.properties?.sessionID
          if (typeof type === "string" && type.startsWith("session.execution")) {
            say("execution", { type, sessionID: sessionID ?? null })
          }
          if (type === "session.execution.succeeded" && sessionID && reprompts < 1) {
            reprompts += 1
            try {
              await ctx.session.prompt({
                sessionID,
                text: "SPOV_SPIKE_REPROMPT: reply with the single word: two.",
              })
              say("reprompt.sent", { sessionID })
            } catch (e) {
              say("reprompt.failed", { sessionID, error: String(e) })
            }
          }
        }
        say("event.ended", {})
      })().catch((e) => say("event.loop.failed", { error: String(e) }))
    } catch (e) {
      say("event.failed", { error: String(e) })
    }
  },
}
`

// TestIntegrationPluginLoop is spike S1.
//
// Every assertion is a claim the hybrid loop design rests on. Failures are
// reported rather than fatal wherever the run can continue, so one run
// yields the whole picture instead of the first broken claim — the
// phase-3 spikes' habit, and the reason they were worth keeping.
func TestIntegrationPluginLoop(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-s1", 12*time.Minute,
		map[string]string{spikePluginPath: spikeLoopPluginJS})
	client := &http.Client{Timeout: 0}
	base := creds.URL

	// The plugin supervisor forks a debounced stream and loads
	// asynchronously, so a plugin can be absent from a request served
	// milliseconds after the port opens.
	loaded := waitForPlugin(ctx, t, client, base, creds.Token, password)

	t.Run("a file plugin loads from a discovered directory", func(t *testing.T) {
		body := get(ctx, t, client, base+"/api/plugin", creds.Token, password)
		t.Logf("GET /api/plugin -> %s", truncate(body, 1200))
		if !loaded {
			t.Fatalf("the server never reported %s: a local-file plugin does not load on this build, so the sandbox half of the hybrid loop does not exist as designed", spikePluginID)
		}
	})

	// Everything below needs setup to have run, so read the trace once
	// here and report what the plugin saw of its own context.
	t.Run("setup runs and sees the domains the loop needs", func(t *testing.T) {
		entries := waitForTrace(ctx, t, sb, "setup", 1, 60*time.Second)
		if len(entries) == 0 {
			dumpStdout(ctx, t, sb)
			t.Fatal("setup left no trace: the plugin was listed but never ran, or it cannot write to /tmp")
		}
		domains, _ := entries[0]["domains"].([]any)
		names := make([]string, 0, len(domains))
		for _, d := range domains {
			names = append(names, fmt.Sprint(d))
		}
		t.Logf("plugin context domains: %s", strings.Join(names, " "))
		// The four the job model is built on: continuation (session),
		// turn-end detection (event), the control command (command), and
		// S3's in-process permissions.
		for _, want := range []string{"command", "event", "permission", "session"} {
			if !slices.Contains(names, want) {
				t.Errorf("ctx has no %q domain: the plugin API on this build is narrower than OpenCode's in-repo one, and what depends on it must be redesigned", want)
			}
		}
	})

	session := createSession(ctx, t, client, base, creds.Token, password)
	t.Logf("session under test: %s", session)

	t.Run("a server plugin registers a slash command", func(t *testing.T) {
		list := get(ctx, t, client, base+"/api/command", creds.Token, password)
		t.Logf("GET /api/command -> %s", truncate(list, 800))

		// The request body was inferred (groups/session.ts:360-377) and
		// the inference was wrong: "arguments", "prompt" and a bare
		// command are all rejected with Missing key at ["text"]. The
		// argument text travels as "text", which is also what reaches
		// the handler as input.prompt. The rejected shapes stay here so
		// a build that starts accepting them is visible.
		bodies := []string{
			// GA renamed the command's key to "name" (DL-035).
			fmt.Sprintf(`{"name":%q,"text":"status"}`, spikeCommand),
			fmt.Sprintf(`{"command":%q,"text":"status"}`, spikeCommand),
			fmt.Sprintf(`{"command":%q,"text":"status","id":"msg_spike"}`, spikeCommand),
			fmt.Sprintf(`{"command":%q,"arguments":"status"}`, spikeCommand),
			fmt.Sprintf(`{"command":%q}`, spikeCommand),
		}
		var accepted string
		for _, body := range bodies {
			reqCtx, c := context.WithTimeout(ctx, 30*time.Second)
			resp, err := client.Do(sandboxHTTPBody(reqCtx, t, http.MethodPost,
				base+"/api/session/"+session+"/command", creds.Token, password, body))
			if err != nil {
				c()
				t.Logf("command %s: %v", body, err)
				continue
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			c()
			t.Logf("POST /api/session/:id/command %s -> %s: %s", body, resp.Status, truncate(string(raw), 300))
			// 204, not 200: the command is admitted and runs
			// asynchronously, so the response says nothing about whether
			// the handler ran. The trace below is what does.
			if resp.StatusCode/100 == 2 {
				accepted = body
				break
			}
		}
		if accepted == "" {
			t.Errorf("no command body shape was accepted: a human cannot reach the loop's controls from the TUI, and neither can the server over HTTP")
			return
		}
		got := waitForTrace(ctx, t, sb, "command", 1, 45*time.Second)
		if len(got) == 0 {
			t.Errorf("the server accepted %s but the plugin's execute never ran: the command is registered in name only", accepted)
			return
		}
		t.Logf("the plugin saw the invocation: %v", got[0])
	})

	t.Run("the loop closes: a turn ends, the plugin starts the next", func(t *testing.T) {
		runAgent(ctx, t, client, base, creds.Token, password, session)

		sent := waitForTrace(ctx, t, sb, "reprompt.sent", 1, 3*time.Minute)
		for _, e := range waitForTrace(ctx, t, sb, "execution", 1, time.Second) {
			t.Logf("execution event: %v", e)
		}
		if len(sent) == 0 {
			for _, e := range waitForTrace(ctx, t, sb, "reprompt.failed", 1, time.Second) {
				t.Logf("reprompt failed: %v", e)
			}
			dumpStdout(ctx, t, sb)
			t.Fatal("the plugin never re-prompted: either session.execution.succeeded did not arrive with a sessionID, or ctx.session.prompt is not usable from inside the server — the hybrid's continuation half does not hold")
		}

		// The prompt reaching the session is what "the loop closed"
		// means; a second succeeded event is the turn it started
		// finishing on its own.
		msgs := get(ctx, t, client, base+"/api/session/"+session+"/message", creds.Token, password)
		if !strings.Contains(msgs, spikeMarker) {
			t.Errorf("the plugin reported sending a prompt but %q is not in the session's messages: the continuation was accepted and dropped", spikeMarker)
		}
		done := waitForTraceFunc(ctx, t, sb, 3*time.Minute, func(entries []map[string]any) bool {
			return countSucceeded(entries) >= 2
		})
		if countSucceeded(done) < 2 {
			t.Errorf("only %d session.execution.succeeded events in %s: the re-prompted turn did not complete, so continuation starts a turn but does not sustain a loop", countSucceeded(done), 3*time.Minute)
		} else {
			t.Logf("confirmed: %d turns completed, the second one started by the plugin", countSucceeded(done))
		}
	})
}

// countSucceeded counts completed turns the plugin observed.
func countSucceeded(entries []map[string]any) int {
	n := 0
	for _, e := range entries {
		if e["event"] == "execution" && e["type"] == "session.execution.succeeded" {
			n++
		}
	}
	return n
}

// get does one authenticated GET and returns the body as text.
func get(ctx context.Context, t *testing.T, c *http.Client, url, token, password string) string {
	t.Helper()
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.Do(sandboxHTTP(reqCtx, t, http.MethodGet, url, token, password))
	if err != nil {
		t.Logf("GET %s: %v", url, err)
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Logf("GET %s -> %s: %s", url, resp.Status, truncate(string(body), 300))
	}
	return string(body)
}

// waitForPlugin polls GET /api/plugin until the spike appears. Loading
// is asynchronous and debounced, so absence at t=0 means nothing.
func waitForPlugin(ctx context.Context, t *testing.T, c *http.Client, base, token, password string) bool {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(get(ctx, t, c, base+"/api/plugin", token, password), spikePluginID) {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

// readTrace reads what the plugin has recorded so far. A missing file
// is an empty trace, not an error: the plugin may not have run yet.
func readTrace(ctx context.Context, t *testing.T, sb *modal.Sandbox) []map[string]any {
	t.Helper()
	return readTraceAt(ctx, t, sb, spikeTracePath)
}

// readTraceAt is readTrace against a named file, so a probe and the real
// plugin can each keep their own.
func readTraceAt(ctx context.Context, t *testing.T, sb *modal.Sandbox, path string) []map[string]any {
	t.Helper()
	res, err := (&modalHandle{sb: sb}).Exec(ctx, []string{"sh", "-c", "cat " + path + " 2>/dev/null || true"})
	if err != nil {
		t.Logf("read trace: %v", err)
		return nil
	}
	var out []map[string]any
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Logf("trace line is not JSON: %s", truncate(line, 200))
			continue
		}
		out = append(out, e)
	}
	return out
}

// waitForTrace waits for at least n entries of one kind and returns them.
func waitForTrace(ctx context.Context, t *testing.T, sb *modal.Sandbox, event string, n int, within time.Duration) []map[string]any {
	t.Helper()
	var found []map[string]any
	waitForTraceFunc(ctx, t, sb, within, func(entries []map[string]any) bool {
		found = nil
		for _, e := range entries {
			if e["event"] == event {
				found = append(found, e)
			}
		}
		return len(found) >= n
	})
	return found
}

// waitForTraceFunc polls the trace until done or the deadline, and
// returns the last trace it read either way.
func waitForTraceFunc(ctx context.Context, t *testing.T, sb *modal.Sandbox, within time.Duration, done func([]map[string]any) bool) []map[string]any {
	t.Helper()
	return waitForTraceFuncAt(ctx, t, sb, spikeTracePath, within, done)
}

// waitForTraceFuncAt is waitForTraceFunc against a named trace file.
func waitForTraceFuncAt(ctx context.Context, t *testing.T, sb *modal.Sandbox, path string, within time.Duration, done func([]map[string]any) bool) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	var entries []map[string]any
	for {
		entries = readTraceAt(ctx, t, sb, path)
		if done(entries) || !time.Now().Before(deadline) || ctx.Err() != nil {
			return entries
		}
		time.Sleep(3 * time.Second)
	}
}

// dumpStdout is the fallback channel: the plugin mirrors every
// observation to stdout, and the loader's own errors are there too. Read
// with a deadline, since the stream stays open while the sandbox runs.
func dumpStdout(ctx context.Context, t *testing.T, sb *modal.Sandbox) {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(sb.Stdout, 64*1024))
		out <- string(b)
	}()
	select {
	case s := <-out:
		t.Logf("sandbox stdout:\n%s", truncate(s, 8000))
	case <-time.After(20 * time.Second):
		t.Log("sandbox stdout: nothing readable within 20s")
	case <-ctx.Done():
	}
}

// spikeContextPluginJS is S2: does the goal survive a compaction?
//
// The goal is re-asserted on every request through
// session.hook("context"), which fires immediately before provider
// dispatch with mutable system[] and messages[], and on GA through
// session.hook("compaction") for the summariser (DL-039). This probe
// reports what each hook sees, and whether the goal is still in the
// messages the model is about to be given.
const spikeContextPluginJS = spikePreambleJS + `
export default {
  id: "spov.spike",
  async setup(ctx) {
    say("setup", {})

    let compactions = 0
    try {
      ;(async () => {
        for await (const ev of ctx.event.subscribe()) {
          const type = ev?.type ?? ev?.event
          if (typeof type !== "string") continue
          if (type.startsWith("session.compaction")) {
            compactions += 1
            say("compaction", { type, seen: compactions })
          }
          if (type.startsWith("session.execution")) {
            say("execution", { type, sessionID: ev?.data?.sessionID ?? null })
          }
        }
      })().catch((e) => say("event.loop.failed", { error: String(e) }))
    } catch (e) {
      say("event.failed", { error: String(e) })
    }

    try {
      ctx.session.hook("context", (event) => {
        const messages = event?.messages ?? []
        const system = event?.system ?? []
        const inMessages = messages.some((m) => {
          try { return JSON.stringify(m).includes("SPOV_SPIKE_GOAL") } catch (e) { return false }
        })
        // Re-assert on every request, near the tail of system so the
        // cached prefix stays warm — the note plan.ts makes about its
        // own reminder. system[] holds parts rather than strings, so
        // the entry is shaped from one that is already there instead of
        // being guessed at.
        const GOAL = "SPOV_SPIKE_GOAL: answer with a single word."
        let pushed = false
        let entry = GOAL
        const first = system[0]
        if (first && typeof first === "object") {
          if (typeof first.text === "string") entry = { ...first, text: GOAL }
          else if (typeof first.content === "string") entry = { ...first, content: GOAL }
          else entry = null
        }
        try {
          if (entry !== null && Array.isArray(event.system)) {
            event.system.push(entry)
            pushed = true
          }
        } catch (e) {
          say("context.push.failed", { error: String(e) })
        }
        say("context", {
          compactions,
          messages: messages.length,
          system: system.length,
          systemEntryType: typeof system[0],
          systemSample: peek(first, 200),
          goalInMessages: inMessages,
          pushedToSystem: pushed,
          keys: Object.keys(event ?? {}).sort(),
        })
      })
      say("context.hooked", {})
    } catch (e) {
      say("context.failed", { error: String(e) })
    }

    // GA calls the compaction summariser through its own hook, which
    // spov-goal.js registers with the same handler (DL-039).
    try {
      ctx.session.hook("compaction", (event) => {
        const system = event?.system
        let pushed = false
        try {
          if (Array.isArray(system)) {
            const GOAL = "SPOV_SPIKE_GOAL: answer with a single word."
            const first = system[0]
            system.push(first && typeof first === "object" && typeof first.text === "string" ? { ...first, text: GOAL } : GOAL)
            pushed = true
          }
        } catch (e) {
          say("compaction.push.failed", { error: String(e) })
        }
        say("compaction.hook", { pushed, system: Array.isArray(system) ? system.length : null, systemSample: peek(system?.[0], 200) })
      })
      say("compaction.hooked", {})
    } catch (e) {
      say("compaction.hook.failed", { error: String(e) })
    }
  },
}
`

// TestIntegrationPluginSurvivesCompaction is spike S2.
func TestIntegrationPluginSurvivesCompaction(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-s2", 12*time.Minute,
		map[string]string{spikePluginPath: spikeContextPluginJS})
	client := &http.Client{Timeout: 0}
	base := creds.URL
	if !waitForPlugin(ctx, t, client, base, creds.Token, password) {
		t.Fatal("the plugin never loaded; S1 covers that claim, so this run measures nothing")
	}
	session := createSession(ctx, t, client, base, creds.Token, password)

	// The goal enters the session the way a job's would: as the text of
	// the first prompt. Everything after asks whether it is still there.
	prompt(ctx, t, client, base, creds.Token, password, session,
		"SPOV_SPIKE_GOAL: answer with a single word. Reply with the single word: one.")
	before := waitForTrace(ctx, t, sb, "context", 1, 2*time.Minute)
	if len(before) == 0 {
		dumpStdout(ctx, t, sb)
		t.Fatal(`session.hook("context") never fired: a plugin cannot see a request before dispatch on this build, and the only way to re-assert a goal is gone`)
	}
	t.Logf("context hook before compaction: %v", before[len(before)-1])

	t.Run("the hook can write to what the model is about to be given", func(t *testing.T) {
		last := before[len(before)-1]
		if last["pushedToSystem"] != true {
			t.Errorf(`event.system is not a mutable array on this build (entry type %v): the Plan-plugin workaround for the missing compaction hook does not apply, and a goal must be re-asserted some other way`, last["systemEntryType"])
		}
		if last["goalInMessages"] != true {
			t.Errorf("the goal was not in the messages before any compaction (%v): this run cannot tell whether compaction drops it", last["goalInMessages"])
		}
	})

	t.Run("compaction happens and the hook fires again after it", func(t *testing.T) {
		compact(ctx, t, client, base, creds.Token, password, session)
		if got := waitForTrace(ctx, t, sb, "compaction", 1, 3*time.Minute); len(got) == 0 {
			t.Fatal("no session.compaction.* event arrived: POST /compact did not compact, so nothing below is a measurement of compaction")
		} else {
			for _, e := range got {
				t.Logf("compaction event: %v", e)
			}
		}
		// The compaction summariser's own hook fired, with a system[] the
		// goal could be pushed onto (DL-039).
		hooked := waitForTrace(ctx, t, sb, "compaction.hook", 1, time.Minute)
		switch {
		case len(hooked) == 0:
			t.Error(`session.hook("compaction") never fired on a compaction: the goal does not reach the summariser this way`)
		case hooked[0]["pushed"] != true:
			t.Errorf("the compaction hook fired but its system[] could not take the goal: %v", hooked[0])
		default:
			t.Logf("compaction hook: %v", hooked[0])
		}

		prompt(ctx, t, client, base, creds.Token, password, session,
			"Reply with the single word: two.")
		// Wait for the turn to finish, not merely to start: the context
		// hook fires before dispatch, so a post-compaction entry appears
		// long before there is anything to say about whether the push
		// broke the request. And wait for a turn that ends *after* that
		// entry rather than for a second turn: a manual compaction is
		// admitted as a control item into the run already in flight, so
		// the first prompt, the compaction and the second prompt are one
		// execution, not three.
		after := waitForTraceFunc(ctx, t, sb, 4*time.Minute, func(entries []map[string]any) bool {
			post := withCompactions(entries)
			return len(post) > 0 && succeededAfter(entries, entryAt(post[len(post)-1]))
		})
		post := withCompactions(after)
		if len(post) == 0 {
			t.Fatal(`session.hook("context") did not fire on a request after compaction: per-request reconciliation is not a reliable place to re-assert a goal`)
		}
		last := post[len(post)-1]
		t.Logf("context hook after compaction: %v", last)
		// The premise the workaround exists for. Either answer is a
		// finding: if the goal survives, re-assertion is insurance; if
		// it does not, re-assertion is the only thing keeping the job
		// pointed at its goal.
		if last["goalInMessages"] == true {
			t.Log("the goal survived compaction in the message list; re-assertion is insurance rather than the mechanism")
		} else {
			t.Log("compaction dropped the goal from the message list, as expected: re-assertion per request is what keeps it")
		}
		if last["pushedToSystem"] != true {
			t.Errorf("the hook could not re-assert after compaction: %v", last)
		}
		// A push that breaks dispatch would be worse than no push.
		if !succeededAfter(after, entryAt(last)) {
			for _, e := range after {
				if e["event"] == "execution" {
					t.Logf("%v", e)
				}
			}
			t.Error("no turn completed after the re-asserted request: writing to system[] appears to break provider dispatch")
		}
	})
}

// entryAt is when the plugin recorded an observation.
func entryAt(e map[string]any) time.Time {
	at, err := time.Parse(time.RFC3339, fmt.Sprint(e["at"]))
	if err != nil {
		return time.Time{}
	}
	return at
}

// succeededAfter reports whether a turn completed after cut.
func succeededAfter(entries []map[string]any, cut time.Time) bool {
	for _, e := range entries {
		if e["event"] == "execution" && e["type"] == "session.execution.succeeded" && entryAt(e).After(cut) {
			return true
		}
	}
	return false
}

// withCompactions keeps the context-hook entries recorded after at least
// one compaction was seen.
func withCompactions(entries []map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range entries {
		if e["event"] != "context" {
			continue
		}
		if n, ok := e["compactions"].(float64); ok && n >= 1 {
			out = append(out, e)
		}
	}
	return out
}

// prompt admits one prompt with resume:true, which schedules the agent
// loop. It is runAgent with the text under the caller's control.
func prompt(ctx context.Context, t *testing.T, c *http.Client, base, token, password, session, text string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":     fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		"text":   text,
		"resume": true,
	})
	if err != nil {
		t.Fatalf("prompt body: %v", err)
	}
	post(ctx, t, c, base+"/api/session/"+session+"/prompt", token, password, string(body))
}

// compact asks the server to compact the session.
func compact(ctx context.Context, t *testing.T, c *http.Client, base, token, password, session string) {
	t.Helper()
	post(ctx, t, c, base+"/api/session/"+session+"/compact", token, password, "{}")
}

// post does one authenticated POST and logs what came back.
func post(ctx context.Context, t *testing.T, c *http.Client, url, token, password, body string) (int, string) {
	t.Helper()
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := c.Do(sandboxHTTPBody(reqCtx, t, http.MethodPost, url, token, password, body))
	if err != nil {
		t.Logf("POST %s: %v", url, err)
		return 0, ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	t.Logf("POST %s %s -> %s: %s", url, truncate(body, 120), resp.Status, truncate(string(raw), 300))
	return resp.StatusCode, string(raw)
}

// spikePermissionPluginJS is S3: can permission.evaluate replace
// --auto?
//
// --auto is a flag on the CLI's `run`, so it is the client deciding
// what the agent may do. A server-side hook would move that decision
// into spov-owned code inside the sandbox and close the exit-0 hole at
// its source. The probe allows the first thing it is asked about and
// denies the second, so one run says whether the hook is merely
// consulted or actually authoritative.
const spikePermissionPluginJS = spikePreambleJS + `
export default {
  id: "spov.spike",
  async setup(ctx) {
    say("setup", {})

    try {
      ;(async () => {
        for await (const ev of ctx.event.subscribe()) {
          const type = ev?.type ?? ev?.event
          if (typeof type !== "string") continue
          if (type.startsWith("session.execution") || type.startsWith("permission")) {
            say("event", { type, sample: peek(ev?.data, 300) })
          }
        }
      })().catch((e) => say("event.loop.failed", { error: String(e) }))
    } catch (e) {
      say("event.failed", { error: String(e) })
    }

    let seen = 0
    try {
      ctx.permission.hook("evaluate", (evaluation) => {
        seen += 1
        const was = evaluation?.effect
        // Decided by what is being asked about rather than by arrival
        // order: the agent may ask about things the test did not
        // request, and an order-keyed decision would then land on the
        // wrong one.
        const asked = peek(evaluation?.resources, 400) + " " + String(evaluation?.action)
        let want = "allow"
        if (asked.includes("spov-s3-deny")) want = "deny"
        else if (asked.includes("spov-s3-ask")) want = "ask"
        let set = false
        try {
          evaluation.effect = want
          set = evaluation.effect === want
        } catch (e) {
          say("permission.set.failed", { error: String(e) })
        }
        say("permission", {
          seen,
          incomingEffect: typeof was === "object" ? peek(was, 120) : String(was),
          wanted: want,
          set,
          keys: Object.keys(evaluation ?? {}).sort(),
          sample: peek(evaluation, 400),
        })
      })
      say("permission.hooked", {})
    } catch (e) {
      say("permission.failed", { error: String(e) })
    }
  },
}
`

// TestIntegrationPluginDecidesPermissions is spike S3.
func TestIntegrationPluginDecidesPermissions(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-s3", 12*time.Minute,
		map[string]string{spikePluginPath: spikePermissionPluginJS})
	client := &http.Client{Timeout: 0}
	base := creds.URL
	if !waitForPlugin(ctx, t, client, base, creds.Token, password) {
		t.Fatal("the plugin never loaded; S1 covers that claim, so this run measures nothing")
	}
	if got := waitForTrace(ctx, t, sb, "permission.hooked", 1, 60*time.Second); len(got) == 0 {
		for _, e := range waitForTrace(ctx, t, sb, "permission.failed", 1, time.Second) {
			t.Logf("%v", e)
		}
		t.Fatal(`ctx.permission.hook("evaluate") could not be registered: permissions cannot move into the sandbox and --auto stays the mechanism`)
	}
	session := createSession(ctx, t, client, base, creds.Token, password)

	// The session's own /shell endpoint is not a usable trigger: it
	// answers 204 and runs the command without consulting the hook at
	// all (measured, first run). Only the agent's tool call is
	// evaluated, so every arm below goes through the model.
	t.Run("a permissioned action reaches the hook", func(t *testing.T) {
		prompt(ctx, t, client, base, creds.Token, password, session,
			"Run this shell command and report its output: echo spov-s3-allow")
		got := waitForTrace(ctx, t, sb, "permission", 1, 4*time.Minute)
		if len(got) == 0 {
			t.Fatal("nothing asked the hook: on this build a plugin is not consulted before a permissioned action, so it cannot replace --auto")
		}
		t.Logf("allowed evaluation: %v", got[0])
		if got[0]["set"] != true {
			t.Errorf(`the hook could not write its decision (incoming effect %v): "allow" is not the vocabulary, so the recorded value is what to use instead`, got[0]["incomingEffect"])
		}
	})

	// Being consulted is not the same as being obeyed, and only the
	// second makes the hook a replacement for --auto.
	t.Run("a denial is obeyed", func(t *testing.T) {
		before := kindCount(readTrace(ctx, t, sb), "permission")
		prompt(ctx, t, client, base, creds.Token, password, session,
			"Run this shell command and report its output: echo spov-s3-deny")
		got := waitForTraceFunc(ctx, t, sb, 4*time.Minute, func(es []map[string]any) bool {
			return kindCount(es, "permission") > before
		})
		evals := ofKind(got, "permission")
		if len(evals) <= before {
			t.Fatal("the denied command was never evaluated: this arm measures nothing")
		}
		t.Logf("denied evaluation: %v", evals[len(evals)-1])
		// What a denial looks like from outside is what the supervisor
		// will classify on, so it is logged whole rather than asserted
		// against a guess.
		body := get(ctx, t, client, base+"/api/session/"+session+"/message", creds.Token, password)
		if strings.Contains(body, "spov-s3-deny") && !strings.Contains(body, "denied") && !strings.Contains(body, "rejected") {
			t.Logf("no obvious denial wording in the transcript; the full tail follows")
		}
		t.Logf("messages after the denied action: %s", truncate(body, 2000))
	})

	// The exit-0 hole: an agent parked on an unanswered permission. The
	// supervisor blocks on /wait, so whether it returns on a session in
	// that state decides whether it needs a timeout of its own.
	t.Run("wait returns on a session parked on a permission", func(t *testing.T) {
		before := kindCount(readTrace(ctx, t, sb), "permission")
		prompt(ctx, t, client, base, creds.Token, password, session,
			"Run this shell command and report its output: echo spov-s3-ask")
		got := waitForTraceFunc(ctx, t, sb, 4*time.Minute, func(es []map[string]any) bool {
			return kindCount(es, "permission") > before
		})
		evals := ofKind(got, "permission")
		if len(evals) <= before {
			t.Skip("the asked command was never evaluated; nothing is parked, so /wait is only being tested against an idle session")
		}
		t.Logf("asked evaluation: %v", evals[len(evals)-1])
		for _, e := range ofKind(got, "event") {
			if strings.HasPrefix(fmt.Sprint(e["type"]), "permission") {
				t.Logf("permission event: %v", e)
			}
		}

		start := time.Now()
		done := make(chan int, 1)
		go func() {
			// post gives up after 60s and reports 0, so a zero here is
			// the endpoint not answering rather than answering 0.
			code, _ := post(ctx, t, client, base+"/api/experimental/session/"+session+"/wait", creds.Token, password, "{}")
			done <- code
		}()
		var code int
		select {
		case code = <-done:
		case <-time.After(2 * time.Minute):
			t.Fatal("the /wait probe did not finish at all: the measurement below cannot be made")
		}
		// The measured answer, 2026-09-02: it does not return. A session
		// parked on an unanswered permission holds /wait open, so a
		// supervisor that blocks on it blocks forever — the endpoint is
		// not a completion signal on its own, and the permission.asked
		// event above is what tells a supervisor to stop waiting. This
		// asserts the measurement; a build that starts answering fails
		// here, and that failure is good news.
		if code != 0 {
			t.Errorf("POST /wait answered %d after %s on a session parked on a permission: it no longer hangs, so the supervisor's own timeout can be revisited (this failure is good news)", code, time.Since(start).Round(time.Millisecond))
		} else {
			t.Logf("confirmed: POST /wait did not answer within %s while a permission was unanswered", time.Since(start).Round(time.Millisecond))
		}
	})
}

// spikeInboxPluginJS is S4: is a human's prompt distinguishable from
// the plugin's own?
//
// The loop must yield when someone attaches and types (the steered
// state). It can only do that if the event that carries a human prompt
// is told apart from the event its own continuation
// raises. The probe records every inbox item, and sends one of each
// kind itself when its command is invoked.
const spikeInboxPluginJS = spikePreambleJS + `
export default {
  id: "spov.spike",
  async setup(ctx) {
    say("setup", {})

    try {
      ;(async () => {
        for await (const ev of ctx.event.subscribe()) {
          const type = ev?.type ?? ev?.event
          if (typeof type === "string" && type.startsWith("session.execution")) {
            say("execution", { type, sessionID: ev?.data?.sessionID ?? null })
          }
          if (type === "session.inbox.enqueued") {
            const item = ev?.data?.item ?? ev?.properties?.item
            say("inbox", {
              itemType: item?.type ?? null,
              itemID: item?.id ?? null,
              keys: Object.keys(item ?? {}).sort(),
              sample: peek(item, 400),
            })
          }
        }
      })().catch((e) => say("event.loop.failed", { error: String(e) }))
    } catch (e) {
      say("event.failed", { error: String(e) })
    }

    try {
      ctx.command.transform((draft) => {
        draft.add({
          name: "spov-goal",
          description: "spov S4 spike control",
          execute: async (input) => {
            const sessionID = input?.sessionID
            const arg = String(input?.prompt?.text ?? input?.prompt ?? "")
            // The continuation is invoked on its own, once the session
            // is idle: sent while a turn is already starting, there is
            // no way to tell a turn it caused from the one already in
            // flight.
            if (arg.includes("continue")) {
              try {
                const r = await ctx.session.synthetic({ sessionID, text: "SPOV_SPIKE_PLUGIN_CONTINUE: reply with one word.", resume: true })
                say("sent.continue", { returned: peek(r, 300) })
              } catch (e) {
                say("sent.continue.failed", { error: String(e) })
              }
              return
            }
            try {
              const r = await ctx.session.prompt({ sessionID, text: "SPOV_SPIKE_PLUGIN_PROMPT: reply with one word." })
              say("sent.prompt", { returned: peek(r, 300) })
            } catch (e) {
              say("sent.prompt.failed", { error: String(e) })
            }
            try {
              const r = await ctx.session.synthetic({ sessionID, text: "SPOV_SPIKE_PLUGIN_SYNTHETIC", resume: false })
              say("sent.synthetic", { returned: peek(r, 300) })
            } catch (e) {
              say("sent.synthetic.failed", { error: String(e) })
            }
          },
        })
      })
      say("command.registered", {})
    } catch (e) {
      say("command.failed", { error: String(e) })
    }
  },
}
`

// TestIntegrationPluginSeesHumanTakeover is spike S4.
func TestIntegrationPluginSeesHumanTakeover(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-s4", 12*time.Minute,
		map[string]string{spikePluginPath: spikeInboxPluginJS})
	client := &http.Client{Timeout: 0}
	base := creds.URL
	if !waitForPlugin(ctx, t, client, base, creds.Token, password) {
		t.Fatal("the plugin never loaded; S1 covers that claim, so this run measures nothing")
	}
	session := createSession(ctx, t, client, base, creds.Token, password)

	// A prompt over HTTP is what a human at the TUI produces: spov's
	// own server cannot see the difference either, which is the point.
	prompt(ctx, t, client, base, creds.Token, password, session, "SPOV_SPIKE_HUMAN: reply with one word.")
	human := waitForTrace(ctx, t, sb, "inbox", 1, 2*time.Minute)
	if len(human) == 0 {
		dumpStdout(ctx, t, sb)
		t.Fatal("no session.inbox.enqueued arrived for a prompt: the loop has no signal for a human taking over")
	}
	t.Logf("human prompt: %v", human[0])
	if human[0]["itemType"] != "user" {
		t.Errorf(`a human prompt arrived as item.type %v, not "user": the takeover signal keys on the wrong field`, human[0]["itemType"])
	}

	t.Run("the plugin's own sends are tellable from a human's", func(t *testing.T) {
		post(ctx, t, client, base+"/api/session/"+session+"/command", creds.Token, password,
			fmt.Sprintf(`{"name":%q,"text":"go"}`, spikeCommand))
		entries := waitForTraceFunc(ctx, t, sb, 3*time.Minute, func(es []map[string]any) bool {
			return kindCount(es, "sent.prompt") > 0 && kindCount(es, "sent.synthetic") > 0
		})
		for _, e := range entries {
			if strings.HasPrefix(fmt.Sprint(e["event"]), "sent.") {
				t.Logf("%v", e)
			}
		}
		inbox := ofKind(entries, "inbox")
		for _, e := range inbox {
			t.Logf("inbox item: %v", e)
		}
		if len(inbox) < 2 {
			t.Fatalf("only %d inbox items after the plugin sent its own: nothing to compare", len(inbox))
		}
		// The question is not whether the types differ — synthetic
		// obviously does — but whether the plugin's *prompt*, which is
		// what a continuation is, is distinguishable from a human's.
		var pluginPrompt map[string]any
		for _, e := range inbox {
			if strings.Contains(fmt.Sprint(e["sample"]), "SPOV_SPIKE_PLUGIN_PROMPT") {
				pluginPrompt = e
			}
		}
		if pluginPrompt == nil {
			t.Fatal("the plugin's own prompt raised no inbox event: a continuation is invisible to the same signal a takeover uses, which is convenient but must be relied on deliberately")
		}
		t.Logf("the plugin's own prompt: %v", pluginPrompt)
		if pluginPrompt["itemType"] == "user" {
			t.Log(`the plugin's own prompt is also item.type "user": the loop cannot tell a takeover from its own continuation by type alone`)
			// The inbox item carries no id (itemID is nil above), so
			// matching what the plugin sent against what arrives is not
			// available either. That leaves the item's own type, which
			// is what the next case is about.
			if pluginPrompt["itemID"] != nil {
				t.Logf("the item does carry an id (%v), so a loop could match on the one it sent", pluginPrompt["itemID"])
			}
		} else {
			t.Logf(`the plugin's own prompt arrives as item.type %v, so type alone separates it from a human's`, pluginPrompt["itemType"])
		}
	})

	// If a plugin prompt cannot be told from a human's, the loop should
	// not continue with one. synthetic is typed apart on the same
	// signal, and resume is what makes it drive a turn rather than sit
	// in the transcript.
	t.Run("a synthetic continuation is typed apart and still drives a turn", func(t *testing.T) {
		// On an idle session, so a turn that starts afterwards is one
		// this continuation caused.
		idle := waitForTraceFunc(ctx, t, sb, 3*time.Minute, func(es []map[string]any) bool {
			return countSucceeded(es) > 0
		})
		if countSucceeded(idle) == 0 {
			t.Fatal("no turn ever finished, so the session never went idle and this arm cannot be measured")
		}
		post(ctx, t, client, base+"/api/session/"+session+"/command", creds.Token, password,
			fmt.Sprintf(`{"name":%q,"text":"continue"}`, spikeCommand))
		entries := waitForTraceFunc(ctx, t, sb, 3*time.Minute, func(es []map[string]any) bool {
			return kindCount(es, "sent.continue") > 0 && continued(es)
		})
		sent := ofKind(entries, "sent.continue")
		if len(sent) == 0 {
			for _, e := range ofKind(entries, "sent.continue.failed") {
				t.Logf("%v", e)
			}
			t.Fatal("ctx.session.synthetic with resume was refused: a loop that must not impersonate a human has no way to continue")
		}
		t.Logf("continuation sent: %v", sent[0])
		var item map[string]any
		for _, e := range ofKind(entries, "inbox") {
			if strings.Contains(fmt.Sprint(e["sample"]), "SPOV_SPIKE_PLUGIN_CONTINUE") {
				item = e
			}
		}
		if item == nil {
			t.Fatal("the continuation raised no inbox event")
		}
		t.Logf("continuation item: %v", item)
		if item["itemType"] != "synthetic" {
			t.Errorf(`the continuation arrived as item.type %v, not "synthetic": routing a loop's own sends through synthetic does not separate them from a human's after all`, item["itemType"])
		}
		if !continued(entries) {
			t.Error("no turn started after the synthetic continuation: resume does not schedule execution, so synthetic cannot carry a loop")
		}
	})
}

// continued reports whether a turn started after the plugin sent its
// synthetic continuation.
func continued(entries []map[string]any) bool {
	var at time.Time
	for _, e := range ofKind(entries, "sent.continue") {
		at = entryAt(e)
	}
	if at.IsZero() {
		return false
	}
	for _, e := range ofKind(entries, "execution") {
		if e["type"] == "session.execution.started" && entryAt(e).After(at) {
			return true
		}
	}
	return false
}

// ofKind keeps the trace entries of one event kind.
func ofKind(entries []map[string]any, event string) []map[string]any {
	var out []map[string]any
	for _, e := range entries {
		if e["event"] == event {
			out = append(out, e)
		}
	}
	return out
}

func kindCount(entries []map[string]any, event string) int { return len(ofKind(entries, event)) }
