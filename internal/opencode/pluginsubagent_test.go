package opencode

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// A job's subagents in the shapes spike S-SA3 measured on the plugin's
// own subscription at 2.3.0: a child's session.created names the job's
// session as its parent, and its execution frames carry its own id.
const (
	kidCreated = `{ type: "session.created", data: { sessionID: "ses_kid", parentID: "ses_test", title: "probe child" } }`
	kidStarted = `{ type: "session.execution.started", data: { sessionID: "ses_kid" } }`
	kidDone    = `{ type: "session.execution.succeeded", data: { sessionID: "ses_kid" } }`
	kidReport  = `{ type: "session.inbox.enqueued", data: { sessionID: "ses_test", inboxID: "msg_r",` +
		` item: { type: "synthetic", payload: { text: "<subagent sessionID=\"ses_kid\" state=\"completed\">done</subagent>",` +
		` metadata: { source: "subagent", childID: "ses_kid", state: "completed" } }, delivery: "steer" } } }`
	grandkidCreated = `{ type: "session.created", data: { sessionID: "ses_grandkid", parentID: "ses_kid" } }`
	grandkidStarted = `{ type: "session.execution.started", data: { sessionID: "ses_grandkid" } }`
)

// continuations is how many continuations the plugin sent.
func continuations(run resumeRun) []map[string]any {
	var out []map[string]any
	for _, m := range run.Synth {
		if m["resume"] == true {
			out = append(out, m)
		}
	}
	return out
}

// withSubagentWait renders the plugin with a wait of ms milliseconds.
func withSubagentWait(ms int) func(string) string {
	return func(source string) string {
		want := `"subagentWaitMs":` + strconv.FormatInt(SubagentWait.Milliseconds(), 10)
		if !strings.Contains(source, want) {
			panic("the rendered plugin has no " + want)
		}
		return strings.Replace(source, want, `"subagentWaitMs":`+strconv.Itoa(ms), 1)
	}
}

// The job's turn ending while its subagent works sends no continuation;
// the child's report starts the next turn, and that turn's end continues
// as any other.
//
// @decision(DL-143)
func TestTheLoopWaitsWhileASubagentOfTheJobRuns(t *testing.T) {
	run := driveEvents(t, []string{holdStarted, kidCreated, kidStarted, holdSucceeded}, "")
	if got := continuations(run); len(got) != 0 {
		t.Fatalf("a continuation went out while the job's subagent ran: %v", got)
	}
	waiting := evidenceNamed(run, "continue.waiting")
	if len(waiting) != 1 || waiting[0]["running"] != float64(1) {
		t.Errorf("continue.waiting = %v, want one naming one running subagent", waiting)
	}

	run = driveEvents(t, []string{holdStarted, kidCreated, kidStarted, holdSucceeded, kidDone, kidReport, holdStarted, holdSucceeded}, "")
	got := continuations(run)
	if len(got) != 1 || got[0]["text"] != continueText || got[0]["delivery"] != "queue" {
		t.Errorf("after the report's turn the plugin sent %v, want one ordinary queued continuation", got)
	}
	if len(evidenceNamed(run, "loop.held")) != 0 {
		t.Error("the child's report took the job over")
	}
}

// A child's failed or interrupted turn has ended too, and a child of a
// child is its parent's to report on.
//
// @decision(DL-143)
func TestOnlyTheJobsOwnRunningChildrenHoldTheLoop(t *testing.T) {
	for _, ending := range []string{"failed", "interrupted"} {
		ended := strings.ReplaceAll(kidDone, "succeeded", ending)
		run := driveEvents(t, []string{holdStarted, kidCreated, kidStarted, ended, holdSucceeded}, "")
		if len(continuations(run)) != 1 {
			t.Errorf("a child whose turn %s still held the loop: %v", ending, run.Evidence)
		}
	}
	run := driveEvents(t, []string{holdStarted, kidCreated, kidStarted, grandkidCreated, grandkidStarted, kidDone, holdSucceeded}, "")
	if len(continuations(run)) != 1 {
		t.Errorf("a grandchild held the loop: %v", run.Evidence)
	}
	// A root that is not the job's has no say either.
	other := strings.ReplaceAll(strings.ReplaceAll(kidCreated, `parentID: "ses_test"`, `parentID: "ses_else"`), "ses_kid", "ses_x")
	run = driveEvents(t, []string{holdStarted, other, strings.ReplaceAll(kidStarted, "ses_kid", "ses_x"), holdSucceeded}, "")
	if len(continuations(run)) != 1 {
		t.Errorf("another session's child held the loop: %v", run.Evidence)
	}
}

// The hold is bounded: at SubagentWait with no turn of the job's started,
// the ordinary continuation goes out with a sentence on the subagents
// still running. A turn that starts first takes its place.
//
// @decision(DL-143)
func TestTheHoldEndsAtTheSubagentWait(t *testing.T) {
	run := driveEventsPaced(t, []string{holdStarted, kidCreated, kidStarted, holdSucceeded}, "", 30, withSubagentWait(40))
	got := continuations(run)
	if len(got) != 1 {
		t.Fatalf("at the wait the plugin sent %v, want one continuation", got)
	}
	text, _ := got[0]["text"].(string)
	if !strings.HasPrefix(text, continueText) || !strings.Contains(text, "1 subagent") {
		t.Errorf("the bounded continuation says %q, want the ordinary text and the one subagent still running", text)
	}
	if got[0]["delivery"] != "queue" {
		t.Errorf("the bounded continuation was sent as %v", got[0])
	}

	// The report's turn began before the wait ran out: it alone decides.
	run = driveEventsPaced(t, []string{holdStarted, kidCreated, kidStarted, holdSucceeded, kidDone, holdStarted}, "", 30, withSubagentWait(120))
	if got := continuations(run); len(got) != 0 {
		t.Errorf("the wait sent %v though a turn of the job's had started", got)
	}
}

