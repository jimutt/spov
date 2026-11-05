package jobs

import (
	"encoding/json"
	"strings"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/store"
)

// The supervisor learns what a job is doing from rows the recorder
// already wrote, and never reads the sandbox itself. The one collect at
// delivery goes through the Collect and Evidence seams the server sets;
// the supervisor-holds-no-http-surface rule in .golangci.yml covers the
// rest. This file holds the reading; supervisor_worker.go acts on it.
//
// @decision(DL-046) @decision(DL-018)

// The frames the supervisor reads, by the exact names the recorder
// stores them under.
//
// @decision(DL-046)
const (
	eventPermissionAsked   = "permission.asked"
	eventPermissionReplied = "permission.replied"
	EventInboxEnqueued     = "session.inbox.enqueued"
	EventInboxDelivered    = "session.inbox.delivered"
	EventInboxCancelled    = "session.inbox.cancelled"
)

// AfterContinue and AfterHandover are what a person's message says
// follows it on a job, carried at item.payload.metadata.spov.after. An
// item that says nothing hands over (takesOver).
//
// @decision(DL-052)
const (
	AfterContinue = "continue"
	AfterHandover = "handover"
)

// jobSignal is what one stored event means for a job.
//
// @decision(DL-046)
type jobSignal int

const (
	// signalNone is an event the supervisor does not act on.
	signalNone jobSignal = iota
	// signalTurnEnded is a turn finishing, which is when a job's
	// progress is reconsidered.
	signalTurnEnded
	// signalParked is the session waiting on a permission nobody has
	// answered.
	signalParked
	// signalAnswered is that permission being answered. It does not by
	// itself release the job: the frame names no principal, so it
	// cannot be told apart from the job's own plugin answering its own
	// question.
	signalAnswered
	// signalSteered is a human taking the session over.
	signalSteered
	// signalGap is the recorder saying it was not listening for a
	// while. It is not "nothing happened" — it is "what happened here
	// is unknown".
	signalGap
	// signalStopped is the plugin announcing that its loop has stopped
	// continuing. It is the one hard completion signal a job has.
	signalStopped
	// signalDeclared is signalStopped for the one reason that is a
	// statement rather than an ending: the agent called DL-048's
	// completion tool.
	//
	// @decision(DL-048)
	signalDeclared
	// signalResumed is spov's resume marker. It moves no state (the
	// resume already wrote `running`) but retracts a steer read earlier
	// in the same pass, so a human's "carry on" does not steer the job
	// straight back.
	//
	// @decision(DL-054)
	signalResumed
	// signalReleased is a person answering a permission through spov
	// (MarkerAnswered). It releases a blocked job, but only once follow
	// has found no other request unanswered; nextState never sees it
	// otherwise.
	//
	// @decision(DL-054)
	signalReleased
	// signalLast is one past the last signal, so a test can iterate
	// every signal.
	//
	// @decision(DL-048)
	signalLast
)

// signalFor reads one event. spov's markers are matched by name and
// source, the sandbox's permission and inbox frames by exact name, and a
// turn ending by suffix (TurnEnded).
//
// @decision(DL-046)
func signalFor(e store.Event) jobSignal {
	// The spov markers the supervisor reads back, matched by name and
	// source before the check below drops every other marker.
	// MarkerInterrupted yields signalSteered, which gives an interrupt
	// DL-054's retraction and pause.
	//
	// @decision(DL-053)
	if e.Source == store.SourceSpov {
		switch e.Name {
		case MarkerGap:
			return signalGap
		case MarkerResumed:
			return signalResumed
		case MarkerInterrupted:
			return signalSteered
		// @decision(DL-054)
		case MarkerAnswered:
			return signalReleased
		}
		return signalNone
	}
	// Only the sandbox's own frames say anything about the run; a spov
	// marker is never read back as the session's voice (DL-046).
	if e.Source != store.SourceOpenCode && e.Source != store.SourceEphemeral {
		return signalNone
	}
	// Exact names, never suffixes, because the sandbox chooses them.
	// Tracks internal/server/frames.go's ephemeralNames.
	switch {
	case e.Name == eventPermissionAsked:
		return signalParked
	case e.Name == eventPermissionReplied:
		return signalAnswered
	// A person's message steers on its delivery, which follow reads with
	// the enqueue it joins to (takeoverDelivered), not here.
	case isLoopStopped(e):
		if stopReason(e) == opencode.CompletionReason {
			return signalDeclared
		}
		return signalStopped
	case TurnEnded(e.Name):
		return signalTurnEnded
	}
	return signalNone
}

