package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/url"
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

	mu sync.Mutex
	// runCtx is the context Run was started with; Wake parents captures
	// to it rather than to its caller's context, which is a job pass
	// that ends in seconds. Nil until Run starts, and Wake then does
	// nothing.
	//
	// @decision(DL-017)
	runCtx   context.Context
	captures map[string]context.CancelFunc
	// abandoned holds sessions whose redial budget ran out. Never
	// retried while this process leads; a new leader starts afresh.
	abandoned map[string]bool
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
		store: st, now: now, warn: warn, interval: ReconcileInterval, metrics: metrics,
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
		abandoned: map[string]bool{},
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

// Run syncs now and then on the reconciler's clock until ctx ends, at
// which point every capture is cancelled and waited for.
//
// @decision(DL-017)
func (r *Recorder) Run(ctx context.Context) {
	defer r.stopAll()
	// A new term starts the redial budget afresh, whichever process
	// leads it.
	r.mu.Lock()
	r.abandoned = map[string]bool{}
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
		if _, on := r.captures[sess.ID]; on || r.abandoned[sess.ID] {
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
	if _, on := r.captures[sess.ID]; on || r.abandoned[sess.ID] {
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
			r.abandoned[sess.ID] = true
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

// capture is one span's working state: the connection it holds and the
// conversations it has seen, by OpenCode id.
type capture struct {
	sess  store.Session
	conn  Connection
	convs map[string]store.Conversation
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
	return r.catchUp(ctx, c)
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
	ids, titles := conversations(listed)
	// The count comes from the sandbox; see MaxConversations.
	if len(ids) > MaxConversations {
		r.warn(fmt.Sprintf("recorder %s: the sandbox lists %d conversations; reading the first %d", c.sess.ID, len(ids), MaxConversations))
		ids = ids[:MaxConversations]
	}
	for _, id := range ids {
		conv, err := r.conversation(ctx, c, id, titles[id])
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
// it on first sight.
func (r *Recorder) conversation(ctx context.Context, c *capture, opencodeID, title string) (store.Conversation, error) {
	if conv, ok := c.convs[opencodeID]; ok && (title == "" || conv.Title == title) {
		return conv, nil
	}
	conv, err := r.store.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(r.now()), OrgID: c.sess.OrgID, SessionID: c.sess.ID, OpenCodeID: opencodeID, Title: title,
	})
	if err != nil {
		return store.Conversation{}, fmt.Errorf("write the store: %w", err)
	}
	c.convs[opencodeID] = conv
	return conv, nil
}

// readMessages reads one conversation's messages and upserts them. A
// read that fails is a warning: the next turn end reads again.
//
// @decision(DL-017)
func (r *Recorder) readMessages(ctx context.Context, c *capture, conv store.Conversation) error {
	body, err := r.fetch(ctx, c.conn, "/api/session/"+url.PathEscape(conv.OpenCodeID)+"/message")
	if err != nil {
		r.warn(fmt.Sprintf("recorder %s: read %s: %s", c.sess.ID, conv.OpenCodeID, redact.Error(err)))
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
	if _, err := r.store.Conversations().PutMessages(ctx, msgs); err != nil {
		return fmt.Errorf("write the store: %w", err)
	}
	return nil
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
	conv, err := r.conversation(ctx, c, ev.Session, "")
	if err != nil {
		return err
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
	_, err = r.store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: c.sess.OrgID, SessionID: c.sess.ID, ConversationID: conv.ID,
		Source: source, Seq: seq, Name: ev.Name, Text: ev.Text, At: at, Raw: []byte(raw),
	}})
	if err != nil {
		return fmt.Errorf("write the store: %w", err)
	}
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
	if err := r.catchUp(ctx, c); err != nil {
		r.warn(fmt.Sprintf("recorder %s: final read: %s", sess.ID, redact.Error(err)))
	}
}

func truncateFrame(raw string) string {
	if len(raw) <= 200 {
		return raw
	}
	return raw[:200] + "…"
}