// A stop is never held back by a subagent, and a held or paused loop is
// left as it was.
//
// @decision(DL-143) @decision(DL-052)
func TestTheHoldNeitherDelaysAStopNorReleasesAHeldLoop(t *testing.T) {
	run := driveEvents(t, []string{holdStarted, kidCreated, kidStarted, holdHuman, holdHumanDelivered, holdSucceeded}, "")
	if len(run.Synth) != 0 || len(evidenceNamed(run, "continue.waiting")) != 0 {
		t.Errorf("a held loop was treated as waiting: %v", run.Evidence)
	}
	run = driveEvents(t, []string{holdStarted, kidCreated, kidStarted}, "stop")
	stopped := false
	for _, m := range run.Synth {
		if text, _ := m["text"].(string); strings.HasPrefix(text, LoopStoppedMarker) {
			stopped = true
		}
	}
	if !stopped {
		t.Errorf("a running subagent kept the stop from being announced: %v", run.Synth)
	}
}

// The wait the plugin is given is SubagentWait.
//
// @decision(DL-143)
func TestThePluginIsGivenTheSubagentWait(t *testing.T) {
	cfg := decodeRenderedConfig(t, mustRenderPlugin(t))
	if time.Duration(cfg.SubagentWaitMS)*time.Millisecond != SubagentWait || SubagentWait != 10*time.Minute {
		t.Errorf("the plugin waits %d ms, want SubagentWait (%s), which DL-143 sets at ten minutes", cfg.SubagentWaitMS, SubagentWait)
	}
	if !strings.Contains(cfg.SubagentWaitNote, "{n}") {
		t.Errorf("the wait's sentence has no place for the count: %q", cfg.SubagentWaitNote)
	}
}

// The agent is told that subagents report on their own, that work not
// reported is lost when the job completes, and to wait for what it
// needs: in the tool's description, the continuation and AGENTS.md.
//
// @decision(DL-143)
func TestTheAgentIsToldSubagentsReportOnTheirOwn(t *testing.T) {
	run := driveCompletionTool(t, "done", true)
	cfg := decodeRenderedConfig(t, mustRenderPlugin(t))
	for _, place := range []struct{ name, text string }{
		{"the completion tool's description", run.Description},
		{"the continuation", cfg.ContinueText},
		{"AGENTS.md", AgentsMarkdown([]WorkspaceRepo{{Slug: "a/b", Dir: "b", Host: "github.com", CLI: CLIGh}}, "report")},
	} {
		lowered := strings.ToLower(place.text)
		for _, want := range []string{"background", "report", "lost", "wait for the reports you need"} {
			if !strings.Contains(lowered, want) {
				t.Errorf("%s does not say %q: %q", place.name, want, place.text)
			}
		}
	}
	if strings.Contains(AgentsMarkdown(nil, ""), "Subagents") {
		t.Error("an interactive session's AGENTS.md speaks of a job's completion")
	}
}

// The plugin can miss the job's first frames: it subscribes when
// OpenCode loads it, which may be after the job's opening turn began
// (spike S-SA3). A child is then the first session it hears of, and its
// prompt, a `user` item in its own inbox, must neither be taken for a
// person's takeover nor make the child the job's session.
//
// @decision(DL-143) @decision(DL-052) @decision(DL-054)
func TestAChildSeenBeforeTheJobsOwnFramesIsNotTheJob(t *testing.T) {
	kidPrompt := `{ type: "session.inbox.enqueued", data: { sessionID: "ses_kid", inboxID: "msg_k",` +
		` item: { type: "user", payload: { text: "You are a subagent spawned by another session." }, delivery: "steer" } } }`
	kidDelivered := `{ type: "session.inbox.delivered", data: { sessionID: "ses_kid", inboxID: "msg_k" } }`
	run := driveEvents(t, []string{kidCreated, kidPrompt, kidStarted, kidDelivered, holdSucceeded}, "")
	if len(evidenceNamed(run, "loop.held")) != 0 {
		t.Errorf("a subagent's prompt took the job over: %v", run.Evidence)
	}
	if len(evidenceNamed(run, "continue.waiting")) != 1 || len(continuations(run)) != 0 {
		t.Errorf("the job's turn end did not wait for its subagent: %v", run.Evidence)
	}
	run = driveEvents(t, []string{kidCreated, kidPrompt, kidStarted, kidDelivered, kidDone, kidReport, holdStarted, holdSucceeded}, "")
	if got := continuations(run); len(got) != 1 || got[0]["sessionID"] != "ses_test" {
		t.Errorf("after the report the plugin sent %v, want one continuation of the job's session", got)
	}
}
