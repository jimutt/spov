package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jimutt/spov/internal/store"
)

// GapNote is what a job's Error says when its state was concluded over
// a range the recorder was not fully listening to.
//
// @decision(DL-046)
const GapNote = "recording gap: the state was concluded from an incomplete range, and may be stale"

// FollowBatch is how many events are read per store call. The supervisor
// is not latency-critical — its freshness is already the recorder's plus
// the feed's seam — so this trades a larger read for fewer of them.
//
// @decision(DL-046)
const FollowBatch = 256

// followResult is what one pass over a job's new events concluded.
//
// @decision(DL-046)
type followResult struct {
	// Cursor is the Pos to resume from. It advances over every event
	// read, including ones that signalled nothing, so a busy session
	// does not re-deliver the same rows forever.
	Cursor int64
	// State is the job's state after the pass.
	State store.JobState
	// Moved reports whether State differs from where the job started.
	Moved bool
	// SawGap reports that the recorder was not listening for part of
	// this range. The job's state is still whatever the events say; the
	// caller may want to say so out loud.
	SawGap bool
	// TurnsEnded counts completed turns in this pass, which is what a
	// progress bound would read.
	TurnsEnded int
	// Stopped reports that the plugin announced its loop has finished,
	// for any reason including a declaration, and moves the job to
	// delivering. A declaration must be folded in here too, or the
	// caller's abandoned-sandbox backstop overwrites the delivery.
	//
	// @decision(DL-048)
	Stopped bool
	// Answered counts permissions answered in this pass. It moves no
	// state — the frame names no principal, so an answer cannot be told
	// apart from the job's own plugin answering itself — but it is
	// counted rather than dropped so DL-050 has it and so a
	// reader can see the signal is observed.
	Answered int
}

// follow reads a job's new events and works out where they leave it.
//
// It is a pure fold over stored rows: it writes nothing and touches no
// sandbox, so what the events mean is testable without either.
//
// @decision(DL-046)
func (s *Supervisor) follow(ctx context.Context, job store.Job, cursor int64) (followResult, error) {
	out := followResult{Cursor: cursor, State: job.State}
	if isTerminalJob(job.State) {
		return out, nil
	}

	// A steer this pass read but has not committed to, held because a
	// resume marker later in the stream retracts it. Held rather than
	// scanned for per batch, so the boundary works across a FollowBatch
	// edge falling between the two.
	//
	// @decision(DL-054)
	// steeredFrom/steeredMoved are the state and Moved flag before the
	// steer, so a retracted steer restores them rather than assuming
	// `running`.
	var steeredFrom store.JobState
	var steeredMoved bool
	steerHeld := false
	// A permission asked while the steer was held. DL-045's machine has
	// no steered -> blocked edge, so if the steer is later retracted the
	// park must be reapplied rather than lost behind the cursor.
	//
	// @decision(DL-054) @decision(DL-046)
	parkedWhileSteered := false
	// Whether each item enqueued in this pass takes the job over, by
	// inbox id, so a delivery read in the same pass needs no store read.
	//
	// @decision(DL-052)
	takeovers := map[string]bool{}
	// Only the job's own conversation is a person's, or the run's turn;
	// read once per pass, a store read.
	//
	// @decision(DL-114)
	own, err := s.ownConversation(ctx, job)
	if err != nil {
		return out, err
	}

	for {
		before := out.Cursor
		events, err := s.Store.Conversations().ListEvents(ctx, job.OrgID, job.SessionID, out.Cursor, FollowBatch)
		if err != nil {
			return out, fmt.Errorf("supervisor: reading events for job %s: %w", job.ID, err)
		}
		if len(events) == 0 {
			return out, nil
		}
		for _, e := range events {
			// The cursor advances over every event, signalling or not.
			// Advancing only on the ones that mattered would re-read
			// the rest on the next pass forever.
			if e.Pos > out.Cursor {
				out.Cursor = e.Pos
			}
			sig := signalFor(e)
			// A subagent's turn ending is not the run's.
			//
			// @decision(DL-114)
			if sig == signalTurnEnded && othersFrame(e, own) {
				continue
			}
			// A person's message takes the job over when it is delivered,
			// and only the enqueue says whether it is one that does. A
			// subagent's prompt is a user item in its own inbox, written
			// by the agent, so only the job's own conversation counts.
			//
			// @decision(DL-052) @decision(DL-114)
			if e.Source == store.SourceOpenCode && !othersFrame(e, own) {
				switch e.Name {
				case EventInboxEnqueued:
					if id := InboxID(e.Raw); id != "" {
						takeovers[id] = takesOver(e)
					}
				case EventInboxDelivered:
					over, err := s.takeoverDelivered(ctx, job, e, takeovers, own)
					if err != nil {
						return out, err
					}
					if over {
						sig = signalSteered
					}
				}
			}
			switch sig {
			case signalGap:
				out.SawGap = true
				continue
			case signalResumed:
				// The boundary: retracts a steer read before it. The
				// resume already wrote `running`, so this moves no state
				// of its own. The retraction is conditional on the state
				// still being the steer's, or a signal folded in between
				// (a declared stop, say) would be discarded along with it.
				//
				// @decision(DL-054)
				if steerHeld {
					steerHeld = false
					if out.State == store.JobSteered {
						out.State = steeredFrom
						out.Moved = steeredMoved
						// A park the steer swallowed is applied now,
						// against the state the steer interrupted.
						if parkedWhileSteered {
							if next, moved := nextState(out.State, signalParked); moved {
								out.State = next
								out.Moved = true
							}
						}
					}
				}
				parkedWhileSteered = false
				continue
			case signalSteered:
				// Applied immediately, like every other signal, and
				// remembered so a later resume can restore the prior
				// state without the pass guessing in advance.
				//
				// @decision(DL-054)
				if !steerHeld {
					steeredFrom = out.State
					steeredMoved = out.Moved
				}
				steerHeld = true
			case signalParked:
				// Remembered while a steer is held, in case the steer
				// is retracted. Still falls through to nextState, which
				// refuses steered -> blocked on its own.
				//
				// @decision(DL-054)
				if steerHeld {
					parkedWhileSteered = true
				}
			case signalTurnEnded:
				out.TurnsEnded++
			case signalAnswered:
				out.Answered++
			case signalReleased:
				// Released only when nothing else is still asked. The
				// count reads the whole log, since the other request may
				// have been asked in an earlier pass.
				//
				// @decision(DL-054)
				if out.State != store.JobBlocked && !parkedWhileSteered {
					continue
				}
				open, err := s.unansweredAsks(ctx, job, AnsweredRequest(e.Raw))
				if err != nil {
					return out, err
				}
				if open > 0 {
					continue
				}
				// A park a held steer swallowed is answered now, so a
				// later retraction must not reapply it.
				//
				// @decision(DL-054)
				parkedWhileSteered = false
			case signalStopped, signalDeclared:
				out.Stopped = true
			}
			next, moved := nextState(out.State, sig)
			if moved {
				out.State = next
				out.Moved = true
			}
		}
		// A short batch is the end of what the store holds.
		if len(events) < FollowBatch {
			return out, nil
		}
		// A full batch that did not advance the cursor would page over
		// the same rows forever. Pos is strictly increasing in both
		// stores so this cannot happen today; the guard makes it
		// impossible rather than merely unlikely.
		if out.Cursor <= before {
			return out, fmt.Errorf(
				"supervisor: job %s: a full batch did not advance the cursor past %d", job.ID, before)
		}
	}
}

