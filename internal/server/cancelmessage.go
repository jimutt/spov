package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// Cancelling a queued message: a once-per-human-act write, like a send.
// OpenCode answers the same whether or not the item was still pending,
// so the outcome is read from the stored events, never from the answer.
//
// @decision(DL-019)

// MarkerMessageCancelled records who asked for a message to be
// cancelled; whether it was is the sandbox's session.inbox.cancelled.
//
// @decision(DL-019) @decision(DL-058)
const MarkerMessageCancelled = store.SpovNamespace + "session.message.cancelled"

// CancelledMessage is a cancel the sandbox accepted. It does not say the
// message was still pending.
//
// @decision(DL-019)
type CancelledMessage struct {
	ID           string `json:"id"`
	Conversation string `json:"conversation" doc:"The OpenCode conversation the message was in"`
}

// CancelMessage asks a session's sandbox to drop a message it has not
// delivered yet.
//
// @decision(DL-019)
func (s *Service) CancelMessage(ctx context.Context, p auth.Principal, ref, id, conversation string) (CancelledMessage, error) {
	if err := validMessageID(id); err != nil {
		return CancelledMessage{}, err
	}
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return CancelledMessage{}, err
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return CancelledMessage{}, fmt.Errorf("%w (state %s)", ErrSessionNotRunning, sess.State)
	}
	conversation, err = s.steerInto(ctx, sess, conversation)
	if err != nil {
		return CancelledMessage{}, err
	}
	// Only a person's own message: spov's queued continuations are not
	// the caller's to drop.
	if err := s.isPersonsMessage(ctx, sess, id); err != nil {
		return CancelledMessage{}, err
	}
	// The same two bounds as a send: a DELETE is a plain request that
	// resets the sandbox's idle clock.
	//
	// @decision(DL-019)
	if err := s.mintBudgetLeft(ctx, sess, ErrTooManyMessages); err != nil {
		return CancelledMessage{}, err
	}
	if !s.messages.take(sess.ID, s.deps.Now()) {
		return CancelledMessage{}, ErrTooManyMessages
	}
	err = s.withConnection(ctx, p, sess, func(conn Connection) error {
		return sandboxDelete(ctx, conn, inboxItemURL(conn, conversation, id))
	})
	if err != nil {
		return CancelledMessage{}, err
	}
	s.recordCancel(ctx, p, sess, id)
	// @decision(DL-017)
	if s.OnStart != nil {
		s.OnStart(sess)
	}
	return CancelledMessage{ID: id, Conversation: conversation}, nil
}

// isPersonsMessage checks the id against the stored enqueue: a user
// item of this session.
//
// @decision(DL-019)
func (s *Service) isPersonsMessage(ctx context.Context, sess store.Session, id string) error {
	ev, err := s.deps.Store.Conversations().FindEvent(ctx, sess.OrgID, sess.ID, jobs.EventInboxEnqueued,
		[]byte(`"inboxID":"`+id+`"`))
	if errors.Is(err, store.ErrNotFound) || (err == nil && (ev.Source != store.SourceOpenCode ||
		jobs.InboxID(ev.Raw) != id || jobs.InboxItemType(ev.Raw) != "user")) {
		return fmt.Errorf("%w: this session has no message %s from a person", store.ErrNotFound, id)
	}
	return err
}

// inboxItemURL is one pending item of a conversation's inbox.
//
// @decision(DL-019)
func inboxItemURL(conn Connection, conversation, id string) string {
	return conn.URL + "/api/session/" + url.PathEscape(conversation) + "/inbox/" + url.PathEscape(id)
}

// sandboxDelete is sandboxPost for a DELETE, with no body either way.
//
// @decision(DL-019) @decision(DL-046)
func sandboxDelete(ctx context.Context, conn Connection, url string) error {
	ctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.AddCookie(&http.Cookie{Name: proxy.TokenCookie, Value: conn.Token})
	req.SetBasicAuth(conn.User, conn.Password)
	client := &http.Client{
		Timeout:       OpenTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("write to the sandbox: %s", redact.Error(err))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxCreatedSessionBytes))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return sandboxStatus(actWrite, resp.StatusCode, resp.Status)
	}
	return nil
}

// recordCancel writes the audit marker; like recordSteer, a failure
// only warns.
//
// @decision(DL-019)
func (s *Service) recordCancel(ctx context.Context, p auth.Principal, sess store.Session, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SandboxReadTimeout)
	defer cancel()
	now := s.deps.Now()
	_, err := s.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerMessageCancelled, Text: "message cancelled by " + cancelledBy(p) + ": " + id, At: now,
	}})
	if err != nil {
		s.deps.Warn(fmt.Sprintf("cancel %s: recording the act: %s", sess.ID, redact.Error(err)))
	}
}

// cancelMessageInput names the message in the path and, when the
// session has several, its conversation in the query.
//
// @decision(DL-019)
type cancelMessageInput struct {
	Ref          string `path:"ref" doc:"Session ID or name"`
	ID           string `path:"id" doc:"The message's msg_ id"`
	Conversation string `query:"conversation" doc:"OpenCode conversation id; required when the session has more than one"`
}

type cancelMessageOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CancelledMessage
}
