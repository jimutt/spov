package store

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// JournalFormat is the version of the local journal's files. A reader
// refuses a version it does not know.
//
// @decision(DL-092)
const JournalFormat = 1

// The journal's file names inside a session's directory.
//
// @decision(DL-092)
const (
	sessionFile    = "session.json"
	transcriptFile = "transcript.jsonl"
	lockFile       = ".lock"
	// lockWaitDefault is how long a second server waits for the lock,
	// which covers one server being stopped as another starts.
	lockWaitDefault = 5 * time.Second
)

// lockWait is lockWaitDefault, shortened by tests.
var lockWait = lockWaitDefault

// change is one committed write the journal keeps: a session's row with
// its runs, a conversation, or a batch of messages or events.
//
// @decision(DL-091)
type change struct {
	session string
	row     *Session
	runs    []Run
	conv    *Conversation
	msgs    []Message
	evs     []Event
}

// emit hands a committed write to the sink, or holds it in a
// transaction view until the transaction commits.
func (m *Memory) emit(c change) {
	if m.inTx {
		if m.pending != nil {
			*m.pending = append(*m.pending, c)
		}
		return
	}
	if m.sink != nil {
		m.sink([]change{c})
	}
}

// emitRow emits a session's row and runs as they now stand. The caller
// holds the lock.
func (m *Memory) emitRow(id string) {
	if m.sink == nil && m.pending == nil {
		return
	}
	sess, ok := m.sessions[id]
	if !ok {
		return
	}
	row := copySession(sess)
	m.emit(change{session: id, row: &row, runs: slices.Clone(m.runs[id])})
}

// Journal keeps a memory store's sessions on disk, one directory per
// session, and reads them back at open. It holds the directory's lock
// until Close.
//
// @decision(DL-091) @decision(DL-092)
type Journal struct {
	dir  string
	lock *os.File
	warn func(string)

	mu sync.Mutex
	// failing is set while writes fail, so the warning is said once
	// per run of failures.
	failing bool
}

// JournalOptions configures OpenJournal.
//
// @decision(DL-093)
type JournalOptions struct {
	// Retention is how long after it stopped a session's history is
	// kept; 0 keeps it forever.
	Retention time.Duration
	// Now is the clock pruning reads; nil is time.Now.
	Now func() time.Time
	// Warn reports a problem with a file; nil discards it.
	Warn func(string)
}

// ErrJournalLocked is a second server opening a journal another holds.
var ErrJournalLocked = errors.New("store: another spov server is using the local session history")

// OpenJournal takes the directory's lock, reads every session in it
// into m, prunes what the retention lets go, and from then on keeps
// every committed write m makes. m must be empty.
//
// @decision(DL-091) @decision(DL-093)
func OpenJournal(dir string, m *Memory, opts JournalOptions) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create the session history %s: %w", dir, err)
	}
	lock, err := takeLock(filepath.Join(dir, lockFile))
	if err != nil {
		return nil, fmt.Errorf("%w (%s): %w", ErrJournalLocked, dir, err)
	}
	j := &Journal{dir: dir, lock: lock, warn: opts.Warn}
	if j.warn == nil {
		j.warn = func(string) {}
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	if err := j.load(m); err != nil {
		_ = j.Close()
		return nil, err
	}
	j.prune(m, opts.Retention, now())
	m.mu.Lock()
	m.sink = j.write
	m.mu.Unlock()
	return j, nil
}

// takeLock holds an exclusive flock on path, waiting up to lockWait.
func takeLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Close releases the lock. Writes after it are not kept.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(j.lock.Fd()), syscall.LOCK_UN)
	err := j.lock.Close()
	j.lock = nil
	return err
}

