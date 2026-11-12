package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// LogEvent is one item of a session's activity: a durable frame the
// recorder stored, or one of spov's own markers. It is spov's own shape;
// OpenCode's event schema is never forwarded.
//
// @decision(DL-018)
type LogEvent struct {
	// Name is the OpenCode event this was projected from, carried as an
	// opaque label rather than a contract. A client groups on it and
	// must tolerate one it has never seen. spov's own markers are named
	// spov.*.
	Name string `json:"name"`
	// Session is the OpenCode session the event belongs to. One spov
	// session is one sandbox, and a sandbox may hold several
	// conversations; a marker belongs to none of them and carries no
	// session.
	Session string `json:"session,omitempty"`
	// Source says who wrote the event: "opencode" for a frame the
	// sandbox sent, "ephemeral" for one of the few non-durable frames
	// spov keeps (DL-017), "spov" for one of spov's own markers. A client keys
	// on this, never on Name, to draw a marker as spov's.
	//
	// @decision(DL-058)
	Source string `json:"source" enum:"opencode,ephemeral,spov"`
	// Role is who spoke, where the frame says: "user" for a person's
	// prompt, "synthetic" for one of spov's own, "assistant" for the
	// agent. Empty when the frame names nobody, which is most of them.
	//
	// @decision(DL-058)
	Role string `json:"role,omitempty"`
	// Message is OpenCode's id for the message this event enqueued
	// (msg_…), where the event enqueued one; empty otherwise. Named like
	// Session, not message_id.
	//
	// @decision(DL-019)
	Message string `json:"message,omitempty"`
	// Delivery and After are a person's queued message's delivery
	// (queue or steer) and what follows it on a job (continue or
	// handover), on its enqueue only. Together with Message on the
	// delivered and cancelled events, they are what a client derives a
	// message's status from.
	//
	// @decision(DL-019)
	Delivery string `json:"delivery,omitempty"`
	After    string `json:"after,omitempty"`
	// Text is the human-readable content the event carried, where it
	// carried any; a pure activity marker has none.
	Text string `json:"text,omitempty"`
	// Call is the tool call a session.tool.* event belongs to (call_…),
	// so a client can join a call's input, start and result into one
	// line. Empty on every other event.
	//
	// @decision(DL-058)
	Call string `json:"call,omitempty"`
	// Tool is the tool's name, on the one event of a call that carries
	// it (session.tool.input.started). Empty otherwise.
	//
	// @decision(DL-058)
	Tool string `json:"tool,omitempty"`
	// Child is the OpenCode session a tool call started, on the call's
	// result: a subagent's id, from `metadata.sessionID`. A client takes
	// it as a hint and trusts it only for a session it knows to be a
	// child of this one. Empty on every other event.
	//
	// @decision(DL-112)
	Child string `json:"child,omitempty"`
	// Ask is a permission request or question form, or its answer, on
	// the five ask frames only.
	//
	// @decision(DL-020)
	Ask *AskEvent `json:"ask,omitempty"`
	// Model is the model a session.model.selected frame names, as
	// provider/model; empty on every other event.
	//
	// @decision(DL-014)
	Model string    `json:"model,omitempty"`
	At    time.Time `json:"at"`
	// Pos is this event's position in the session's stored log. It is a
	// cursor, not something to show: pass the last one seen back as
	// `after` and the stream resumes exactly where it stopped.
	//
	// @decision(DL-018)
	Pos int64 `json:"pos"`
}

// logInput is one session's log, optionally resumed.
//
// @decision(DL-018)
type logInput struct {
	Ref   string `path:"ref" doc:"Session ID or name"`
	After int64  `query:"after" minimum:"0" doc:"Resume after this log position; omit to replay from the beginning"`
}