// applyFollow writes what a pass concluded: the cursor always, and the
// state when the pass moved it. The cursor is written even when nothing
// moved, since the supervisor's conclusions are not idempotent —
// re-reading an old event could re-derive a state that silently undoes an
// explicit act.
//
// The job is re-read inside the write so the transition is checked
// against what the row says now, not what it said when the pass started.
//
// It reports the state it actually wrote, or the job's own state when it
// wrote none, because DL-054 enacts a pause on the row reaching steered
// and must not fire for a conclusion the store rejected.
//
// @decision(DL-045) @decision(DL-046) @decision(DL-054)
func (s *Supervisor) applyFollow(ctx context.Context, job store.Job, res followResult) (store.JobState, error) {
	applied := job.State
	if !res.Moved && res.Cursor <= job.Cursor {
		return applied, nil
	}
	err := s.Store.Tx(ctx, func(tx store.Store) error {
		current, err := tx.Jobs().Get(ctx, job.OrgID, job.ID)
		if err != nil {
			return err
		}
		// The cursor advances whatever the state does. A terminal job
		// still records what was read, so a later pass does not walk
		// the same events again.
		advanced := false
		if res.Cursor > current.Cursor {
			current.Cursor = res.Cursor
			advanced = true
		}
		switch {
		case !res.Moved:
			// Nothing to write beyond the cursor.
		case isTerminalJob(current.State):
			// Decided while the pass ran. The pass's conclusion is
			// stale, not wrong, and the decided state wins.
		case current.State == res.State:
			// Already there.
		case !store.CanTransition(current.State, res.State):
			// The row moved under the pass — a human resumed it, say.
			// Forcing the transition would be writing a conclusion
			// drawn from a state that no longer holds.
			s.warn("supervisor: job %s moved to %v under the pass; not writing %v",
				job.ID, current.State, res.State)
		default:
			// A person's answer releasing a blocked job clears the
			// suspension's note, as Resume does.
			//
			// @decision(DL-054)
			if current.State == store.JobBlocked && res.State == store.JobRunning {
				current.Error = ""
			}
			current.State = res.State
			applied = res.State
			advanced = true
			// A state concluded across a gap is drawn from a range
			// known to be incomplete: an `asked` before a gap may have
			// been answered inside it. The state is still written —
			// the job WAS parked, and leaving `blocked` is an explicit
			// act anyway, so a stale park is recoverable while a
			// silently-`running` parked job is not — but the row says
			// the range was incomplete rather than presenting it as a
			// clean observation.
			if res.SawGap {
				current.Error = GapNote
			}
		}
		if !advanced {
			return nil
		}
		// The guard above reads without a row lock, while Update
		// re-checks under one. A job decided in that window reaches
		// here with a transition the store then refuses — the outcome
		// this wanted anyway, so it is treated as the decided state
		// winning quietly rather than surfaced as a failure River
		// would retry.
		if err := tx.Jobs().Update(ctx, current); err != nil {
			if errors.Is(err, store.ErrInvalidTransition) {
				s.warn("supervisor: job %s was decided while the pass ran; leaving it alone", job.ID)
				// The row kept whatever it had. Reporting the state
				// this pass wanted would have the caller act on a move
				// the store refused.
				applied = job.State
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		// A failed transaction wrote nothing, so the row is where the
		// pass found it.
		return job.State, err
	}
	return applied, nil
}

// takeoverDelivered reports whether a delivery is of an item that takes
// the job over, joining it to its enqueue: from this pass when it was
// read here, else from the store. An enqueue that was never stored (a
// recorder gap) takes nothing over; the plugin, which saw it live, holds
// the loop, and the wall clock ends the job (DL-052).
//
// @decision(DL-052) @decision(DL-114)
func (s *Supervisor) takeoverDelivered(ctx context.Context, job store.Job, e store.Event, seen map[string]bool, own string) (bool, error) {
	id := InboxID(e.Raw)
	if id == "" {
		return false, nil
	}
	if over, ok := seen[id]; ok {
		return over, nil
	}
	// The key and the value together, as the compact frame spells them:
	// an id quoted inside a message's text is escaped and cannot match.
	needle := []byte(`"inboxID":"` + id + `"`)
	enqueued, err := s.Store.Conversations().FindEvent(ctx, job.OrgID, job.SessionID, EventInboxEnqueued, needle)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("supervisor: finding the enqueue of %s for job %s: %w", id, job.ID, err)
	}
	// The bytes matched; the frame must also be about this item, in the
	// job's own conversation.
	if InboxID(enqueued.Raw) != id || enqueued.Source != store.SourceOpenCode || othersFrame(enqueued, own) {
		return false, nil
	}
	return takesOver(enqueued), nil
}

// unansweredAsks counts the job's permission requests that nobody has
// answered: asked, with neither the sandbox's permission.replied nor a
// MarkerAnswered for it, nor a turn of its conversation ending after it
// (an interrupt drops a pending request without a reply, spike S-F1),
// and not the request just answered, whose replied frame may not be
// stored yet. Read only when a person's answer is folded, which is rare,
// so it pages the whole log.
//
// @decision(DL-054) @decision(DL-020)
func (s *Supervisor) unansweredAsks(ctx context.Context, job store.Job, answered string) (int, error) {
	// By request, the conversation it was asked in.
	asked := map[string]string{}
	done := map[string]bool{answered: true}
	var after int64
	for {
		events, err := s.Store.Conversations().ListEvents(ctx, job.OrgID, job.SessionID, after, FollowBatch)
		if err != nil {
			return 0, fmt.Errorf("supervisor: reading the asks of job %s: %w", job.ID, err)
		}
		for _, e := range events {
			after = e.Pos
			switch {
			case e.Source == store.SourceEphemeral && e.Name == eventPermissionAsked:
				if id := PermissionID(e.Raw); id != "" {
					asked[id] = e.ConversationID
				}
			case e.Source == store.SourceOpenCode && TurnEnded(e.Name):
				for id, conv := range asked {
					if conv == e.ConversationID {
						delete(asked, id)
					}
				}
			case e.Source == store.SourceEphemeral && e.Name == eventPermissionReplied:
				done[findString(rawFrame(e.Raw), "requestID", 0)] = true
			case e.Source == store.SourceSpov && e.Name == MarkerAnswered:
				done[AnsweredRequest(e.Raw)] = true
			}
		}
		if len(events) < FollowBatch {
			break
		}
	}
	open := 0
	for id := range asked {
		if !done[id] {
			open++
		}
	}
	return open, nil
}
