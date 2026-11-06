package postgres

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// pgConversations implements store.ConversationStore. Idempotency is the
// schema's: the unique indexes and the conflict clauses in queries.sql
// are what make a repeated write one row.
//
// @decision(DL-017)
type pgConversations struct{ s *Store }

func (p pgConversations) UpsertConversation(ctx context.Context, c store.Conversation) (store.Conversation, error) {
	now := p.s.now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	row, err := p.s.q.UpsertConversation(ctx, sqlcgen.UpsertConversationParams{
		ID: c.ID, OrgID: c.OrgID, SessionID: c.SessionID, OpencodeID: c.OpenCodeID,
		Title: c.Title, CreatedAt: ts(c.CreatedAt), UpdatedAt: ts(now), Info: nonEmpty(c.Info),
	})
	if err != nil {
		return store.Conversation{}, mapErr("upsert conversation", err)
	}
	return conversationFromRow(row), nil
}

func (p pgConversations) ListConversations(ctx context.Context, orgID, sessionID string) ([]store.Conversation, error) {
	rows, err := p.s.q.ListConversations(ctx, sqlcgen.ListConversationsParams{OrgID: orgID, SessionID: sessionID})
	if err != nil {
		return nil, mapErr("list conversations", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]store.Conversation, 0, len(rows))
	for _, r := range rows {
		out = append(out, conversationFromRow(r))
	}
	return out, nil
}

// PutMessages writes the batch in one transaction. The composite
// foreign key is what refuses a message naming a different org or
// session than its conversation, and mapErr turns that into
// ErrNotFound, so both stores answer the same.
func (p pgConversations) PutMessages(ctx context.Context, msgs []store.Message) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	changed := 0
	put := func(s *Store) error {
		for _, m := range msgs {
			if m.CreatedAt.IsZero() {
				m.CreatedAt = s.now()
			}
			// Never nil, matching what the events path does and what
			// the column's default gives.
			raw := m.Raw
			if raw == nil {
				raw = []byte{}
			}
			n, err := s.q.PutMessage(ctx, sqlcgen.PutMessageParams{
				ID: m.ID, OrgID: m.OrgID, SessionID: m.SessionID, ConversationID: m.ConversationID,
				OpencodeID: m.OpenCodeID, Role: m.Role, Text: m.Text, At: ts(m.At),
				Raw: raw, CreatedAt: ts(m.CreatedAt),
			})
			if err != nil {
				return mapErr("put message", err)
			}
			changed += int(n)
		}
		return nil
	}
	if err := p.s.atomically(ctx, put); err != nil {
		return 0, err
	}
	return changed, nil
}

// atomically runs fn inside the current transaction, or a new one when
// there is none.
func (s *Store) atomically(ctx context.Context, fn func(*Store) error) error {
	if s.inTx {
		return fn(s)
	}
	return s.Tx(ctx, func(tx store.Store) error { return fn(tx.(*Store)) })
}

func (p pgConversations) ListMessages(ctx context.Context, orgID, sessionID string) ([]store.Message, error) {
	rows, err := p.s.q.ListMessages(ctx, sqlcgen.ListMessagesParams{OrgID: orgID, SessionID: sessionID})
	if err != nil {
		return nil, mapErr("list messages", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]store.Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, messageFromRow(r))
	}
	return out, nil
}

func (p pgConversations) LastMessages(ctx context.Context, orgID string, sessionIDs []string) (map[string]store.Message, error) {
	out := map[string]store.Message{}
	if len(sessionIDs) == 0 {
		return out, nil
	}
	rows, err := p.s.q.LastMessages(ctx, sqlcgen.LastMessagesParams{OrgID: orgID, SessionIds: sessionIDs})
	if err != nil {
		return nil, mapErr("last messages", err)
	}
	for _, r := range rows {
		out[r.SessionID] = messageFromRow(r)
	}
	return out, nil
}

