package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// Steering: relaying a human's typed message into a session's sandbox.
// A write once per human act, not a read path, so it stays out of the
// recorder's sandboxio.go.
//
// @decision(DL-019)

const (
	// MaxMessageBytes bounds a steering message's trimmed text, at the
	// same value as a job's goal.
	//
	// @decision(DL-019)
	MaxMessageBytes = MaxGoalBytes
	// MaxMessageIDBytes bounds a client-minted message id. OpenCode's
	// own are around thirty characters.
	//
	// @decision(DL-019) @decision(DL-059)
	MaxMessageIDBytes = 128
	// MessageIDPrefix is what a client-minted id must start with:
	// OpenCode's own message id space.
	//
	// @decision(DL-059)
	MessageIDPrefix = "msg_"
	// MessagesPerMinute is how many messages one session accepts in a
	// minute, per replica. It is abuse control only; RecordingMintReserve
	// is what protects the shared mint budget.
	//
	// @decision(DL-019)
	MessagesPerMinute = 6
	// MintsPerSteer is how many mints one send can cost: withConnection
	// mints on a cache miss and again when the sandbox refuses the
	// credential.
	//
	// @decision(DL-019) @decision(DL-030)
	MintsPerSteer = 2
	// RecordingMintReserve is the part of a session's durable mint budget
	// that steering may not use, read from the shared Mint rows. Three is the
	// recorder's worst case: RecorderBackoff allows at most three redials a minute.
	//
	// @decision(DL-019) @decision(DL-030) @decision(DL-017)
	RecordingMintReserve = 3
	// MessageWindow is the window MessagesPerMinute is counted over.
	//
	// @decision(DL-019)
	MessageWindow = time.Minute
)

// DeliverySteer and DeliveryQueue are OpenCode's two delivery modes:
// `steer` reaches the agent at its next step, once the running tool call
// has finished; `queue` waits for the whole turn to end. An omitted
// delivery is DeliverySteer.
//
// @decision(DL-019)
const (
	DeliverySteer = "steer"
	DeliveryQueue = "queue"
)

// AfterContinue and AfterHandover say what follows a message to a job;
// an omitted one is AfterContinue. The server writes it into the
// prompt's metadata, where the plugin and the supervisor read it.
//
// @decision(DL-052)
const (
	AfterContinue = jobs.AfterContinue
	AfterHandover = jobs.AfterHandover
)

// ErrTooManyMessages is the steering ceiling refusing. It is separate from
// ErrTooManyMints so the message names the limit that was hit.
//
// @decision(DL-019)
var ErrTooManyMessages = errors.New("server: too many messages for this session; try again shortly")

// ErrNoConversation is a session that has nothing to be steered into
// yet. A 409: the same call succeeds once the conversation row exists.
//
// @decision(DL-019)
var ErrNoConversation = errors.New("server: the session has no conversation recorded yet")

// SteerRequest is one message for a session's sandbox.
//
// @decision(DL-019)
type SteerRequest struct {
	// Conversation is the OpenCode session id to send into. It may be
	// omitted only when the session has exactly one conversation.
	Conversation string `json:"conversation,omitempty" doc:"OpenCode conversation id; required when the session has more than one"`
	// Text is what the person typed.
	Text string `json:"text" minLength:"1" maxLength:"16384" doc:"The message to send"`
	// Delivery is steer or queue; omitted means steer.
	//
	// @decision(DL-019)
	Delivery string `json:"delivery,omitempty" enum:"steer,queue" doc:"steer (the default) reaches the agent at its next step, once the running tool call has finished; queue waits for the whole turn to end"`
	// After says what follows the message on a job: continue (the
	// default) keeps the job autonomous, handover hands it to the person
	// once the message is read. Ignored on an interactive session.
	//
	// @decision(DL-052)
	After string `json:"after,omitempty" enum:"continue,handover" doc:"On a job: continue (the default) keeps it working on its own after this message; handover pauses it for you once the agent has read it"`
	// ID is a client-minted message id: the idempotency key and the key a
	// client's optimistic echo reconciles on. Omitted, the sandbox mints one.
	//
	// @decision(DL-059)
	ID string `json:"id,omitempty" maxLength:"128" doc:"Client-minted msg_ id; also the idempotency key"`
}