// isLoopStopped reports whether an event is the plugin announcing that
// its loop has stopped: an inbox item, typed synthetic, whose text starts
// with the marker. It is a guardrail, not a boundary: the agent can post
// such a synthetic itself, so the marker says the loop stopped and never
// that the job completed honestly.
//
// @decision(DL-048) @decision(DL-040)
func isLoopStopped(e store.Event) bool {
	if e.Name != EventInboxEnqueued {
		return false
	}
	if InboxItemType(e.Raw) != "synthetic" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(inboxItemText(e)), opencode.LoopStoppedMarker)
}

// stopReason reads the reason out of a stop marker's JSON payload. It
// fails soft: an unreadable payload returns "", which the caller treats
// as an ordinary stop rather than a declaration.
//
// @decision(DL-048)
func stopReason(e store.Event) string {
	return stopPayloadOf(e).Reason
}

// stopPayload is the part of a stop marker's payload spov reads. Call
// is set when the run ended on a repeated denial (DL-040).
type stopPayload struct {
	Reason string `json:"reason"`
	Call   string `json:"call"`
	// Error is the failed turn's error, with TurnFailedReason (DL-048).
	Error string `json:"error"`
}

// stopPayloadOf reads a stop marker's payload, zero when it cannot.
//
// @decision(DL-048) @decision(DL-040)
func stopPayloadOf(e store.Event) stopPayload {
	text := strings.TrimSpace(inboxItemText(e))
	payload := strings.TrimSpace(strings.TrimPrefix(text, opencode.LoopStoppedMarker))
	// A marker with no payload is an ordinary stop.
	if !strings.HasPrefix(payload, "{") {
		return stopPayload{}
	}
	var got stopPayload
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		return stopPayload{}
	}
	return got
}