// PutEvents inserts one at a time inside one transaction, so a batch
// lands whole or not at all. A frame the index already holds comes back
// as no row, which is the skip the interface promises.
func (p pgConversations) PutEvents(ctx context.Context, evs []store.Event) ([]store.Event, error) {
	if len(evs) == 0 {
		return nil, nil
	}
	for _, ev := range evs {
		if err := ev.Validate(); err != nil {
			return nil, err
		}
	}
	var out []store.Event
	put := func(s *Store) error {
		for _, ev := range evs {
			if ev.CreatedAt.IsZero() {
				ev.CreatedAt = s.now()
			}
			var conv *string
			if ev.ConversationID != "" {
				id := ev.ConversationID
				conv = &id
			}
			raw := ev.Raw
			if raw == nil {
				raw = []byte{}
			}
			// 0 means "no sequence" and is stored as NULL, so a
			// seq-less row is absent from the dedup key rather than
			// colliding with every other seq-less row of its
			// conversation.
			//
			// @decision(DL-017)
			var seq *int64
			if ev.Seq != 0 {
				n := ev.Seq
				seq = &n
			}
			row, err := s.q.InsertEvent(ctx, sqlcgen.InsertEventParams{
				ID: ev.ID, OrgID: ev.OrgID, SessionID: ev.SessionID, ConversationID: conv,
				Source: string(ev.Source), Seq: seq, Name: ev.Name, Text: ev.Text,
				At: ts(ev.At), Raw: raw, CreatedAt: ts(ev.CreatedAt),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return mapErr("insert event", err)
			}
			out = append(out, eventFromRow(row))
		}
		return nil
	}
	if err := p.s.atomically(ctx, put); err != nil {
		return nil, err
	}
	return out, nil
}

// LastEvent reads the newest row by Pos. Backed by the events_tail
// index, so it costs one lookup rather than a scan of the log.
//
// @decision(DL-018)
func (p pgConversations) LastEvent(ctx context.Context, orgID, sessionID string) (store.Event, error) {
	row, err := p.s.q.LastEvent(ctx, sqlcgen.LastEventParams{OrgID: orgID, SessionID: sessionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Event{}, store.ErrNotFound
	}
	if err != nil {
		return store.Event{}, mapErr("last event", err)
	}
	return eventFromRow(row), nil
}

// FindEvent is the newest event of a name whose raw frame contains the
// given bytes.
//
// @decision(DL-052)
func (p pgConversations) FindEvent(ctx context.Context, orgID, sessionID, name string, contains []byte) (store.Event, error) {
	row, err := p.s.q.FindEvent(ctx, sqlcgen.FindEventParams{OrgID: orgID, SessionID: sessionID, Name: name, Contains: contains})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Event{}, store.ErrNotFound
	}
	if err != nil {
		return store.Event{}, mapErr("find event", err)
	}
	return eventFromRow(row), nil
}

// LastSeqs is each conversation's highest stored durable sequence.
//
// @decision(DL-017)
func (p pgConversations) LastSeqs(ctx context.Context, orgID, sessionID string) (map[string]int64, error) {
	rows, err := p.s.q.LastSeqs(ctx, sqlcgen.LastSeqsParams{OrgID: orgID, SessionID: sessionID})
	if err != nil {
		return nil, mapErr("last sequences", err)
	}
	out := map[string]int64{}
	for _, r := range rows {
		if r.LastSeq > 0 {
			out[r.ConversationID] = r.LastSeq
		}
	}
	return out, nil
}

// LastEventExcept is LastEvent passing over the named events.
//
// @decision(DL-021)
func (p pgConversations) LastEventExcept(ctx context.Context, orgID, sessionID string, except []string) (store.Event, error) {
	if except == nil {
		except = []string{}
	}
	row, err := p.s.q.LastEventExcept(ctx, sqlcgen.LastEventExceptParams{OrgID: orgID, SessionID: sessionID, SkippedNames: except})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Event{}, store.ErrNotFound
	}
	if err != nil {
		return store.Event{}, mapErr("last event", err)
	}
	return eventFromRow(row), nil
}

func (p pgConversations) ListEvents(ctx context.Context, orgID, sessionID string, after int64, limit int) ([]store.Event, error) {
	if limit <= 0 {
		limit = math.MaxInt32
	}
	rows, err := p.s.q.ListEvents(ctx, sqlcgen.ListEventsParams{
		OrgID: orgID, SessionID: sessionID, Pos: after, Limit: int32(min(limit, math.MaxInt32)),
	})
	if err != nil {
		return nil, mapErr("list events", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]store.Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, eventFromRow(r))
	}
	return out, nil
}
