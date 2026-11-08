package jobs

import (
	"context"
	"fmt"
	"strings"

	"github.com/jimutt/spov/internal/store"
)

// ownConversation is the store id of the job's own conversation, the one
// jobConversation opened with the goal: store.Primary of the session's.
// It is "" when none is recorded yet, and every reader then reads every
// conversation, as before DL-114.
//
// @decision(DL-114) @decision(DL-110)
func (s *Supervisor) ownConversation(ctx context.Context, job store.Job) (string, error) {
	convs, err := s.Store.Conversations().ListConversations(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return "", fmt.Errorf("supervisor: reading the conversations of job %s: %w", job.ID, err)
	}
	primary, ok := store.Primary(convs)
	if !ok {
		return "", nil
	}
	return primary.ID, nil
}

// othersFrame reports whether e is a sandbox frame of a conversation
// other than own: a subagent's, whose inbox and turns are the agent's
// and never a person's or the job's.
//
// @decision(DL-114)
func othersFrame(e store.Event, own string) bool {
	return own != "" && e.Source == store.SourceOpenCode && e.ConversationID != "" && e.ConversationID != own
}

// withoutOthersInbox is events with every other conversation's inbox
// frames left out, for the readers of what a person wrote. Everything
// else stays: a subagent's asks, tool calls and evidence are the job's.
//
// @decision(DL-114)
func withoutOthersInbox(events []store.Event, own string) []store.Event {
	if own == "" {
		return events
	}
	out := make([]store.Event, 0, len(events))
	for _, e := range events {
		if othersFrame(e, own) && strings.HasPrefix(e.Name, "session.inbox.") {
			continue
		}
		out = append(out, e)
	}
	return out
}
