package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// RecorderRedials is how many times a capture redials a session whose
// stream broke while the store still says it is running, over the
// session's whole life; the waits between them are RecorderBackoff.
//
// @decision(DL-017)
const RecorderRedials = 5

// RecorderBackoff are the waits before each redial.
//
// @decision(DL-017)
var RecorderBackoff = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second}

// RecorderBufferBytes bounds the frames a span holds in memory while its
// catch-up read runs. Past it the span ends as a break, and the redial's
// catch-up recovers the messages.
//
// @decision(DL-017)
const RecorderBufferBytes = 64 << 20

// MaxConversations bounds how many of a sandbox's OpenCode sessions one
// catch-up reads. The count comes from the sandbox, and each one costs a
// request and up to MaxSandboxReadBytes of memory.
//
// @decision(DL-018) @decision(DL-017)
const MaxConversations = 32

// MaxSessionConversations bounds how many conversations one session
// stores. Past it a frame naming a new one is dropped, with one note in
// the session's log.
//
// @decision(DL-131)
const MaxSessionConversations = 800

// MaxConversationInfoBytes bounds the Session.Info stored for one
// conversation. A larger one is not stored; the conversation still is.
//
// @decision(DL-131)
const MaxConversationInfoBytes = 128 << 10

// MaxSessionRows and DefaultSessionBytes bound what one session's
// events and messages occupy. At either the recorder stops recording
// the session and says so in its log; the sandbox runs on.
//
// @decision(DL-131)
const (
	MaxSessionRows      = 1_000_000
	DefaultSessionBytes = 1 << 30
)

// MarkerConversationLimit is written once when a session reaches
// MaxSessionConversations.
//
// @decision(DL-131)
const MarkerConversationLimit = store.SpovNamespace + "recorder.conversations.limit"

// errConversationLimit is a conversation the session has no room for.
// errStorageLimit ends the recording of a session at its storage limit.
//
// @decision(DL-131)
var (
	errConversationLimit = errors.New("the session is at its conversation limit")
	errStorageLimit      = errors.New("the session is at its storage limit")
)

// MessagePageSize is how many messages one request for a conversation
// asks for: the pinned build's own default page, so no page is larger
// than the single unparameterised read was. OpenCode refuses over 200.
//
// @decision(DL-069)
const MessagePageSize = 50

// MaxMessagePages bounds the pages one read of a conversation follows.
// The count comes from the sandbox.
//
// @decision(DL-069)
const MaxMessagePages = 200

// FinalReadTimeout bounds the read Delete makes before it terminates a
// sandbox. A delete should not wait long on a sandbox that is about to
// be gone.
//
// @decision(DL-017)
const FinalReadTimeout = 10 * time.Second

// Marker names the recorder writes into a session's events. They sit in
// store.SpovNamespace, which only a spov-sourced row may carry, so a
// page can draw one as spov's own voice.
//
// @decision(DL-017) @decision(DL-058)
const (
	MarkerGap     = store.SpovNamespace + "recorder.gap"
	MarkerStopped = store.SpovNamespace + "recorder.stopped"
	// MarkerEvidence carries a job's plugin evidence, collected once at
	// delivery. It is the supervisor's marker rather than the
	// recorder's, and it is here so the marker names stay in one list.
	//
	// @decision(DL-045)
	MarkerEvidence = store.SpovNamespace + "job.evidence"
)

// Recorder captures every running session's conversation into the
// store. One per process, run where the reconciler runs; its only state
// is the captures in flight, which the next leader rebuilds.
//
// @decision(DL-017)
type Recorder struct {
	store store.Store
	now   func() time.Time
	warn  func(string)
	// metrics counts gaps and reconnects; nil records nothing.
	//
	// @decision(DL-011)
	metrics *telemetry.Metrics
	// interval is the sync cadence; ReconcileInterval unless a test
	// shortens it.
	interval time.Duration
	// maxPages is MaxMessagePages unless a test lowers it.
	maxPages int

	// The three seams to a sandbox. connect obtains the credential
	// pair, events opens the volatile stream over it, and fetch reads
	// one path over it. Fields so a test can count each.
	connect func(ctx context.Context, p auth.Principal, sess store.Session) (Connection, error)
	events  func(ctx context.Context, conn Connection) (io.ReadCloser, error)
	fetch   func(ctx context.Context, conn Connection, path string) ([]byte, error)
	// drop releases the connection cached for a session, naming the
	// refused token. It is its own seam because the refusal arrives on
	// the stream connect returned.
	//
	// @decision(DL-030)
	drop    func(sessionID, token string)
	backoff []time.Duration
	sleep   func(ctx context.Context, d time.Duration) error
	// The bounds on what one session stores; see the constants.
	//
	// @decision(DL-131)
	maxConversations int
	maxRows          int64
	maxBytes         int64

	mu sync.Mutex
	// runCtx is the context Run was started with; Wake parents captures
	// to it rather than to its caller's context, which is a job pass
	// that ends in seconds. Nil until Run starts, and Wake then does
	// nothing.
	//
	// @decision(DL-017)
	runCtx   context.Context
	captures map[string]context.CancelFunc
	// abandoned holds sessions whose redial budget ran out, with the
	// sandbox given up on. Never retried while this process leads; a new
	// leader starts afresh, and a resumed session's next sandbox too.
	abandoned map[string]string
	wg        sync.WaitGroup
}