// SteeredMessage is what the sandbox admitted.
//
// @decision(DL-019)
type SteeredMessage struct {
	// ID is the message's own id, which is the caller's when it minted
	// one.
	ID string `json:"id,omitempty"`
	// Conversation is the conversation it went into, which is worth
	// saying back when the caller omitted it.
	Conversation string `json:"conversation"`
	Delivery     string `json:"delivery"`
	// After is what follows the message on a job, empty on an
	// interactive session.
	//
	// @decision(DL-052)
	After string `json:"after,omitempty"`
}

// MarkerSteered records who sent a message into this session. It is the
// only attribution: the inbox item is typed `user` and names nobody, and
// with connection reuse the Mint row no longer does either.
//
// @decision(DL-019) @decision(DL-030) @decision(DL-058)
const MarkerSteered = store.SpovNamespace + "session.steered"

// Steer relays one message into a session's sandbox.
//
// @decision(DL-019)
func (s *Service) Steer(ctx context.Context, p auth.Principal, ref string, req SteerRequest) (SteeredMessage, error) {
	text, delivery, after, id, err := validSteer(req)
	if err != nil {
		return SteeredMessage{}, err
	}
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return SteeredMessage{}, err
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return SteeredMessage{}, fmt.Errorf("%w (state %s)", ErrSessionNotRunning, sess.State)
	}
	isJob, err := s.steerableJob(ctx, sess)
	if err != nil {
		return SteeredMessage{}, err
	}
	if !isJob {
		after = ""
	}
	conversation, err := s.steerInto(ctx, sess, req.Conversation)
	// A session with none gets one on its first message, inside the
	// connection below.
	//
	// @decision(DL-019)
	// A job's conversation is jobConversation's to create.
	first := errors.Is(err, ErrNoConversation) && !isJob
	if err != nil && !first {
		return SteeredMessage{}, err
	}
	// Both bounds are taken before anything reaches the sandbox, so a
	// refused caller spends no mint.
	if err := s.mintBudgetLeft(ctx, sess, ErrTooManyMessages); err != nil {
		return SteeredMessage{}, err
	}
	if !s.messages.take(sess.ID, s.deps.Now()) {
		return SteeredMessage{}, ErrTooManyMessages
	}

	body, err := json.Marshal(steerBody(id, text, delivery, after))
	if err != nil {
		return SteeredMessage{}, fmt.Errorf("encoding the message: %w", err)
	}
	var answer []byte
	err = s.withConnection(ctx, p, sess, func(conn Connection) error {
		// A rerun after a refused credential reuses what the first run
		// created, recorded or not.
		if first && conversation == "" {
			id, err := s.firstConversation(ctx, conn, sess)
			if err != nil {
				return err
			}
			conversation = id
		}
		out, err := sandboxPostJSON(ctx, conn, promptURL(conn, conversation), body)
		answer = out
		return err
	})
	if err != nil {
		return SteeredMessage{}, err
	}

	sent := SteeredMessage{ID: promptedID(answer, id), Conversation: conversation, Delivery: delivery, After: after}
	s.recordSteer(ctx, p, sess, sent)
	// Nudge the recorder now rather than at its next poll. Best-effort and
	// non-blocking; the poll remains the mechanism.
	//
	// @decision(DL-017)
	if s.OnStart != nil {
		s.OnStart(sess)
	}
	return sent, nil
}