// write keeps a batch of committed changes. It runs under the memory
// store's lock, so changes reach disk in the order they were made.
func (j *Journal) write(changes []change) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.lock == nil {
		return
	}
	var failed error
	appends := map[string][]any{}
	var order []string
	for _, c := range changes {
		switch {
		case c.row != nil:
			if err := j.writeRow(*c.row, c.runs); err != nil {
				failed = err
			}
		case c.conv != nil:
			order = appendOnce(order, appends, c.conv.SessionID)
			appends[c.conv.SessionID] = append(appends[c.conv.SessionID], conversationRecord(*c.conv))
		case len(c.msgs) > 0:
			for _, msg := range c.msgs {
				order = appendOnce(order, appends, msg.SessionID)
				appends[msg.SessionID] = append(appends[msg.SessionID], messageRecord(msg))
			}
		case len(c.evs) > 0:
			for _, ev := range c.evs {
				order = appendOnce(order, appends, ev.SessionID)
				appends[ev.SessionID] = append(appends[ev.SessionID], eventRecord(ev))
			}
		}
	}
	for _, id := range order {
		if err := j.appendRecords(id, appends[id]); err != nil {
			failed = err
		}
	}
	switch {
	case failed != nil && !j.failing:
		j.failing = true
		j.warn(fmt.Sprintf("the local session history could not be written, so a restart will not remember this: %v", failed))
	case failed == nil:
		j.failing = false
	}
}

func appendOnce(order []string, seen map[string][]any, id string) []string {
	if _, ok := seen[id]; !ok {
		seen[id] = nil
		return append(order, id)
	}
	return order
}

// sessionDir is a session's directory. IDs are ULIDs, which have no path
// separator, but one that is not is refused rather than trusted.
func (j *Journal) sessionDir(id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("store: %q is not a session id", id)
	}
	return filepath.Join(j.dir, id), nil
}

// writeRow rewrites session.json whole, through a renamed temporary
// file.
//
// @decision(DL-092)
func (j *Journal) writeRow(sess Session, runs []Run) error {
	dir, err := j.sessionDir(sess.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := encodeLine(&buf, sessionFileBody{Format: JournalFormat, Session: sessionRecordOf(sess), Runs: runRecordsOf(runs)}); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, sessionFile), buf.Bytes())
}

// appendRecords appends records to a session's transcript in one write,
// starting the file with its header.
//
// @decision(DL-092)
func (j *Journal) appendRecords(id string, records []any) error {
	dir, err := j.sessionDir(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, transcriptFile)
	var buf bytes.Buffer
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := encodeLine(&buf, headerRecord{Type: "header", Format: JournalFormat, Session: id, Created: time.Now().UTC()}); err != nil {
			return err
		}
	}
	for _, r := range records {
		if err := encodeLine(&buf, r); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(buf.Bytes())
	cerr := f.Close()
	return errors.Join(werr, cerr)
}

