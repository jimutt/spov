package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// Answering the agent's asks: a permission request or a question form.
// Like a message, a once-per-human-act write through the server, and
// only for an ask the store holds; whether it was answered is read from
// the stored frames, never from the sandbox's 204.
//
// @decision(DL-020)

// The form frames the recorder keeps by name (ephemeralNames).
//
// @decision(DL-020)
const (
	eventPermissionAsked   = "permission.asked"
	eventPermissionReplied = "permission.replied"
	eventFormCreated       = "form.created"
	eventFormReplied       = "form.replied"
	eventFormCancelled     = "form.cancelled"
)

// The two decisions a reader may give. OpenCode's third, "always",
// saves the permission beyond the request and is refused.
//
// @decision(DL-020)
const (
	DecisionOnce   = "once"
	DecisionReject = "reject"
)

// The prefixes of OpenCode's own ids, which an ask's id must carry.
//
// @decision(DL-020)
const (
	PermissionIDPrefix = "per_"
	FormIDPrefix       = "frm_"
)

// The audit markers of the three acts. Each names who acted and what
// they decided, never the answer's text.
//
// @decision(DL-020) @decision(DL-058)
const (
	MarkerPermissionReplied = store.SpovNamespace + "session.permission.replied"
	MarkerFormAnswered      = store.SpovNamespace + "session.form.answered"
	MarkerFormDismissed     = store.SpovNamespace + "session.form.dismissed"
)

// ErrNoSuchAsk is an id the session's store holds no ask for. A 404,
// worded apart from "no such session".
//
// @decision(DL-020)
var ErrNoSuchAsk = errors.New("server: this session has no such request")

// PermissionReply answers one permission request.
//
// @decision(DL-020)
type PermissionReply struct {
	Decision string `json:"decision" enum:"once,reject" doc:"once allows this one request; reject refuses it"`
	// Message reaches the model with a reject, and is refused otherwise.
	Message string `json:"message,omitempty" maxLength:"16384" doc:"With a reject only: what the agent should do instead"`
}

// FormAnswer answers one question form, keyed by its fields' keys.
//
// @decision(DL-020)
type FormAnswer struct {
	Answer map[string]any `json:"answer" doc:"The answer, keyed by field key: a string, a number, a boolean or a list of strings"`
}

// AnsweredAsk is an answer the sandbox took. It does not say the ask
// was still pending.
//
// @decision(DL-020)
type AnsweredAsk struct {
	ID           string `json:"id"`
	Conversation string `json:"conversation" doc:"The OpenCode conversation the request was asked in"`
	Decision     string `json:"decision" enum:"once,reject,answered,dismissed"`
}

// ReplyPermission answers a permission request the session asked.
//
// @decision(DL-020) @decision(DL-054)
func (s *Service) ReplyPermission(ctx context.Context, p auth.Principal, ref, id string, req PermissionReply) (AnsweredAsk, error) {
	if err := validAskID(id, PermissionIDPrefix); err != nil {
		return AnsweredAsk{}, err
	}
	decision := strings.TrimSpace(req.Decision)
	message := strings.TrimSpace(req.Message)
	switch decision {
	case DecisionOnce:
		if message != "" {
			return AnsweredAsk{}, fmt.Errorf("%w: a message goes only with a reject", ErrInvalidRequest)
		}
	case DecisionReject:
		if len(message) > MaxMessageBytes {
			return AnsweredAsk{}, fmt.Errorf("%w: the message is %d bytes; the limit is %d",
				ErrInvalidRequest, len(message), MaxMessageBytes)
		}
	default:
		return AnsweredAsk{}, fmt.Errorf("%w: decision %q is neither %s nor %s; a permission is saved for good only from an attached TUI",
			ErrInvalidRequest, decision, DecisionOnce, DecisionReject)
	}
	body := map[string]any{"decision": decision}
	if message != "" {
		body["message"] = message
	}
	sess, conversation, err := s.askOf(ctx, p, ref, eventPermissionAsked, id)
	if err != nil {
		return AnsweredAsk{}, err
	}
	err = s.answerAsk(ctx, p, sess, func(conn Connection) error {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		_, err = sandboxPostJSON(ctx, conn, askURL(conn, conversation, "permission", id)+"/reply", raw)
		return err
	})
	if err != nil {
		return AnsweredAsk{}, err
	}
	word := map[string]string{DecisionOnce: "allowed once", DecisionReject: "rejected"}[decision]
	s.recordAsk(ctx, sess, MarkerPermissionReplied, "permission "+id+" "+word+" by "+cancelledBy(p), nil)
	s.markJobAnswered(ctx, p, sess, id)
	return AnsweredAsk{ID: id, Conversation: conversation, Decision: decision}, nil
}