// steerBody is what POST /prompt is sent. `resume` and `delivery` are
// always explicit. A job's message carries what follows it in the
// prompt's metadata, never in its text; an empty after carries none.
//
// @decision(DL-019) @decision(DL-052)
func steerBody(id, text, delivery, after string) map[string]any {
	body := map[string]any{"text": text, "resume": true, "delivery": delivery}
	if id != "" {
		body["id"] = id
	}
	if after != "" {
		body["metadata"] = map[string]any{"spov": map[string]any{"after": after}}
	}
	return body
}

// promptURL is a conversation's prompt route, the only one that makes a
// `user` inbox item.
//
// @decision(DL-019) @decision(DL-052)
func promptURL(conn Connection, conversation string) string {
	return conn.URL + "/api/session/" + url.PathEscape(conversation) + "/prompt"
}

// promptedID reads the message id out of the sandbox's answer,
// {"data":{"id":"msg_…", …}}, falling back to the id the caller minted
// when the body is unreadable or its id fails validMessageID.
//
// @decision(DL-019)
func promptedID(answer []byte, minted string) string {
	var envelope struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(answer, &envelope); err == nil && envelope.Data.ID != "" {
		// The sandbox is untrusted, so its id gets the caller's check.
		if validMessageID(envelope.Data.ID) == nil {
			return envelope.Data.ID
		}
	}
	return minted
}

// validSteer checks what the caller sent and returns the values that
// will be used, so nothing downstream re-derives them.
//
// @decision(DL-019)
func validSteer(req SteerRequest) (text, delivery, after, id string, err error) {
	text = strings.TrimSpace(req.Text)
	switch {
	case text == "":
		return "", "", "", "", fmt.Errorf("%w: a message needs some text", ErrInvalidRequest)
	// Measured on the trimmed text, which is what is sent.
	case len(text) > MaxMessageBytes:
		return "", "", "", "", fmt.Errorf("%w: the message is %d bytes; the limit is %d",
			ErrInvalidRequest, len(text), MaxMessageBytes)
	}

	// @decision(DL-019)
	delivery = strings.TrimSpace(req.Delivery)
	switch delivery {
	case "":
		delivery = DeliverySteer
	case DeliverySteer, DeliveryQueue:
	default:
		return "", "", "", "", fmt.Errorf("%w: delivery %q is neither %s nor %s",
			ErrInvalidRequest, delivery, DeliverySteer, DeliveryQueue)
	}

	// @decision(DL-052)
	after = strings.TrimSpace(req.After)
	switch after {
	case "":
		after = AfterContinue
	case AfterContinue, AfterHandover:
	default:
		return "", "", "", "", fmt.Errorf("%w: after %q is neither %s nor %s",
			ErrInvalidRequest, after, AfterContinue, AfterHandover)
	}

	id = strings.TrimSpace(req.ID)
	if id != "" {
		if err := validMessageID(id); err != nil {
			return "", "", "", "", err
		}
	}
	return text, delivery, after, id, nil
}

// validMessageID checks a message id's prefix, length and character set.
// The id becomes a primary key in the sandbox and the recorder's row key.
//
// @decision(DL-059) @decision(DL-019)
func validMessageID(id string) error {
	switch {
	case !strings.HasPrefix(id, MessageIDPrefix):
		return fmt.Errorf("%w: a message id starts with %q", ErrInvalidRequest, MessageIDPrefix)
	case len(id) > MaxMessageIDBytes:
		return fmt.Errorf("%w: the message id is %d bytes; the limit is %d",
			ErrInvalidRequest, len(id), MaxMessageIDBytes)
	}
	for _, r := range id {
		ok := r == '_' || r == '-' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return fmt.Errorf("%w: a message id is letters, digits, %q and %q", ErrInvalidRequest, "_", "-")
		}
	}
	return nil
}

// ErrJobNotSteerable is a message to a job whose state would enqueue it
// and never deliver it. A 409.
//
// @decision(DL-052)
var ErrJobNotSteerable = errors.New("server: this job cannot take a message")