// encodeLine writes v as one line, without escaping HTML, so a byte
// field written as JSON comes back byte for byte.
func encodeLine(buf *bytes.Buffer, v any) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// writeAtomic writes path through a temporary file in its directory.
func writeAtomic(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(body)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// load reads every session directory into m.
//
// @decision(DL-092)
func (j *Journal) load(m *Memory) error {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return fmt.Errorf("store: read the session history %s: %w", j.dir, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		if err := j.loadSession(m, filepath.Join(j.dir, e.Name())); err != nil {
			j.warn(fmt.Sprintf("the local session history %s was skipped: %v", e.Name(), err))
		}
	}
	return nil
}

func (j *Journal) loadSession(m *Memory, dir string) error {
	body, err := os.ReadFile(filepath.Join(dir, sessionFile))
	if err != nil {
		return err
	}
	var file sessionFileBody
	if err := json.Unmarshal(body, &file); err != nil {
		return fmt.Errorf("%s: %w", sessionFile, err)
	}
	if file.Format != JournalFormat {
		return fmt.Errorf("%s is format %d, which this build does not read", sessionFile, file.Format)
	}
	sess := file.Session.session()
	if sess.ID != filepath.Base(dir) {
		return fmt.Errorf("%s names session %s", sessionFile, sess.ID)
	}
	m.sessions[sess.ID] = sess
	var runs []Run
	for _, r := range file.Runs {
		runs = append(runs, r.run())
	}
	if len(runs) > 0 {
		m.runs[sess.ID] = runs
	}
	return j.loadTranscript(m, sess.ID, filepath.Join(dir, transcriptFile))
}

// loadTranscript replays a session's transcript: the last record for a
// conversation or message wins, events keep their positions, a torn last
// line is cut off, and a transcript mostly superseded is compacted.
//
// @decision(DL-092)
func (j *Journal) loadTranscript(m *Memory, id, path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var (
		good    int64
		records int
		line    int
		torn    bool
		convs   = map[string]Conversation{}
		msgs    = map[string]Message{}
		evs     []Event
	)
	r := bufio.NewReader(f)
	for {
		raw, rerr := r.ReadBytes('\n')
		if len(raw) == 0 && rerr != nil {
			break
		}
		line++
		complete := rerr == nil
		var rec rawRecord
		if err := json.Unmarshal(raw, &rec); err != nil || !complete {
			if !complete {
				torn = true
				break
			}
			j.warn(fmt.Sprintf("%s line %d is not a record and was skipped", path, line))
			good += int64(len(raw))
			continue
		}
		good += int64(len(raw))
		switch rec.Type {
		case "header":
			if rec.Format != JournalFormat {
				return fmt.Errorf("%s is format %d, which this build does not read", transcriptFile, rec.Format)
			}
		case "conversation":
			records++
			if rec.Conversation != nil {
				c := rec.Conversation.conversation()
				convs[c.ID] = c
			}
		case "message":
			records++
			if rec.Message != nil {
				msg := rec.Message.message()
				msgs[msg.ID] = msg
			}
		case "event":
			records++
			if rec.Event != nil {
				evs = append(evs, rec.Event.event())
			}
		default:
			j.warn(fmt.Sprintf("%s line %d has the unknown type %q and was skipped", path, line, rec.Type))
		}
		if rerr != nil {
			break
		}
	}
	if torn {
		if err := os.Truncate(path, good); err != nil {
			return fmt.Errorf("cut the torn last line of %s: %w", path, err)
		}
		j.warn(fmt.Sprintf("%s ended in a torn line, which was cut off", path))
	}
	for _, c := range convs {
		m.conversations[c.ID] = c
	}
	for _, msg := range msgs {
		m.messages[msg.ID] = msg
		m.messageKeys[convMessage{msg.ConversationID, msg.OpenCodeID}] = msg.ID
	}
	slices.SortFunc(evs, func(a, b Event) int { return cmp.Compare(a.Pos, b.Pos) })
	seen := map[string]bool{}
	for _, ev := range evs {
		if seen[ev.ID] {
			continue
		}
		seen[ev.ID] = true
		m.events[ev.ID] = ev
		m.indexEvent(ev)
		m.eventPos = max(m.eventPos, ev.Pos)
	}
	live := len(convs) + len(msgs) + len(seen)
	if records > 2*live && live > 0 {
		if err := j.compact(id, path, convs, msgs, evs); err != nil {
			j.warn(fmt.Sprintf("%s could not be compacted: %v", path, err))
		}
	}
	return nil
}

// compact rewrites a transcript with only its live rows.
//
// @decision(DL-092)
func (j *Journal) compact(id, path string, convs map[string]Conversation, msgs map[string]Message, evs []Event) error {
	var buf bytes.Buffer
	if err := encodeLine(&buf, headerRecord{Type: "header", Format: JournalFormat, Session: id, Created: time.Now().UTC()}); err != nil {
		return err
	}
	cs := make([]Conversation, 0, len(convs))
	for _, c := range convs {
		cs = append(cs, c)
	}
	slices.SortFunc(cs, func(a, b Conversation) int { return strings.Compare(a.ID, b.ID) })
	for _, c := range cs {
		if err := encodeLine(&buf, conversationRecord(c)); err != nil {
			return err
		}
	}
	ms := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		ms = append(ms, msg)
	}
	slices.SortFunc(ms, compareMessages)
	for _, msg := range ms {
		if err := encodeLine(&buf, messageRecord(msg)); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		if seen[ev.ID] {
			continue
		}
		seen[ev.ID] = true
		if err := encodeLine(&buf, eventRecord(ev)); err != nil {
			return err
		}
	}
	return writeAtomic(path, buf.Bytes())
}

