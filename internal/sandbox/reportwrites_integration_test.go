//go:build integration

package sandbox

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// Spikes S-W1 and S-W2, measured before DL-040 were built.
// Each creates one billable sandbox and terminates it.

// spikeEditShapePluginJS allows everything and records each evaluation
// whole, so the resources a file tool asserts can be read back.
const spikeEditShapePluginJS = spikePreambleJS + `
export default {
  id: "spov.spike",
  async setup(ctx) {
    say("setup", {})
    try {
      ;(async () => {
        for await (const ev of ctx.event.subscribe()) {
          const type = ev?.type ?? ev?.event
          if (typeof type === "string" && type.startsWith("session.execution")) {
            say("execution", { type, sessionID: ev?.data?.sessionID ?? ev?.properties?.sessionID ?? null })
          }
        }
      })().catch((e) => say("event.loop.failed", { error: String(e) }))
    } catch (e) {
      say("event.failed", { error: String(e) })
    }
    try {
      ctx.permission.hook("evaluate", (evaluation) => {
        evaluation.effect = "allow"
        say("permission", {
          action: String(evaluation?.action ?? ""),
          resources: evaluation?.resources ?? null,
          keys: Object.keys(evaluation ?? {}).sort(),
          sample: peek(evaluation, 600),
        })
      })
      say("permission.hooked", {})
    } catch (e) {
      say("permission.failed", { error: String(e) })
    }
  },
}
`

// spikeSeed is a workspace with one repository holding two files.
var spikeSeed = map[string]string{
	"/workspace/repo/a.txt": "old a\n",
	"/workspace/repo/b.txt": "old b\n",
	"/workspace/repo/c.txt": "old c\n",
	"/workspace/repo/d.txt": "old d\n",
}

// promptAndWait prompts and waits for the session's turn to end, then
// returns the permission evaluations the turn produced.
func promptAndWait(ctx context.Context, t *testing.T, c *http.Client, sb *modal.Sandbox, base, token, password, session, text string) []map[string]any {
	t.Helper()
	before := readTrace(ctx, t, sb)
	ended := func(es []map[string]any) int {
		n := 0
		for _, e := range ofKind(es, "execution") {
			if e["sessionID"] == session && e["type"] != "session.execution.started" {
				n++
			}
		}
		return n
	}
	was := ended(before)
	prompt(ctx, t, c, base, token, password, session, text)
	after := waitForTraceFunc(ctx, t, sb, 4*time.Minute, func(es []map[string]any) bool { return ended(es) > was })
	return ofKind(after, "permission")[kindCount(before, "permission"):]
}

// TestIntegrationSWEditResourceShapes is spike S-W1: the resources an
// edit asserts for an absolute path, a relative one, a new directory,
// a patch over two files, and from a session inside a repository.
//
// @decision(DL-040)
func TestIntegrationSWEditResourceShapes(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	files := map[string]string{spikePluginPath: spikeEditShapePluginJS}
	for k, v := range spikeSeed {
		files[k] = v
	}
	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-sw1", 25*time.Minute, files)
	client := &http.Client{Timeout: 0}
	base := creds.URL
	if !waitForPlugin(ctx, t, client, base, creds.Token, password) {
		dumpStdout(ctx, t, sb)
		t.Fatal("the spike plugin never loaded")
	}

	atRoot := createSessionAt(ctx, t, client, base, creds.Token, password, "/workspace")
	inRepo := createSessionAt(ctx, t, client, base, creds.Token, password, "/workspace/repo")
	for _, c := range []struct{ name, session, text string }{
		{"absolute", atRoot, "Using your file-writing tool, not the shell, create the file /workspace/out/abs.md containing the word abs. Then reply with the single word: done."},
		{"relative", atRoot, "Using your file-writing tool, not the shell, create the file out/rel.md, written as that relative path, containing the word rel. Then reply with the single word: done."},
		{"new directories", atRoot, "Using your file-writing tool, not the shell, create the file /workspace/.scratch/new/deep/c.txt containing the letter c. Then reply with the single word: done."},
		{"edit tool", atRoot, "Using your file-editing tool, not the shell, replace the word old with new in /workspace/repo/a.txt. Then reply with the single word: done."},
		{"patch over two files", atRoot, "Using your patch tool (apply_patch), not the shell, apply ONE patch that replaces the word old with new in both /workspace/repo/c.txt and /workspace/repo/d.txt. Then reply with the single word: done."},
		{"relative from a repository", inRepo, "Using your file-writing tool, not the shell, create the file ../out/fromrepo.md, written as that relative path, containing the word x. Then reply with the single word: done."},
		{"absolute from a repository", inRepo, "Using your file-writing tool, not the shell, create the file /workspace/out/fromrepo-abs.md containing the word y. Then reply with the single word: done."},
	} {
		evals := promptAndWait(ctx, t, client, sb, base, creds.Token, password, c.session, c.text)
		if len(evals) == 0 {
			t.Logf("MEASURED %s: no permission was evaluated", c.name)
		}
		for _, e := range evals {
			t.Logf("MEASURED %s: action=%v resources=%v keys=%v sample=%v", c.name, e["action"], e["resources"], e["keys"], e["sample"])
		}
	}
	code, out, _ := execIn(ctx, t, sb, "sh", "-c", "cd /workspace && find . -type f | sort && cat repo/*.txt")
	t.Logf("workspace afterwards (exit %d):\n%s", code, out)
}