// registerLive serves the two live read surfaces: the caller's session
// list, and one session's activity.
//
// @decision(DL-018)
func (s *Server) registerLive() {
	// Registered before the {ref} routes read like a conflict and are
	// not one: this path has one segment where those have two.
	registerSSE(s.api, huma.Operation{
		OperationID: "session-state-stream", Method: http.MethodGet, Path: "/api/v1/sessions/events",
		Summary: "Stream the caller's sessions as their state changes", Tags: []string{"sessions"},
	}, map[string]any{
		"sessions": SessionsEvent{},
		"end":      EndEvent{},
		"error":    ErrorEvent{},
	}, func(ctx context.Context, in *sessionStreamInput, send sse.Sender) {
		s.streamSessions(ctx, in, send)
	})

	registerSSE(s.api, huma.Operation{
		OperationID: "session-log", Method: http.MethodGet, Path: "/api/v1/sessions/{ref}/log",
		Summary: "Stream a session's recorded activity, from a position or from the beginning", Tags: []string{"sessions"},
	}, map[string]any{
		"log":   LogEvent{},
		"end":   EndEvent{},
		"error": ErrorEvent{},
	}, func(ctx context.Context, in *logInput, send sse.Sender) {
		s.streamLog(ctx, in, send)
	})

	// Paired with the GET below rather than given a surface of its own:
	// one address for a session's messages, read and written.
	//
	// @decision(DL-019)
	register(s.api, huma.Operation{
		OperationID: "send-message", Method: http.MethodPost, Path: "/api/v1/sessions/{ref}/messages",
		Summary: "Send a message into a running session", DefaultStatus: http.StatusAccepted,
		Tags: []string{"sessions"}, Errors: []int{http.StatusConflict, http.StatusTooManyRequests},
	}, func(ctx context.Context, in *steerInput) (*steerOutput, error) {
		sent, err := s.svc.Steer(ctx, principal(ctx), in.Ref, in.Body)
		if err != nil {
			return nil, toSteerHumaError(err)
		}
		out := &steerOutput{Body: sent}
		// Admission is not action: the sandbox has taken the item
		// durably, which is not the model having read it.
		out.CacheControl = "no-store"
		return out, nil
	})

	// Answered once the sandbox took the request; whether the message was
	// still pending is the stored session.inbox.cancelled.
	//
	// @decision(DL-019)
	register(s.api, huma.Operation{
		OperationID: "cancel-message", Method: http.MethodDelete, Path: "/api/v1/sessions/{ref}/messages/{id}",
		Summary: "Cancel a message the session has not read yet", DefaultStatus: http.StatusAccepted,
		Tags: []string{"sessions"}, Errors: []int{http.StatusConflict, http.StatusTooManyRequests},
	}, func(ctx context.Context, in *cancelMessageInput) (*cancelMessageOutput, error) {
		done, err := s.svc.CancelMessage(ctx, principal(ctx), in.Ref, in.ID, in.Conversation)
		if err != nil {
			return nil, toSteerHumaError(err)
		}
		out := &cancelMessageOutput{Body: done}
		out.CacheControl = "no-store"
		return out, nil
	})

	// Its own address rather than a field on the send: stopping a turn
	// is not a kind of message, and a reader presses it when they want
	// nothing more said.
	//
	// @decision(DL-053)
	register(s.api, huma.Operation{
		OperationID: "interrupt-session", Method: http.MethodPost, Path: "/api/v1/sessions/{ref}/interrupt",
		Summary: "Interrupt the turn a session is running", Tags: []string{"sessions"},
		Errors: []int{http.StatusConflict, http.StatusTooManyRequests},
	}, func(ctx context.Context, in *interruptInput) (*interruptOutput, error) {
		stopped, err := s.svc.Interrupt(ctx, principal(ctx), in.Ref, in.Body)
		if err != nil {
			return nil, toInterruptHumaError(err)
		}
		out := &interruptOutput{Body: stopped}
		out.CacheControl = "no-store"
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "session-messages", Method: http.MethodGet, Path: "/api/v1/sessions/{ref}/messages",
		Summary: "The session's conversation so far", Tags: []string{"sessions"},
	}, func(ctx context.Context, in *refInput) (*messagesOutput, error) {
		sess, err := s.svc.Get(ctx, principal(ctx), in.Ref)
		if err != nil {
			return nil, toHumaError(err)
		}
		conv, warnings, truncated, err := s.transcript(ctx, sess)
		if err != nil {
			return nil, toHumaError(err)
		}
		out := &messagesOutput{}
		out.Body.Conversations = conv
		out.Body.Warnings = warnings
		// @decision(DL-131)
		out.Body.Truncated = truncated
		// The rows move while the session runs, so this is never a
		// cacheable representation of it.
		out.CacheControl = "no-store"
		return out, nil
	})
}