// prune deletes the history of every session stopped longer ago than
// retention, unless one of its runs still has a kept snapshot.
//
// @decision(DL-093)
func (j *Journal) prune(m *Memory, retention time.Duration, now time.Time) {
	if retention <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, sess := range m.sessions {
		if sess.State.Live() || sess.StoppedAt == nil || now.Sub(*sess.StoppedAt) <= retention {
			continue
		}
		if slices.ContainsFunc(m.runs[id], func(r Run) bool { return r.SnapshotState == SnapshotKept }) {
			continue
		}
		dir, err := j.sessionDir(id)
		if err == nil {
			err = os.RemoveAll(dir)
		}
		if err != nil {
			j.warn(fmt.Sprintf("the history of session %s could not be pruned: %v", id, err))
			continue
		}
		m.forgetSessionLocked(id)
	}
}

// forgetSessionLocked removes a session and everything recorded for it.
// The caller holds the lock.
func (m *Memory) forgetSessionLocked(id string) {
	delete(m.sessions, id)
	delete(m.runs, id)
	delete(m.proxyCAs, id)
	for _, evID := range m.eventsBySession[id] {
		ev := m.events[evID]
		delete(m.eventSeqs, convSeq{ev.ConversationID, ev.Seq})
		delete(m.events, evID)
	}
	delete(m.eventsBySession, id)
	for msgID, msg := range m.messages {
		if msg.SessionID == id {
			delete(m.messageKeys, convMessage{msg.ConversationID, msg.OpenCodeID})
			delete(m.messages, msgID)
		}
	}
	for convID, c := range m.conversations {
		if c.SessionID == id {
			delete(m.conversations, convID)
		}
	}
}

// The journal's records. Their JSON names are the format, independent
// of the store's Go types.
//
// @decision(DL-092)
type (
	sessionFileBody struct {
		Format  int           `json:"format"`
		Session sessionRecord `json:"session"`
		Runs    []runRecord   `json:"runs"`
	}
	headerRecord struct {
		Type    string    `json:"type"`
		Format  int       `json:"format"`
		Session string    `json:"session"`
		Created time.Time `json:"created"`
	}
	rawRecord struct {
		Type         string              `json:"type"`
		Format       int                 `json:"format,omitempty"`
		Conversation *conversationFields `json:"conversation,omitempty"`
		Message      *messageFields      `json:"message,omitempty"`
		Event        *eventFields        `json:"event,omitempty"`
	}
)

// blob is a byte field: written as JSON when it holds compact JSON, so
// a reader with jq sees it and it reads back byte for byte, and as
// base64 otherwise.
type blob struct {
	JSON   json.RawMessage `json:"json,omitempty"`
	Base64 string          `json:"base64,omitempty"`
}

func blobOf(b []byte) *blob {
	switch {
	case len(b) == 0:
		return nil
	case compactJSON(b):
		return &blob{JSON: json.RawMessage(bytes.Clone(b))}
	}
	return &blob{Base64: base64.StdEncoding.EncodeToString(b)}
}

// compactJSON reports whether b is JSON the encoder writes unchanged.
func compactJSON(b []byte) bool {
	if !json.Valid(b) {
		return false
	}
	var c bytes.Buffer
	if err := json.Compact(&c, b); err != nil {
		return false
	}
	return bytes.Equal(c.Bytes(), b)
}

func (b *blob) bytes() []byte {
	switch {
	case b == nil:
		return nil
	case len(b.JSON) > 0:
		return bytes.Clone(b.JSON)
	}
	out, _ := base64.StdEncoding.DecodeString(b.Base64)
	return out
}

