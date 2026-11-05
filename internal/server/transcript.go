package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// ConversationView is one OpenCode session inside the sandbox. A spov
// session is a sandbox and may hold several, or none.
//
// @decision(DL-018)
type ConversationView struct {
	ID       string        `json:"id"`
	Title    string        `json:"title,omitempty"`
	Messages []MessageView `json:"messages"`
}

// MessageView is one message, projected onto spov's own shape: the text
// it said and who said it. OpenCode's message schema is read by the
// recorder and never forwarded.
//
// @decision(DL-018)
type MessageView struct {
	// ID is OpenCode's own message id (msg_…), carried so a client can
	// recognise a message it has already drawn. Empty when none is
	// recorded.
	//
	// @decision(DL-019)
	ID   string `json:"id,omitempty"`
	Role string `json:"role,omitempty" doc:"Who spoke: user or assistant"`
	Text string `json:"text,omitempty"`
	// At is when the message was created, so a client can lay the
	// conversation and the live event stream on one timeline.
	//
	// @decision(DL-058)
	At *time.Time `json:"at,omitempty"`
}

type messagesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Conversations []ConversationView `json:"conversations"`
		// Warnings say why a transcript is empty or stale, so a client
		// can tell a session nobody recorded from one that said
		// nothing.
		Warnings []string `json:"warnings,omitempty"`
	}
}

// notRecorded is the warning a session with no rows carries. There is
// no fallback to the sandbox: the recorder is its only reader.
//
// @decision(DL-018)
const notRecorded = "nothing has been recorded for this session"

// transcript reads the session's conversation from the store. It must
// not touch the sandbox; the recorder is the sandbox's only reader.
//
// @decision(DL-018) @decision(DL-017)
func (s *Server) transcript(ctx context.Context, sess store.Session) ([]ConversationView, []string, error) {
	cs := s.deps.Store.Conversations()
	convs, err := cs.ListConversations(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return nil, nil, err
	}
	msgs, err := cs.ListMessages(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return nil, nil, err
	}
	byConversation := map[string][]MessageView{}
	for _, m := range msgs {
		byConversation[m.ConversationID] = append(byConversation[m.ConversationID], messageViewOf(m))
	}
	out := make([]ConversationView, 0, len(convs))
	for _, c := range convs {
		messages := byConversation[c.ID]
		if messages == nil {
			messages = []MessageView{}
		}
		// The id a client sees is OpenCode's, not the row's: it is what
		// the event stream names, and what a client keys on.
		out = append(out, ConversationView{ID: c.OpenCodeID, Title: c.Title, Messages: messages})
	}

	// The newest event, not the newest marker: a recorder that gave up
	// and one that later resumed both leave a stopped marker behind,
	// and only the one nothing has been written after is still true.
	// A failed-renewal note is not news of the recording.
	//
	// @decision(DL-021)
	last, err := cs.LastEventExcept(ctx, sess.OrgID, sess.ID, []string{MarkerRenewalFailed})
	switch {
	case err == nil, errors.Is(err, store.ErrNotFound):
	default:
		s.deps.Warn(fmt.Sprintf("transcript %s: %s", sess.ID, redact.Error(err)))
	}
	recorded := len(convs) > 0 || err == nil

	var warnings []string
	if !recorded {
		warnings = append(warnings, notRecorded)
	}
	// Check the source as well as the name: a sandbox chooses event
	// names. The store also refuses spov.-named sandbox rows.
	if err == nil && last.Source == store.SourceSpov && last.Name == MarkerStopped {
		warnings = append(warnings, last.Text)
	}
	return out, warnings, nil
}

// messageViewOf projects a stored message.
//
// @decision(DL-018)
func messageViewOf(m store.Message) MessageView {
	v := MessageView{ID: m.OpenCodeID, Role: m.Role, Text: m.Text}
	if !m.At.IsZero() {
		at := m.At.UTC()
		v.At = &at
	}
	return v
}
