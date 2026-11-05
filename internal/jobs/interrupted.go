package jobs

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// Telling a turn a human stopped from a call the agent was refused.
//
// Both leave the same evidence: OpenCode marks an in-flight tool part
// error: {type: "aborted"} on both an interrupt and a human's permission
// reject. Gate 3 discounts a part as spov's own Stop only when both
// remaining signals agree — the part's time.completed lands within
// stopWindow of a session.execution.interrupted event with reason "user",
// and the error message matches the interrupt's wording. Neither signal
// alone is enough: filtering the event on reason is required because a
// human's reject also raises session.execution.interrupted, at the same
// proximity, with reason "shutdown".
//
// @decision(DL-053) @decision(DL-050)

const (
	// stopReasonUser is the interrupted `reason` a person stopping a
	// turn carries.
	stopReasonUser = "user"
	// stopReasonShutdown is OpenCode's own housekeeping reason — and also
	// what a human's tool-call reject produces, which is why wasShutDown
	// needs a second signal.
	stopReasonShutdown = "shutdown"
	// interruptedMessage is what OpenCode writes on a part its own
	// interrupt stopped. Corroboration only.
	interruptedMessage = "Tool execution interrupted"
	// declinedMessage is what a human's reject writes. Corroboration
	// only — see declineTimes.
	declinedMessage = "The user declined this tool call"
	// stopWindow is how far a part's `time.completed` may sit from the
	// interrupt event and still be one that interrupt stopped. Measured
	// at 7-14ms against 17-22s for the part a human refused in the same
	// session; see DL-053.
	stopWindow = 2 * time.Second
)

// humanStops reads the instants at which a person stopped a turn, in
// milliseconds on the sandbox's clock. The timestamp is read out of the
// frame rather than store.Event.At, which falls back to the server's
// clock when a frame carries none and would silently cross clocks.
//
// @decision(DL-053)
func humanStops(events []store.Event) []int64 {
	return interruptsBecause(events, stopReasonUser)
}

// isHumanStop reports whether one event is a turn a person stopped.
//
// @decision(DL-053)
func isHumanStop(e store.Event) bool {
	if e.Source != store.SourceOpenCode {
		return false
	}
	if !strings.HasSuffix(e.Name, ".execution.interrupted") {
		return false
	}
	return interruptReason(e.Raw) == stopReasonUser
}

// interruptsBecause reads the instants of every turn interrupted for
// this reason, zero for a frame that carries no `created`. The zero is
// kept rather than dropped: it cannot match a real `time.completed`, so
// the part-level discount simply never fires on it, while wasShutDown
// treats an uncorrelated shutdown as a shutdown either way.
//
// SourceOpenCode only, the source the frame actually arrives on and
// never stored ephemeral; this keeps spov's own markers from being read
// back as the session's voice (DL-046), not a boundary against the
// sandbox.
//
// @decision(DL-053) @decision(DL-018)
func interruptsBecause(events []store.Event, reason string) []int64 {
	var out []int64
	for _, e := range events {
		if e.Source != store.SourceOpenCode {
			continue
		}
		if !strings.HasSuffix(e.Name, ".execution.interrupted") {
			continue
		}
		if interruptReason(e.Raw) != reason {
			continue
		}
		out = append(out, frameCreated(e.Raw))
	}
	return out
}

// declineTimes reads when each call a human rejected stopped. It is the
// mirror of stoppedByAHuman and takes the same two signals together.
//
// @decision(DL-053)
func declineTimes(messages []store.Message) []int64 {
	var out []int64
	for _, m := range messages {
		for _, p := range toolParts(m.Raw) {
			if p.ErrorType != refusalByHuman || p.CompletedAt <= 0 {
				continue
			}
			if strings.Contains(p.ErrorMessage, declinedMessage) {
				out = append(out, p.CompletedAt)
			}
		}
	}
	return out
}

// nearAny reports whether an instant sits within stopWindow of one of
// these.
//
// @decision(DL-053)
func nearAny(at int64, times []int64) bool {
	window := stopWindow.Milliseconds()
	for _, t := range times {
		d := at - t
		if d < 0 {
			d = -d
		}
		if d <= window {
			return true
		}
	}
	return false
}

// frameCreated reads a frame's own `created`, at the top level beside
// `id` and `type`, not inside `data` where `reason` is.
//
// @decision(DL-053)
func frameCreated(raw []byte) int64 {
	if len(raw) == 0 {
		return 0
	}
	// float64, not int64, matching the recorder's own findMillis:
	// decoding into an int64 would make a build that emits 1.789e12 an
	// unmarshal error, silently turning the correlation off.
	var frame struct {
		Created float64 `json:"created"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return 0
	}
	return int64(frame.Created)
}

// stoppedByAHuman reports whether this part was cut off by a person
// stopping the turn rather than refused by a permission decision. Three
// things must hold together: the type an interrupt leaves, completion
// within stopWindow of an interrupt a person caused, and the interrupt's
// message wording.
//
// @decision(DL-053) @decision(DL-050)
func stoppedByAHuman(p toolPart, stops []int64) bool {
	if p.ErrorType != refusalByHuman || p.CompletedAt <= 0 {
		return false
	}
	if !strings.Contains(p.ErrorMessage, interruptedMessage) {
		return false
	}
	return nearAny(p.CompletedAt, stops)
}