type sessionRecord struct {
	ID                string                    `json:"id"`
	OrgID             string                    `json:"org_id"`
	OwnerID           string                    `json:"owner_id"`
	Name              string                    `json:"name"`
	State             SessionState              `json:"state"`
	SandboxID         string                    `json:"sandbox_id,omitempty"`
	Request           SessionRequest            `json:"request"`
	Clones            []string                  `json:"clones,omitempty"`
	Landing           string                    `json:"landing,omitempty"`
	Error             string                    `json:"error,omitempty"`
	CreatedAt         time.Time                 `json:"created_at"`
	UpdatedAt         time.Time                 `json:"updated_at"`
	StoppedAt         *time.Time                `json:"stopped_at,omitempty"`
	CancelRequestedAt *time.Time                `json:"cancel_requested_at,omitempty"`
	Credentials       []SessionCredential       `json:"credentials,omitempty"`
	RoutedCredentials []SessionRoutedCredential `json:"routed_credentials,omitempty"`
	GitTokenMintedAt  *time.Time                `json:"git_token_minted_at,omitempty"`
	GitTokenExpiresAt *time.Time                `json:"git_token_expires_at,omitempty"`
	OpenCodeGrant     int                       `json:"opencode_grant,omitempty"`
	Booted            BootedImage               `json:"booted"`
}

// sessionRecordOf and session convert, which needs the record to have
// exactly the row's fields: a field added to Session fails to compile
// here until the journal keeps it too.
func sessionRecordOf(s Session) sessionRecord { return sessionRecord(s) }

func (r sessionRecord) session() Session { return copySession(Session(r)) }

type runRecord struct {
	N             int           `json:"n"`
	OrgID         string        `json:"org_id"`
	SessionID     string        `json:"session_id"`
	OwnerID       string        `json:"owner_id"`
	SandboxID     string        `json:"sandbox_id,omitempty"`
	Booted        string        `json:"booted,omitempty"`
	BootedImageID string        `json:"booted_image_id,omitempty"`
	OpenCode      string        `json:"opencode,omitempty"`
	Origin        RunOrigin     `json:"origin"`
	StartedAt     time.Time     `json:"started_at"`
	RunningAt     *time.Time    `json:"running_at,omitempty"`
	EndedAt       *time.Time    `json:"ended_at,omitempty"`
	Snapshot      string        `json:"snapshot,omitempty"`
	SnapshotState SnapshotState `json:"snapshot_state"`
	SnapshotAt    *time.Time    `json:"snapshot_at,omitempty"`
	FirstMessage  *blob         `json:"first_message,omitempty"`
}

func runRecordsOf(runs []Run) []runRecord {
	out := make([]runRecord, 0, len(runs))
	for _, r := range runs {
		out = append(out, runRecord{
			N: r.N, OrgID: r.OrgID, SessionID: r.SessionID, OwnerID: r.OwnerID, SandboxID: r.SandboxID,
			Booted: r.Booted, BootedImageID: r.BootedImageID, OpenCode: r.OpenCode, Origin: r.Origin,
			StartedAt: r.StartedAt, RunningAt: r.RunningAt, EndedAt: r.EndedAt,
			Snapshot: r.Snapshot, SnapshotState: r.SnapshotState, SnapshotAt: r.SnapshotAt, FirstMessage: blobOf(r.FirstMessage),
		})
	}
	return out
}

func (r runRecord) run() Run {
	return Run{
		N: r.N, OrgID: r.OrgID, SessionID: r.SessionID, OwnerID: r.OwnerID, SandboxID: r.SandboxID,
		Booted: r.Booted, BootedImageID: r.BootedImageID, OpenCode: r.OpenCode, Origin: r.Origin,
		StartedAt: r.StartedAt, RunningAt: r.RunningAt, EndedAt: r.EndedAt,
		Snapshot: r.Snapshot, SnapshotState: r.SnapshotState, SnapshotAt: r.SnapshotAt, FirstMessage: r.FirstMessage.bytes(),
	}
}

