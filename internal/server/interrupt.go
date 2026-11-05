package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
)

// Stopping a turn: the interrupt, and spov's own record of it. Like
// steer.go this is a once-per-human-act write, not a read path.
//
// @decision(DL-053)

// ResumeOnInterrupt is the `resume` Interrupt sends: true, so a message
// the person has sent is read at once rather than left parked. It must
// be sent: the sandbox treats an absent value as false.
//
// @decision(DL-053) @decision(DL-035)
const ResumeOnInterrupt = true

// ErrTooManyStops is the mint reserve refusing a stop. It shares
// ErrTooManyMessages' budget and status but not its wording, so a reader
// who pressed Stop is not told their messages were rate-limited.
//
// @decision(DL-053) @decision(DL-019)
var ErrTooManyStops = errors.New("server: too many interrupts for this session; try again shortly")

// ErrJobNotInterruptible is a job whose state does not allow its turn to
// be stopped; a 409, like ErrJobNotSteerable.
//
// @decision(DL-053) @decision(DL-023)
var ErrJobNotInterruptible = errors.New("server: this job's turn cannot be interrupted")

// InterruptRequest names which conversation to stop.
//
// @decision(DL-053)
type InterruptRequest struct {
	// Conversation is resolved as a message's is: against the store's own
	// rows, and omitted only when the session has exactly one.
	//
	// @decision(DL-019)
	Conversation string `json:"conversation,omitempty" doc:"OpenCode conversation id; required when the session has more than one"`
}

// Interrupted is what the sandbox answered.
//
// @decision(DL-053)
type Interrupted struct {
	// Interrupted is false when no turn was running. That is a success
	// and does not mean nothing happened: resume=true may still deliver
	// a waiting message and start a turn, so a client may not render it
	// as "the session was idle".
	//
	// @decision(DL-053) @decision(DL-059)
	Interrupted bool `json:"interrupted"`
	// Conversation is the one that was stopped, worth saying back when
	// the caller named none.
	Conversation string `json:"conversation" doc:"The OpenCode conversation whose turn was stopped"`
}

// Interrupt stops a session's turn and, for a job's session, writes the
// marker the supervisor's next pass reads as a steer. It never writes the
// job row.
//
// @decision(DL-053)
func (s *Service) Interrupt(ctx context.Context, p auth.Principal, ref string, req InterruptRequest) (Interrupted, error) {
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return Interrupted{}, err
	}
	// The job's own refusal first: a queued or provisioning job's session
	// is not running yet, and the job's state says why.
	//
	// @decision(DL-023)
	job, isJob, err := s.interruptibleJob(ctx, sess)
	if err != nil {
		return Interrupted{}, err
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return Interrupted{}, fmt.Errorf("%w (state %s)", ErrSessionNotRunning, sess.State)
	}
	conversation, err := s.steerInto(ctx, sess, req.Conversation)
	if err != nil {
		return Interrupted{}, err
	}

	// The recorder's mint reserve is checked only when this act would mint
	// (a cache miss), unlike Steer where the check is unconditional, so a
	// stop is never refused for a mint it would not spend.
	//
	// @decision(DL-053) @decision(DL-019) @decision(DL-030)
	if !s.hasCachedConnection(sess.ID) {
		if err := s.mintBudgetLeft(ctx, sess, ErrTooManyStops); err != nil {
			return Interrupted{}, err
		}
	}

	var answer []byte
	err = s.withConnection(ctx, p, sess, func(conn Connection) error {
		out, err := sandboxPostJSON(ctx, conn, interruptURL(conn, conversation, ResumeOnInterrupt), nil)
		answer = out
		return err
	})
	if err != nil {
		return Interrupted{}, err
	}
	out := Interrupted{Interrupted: wasInterrupted(answer), Conversation: conversation}
	// An interactive session gets the call and nothing else: no row, no
	// marker, no loop to hold.
	if !isJob {
		return out, nil
	}
	if err := s.markInterrupted(ctx, p, job); err != nil {
		return Interrupted{}, err
	}
	// Wake the recorder now: gate 3's interrupt discount reads
	// `session.execution.interrupted` from the store, and only an attached
	// span records it. Best-effort; the poll remains the mechanism.
	//
	// @decision(DL-053) @decision(DL-017)
	if s.OnStart != nil {
		s.OnStart(sess)
	}
	return out, nil
}

