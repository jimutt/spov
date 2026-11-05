package opencode

import (
	"strings"
	"testing"
)

// TestThePauseNoticeSaysWhatItActuallyDoes pins that the notice does not
// claim to stop work already running, and says that scope out loud.
//
// @decision(DL-039)
func TestThePauseNoticeSaysWhatItActuallyDoes(t *testing.T) {
	notice := pauseNotice(t)
	lowered := strings.ToLower(notice)

	// It must NOT claim to stop what is running. "The loop will not
	// continue until resumed" reads as a plain contradiction to a human
	// watching the agent open files.
	if strings.Contains(lowered, "will not continue until resumed") {
		t.Error("the notice still claims the loop will not continue until resumed; " +
			"a reader watching the agent work is being told the opposite of what they can see")
	}
	// And it must say the scope out loud, or the reader has to infer it
	// from an absence.
	if !strings.Contains(lowered, "already running") && !strings.Contains(lowered, "in flight") {
		t.Error("the notice does not say that work already running is unaffected, " +
			"which is the one thing a reader watching the agent needs to know")
	}
}

// TestThePauseNoticeAsksTheAgentToStop pins that the notice addresses
// the agent directly and asks it to take no new action.
//
// @decision(DL-039)
func TestThePauseNoticeAsksTheAgentToStop(t *testing.T) {
	notice := pauseNotice(t)
	lowered := strings.ToLower(notice)

	// Addressed, so a model reads it as an instruction rather than as
	// status text about something else.
	if !strings.Contains(lowered, "to the agent") {
		t.Error("the notice is not addressed to the agent, so it reads as status " +
			"rather than as something to act on")
	}
	// The instruction itself.
	if !strings.Contains(lowered, "stop") {
		t.Error("the notice does not ask the agent to stop")
	}
	if !strings.Contains(lowered, "no new action") && !strings.Contains(lowered, "not start") &&
		!strings.Contains(lowered, "do not start") {
		t.Error("the notice does not tell the agent to take no new action, which is the " +
			"part that distinguishes it from an agent finishing its turn normally")
	}
	// And what happens next, because an instruction with no resolution
	// is one a model talks itself out of — the measured run's reasoning
	// was exactly "there's no way to wait".
	if !strings.Contains(lowered, "resume") {
		t.Error("the notice does not say anyone will resume the agent, so it leaves " +
			"the same gap the measured run reasoned its way out of")
	}
}

// TestThePauseNoticeDoesNotInviteCompletion pins that the notice never
// names the completion tool, and says explicitly not to declare
// completion.
//
// @decision(DL-039) @decision(DL-048)
func TestThePauseNoticeDoesNotInviteCompletion(t *testing.T) {
	notice := pauseNotice(t)
	if strings.Contains(notice, CompletionTool) {
		t.Errorf("the pause notice names %s; an agent told to stop would end the job "+
			"rather than suspend it, which is the opposite of what a pause is for", CompletionTool)
	}
	lowered := strings.ToLower(notice)
	// It says the opposite explicitly, rather than merely omitting it.
	if !strings.Contains(lowered, "not declare") && !strings.Contains(lowered, "don't declare") {
		t.Error("the notice does not tell the agent NOT to declare completion; omitting " +
			"the tool's name is weaker than saying so, and an agent looking for a way " +
			"to comply may find the tool on its own")
	}
}

// pauseNotice is what the pause command replies with, read out of the
// RENDERED plugin rather than from a Go literal — there is no Go
// constant for it, so the source is the only honest place to read it.
func pauseNotice(t *testing.T) string {
	t.Helper()
	src, err := (&Plugin{Kind: "report", Goal: "g"}).Source()
	if err != nil {
		t.Fatal(err)
	}
	const marker = "const PAUSE_NOTICE ="
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatal("the plugin declares no PAUSE_NOTICE; the pause reply has been inlined or renamed")
	}
	rest := src[i+len(marker):]
	// The declaration ends at the first blank line, since it is a
	// concatenation spanning several source lines.
	if j := strings.Index(rest, "\n\n"); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(src, "reply(ctx, sessionID, PAUSE_NOTICE)") {
		t.Error("the pause command does not send PAUSE_NOTICE, so this test reads text " +
			"nothing emits")
	}
	return rest
}