type conversationFields struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"org_id"`
	SessionID  string    `json:"session_id"`
	OpenCodeID string    `json:"opencode_id"`
	Title      string    `json:"title,omitempty"`
	Info       *blob     `json:"info,omitempty"`
	SeqBase    int64     `json:"seq_base,omitempty"`
	SeqAfter   int64     `json:"seq_after,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func conversationRecord(c Conversation) rawRecord {
	return rawRecord{Type: "conversation", Conversation: &conversationFields{
		ID: c.ID, OrgID: c.OrgID, SessionID: c.SessionID, OpenCodeID: c.OpenCodeID, Title: c.Title,
		Info: blobOf(c.Info), SeqBase: c.SeqBase, SeqAfter: c.SeqAfter, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}}
}

func (c conversationFields) conversation() Conversation {
	return Conversation{
		ID: c.ID, OrgID: c.OrgID, SessionID: c.SessionID, OpenCodeID: c.OpenCodeID, Title: c.Title,
		Info: c.Info.bytes(), SeqBase: c.SeqBase, SeqAfter: c.SeqAfter, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

type messageFields struct {
	ID             string    `json:"id"`
	OrgID          string    `json:"org_id"`
	SessionID      string    `json:"session_id"`
	ConversationID string    `json:"conversation_id"`
	OpenCodeID     string    `json:"opencode_id"`
	Role           string    `json:"role,omitempty"`
	Text           string    `json:"text,omitempty"`
	At             time.Time `json:"at"`
	Raw            *blob     `json:"raw,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

func messageRecord(msg Message) rawRecord {
	return rawRecord{Type: "message", Message: &messageFields{
		ID: msg.ID, OrgID: msg.OrgID, SessionID: msg.SessionID, ConversationID: msg.ConversationID, OpenCodeID: msg.OpenCodeID,
		Role: msg.Role, Text: msg.Text, At: msg.At, Raw: blobOf(msg.Raw), CreatedAt: msg.CreatedAt,
	}}
}

func (m messageFields) message() Message {
	raw := m.Raw.bytes()
	if raw == nil {
		raw = []byte{}
	}
	return Message{
		ID: m.ID, OrgID: m.OrgID, SessionID: m.SessionID, ConversationID: m.ConversationID, OpenCodeID: m.OpenCodeID,
		Role: m.Role, Text: m.Text, At: m.At, Raw: raw, CreatedAt: m.CreatedAt,
	}
}

type eventFields struct {
	ID             string      `json:"id"`
	OrgID          string      `json:"org_id"`
	SessionID      string      `json:"session_id"`
	ConversationID string      `json:"conversation_id,omitempty"`
	Source         EventSource `json:"source"`
	Seq            int64       `json:"seq,omitempty"`
	Pos            int64       `json:"pos"`
	Name           string      `json:"name"`
	Text           string      `json:"text,omitempty"`
	At             time.Time   `json:"at"`
	Raw            *blob       `json:"raw,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
}

func eventRecord(ev Event) rawRecord {
	return rawRecord{Type: "event", Event: &eventFields{
		ID: ev.ID, OrgID: ev.OrgID, SessionID: ev.SessionID, ConversationID: ev.ConversationID, Source: ev.Source,
		Seq: ev.Seq, Pos: ev.Pos, Name: ev.Name, Text: ev.Text, At: ev.At, Raw: blobOf(ev.Raw), CreatedAt: ev.CreatedAt,
	}}
}

func (e eventFields) event() Event {
	raw := e.Raw.bytes()
	if raw == nil {
		raw = []byte{}
	}
	return Event{
		ID: e.ID, OrgID: e.OrgID, SessionID: e.SessionID, ConversationID: e.ConversationID, Source: e.Source,
		Seq: e.Seq, Pos: e.Pos, Name: e.Name, Text: e.Text, At: e.At, Raw: raw, CreatedAt: e.CreatedAt,
	}
}