// markInterrupted writes the marker the supervisor acts on. Unlike
// recordSteer it is not best-effort: a failure fails the request, and a
// retry is safe. It runs on a detached context so a caller hanging up
// cannot leave the stopped job unmarked, bounded by recordSteer's
// SandboxReadTimeout although no sandbox is touched.
//
// @decision(DL-053) @decision(DL-019)
func (s *Service) markInterrupted(ctx context.Context, p auth.Principal, job store.Job) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SandboxReadTimeout)
	defer cancel()

	if err := s.MarkJobEvent(ctx, job, jobs.MarkerInterrupted, "turn stopped by "+cancelledBy(p)); err != nil {
		return fmt.Errorf("recording the interrupt: %w", err)
	}
	return nil
}

// interruptURL is a conversation's interrupt route with its `resume`
// value, which every caller states.
//
// @decision(DL-053) @decision(DL-035)
func interruptURL(conn Connection, conversation string, resume bool) string {
	return conn.URL + "/api/session/" + url.PathEscape(conversation) + "/interrupt?resume=" + strconv.FormatBool(resume)
}

// wasInterrupted reads the sandbox's `{"interrupted": bool}` answer. An
// unreadable body is taken as false; nothing downstream keys on it (see
// Interrupted).
//
// @decision(DL-053)
func wasInterrupted(answer []byte) bool {
	var envelope struct {
		Interrupted bool `json:"interrupted"`
	}
	if err := json.Unmarshal(answer, &envelope); err != nil {
		return false
	}
	return envelope.Interrupted
}

// interruptibleJob returns the session's job and whether it has one,
// refusing a job whose state does not allow a stop. An interactive session
// returns false and no error.
//
// @decision(DL-053)
func (s *Service) interruptibleJob(ctx context.Context, sess store.Session) (store.Job, bool, error) {
	job, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Job{}, false, nil
	}
	if err != nil {
		return store.Job{}, false, err
	}
	if why, ok := jobRefusesAnInterrupt(job.State); ok {
		return store.Job{}, false, fmt.Errorf("%w: the job is %s, so %s", ErrJobNotInterruptible, job.State, why)
	}
	return job, true, nil
}

// jobRefusesAnInterrupt reports the states where stopping the turn is
// refused, with the reason. `running` and `steered` are allowed.
//
// The refused set currently matches jobRefusesAMessage's, pinned by
// TestTheTwoRefusalsCoverTheSameStates. The two questions differ, so a
// deliberate divergence means changing that test.
//
// @decision(DL-053) @decision(DL-052)
func jobRefusesAnInterrupt(state store.JobState) (string, bool) {
	switch state {
	case store.JobBlocked:
		// Not because nothing is running: interrupting the turn would strand
		// the question rather than resolve it.
		return "its agent is waiting on a question, and interrupting the turn would strand it", true
	case store.JobQueued:
		return "it has no sandbox yet", true
	case store.JobProvisioning:
		// The machine has no provisioning -> steered edge, so the marker
		// would do nothing while the caller was told the turn stopped.
		return "its agent has not been started yet", true
	case store.JobDelivering:
		return "its run is over and it is being judged", true
	}
	if state.Terminal() {
		return "it has finished", true
	}
	return "", false
}

// interruptInput is one stop: the session in the path.
//
// @decision(DL-053)
type interruptInput struct {
	Ref  string `path:"ref" doc:"Session ID or name"`
	Body InterruptRequest
}

type interruptOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         Interrupted
}

// toInterruptHumaError maps the interrupt's own conflict.
//
// @decision(DL-053)
func toInterruptHumaError(err error) error {
	switch {
	case errors.Is(err, ErrJobNotInterruptible):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, ErrTooManyStops):
		return huma.Error429TooManyRequests(err.Error())
	}
	return toSteerHumaError(err)
}