// AnswerForm answers a question form the session asked.
//
// @decision(DL-020)
func (s *Service) AnswerForm(ctx context.Context, p auth.Principal, ref, id string, req FormAnswer) (AnsweredAsk, error) {
	if err := validAskID(id, FormIDPrefix); err != nil {
		return AnsweredAsk{}, err
	}
	if err := validAnswer(req.Answer); err != nil {
		return AnsweredAsk{}, err
	}
	sess, conversation, err := s.askOf(ctx, p, ref, eventFormCreated, id)
	if err != nil {
		return AnsweredAsk{}, err
	}
	raw, err := json.Marshal(map[string]any{"answer": req.Answer})
	if err != nil {
		return AnsweredAsk{}, fmt.Errorf("encoding the answer: %w", err)
	}
	err = s.answerAsk(ctx, p, sess, func(conn Connection) error {
		_, err := sandboxPostJSON(ctx, conn, askURL(conn, conversation, "form", id)+"/reply", raw)
		return err
	})
	if err != nil {
		return AnsweredAsk{}, err
	}
	s.recordAsk(ctx, sess, MarkerFormAnswered, "form "+id+" answered by "+cancelledBy(p), nil)
	return AnsweredAsk{ID: id, Conversation: conversation, Decision: "answered"}, nil
}

// DismissForm dismisses a question form the session asked. The agent's
// turn ends with it (spike S-F1).
//
// @decision(DL-020)
func (s *Service) DismissForm(ctx context.Context, p auth.Principal, ref, id string) (AnsweredAsk, error) {
	if err := validAskID(id, FormIDPrefix); err != nil {
		return AnsweredAsk{}, err
	}
	sess, conversation, err := s.askOf(ctx, p, ref, eventFormCreated, id)
	if err != nil {
		return AnsweredAsk{}, err
	}
	err = s.answerAsk(ctx, p, sess, func(conn Connection) error {
		return sandboxDelete(ctx, conn, askURL(conn, conversation, "form", id))
	})
	if err != nil {
		return AnsweredAsk{}, err
	}
	s.recordAsk(ctx, sess, MarkerFormDismissed, "form "+id+" dismissed by "+cancelledBy(p), nil)
	return AnsweredAsk{ID: id, Conversation: conversation, Decision: "dismissed"}, nil
}

// askOf finds a running session the caller may act on and the stored
// ask with this id, returning the conversation it was asked in. The
// conversation comes from the stored row, never from the caller or the
// frame.
//
// @decision(DL-020)
func (s *Service) askOf(ctx context.Context, p auth.Principal, ref, name, id string) (store.Session, string, error) {
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return store.Session{}, "", err
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return store.Session{}, "", fmt.Errorf("%w (state %s)", ErrSessionNotRunning, sess.State)
	}
	if err := s.answerableJob(ctx, sess); err != nil {
		return store.Session{}, "", err
	}
	// The key and the value together, as the compact frame spells them.
	ev, err := s.deps.Store.Conversations().FindEvent(ctx, sess.OrgID, sess.ID, name, []byte(`"id":"`+id+`"`))
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, "", fmt.Errorf("%w: %s", ErrNoSuchAsk, id)
	}
	if err != nil {
		return store.Session{}, "", err
	}
	// The bytes matched; the frame must also be this ask, from the sandbox.
	if ev.Source != store.SourceEphemeral || askedID(name, ev.Raw) != id {
		return store.Session{}, "", fmt.Errorf("%w: %s", ErrNoSuchAsk, id)
	}
	convs, err := s.deps.Store.Conversations().ListConversations(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return store.Session{}, "", err
	}
	for _, c := range convs {
		if c.ID == ev.ConversationID && c.OpenCodeID != "" {
			return sess, c.OpenCodeID, nil
		}
	}
	return store.Session{}, "", fmt.Errorf("%w: %s", ErrNoSuchAsk, id)
}

