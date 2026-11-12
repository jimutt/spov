package store

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Memory is the store local mode runs on. It holds everything in process
// and forgets it at exit; Modal tags are the durable record there.
//
// @decision(DL-003)
type Memory struct {
	// Now supplies timestamps; nil means time.Now.
	Now func() time.Time

	mu          sync.Mutex
	sessions    map[string]Session
	mints       map[string]Mint
	users       map[string]User
	tokens      map[string]APIToken
	deviceCodes map[string]DeviceCode
	credentials map[string]UserCredential
	allowlist   map[string]AllowlistEntry
	// @decision(DL-042)
	orgCredentials map[string]OrgCredential
	adminEvents    []AdminEvent
	// @decision(DL-017)
	conversations map[string]Conversation
	messages      map[string]Message
	events        map[string]Event
	// The indexes over messages and events, so a write or a read of a
	// long session costs no scan of every row. They are derived, and
	// shared with a transaction's view like the maps.
	//
	// @decision(DL-071)
	eventsBySession map[string][]string // event IDs in Pos order
	eventSeqs       map[convSeq]bool    // the OpenCode sequences stored
	messageKeys     map[convMessage]string
	// messagesBySession is each session's message IDs.
	//
	// @decision(DL-131)
	messagesBySession map[string]map[string]struct{}
	// proxyCAs are the sealed proxy CAs, by session ID.
	//
	// @decision(DL-034)
	proxyCAs map[string]proxyCA
	// @decision(DL-045)
	jobs map[string]Job
	// runs are each session's runs, oldest first, by session ID.
	//
	// @decision(DL-085)
	runs map[string][]Run
	// deliverables and jobFiles are keyed by job ID, then by key or path.
	deliverables map[string]map[string]Deliverable
	jobFiles     map[string]map[string]JobFile
	// jobSeeds are sync jobs' seed bundles, by job ID.
	//
	// @decision(DL-100)
	jobSeeds map[string][]byte
	// eventPos is the last Pos handed out; it only ever grows, which is
	// what makes Pos a cursor.
	eventPos int64
	// undo is a transaction view's journal: one entry per write to the
	// conversations, messages or events, run newest first on rollback.
	// Those maps grow with a session, so they are journalled rather than
	// copied at every transaction.
	//
	// @decision(DL-071)
	undo *[]func()
	// inTx marks a store handed to a Tx callback: it holds the lock
	// already, so its methods must not take it again. done is set when
	// the callback returns, so a retained view fails instead of racing.
	inTx bool
	done bool
	// sink receives every committed write the local journal keeps, and
	// pending holds a transaction view's until it commits. Both are nil
	// unless OpenJournal set a sink.
	//
	// @decision(DL-091)
	sink    func([]change)
	pending *[]change
}

// NewMemory returns an empty memory store.
func NewMemory() *Memory {
	return &Memory{
		sessions:        map[string]Session{},
		proxyCAs:        map[string]proxyCA{},
		mints:           map[string]Mint{},
		users:           map[string]User{},
		tokens:          map[string]APIToken{},
		deviceCodes:     map[string]DeviceCode{},
		credentials:     map[string]UserCredential{},
		allowlist:       map[string]AllowlistEntry{},
		orgCredentials:  map[string]OrgCredential{},
		conversations:   map[string]Conversation{},
		messages:        map[string]Message{},
		events:          map[string]Event{},
		eventsBySession: map[string][]string{},
		eventSeqs:       map[convSeq]bool{},
		messageKeys:     map[convMessage]string{},
		jobs:            map[string]Job{},
		// @decision(DL-131)
		messagesBySession: map[string]map[string]struct{}{},
		runs:              map[string][]Run{},
		deliverables:      map[string]map[string]Deliverable{},
		jobFiles:          map[string]map[string]JobFile{},
		jobSeeds:          map[string][]byte{},
	}
}

func (m *Memory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	// UTC to match what Postgres hands back, so a caller comparing
	// timestamps behaves the same on either store.
	return time.Now().UTC()
}

func (m *Memory) lock() func() {
	if m.inTx {
		if m.done {
			panic("store: a Tx view was used after the transaction ended")
		}
		return func() {}
	}
	m.mu.Lock()
	return m.mu.Unlock
}

// Sessions implements Store.
func (m *Memory) Sessions() SessionStore { return memSessions{m} }

// Mints implements Store.
func (m *Memory) Mints() MintStore { return memMints{m} }

// Users implements Store.
//
// @decision(DL-003)
func (m *Memory) Users() UserStore { return memUsers{m} }

// Tokens implements Store.
//
// @decision(DL-006)
func (m *Memory) Tokens() TokenStore { return memTokens{m} }

// DeviceCodes implements Store.
//
// @decision(DL-006)
func (m *Memory) DeviceCodes() DeviceCodeStore { return memDeviceCodes{m} }

// Credentials implements Store.
//
// @decision(DL-008)
func (m *Memory) Credentials() CredentialStore { return memCredentials{m} }

// OrgCredentials implements Store.
//
// @decision(DL-042)
func (m *Memory) OrgCredentials() OrgCredentialStore { return memOrgCredentials{m} }

// AdminEvents implements Store.
//
// @decision(DL-042)
func (m *Memory) AdminEvents() AdminEventStore { return memAdminEvents{m} }

// Allowlist implements Store.
//
// @decision(DL-007)
func (m *Memory) Allowlist() AllowlistStore { return memAllowlist{m} }

// Conversations implements Store.
//
// @decision(DL-017)
func (m *Memory) Conversations() ConversationStore { return memConversations{m} }

// Jobs implements Store.
//
// @decision(DL-045)
func (m *Memory) Jobs() JobStore { return memJobs{m} }

// Runs implements Store.
//
// @decision(DL-085)
func (m *Memory) Runs() RunStore { return memRuns{m} }

// Ping implements Store.
func (m *Memory) Ping(context.Context) error { return nil }