// NewRecorder builds a recorder that reaches sandboxes through svc, as
// the recorder principal.
//
// @decision(DL-017)
func NewRecorder(svc *Service, st store.Store, now func() time.Time, warn func(string)) *Recorder {
	if now == nil {
		now = time.Now
	}
	if warn == nil {
		warn = func(string) {}
	}
	var metrics *telemetry.Metrics
	if svc != nil {
		metrics = svc.deps.Metrics
	}
	return &Recorder{
		store: st, now: now, warn: warn, interval: ReconcileInterval, metrics: metrics, maxPages: MaxMessagePages,
		maxConversations: MaxSessionConversations, maxRows: MaxSessionRows, maxBytes: DefaultSessionBytes,
		// A span is a server-side act, so it reuses a cached connection
		// and a redial usually costs no mint.
		//
		// @decision(DL-030)
		connect: func(ctx context.Context, p auth.Principal, sess store.Session) (Connection, error) {
			return svc.connectReusing(ctx, p, sess)
		},
		drop:   func(id, token string) { svc.forgetRefusedConnection(id, token) },
		events: openSandboxEvents,
		fetch: func(ctx context.Context, conn Connection, path string) ([]byte, error) {
			return sandboxGet(ctx, conn.URL+path, conn.Token, conn.User, conn.Password)
		},
		backoff:   RecorderBackoff,
		sleep:     sleepFor,
		captures:  map[string]context.CancelFunc{},
		abandoned: map[string]string{},
	}
}