// streamLog replays a session's stored log from in.After and then
// follows it. The session is resolved through Service.Get, so a
// non-owner gets 404. It reads the store only, never the sandbox.
//
// @decision(DL-018)
func (s *Server) streamLog(ctx context.Context, in *logInput, send sseSender) {
	p := principal(ctx)
	sess, err := s.svc.Get(ctx, p, in.Ref)
	if err != nil {
		_ = send.Data(ErrorEvent{Problem: problemOf(err)})
		return
	}
	// The feed goroutine outlives this function otherwise.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	updates, err := s.events.Subscribe(ctx, sess.OrgID, sess.ID, in.After)
	if err != nil {
		_ = send.Data(ErrorEvent{Problem: problemOf(err)})
		return
	}
	names := &conversationNames{byID: map[string]string{}, list: func(ctx context.Context) ([]store.Conversation, error) {
		return s.deps.Store.Conversations().ListConversations(ctx, sess.OrgID, sess.ID)
	}}

	life := time.NewTimer(s.streamLifetime)
	defer life.Stop()
	beat := time.NewTicker(s.heartbeat)
	defer beat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			// An orderly close: the client reopens with the last
			// position it saw and misses nothing.
			_ = send.Data(EndEvent{Reason: "lifetime", At: s.deps.Now()})
			return
		case <-beat.C:
			if err := send.Comment("keepalive"); err != nil {
				return
			}
		case batch, ok := <-updates:
			if !ok {
				return
			}
			names.begin()
			for _, ev := range batch.Events {
				if err := send.Data(s.logEvent(ctx, names, ev)); err != nil {
					return
				}
			}
			// Caught up and quiet: end the stream if the session can no
			// longer produce events. Asked only on a quiet tick, so a
			// busy session costs no extra read per batch.
			if batch.CaughtUp && len(batch.Events) == 0 && s.sessionEnded(ctx, p, sess.ID) {
				_ = send.Data(EndEvent{Reason: "stopped", At: s.deps.Now()})
				return
			}
		}
	}
}

// logEvent projects a stored event onto the wire shape.
//
// @decision(DL-018)
func (s *Server) logEvent(ctx context.Context, names *conversationNames, ev store.Event) LogEvent {
	call, tool := storedToolCall(ev)
	role := storedRole(ev)
	delivery, after := storedInbox(ev, role)
	return LogEvent{
		Name:     ev.Name,
		Session:  names.name(ctx, ev.ConversationID),
		Source:   string(ev.Source),
		Role:     role,
		Message:  storedMessage(ev),
		Delivery: delivery,
		After:    after,
		Text:     ev.Text,
		Call:     call,
		Tool:     tool,
		Child:    storedChild(ev),
		Ask:      storedAsk(ev),
		Model:    storedModel(ev),
		At:       ev.At,
		Pos:      ev.Pos,
	}
}