// answerableJob refuses an answer on a job whose run is over or not yet
// begun: an allow there would run a tool while the job is judged. A
// blocked job is exactly the one to answer.
//
// @decision(DL-020) @decision(DL-052)
func (s *Service) answerableJob(ctx context.Context, sess store.Session) error {
	job, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if job.State == store.JobBlocked {
		return nil
	}
	if why, ok := jobRefusesAMessage(job.State); ok {
		return fmt.Errorf("%w: the job is %s, so %s", ErrJobNotSteerable, job.State, why)
	}
	return nil
}

// askedID reads which ask a stored frame is: a permission at data.id, a
// form at data.form.id (spike S-F1).
//
// @decision(DL-020)
func askedID(name string, raw []byte) string {
	if name == eventPermissionAsked {
		return jobs.PermissionID(raw)
	}
	var frame struct {
		Data struct {
			Form struct {
				ID string `json:"id"`
			} `json:"form"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return ""
	}
	return frame.Data.Form.ID
}

// answerAsk takes a send's two bounds, then runs the write over the
// session's cached connection and nudges the recorder.
//
// @decision(DL-020) @decision(DL-019) @decision(DL-030)
func (s *Service) answerAsk(ctx context.Context, p auth.Principal, sess store.Session, write func(Connection) error) error {
	if err := s.mintBudgetLeft(ctx, sess, ErrTooManyMessages); err != nil {
		return err
	}
	if !s.messages.take(sess.ID, s.deps.Now()) {
		return ErrTooManyMessages
	}
	if err := s.withConnection(ctx, p, sess, write); err != nil {
		return err
	}
	// @decision(DL-017)
	if s.OnStart != nil {
		s.OnStart(sess)
	}
	return nil
}

// askURL is one ask of a conversation: kind is "permission" or "form".
//
// @decision(DL-020)
func askURL(conn Connection, conversation, kind, id string) string {
	return conn.URL + "/api/session/" + url.PathEscape(conversation) + "/" + kind + "/" + url.PathEscape(id)
}

// validAskID checks an ask id's prefix, length and character set, as a
// message id is checked.
//
// @decision(DL-020)
func validAskID(id, prefix string) error {
	if !strings.HasPrefix(id, prefix) || len(id) > MaxMessageIDBytes {
		return fmt.Errorf("%w: a request id starts with %q and is at most %d bytes", ErrInvalidRequest, prefix, MaxMessageIDBytes)
	}
	for _, r := range id {
		ok := r == '_' || r == '-' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return fmt.Errorf("%w: a request id is letters, digits, %q and %q", ErrInvalidRequest, "_", "-")
		}
	}
	return nil
}

// validAnswer checks an answer's shape, OpenCode's Form.Value: each
// value a string, a number, a boolean or a list of strings, the whole
// bounded like a message. Whether it fits the form is the sandbox's to
// say.
//
// @decision(DL-020)
func validAnswer(answer map[string]any) error {
	if answer == nil {
		return fmt.Errorf("%w: an answer is an object keyed by field", ErrInvalidRequest)
	}
	for key, v := range answer {
		switch v := v.(type) {
		case string, float64, bool:
		case []any:
			for _, item := range v {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("%w: field %q: a list holds only strings", ErrInvalidRequest, key)
				}
			}
		default:
			return fmt.Errorf("%w: field %q is not a string, number, boolean or list of strings", ErrInvalidRequest, key)
		}
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return fmt.Errorf("%w: the answer cannot be encoded", ErrInvalidRequest)
	}
	if len(raw) > MaxMessageBytes {
		return fmt.Errorf("%w: the answer is %d bytes; the limit is %d", ErrInvalidRequest, len(raw), MaxMessageBytes)
	}
	return nil
}

// recordAsk writes an act's audit marker; like recordSteer, a failure
// only warns.
//
// @decision(DL-020) @decision(DL-019)
func (s *Service) recordAsk(ctx context.Context, sess store.Session, name, text string, raw []byte) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SandboxReadTimeout)
	defer cancel()
	now := s.deps.Now()
	_, err := s.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: name, Text: text, At: now, Raw: raw,
	}})
	if err != nil {
		s.deps.Warn(fmt.Sprintf("answer %s: recording the act: %s", sess.ID, redact.Error(err)))
	}
}

// markJobAnswered writes jobs.MarkerAnswered when the session is a
// job's, which the supervisor reads as a release of a blocked job. A
// failure warns: the job stays blocked and Resume still releases it.
//
// @decision(DL-054)
func (s *Service) markJobAnswered(ctx context.Context, p auth.Principal, sess store.Session, id string) {
	if _, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.deps.Warn(fmt.Sprintf("answer %s: reading the job: %s", sess.ID, redact.Error(err)))
		}
		return
	}
	raw, _ := json.Marshal(map[string]string{"requestID": id})
	s.recordAsk(ctx, sess, jobs.MarkerAnswered, "permission "+id+" answered by "+cancelledBy(p), raw)
}

// permissionReplyInput names the request in the path, the decision in
// the body.
//
// @decision(DL-020)
type permissionReplyInput struct {
	Ref  string `path:"ref" doc:"Session ID or name"`
	ID   string `path:"id" doc:"The request's per_ id"`
	Body PermissionReply
}

type formAnswerInput struct {
	Ref  string `path:"ref" doc:"Session ID or name"`
	ID   string `path:"id" doc:"The form's frm_ id"`
	Body FormAnswer
}

type formDismissInput struct {
	Ref string `path:"ref" doc:"Session ID or name"`
	ID  string `path:"id" doc:"The form's frm_ id"`
}

type answeredAskOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AnsweredAsk
}

// toAskHumaError maps ErrNoSuchAsk before the steering errors, which
// would call it "no such session".
//
// @decision(DL-020)
func toAskHumaError(err error) error {
	if errors.Is(err, ErrNoSuchAsk) {
		return huma.Error404NotFound(err.Error())
	}
	return toSteerHumaError(err)
}

// registerAsks adds the three answer routes.
//
// @decision(DL-020)
func (s *Server) registerAsks() {
	reply := func(out AnsweredAsk, err error) (*answeredAskOutput, error) {
		if err != nil {
			return nil, toAskHumaError(err)
		}
		return &answeredAskOutput{CacheControl: "no-store", Body: out}, nil
	}
	errs := []int{http.StatusNotFound, http.StatusConflict, http.StatusTooManyRequests}
	register(s.api, huma.Operation{
		OperationID: "reply-permission", Method: http.MethodPost, Path: "/api/v1/sessions/{ref}/permissions/{id}",
		Summary: "Allow once or reject a permission the agent asked for", DefaultStatus: http.StatusAccepted,
		Tags: []string{"sessions"}, Errors: errs,
	}, func(ctx context.Context, in *permissionReplyInput) (*answeredAskOutput, error) {
		return reply(s.svc.ReplyPermission(ctx, principal(ctx), in.Ref, in.ID, in.Body))
	})
	register(s.api, huma.Operation{
		OperationID: "answer-form", Method: http.MethodPost, Path: "/api/v1/sessions/{ref}/forms/{id}",
		Summary: "Answer a question the agent asked", DefaultStatus: http.StatusAccepted,
		Tags: []string{"sessions"}, Errors: errs,
	}, func(ctx context.Context, in *formAnswerInput) (*answeredAskOutput, error) {
		return reply(s.svc.AnswerForm(ctx, principal(ctx), in.Ref, in.ID, in.Body))
	})
	register(s.api, huma.Operation{
		OperationID: "dismiss-form", Method: http.MethodDelete, Path: "/api/v1/sessions/{ref}/forms/{id}",
		Summary: "Dismiss a question the agent asked, which ends its turn", DefaultStatus: http.StatusAccepted,
		Tags: []string{"sessions"}, Errors: errs,
	}, func(ctx context.Context, in *formDismissInput) (*answeredAskOutput, error) {
		return reply(s.svc.DismissForm(ctx, principal(ctx), in.Ref, in.ID))
	})
}

// AskEvent is what a client needs to draw and answer an ask, re-derived
// from a stored permission or form frame (spike S-F1's shapes). It is on
// the five ask frames only.
//
// @decision(DL-020)
type AskEvent struct {
	// ID is the request (per_…) or form (frm_…) the frame is about.
	ID   string `json:"id"`
	Kind string `json:"kind" enum:"permission,form"`
	// Action and Resources are what a permission asks to do, and Call
	// the tool call that asked.
	Action    string   `json:"action,omitempty"`
	Resources []string `json:"resources,omitempty"`
	Call      string   `json:"call,omitempty"`
	// Title and Fields are a form's, on form.created.
	Title  string      `json:"title,omitempty"`
	Fields []FormField `json:"fields,omitempty"`
	// Reply is a permission.replied's decision (once, always or reject).
	Reply string `json:"reply,omitempty"`
	// Answer is a form.replied's answer, keyed by field.
	Answer map[string]any `json:"answer,omitempty"`
}

// FormField is one field of a question form, as OpenCode's Form.Field
// carries it; a client must tolerate a type it does not draw.
//
// @decision(DL-020)
type FormField struct {
	Key         string       `json:"key"`
	Type        string       `json:"type"`
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Placeholder string       `json:"placeholder,omitempty"`
	URL         string       `json:"url,omitempty"`
	Options     []FormOption `json:"options,omitempty"`
	Custom      bool         `json:"custom,omitempty"`
	Required    bool         `json:"required,omitempty"`
	Hidden      bool         `json:"hidden,omitempty"`
	When        []FormWhen   `json:"when,omitempty"`
	// Default is the field's preset value: a string, number, boolean or
	// list of strings.
	Default any `json:"default,omitempty"`
}

// FormOption is one choice of a field.
type FormOption struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// FormWhen shows a field only while another field's value is (eq) or is
// not (neq) the given one.
type FormWhen struct {
	Key   string `json:"key"`
	Op    string `json:"op" enum:"eq,neq"`
	Value any    `json:"value"`
}

// storedAsk re-derives an ask frame's AskEvent, nil for any other row.
// The sandbox's own ephemeral frames only.
//
// @decision(DL-020) @decision(DL-018)
func storedAsk(ev store.Event) *AskEvent {
	if ev.Source != store.SourceEphemeral || len(ev.Raw) == 0 {
		return nil
	}
	var frame struct {
		Data struct {
			ID        string         `json:"id"`
			RequestID string         `json:"requestID"`
			Action    string         `json:"action"`
			Resources []string       `json:"resources"`
			Reply     string         `json:"reply"`
			Answer    map[string]any `json:"answer"`
			Source    struct {
				ID string `json:"id"`
			} `json:"source"`
			Form struct {
				ID       string      `json:"id"`
				Title    string      `json:"title"`
				Fields   []FormField `json:"fields"`
				Metadata struct {
					Tool struct {
						ID string `json:"id"`
					} `json:"tool"`
				} `json:"metadata"`
			} `json:"form"`
		} `json:"data"`
	}
	if json.Unmarshal(ev.Raw, &frame) != nil {
		return nil
	}
	d := frame.Data
	var out AskEvent
	switch ev.Name {
	case eventPermissionAsked:
		out = AskEvent{ID: d.ID, Kind: "permission", Action: d.Action, Resources: d.Resources, Call: d.Source.ID}
	case eventPermissionReplied:
		out = AskEvent{ID: d.RequestID, Kind: "permission", Reply: d.Reply}
	case eventFormCreated:
		out = AskEvent{ID: d.Form.ID, Kind: "form", Title: d.Form.Title, Fields: d.Form.Fields, Call: d.Form.Metadata.Tool.ID}
		// A link the sandbox wrote reaches a page as an href, so only a
		// web address survives (the page checks again).
		for i := range out.Fields {
			if !webURL(out.Fields[i].URL) {
				out.Fields[i].URL = ""
			}
		}
	case eventFormReplied:
		out = AskEvent{ID: d.ID, Kind: "form", Answer: d.Answer}
	case eventFormCancelled:
		out = AskEvent{ID: d.ID, Kind: "form"}
	default:
		return nil
	}
	if out.ID == "" {
		return nil
	}
	return &out
}

// webURL reports an absolute http or https address.
//
// @decision(DL-020)
func webURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