func sleepFor(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// SetSessionBytes sets the storage limit of one session, in bytes. Zero
// or less keeps the default.
//
// @decision(DL-131)
func (r *Recorder) SetSessionBytes(n int64) {
	if n > 0 {
		r.maxBytes = n
	}
}

// Run syncs now and then on the reconciler's clock until ctx ends, at
// which point every capture is cancelled and waited for.
//
// @decision(DL-017)
func (r *Recorder) Run(ctx context.Context) {
	defer r.stopAll()
	// A new term starts the redial budget afresh, whichever process
	// leads it.
	r.mu.Lock()
	r.abandoned = map[string]string{}
	r.runCtx = ctx
	r.mu.Unlock()
	for {
		if err := r.Sync(ctx); err != nil && ctx.Err() == nil {
			r.warn("recorder: " + err.Error())
		}
		wait := r.interval + rand.N(r.interval/5+1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Sync starts a capture for every running session with a sandbox that
// none is recording, and cancels captures for sessions that are no
// longer running.
//
// @decision(DL-017)
func (r *Recorder) Sync(ctx context.Context) error {
	live, err := r.store.Sessions().ListLive(ctx)
	if err != nil {
		return err
	}
	running := map[string]bool{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sess := range live {
		if sess.State != store.StateRunning || sess.SandboxID == "" {
			continue
		}
		running[sess.ID] = true
		if _, on := r.captures[sess.ID]; on || r.gaveUpOn(sess) {
			continue
		}
		r.startCaptureLocked(ctx, sess)
	}
	for id, cancel := range r.captures {
		if !running[id] {
			cancel()
		}
	}
	return nil
}

// startCaptureLocked begins recording one session. The caller holds mu
// and has already decided this session wants a capture.
//
// @decision(DL-017)
func (r *Recorder) startCaptureLocked(ctx context.Context, sess store.Session) {
	cctx, cancel := context.WithCancel(ctx)
	r.captures[sess.ID] = cancel
	r.wg.Go(func() {
		defer r.forgetCapture(sess.ID, cancel)
		r.record(cctx, sess)
	})
}

// Wake starts recording one session now rather than at the next poll.
// It is an optimisation: the poll still finds any session a nudge
// misses. It is safe to call for a session already being recorded, and
// it does not block, since the capture dials on its own goroutine.
//
// @decision(DL-017)
func (r *Recorder) Wake(sess store.Session) {
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runCtx == nil {
		// Run has not started, so there is no lifetime to hang a
		// capture from. The poll is the only path in that case, which
		// is what local mode and most tests use.
		return
	}
	if _, on := r.captures[sess.ID]; on || r.gaveUpOn(sess) {
		return
	}
	r.startCaptureLocked(r.runCtx, sess)
}

func (r *Recorder) forgetCapture(id string, cancel context.CancelFunc) {
	cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.captures[id] != nil {
		delete(r.captures, id)
	}
}

func (r *Recorder) stopAll() {
	r.mu.Lock()
	for _, cancel := range r.captures {
		cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// Recording reports whether a capture is in flight for the session.
func (r *Recorder) Recording(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, on := r.captures[sessionID]
	return on
}

// record is one session's capture: a span per dial, redialled within
// the budget while the store still says the session is running.
//
// @decision(DL-017)
func (r *Recorder) record(ctx context.Context, sess store.Session) {
	redials := 0
	for {
		// A redial is judged like any attach, from where the store's
		// recording stopped (DL-017).
		err := r.span(ctx, sess)
		if ctx.Err() != nil {
			return
		}
		// Nothing more of this session is recorded by this process.
		//
		// @decision(DL-131)
		if errors.Is(err, errStorageLimit) {
			r.stopAtStorageLimit(ctx, sess)
			return
		}
		cur, gerr := r.store.Sessions().Get(ctx, sess.OrgID, sess.ID)
		if gerr == nil && cur.State != store.StateRunning {
			// The sandbox went, and the reconciler has said so or is
			// about to. Nothing to mark: the stream ended because the
			// session did.
			return
		}
		if gerr != nil {
			// A store that cannot say counts as a break rather than an
			// exit: returning here would let the next sync start a
			// fresh capture with a fresh budget, every 30 s, for as
			// long as the store flaps.
			err = fmt.Errorf("%w; and the session could not be re-read: %w", err, gerr)
		}
		if redials >= len(r.backoff) {
			r.warn(fmt.Sprintf("recorder %s: giving up after %d redials: %s", sess.ID, redials, redact.Error(err)))
			if merr := r.mark(ctx, sess, MarkerStopped, fmt.Sprintf("recording stopped after %d failed reconnects: %s", redials, redact.Error(err))); merr != nil {
				r.warn(fmt.Sprintf("recorder %s: %s", sess.ID, merr))
			}
			r.mu.Lock()
			r.abandoned[sess.ID] = sess.SandboxID
			r.mu.Unlock()
			return
		}
		wait := r.backoff[redials]
		redials++
		// @decision(DL-011)
		r.metrics.RecorderReconnect(ctx)
		r.warn(fmt.Sprintf("recorder %s: %s; redialling in %s (%d of %d)", sess.ID, redact.Error(err), wait, redials, len(r.backoff)))
		if r.sleep(ctx, wait) != nil {
			return
		}
	}
}

// stopAtStorageLimit says once in the session's log that its recording
// ended at the storage limit, and gives the session up. A later process
// finds the marker and stays quiet.
//
// @decision(DL-131)
func (r *Recorder) stopAtStorageLimit(ctx context.Context, sess store.Session) {
	r.mu.Lock()
	r.abandoned[sess.ID] = sess.SandboxID
	r.mu.Unlock()
	last, err := r.store.Conversations().LastEvent(ctx, sess.OrgID, sess.ID)
	if err == nil && last.Source == store.SourceSpov && last.Name == MarkerStopped {
		return
	}
	text := fmt.Sprintf("recording stopped: this session has stored its limit of %d MiB or %d events and messages; the sandbox is still running", r.maxBytes>>20, r.maxRows)
	r.warn(fmt.Sprintf("recorder %s: %s", sess.ID, text))
	if merr := r.mark(ctx, sess, MarkerStopped, text); merr != nil {
		r.warn(fmt.Sprintf("recorder %s: %s", sess.ID, merr))
	}
}

// capture is one span's working state: the connection it holds and the
// conversations it has seen, by OpenCode id.
type capture struct {
	sess  store.Session
	conn  Connection
	convs map[string]store.Conversation
	// known is every conversation the session has stored, by OpenCode
	// id, read at attach; infoSum is the Info last written for each, so
	// none is held. size is what the session has stored, as measured at
	// attach and counted since.
	//
	// @decision(DL-131)
	known         map[string]bool
	infoSum       map[string][sha256.Size]byte
	size          store.StoredSize
	measured      bool
	warnedInfo    bool
	warnedConvCap bool
	// warnedNoSeq is set once a durable frame without a sequence has
	// been reported, so a flattened marker is said once per span
	// rather than once per frame — and never silently.
	warnedNoSeq bool
	// warnedNamespace is the same, for a frame claiming a name in
	// store.SpovNamespace.
	//
	// @decision(DL-058)
	warnedNamespace bool
	// t0 is when this span opened its event stream. The gap marker is
	// stamped with it; see markAt.
	//
	// @decision(DL-017)
	t0 time.Time
	// The gap is decided from the first durable frame of each
	// conversation. lastSeq is where the store's recording of each
	// conversation stopped, read at attach; gapChecked names the
	// conversations already checked, and marked stops at one marker per
	// span.
	//
	// @decision(DL-017)
	lastSeq    map[string]int64
	gapChecked map[string]bool
	marked     bool
}

// newCapture is the only way one is built. The zero value is a trap:
// markAt on a zero t0 fails store.Event.Validate ("at is required"),
// which surfaces as a store write failure and ends the span.
//
// @decision(DL-017)
func newCapture(sess store.Session, conn Connection) *capture {
	return &capture{
		sess:       sess,
		conn:       conn,
		convs:      map[string]store.Conversation{},
		known:      map[string]bool{},
		infoSum:    map[string][sha256.Size]byte{},
		gapChecked: map[string]bool{},
		t0:         time.Now(),
	}
}

// frameQueue holds frames read from the stream until the span is ready
// for them, bounded by bytes rather than count.
type frameQueue struct {
	mu     sync.Mutex
	items  []string
	bytes  int
	closed bool
	err    error
	wake   chan struct{}
}

func newFrameQueue() *frameQueue { return &frameQueue{wake: make(chan struct{}, 1)} }

// push queues one frame, or reports that the bound was exceeded.
func (q *frameQueue) push(raw string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return true
	}
	if q.bytes+len(raw) > RecorderBufferBytes {
		return false
	}
	q.items = append(q.items, raw)
	q.bytes += len(raw)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// close ends the queue; err says why the stream stopped.
func (q *frameQueue) close(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed, q.err = true, err
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// next returns the next frame, blocking until one arrives, the queue is
// closed (ok false, and the stream's error), or ctx ends.
func (q *frameQueue) next(ctx context.Context) (raw string, ok bool, err error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			raw = q.items[0]
			q.items = q.items[1:]
			q.bytes -= len(raw)
			q.mu.Unlock()
			return raw, true, nil
		}
		if q.closed {
			err = q.err
			q.mu.Unlock()
			return "", false, err
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-q.wake:
		}
	}
}

// span is one attach-and-stream: dial, catch up, then write frames
// until the stream ends. The error says why it ended; io.EOF means the
// sandbox closed it. On a refused credential it drops that token's
// cached connection, or every redial would be served the same one.
//
// @decision(DL-017) @decision(DL-030)
func (r *Recorder) span(ctx context.Context, sess store.Session) error {
	conn, err := r.connect(ctx, auth.Recorder(sess.OrgID), sess)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	err = r.spanOver(ctx, sess, conn)
	if errors.Is(err, ErrSandboxUnauthorized) && r.drop != nil {
		r.drop(sess.ID, conn.Token)
	}
	return err
}

// spanOver is span's body, over a connection already obtained.
//
// @decision(DL-017)
func (r *Recorder) spanOver(ctx context.Context, sess store.Session, conn Connection) error {
	c := newCapture(sess, conn)

	// The span's own context: whatever ends the span ends its reader
	// too, so no goroutine outlives it.
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	// Before the dial: the stream replays nothing, so t0 is the earliest
	// moment this span can have carried anything.
	//
	// @decision(DL-017)
	c.t0 = r.now()
	body, err := r.events(sctx, conn)
	if err != nil {
		return fmt.Errorf("open the event stream: %w", err)
	}

	// The stream is read into a queue from the start, so nothing that
	// happens during the catch-up read is lost between the two.
	queue := newFrameQueue()
	var reader sync.WaitGroup
	reader.Go(func() {
		defer func() { _ = body.Close() }()
		err := readSSEFrames(sctx, body, func(raw string) bool {
			if !queue.push(raw) {
				queue.close(fmt.Errorf("the sandbox outran the recorder by more than %d bytes", RecorderBufferBytes))
				return false
			}
			return true
		})
		if err == nil {
			err = io.EOF
		}
		queue.close(err)
	})
	defer func() {
		stop()
		_ = body.Close()
		reader.Wait()
	}()

	if err := r.attach(sctx, c); err != nil {
		return err
	}
	for {
		raw, ok, err := queue.next(sctx)
		if !ok {
			return fmt.Errorf("the event stream ended: %w", err)
		}
		if err := r.frame(sctx, c, raw); err != nil {
			return err
		}
	}
}

// attach arms the gap check against where the store's recording of
// each conversation stopped, then catches the store up with the
// sandbox's message store. A redial, a restart and a first attach are
// one rule: a session never recorded has nothing stored, and its logs
// are judged from sequence 1.
//
// @decision(DL-017)
func (r *Recorder) attach(ctx context.Context, c *capture) error {
	last, err := r.store.Conversations().LastSeqs(ctx, c.sess.OrgID, c.sess.ID)
	if err != nil {
		return fmt.Errorf("read the store: %w", err)
	}
	c.lastSeq = last
	if err := r.measure(ctx, c); err != nil {
		return err
	}
	return r.catchUp(ctx, c)
}

// measure reads what the session has stored: which conversations, and
// how much. It answers errStorageLimit for a session already at its
// limit.
//
// @decision(DL-131)
func (r *Recorder) measure(ctx context.Context, c *capture) error {
	convs, err := r.store.Conversations().ListConversations(ctx, c.sess.OrgID, c.sess.ID)
	if err != nil {
		return fmt.Errorf("read the store: %w", err)
	}
	for _, conv := range convs {
		c.known[conv.OpenCodeID] = true
	}
	size, err := r.store.Conversations().StoredSize(ctx, c.sess.OrgID, c.sess.ID)
	if err != nil {
		return fmt.Errorf("read the store: %w", err)
	}
	c.size, c.measured = size, true
	return r.room(ctx, c)
}

// room reports errStorageLimit once the session has stored its limit.
// The count kept since attach overstates a rewritten message, so the
// store is measured again before the limit is believed.
//
// @decision(DL-131)
func (r *Recorder) room(ctx context.Context, c *capture) error {
	if !c.measured || (c.size.Rows < r.maxRows && c.size.Bytes < r.maxBytes) {
		return nil
	}
	size, err := r.store.Conversations().StoredSize(ctx, c.sess.OrgID, c.sess.ID)
	if err != nil {
		return fmt.Errorf("read the store: %w", err)
	}
	c.size = size
	if size.Rows >= r.maxRows || size.Bytes >= r.maxBytes {
		return errStorageLimit
	}
	return nil
}

// missedCount agrees with its number. Built by concatenation, which is
// where agreement fails silently.
//
// @decision(DL-017)
func missedCount(n int64) string {
	if n == 1 {
		return "1 earlier event"
	}
	return fmt.Sprintf("%d earlier events", n)
}

// gapText is what the gap marker says, from gapCheck.
//
// @decision(DL-017)
const gapText = "recording resumed; what happened before this was not captured"

// gapCheck decides, from the first durable frame of each conversation,
// whether this span began past where the recording of it stopped: at
// the store's last sequence for it, or before sequence 1 when nothing
// is stored. It runs at most once per conversation, marking it checked
// before judging the sequence, and writes at most one marker per span,
// stamped with the attach time.
//
// @decision(DL-017)
func (r *Recorder) gapCheck(ctx context.Context, c *capture, conversation string, seq int64) error {
	if c.marked || c.gapChecked[conversation] {
		return nil
	}
	c.gapChecked[conversation] = true
	last := c.lastSeq[conversation]
	if seq <= last+1 {
		return nil
	}
	if err := r.markAt(ctx, c.sess, MarkerGap, gapText+" ("+missedCount(seq-last-1)+")", c.t0); err != nil {
		return err
	}
	c.marked = true
	// @decision(DL-011)
	r.metrics.RecorderGap(ctx)
	return nil
}

// gaveUpOn reports whether the recorder abandoned this run of the
// session; a resumed session's next sandbox gets a budget of its own.
// The caller holds r.mu.
//
// @decision(DL-085) @decision(DL-017)
func (r *Recorder) gaveUpOn(sess store.Session) bool {
	sandbox, ok := r.abandoned[sess.ID]
	return ok && sandbox == sess.SandboxID
}

// catchUp reads every conversation in the sandbox once and upserts what
// it finds. An unreadable conversation is skipped with a warning; a
// store write failure ends the span.
//
// @decision(DL-017)
func (r *Recorder) catchUp(ctx context.Context, c *capture) error {
	listed, err := r.fetch(ctx, c.conn, "/api/session")
	if err != nil {
		return fmt.Errorf("list conversations: %w", err)
	}
	ids, titles, infos, parents := conversations(listed)
	// The count comes from the sandbox; see MaxConversations.
	if len(ids) > MaxConversations {
		r.warn(fmt.Sprintf("recorder %s: the sandbox lists %d conversations; reading the first %d", c.sess.ID, len(ids), MaxConversations))
		ids = ids[:MaxConversations]
	}
	for _, id := range ids {
		conv, err := r.conversationWith(ctx, c, id, titles[id], infos[id], parents[id])
		if errors.Is(err, errConversationLimit) {
			continue
		}
		if err != nil {
			return err
		}
		if err := r.readMessages(ctx, c, conv); err != nil {
			return err
		}
	}
	return nil
}

// conversation resolves an OpenCode session to its stored row, creating
// it on first sight. A session first seen by a frame other than its
// session.created is read once, before its row is written, so no frame
// of a subagent is stored before its parent is known. A read that fails
// stores the row as a root, which the next catch-up corrects.
//
// @decision(DL-110)
func (r *Recorder) conversation(ctx context.Context, c *capture, opencodeID string) (store.Conversation, error) {
	if conv, ok := c.convs[opencodeID]; ok {
		return conv, nil
	}
	// Before the read: a conversation with no room costs the sandbox a
	// frame and the server nothing.
	//
	// @decision(DL-131)
	if err := r.conversationRoom(ctx, c, opencodeID); err != nil {
		return store.Conversation{}, err
	}
	var (
		title, parent string
		info          []byte
	)
	body, err := r.fetch(ctx, c.conn, "/api/session/"+url.PathEscape(opencodeID))
	if err == nil {
		var ok bool
		if title, parent, info, ok = sessionInfo(body); !ok {
			title, parent, info = "", "", nil
		}
	} else if ctx.Err() == nil {
		r.warn(fmt.Sprintf("recorder %s: read conversation %s: %s", c.sess.ID, opencodeID, redact.Error(err)))
	}
	return r.conversationWith(ctx, c, opencodeID, title, info, parent)
}

// conversationWith is conversation with what is already known: the
// listing's title and Session.Info, and the parent, each written when
// there is one and it changed.
//
// @decision(DL-089) @decision(DL-110)
func (r *Recorder) conversationWith(ctx context.Context, c *capture, opencodeID, title string, info []byte, parent string) (store.Conversation, error) {
	// @decision(DL-131)
	if len(info) > MaxConversationInfoBytes {
		if !c.warnedInfo {
			c.warnedInfo = true
			r.warn(fmt.Sprintf("recorder %s: the info of conversation %s is %d bytes, over the limit of %d, and is not stored", c.sess.ID, opencodeID, len(info), MaxConversationInfoBytes))
		}
		info = nil
	}
	var sum [sha256.Size]byte
	if len(info) > 0 {
		sum = sha256.Sum256(info)
	}
	if conv, ok := c.convs[opencodeID]; ok && (title == "" || conv.Title == title) && (len(info) == 0 || c.infoSum[opencodeID] == sum) &&
		(parent == "" || conv.ParentOpenCodeID == parent) {
		return conv, nil
	}
	if err := r.conversationRoom(ctx, c, opencodeID); err != nil {
		return store.Conversation{}, err
	}
	conv, err := r.store.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(r.now()), OrgID: c.sess.OrgID, SessionID: c.sess.ID, OpenCodeID: opencodeID, Title: title, Info: info,
		ParentOpenCodeID: parent,
	})
	if err != nil {
		return store.Conversation{}, fmt.Errorf("write the store: %w", err)
	}
	// The row is kept without its Info, which only an import reads.
	//
	// @decision(DL-131)
	conv.Info = nil
	if len(info) > 0 {
		c.infoSum[opencodeID] = sum
	}
	c.convs[opencodeID] = conv
	c.known[opencodeID] = true
	return conv, nil
}

// conversationRoom answers errConversationLimit for a conversation the
// session has not stored once it holds its limit, and writes the note
// the first time any span of the session is refused one.
//
// @decision(DL-131)
func (r *Recorder) conversationRoom(ctx context.Context, c *capture, opencodeID string) error {
	if c.known[opencodeID] || len(c.known) < r.maxConversations {
		return nil
	}
	if c.warnedConvCap {
		return errConversationLimit
	}
	c.warnedConvCap = true
	r.warn(fmt.Sprintf("recorder %s: the session holds its limit of %d conversations; %s and any later new one are not recorded", c.sess.ID, r.maxConversations, opencodeID))
	if _, err := r.store.Conversations().FindEvent(ctx, c.sess.OrgID, c.sess.ID, MarkerConversationLimit, nil); errors.Is(err, store.ErrNotFound) {
		text := fmt.Sprintf("this session has reached its limit of %d conversations; new ones, such as further subagents, are not recorded", r.maxConversations)
		if merr := r.mark(ctx, c.sess, MarkerConversationLimit, text); merr != nil {
			return merr
		}
	}
	return errConversationLimit
}

// readMessages reads one conversation's messages newest first, a page
// at a time, and upserts each page. It stops at a short page, or after
// a page that changed nothing in the store, so a conversation never
// recorded is read back to its start and any later read takes what is
// new plus one page the store already holds. A read that fails is a
// warning: the next turn end reads again.
//
// @decision(DL-017) @decision(DL-069)
func (r *Recorder) readMessages(ctx context.Context, c *capture, conv store.Conversation) error {
	path := "/api/session/" + url.PathEscape(conv.OpenCodeID) + "/message?"
	query := url.Values{"limit": {strconv.Itoa(MessagePageSize)}, "order": {"desc"}}
	for page := 0; ; page++ {
		if page == r.maxPages {
			r.warn(fmt.Sprintf("recorder %s: read %s to its bound of %d pages, the newest %d messages; any older are not stored", c.sess.ID, conv.OpenCodeID, r.maxPages, r.maxPages*MessagePageSize))
			return nil
		}
		body, err := r.fetch(ctx, c.conn, path+query.Encode())
		if err != nil {
			r.warn(fmt.Sprintf("recorder %s: read %s: %s", c.sess.ID, conv.OpenCodeID, redact.Error(err)))
			return nil
		}
		// A page past MaxSandboxReadBytes arrives cut short, and older
		// pages would be lost without a word.
		if !json.Valid(body) {
			r.warn(fmt.Sprintf("recorder %s: a page of %s's messages is not JSON (%d bytes); older messages are not read", c.sess.ID, conv.OpenCodeID, len(body)))
			return nil
		}
		var msgs []store.Message
		for _, m := range parseMessages(body) {
			if m.ID == "" {
				continue
			}
			msg := store.Message{
				ID: store.NewID(r.now()), OrgID: c.sess.OrgID, SessionID: c.sess.ID, ConversationID: conv.ID,
				OpenCodeID: m.ID, Role: m.Role, Text: m.Text, Raw: m.Raw,
			}
			if m.At != nil {
				msg.At = *m.At
			}
			msgs = append(msgs, msg)
		}
		// @decision(DL-131)
		if err := r.room(ctx, c); err != nil {
			return err
		}
		changed, err := r.store.Conversations().PutMessages(ctx, msgs)
		if err != nil {
			return fmt.Errorf("write the store: %w", err)
		}
		c.count(changed, len(msgs), messageBytes(msgs))
		n, next := messagePage(body)
		if n < MessagePageSize || next == "" || changed == 0 {
			return nil
		}
		// A cursor keeps the first page's order and refuses an order
		// beside it.
		query = url.Values{"limit": {strconv.Itoa(MessagePageSize)}, "cursor": {next}}
	}
}

// count adds a write to the span's running size: changed of n rows,
// which together are bytes.
//
// @decision(DL-131)
func (c *capture) count(changed, n int, bytes int64) {
	if changed <= 0 || n <= 0 {
		return
	}
	c.size.Rows += int64(changed)
	c.size.Bytes += bytes * int64(changed) / int64(n)
}

func messageBytes(msgs []store.Message) int64 {
	var n int64
	for _, m := range msgs {
		n += int64(len(m.Text) + len(m.Raw))
	}
	return n
}

// messagePage is how many messages a /message page holds and its next
// cursor. The last page still carries one, so a short page is the end.
//
// @decision(DL-069)
func messagePage(body []byte) (int, string) {
	var page struct {
		Data   []json.RawMessage `json:"data"`
		Cursor struct {
			Next *string `json:"next"`
		} `json:"cursor"`
	}
	if json.Unmarshal(body, &page) != nil || page.Cursor.Next == nil {
		return len(page.Data), ""
	}
	return len(page.Data), *page.Cursor.Next
}

// turnEnded reports whether an event closes a turn, which is when the
// conversation's messages are read.
//
// @decision(DL-017)
func turnEnded(name string) bool {
	return strings.HasSuffix(name, ".execution.succeeded") ||
		strings.HasSuffix(name, ".execution.failed") ||
		strings.HasSuffix(name, ".execution.interrupted")
}

// frame writes one frame of the stream through to the store, and reads
// the conversation's messages when it ended a turn.
//
// @decision(DL-017)
func (r *Recorder) frame(ctx context.Context, c *capture, raw string) error {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}
	now := r.now()
	ev, ok := projectFrame(decoded, now)
	if !ok {
		return nil
	}
	if strings.HasPrefix(ev.Name, store.SpovNamespace) {
		// A frame claiming spov's own namespace. The store refuses it,
		// and it is dropped here rather than counted as a write failure
		// so a sandbox sending one cannot break its own recording.
		//
		// @decision(DL-058)
		if !c.warnedNamespace {
			c.warnedNamespace = true
			r.warn(fmt.Sprintf("recorder %s: a sandbox frame claims spov's own namespace and is not stored: %s", c.sess.ID, truncateFrame(raw)))
		}
		return nil
	}
	// A conversation's created frame says whether it is a subagent's, so
	// its row is written from the frame and needs no read.
	//
	// @decision(DL-110)
	if parent, ok := createdParent(decoded); ok && ev.Session != "" {
		if _, known := c.convs[ev.Session]; !known || parent != "" {
			if _, err := r.conversationWith(ctx, c, ev.Session, "", nil, parent); errors.Is(err, errConversationLimit) {
				return nil
			} else if err != nil {
				return err
			}
		}
	}
	// An ephemeral frame spov keeps anyway (DL-017): it has no durable
	// marker and therefore no sequence, and it is stored under its own
	// source so it is neither deduplicated against the durable log nor
	// mistaken for spov's own voice on the page.
	source := store.SourceOpenCode
	seq := durableSeq(decoded)
	if isEphemeralKept(ev.Name) {
		source, seq = store.SourceEphemeral, 0
	} else if seq == 0 {
		// Durable by its marker but without a position: it cannot be
		// keyed, so it cannot be a row. Said once, because a shape
		// change here would otherwise drop every frame in silence.
		if !c.warnedNoSeq {
			c.warnedNoSeq = true
			r.warn(fmt.Sprintf("recorder %s: a durable frame carries no durable.seq and is not stored: %s", c.sess.ID, truncateFrame(raw)))
		}
		return nil
	}
	conv, err := r.conversation(ctx, c, ev.Session)
	// @decision(DL-131)
	if errors.Is(err, errConversationLimit) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.room(ctx, c); err != nil {
		return err
	}
	// An imported conversation's log started again: its echo of the
	// import is already stored, and later frames continue the stored
	// sequence.
	//
	// @decision(DL-089)
	if source == store.SourceOpenCode && conv.SeqAfter > 0 {
		if seq <= conv.SeqAfter {
			return nil
		}
		seq = conv.SeqBase + seq - conv.SeqAfter
	}
	// Durable frames only: an ephemeral frame has seq 0 and would spend
	// the conversation's one check.
	//
	// @decision(DL-017)
	if source == store.SourceOpenCode {
		if err := r.gapCheck(ctx, c, conv.ID, seq); err != nil {
			return err
		}
	}
	at := now
	if ms := findMillis(decoded, createdKeys); ms > 0 {
		at = time.UnixMilli(ms).UTC()
	}
	wrote, err := r.store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: c.sess.OrgID, SessionID: c.sess.ID, ConversationID: conv.ID,
		Source: source, Seq: seq, Name: ev.Name, Text: ev.Text, At: at, Raw: []byte(raw),
	}})
	if err != nil {
		return fmt.Errorf("write the store: %w", err)
	}
	c.count(len(wrote), 1, int64(len(ev.Text)+len(raw)))
	if turnEnded(ev.Name) {
		return r.readMessages(ctx, c, conv)
	}
	return nil
}