// storedRole re-derives who spoke from the event's raw frame; store.Event
// has no role column. It must be set here, not in projectFrame, whose
// output is stored and never sent. Only inbox events are parsed: they
// are the one name where a person and spov share an event.
//
// @decision(DL-058) @decision(DL-018)
func storedRole(ev store.Event) string {
	if ev.Name != jobs.EventInboxEnqueued || len(ev.Raw) == 0 {
		return ""
	}
	return jobs.InboxItemType(ev.Raw)
}

// inboxIDKeys names the enqueued message's id. Do not add "id" (the
// frame's own evt_… id), "assistantMessageID" or "messageID": each
// names something other than the enqueued message; see DL-019.
//
// @decision(DL-019)
var inboxIDKeys = []string{"inboxID"}

// storedMessage re-derives the enqueued message's id from the event's
// raw frame, as storedRole does for the role. It decodes separately so
// storedRole can keep using the supervisor's own jobs.InboxItemType.
//
// @decision(DL-019) @decision(DL-018)
func storedMessage(ev store.Event) string {
	if !namesAMessage(ev) || len(ev.Raw) == 0 {
		return ""
	}
	var frame map[string]any
	if err := json.Unmarshal(ev.Raw, &frame); err != nil {
		return ""
	}
	return find(frame, inboxIDKeys)
}

// namesAMessage reports the inbox events whose inboxID is the message
// they are about: its enqueue, and from the sandbox its delivery and
// cancellation.
//
// @decision(DL-019)
func namesAMessage(ev store.Event) bool {
	switch ev.Name {
	case jobs.EventInboxEnqueued:
		return true
	case jobs.EventInboxDelivered, jobs.EventInboxCancelled:
		return ev.Source == store.SourceOpenCode
	}
	return false
}

// storedInbox re-derives a person's enqueued message's delivery and what
// follows it. Only a user item's: spov's own items have no status to
// show.
//
// @decision(DL-019)
func storedInbox(ev store.Event, role string) (delivery, after string) {
	if ev.Name != jobs.EventInboxEnqueued || role != "user" {
		return "", ""
	}
	return jobs.InboxItemDelivery(ev.Raw), jobs.InboxItemAfter(ev.Raw)
}

// toolEventPrefix names the events storedToolCall reads.
const toolEventPrefix = "session.tool."

// toolStartedEvent is the one tool event that names its tool.
const toolStartedEvent = toolEventPrefix + "input.started"

// storedToolCall re-derives a tool event's call id, and on
// toolStartedEvent its tool's name, from `data.id` and `data.name` in the
// raw frame. It reads tokens and stops once it has what the name can
// carry, so a tool.success holding a large output is not decoded past
// its id. An OpenCode event only: spov's markers are never parsed.
//
// @decision(DL-058) @decision(DL-018)
func storedToolCall(ev store.Event) (call, tool string) {
	if ev.Source != store.SourceOpenCode || !strings.HasPrefix(ev.Name, toolEventPrefix) || len(ev.Raw) == 0 {
		return "", ""
	}
	wantName := ev.Name == toolStartedEvent
	dec := json.NewDecoder(bytes.NewReader(ev.Raw))
	if !openObject(dec) {
		return "", ""
	}
	for dec.More() {
		key, ok := objectKey(dec)
		if !ok {
			return "", ""
		}
		if key != "data" {
			if skipValue(dec) != nil {
				return "", ""
			}
			continue
		}
		if !openObject(dec) {
			return "", ""
		}
		for dec.More() {
			k, ok := objectKey(dec)
			if !ok {
				return call, tool
			}
			var v any
			if k == "id" || k == "name" {
				if dec.Decode(&v) != nil {
					return call, tool
				}
				if s, ok := v.(string); ok {
					if k == "id" {
						call = s
					} else if wantName {
						tool = s
					}
				}
				if call != "" && (!wantName || tool != "") {
					return call, tool
				}
				continue
			}
			if skipValue(dec) != nil {
				return call, tool
			}
		}
		return call, tool
	}
	return "", ""
}

