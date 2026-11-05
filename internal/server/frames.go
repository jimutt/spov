package server

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// This file reads OpenCode's own shapes: its event-stream frames and the
// bodies of its session and message endpoints. The shapes are inferred, so
// fields are looked up by name across a frame's first levels; the ones
// measured against a sandbox are pinned by the captured frames in
// frames_test.go and recorder_test.go. The recorder is the only caller of
// the projections; `find` is also used by livehandlers.go.
//
// @decision(DL-018) @decision(DL-017)

// durableMarker is the value of the older `durability` string form. The
// shape a real sandbox sends is an object; see isDurable.
//
// @decision(DL-018)
const durableMarker = "durable"

// projectFrame turns one decoded frame of OpenCode's stream into spov's
// own event, or reports that it is not one a transcript wants.
//
// @decision(DL-018) @decision(DL-017)
func projectFrame(frame map[string]any, now time.Time) (LogEvent, bool) {
	name := find(frame, nameKeys)
	if name == "" {
		return LogEvent{}, false
	}
	// A frame is kept if OpenCode's own store keeps it, or if it is one
	// of the few ephemeral ones the job model needs (DL-017).
	if !isDurable(frame) && !isEphemeralKept(name) {
		return LogEvent{}, false
	}
	// A session event is what a session's log carries; server-level
	// ones (server.connected, installation.update-available) belong to
	// nobody's transcript.
	session := find(frame, sessionKeys)
	if session == "" {
		return LogEvent{}, false
	}
	// No Role and no Message here: this output is stored and never sent,
	// so both are re-derived from the row's Raw in logEvent. Set here
	// they would reach nobody.
	//
	// @decision(DL-058) @decision(DL-019) @decision(DL-018)
	return LogEvent{Name: name, Session: session, Text: find(frame, textKeys), At: now}, true
}

// durableSeq reads the frame's position in its conversation's durable
// log: `durable.seq` in the measured shape. Zero when there is none,
// which the store refuses for an OpenCode event.
//
// @decision(DL-017)
func durableSeq(frame map[string]any) int64 {
	d, ok := frame["durable"].(map[string]any)
	if !ok {
		return 0
	}
	if seq, ok := d["seq"].(float64); ok && seq > 0 {
		return int64(seq)
	}
	return 0
}

// ephemeralNames are the non-durable frames spov stores anyway, named
// rather than matched by prefix so deltas and status frames stay out of
// the store. permission.asked and permission.replied tell the supervisor
// a job is parked and that its question was answered. With the three
// form frames they are also what the page reads a pending ask from.
//
// @decision(DL-017) @decision(DL-020)
var ephemeralNames = map[string]bool{
	eventPermissionAsked:   true,
	eventPermissionReplied: true,
	eventFormCreated:       true,
	eventFormReplied:       true,
	eventFormCancelled:     true,
}

// isEphemeralKept reports whether a non-durable frame is one of the few
// spov stores anyway.
//
// @decision(DL-017)
func isEphemeralKept(name string) bool { return ephemeralNames[name] }

var (
	nameKeys    = []string{"event", "type"}
	sessionKeys = []string{"sessionID", "sessionId", "session_id", "session"}
	textKeys    = []string{"text", "content", "message", "title"}
)

// isDurable reports whether a frame belongs to OpenCode's durable log.
// The marker is a `durable` object, so its presence is the test; a
// flattened string form is accepted too rather than dropping frames
// silently.
//
// @decision(DL-018)
func isDurable(frame map[string]any) bool {
	// The measured shape: a `durable` object. Its presence is the marker.
	switch v := frame["durable"].(type) {
	case map[string]any:
		return len(v) > 0
	case string:
		// A server that flattened the object; accepted rather than dropped.
		return v != ""
	}
	// The older `durability` string form, kept as a cheap fallback.
	if d, ok := frame["durability"].(string); ok {
		return d == durableMarker
	}
	return false
}

// maxProjectDepth bounds the search. A user's prompt sits four levels
// down, at `data.item.payload.text`; five leaves one level of slack.
//
// @decision(DL-018)
const maxProjectDepth = 5

// find returns the first string any of keys resolves to, breadth-first
// so a top-level field beats one nested inside the payload.
func find(v any, keys []string) string {
	level := []any{v}
	for depth := 0; depth < maxProjectDepth && len(level) > 0; depth++ {
		var next []any
		for _, node := range level {
			m, ok := node.(map[string]any)
			if !ok {
				continue
			}
			for _, k := range keys {
				if s, ok := m[k].(string); ok && s != "" {
					return s
				}
			}
			// Sorted, so two structurally identical frames project the
			// same way: a map's iteration order is not stable, and
			// sibling objects can both carry a matching key.
			for _, k := range sortedKeys(m) {
				if _, ok := m[k].(map[string]any); ok {
					next = append(next, m[k])
				}
			}
		}
		level = next
	}
	return ""
}