// Tx implements Store. The callback sees a view that shares the maps and
// the held lock; a failing or panicking callback rolls the maps back, and
// the view is dead once the callback returns.
func (m *Memory) Tx(_ context.Context, fn func(Store) error) (err error) {
	if m.inTx {
		return ErrNestedTx
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sessions, mints := cloneMap(m.sessions), cloneMap(m.mints)
	users, tokens := cloneMap(m.users), cloneMap(m.tokens)
	codes, creds := cloneMap(m.deviceCodes), cloneMap(m.credentials)
	allowed := cloneMap(m.allowlist)
	orgCreds, admin := cloneMap(m.orgCredentials), slices.Clone(m.adminEvents)
	jobs, seeds := cloneMap(m.jobs), cloneMap(m.jobSeeds)
	runs := cloneRuns(m.runs)
	var undo []func()
	var pending []change
	pos := m.eventPos
	view := &Memory{Now: m.Now, sessions: m.sessions, mints: m.mints,
		users: m.users, tokens: m.tokens, deviceCodes: m.deviceCodes,
		credentials: m.credentials, allowlist: m.allowlist,
		orgCredentials: m.orgCredentials, adminEvents: m.adminEvents,
		conversations: m.conversations, messages: m.messages, events: m.events,
		eventsBySession: m.eventsBySession, eventSeqs: m.eventSeqs, messageKeys: m.messageKeys,
		messagesBySession: m.messagesBySession,
		jobs:              m.jobs, runs: m.runs, eventPos: m.eventPos, undo: &undo, inTx: true, pending: &pending,
		jobSeeds: m.jobSeeds}
	committed := false
	defer func() {
		view.done = true
		if committed {
			// The view handed out positions the parent must not reuse.
			m.eventPos = view.eventPos
			// A slice appended to in the view is the view's own header.
			m.adminEvents = view.adminEvents
			// @decision(DL-091)
			if m.sink != nil && len(pending) > 0 {
				m.sink(pending)
			}
			return
		}
		m.sessions, m.mints = sessions, mints
		m.users, m.tokens = users, tokens
		m.deviceCodes, m.credentials = codes, creds
		m.allowlist = allowed
		m.orgCredentials, m.adminEvents = orgCreds, admin
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		m.jobs, m.jobSeeds = jobs, seeds
		m.runs = runs
		m.eventPos = pos
	}()
	if err := fn(view); err != nil {
		return err
	}
	committed = true
	return nil
}

// convSeq and convMessage are the keys of the memory store's indexes.
type convSeq struct {
	conversation string
	seq          int64
}

type convMessage struct {
	conversation, opencodeID string
}

// journal records how to undo a write, inside a transaction only.
func (m *Memory) journal(undo func()) {
	if m.undo != nil {
		*m.undo = append(*m.undo, undo)
	}
}

// indexEvent adds a stored event to the indexes. Events are stored in
// Pos order, so appending keeps each session's list sorted.
func (m *Memory) indexEvent(e Event) {
	m.eventsBySession[e.SessionID] = append(m.eventsBySession[e.SessionID], e.ID)
	if e.Source == SourceOpenCode {
		m.eventSeqs[convSeq{e.ConversationID, e.Seq}] = true
	}
	m.journal(func() {
		delete(m.events, e.ID)
		ids := m.eventsBySession[e.SessionID]
		m.eventsBySession[e.SessionID] = ids[:len(ids)-1]
		if e.Source == SourceOpenCode {
			delete(m.eventSeqs, convSeq{e.ConversationID, e.Seq})
		}
	})
}

// indexMessage adds a newly stored message to the session's index.
//
// @decision(DL-131)
func (m *Memory) indexMessage(msg Message) {
	ids := m.messagesBySession[msg.SessionID]
	if ids == nil {
		ids = map[string]struct{}{}
		m.messagesBySession[msg.SessionID] = ids
	}
	ids[msg.ID] = struct{}{}
}

// sessionMessages is a session's messages, in no order.
//
// @decision(DL-131)
func (m *Memory) sessionMessages(orgID, sessionID string) []Message {
	ids := m.messagesBySession[sessionID]
	out := make([]Message, 0, len(ids))
	for id := range ids {
		if msg := m.messages[id]; msg.OrgID == orgID {
			out = append(out, msg)
		}
	}
	return out
}

func cloneMap[V any](in map[string]V) map[string]V {
	out := make(map[string]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type memSessions struct{ m *Memory }

func (s memSessions) Create(_ context.Context, sess Session) error {
	defer s.m.lock()()
	if _, ok := s.m.sessions[sess.ID]; ok {
		return ErrConflict
	}
	if sess.State.Live() && s.liveByName(sess.OrgID, sess.Name) != nil {
		return ErrConflict
	}
	now := s.m.now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	// Only SetGitToken writes these, as in the Postgres store.
	//
	// @decision(DL-021)
	sess.GitTokenMintedAt, sess.GitTokenExpiresAt = nil, nil
	sess.OpenCodeGrant = 0
	s.m.sessions[sess.ID] = copySession(sess)
	s.m.emitRow(sess.ID)
	return nil
}

func (s memSessions) liveByName(orgID, name string) *Session {
	if name == "" {
		return nil
	}
	for _, sess := range s.m.sessions {
		if sess.OrgID == orgID && sess.Name == name && sess.State.Live() {
			return &sess
		}
	}
	return nil
}

// @decision(DL-101)
func (s memSessions) ByIDs(_ context.Context, orgID string, ids []string) (map[string]Session, error) {
	defer s.m.lock()()
	out := make(map[string]Session, len(ids))
	for _, id := range ids {
		if sess, ok := s.m.sessions[id]; ok && sess.OrgID == orgID {
			out[id] = copySession(sess)
		}
	}
	return out, nil
}

func (s memSessions) Get(_ context.Context, orgID, ref string) (Session, error) {
	defer s.m.lock()()
	if sess, ok := s.m.sessions[ref]; ok && sess.OrgID == orgID {
		return copySession(sess), nil
	}
	if sess := s.liveByName(orgID, ref); sess != nil {
		return copySession(*sess), nil
	}
	return Session{}, ErrNotFound
}

// Lock only checks the session exists: a memory Tx already holds the
// store's lock for its whole length.
//
// @decision(DL-065)
func (s memSessions) Lock(_ context.Context, orgID, id string) error {
	defer s.m.lock()()
	if sess, ok := s.m.sessions[id]; ok && sess.OrgID == orgID {
		return nil
	}
	return ErrNotFound
}

// @decision(DL-012)
func (s memSessions) List(_ context.Context, orgID string, f SessionFilter) ([]Session, error) {
	defer s.m.lock()()
	var out []Session
	for _, sess := range s.m.sessions {
		if sess.OrgID == orgID && f.matches(sess) {
			out = append(out, copySession(sess))
		}
	}
	slices.SortFunc(out, func(a, b Session) int { return strings.Compare(b.ID, a.ID) })
	return firstN(out, f.Limit), nil
}

func (s memSessions) Count(ctx context.Context, orgID string, f SessionFilter) (int, error) {
	f.Before, f.Limit = "", 0
	all, err := s.List(ctx, orgID, f)
	return len(all), err
}

// firstN keeps the first n of a list, or all of it when n is not
// positive.
func firstN[T any](xs []T, n int) []T {
	if n > 0 && len(xs) > n {
		return xs[:n]
	}
	return xs
}

// MarkStopped moves a live session to a stopped state, if it still is.
//
// @decision(DL-011)
func (s memSessions) MarkStopped(_ context.Context, orgID, id string, state SessionState, at time.Time, errText string) (bool, error) {
	defer s.m.lock()()
	cur, ok := s.m.sessions[id]
	if !ok || cur.OrgID != orgID || !cur.State.Live() {
		return false, nil
	}
	cur.State, cur.Error, cur.UpdatedAt = state, errText, s.m.now()
	stopped := at
	cur.StoppedAt = &stopped
	s.m.sessions[id] = copySession(cur)
	// @decision(DL-085)
	if runs := s.m.runs[id]; len(runs) > 0 && runs[len(runs)-1].EndedAt == nil {
		runs[len(runs)-1].EndedAt = &stopped
	}
	s.m.emitRow(id)
	// @decision(DL-034)
	delete(s.m.proxyCAs, id)
	return true, nil
}

func (s memSessions) Update(_ context.Context, sess Session) error {
	defer s.m.lock()()
	cur, ok := s.m.sessions[sess.ID]
	if !ok || cur.OrgID != sess.OrgID {
		return ErrNotFound
	}
	if sess.State.Live() {
		if other := s.liveByName(sess.OrgID, sess.Name); other != nil && other.ID != sess.ID {
			return ErrConflict
		}
	}
	sess.CreatedAt = cur.CreatedAt
	sess.UpdatedAt = s.m.now()
	// The cancel flag belongs to whoever set it, not to the caller's
	// copy of the row: a launch that read the session before a delete
	// arrived would otherwise write the flag back out.
	//
	// @decision(DL-003)
	sess.CancelRequestedAt = cur.CancelRequestedAt
	// The same for the git token's times, which SetGitToken owns.
	//
	// @decision(DL-021)
	sess.GitTokenMintedAt, sess.GitTokenExpiresAt = cur.GitTokenMintedAt, cur.GitTokenExpiresAt
	// @decision(DL-015)
	sess.OpenCodeGrant = cur.OpenCodeGrant
	s.m.sessions[sess.ID] = copySession(sess)
	s.m.emitRow(sess.ID)
	return nil
}

// SetGitToken records the session's current renewable git token.
//
// @decision(DL-021)
func (s memSessions) SetGitToken(_ context.Context, orgID, id string, minted, expires time.Time) error {
	defer s.m.lock()()
	cur, ok := s.m.sessions[id]
	if !ok || cur.OrgID != orgID {
		return ErrNotFound
	}
	m, e := minted, expires
	cur.GitTokenMintedAt, cur.GitTokenExpiresAt = &m, &e
	cur.UpdatedAt = s.m.now()
	s.m.sessions[id] = cur
	s.m.emitRow(id)
	return nil
}

// proxyCA is one session's sealed proxy CA and its org.
type proxyCA struct {
	orgID string
	ca    SealedValue
}

// SetProxyCA keeps the session's sealed proxy CA, until MarkStopped.
//
// @decision(DL-034)
func (s memSessions) SetProxyCA(_ context.Context, orgID, id string, ca SealedValue) error {
	defer s.m.lock()()
	if cur, ok := s.m.sessions[id]; !ok || cur.OrgID != orgID {
		return ErrNotFound
	}
	s.m.proxyCAs[id] = proxyCA{orgID: orgID, ca: SealedValue{
		Ciphertext: bytes.Clone(ca.Ciphertext), Nonce: bytes.Clone(ca.Nonce), KeyVersion: ca.KeyVersion,
	}}
	return nil
}

// ProxyCA reads the session's sealed proxy CA back.
//
// @decision(DL-034)
func (s memSessions) ProxyCA(_ context.Context, orgID, id string) (SealedValue, error) {
	defer s.m.lock()()
	got, ok := s.m.proxyCAs[id]
	if !ok || got.orgID != orgID {
		return SealedValue{}, ErrNotFound
	}
	return SealedValue{Ciphertext: bytes.Clone(got.ca.Ciphertext), Nonce: bytes.Clone(got.ca.Nonce), KeyVersion: got.ca.KeyVersion}, nil
}

// SetOpenCodeGrant records which OpenCode Console grant generation the
// session holds.
//
// @decision(DL-015)
func (s memSessions) SetOpenCodeGrant(_ context.Context, orgID, id string, generation int) error {
	defer s.m.lock()()
	cur, ok := s.m.sessions[id]
	if !ok || cur.OrgID != orgID {
		return ErrNotFound
	}
	cur.OpenCodeGrant = generation
	cur.UpdatedAt = s.m.now()
	s.m.sessions[id] = cur
	s.m.emitRow(id)
	return nil
}

// CountLive counts a user's starting and running sessions, excluding
// the ones an unfinished job owns: those are counted by
// JobStore.CountLive against their own limit.
//
// The job's state decides it, not merely the existence of a job row. A
// terminal job stops counting against the job quota, so a session still
// running under one — a terminate that failed, leaving a billing
// sandbox — would otherwise be counted by neither limit.
//
// @decision(DL-009) @decision(DL-045)
func (s memSessions) CountLive(_ context.Context, orgID, ownerID string) (int, error) {
	defer s.m.lock()()
	jobSessions := make(map[string]bool, len(s.m.jobs))
	for _, j := range s.m.jobs {
		// @decision(DL-101)
		if j.OrgID == orgID && (j.State.Counts() || SyncHeld(j, s.m.sessions[j.SessionID])) {
			jobSessions[j.SessionID] = true
		}
	}
	n := 0
	for _, sess := range s.m.sessions {
		if sess.OrgID == orgID && sess.OwnerID == ownerID && sess.State.Live() && !jobSessions[sess.ID] {
			n++
		}
	}
	return n, nil
}

// RequestCancel records a cancel against a starting session.
//
// @decision(DL-003)
func (s memSessions) RequestCancel(_ context.Context, orgID, id string, at time.Time) (bool, error) {
	defer s.m.lock()()
	cur, ok := s.m.sessions[id]
	if !ok || cur.OrgID != orgID || cur.State != StateStarting {
		return false, nil
	}
	when := at
	cur.CancelRequestedAt = &when
	cur.UpdatedAt = s.m.now()
	s.m.sessions[id] = cur
	s.m.emitRow(id)
	return true, nil
}

// CancelRequested reports whether a cancel was recorded.
//
// @decision(DL-003)
func (s memSessions) CancelRequested(_ context.Context, orgID, id string) (bool, error) {
	defer s.m.lock()()
	cur, ok := s.m.sessions[id]
	if !ok || cur.OrgID != orgID {
		return false, ErrNotFound
	}
	return cur.CancelRequestedAt != nil, nil
}

func (s memSessions) BySandbox(_ context.Context, sandboxID string) (Session, error) {
	defer s.m.lock()()
	if sandboxID == "" {
		return Session{}, ErrNotFound
	}
	for _, sess := range s.m.sessions {
		if sess.SandboxID == sandboxID {
			return copySession(sess), nil
		}
	}
	return Session{}, ErrNotFound
}

func (s memSessions) ListLive(_ context.Context) ([]Session, error) {
	defer s.m.lock()()
	var out []Session
	for _, sess := range s.m.sessions {
		if sess.State.Live() {
			out = append(out, copySession(sess))
		}
	}
	slices.SortFunc(out, func(a, b Session) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// copySession deep-copies every reference field of Session. A new slice
// or map field on Session must be added here, and to the "returned
// sessions are copies" contract case.
func copySession(s Session) Session {
	s.Clones = slices.Clone(s.Clones)
	s.Request.Repos = slices.Clone(s.Request.Repos)
	s.Request.Sandbox.Regions = slices.Clone(s.Request.Sandbox.Regions)
	s.Request.Sandbox.InboundCIDRs = slices.Clone(s.Request.Sandbox.InboundCIDRs)
	// @decision(DL-033)
	s.Request.Addons = slices.Clone(s.Request.Addons)
	s.Request.Apt = slices.Clone(s.Request.Apt)
	// @decision(DL-096)
	s.Request.Env = maps.Clone(s.Request.Env)
	// @decision(DL-027)
	if s.Request.Sync != nil {
		sync := *s.Request.Sync
		sync.Bundle = nil
		s.Request.Sync = &sync
	}
	s.Booted.Addons = slices.Clone(s.Booted.Addons)
	// @decision(DL-075)
	s.RoutedCredentials = slices.Clone(s.RoutedCredentials)
	for i := range s.RoutedCredentials {
		s.RoutedCredentials[i].Hosts = slices.Clone(s.RoutedCredentials[i].Hosts)
		s.RoutedCredentials[i].Paths = slices.Clone(s.RoutedCredentials[i].Paths)
	}
	s.Request.Routed.Names = slices.Clone(s.Request.Routed.Names)
	s.Request.Routed.Preferred = slices.Clone(s.Request.Routed.Preferred)
	s.Request.Routed.Needs = slices.Clone(s.Request.Routed.Needs)
	if s.StoppedAt != nil {
		t := *s.StoppedAt
		s.StoppedAt = &t
	}
	// @decision(DL-003)
	if s.CancelRequestedAt != nil {
		t := *s.CancelRequestedAt
		s.CancelRequestedAt = &t
	}
	// @decision(DL-021) @decision(DL-043)
	s.Credentials = slices.Clone(s.Credentials)
	if s.GitTokenMintedAt != nil {
		t := *s.GitTokenMintedAt
		s.GitTokenMintedAt = &t
	}
	if s.GitTokenExpiresAt != nil {
		t := *s.GitTokenExpiresAt
		s.GitTokenExpiresAt = &t
	}
	return s
}

type memMints struct{ m *Memory }

func (s memMints) Create(_ context.Context, mint Mint) error {
	defer s.m.lock()()
	if _, ok := s.m.mints[mint.ID]; ok {
		return ErrConflict
	}
	if mint.CreatedAt.IsZero() {
		mint.CreatedAt = s.m.now()
	}
	s.m.mints[mint.ID] = mint
	return nil
}

func (s memMints) ListBySession(_ context.Context, orgID, sessionID string) ([]Mint, error) {
	defer s.m.lock()()
	var out []Mint
	for _, mint := range s.m.mints {
		if mint.OrgID == orgID && mint.SessionID == sessionID {
			out = append(out, mint)
		}
	}
	slices.SortFunc(out, func(a, b Mint) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (s memMints) SetExpiry(_ context.Context, orgID, mintID string, expires time.Time) error {
	defer s.m.lock()()
	mint, ok := s.m.mints[mintID]
	if !ok || mint.OrgID != orgID {
		return ErrNotFound
	}
	mint.ExpiresAt = expires
	s.m.mints[mintID] = mint
	return nil
}

func (s memMints) CountSince(_ context.Context, orgID, sessionID string, t time.Time) (int, error) {
	defer s.m.lock()()
	n := 0
	for _, mint := range s.m.mints {
		if mint.OrgID == orgID && mint.SessionID == sessionID && !mint.CreatedAt.Before(t) {
			n++
		}
	}
	return n, nil
}

type memUsers struct{ m *Memory }

func (s memUsers) Create(_ context.Context, u User) error {
	defer s.m.lock()()
	if _, ok := s.m.users[u.ID]; ok {
		return ErrConflict
	}
	if s.byEmail(u.OrgID, u.Email) != nil {
		return ErrConflict
	}
	// @decision(DL-128)
	if u.GithubID != 0 && s.byGithubID(u.OrgID, u.GithubID) != nil {
		return ErrConflict
	}
	now := s.m.now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	s.m.users[u.ID] = copyUser(u)
	return nil
}

func (s memUsers) byEmail(orgID, email string) *User {
	if email == "" {
		return nil
	}
	for _, u := range s.m.users {
		if u.OrgID == orgID && strings.EqualFold(u.Email, email) {
			return &u
		}
	}
	return nil
}

func (s memUsers) byGithubID(orgID string, githubID int64) *User {
	if githubID == 0 {
		return nil
	}
	for _, u := range s.m.users {
		if u.OrgID == orgID && u.GithubID == githubID {
			return &u
		}
	}
	return nil
}

// @decision(DL-128)
func (s memUsers) ByGithubID(_ context.Context, orgID string, githubID int64) (User, error) {
	defer s.m.lock()()
	if u := s.byGithubID(orgID, githubID); u != nil {
		return copyUser(*u), nil
	}
	return User{}, ErrNotFound
}

// @decision(DL-128)
func (s memUsers) ByGithubLogin(_ context.Context, orgID, login string) ([]User, error) {
	defer s.m.lock()()
	var out []User
	for _, u := range s.m.users {
		if u.OrgID == orgID && u.GithubID != 0 && login != "" && strings.EqualFold(u.GithubLogin, login) {
			out = append(out, copyUser(u))
		}
	}
	slices.SortFunc(out, func(a, b User) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// @decision(DL-128)
func (s memUsers) BindGithub(_ context.Context, orgID, userID string, githubID int64) error {
	defer s.m.lock()()
	u, ok := s.m.users[userID]
	if !ok || u.OrgID != orgID {
		return ErrNotFound
	}
	switch {
	case u.GithubID == githubID:
		return nil
	case u.GithubID != 0, githubID == 0:
		return ErrConflict
	}
	if other := s.byGithubID(orgID, githubID); other != nil {
		return ErrConflict
	}
	u.GithubID = githubID
	u.UpdatedAt = s.m.now()
	s.m.users[userID] = u
	return nil
}

func (s memUsers) Get(_ context.Context, orgID, id string) (User, error) {
	defer s.m.lock()()
	if u, ok := s.m.users[id]; ok && u.OrgID == orgID {
		return copyUser(u), nil
	}
	return User{}, ErrNotFound
}

func (s memUsers) ByEmail(_ context.Context, orgID, email string) (User, error) {
	defer s.m.lock()()
	if u := s.byEmail(orgID, email); u != nil {
		return copyUser(*u), nil
	}
	return User{}, ErrNotFound
}

func (s memUsers) List(_ context.Context, orgID string) ([]User, error) {
	defer s.m.lock()()
	var out []User
	for _, u := range s.m.users {
		if u.OrgID == orgID {
			out = append(out, copyUser(u))
		}
	}
	slices.SortFunc(out, func(a, b User) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (s memUsers) Update(_ context.Context, u User) error {
	defer s.m.lock()()
	cur, ok := s.m.users[u.ID]
	if !ok || cur.OrgID != u.OrgID {
		return ErrNotFound
	}
	if other := s.byEmail(u.OrgID, u.Email); other != nil && other.ID != u.ID {
		return ErrConflict
	}
	u.CreatedAt = cur.CreatedAt
	// Only Create and BindGithub set the account.
	//
	// @decision(DL-128)
	u.GithubID = cur.GithubID
	u.UpdatedAt = s.m.now()
	s.m.users[u.ID] = copyUser(u)
	return nil
}

func (s memUsers) Delete(_ context.Context, orgID, id string) error {
	defer s.m.lock()()
	u, ok := s.m.users[id]
	if !ok || u.OrgID != orgID {
		return ErrNotFound
	}
	delete(s.m.users, id)
	return nil
}

// copyUser deep-copies every reference field of User. A new pointer
// field on User must be added here, and to the "returned users are
// copies" contract case — a shared pointer lets a caller's mutation
// reach the store, and survives a rolled-back Tx.
//
// @decision(DL-009)
func copyUser(u User) User {
	if u.LastLoginAt != nil {
		t := *u.LastLoginAt
		u.LastLoginAt = &t
	}
	u.Quota = u.Quota.Clone()
	return u
}

type memTokens struct{ m *Memory }

func (s memTokens) Create(_ context.Context, t APIToken) error {
	defer s.m.lock()()
	if _, ok := s.m.tokens[t.ID]; ok {
		return ErrConflict
	}
	if len(t.Hash) == 0 {
		return ErrConflict
	}
	for _, other := range s.m.tokens {
		if bytes.Equal(other.Hash, t.Hash) {
			return ErrConflict
		}
	}
	now := s.m.now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	s.m.tokens[t.ID] = copyToken(t)
	return nil
}

func (s memTokens) ByHash(_ context.Context, hash []byte) (APIToken, error) {
	defer s.m.lock()()
	if len(hash) == 0 {
		return APIToken{}, ErrNotFound
	}
	for _, t := range s.m.tokens {
		if bytes.Equal(t.Hash, hash) {
			return copyToken(t), nil
		}
	}
	return APIToken{}, ErrNotFound
}

// ByID implements TokenStore.
//
// @decision(DL-006)
func (s memTokens) ByID(_ context.Context, orgID, id string) (APIToken, error) {
	defer s.m.lock()()
	t, ok := s.m.tokens[id]
	if !ok || t.OrgID != orgID {
		return APIToken{}, ErrNotFound
	}
	return copyToken(t), nil
}

func (s memTokens) ListByUser(_ context.Context, orgID, userID string) ([]APIToken, error) {
	defer s.m.lock()()
	var out []APIToken
	for _, t := range s.m.tokens {
		if t.OrgID == orgID && t.UserID == userID {
			out = append(out, copyToken(t))
		}
	}
	slices.SortFunc(out, func(a, b APIToken) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (s memTokens) Touch(_ context.Context, orgID, id string, at time.Time) error {
	defer s.m.lock()()
	t, ok := s.m.tokens[id]
	if !ok || t.OrgID != orgID {
		return ErrNotFound
	}
	t.LastUsedAt = &at
	s.m.tokens[id] = copyToken(t)
	return nil
}

func (s memTokens) Revoke(_ context.Context, orgID, id string, at time.Time) error {
	defer s.m.lock()()
	t, ok := s.m.tokens[id]
	if !ok || t.OrgID != orgID {
		return ErrNotFound
	}
	if t.RevokedAt == nil {
		t.RevokedAt = &at
	}
	t.UpdatedAt = s.m.now()
	s.m.tokens[id] = copyToken(t)
	return nil
}

func copyToken(t APIToken) APIToken {
	t.Hash = slices.Clone(t.Hash)
	t.LastUsedAt = copyTime(t.LastUsedAt)
	t.ExpiresAt = copyTime(t.ExpiresAt)
	t.RevokedAt = copyTime(t.RevokedAt)
	return t
}

// copyBytes detaches a byte slice and never returns nil, so a row
// stored without a frame reads back the same empty value Postgres
// gives rather than a nil the caller must special-case.
func copyBytes(b []byte) []byte {
	return append([]byte{}, b...)
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

type memDeviceCodes struct{ m *Memory }

func (s memDeviceCodes) Create(_ context.Context, d DeviceCode) error {
	defer s.m.lock()()
	if _, ok := s.m.deviceCodes[d.ID]; ok {
		return ErrConflict
	}
	if len(d.Hash) == 0 || d.UserCode == "" {
		return ErrConflict
	}
	for _, other := range s.m.deviceCodes {
		if bytes.Equal(other.Hash, d.Hash) || other.UserCode == d.UserCode {
			return ErrConflict
		}
	}
	now := s.m.now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now
	s.m.deviceCodes[d.ID] = copyDeviceCode(d)
	return nil
}

func (s memDeviceCodes) ByHash(_ context.Context, hash []byte) (DeviceCode, error) {
	defer s.m.lock()()
	if len(hash) == 0 {
		return DeviceCode{}, ErrNotFound
	}
	for _, d := range s.m.deviceCodes {
		if bytes.Equal(d.Hash, hash) {
			return copyDeviceCode(d), nil
		}
	}
	return DeviceCode{}, ErrNotFound
}

func (s memDeviceCodes) ByUserCode(_ context.Context, userCode string) (DeviceCode, error) {
	defer s.m.lock()()
	if userCode == "" {
		return DeviceCode{}, ErrNotFound
	}
	for _, d := range s.m.deviceCodes {
		if d.UserCode == userCode {
			return copyDeviceCode(d), nil
		}
	}
	return DeviceCode{}, ErrNotFound
}

func (s memDeviceCodes) Update(_ context.Context, d DeviceCode) error {
	defer s.m.lock()()
	cur, ok := s.m.deviceCodes[d.ID]
	if !ok {
		return ErrNotFound
	}
	d.CreatedAt = cur.CreatedAt
	d.UpdatedAt = s.m.now()
	s.m.deviceCodes[d.ID] = copyDeviceCode(d)
	return nil
}

func (s memDeviceCodes) Delete(_ context.Context, id string) error {
	defer s.m.lock()()
	if _, ok := s.m.deviceCodes[id]; !ok {
		return ErrNotFound
	}
	delete(s.m.deviceCodes, id)
	return nil
}

func (s memDeviceCodes) DeleteExpired(_ context.Context, before time.Time) (int, error) {
	defer s.m.lock()()
	n := 0
	for id, d := range s.m.deviceCodes {
		if d.ExpiresAt.Before(before) {
			delete(s.m.deviceCodes, id)
			n++
		}
	}
	return n, nil
}

func copyDeviceCode(d DeviceCode) DeviceCode {
	d.Hash = slices.Clone(d.Hash)
	d.LastPolledAt = copyTime(d.LastPolledAt)
	return d
}

type memCredentials struct{ m *Memory }

// credKey is the (user, platform) uniqueness the schema enforces.
func credKey(orgID, userID, platform string) string {
	return orgID + "\x00" + userID + "\x00" + platform
}

func (s memCredentials) Put(_ context.Context, c UserCredential) error {
	defer s.m.lock()()
	key := credKey(c.OrgID, c.UserID, c.Platform)
	now := s.m.now()
	if cur, ok := s.m.credentials[key]; ok {
		c.ID = cur.ID
		c.CreatedAt = cur.CreatedAt
	} else if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	s.m.credentials[key] = copyCredential(c)
	return nil
}

func (s memCredentials) Get(_ context.Context, orgID, userID, platform string) (UserCredential, error) {
	defer s.m.lock()()
	c, ok := s.m.credentials[credKey(orgID, userID, platform)]
	if !ok {
		return UserCredential{}, ErrNotFound
	}
	return copyCredential(c), nil
}

func (s memCredentials) ListByUser(_ context.Context, orgID, userID string) ([]UserCredential, error) {
	defer s.m.lock()()
	var out []UserCredential
	for _, c := range s.m.credentials {
		if c.OrgID == orgID && c.UserID == userID {
			out = append(out, copyCredential(c))
		}
	}
	slices.SortFunc(out, func(a, b UserCredential) int { return strings.Compare(a.Platform, b.Platform) })
	return out, nil
}

// Lock holds nothing more than the transaction it is called in: a memory
// transaction already excludes every other.
//
// @decision(DL-015)
func (s memCredentials) Lock(context.Context, string, string, string) error { return nil }

func (s memCredentials) Delete(_ context.Context, orgID, userID, platform string) error {
	defer s.m.lock()()
	key := credKey(orgID, userID, platform)
	if _, ok := s.m.credentials[key]; !ok {
		return ErrNotFound
	}
	delete(s.m.credentials, key)
	return nil
}

func copyCredential(c UserCredential) UserCredential {
	c.Ciphertext = slices.Clone(c.Ciphertext)
	c.Nonce = slices.Clone(c.Nonce)
	// Postgres reads an empty array back as nil; so does this.
	c.Scopes = slices.Clone(c.Scopes)
	if len(c.Scopes) == 0 {
		c.Scopes = nil
	}
	// @decision(DL-073)
	c.Route = slices.Clone(c.Route)
	return c
}

type memAllowlist struct{ m *Memory }

// allowKey is an address entry's identity. The e-mail is lowercased
// here so that a caller passing whatever case a person typed finds the
// same row, matching the Postgres store's normalisation rather than
// relying on every caller to normalise first.
//
// @decision(DL-007)
func allowKey(orgID, email string) string {
	return orgID + "\x00a\x00" + normalizeAllowEmail(email)
}

// accountKey is an account entry's identity, its numeric GitHub ID.
//
// @decision(DL-128)
func accountKey(orgID string, githubID int64) string {
	return orgID + "\x00g\x00" + strconv.FormatInt(githubID, 10)
}

func normalizeAllowEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (s memAllowlist) List(_ context.Context, orgID string) ([]AllowlistEntry, error) {
	defer s.m.lock()()
	var out []AllowlistEntry
	for _, e := range s.m.allowlist {
		if e.OrgID == orgID {
			out = append(out, e)
		}
	}
	// Account entries have no address and sort first, by ID, as the
	// Postgres store orders them.
	slices.SortFunc(out, func(a, b AllowlistEntry) int {
		if c := strings.Compare(a.Email, b.Email); c != 0 {
			return c
		}
		return cmp.Compare(a.GithubID, b.GithubID)
	})
	return out, nil
}

func (s memAllowlist) Get(_ context.Context, orgID, email string) (AllowlistEntry, error) {
	defer s.m.lock()()
	if normalizeAllowEmail(email) == "" {
		return AllowlistEntry{}, ErrNotFound
	}
	e, ok := s.m.allowlist[allowKey(orgID, email)]
	if !ok {
		return AllowlistEntry{}, ErrNotFound
	}
	return e, nil
}

// @decision(DL-128)
func (s memAllowlist) GetAccount(_ context.Context, orgID string, githubID int64) (AllowlistEntry, error) {
	defer s.m.lock()()
	e, ok := s.m.allowlist[accountKey(orgID, githubID)]
	if !ok || githubID == 0 {
		return AllowlistEntry{}, ErrNotFound
	}
	return e, nil
}

// @decision(DL-128)
func (s memAllowlist) ByLogin(_ context.Context, orgID, login string) ([]AllowlistEntry, error) {
	defer s.m.lock()()
	var out []AllowlistEntry
	for _, e := range s.m.allowlist {
		if e.OrgID == orgID && e.GithubID != 0 && login != "" && strings.EqualFold(e.GithubLogin, login) {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b AllowlistEntry) int { return cmp.Compare(a.GithubID, b.GithubID) })
	return out, nil
}

func (s memAllowlist) Put(_ context.Context, e AllowlistEntry) error {
	defer s.m.lock()()
	e.Email = normalizeAllowEmail(e.Email)
	var key string
	switch {
	case e.Email != "" && e.GithubID == 0:
		key = allowKey(e.OrgID, e.Email)
		e.GithubLogin, e.GithubName = "", ""
	case e.Email == "" && e.GithubID != 0:
		key = accountKey(e.OrgID, e.GithubID)
	default:
		return ErrMixedEntry
	}
	if cur, ok := s.m.allowlist[key]; ok {
		e.CreatedAt = cur.CreatedAt
	} else if e.CreatedAt.IsZero() {
		e.CreatedAt = s.m.now()
	}
	s.m.allowlist[key] = e
	return nil
}

// @decision(DL-128)
func (s memAllowlist) Relabel(_ context.Context, orgID string, githubID int64, login, name string) error {
	defer s.m.lock()()
	key := accountKey(orgID, githubID)
	e, ok := s.m.allowlist[key]
	if !ok {
		return nil
	}
	e.GithubLogin, e.GithubName = login, name
	s.m.allowlist[key] = e
	return nil
}

func (s memAllowlist) Delete(_ context.Context, orgID, email string) error {
	defer s.m.lock()()
	key := allowKey(orgID, email)
	if _, ok := s.m.allowlist[key]; !ok || normalizeAllowEmail(email) == "" {
		return ErrNotFound
	}
	delete(s.m.allowlist, key)
	return nil
}

// @decision(DL-128)
func (s memAllowlist) DeleteAccount(_ context.Context, orgID string, githubID int64) error {
	defer s.m.lock()()
	key := accountKey(orgID, githubID)
	if _, ok := s.m.allowlist[key]; !ok || githubID == 0 {
		return ErrNotFound
	}
	delete(s.m.allowlist, key)
	return nil
}

// CountAdmins takes no lock: the memory store serialises every call on
// one mutex, and inside Tx the caller already holds it, so the mutual
// exclusion the Postgres advisory lock buys is structural here.
func (s memAllowlist) CountAdmins(_ context.Context, orgID string) (int, error) {
	defer s.m.lock()()
	n := 0
	for _, e := range s.m.allowlist {
		if e.OrgID == orgID && e.Admin {
			n++
		}
	}
	return n, nil
}

type memConversations struct{ m *Memory }

func (s memConversations) byKey(sessionID, opencodeID string) *Conversation {
	for _, c := range s.m.conversations {
		if c.SessionID == sessionID && c.OpenCodeID == opencodeID {
			return &c
		}
	}
	return nil
}

// UpsertConversation implements ConversationStore.
//
// @decision(DL-017)
func (s memConversations) UpsertConversation(_ context.Context, c Conversation) (Conversation, error) {
	defer s.m.lock()()
	if sess, ok := s.m.sessions[c.SessionID]; !ok || sess.OrgID != c.OrgID {
		return Conversation{}, ErrNotFound
	}
	now := s.m.now()
	if cur := s.byKey(c.SessionID, c.OpenCodeID); cur != nil {
		if c.Title != "" {
			cur.Title = c.Title
		}
		if len(c.Info) > 0 {
			cur.Info = c.Info
		}
		// @decision(DL-110)
		if c.ParentOpenCodeID != "" {
			cur.ParentOpenCodeID = c.ParentOpenCodeID
		}
		cur.UpdatedAt = now
		old := s.m.conversations[cur.ID]
		s.m.conversations[cur.ID] = *cur
		s.m.journal(func() { s.m.conversations[old.ID] = old })
		s.m.emit(change{session: cur.SessionID, conv: cur})
		return *cur, nil
	}
	if _, ok := s.m.conversations[c.ID]; ok {
		return Conversation{}, ErrConflict
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	s.m.conversations[c.ID] = c
	s.m.journal(func() { delete(s.m.conversations, c.ID) })
	stored := c
	s.m.emit(change{session: c.SessionID, conv: &stored})
	return c, nil
}

// ListConversations implements ConversationStore.
//
// @decision(DL-025)
func (s memConversations) ListConversations(_ context.Context, orgID, sessionID string) ([]Conversation, error) {
	defer s.m.lock()()
	var out []Conversation
	for _, c := range s.m.conversations {
		if c.OrgID == orgID && c.SessionID == sessionID {
			c.Info = nil
			out = append(out, c)
		}
	}
	// Oldest first, matching the SQL: a ULID is only monotonic across
	// milliseconds, so id alone is not creation order for two
	// conversations minted in the same one. Id breaks the tie.
	slices.SortFunc(out, func(a, b Conversation) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// ConversationInfos implements ConversationStore.
//
// @decision(DL-131)
func (s memConversations) ConversationInfos(_ context.Context, orgID, sessionID string) (map[string][]byte, error) {
	defer s.m.lock()()
	out := map[string][]byte{}
	for _, c := range s.m.conversations {
		if c.OrgID == orgID && c.SessionID == sessionID && len(c.Info) > 0 {
			out[c.ID] = slices.Clone(c.Info)
		}
	}
	return out, nil
}

// StoredSize implements ConversationStore.
//
// @decision(DL-131)
func (s memConversations) StoredSize(_ context.Context, orgID, sessionID string) (StoredSize, error) {
	defer s.m.lock()()
	var size StoredSize
	for _, c := range s.m.conversations {
		if c.OrgID == orgID && c.SessionID == sessionID {
			size.Conversations++
		}
	}
	for _, id := range s.sessionEvents(sessionID) {
		if e := s.m.events[id]; e.OrgID == orgID {
			size.Rows++
			size.Bytes += int64(len(e.Text) + len(e.Raw))
		}
	}
	for _, m := range s.m.sessionMessages(orgID, sessionID) {
		size.Rows++
		size.Bytes += int64(len(m.Text) + len(m.Raw))
	}
	return size, nil
}

// belongsTo reports whether the conversation exists and carries the
// org and session the row claims, which is what the composite foreign
// keys enforce in Postgres.
func (s memConversations) belongsTo(conversationID, orgID, sessionID string) bool {
	conv, ok := s.m.conversations[conversationID]
	return ok && conv.OrgID == orgID && conv.SessionID == sessionID
}

// PutMessages implements ConversationStore. Every row is checked before
// any is written, so a bad batch leaves the store as it was.
//
// @decision(DL-017) @decision(DL-131)
func (s memConversations) PutMessages(_ context.Context, msgs []Message) (MessageWrite, error) {
	defer s.m.lock()()
	for _, msg := range msgs {
		if !s.belongsTo(msg.ConversationID, msg.OrgID, msg.SessionID) {
			return MessageWrite{}, ErrNotFound
		}
	}
	var w MessageWrite
	var stored []Message
	// @decision(DL-091)
	defer func() {
		if len(stored) > 0 {
			s.m.emit(change{session: stored[0].SessionID, msgs: stored})
		}
	}()
	for _, msg := range msgs {
		if cur := s.messageByKey(msg.ConversationID, msg.OpenCodeID); cur != nil {
			if cur.Role == msg.Role && cur.Text == msg.Text && cur.At.Equal(msg.At) &&
				bytes.Equal(cur.Raw, msg.Raw) {
				continue
			}
			old := *cur
			cur.Role, cur.Text, cur.At = msg.Role, msg.Text, msg.At
			cur.Raw = copyBytes(msg.Raw)
			s.m.messages[cur.ID] = *cur
			s.m.journal(func() { s.m.messages[old.ID] = old })
			stored = append(stored, *cur)
			w.Changed++
			w.Bytes += int64(len(cur.Text)+len(cur.Raw)) - int64(len(old.Text)+len(old.Raw))
			continue
		}
		if _, ok := s.m.messages[msg.ID]; ok {
			// Checked here rather than up front because a message
			// whose key already exists is an update, not an insert,
			// and only an insert needs a fresh id.
			return w, ErrConflict
		}
		if msg.CreatedAt.IsZero() {
			msg.CreatedAt = s.m.now()
		}
		// Never nil, so a message stored without its frame reads back
		// the same empty value Postgres gives.
		msg.Raw = copyBytes(msg.Raw)
		s.m.messages[msg.ID] = msg
		key := convMessage{msg.ConversationID, msg.OpenCodeID}
		s.m.messageKeys[key] = msg.ID
		s.m.indexMessage(msg)
		s.m.journal(func() {
			delete(s.m.messages, msg.ID)
			delete(s.m.messageKeys, key)
			delete(s.m.messagesBySession[msg.SessionID], msg.ID)
		})
		stored = append(stored, msg)
		w.Changed++
		w.Added++
		w.Bytes += int64(len(msg.Text) + len(msg.Raw))
	}
	return w, nil
}

func (s memConversations) messageByKey(conversationID, opencodeID string) *Message {
	id, ok := s.m.messageKeys[convMessage{conversationID, opencodeID}]
	if !ok {
		return nil
	}
	m := s.m.messages[id]
	return &m
}

func compareMessages(a, b Message) int {
	if c := a.At.Compare(b.At); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

func (s memConversations) ListMessages(_ context.Context, orgID, sessionID string) ([]Message, error) {
	defer s.m.lock()()
	var out []Message
	for _, m := range s.m.sessionMessages(orgID, sessionID) {
		out = append(out, copyMessage(m))
	}
	slices.SortFunc(out, compareMessages)
	return out, nil
}

// RecentMessages implements ConversationStore, over the session's index.
//
// @decision(DL-131)
func (s memConversations) RecentMessages(_ context.Context, orgID, sessionID, conversationID string, limit int, maxBytes int64) ([]Message, bool, error) {
	defer s.m.lock()()
	if limit <= 0 {
		return nil, false, nil
	}
	var all []Message
	for _, m := range s.m.sessionMessages(orgID, sessionID) {
		if conversationID == "" || m.ConversationID == conversationID {
			all = append(all, m)
		}
	}
	slices.SortFunc(all, func(a, b Message) int { return compareMessages(b, a) })
	var out []Message
	var running int64
	for _, m := range all {
		running += int64(len(m.Text) + len(m.Raw))
		if len(out) == limit || (maxBytes > 0 && running > maxBytes) {
			break
		}
		out = append(out, copyMessage(m))
	}
	slices.Reverse(out)
	return out, len(all) > len(out), nil
}

// LastMessages implements ConversationStore, over root conversations
// only.
//
// @decision(DL-017) @decision(DL-115)
func (s memConversations) LastMessages(_ context.Context, orgID string, sessionIDs []string) (map[string]Message, error) {
	defer s.m.lock()()
	want := map[string]bool{}
	for _, id := range sessionIDs {
		want[id] = true
	}
	out := map[string]Message{}
	for id := range want {
		for _, m := range s.m.sessionMessages(orgID, id) {
			if !s.m.conversations[m.ConversationID].IsRoot() {
				continue
			}
			if cur, ok := out[m.SessionID]; !ok || compareMessages(m, cur) > 0 {
				out[m.SessionID] = copyMessage(m)
			}
		}
	}
	return out, nil
}

// PutEvents implements ConversationStore. Every row is checked before
// any is written, so a bad batch leaves the store — and eventPos — as
// they were, which is what the Postgres transaction gives.
//
// @decision(DL-017)
func (s memConversations) PutEvents(_ context.Context, evs []Event) ([]Event, error) {
	defer s.m.lock()()
	if len(evs) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		if err := ev.Validate(); err != nil {
			return nil, err
		}
		if ev.ConversationID != "" && !s.belongsTo(ev.ConversationID, ev.OrgID, ev.SessionID) {
			return nil, ErrNotFound
		}
		if _, ok := s.m.events[ev.ID]; ok || seen[ev.ID] {
			return nil, ErrConflict
		}
		seen[ev.ID] = true
	}
	var out []Event
	for _, ev := range evs {
		if ev.Source == SourceOpenCode && s.eventStored(ev.ConversationID, ev.Seq) {
			continue
		}
		s.m.eventPos++
		ev.Pos = s.m.eventPos
		if ev.CreatedAt.IsZero() {
			ev.CreatedAt = s.m.now()
		}
		// Never nil, so a marker without a frame reads back the same
		// empty value Postgres gives.
		ev.Raw = copyBytes(ev.Raw)
		s.m.events[ev.ID] = ev
		s.m.indexEvent(ev)
		out = append(out, copyEvent(ev))
	}
	// @decision(DL-091)
	if len(out) > 0 {
		s.m.emit(change{session: out[0].SessionID, evs: out})
	}
	return out, nil
}

func (s memConversations) eventStored(conversationID string, seq int64) bool {
	return s.m.eventSeqs[convSeq{conversationID, seq}]
}

// sessionEvents is a session's events in Pos order.
func (s memConversations) sessionEvents(sessionID string) []string {
	return s.m.eventsBySession[sessionID]
}

func (s memConversations) ListEvents(_ context.Context, orgID, sessionID string, after int64, limit int) ([]Event, error) {
	defer s.m.lock()()
	ids := s.sessionEvents(sessionID)
	// The first event past after; comparing without after+1, which
	// wraps at the largest cursor.
	from, _ := slices.BinarySearchFunc(ids, after, func(id string, after int64) int {
		if s.m.events[id].Pos <= after {
			return -1
		}
		return 1
	})
	var out []Event
	for _, id := range ids[from:] {
		if limit > 0 && len(out) == limit {
			break
		}
		if e := s.m.events[id]; e.OrgID == orgID {
			out = append(out, copyEvent(e))
		}
	}
	return out, nil
}

// lastEvent is a session's newest event that match accepts.
func (s memConversations) lastEvent(orgID, sessionID string, match func(Event) bool) (Event, error) {
	ids := s.sessionEvents(sessionID)
	for i := len(ids) - 1; i >= 0; i-- {
		if e := s.m.events[ids[i]]; e.OrgID == orgID && match(e) {
			return copyEvent(e), nil
		}
	}
	return Event{}, ErrNotFound
}

// LastEvent implements ConversationStore.
//
// @decision(DL-018)
func (s memConversations) LastEvent(_ context.Context, orgID, sessionID string) (Event, error) {
	defer s.m.lock()()
	return s.lastEvent(orgID, sessionID, func(Event) bool { return true })
}

// FindEvent implements ConversationStore.
//
// @decision(DL-052)
func (s memConversations) FindEvent(_ context.Context, orgID, sessionID, name string, contains []byte) (Event, error) {
	defer s.m.lock()()
	return s.lastEvent(orgID, sessionID, func(e Event) bool { return e.Name == name && bytes.Contains(e.Raw, contains) })
}

// SetSeqOffset implements ConversationStore.
//
// @decision(DL-089)
func (s memConversations) SetSeqOffset(_ context.Context, orgID, conversationID string, base, after int64) error {
	defer s.m.lock()()
	c, ok := s.m.conversations[conversationID]
	if !ok || c.OrgID != orgID {
		return ErrNotFound
	}
	old := c
	c.SeqBase, c.SeqAfter = base, after
	s.m.conversations[conversationID] = c
	s.m.journal(func() { s.m.conversations[old.ID] = old })
	s.m.emit(change{session: c.SessionID, conv: &c})
	return nil
}

// LastSeqs implements ConversationStore.
//
// @decision(DL-017)
func (s memConversations) LastSeqs(_ context.Context, orgID, sessionID string) (map[string]int64, error) {
	defer s.m.lock()()
	out := map[string]int64{}
	for _, id := range s.sessionEvents(sessionID) {
		e := s.m.events[id]
		if e.OrgID != orgID || e.Source != SourceOpenCode {
			continue
		}
		if e.Seq > out[e.ConversationID] {
			out[e.ConversationID] = e.Seq
		}
	}
	return out, nil
}

// LastEventExcept implements ConversationStore.
//
// @decision(DL-021)
func (s memConversations) LastEventExcept(_ context.Context, orgID, sessionID string, except []string) (Event, error) {
	defer s.m.lock()()
	return s.lastEvent(orgID, sessionID, func(e Event) bool { return !slices.Contains(except, e.Name) })
}

func copyEvent(e Event) Event {
	e.Raw = copyBytes(e.Raw)
	return e
}

// copyMessage detaches a message's frame, so a caller writing through
// Raw cannot reach into the stored row — the same rule copyEvent
// applies.
//
// @decision(DL-017)
func copyMessage(m Message) Message {
	m.Raw = copyBytes(m.Raw)
	return m
}

// memJobs is the memory JobStore.
//
// @decision(DL-045)
type memJobs struct{ m *Memory }

func (s memJobs) Create(_ context.Context, j Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	defer s.m.lock()()
	// What the composite foreign key enforces in Postgres: a job's
	// session must exist and must be the same org's, or the job would
	// be filed under an org that cannot see the session it names.
	if sess, ok := s.m.sessions[j.SessionID]; !ok || sess.OrgID != j.OrgID {
		return ErrNotFound
	}
	if _, ok := s.m.jobs[j.ID]; ok {
		return ErrConflict
	}
	// One job per session: a session running two loops has two things
	// continuing it, and neither would know about the other.
	for _, cur := range s.m.jobs {
		if cur.OrgID == j.OrgID && cur.SessionID == j.SessionID {
			return ErrConflict
		}
	}
	now := s.m.now()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now
	}
	j.UpdatedAt = now
	s.m.jobs[j.ID] = copyJob(j)
	return nil
}

func (s memJobs) Get(_ context.Context, orgID, id string) (Job, error) {
	defer s.m.lock()()
	j, ok := s.m.jobs[id]
	if !ok || j.OrgID != orgID {
		return Job{}, ErrNotFound
	}
	return copyJob(j), nil
}

// GetForUpdate is Get: a memory Tx holds the store's lock for its whole
// callback, so nothing can write the row in between.
func (s memJobs) GetForUpdate(ctx context.Context, orgID, id string) (Job, error) {
	return s.Get(ctx, orgID, id)
}

// @decision(DL-012)
func (s memJobs) List(_ context.Context, orgID string, f JobFilter) ([]Job, error) {
	defer s.m.lock()()
	var out []Job
	for _, j := range s.m.jobs {
		if j.OrgID == orgID && f.match(j) {
			out = append(out, copyJob(j))
		}
	}
	sortJobs(out)
	return firstN(out, f.Limit), nil
}

func (s memJobs) Count(ctx context.Context, orgID string, f JobFilter) (int, error) {
	f.Before, f.Limit = "", 0
	all, err := s.List(ctx, orgID, f)
	return len(all), err
}

func (s memJobs) Update(_ context.Context, j Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	defer s.m.lock()()
	cur, ok := s.m.jobs[j.ID]
	if !ok || cur.OrgID != j.OrgID {
		return ErrNotFound
	}
	if !CanTransition(cur.State, j.State) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, cur.State, j.State)
	}
	now := s.m.now()
	// The columns UpdateJob does not write. Carried over rather than
	// rejected so the two stores end in the same place from the same
	// call — and because re-pointing a job at another session here
	// would slip past the one-job-per-session rule that Create and the
	// unique constraint both defend.
	j.CreatedAt = cur.CreatedAt
	j.SessionID, j.Kind, j.OwnerID = cur.SessionID, cur.Kind, cur.OwnerID
	// Written once, at create: DL-014 leaves changing a running job's
	// model out of scope, and UpdateJob does not set the column.
	//
	// @decision(DL-014)
	j.Model = cur.Model
	// Also fixed at create, and also not written by UpdateJob.
	//
	// @decision(DL-049)
	j.MaxDuration = cur.MaxDuration
	// Fixed at create, and not written by UpdateJob.
	//
	// @decision(DL-137) @decision(DL-138)
	j.PRPolicy, j.SkipPrep = cur.PRPolicy, cur.SkipPrep
	// Only SetSyncApplied writes it.
	//
	// @decision(DL-102)
	j.SyncAppliedAt = cur.SyncAppliedAt
	j.UpdatedAt = now
	StampJobTimes(&j, cur, now)
	s.m.jobs[j.ID] = copyJob(j)
	// @decision(DL-100)
	if j.State.Terminal() {
		delete(s.m.jobSeeds, j.ID)
	}
	return nil
}

func (s memJobs) BySession(_ context.Context, orgID, sessionID string) (Job, error) {
	defer s.m.lock()()
	for _, j := range s.m.jobs {
		if j.OrgID == orgID && j.SessionID == sessionID {
			return copyJob(j), nil
		}
	}
	return Job{}, ErrNotFound
}

// @decision(DL-060)
func (s memJobs) BySessions(_ context.Context, orgID string, sessionIDs []string) (map[string]Job, error) {
	defer s.m.lock()()
	want := map[string]bool{}
	for _, id := range sessionIDs {
		want[id] = true
	}
	out := map[string]Job{}
	for _, j := range s.m.jobs {
		if j.OrgID != orgID || !want[j.SessionID] {
			continue
		}
		// A session has at most one job — the foreign key says so — but
		// map iteration is unordered, so ties are broken deterministically
		// rather than by whichever the range happened to reach last.
		// Otherwise a corrupt store would decorate the same list
		// differently on each request, which is worse to debug than
		// consistently wrong.
		if cur, ok := out[j.SessionID]; ok && cur.ID >= j.ID {
			continue
		}
		out[j.SessionID] = copyJob(j)
	}
	return out, nil
}

// @decision(DL-045)
func (s memJobs) CountLive(_ context.Context, orgID, ownerID string) (int, error) {
	defer s.m.lock()()
	n := 0
	for _, j := range s.m.jobs {
		// @decision(DL-101)
		if j.OrgID == orgID && j.OwnerID == ownerID && (j.State.Counts() || SyncHeld(j, s.m.sessions[j.SessionID])) {
			n++
		}
	}
	return n, nil
}

func (s memJobs) ListLive(context.Context) ([]Job, error) {
	defer s.m.lock()()
	var out []Job
	for _, j := range s.m.jobs {
		if !j.State.Terminal() {
			out = append(out, copyJob(j))
		}
	}
	sortJobs(out)
	return out, nil
}

// copyJob detaches a job's pointer fields, so a caller writing through
// a returned StartedAt cannot reach into the stored row. Every other
// memory store does the same on the way out.
func copyJob(j Job) Job {
	j.StartedAt, j.StoppedAt = copyTime(j.StartedAt), copyTime(j.StoppedAt)
	j.ClockStartedAt = copyTime(j.ClockStartedAt)
	j.SyncAppliedAt = copyTime(j.SyncAppliedAt)
	return j
}

// sortJobs orders a listing newest first, which is how the sessions
// list already reads and what a person scanning jobs wants. IDs are
// ULIDs, so ordering by id descending is ordering by creation time.
func sortJobs(js []Job) {
	slices.SortFunc(js, func(a, b Job) int { return strings.Compare(b.ID, a.ID) })
}

// PutDeliverable keeps the stored member when it is verified.
//
// @decision(DL-051)
func (s memJobs) PutDeliverable(_ context.Context, orgID string, d Deliverable) (bool, error) {
	defer s.m.lock()()
	if j, ok := s.m.jobs[d.JobID]; !ok || j.OrgID != orgID {
		return false, ErrNotFound
	}
	byKey := s.m.deliverables[d.JobID]
	if byKey == nil {
		byKey = map[string]Deliverable{}
		s.m.deliverables[d.JobID] = byKey
	}
	now := s.m.now()
	cur, ok := byKey[d.Key]
	if ok && cur.Status == DeliverableVerified {
		return false, nil
	}
	d.CreatedAt = now
	if ok {
		d.CreatedAt = cur.CreatedAt
		// @decision(DL-051)
		if d.Forge == "" {
			d.Forge = cur.Forge
		}
	}
	d.UpdatedAt = now
	byKey[d.Key] = d
	return true, nil
}

func (s memJobs) Deliverables(_ context.Context, orgID string, jobIDs []string) (map[string][]Deliverable, error) {
	defer s.m.lock()()
	out := map[string][]Deliverable{}
	for _, id := range jobIDs {
		if j, ok := s.m.jobs[id]; !ok || j.OrgID != orgID {
			continue
		}
		for _, d := range s.m.deliverables[id] {
			out[id] = append(out[id], d)
		}
		slices.SortFunc(out[id], func(a, b Deliverable) int { return strings.Compare(a.Key, b.Key) })
	}
	return out, nil
}

// PutJobFile inserts once: a stored file is never replaced.
//
// @decision(DL-051)
func (s memJobs) PutJobFile(_ context.Context, orgID string, f JobFile) (bool, error) {
	defer s.m.lock()()
	if j, ok := s.m.jobs[f.JobID]; !ok || j.OrgID != orgID {
		return false, ErrNotFound
	}
	byPath := s.m.jobFiles[f.JobID]
	if byPath == nil {
		byPath = map[string]JobFile{}
		s.m.jobFiles[f.JobID] = byPath
	}
	if _, ok := byPath[f.Path]; ok {
		return false, nil
	}
	f.Body = copyBytes(f.Body)
	f.CreatedAt = s.m.now()
	byPath[f.Path] = f
	return true, nil
}

func (s memJobs) JobFile(_ context.Context, orgID, jobID, path string) (JobFile, error) {
	defer s.m.lock()()
	if j, ok := s.m.jobs[jobID]; !ok || j.OrgID != orgID {
		return JobFile{}, ErrNotFound
	}
	f, ok := s.m.jobFiles[jobID][path]
	if !ok {
		return JobFile{}, ErrNotFound
	}
	f.Body = copyBytes(f.Body)
	return f, nil
}

func (s memJobs) JobFiles(_ context.Context, orgID, jobID string) ([]JobFile, error) {
	defer s.m.lock()()
	if j, ok := s.m.jobs[jobID]; !ok || j.OrgID != orgID {
		return nil, ErrNotFound
	}
	var out []JobFile
	for _, f := range s.m.jobFiles[jobID] {
		f.Body = nil
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b JobFile) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// @decision(DL-100)
func (s memJobs) PutSeed(_ context.Context, orgID, jobID string, bundle []byte) error {
	defer s.m.lock()()
	if j, ok := s.m.jobs[jobID]; !ok || j.OrgID != orgID {
		return ErrNotFound
	}
	s.m.jobSeeds[jobID] = copyBytes(bundle)
	return nil
}

func (s memJobs) Seed(_ context.Context, orgID, jobID string) ([]byte, error) {
	defer s.m.lock()()
	if j, ok := s.m.jobs[jobID]; !ok || j.OrgID != orgID {
		return nil, ErrNotFound
	}
	b, ok := s.m.jobSeeds[jobID]
	if !ok {
		return nil, ErrNotFound
	}
	return copyBytes(b), nil
}

func (s memJobs) DeleteSeed(_ context.Context, orgID, jobID string) error {
	defer s.m.lock()()
	if j, ok := s.m.jobs[jobID]; ok && j.OrgID == orgID {
		delete(s.m.jobSeeds, jobID)
	}
	return nil
}

// @decision(DL-102)
func (s memJobs) SetSyncApplied(_ context.Context, orgID, jobID string, at time.Time) error {
	defer s.m.lock()()
	j, ok := s.m.jobs[jobID]
	if !ok || j.OrgID != orgID {
		return ErrNotFound
	}
	j.SyncAppliedAt = &at
	j.UpdatedAt = s.m.now()
	s.m.jobs[jobID] = copyJob(j)
	return nil
}

type memOrgCredentials struct{ m *Memory }

func (s memOrgCredentials) Put(_ context.Context, c OrgCredential) error {
	defer s.m.lock()()
	key := c.OrgID + "\x00" + c.Platform
	now := s.m.now()
	if cur, ok := s.m.orgCredentials[key]; ok {
		c.ID = cur.ID
		c.CreatedAt = cur.CreatedAt
	} else if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	c.Ciphertext, c.Nonce, c.Meta = slices.Clone(c.Ciphertext), slices.Clone(c.Nonce), slices.Clone(c.Meta)
	s.m.orgCredentials[key] = c
	return nil
}

func (s memOrgCredentials) Get(_ context.Context, orgID, platform string) (OrgCredential, error) {
	defer s.m.lock()()
	c, ok := s.m.orgCredentials[orgID+"\x00"+platform]
	if !ok {
		return OrgCredential{}, ErrNotFound
	}
	c.Ciphertext, c.Nonce, c.Meta = slices.Clone(c.Ciphertext), slices.Clone(c.Nonce), slices.Clone(c.Meta)
	return c, nil
}

func (s memOrgCredentials) List(_ context.Context, orgID string) ([]OrgCredential, error) {
	defer s.m.lock()()
	var out []OrgCredential
	for _, c := range s.m.orgCredentials {
		if c.OrgID == orgID {
			c.Ciphertext, c.Nonce, c.Meta = slices.Clone(c.Ciphertext), slices.Clone(c.Nonce), slices.Clone(c.Meta)
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b OrgCredential) int { return strings.Compare(a.Platform, b.Platform) })
	return out, nil
}

func (s memOrgCredentials) Delete(_ context.Context, orgID, platform string) error {
	defer s.m.lock()()
	key := orgID + "\x00" + platform
	if _, ok := s.m.orgCredentials[key]; !ok {
		return ErrNotFound
	}
	delete(s.m.orgCredentials, key)
	return nil
}

type memAdminEvents struct{ m *Memory }

func (s memAdminEvents) Append(_ context.Context, e AdminEvent) error {
	defer s.m.lock()()
	if e.At.IsZero() {
		e.At = s.m.now()
	}
	s.m.adminEvents = append(s.m.adminEvents, e)
	return nil
}

func (s memAdminEvents) List(_ context.Context, orgID string, limit int) ([]AdminEvent, error) {
	defer s.m.lock()()
	var out []AdminEvent
	for i := len(s.m.adminEvents) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		if e := s.m.adminEvents[i]; e.OrgID == orgID {
			out = append(out, e)
		}
	}
	return out, nil
}
