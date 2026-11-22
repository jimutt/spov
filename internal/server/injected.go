package server

import (
	"encoding/json"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
)

// Injected names a synthetic message OpenCode (or a plugin) added to a
// conversation by itself, which spov never sends.
//
// @decision(DL-180)
type Injected struct {
	Kind    string `json:"kind" enum:"shell,instructions,other" doc:"What was added: a shell command's result, a directory's instructions, or anything else"`
	Summary string `json:"summary,omitempty" doc:"OpenCode's one-line description of what it added, absent when it gave none"`
	// Shell is set only on a shell command's result; the message's text
	// is then the command's output alone.
	Shell *ShellResult `json:"shell,omitempty"`
}

// ShellResult is a shell command's end, as OpenCode reported it.
//
// @decision(DL-180)
type ShellResult struct {
	Command  string `json:"command" doc:"The command that ran"`
	State    string `json:"state,omitempty" doc:"How it ended, as OpenCode says: completed, cancelled or error"`
	Exit     *int   `json:"exit,omitempty" doc:"Its exit code, when it exited"`
	ByPerson bool   `json:"by_person,omitempty" doc:"A person ran it from their own client, not the agent"`
}

// syntheticEvent is the frame OpenCode publishes a synthetic message
// with when it does not go through the inbox (a directory's
// instructions, a restart notice).
//
// @decision(DL-180)
const syntheticEvent = "session.synthetic"

// maxInjectedSummary bounds Injected.Summary.
const maxInjectedSummary = 200

// shellPreamble is the line OpenCode puts before a person's own shell
// command's result (spike S-OI1).
const shellPreamble = "The following shell command was executed by the user:\n"

// syntheticParts are the fields of a synthetic message that say who
// added it: what spov sends carries neither metadata nor a description.
type syntheticParts struct {
	Text        string         `json:"text"`
	Description string         `json:"description"`
	Metadata    map[string]any `json:"metadata"`
}

// injectedOf names a synthetic OpenCode added and gives the text to
// serve for it, or returns nil and the text unchanged for spov's own
// and for a subagent's report, which DL-144 names instead.
//
// @decision(DL-180)
func injectedOf(p syntheticParts, text string) (*Injected, string) {
	if len(p.Metadata) == 0 && p.Description == "" {
		return nil, text
	}
	if p.Metadata["source"] == "subagent" {
		return nil, text
	}
	in := &Injected{Kind: "other", Summary: summaryLine(p.Description, maxInjectedSummary)}
	switch {
	case p.Metadata["source"] == "shell":
		in.Kind = "shell"
		in.Shell, text = shellResultOf(p, text)
	case p.Metadata["instruction"] != nil:
		in.Kind = "instructions"
	}
	return in, text
}

// shellResultOf reads a shell command's result: its parts from the
// metadata and the description, and its output from inside OpenCode's
// `<shell …>` wrapper. Text not in the wrapper's shape is kept whole.
//
// @decision(DL-180)
func shellResultOf(p syntheticParts, text string) (*ShellResult, string) {
	r := &ShellResult{Command: p.Description}
	r.State, _ = p.Metadata["state"].(string)
	if exit, ok := p.Metadata["exit"].(float64); ok && exit == math.Trunc(exit) && math.Abs(exit) < 1<<31 {
		n := int(exit)
		r.Exit = &n
	}
	body := text
	if rest, ok := strings.CutPrefix(body, shellPreamble); ok {
		r.ByPerson = true
		body = rest
	}
	rest, ok := strings.CutPrefix(body, `<shell id="`)
	if !ok {
		return r, text
	}
	_, rest, ok = strings.Cut(rest, `" command="`)
	if !ok {
		return r, text
	}
	var command string
	if p.Description != "" && strings.HasPrefix(rest, p.Description+"\">\n") {
		command, rest = p.Description, rest[len(p.Description)+3:]
	} else if command, rest, ok = strings.Cut(rest, "\">\n"); !ok {
		return r, text
	}
	output, ok := strings.CutSuffix(rest, "\n</shell>")
	if !ok {
		return r, text
	}
	r.Command = command
	return r, strings.TrimSpace(output)
}

// summaryLine is s's first line, trimmed and cut to at most n characters.
func summaryLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// messageInjected reads a stored synthetic message's parts, which are
// top level in OpenCode's message.
//
// @decision(DL-180)
func messageInjected(m store.Message) (*Injected, string) {
	if m.Role != "synthetic" || len(m.Raw) == 0 {
		return nil, m.Text
	}
	var p syntheticParts
	if json.Unmarshal(m.Raw, &p) != nil {
		return nil, m.Text
	}
	return injectedOf(p, m.Text)
}

// storedInjected reads an event's synthetic parts: an inbox item's
// payload, or a `session.synthetic` frame's data.
//
// @decision(DL-180)
func storedInjected(ev store.Event, role string) (*Injected, string) {
	if ev.Source != store.SourceOpenCode || role != "synthetic" || len(ev.Raw) == 0 {
		return nil, ev.Text
	}
	var p syntheticParts
	switch ev.Name {
	case jobs.EventInboxEnqueued:
		var frame struct {
			Data struct {
				Item struct {
					Payload syntheticParts `json:"payload"`
				} `json:"item"`
			} `json:"data"`
		}
		if json.Unmarshal(ev.Raw, &frame) != nil {
			return nil, ev.Text
		}
		p = frame.Data.Item.Payload
	case syntheticEvent:
		var frame struct {
			Data syntheticParts `json:"data"`
		}
		if json.Unmarshal(ev.Raw, &frame) != nil {
			return nil, ev.Text
		}
		p = frame.Data
	default:
		return nil, ev.Text
	}
	return injectedOf(p, ev.Text)
}
