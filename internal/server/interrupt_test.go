package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// stopped is what the fake sandbox recorded about one POST /interrupt.
type stopped struct {
	path  string
	query string
}

type interruptFix struct {
	svc  *Service
	sess store.Session
	job  store.Job
	p    auth.Principal
	fake *sandbox.Fake

	mu    sync.Mutex
	calls []stopped
	// answer is the body the sandbox returns, so a test can choose
	// which half of S-LS1's measurement it is exercising.
	answer string
	// fail makes the sandbox refuse the interrupt.
	fail bool
}

func (f *interruptFix) stops() []stopped {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]stopped(nil), f.calls...)
}

// newInterruptFixture gives a running session whose sandbox answers
// POST /interrupt the way S-LS1 measured.
func newInterruptFixture(t *testing.T, state store.JobState, conversations ...string) *interruptFix {
	t.Helper()
	f := &interruptFix{answer: `{"interrupted":true}`}
	svc, job, fake := jobSandbox(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/interrupt") {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		f.mu.Lock()
		fail := f.fail
		if !fail {
			f.calls = append(f.calls, stopped{path: r.URL.Path, query: r.URL.RawQuery})
		}
		answer := f.answer
		f.mu.Unlock()
		if fail {
			http.Error(w, "the sandbox is unwell", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	})
	ctx := context.Background()
	sess, err := svc.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	f.svc, f.sess, f.fake, f.job = svc, sess, fake, job
	setJobState(t, svc, job, state)
	for _, id := range conversations {
		if _, err := svc.deps.Store.Conversations().UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(svc.deps.Now()), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.p = auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	return f
}

// markersOn returns the spov events of this name on a session.
func markersOn(t *testing.T, svc *Service, sess store.Session, name string) []store.Event {
	t.Helper()
	events, err := svc.deps.Store.Conversations().ListEvents(context.Background(), sess.OrgID, sess.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, e := range events {
		if e.Source == store.SourceSpov && e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// The whole of the endpoint: the sandbox is told to stop, and spov
// writes the marker its own supervisor reads as a steer.
//
// @decision(DL-053)
func TestAnInterruptStopsTheTurnAndRecordsIt(t *testing.T) {
	f := newInterruptFixture(t, store.JobRunning, "ses_1")
	got, err := f.svc.Interrupt(context.Background(), f.p, f.sess.ID, InterruptRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Interrupted {
		t.Error("interrupted = false for a sandbox that answered true")
	}
	if got.Conversation != "ses_1" {
		t.Errorf("conversation = %q, want ses_1 said back to a caller that named none", got.Conversation)
	}
	calls := f.stops()
	if len(calls) != 1 {
		t.Fatalf("the sandbox saw %d interrupts, want 1", len(calls))
	}
	if !strings.HasSuffix(calls[0].path, "/api/session/ses_1/interrupt") {
		t.Errorf("path = %q, want the conversation's own interrupt route", calls[0].path)
	}
	// `resume` has no server-side default in the sandbox: an absent value
	// behaves exactly as false, and a message the person sent before
	// pressing Interrupt would stay parked.
	//
	// @decision(DL-053)
	if calls[0].query != "resume=true" {
		t.Errorf("query = %q, want resume=true sent explicitly", calls[0].query)
	}
	marks := markersOn(t, f.svc, f.sess, jobs.MarkerInterrupted)
	if len(marks) != 1 {
		t.Fatalf("the session carries %d interrupt markers, want 1", len(marks))
	}
	// The marker names who pressed it. Service.Get admits an org admin
	// to another user's session, so a suspension with no attribution
	// leaves nobody to ask.
	if !strings.Contains(marks[0].Text, cancelledBy(f.p)) {
		t.Errorf("the marker says %q, which does not name who stopped the turn", marks[0].Text)
	}
}

// The handler does not write the job row; it writes a marker, and the
// supervisor's own pass moves the state.
//
// @decision(DL-053)
func TestAnInterruptDoesNotWriteTheJobRow(t *testing.T) {
	f := newInterruptFixture(t, store.JobRunning, "ses_1")
	if _, err := f.svc.Interrupt(context.Background(), f.p, f.sess.ID, InterruptRequest{}); err != nil {
		t.Fatal(err)
	}
	job, err := f.svc.deps.Store.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.JobRunning {
		t.Errorf("state = %v, want running: the handler moved the row the supervisor owns", job.State)
	}
}

// An interactive session gets the call and nothing else.
//
// @decision(DL-053)
func TestAnInterruptOnAnInteractiveSessionIsOnlyTheCall(t *testing.T) {
	f := newInterruptFixture(t, store.JobRunning)
	inter := interactiveFixture(t, &steerFix{svc: f.svc, sess: f.sess}, "ses_i")

	got, err := f.svc.Interrupt(context.Background(), f.p, inter.ID, InterruptRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Interrupted {
		t.Error("interrupted = false for a sandbox that answered true")
	}
	if len(f.stops()) != 1 {
		t.Fatalf("the sandbox saw %d interrupts, want 1", len(f.stops()))
	}
	if marks := markersOn(t, f.svc, inter, jobs.MarkerInterrupted); len(marks) != 0 {
		t.Errorf("an interactive session carries %d interrupt markers, want none", len(marks))
	}
}

// `interrupted: false` is an ordinary success, not an error.
//
// @decision(DL-053)
func TestAnIdleSessionIsNotAFailure(t *testing.T) {
	f := newInterruptFixture(t, store.JobRunning, "ses_1")
	f.mu.Lock()
	f.answer = `{"interrupted":false}`
	f.mu.Unlock()

	got, err := f.svc.Interrupt(context.Background(), f.p, f.sess.ID, InterruptRequest{})
	if err != nil {
		t.Fatalf("an idle session was an error: %v", err)
	}
	if got.Interrupted {
		t.Error("interrupted = true for a sandbox that answered false")
	}
	// The marker is written anyway: `false` does not mean the turn is
	// still going.
	if marks := markersOn(t, f.svc, f.sess, jobs.MarkerInterrupted); len(marks) != 1 {
		t.Errorf("the session carries %d interrupt markers, want 1", len(marks))
	}
}

// Every job state is decided one way or the other, read from the
// machine rather than from a list someone has to remember to extend.
//
// @decision(DL-053) @decision(DL-052)
func TestEveryJobStateDecidesAnInterrupt(t *testing.T) {
	allowed := map[store.JobState]bool{store.JobRunning: true, store.JobSteered: true}
	for _, state := range store.JobStates() {
		why, refused := jobRefusesAnInterrupt(state)
		if refused == allowed[state] {
			t.Errorf("%s: refused = %v, want %v", state, refused, !allowed[state])
		}
		if refused && strings.TrimSpace(why) == "" {
			t.Errorf("%s is refused with no reason", state)
		}
	}
}

// The two refusals name the same states today, an observation rather
// than a law; a deliberate divergence is a test to change, an
// accidental one a state nobody decided.
//
// @decision(DL-053) @decision(DL-052)
func TestTheTwoRefusalsCoverTheSameStates(t *testing.T) {
	for _, state := range store.JobStates() {
		_, message := jobRefusesAMessage(state)
		_, interrupt := jobRefusesAnInterrupt(state)
		if message != interrupt {
			t.Errorf("%s: a message is refused = %v but an interrupt = %v; "+
				"if that divergence is deliberate, say so here", state, message, interrupt)
		}
	}
}

// Blocked is refused: not because nothing is running, but because
// stopping its turn would strand the question rather than resolve it.
//
// @decision(DL-053)
func TestAParkedJobsTurnIsNotStopped(t *testing.T) {
	f := newInterruptFixture(t, store.JobBlocked, "ses_1")
	_, err := f.svc.Interrupt(context.Background(), f.p, f.sess.ID, InterruptRequest{})
	if !errors.Is(err, ErrJobNotInterruptible) {
		t.Fatalf("err = %v, want ErrJobNotInterruptible", err)
	}
	if !strings.Contains(err.Error(), "strand") {
		t.Errorf("the refusal says %q, which does not say what stopping it would cost", err)
	}
	// And nothing reached the sandbox: a refusal must cost neither a
	// mint nor a turn.
	if n := len(f.stops()); n != 0 {
		t.Errorf("the sandbox saw %d interrupts for a refused job", n)
	}
	if n := len(markersOn(t, f.svc, f.sess, jobs.MarkerInterrupted)); n != 0 {
		t.Errorf("a refused interrupt wrote %d markers", n)
	}
}

// A queued job's session is not running yet. The refusal names the job's
// state, which is what `spov jobs interrupt` was asked about.
//
// @decision(DL-023)
func TestAQueuedJobIsRefusedInTheJobsWords(t *testing.T) {
	f := newInterruptFixture(t, store.JobQueued, "ses_1")
	ctx := context.Background()
	sess, err := f.svc.deps.Store.Sessions().Get(ctx, f.sess.OrgID, f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State = store.StateStarting
	if err := f.svc.deps.Store.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Interrupt(ctx, f.p, f.sess.ID, InterruptRequest{})
	if !errors.Is(err, ErrJobNotInterruptible) || !strings.Contains(err.Error(), "no sandbox yet") {
		t.Fatalf("err = %v, want the job's own refusal", err)
	}
}

// Stop is allowed from steered, the primary use of it.
//
// @decision(DL-053)
func TestASteeredJobsTurnCanBeStopped(t *testing.T) {
	f := newInterruptFixture(t, store.JobSteered, "ses_1")
	if _, err := f.svc.Interrupt(context.Background(), f.p, f.sess.ID, InterruptRequest{}); err != nil {
		t.Fatalf("a steered job refused a stop: %v", err)
	}
	if n := len(markersOn(t, f.svc, f.sess, jobs.MarkerInterrupted)); n != 1 {
		t.Errorf("the session carries %d interrupt markers, want 1", n)
	}
}

// A marker that cannot be written fails the request, unlike recordSteer
// whose loss only costs attribution.
//
// @decision(DL-053) @decision(DL-019)
func TestATurnStoppedWithNoMarkerIsAnError(t *testing.T) {
	f := newInterruptFixture(t, store.JobRunning, "ses_1")
	f.svc.deps.Store = storeWith{Store: f.svc.deps.Store,
		convs: unwritableEvents{ConversationStore: f.svc.deps.Store.Conversations()}}

	_, err := f.svc.Interrupt(context.Background(), f.p, f.sess.ID, InterruptRequest{})
	if err == nil {
		t.Fatal("a turn was stopped and the marker lost, and the caller was told it worked")
	}
	if !strings.Contains(err.Error(), "recording the interrupt") {
		t.Errorf("err = %v, which does not say the record is what failed", err)
	}
	// The premise, asserted rather than assumed: this test is about a
	// turn that WAS stopped. Without it the same error would be
	// produced by a marker written before the sandbox was ever called,
	// which is the opposite bug.
	if n := len(f.stops()); n != 1 {
		t.Errorf("the sandbox saw %d interrupts; the marker was lost on a turn that was never stopped", n)
	}
}

// The order matters and nothing else pins it: writing the marker
// before the interrupt would produce a suspended row above an agent
// still working, whenever the sandbox refused.
//
// @decision(DL-053)
func TestAMarkerIsNotWrittenForATurnThatWasNotStopped(t *testing.T) {
	f := newInterruptFixture(t, store.JobRunning, "ses_1")
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()

	if _, err := f.svc.Interrupt(context.Background(), f.p, f.sess.ID, InterruptRequest{}); err == nil {
		t.Fatal("a sandbox that refused the interrupt was reported as a success")
	}
	if n := len(markersOn(t, f.svc, f.sess, jobs.MarkerInterrupted)); n != 0 {
		t.Errorf("a turn that was never stopped carries %d interrupt markers", n)
	}
	job, err := f.svc.deps.Store.Jobs().Get(context.Background(), f.job.OrgID, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.JobRunning {
		t.Errorf("state = %v, want running", job.State)
	}
}

// A non-owner cannot stop somebody else's turn, and the property is
// defended TWICE — which is worth saying rather than assuming, because
// it changes what this test can prove. Service.Get is the endpoint's
// own check, and Connect resolves through Service.Get as well (DL-030),
// so removing the first still leaves a colleague refused.
//
// What the first one decides is WHEN, and the last arm is where that
// shows: refused on its own terms, before the job and conversation
// reads run against a session the caller may not see. Same shape as
// TestAStoppedSessionsTurnCannotBeStopped.
//
// @decision(DL-053) @decision(DL-019) @decision(DL-030)
func TestANonOwnerCannotStopATurn(t *testing.T) {
	f := newInterruptFixture(t, store.JobRunning, "ses_1")
	// Same ORG, different user, not an admin. That is the case
	// Service.Get adds over a bare store read — a stranger in another
	// org is refused by the row lookup itself, so testing with one
	// proves nothing about the ownership check.
	colleague := auth.Principal{OrgID: f.sess.OrgID, UserID: "someone-else"}

	_, err := f.svc.Interrupt(context.Background(), colleague, f.sess.ID, InterruptRequest{})
	if err == nil {
		t.Fatal("a colleague stopped somebody else's turn")
	}
	// Not found rather than forbidden: the existence of another user's
	// session is not the caller's to learn.
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if n := len(f.stops()); n != 0 {
		t.Errorf("the sandbox saw %d interrupts from a non-owner", n)
	}
	// And an admin in the same org IS admitted, or the test above would
	// pass just as well against a check that refused everyone.
	admin := auth.Principal{OrgID: f.sess.OrgID, UserID: "an-admin", Admin: true}
	if _, err := f.svc.Interrupt(context.Background(), admin, f.sess.ID, InterruptRequest{}); err != nil {
		t.Errorf("an org admin was refused: %v", err)
	}

	// And the refusal is the ownership one rather than whatever the
	// later reads happen to report. On a session with no conversation
	// row, an endpoint that resolved the session without checking
	// access would tell a non-owner it has no conversation — a true
	// sentence about a session that is none of their business.
	bare := newInterruptFixture(t, store.JobRunning)
	other := auth.Principal{OrgID: bare.sess.OrgID, UserID: "someone-else"}
	_, err = bare.svc.Interrupt(context.Background(), other, bare.sess.ID, InterruptRequest{})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound: a non-owner was answered about the session's contents", err)
	}
}

// A dead session is refused before the connection is reached, so a
// stopped session is not refused only after spending a mint.
//
// @decision(DL-053) @decision(DL-030)
func TestAStoppedSessionsTurnCannotBeStopped(t *testing.T) {
	// No conversation row, deliberately. Connect refuses a stopped
	// session too, so the guard is belt-and-braces on the REFUSAL and
	// not on the credential — what it decides is which reason the
	// caller is given. Without it, steerInto runs first and a stopped
	// session is told it has no conversation, which is true, secondary,
	// and sends the reader looking at the recorder.
	f := newInterruptFixture(t, store.JobRunning)
	ctx := context.Background()
	sess, err := f.svc.deps.Store.Sessions().Get(ctx, f.sess.OrgID, f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess.State = store.StateStopped
	if err := f.svc.deps.Store.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}
	before := mintsOf(f.fake)

	_, err = f.svc.Interrupt(ctx, f.p, f.sess.ID, InterruptRequest{})
	if !errors.Is(err, ErrSessionNotRunning) {
		t.Fatalf("err = %v, want ErrSessionNotRunning", err)
	}
	if got := mintsOf(f.fake) - before; got != 0 {
		t.Errorf("a stopped session spent %d mints on its way to being refused", got)
	}
}

// The reserve is taken only when the act would mint: a cached
// connection stops however spent its budget, a fresh mint is refused
// against an exhausted one.
//
// @decision(DL-053) @decision(DL-019) @decision(DL-030)
func TestTheReserveIsTakenOnlyWhenAStopWouldMint(t *testing.T) {
	ctx := context.Background()
	spend := func(f *interruptFix) {
		t.Helper()
		// Spend the budget down past the reserve.
		for i := 0; i < MintsPerMinute-RecordingMintReserve; i++ {
			if err := f.svc.deps.Store.Mints().Create(ctx, store.Mint{
				ID: store.NewID(f.svc.deps.Now()), OrgID: f.sess.OrgID, SessionID: f.sess.ID,
				UserID: f.sess.OwnerID, CreatedAt: f.svc.deps.Now(),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// A cold session: the stop would mint, so the reserve refuses it.
	cold := newInterruptFixture(t, store.JobRunning, "ses_1")
	spend(cold)
	if _, err := cold.svc.Interrupt(ctx, cold.p, cold.sess.ID, InterruptRequest{}); !errors.Is(err, ErrTooManyStops) {
		t.Errorf("err = %v, want ErrTooManyStops for a stop that would mint on an exhausted budget", err)
	}
	if n := len(cold.stops()); n != 0 {
		t.Errorf("the sandbox saw %d interrupts past the reserve", n)
	}

	// A warm one: the connection is cached, so the stop costs no mint
	// and the same budget does not refuse it.
	warm := newInterruptFixture(t, store.JobRunning, "ses_1")
	if _, err := warm.svc.Interrupt(ctx, warm.p, warm.sess.ID, InterruptRequest{}); err != nil {
		t.Fatal(err)
	}
	if !warm.svc.hasCachedConnection(warm.sess.ID) {
		t.Fatal("the first stop did not cache a connection; this test proves nothing")
	}
	spend(warm)
	if _, err := warm.svc.Interrupt(ctx, warm.p, warm.sess.ID, InterruptRequest{}); err != nil {
		t.Errorf("a stop that would cost no mint was refused on a spent budget: %v", err)
	}
}

// The conversation is resolved against the store's own rows, exactly
// as a message's is: unnamed, the oldest; named, the one named.
//
// @decision(DL-053) @decision(DL-109)
func TestAnInterruptNamesAConversationTheSessionHas(t *testing.T) {
	f := newInterruptFixture(t, store.JobRunning)
	ctx := context.Background()
	// A person's conversation and a later one of the agent's own, in
	// creation order: the agent's subagent cannot predate the session
	// it was spawned from.
	older := f.svc.deps.Now()
	for i, id := range []string{"ses_1", "ses_2"} {
		if _, err := f.svc.deps.Store.Conversations().UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(f.svc.deps.Now()), OrgID: f.sess.OrgID, SessionID: f.sess.ID, OpenCodeID: id,
			CreatedAt: older.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Several, and none named: the oldest, which is the person's own.
	// Anything later is the agent's own or a race stray.
	got, err := f.svc.Interrupt(ctx, f.p, f.sess.ID, InterruptRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Conversation != "ses_1" {
		t.Errorf("conversation = %q, want ses_1, the oldest unnamed", got.Conversation)
	}
	if n := len(f.stops()); n != 1 {
		t.Fatalf("the sandbox saw %d interrupts, want 1", n)
	}
	// Named, and not this session's.
	_, err = f.svc.Interrupt(ctx, f.p, f.sess.ID, InterruptRequest{Conversation: "ses_elsewhere"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want a refusal for a conversation this session does not have", err)
	}
	// Named, and this session's.
	got, err = f.svc.Interrupt(ctx, f.p, f.sess.ID, InterruptRequest{Conversation: "ses_2"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Conversation != "ses_2" {
		t.Errorf("conversation = %q, want ses_2", got.Conversation)
	}
}

// unwritableEvents is a conversation store whose event writes fail,
// which is the only way the marker can go missing once the turn has
// already been stopped.
type unwritableEvents struct {
	store.ConversationStore
}

func (unwritableEvents) PutEvents(context.Context, []store.Event) ([]store.Event, error) {
	return nil, errors.New("the events table is unreachable")
}