// steerableJob refuses a message this session's job cannot take, and
// reports whether the session has a job.
//
// @decision(DL-052) @decision(DL-019)
func (s *Service) steerableJob(ctx context.Context, sess store.Session) (bool, error) {
	job, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if why, ok := jobRefusesAMessage(job.State); ok {
		return true, fmt.Errorf("%w: the job is %s, so %s", ErrJobNotSteerable, job.State, why)
	}
	return true, nil
}

// jobRefusesAMessage reports the states where a message would be
// enqueued and not delivered, with the reason to show. Every job state
// must be decided here; TestEveryJobStateIsDecided checks it.
//
// @decision(DL-052) @decision(DL-045)
func jobRefusesAMessage(state store.JobState) (string, bool) {
	switch state {
	case store.JobBlocked:
		// The agent is not reading its inbox while it waits.
		return "the agent is parked on a question and will not read it", true
	case store.JobQueued:
		return "it has no sandbox yet", true
	case store.JobProvisioning:
		// The agent has no goal yet, and provisioning has no -> steered edge.
		return "its agent has not been started yet", true
	case store.JobDelivering:
		return "its run is over and it is being judged", true
	}
	if state.Terminal() {
		return "it has finished", true
	}
	return "", false
}

// steerInto resolves which conversation the message goes to, against
// the store's own rows and never on the caller's word, since OpenCode
// ids are unique per session only.
//
// @decision(DL-019)
func (s *Service) steerInto(ctx context.Context, sess store.Session, named string) (string, error) {
	convs, err := s.deps.Store.Conversations().ListConversations(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return "", err
	}
	named = strings.TrimSpace(named)
	if named != "" {
		for _, c := range convs {
			if c.OpenCodeID == named {
				return named, nil
			}
		}
		// Says nothing about whether the id exists in another session.
		return "", fmt.Errorf("%w: this session has no conversation %q", ErrInvalidRequest, named)
	}
	switch len(convs) {
	case 0:
		// A send creates one (firstConversation); a cancel has nothing
		// to cancel in.
		return "", ErrNoConversation
	case 1:
		return convs[0].OpenCodeID, nil
	default:
		return "", fmt.Errorf("%w: this session has %d conversations; name one",
			ErrInvalidRequest, len(convs))
	}
}

// firstConversation creates a session's first conversation in its
// landing, as jobConversation does a job's, and records it. Serialised
// per session and re-checked inside, so concurrent first sends on one
// replica create one. Steer keeps the id it returns, so withConnection
// running its closure twice does not create a second.
//
// @decision(DL-019)
func (s *Service) firstConversation(ctx context.Context, conn Connection, sess store.Session) (string, error) {
	unlock := s.firstSends.lock(sess.ID)
	defer unlock()
	convs, err := s.deps.Store.Conversations().ListConversations(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return "", err
	}
	switch len(convs) {
	case 0:
	case 1:
		return convs[0].OpenCodeID, nil
	default:
		return "", fmt.Errorf("%w: this session has %d conversations; name one", ErrInvalidRequest, len(convs))
	}
	id, err := createConversation(ctx, conn, sessionDir(sess))
	if err != nil {
		return "", fmt.Errorf("opening a conversation: %w", err)
	}
	// @decision(DL-014)
	if err := setConversationModel(ctx, conn, id, sess.Request.Model); err != nil {
		s.deps.Warn(fmt.Sprintf("steer %s: setting the model: %s", sess.ID, redact.Error(err)))
	}
	if _, err := s.deps.Store.Conversations().UpsertConversation(ctx, store.Conversation{
		ID: store.NewID(s.deps.Now()), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: id,
	}); err != nil {
		// The conversation exists either way, and the recorder records it.
		s.deps.Warn(fmt.Sprintf("steer %s: recording the conversation: %s", sess.ID, redact.Error(err)))
	}
	return id, nil
}