// mark writes one of the recorder's own events.
//
// @decision(DL-017)
func (r *Recorder) mark(ctx context.Context, sess store.Session, name, text string) error {
	return r.markAt(ctx, sess, name, text, r.now())
}

// markAt writes one of the recorder's own events at a given time. The
// gap marker uses the attach time, not now: it can be decided minutes
// later, and the page orders by At before Seq, so the write time would
// sort it below frames this span already stored.
//
// @decision(DL-017)
func (r *Recorder) markAt(ctx context.Context, sess store.Session, name, text string, at time.Time) error {
	_, err := r.store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(r.now()), OrgID: sess.OrgID, SessionID: sess.ID,
		Source: store.SourceSpov, Name: name, Text: text, At: at,
	}})
	if err != nil {
		return fmt.Errorf("write the store: %w", err)
	}
	return nil
}

// Final reads a session's conversations once, as the principal deleting
// it, before its sandbox is terminated. Best-effort: a failure is a
// warning and the delete proceeds.
//
// @decision(DL-017)
func (r *Recorder) Final(ctx context.Context, p auth.Principal, sess store.Session) {
	ctx, cancel := context.WithTimeout(ctx, FinalReadTimeout)
	defer cancel()
	conn, err := r.connect(ctx, p, sess)
	if err != nil {
		r.warn(fmt.Sprintf("recorder %s: final read: %s", sess.ID, redact.Error(err)))
		return
	}
	c := newCapture(sess, conn)
	// The last read keeps the session's limits too.
	//
	// @decision(DL-131)
	err = r.measure(ctx, c)
	if err == nil {
		err = r.catchUp(ctx, c)
	}
	if err != nil && !errors.Is(err, errStorageLimit) {
		r.warn(fmt.Sprintf("recorder %s: final read: %s", sess.ID, redact.Error(err)))
	}
}

func truncateFrame(raw string) string {
	if len(raw) <= 200 {
		return raw
	}
	return raw[:200] + "…"
}
