package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// MarkerImportFailed notes a conversation a transcript resume could not
// import: its transcript is still on the page, but the agent will not
// see it.
//
// @decision(DL-089)
const MarkerImportFailed = store.SpovNamespace + "session.import.failed"

// importConversations rebuilds each of the session's stored
// conversations in a fresh sandbox, oldest first, through the
// launcher's own connection, and records where each one's durable log
// now continues the stored one. A conversation that fails is noted and
// left out; the resume carries on.
//
// @decision(DL-089)
func (s *Service) importConversations(ctx context.Context, sess store.Session, live *session.Session, log *launchLog) {
	ctx, cancel := context.WithTimeout(ctx, ImportBudget)
	defer cancel()
	convs, err := s.deps.Store.Conversations().ListConversations(ctx, sess.OrgID, sess.ID)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("resume %s: list conversations: %s", sess.ID, redact.Error(err)))
		return
	}
	msgs, err := s.deps.Store.Conversations().ListMessages(ctx, sess.OrgID, sess.ID)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("resume %s: list messages: %s", sess.ID, redact.Error(err)))
		return
	}
	last, err := s.deps.Store.Conversations().LastSeqs(ctx, sess.OrgID, sess.ID)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("resume %s: read the stored sequences: %s", sess.ID, redact.Error(err)))
		return
	}
	conn := Connection{URL: live.URL, User: opencode.BasicAuthUser, Password: live.Password}
	for _, conv := range convs {
		raws := messagesOf(msgs, conv.ID)
		if len(raws) == 0 {
			continue
		}
		info := conv.Info
		if len(info) == 0 {
			info = builtInfo(conv, sess.Landing)
		}
		after, err := importConversation(ctx, conn, info, raws)
		if err == nil {
			err = s.deps.Store.Conversations().SetSeqOffset(ctx, sess.OrgID, conv.ID, last[conv.ID], after)
		}
		if err != nil {
			s.deps.Warn(fmt.Sprintf("resume %s: import %s: %s", sess.ID, conv.OpenCodeID, redact.Error(err)))
			log.Note(fmt.Sprintf("the conversation %s could not be restored, so the agent will not see it", conv.OpenCodeID))
			s.noteImportFailed(context.WithoutCancel(ctx), sess, conv, err)
		}
	}
}

// messagesOf is a conversation's stored messages whole, oldest first.
func messagesOf(msgs []store.Message, conversationID string) []json.RawMessage {
	var out []json.RawMessage
	for _, m := range msgs {
		if m.ConversationID == conversationID && json.Valid(m.Raw) {
			out = append(out, json.RawMessage(m.Raw))
		}
	}
	return out
}

// builtInfo is a Session.Info from what the store holds, for a
// conversation recorded before its info was kept.
//
// @decision(DL-089)
func builtInfo(conv store.Conversation, landing string) []byte {
	created, updated := conv.CreatedAt.UnixMilli(), conv.UpdatedAt.UnixMilli()
	info, _ := json.Marshal(map[string]any{
		"id": conv.OpenCodeID, "title": conv.Title, "projectID": "global",
		"cost":     0,
		"tokens":   map[string]any{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
		"time":     map[string]any{"created": created, "updated": updated},
		"location": map[string]any{"directory": cmpOr(landing, opencode.WorkspaceRoot)},
	})
	return info
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// importConversation imports one conversation and returns the durable
// sequence its import reached, which the recorder skips past.
//
// @decision(DL-089)
func importConversation(ctx context.Context, conn Connection, info []byte, messages []json.RawMessage) (int64, error) {
	body, err := json.Marshal(map[string]any{"info": json.RawMessage(info), "messages": messages})
	if err != nil {
		return 0, err
	}
	var id struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(info, &id); err != nil || id.ID == "" {
		return 0, fmt.Errorf("the conversation's info names no id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, conn.URL+"/api/experimental/session/import", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: proxy.TokenCookie, Value: conn.Token})
	req.SetBasicAuth(conn.User, conn.Password)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("import: %s", redact.Error(err))
	}
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("import: %s: %s", resp.Status, strings.TrimSpace(redact.Text(string(answer))))
	}
	return logHead(ctx, conn, id.ID)
}

// logHead is the highest durable sequence in a conversation's log, read
// once with follow off.
//
// @decision(DL-089)
func logHead(ctx context.Context, conn Connection, conversation string) (int64, error) {
	path := "/api/experimental/session/" + url.PathEscape(conversation) + "/log?" + url.Values{"after": {"0"}, "follow": {"false"}}.Encode()
	body, err := sandboxGet(ctx, conn.URL+path, conn.Token, conn.User, conn.Password)
	if err != nil {
		return 0, fmt.Errorf("read the imported log: %w", err)
	}
	var head int64
	for _, frame := range logFrames(body) {
		head = max(head, durableSeq(frame))
		// With follow off the pinned build answers only its sync point,
		// {"type":"log.synced","seq":N}, which is the head (S-TR1).
		if frame["type"] == "log.synced" {
			if seq, ok := frame["seq"].(float64); ok {
				head = max(head, int64(seq))
			}
		}
	}
	return head, nil
}

// logFrames reads a durable log answer: server-sent events, a JSON
// array, or a {data: [...]} envelope.
func logFrames(body []byte) []map[string]any {
	var out []map[string]any
	var list []map[string]any
	if json.Unmarshal(body, &list) == nil {
		return list
	}
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Data != nil {
		return envelope.Data
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), int(MaxSandboxReadBytes))
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "data:"))
		var frame map[string]any
		if line != "" && json.Unmarshal([]byte(line), &frame) == nil {
			out = append(out, frame)
		}
	}
	return slices.Clip(out)
}

func (s *Service) noteImportFailed(ctx context.Context, sess store.Session, conv store.Conversation, err error) {
	now := s.deps.Now()
	if _, perr := s.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov, Name: MarkerImportFailed,
		Text: fmt.Sprintf("the conversation could not be restored, so the agent will not see it: %s", redact.Error(err)), At: now,
	}}); perr != nil {
		s.deps.Warn(fmt.Sprintf("resume %s: note the failed import of %s: %s", sess.ID, conv.OpenCodeID, redact.Error(perr)))
	}
}