// keyedLocks is one mutex per key, dropped once nobody holds or waits
// for it.
//
// @decision(DL-019)
type keyedLocks struct {
	mu   sync.Mutex
	byID map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// lock takes the key's mutex and returns its release.
func (k *keyedLocks) lock(id string) func() {
	k.mu.Lock()
	if k.byID == nil {
		k.byID = map[string]*keyedLock{}
	}
	l := k.byID[id]
	if l == nil {
		l = &keyedLock{}
		k.byID[id] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.byID, id)
		}
		k.mu.Unlock()
	}
}

// recordSteer writes the audit marker after the send. A failure only
// warns: failing a delivered message would invite a duplicate retry.
//
// @decision(DL-019)
func (s *Service) recordSteer(ctx context.Context, p auth.Principal, sess store.Session, sent SteeredMessage) {
	// Detached, so a caller hanging up after the send cannot skip the record.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SandboxReadTimeout)
	defer cancel()

	now := s.deps.Now()
	note := "message sent by " + cancelledBy(p)
	if sent.Delivery == DeliveryQueue {
		note += " (queued)"
	}
	if sent.After == AfterHandover {
		note += ", handing the job over"
	}
	// The message id ties the marker to one message.
	if sent.ID != "" {
		note += ": " + sent.ID
	}
	_, err := s.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: sess.OrgID, SessionID: sess.ID,
		Source: store.SourceSpov, Name: MarkerSteered, Text: note, At: now,
	}})
	if err != nil {
		s.deps.Warn(fmt.Sprintf("steer %s: recording the act: %s", sess.ID, redact.Error(err)))
	}
}

// mintBudgetLeft returns refusal when an act costing MintsPerSteer would
// eat into RecordingMintReserve. Callers pass their own sentinel, since a
// send and a stop both spend from this budget. A failed read is returned
// as an error, neither a refusal nor a pass.
//
// @decision(DL-019) @decision(DL-030) @decision(DL-053)
func (s *Service) mintBudgetLeft(ctx context.Context, sess store.Session, refusal error) error {
	spent, err := s.deps.Store.Mints().CountSince(ctx, sess.OrgID, sess.ID, s.deps.Now().Add(-time.Minute))
	if err != nil {
		return fmt.Errorf("reading the session's mint budget: %w", err)
	}
	if spent+MintsPerSteer > MintsPerMinute-RecordingMintReserve {
		return refusal
	}
	return nil
}

// messageLimiter counts sends per session over a sliding window:
// attemptLimiter's shape without its lockout.
//
// @decision(DL-019)
type messageLimiter struct {
	mu   sync.Mutex
	max  int
	byID map[string][]time.Time
}

func newMessageLimiter(max int) *messageLimiter {
	return &messageLimiter{max: max, byID: map[string][]time.Time{}}
}

// take records a send and reports whether it is within the ceiling. It
// records nothing when it refuses, so a caller hammering a full window
// does not push it out.
func (l *messageLimiter) take(id string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	seen := l.byID[id]
	if len(seen) >= l.max {
		return false
	}
	l.byID[id] = append(seen, now)
	return true
}

// sweep drops times outside the window, and sessions left with none, so
// the map does not grow with every session ever steered. Called under
// the lock.
func (l *messageLimiter) sweep(now time.Time) {
	cut := now.Add(-MessageWindow)
	for id, seen := range l.byID {
		kept := seen[:0]
		for _, t := range seen {
			if t.After(cut) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(l.byID, id)
			continue
		}
		l.byID[id] = kept
	}
}

// steerInput is one send: the session in the path, the message in the
// body.
//
// @decision(DL-019)
type steerInput struct {
	Ref  string `path:"ref" doc:"Session ID or name"`
	Body SteerRequest
}

type steerOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SteeredMessage
}

// toSteerHumaError maps the steering errors toHumaError does not.
//
// @decision(DL-019)
func toSteerHumaError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrTooManyMessages):
		return huma.Error429TooManyRequests(err.Error())
	case errors.Is(err, ErrNoConversation):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, ErrJobNotSteerable):
		return huma.Error409Conflict(err.Error())
	}
	return toHumaError(err)
}
