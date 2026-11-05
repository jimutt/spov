package jobs

import (
	"encoding/json"
	"strings"
)

// Reading the tool parts out of a stored message, for gate 3. The shape
// was measured by spike S6
// (internal/sandbox/phase4c_permissions_integration_test.go):
//
//	{"type":"tool","name":"shell","executed":false,
//	 "state":{"status":"error",
//	          "error":{"type":"aborted",
//	                   "message":"The user declined this tool call"}}}
//
// It is still read defensively, by key name with a depth budget, because
// a projection that returns nothing would make a blocked job look clean.
//
// @decision(DL-050)

// toolPart is one tool call as a message records it.
//
// @decision(DL-050)
type toolPart struct {
	// Name is the tool, for saying which call was refused. It is
	// reported and never matched on: the action vocabulary is not
	// stable API (DL-040) and an MCP tool's name is open-ended.
	Name string
	// Executed is whether the call ran. A refusal leaves it false.
	Executed bool
	// Status is state.status: "error" on a refusal.
	Status string
	// ErrorType is state.error.type. Several values mean a refusal; see
	// refusalReason.
	ErrorType string
	// ErrorMessage is state.error.message. Gate 3 uses it only to
	// corroborate the correlation in interrupted.go, never on its own.
	//
	// @decision(DL-053)
	ErrorMessage string
	// RanAt is `time.ran`: when the call started, zero if it never did.
	// It tells a call cut off part-way from one stopped before it began;
	// `time.completed` cannot, since an aborted part carries one either way.
	//
	// @decision(DL-053)
	RanAt int64
	// CompletedAt is `time.completed`: when the call stopped, in
	// milliseconds on the sandbox's clock, zero when absent. The interrupt
	// event's `created` is on the same clock, so the two compare directly.
	//
	// @decision(DL-053)
	CompletedAt int64
}

// Refused reports whether this part records a call the agent was not
// allowed to make: an error status with a non-empty error type other than
// errorAtRuntime. Any unrecognised type counts, since the policy and a
// human's reject leave different ones.
//
// @decision(DL-050)
func (p toolPart) Refused() bool {
	if p.Executed || p.Status != toolStatusError {
		return false
	}
	// `executed` is OpenCode's providerExecuted and false for every local
	// tool, so the check above only excludes provider-executed parts. The
	// error type is the whole test: an untyped error is a failed call, not
	// a refusal. Do not remove this as redundant; see DL-050.
	//
	// @decision(DL-050)
	if strings.TrimSpace(p.ErrorType) == "" {
		return false
	}
	// A runtime error is excluded by name, not by an allowlist of refusal
	// types, so an unrecognised type is still a refusal.
	//
	// @decision(DL-050)
	return p.ErrorType != errorAtRuntime
}

// The measured values. Named so the verdict can say WHY it is confident
// about a refusal, never so it can decide whether one happened.
//
// @decision(DL-050) @decision(DL-040)
const (
	// toolStatusError is state.status on a call that did not run.
	toolStatusError = "error"
	// refusalByPolicy is what the plugin's own permission.evaluate
	// deny leaves (spike S3).
	refusalByPolicy = "permission.rejected"
	// refusalByHuman is what a reject through the reply endpoint
	// leaves (spike S6). The message is "The user declined this tool
	// call".
	refusalByHuman = "aborted"
	// errorAtRuntime is what a tool that ran and failed leaves. It is not
	// a refusal.
	//
	// @decision(DL-050)
	errorAtRuntime = "tool.execution"
)

// refusalReason names the refusal path when the type is one of the two
// that were measured, and reports the raw type otherwise. It only words
// the reason; Refused has already decided.
//
// @decision(DL-050)
func refusalReason(p toolPart) string {
	switch p.ErrorType {
	case refusalByPolicy:
		return "the job's permission policy refused it"
	case refusalByHuman:
		return "it was declined through the permission reply"
	case "":
		return "it did not run and reported an error"
	default:
		return "it did not run: " + p.ErrorType
	}
}

// maxPartDepth bounds the walk for the parts array. A message's parts
// sit at `content` in the measured shape; the budget leaves room for an
// envelope around it without inviting a deeper schema.
const maxPartDepth = 4

// toolParts gathers every tool call one stored message records.
//
// The parts array is located by walking for arrays of objects rather
// than at a fixed key: the measured shape calls it `content`, another
// spelling is `parts`, and a message that projects to nothing here
// would silently make every job look unrefused. A part is recognised by
// its own `type: "tool"` rather than by where it was found.
//
// @decision(DL-050)
func toolParts(raw []byte) []toolPart {
	if len(raw) == 0 {
		return nil
	}
	var msg any
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	var out []toolPart
	collectToolParts(msg, 0, &out)
	return out
}

// collectToolParts walks a decoded message gathering tool parts.
func collectToolParts(v any, depth int, out *[]toolPart) {
	if depth > maxPartDepth {
		return
	}
	switch node := v.(type) {
	case map[string]any:
		if kind, _ := node["type"].(string); kind == "tool" {
			*out = append(*out, readToolPart(node))
			// A tool part carries no tool parts. Descending would only
			// find its own state object.
			return
		}
		// Deterministic order, for the same reason
		// internal/server/frames.go sorts: a map's iteration order is
		// not stable, and a verdict that reordered its reasons between
		// reads would be hard to trust.
		for _, k := range sortedKeys(node) {
			collectToolParts(node[k], depth+1, out)
		}
	case []any:
		for _, item := range node {
			collectToolParts(item, depth+1, out)
		}
	}
}

// readToolPart reads one part object.
//
// Every field is optional. A part missing `executed` reads as false,
// which is the safe direction for a verdict: combined with an error
// status it is a refusal, and an absent status means it is not.
func readToolPart(node map[string]any) toolPart {
	p := toolPart{}
	p.Name, _ = node["name"].(string)
	p.Executed, _ = node["executed"].(bool)
	// Read before the state block, which returns early, so a part with no
	// error still gets its times.
	//
	// @decision(DL-053)
	if t, ok := node["time"].(map[string]any); ok {
		if ms, ok := t["ran"].(float64); ok {
			p.RanAt = int64(ms)
		}
		if ms, ok := t["completed"].(float64); ok {
			p.CompletedAt = int64(ms)
		}
	}
	state, ok := node["state"].(map[string]any)
	if !ok {
		return p
	}
	p.Status, _ = state["status"].(string)
	errObj, ok := state["error"].(map[string]any)
	if !ok {
		return p
	}
	p.ErrorType, _ = errObj["type"].(string)
	p.ErrorMessage, _ = errObj["message"].(string)
	return p
}