// inboxItemText reads the enqueued item's text from
// data.item.payload.text, the path measured in frames_test.go's captured
// frame. It falls back to the projected Text only as a backstop, for a
// frame whose raw form cannot be re-read.
//
// @decision(DL-048) @decision(DL-017)
func inboxItemText(e store.Event) string {
	if len(e.Raw) > 0 {
		var frame map[string]any
		if err := json.Unmarshal(e.Raw, &frame); err == nil {
			if item, ok := findMap(frame, "item", 0); ok {
				if payload, ok := item["payload"].(map[string]any); ok {
					if s, ok := payload["text"].(string); ok && s != "" {
						return s
					}
				}
				// A flattened item, in case the shape changes.
				if s, ok := item["text"].(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return e.Text
}

// takesOver reports whether an enqueued item takes the job over once it
// is delivered: a person's item, typed "user", whose spov metadata does
// not say AfterContinue. The plugin's takesOver applies the same rule
// (TestThePluginAgreesOnWhatTakesOver).
//
// @decision(DL-046) @decision(DL-052)
func takesOver(e store.Event) bool {
	if e.Name != EventInboxEnqueued {
		return false
	}
	return InboxItemType(e.Raw) == "user" && InboxItemAfter(e.Raw) != AfterContinue
}

// InboxItemAfter reads what an enqueued item says follows it, "" when it
// says nothing.
//
// @decision(DL-052)
func InboxItemAfter(raw []byte) string {
	item := inboxItem(raw)
	payload, _ := item["payload"].(map[string]any)
	metadata, _ := payload["metadata"].(map[string]any)
	spov, _ := metadata["spov"].(map[string]any)
	after, _ := spov["after"].(string)
	return after
}

// InboxItemDelivery reads an enqueued item's delivery, "" when it has
// none.
//
// @decision(DL-019)
func InboxItemDelivery(raw []byte) string {
	d, _ := inboxItem(raw)["delivery"].(string)
	return d
}

// InboxID reads the inbox item an inbox event is about (data.inboxID in
// the measured shape), "" when there is none.
//
// @decision(DL-052)
func InboxID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		return ""
	}
	return findString(frame, "inboxID", 0)
}

// findString is findMap for a non-empty string value.
//
// @decision(DL-052)
func findString(v any, key string, depth int) string {
	if depth >= maxProjectDepth {
		return ""
	}
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if s, ok := m[key].(string); ok && s != "" {
		return s
	}
	for _, k := range sortedKeys(m) {
		if child, ok := m[k].(map[string]any); ok {
			if s := findString(child, key, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}

func inboxItem(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		return nil
	}
	item, _ := findMap(frame, "item", 0)
	return item
}

// InboxItemType reads the enqueued item's type out of the raw frame. It
// locates the item object first, because the frame carries several "type"
// fields (the event's own among them). The shape is inferred, not
// measured. An unreadable frame reports "", so it never steers a job.
//
// @decision(DL-046)
func InboxItemType(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	s, _ := inboxItem(raw)["type"].(string)
	return s
}

// findMap returns the first object under key, breadth-first, within the
// same depth budget the rest of the projection uses.
//
// @decision(DL-046)
func findMap(v any, key string, depth int) (map[string]any, bool) {
	if depth >= maxProjectDepth {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	if found, ok := m[key].(map[string]any); ok {
		return found, true
	}
	// Sorted, so two structurally identical frames project the same
	// way: a map's iteration order is not stable.
	for _, k := range sortedKeys(m) {
		if child, ok := m[k].(map[string]any); ok {
			if found, ok := findMap(child, key, depth+1); ok {
				return found, true
			}
		}
	}
	return nil, false
}

// nextState is the state a job in `from` moves to on this signal, and
// whether it moves at all. permission.replied names no principal, so a
// parked job is released only by an explicit act: Resume, or a person's
// answer through spov (signalReleased).
//
// @decision(DL-046) @decision(DL-054)
func nextState(from store.JobState, sig jobSignal) (store.JobState, bool) {
	switch sig {
	case signalReleased:
		if from == store.JobBlocked {
			return store.JobRunning, true
		}
	case signalStopped:
		// The loop has finished. Only a running job moves: a job that
		// is blocked or steered is suspended by something a human has
		// to release, and the plugin's own loop ending does not
		// release it.
		if from == store.JobRunning {
			return store.JobDelivering, true
		}
	case signalDeclared:
		// A declaration also releases a steered job. Blocked is not
		// included: that job is parked on a question nobody answered,
		// so the run is waiting rather than finished.
		//
		// @decision(DL-048)
		if from == store.JobRunning || from == store.JobSteered {
			return store.JobDelivering, true
		}
	case signalGap:
		// A gap moves nothing: what happened during it is unknown. The
		// events after it are read normally.
		return from, false
	case signalParked:
		// Only a job that is actually running can park. A job already
		// blocked stays blocked, and asking again does not re-enter it.
		if from == store.JobRunning {
			return store.JobBlocked, true
		}
	case signalSteered:
		// Only a running job is steered. The machine has no
		// blocked -> steered edge on purpose: someone typing into a
		// parked job is answering it, not taking it over.
		if from == store.JobRunning {
			return store.JobSteered, true
		}
	}
	return from, false
}

// PermissionID reads the request a permission.asked frame is, at
// data.id in the measured shape (spike S-F1), "" when it has none.
//
// @decision(DL-020)
func PermissionID(raw []byte) string {
	var frame struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return ""
	}
	return frame.Data.ID
}

// rawFrame decodes a stored frame for findString, nil when it cannot.
func rawFrame(raw []byte) map[string]any {
	var frame map[string]any
	if json.Unmarshal(raw, &frame) != nil {
		return nil
	}
	return frame
}