// conversations reads the session listing. OpenCode wraps it in a data
// envelope; the shape below that has not been pinned against a running
// sandbox, so ids are gathered by name rather than by position.
//
// @decision(DL-018)
func conversations(body []byte) (ids []string, titles map[string]string) {
	titles = map[string]string{}
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Data == nil {
		// Some builds answer with a bare array.
		var bare []map[string]any
		if err := json.Unmarshal(body, &bare); err != nil {
			return nil, titles
		}
		envelope.Data = bare
	}
	for _, item := range envelope.Data {
		id, _ := item["id"].(string)
		if id == "" {
			continue
		}
		ids = append(ids, id)
		if t, ok := item["title"].(string); ok {
			titles[id] = t
		}
	}
	return ids, titles
}

// parsedMessage is one message with the raw body it was projected
// from. The id OpenCode gave it is on the embedded view, which the
// recorder keys on and which also reaches a client.
//
// @decision(DL-017)
type parsedMessage struct {
	// Raw is the message object as received, kept so gate 3 can
	// re-project the tool parts the view drops.
	//
	// @decision(DL-017) @decision(DL-050)
	Raw []byte
	MessageView
}

// idKeys names a message's own id. Breadth-first, so the top-level
// `id` wins over any id a content part carries.
var idKeys = []string{"id"}

// parseMessages projects one conversation's messages. Every text a
// message carries is gathered in the order it appears, because OpenCode
// splits a reply across parts and the transcript wants the reply, not
// the parts.
//
// @decision(DL-018) @decision(DL-017)
func parseMessages(body []byte) []parsedMessage {
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Data == nil {
		var bare []json.RawMessage
		if err := json.Unmarshal(body, &bare); err != nil {
			return nil
		}
		envelope.Data = bare
	}
	out := make([]parsedMessage, 0, len(envelope.Data))
	for _, raw := range envelope.Data {
		var m any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		text := joinText(collectText(m, 0))
		role := find(m, roleKeys)
		if text == "" && role == "" {
			continue
		}
		view := MessageView{ID: find(m, idKeys), Role: role, Text: text}
		if ms := findMillis(m, createdKeys); ms > 0 {
			at := time.UnixMilli(ms).UTC()
			view.At = &at
		}
		out = append(out, parsedMessage{Raw: raw, MessageView: view})
	}
	return out
}

// roleKeys names who spoke. OpenCode calls it `type` ("user",
// "assistant") and sends no `role`. `role` is listed first, and the
// breadth-first search lets a top-level `type` win over a content part's.
//
// @decision(DL-018)
var roleKeys = []string{"role", "type"}

// createdKeys names the message's own timestamp, `time.created` in epoch
// milliseconds, so a client can lay messages and live events on one
// timeline.
//
// @decision(DL-018)
var createdKeys = []string{"created"}

// maxCollectDepth bounds the walk over one message.
const maxCollectDepth = 6

// collectText gathers every text field under v, walking objects and the
// parts arrays a message is split into.
func collectText(v any, depth int) []string {
	if depth > maxCollectDepth {
		return nil
	}
	switch node := v.(type) {
	case map[string]any:
		// Reasoning and tool parts carry `text` but are not what the speaker
		// said: reasoning is the model's working, and a tool part holds the
		// tool's output (a whole file, for a `read`), which the event row
		// already reports.
		//
		// @decision(DL-058) @decision(DL-018)
		if kind, ok := node["type"].(string); ok && (kind == "reasoning" || kind == "tool") {
			return nil
		}
		var out []string
		if s, ok := node["text"].(string); ok && s != "" {
			out = append(out, s)
		}
		// Sorted keys, so a transcript does not reshuffle between reads.
		for _, k := range sortedKeys(node) {
			if k == "text" {
				continue
			}
			out = append(out, collectText(node[k], depth+1)...)
		}
		return out
	case []any:
		var out []string
		for _, item := range node {
			out = append(out, collectText(item, depth+1)...)
		}
		return out
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// findMillis returns the first number any of keys resolves to, breadth
// first like find. JSON numbers decode as float64, and an epoch in
// milliseconds is well inside float64's exact-integer range.
func findMillis(v any, keys []string) int64 {
	level := []any{v}
	for depth := 0; depth < maxProjectDepth && len(level) > 0; depth++ {
		var next []any
		for _, node := range level {
			m, ok := node.(map[string]any)
			if !ok {
				continue
			}
			for _, k := range keys {
				if f, ok := m[k].(float64); ok && f > 0 {
					return int64(f)
				}
			}
			for _, k := range sortedKeys(m) {
				if _, ok := m[k].(map[string]any); ok {
					next = append(next, m[k])
				}
			}
		}
		level = next
	}
	return 0
}

func joinText(parts []string) string {
	return strings.Join(parts, "\n")
}