// spikeMidTurnPluginJS denies an edit of a path containing "blocked"
// and, on the first such denial, sends one resume:false synthetic
// asking for a shell command the agent would not otherwise run.
const spikeMidTurnPluginJS = spikePreambleJS + `
let noted = false
let session = null
export default {
  id: "spov.spike",
  async setup(ctx) {
    say("setup", {})
    try {
      ;(async () => {
        for await (const ev of ctx.event.subscribe()) {
          const type = ev?.type ?? ev?.event
          if (typeof type !== "string") continue
          const sid = ev?.data?.sessionID ?? ev?.properties?.sessionID ?? null
          if (type.startsWith("session.execution")) {
            if (!session && sid) session = sid
            say("execution", { type, sessionID: sid })
          }
          if (type === "session.inbox.enqueued") {
            say("inbox", { sample: peek(ev?.data ?? ev?.properties, 300) })
          }
        }
      })().catch((e) => say("event.loop.failed", { error: String(e) }))
    } catch (e) {
      say("event.failed", { error: String(e) })
    }
    try {
      ctx.permission.hook("evaluate", (evaluation) => {
        const subject = String(evaluation?.action) + " " + peek(evaluation?.resources, 400)
        const want = subject.includes("blocked") ? "deny" : "allow"
        evaluation.effect = want
        say("permission", { want, subject, keys: Object.keys(evaluation ?? {}).sort(), sessionID: evaluation?.sessionID ?? null })
        if (want === "deny" && !noted) {
          noted = true
          const sessionID = evaluation?.sessionID ?? session
          ctx.session.synthetic({
            sessionID,
            text: "spov: writing that file is not allowed and retrying will not change it. " +
              "Instead, run the shell command: echo SPOV-NOTE-SEEN",
            resume: false,
          }).then(
            (r) => say("note.sent", { sessionID, returned: peek(r, 200) }),
            (e) => say("note.failed", { sessionID, error: String(e) }),
          )
        }
      })
      say("permission.hooked", {})
    } catch (e) {
      say("permission.failed", { error: String(e) })
    }
  },
}
`

// TestIntegrationSWSyntheticMidTurn is spike S-W2: whether a synthetic
// sent while a turn is running reaches the model before that turn ends.
//
// @decision(DL-040)
func TestIntegrationSWSyntheticMidTurn(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	files := map[string]string{spikePluginPath: spikeMidTurnPluginJS}
	for k, v := range spikeSeed {
		files[k] = v
	}
	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-sw2", 12*time.Minute, files)
	client := &http.Client{Timeout: 0}
	base := creds.URL
	if !waitForPlugin(ctx, t, client, base, creds.Token, password) {
		dumpStdout(ctx, t, sb)
		t.Fatal("the spike plugin never loaded")
	}
	session := createSessionAt(ctx, t, client, base, creds.Token, password, "/workspace")
	promptAndWait(ctx, t, client, sb, base, creds.Token, password, session,
		"Using your file-writing tool, not the shell, create /workspace/blocked.txt containing hi. "+
			"If it fails, read any message you receive and do what it says, then reply with what happened.")
	// Anything the note starts after the turn has ended is a second turn.
	time.Sleep(30 * time.Second)

	trace := readTrace(ctx, t, sb)
	turnEnd, sawNote := -1, -1
	for i, e := range trace {
		switch e["event"] {
		case "execution":
			t.Logf("%v %v", e["at"], e["type"])
			if turnEnd < 0 && e["type"] != "session.execution.started" {
				turnEnd = i
			}
		case "permission":
			t.Logf("%v permission %v: %v (keys %v, sessionID %v)", e["at"], e["want"], e["subject"], e["keys"], e["sessionID"])
			if subject, _ := e["subject"].(string); sawNote < 0 && strings.Contains(subject, "SPOV-NOTE-SEEN") {
				sawNote = i
			}
		default:
			t.Logf("%v %v %v", e["at"], e["event"], e)
		}
	}
	switch {
	case sawNote < 0:
		t.Log("MEASURED the agent never ran the note's command")
	case turnEnd < 0 || sawNote < turnEnd:
		t.Log("MEASURED the note reached the model within the turn it was sent in")
	default:
		t.Log("MEASURED the note was acted on only after the turn it was sent in had ended")
	}
	messages := get(ctx, t, client, base+"/api/session/"+session+"/message", creds.Token, password)
	t.Logf("messages: %s", truncate(messages, 6000))
}
