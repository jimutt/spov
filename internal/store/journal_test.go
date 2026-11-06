package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// journaled opens a fresh journal in a temporary directory.
func journaled(t *testing.T, dir string, opts JournalOptions) (*Memory, *Journal) {
	t.Helper()
	m := NewMemory()
	j, err := OpenJournal(dir, m, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return m, j
}

var jt0 = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

// reads is everything a server asks the store about one session.
type reads struct {
	Session  Session
	Runs     map[string][]Run
	Convs    []Conversation
	Messages []Message
	Events   []Event
	LastSeqs map[string]int64
}

func readAll(t *testing.T, s Store, id string) reads {
	t.Helper()
	ctx := context.Background()
	var r reads
	var err error
	if r.Session, err = s.Sessions().Get(ctx, "org", id); err != nil {
		t.Fatal(err)
	}
	r.Runs, _ = s.Runs().List(ctx, "org", []string{id})
	r.Convs, _ = s.Conversations().ListConversations(ctx, "org", id)
	r.Messages, _ = s.Conversations().ListMessages(ctx, "org", id)
	r.Events, _ = s.Conversations().ListEvents(ctx, "org", id, 0, 0)
	r.LastSeqs, _ = s.Conversations().LastSeqs(ctx, "org", id)
	return r
}

// A journaled store reopened from its directory answers every read as
// the store that wrote it did: rows, runs, an upserted message's last
// version, events at their positions; a rolled-back transaction left no
// trace, and new events continue the positions.
//
// @decision(DL-091) @decision(DL-092)
func TestAJournalReopensToTheSameStore(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	m, j := journaled(t, dir, JournalOptions{})
	m.Now = func() time.Time { return jt0 }
	sess := Session{ID: NewID(jt0), OrgID: "org", OwnerID: "u", Name: "work", State: StateRunning, SandboxID: "sb-1",
		Request: SessionRequest{Name: "work", Repos: []RepoRef{{Platform: "github", Owner: "o", Name: "r"}}}}
	if err := m.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	running := jt0.Add(time.Minute)
	if err := m.Runs().Insert(ctx, Run{OrgID: "org", SessionID: sess.ID, OwnerID: "u", N: 1, SandboxID: "sb-1", Origin: RunFromLaunch,
		StartedAt: jt0, RunningAt: &running, SnapshotState: SnapshotPending, FirstMessage: []byte(`{"text": "a <b> & c"}`)}); err != nil {
		t.Fatal(err)
	}
	conv, err := m.Conversations().UpsertConversation(ctx, Conversation{ID: NewID(jt0), OrgID: "org", SessionID: sess.ID,
		OpenCodeID: "ses_1", Title: "t", Info: []byte(`{"id":"ses_1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Conversations().SetSeqOffset(ctx, "org", conv.ID, 13, 4); err != nil {
		t.Fatal(err)
	}
	msg := Message{ID: NewID(jt0), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID, OpenCodeID: "msg_1",
		Role: "assistant", Text: "first", At: jt0, Raw: []byte(`{"id":"msg_1"}`)}
	if _, err := m.Conversations().PutMessages(ctx, []Message{msg}); err != nil {
		t.Fatal(err)
	}
	msg.ID, msg.Text = NewID(jt0.Add(time.Second)), "first, finished"
	if _, err := m.Conversations().PutMessages(ctx, []Message{msg}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Conversations().PutEvents(ctx, []Event{
		{ID: NewID(jt0), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID, Source: SourceOpenCode, Seq: 14, Name: "session.step.started", At: jt0, Raw: []byte("not json \x00")},
		{ID: NewID(jt0.Add(time.Millisecond)), OrgID: "org", SessionID: sess.ID, Source: SourceSpov, Name: SpovNamespace + "session.resumed", Text: "x", At: jt0},
	}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("roll back")
	_ = m.Tx(ctx, func(tx Store) error {
		cur, _ := tx.Sessions().Get(ctx, "org", sess.ID)
		cur.Name = "never"
		_ = tx.Sessions().Update(ctx, cur)
		return boom
	})
	if err := m.Tx(ctx, func(tx Store) error {
		cur, _ := tx.Sessions().Get(ctx, "org", sess.ID)
		cur.Landing = "/workspace/r"
		return tx.Sessions().Update(ctx, cur)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Sessions().MarkStopped(ctx, "org", sess.ID, StateStopped, jt0.Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	want := readAll(t, m, sess.ID)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	again, j2 := journaled(t, dir, JournalOptions{})
	got := readAll(t, again, sess.ID)
	if !reflect.DeepEqual(got, want) {
		for _, f := range []struct {
			name      string
			got, want any
		}{{"session", got.Session, want.Session}, {"runs", got.Runs, want.Runs}, {"convs", got.Convs, want.Convs},
			{"messages", got.Messages, want.Messages}, {"events", got.Events, want.Events}, {"seqs", got.LastSeqs, want.LastSeqs}} {
			if !reflect.DeepEqual(f.got, f.want) {
				t.Errorf("reopened %s:\n got  %#v\n want %#v", f.name, f.got, f.want)
			}
		}
	}
	if got.Session.Name != "work" || got.Session.Landing != "/workspace/r" {
		t.Errorf("the rolled-back and committed transactions: %+v", got.Session)
	}
	evs, err := again.Conversations().PutEvents(ctx, []Event{{ID: NewID(jt0.Add(time.Hour)), OrgID: "org", SessionID: sess.ID,
		Source: SourceSpov, Name: SpovNamespace + "x", At: jt0}})
	if err != nil || evs[0].Pos <= want.Events[len(want.Events)-1].Pos {
		t.Errorf("a new event after reopening: %+v, %v", evs, err)
	}
	if dup, _ := again.Conversations().PutEvents(ctx, []Event{{ID: NewID(jt0.Add(2 * time.Hour)), OrgID: "org", SessionID: sess.ID,
		ConversationID: conv.ID, Source: SourceOpenCode, Seq: 14, Name: "session.step.started", At: jt0}}); len(dup) != 0 {
		t.Error("a stored sequence was not known after reopening")
	}
	_ = j2.Close()

	// The files are the documented ones, readable on their own.
	info, err := os.Stat(filepath.Join(dir, sess.ID, transcriptFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("transcript mode: %v, %v", info, err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, sess.ID, transcriptFile))
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	var head map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &head)
	if head["type"] != "header" || head["format"] != float64(JournalFormat) {
		t.Errorf("the first line: %s", lines[0])
	}
	if !strings.Contains(string(body), `"raw":{"json":{"id":"msg_1"}}`) || !strings.Contains(string(body), `"base64"`) {
		t.Errorf("byte fields are not readable JSON or base64:\n%s", body)
	}
}

// A torn last line is cut off before anything else is appended, and a
// mostly superseded transcript is compacted.
//
// @decision(DL-092)
func TestATornLineIsCutAndASupersededTranscriptCompacted(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	var warned []string
	m, j := journaled(t, dir, JournalOptions{})
	sess := Session{ID: NewID(jt0), OrgID: "org", OwnerID: "u", Name: "w", State: StateRunning}
	_ = m.Sessions().Create(ctx, sess)
	conv, _ := m.Conversations().UpsertConversation(ctx, Conversation{ID: NewID(jt0), OrgID: "org", SessionID: sess.ID, OpenCodeID: "ses_1"})
	for i := range 10 {
		msg := Message{ID: NewID(jt0.Add(time.Duration(i) * time.Second)), OrgID: "org", SessionID: sess.ID, ConversationID: conv.ID,
			OpenCodeID: "msg_1", Role: "assistant", Text: strings.Repeat("x", i+1), At: jt0}
		if _, err := m.Conversations().PutMessages(ctx, []Message{msg}); err != nil {
			t.Fatal(err)
		}
	}
	_ = j.Close()
	path := filepath.Join(dir, sess.ID, transcriptFile)
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	_, _ = f.WriteString(`{"type":"event","event":{"id":`)
	_ = f.Close()

	again, j2 := journaled(t, dir, JournalOptions{Warn: func(s string) { warned = append(warned, s) }})
	msgs, _ := again.Conversations().ListMessages(ctx, "org", sess.ID)
	if len(msgs) != 1 || msgs[0].Text != strings.Repeat("x", 10) {
		t.Fatalf("messages = %+v", msgs)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "torn") {
		t.Errorf("warnings = %v", warned)
	}
	body, _ := os.ReadFile(path)
	if n := strings.Count(string(body), "\n"); n != 3 || strings.Contains(string(body), `"id":`+"\n") {
		t.Errorf("after compaction %d lines:\n%s", n, body)
	}
	if _, err := again.Conversations().PutEvents(ctx, []Event{{ID: NewID(jt0), OrgID: "org", SessionID: sess.ID, Source: SourceSpov, Name: SpovNamespace + "x", At: jt0}}); err != nil {
		t.Fatal(err)
	}
	_ = j2.Close()
	third, _ := journaled(t, dir, JournalOptions{})
	if evs, _ := third.Conversations().ListEvents(ctx, "org", sess.ID, 0, 0); len(evs) != 1 {
		t.Errorf("an event appended after the cut did not survive: %+v", evs)
	}
}

// Pruning deletes a stopped session's history past the retention, but
// not a live one's, a recent one's, or one whose snapshot is kept.
//
// @decision(DL-093)
func TestPruningFollowsTheRetention(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	m, j := journaled(t, dir, JournalOptions{})
	mk := func(name string, state SessionState, stoppedAgo time.Duration, snapshot SnapshotState) string {
		id := NewID(jt0.Add(time.Duration(len(name)) * time.Millisecond))
		if err := m.Sessions().Create(ctx, Session{ID: id, OrgID: "org", OwnerID: "u", Name: name, State: StateRunning}); err != nil {
			t.Fatal(err)
		}
		if snapshot != "" {
			_ = m.Runs().Insert(ctx, Run{OrgID: "org", SessionID: id, OwnerID: "u", N: 1, Origin: RunFromLaunch, StartedAt: jt0, SnapshotState: snapshot})
		}
		if state != StateRunning {
			_, _ = m.Sessions().MarkStopped(ctx, "org", id, state, jt0.Add(-stoppedAgo), "")
		}
		return id
	}
	old := mk("old", StateStopped, 40*24*time.Hour, "")
	kept := mk("kept-snapshot", StateStopped, 40*24*time.Hour, SnapshotKept)
	recent := mk("recent", StateFailed, 2*24*time.Hour, "")
	live := mk("live-one", StateRunning, 0, "")
	_ = j.Close()

	again, _ := journaled(t, dir, JournalOptions{Retention: 30 * 24 * time.Hour, Now: func() time.Time { return jt0 }})
	if _, err := again.Sessions().Get(ctx, "org", old); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old session was kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, old)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old session's directory is still there: %v", err)
	}
	for _, id := range []string{kept, recent, live} {
		if _, err := again.Sessions().Get(ctx, "org", id); err != nil {
			t.Errorf("%s was pruned: %v", id, err)
		}
	}
}

// One server at a time: a second open of the same directory waits, then
// refuses; a format this build does not know is skipped and said.
//
// @decision(DL-091) @decision(DL-092)
func TestOneWriterAndKnownFormatsOnly(t *testing.T) {
	old := lockWait
	lockWait = 200 * time.Millisecond
	t.Cleanup(func() { lockWait = old })
	dir := t.TempDir()
	_, _ = journaled(t, dir, JournalOptions{})
	if _, err := OpenJournal(dir, NewMemory(), JournalOptions{}); !errors.Is(err, ErrJournalLocked) {
		t.Errorf("a second writer: %v", err)
	}

	other := t.TempDir()
	id := NewID(jt0)
	_ = os.MkdirAll(filepath.Join(other, id), 0o700)
	_ = os.WriteFile(filepath.Join(other, id, sessionFile), []byte(`{"format":99,"session":{"id":"`+id+`"}}`), 0o600)
	var warned []string
	m, _ := journaled(t, other, JournalOptions{Warn: func(s string) { warned = append(warned, s) }})
	if _, err := m.Sessions().Get(context.Background(), "org", id); !errors.Is(err, ErrNotFound) || len(warned) != 1 || !strings.Contains(warned[0], "format 99") {
		t.Errorf("an unknown format: %v, %v", err, warned)
	}
}