// metadataKey is the key a tool event's result carries its metadata
// under.
var metadataKey = []byte(`"metadata"`)

// storedChild re-derives the session a tool call started from the
// result's `metadata.sessionID` (spike S-SA1: a `subagent` call's
// success names its child there, whether the call is done or a
// background one is still running). The metadata follows the output in
// the frame, so it is found from the end of the raw bytes and only it is
// decoded; a large output is never walked. An OpenCode tool event only.
// A frame that does not read this way names no child, and the page then
// places the call by its span.
//
// @decision(DL-112) @decision(DL-110)
func storedChild(ev store.Event) string {
	if ev.Source != store.SourceOpenCode || !strings.HasPrefix(ev.Name, toolEventPrefix) {
		return ""
	}
	i := bytes.LastIndex(ev.Raw, metadataKey)
	if i < 0 {
		return ""
	}
	rest := bytes.TrimLeft(ev.Raw[i+len(metadataKey):], " \t\r\n")
	if len(rest) == 0 || rest[0] != ':' {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(rest[1:]))
	if !openObject(dec) {
		return ""
	}
	for dec.More() {
		key, ok := objectKey(dec)
		if !ok {
			return ""
		}
		if key != "sessionID" {
			if skipValue(dec) != nil {
				return ""
			}
			continue
		}
		var id string
		if dec.Decode(&id) != nil {
			return ""
		}
		return id
	}
	return ""
}

// openObject consumes a `{` and reports whether that is what came next.
func openObject(dec *json.Decoder) bool {
	t, err := dec.Token()
	d, ok := t.(json.Delim)
	return err == nil && ok && d == '{'
}

// objectKey reads the next key of the object being walked.
func objectKey(dec *json.Decoder) (string, bool) {
	t, err := dec.Token()
	k, ok := t.(string)
	return k, err == nil && ok
}

// skipValue consumes one value without keeping it.
func skipValue(dec *json.Decoder) error {
	var skip json.RawMessage
	return dec.Decode(&skip)
}

// sessionEnded reports whether the session can still produce events. A
// read that fails is not an end: the stream keeps its lifetime bound and
// the next tick asks again.
//
// @decision(DL-018)
func (s *Server) sessionEnded(ctx context.Context, p auth.Principal, id string) bool {
	sess, err := s.svc.Get(ctx, p, id)
	switch {
	case err == nil:
		return !sess.State.Live()
	case errors.Is(err, store.ErrNotFound):
		// Deleted while being watched.
		return true
	default:
		if ctx.Err() == nil {
			s.deps.Warn(fmt.Sprintf("session log %s: %s", id, redact.Error(err)))
		}
		return false
	}
}

// conversationNames resolves a stored conversation to the OpenCode
// session id a client knows it by. The map is filled once per stream and
// refreshed for an id it has never seen, which is a conversation started
// after the stream opened.
//
// @decision(DL-018)
type conversationNames struct {
	list func(ctx context.Context) ([]store.Conversation, error)
	byID map[string]string
	// refreshed bounds the map to one re-read per batch, so a store
	// that is refusing reads costs one attempt rather than one per
	// event in a page of two hundred.
	refreshed bool
}

// begin opens a batch, allowing one refresh within it.
func (c *conversationNames) begin() { c.refreshed = false }

func (c *conversationNames) name(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	if name, ok := c.byID[id]; ok {
		return name
	}
	if c.refreshed {
		return ""
	}
	c.refreshed = true
	convs, err := c.list(ctx)
	if err != nil {
		// Not remembered: a read that failed says nothing about the
		// conversation, and the next batch asks again.
		return ""
	}
	for _, conv := range convs {
		c.byID[conv.ID] = conv.OpenCodeID
	}
	if _, ok := c.byID[id]; !ok {
		// Remembered as unresolved, so one id that cannot be named does
		// not re-read the conversations for every event after it.
		c.byID[id] = ""
	}
	return c.byID[id]
}
