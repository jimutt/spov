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
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	// Parent is the OpenCode id of the conversation that started this
	// one as a subagent, absent for a root.
	//
	// @decision(DL-110)
	Parent   string        `json:"parent,omitempty"`
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
		// Truncated is set when only the newest messages are here.
		//
		// @decision(DL-131)
		Truncated bool `json:"truncated,omitempty" doc:"Only the newest messages are included; older ones are left out"`
	}
}

// MaxTranscriptMessages and MaxTranscriptBytes bound one transcript
// response: the session's newest messages, at most this many and this
// size of text and raw frame. Older ones are left out and the response
// says so.
//
// @decision(DL-131)
const (
	MaxTranscriptMessages = 20_000
	MaxTranscriptBytes    = 64 << 20
)

// transcriptTruncated is the warning a bounded transcript carries.
//
// @decision(DL-131)
var transcriptTruncated = fmt.Sprintf("this transcript holds only its newest messages, at most %d of them or %d MiB; older ones are not shown",
	MaxTranscriptMessages, MaxTranscriptBytes>>20)

// notRecorded is the warning a session with no rows carries. There is
// no fallback to the sandbox: the recorder is its only reader.
//
// @decision(DL-018)
const notRecorded = "nothing has been recorded for this session"

// transcript reads the session's conversation from the store, its
// newest messages within the bounds above, and reports whether older
// ones were left out. It must not touch the sandbox; the recorder is the
// sandbox's only reader.
//
// @decision(DL-018) @decision(DL-017) @decision(DL-131)
func (s *Server) transcript(ctx context.Context, sess store.Session) ([]ConversationView, []string, bool, error) {
	cs := s.deps.Store.Conversations()
	convs, err := cs.ListConversations(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return nil, nil, false, err
	}
	msgs, truncated, err := cs.RecentMessages(ctx, sess.OrgID, sess.ID, "", MaxTranscriptMessages, MaxTranscriptBytes)
	if err != nil {
		return nil, nil, false, err
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
		out = append(out, ConversationView{ID: c.OpenCodeID, Title: c.Title, Parent: c.ParentOpenCodeID, Messages: messages})
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
	if truncated {
		warnings = append(warnings, transcriptTruncated)
	}
	// Check the source as well as the name: a sandbox chooses event
	// names. The store also refuses spov.-named sandbox rows.
	if err == nil && last.Source == store.SourceSpov && last.Name == MarkerStopped {
		warnings = append(warnings, last.Text)
	}
	return out, warnings, truncated, nil
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
