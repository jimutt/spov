package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// The run loop carries a job from `queued` through `running` to
// `delivering`, one step per River attempt. Each attempt re-reads the row
// and does only what it says is outstanding, so a redelivered job resumes
// rather than restarts.
//
// @decision(DL-045) @decision(DL-046)

// FollowInterval is the snooze between passes over a job's events. It
// paces store reads, not sandbox reads.
//
// @decision(DL-045) @decision(DL-018)
const FollowInterval = 10 * time.Second

// ProvisionTimeout bounds a job's launch: the server's ordinary launch
// budget.
//
// @decision(DL-045)
const ProvisionTimeout = config.DefaultLaunchTimeout

// Provision launches a job's session and returns once the sandbox is
// running. It is a function seam so tests can drive the run loop without
// a launcher, a forge or a provider.
//
// @decision(DL-045)
type Provision func(ctx context.Context, job store.Job) error

// Open issues a job's opening goal into its running sandbox.
//
// @decision(DL-045)
type Open func(ctx context.Context, job store.Job) error

// Collect reads a file out of a job's sandbox at job end: the plugin's
// evidence.
//
// @decision(DL-045)
type Collect func(ctx context.Context, job store.Job) ([]byte, error)

// Adopt records a sandbox an earlier attempt created onto the job's
// session row.
//
// @decision(DL-045)
type Adopt func(ctx context.Context, job store.Job, sandboxID string) error

// Run supervises one job, from wherever its row says it currently is.
// The unused int is River's attempt count: snoozes decrement it, so the
// row, not the count, says how far the job has got.
//
// @decision(DL-046) @decision(DL-045)
func (s *Supervisor) Run(ctx context.Context, args JobArgs, _ int) error {
	if s.onRun != nil {
		s.onRun(args)
	}
	job, err := s.Store.Jobs().Get(ctx, args.OrgID, args.JobID)
	if err != nil {
		// A job that is gone is not a failure to retry.
		if errors.Is(err, store.ErrNotFound) {
			s.warn("supervisor: job %s is gone; nothing to supervise", args.JobID)
			return nil
		}
		return fmt.Errorf("supervisor: reading job %s: %w", args.JobID, err)
	}

	// Retries and rescues can deliver a job decided while the attempt was
	// in flight; leave it alone.
	if isTerminalJob(job.State) {
		return nil
	}

	done, err := s.step(ctx, job)
	// @decision(DL-011)
	s.Metrics.SupervisorPass(ctx, err)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	// One step per attempt, then snooze to release the worker slot. Do
	// not loop here until the job finishes; see DL-045.
	return s.snooze(s.followInterval())
}

// snooze asks River to redeliver this job after d. It is a function
// field so a test can drive Run without River's sentinel error.
//
// @decision(DL-045)
func (s *Supervisor) snooze(d time.Duration) error {
	if s.Snooze == nil {
		// No queue to be redelivered by: the caller is a test, or a
		// supervisor driven directly. One pass is all there is.
		return nil
	}
	return s.Snooze(d)
}

// step advances a job by whatever the row just read says is outstanding,
// and reports whether supervision is finished.
//
// @decision(DL-045)
func (s *Supervisor) step(ctx context.Context, job store.Job) (done bool, err error) {
	switch job.State {
	case store.JobQueued:
		return false, s.provision(ctx, job)
	case store.JobProvisioning:
		return false, s.open(ctx, job)
	case store.JobRunning, store.JobBlocked, store.JobSteered:
		// A parked job is still followed: the events around its release
		// arrive on the same feed.
		return false, s.followOnce(ctx, job)
	case store.JobDelivering:
		// Collect, judge from stored rows (DL-050), and conclude.
		return true, s.deliver(ctx, job)
	}
	return true, nil
}

// followInterval is the wait between passes, overridable by a test.
func (s *Supervisor) followInterval() time.Duration {
	if s.FollowInterval > 0 {
		return s.FollowInterval
	}
	return FollowInterval
}

// provision launches the job's sandbox and moves it to running.
//
// The row moves to `provisioning` before the launch, so a crash between
// the two leaves a row the tag query settles rather than a `queued` row a
// retry would provision again.
//
// @decision(DL-045) @decision(DL-046)
func (s *Supervisor) provision(ctx context.Context, job store.Job) error {
	if s.Provision == nil {
		return fmt.Errorf("supervisor: job %s: no way to provision a sandbox", job.ID)
	}
	// A refused move means the job was decided (usually cancelled) under
	// this pass; do not launch.
	took, err := s.moved(ctx, job, store.JobProvisioning, "")
	if err != nil {
		return err
	}
	if !took {
		return nil
	}

	// The preparation pass runs after the move, so a cancelled job is
	// never paid for, and before the launch, because the opening turn
	// carries its assessment. It cannot fail the job.
	//
	// @decision(DL-047)
	job = s.prepare(ctx, job)
	// Bounded on its own: the attempt's context lasts the sandbox's
	// whole lifetime.
	lctx, cancel := context.WithTimeout(ctx, ProvisionTimeout)
	defer cancel()
	// Adopt a sandbox already tagged with this job rather than launch a
	// second one. A listing failure aborts rather than launching.
	//
	// @decision(DL-046)
	id, adopted, err := s.adoptOrCreate(lctx, job.ID, func(ctx context.Context) (string, error) {
		return "", s.Provision(ctx, job)
	})
	if adopted {
		// An earlier attempt died before recording its sandbox. Record it
		// here: the reconciler skips a counting job's sandbox (DL-004), so
		// the session would otherwise stay `starting` until the orphan
		// sweep failed it.
		s.warn("supervisor: job %s already has a running sandbox; adopting it rather than launching another", job.ID)
		if s.Adopt == nil {
			return fmt.Errorf("supervisor: job %s: no way to record the adopted sandbox %s", job.ID, id)
		}
		return s.Adopt(ctx, job, id)
	}
	if err != nil {
		// A failed launch is the job's failure. Do not return it: a River
		// retry would provision again; see DL-045.
		s.fail(ctx, job, fmt.Sprintf("provisioning the sandbox: %s", redact.Error(err)))
		return nil
	}
	return nil
}

// open issues the job's first turn and moves it to running. It waits on
// the session's state; a session still starting means the launch is in
// flight here or on another replica, and the next pass looks again.
//
// @decision(DL-045) @decision(DL-046)
func (s *Supervisor) open(ctx context.Context, job store.Job) error {
	sess, err := s.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(ctx, job, "the job's session is gone")
			return nil
		}
		return fmt.Errorf("supervisor: reading session for job %s: %w", job.ID, err)
	}
	switch sess.State {
	case store.StateRunning:
	case store.StateFailed, store.StateStopped:
		// The launch concluded without a sandbox; carry the session's
		// error, which names the failed step.
		why := sess.Error
		if why == "" {
			why = "the session " + string(sess.State) + " before the job could start"
		}
		s.fail(ctx, job, why)
		return nil
	default:
		// Still starting; look again next pass. There is deliberately no
		// timeout here: the reconciler fails the session past
		// LaunchTimeout and the branch above then fails the job.
		//
		// @decision(DL-045) @decision(DL-004)
		return nil
	}

	// Nudge the recorder before the goal goes out. Wake does not block,
	// and a nil Wake leaves the recorder's poll to find the session.
	//
	// @decision(DL-017)
	if s.Wake != nil {
		s.Wake(sess)
	}

	if s.Open == nil {
		return fmt.Errorf("supervisor: job %s: no way to open the goal", job.ID)
	}
	if err := s.Open(ctx, job); err != nil {
		// Returned for a River retry, unlike a failed launch: the job
		// stays `provisioning` and the retry reissues the turn. River's
		// attempt budget bounds it.
		return fmt.Errorf("supervisor: job %s: opening the goal: %w", job.ID, err)
	}
	return s.move(ctx, job, store.JobRunning, "")
}

// followOnce is one pass over the job's new events.
//
// @decision(DL-046)
func (s *Supervisor) followOnce(ctx context.Context, job store.Job) error {
	res, err := s.follow(ctx, job, job.Cursor)
	if err != nil {
		return s.boundDespite(ctx, job, err)
	}
	if res.SawGap {
		// Not an error: the events after the gap still hold.
		s.warn("supervisor: job %s has a recording gap; what happened during it is unknown", job.ID)
	}
	// A running job whose pass saw no stop event looks for the stop
	// marker in the stored transcript, in case the event never reached
	// the log. The `running` guard is what makes this safe; do not make it
	// conditional on a gap instead (DL-048). One store query per pass.
	//
	// @decision(DL-048) @decision(DL-045) @decision(DL-017)
	if !res.Stopped && res.State == store.JobRunning {
		stopped, err := s.loopStoppedInMessages(ctx, job)
		if err != nil {
			s.warn("supervisor: job %s: looking for the stop marker in the transcript: %s",
				job.ID, redact.Error(err))
		} else if stopped {
			s.warn("supervisor: job %s: the stop marker never reached the event log and was "+
				"recovered from the transcript", job.ID)
			res.Stopped = true
			res.State = store.JobDelivering
			res.Moved = true
		}
	}
	// applyFollow writes everything the pass concluded, including the
	// move to delivering when the plugin announced its stop. That move is
	// never inferred from silence; see DL-048.
	applied, err := s.applyFollow(ctx, job, res)
	if err != nil {
		return s.boundDespite(ctx, job, err)
	}
	// Writing `steered` also pauses the plugin's loop. Key on the state
	// applyFollow actually wrote, not on what the pass concluded.
	//
	// @decision(DL-054)
	if applied == store.JobSteered && job.State != store.JobSteered {
		s.pauseLoop(ctx, job)
	}
	// A job that ran out its wall clock is ended here, after the pass,
	// so a stop the pass just read wins over the bound.
	//
	// @decision(DL-049)
	if !res.Stopped && (applied == store.JobRunning || applied == store.JobBlocked) {
		ended, err := s.endAtWallClock(ctx, job)
		if err != nil || ended {
			return err
		}
	}
	// A person's answer releasing a blocked job releases the plugin's
	// loop too, as Resume does: it is held if someone typed into the job
	// while it was parked. After the bound, so a job ended there is not
	// first nudged on.
	//
	// @decision(DL-050) @decision(DL-054)
	if applied == store.JobRunning && job.State == store.JobBlocked {
		s.releaseLoop(ctx, job)
	}
	// The backstop for a stop announcement that never landed. It routes
	// on the row as the pass left it, read inside movedTo's transaction.
	//
	// @decision(DL-045)
	if !res.Stopped {
		return s.checkAbandoned(ctx, job)
	}
	return nil
}

// boundDespite returns a pass's error unless the job has run out its wall
// clock, which is then applied: a pass that keeps failing must not leave
// a job unbounded until its sandbox times out.
//
// @decision(DL-049)
func (s *Supervisor) boundDespite(ctx context.Context, job store.Job, passErr error) error {
	ended, err := s.endAtWallClock(ctx, job)
	if err != nil || !ended {
		return passErr
	}
	s.warn("supervisor: job %s: ended at its wall clock after a failed pass: %s", job.ID, redact.Error(passErr))
	return nil
}

// MarkerBounded records that a job reached its wall clock. Its text is
// the note the verdict opens with. Only spov can write a spov-namespaced
// event, so a sandbox cannot forge one.
//
// @decision(DL-049)
const MarkerBounded = store.SpovNamespace + "job.bounded"

// MarkerProxyFailed records that the job's proxy sidecar died a second
// time and its sandbox was ended. Its text is the reason the verdict
// fails the job with.
//
// @decision(DL-034)
const MarkerProxyFailed = store.SpovNamespace + "job.proxy-failed"

// endAtWallClock delivers a running or blocked job that has used its
// MaxDuration across its stretches, and reports whether it did. The row
// is read locked, so a resume that starts a stretch, or a steer that
// banks one, cannot land between the decision and the write.
// The marker is written in the same transaction; deliver reads it to
// stop the run, which it does only once the row says delivering, so the
// interrupt cannot be read as a human steering the job.
//
// @decision(DL-049)
func (s *Supervisor) endAtWallClock(ctx context.Context, job store.Job) (bool, error) {
	var note string
	err := s.Store.Tx(ctx, func(tx store.Store) error {
		current, err := tx.Jobs().GetForUpdate(ctx, job.OrgID, job.ID)
		if err != nil {
			return err
		}
		now := s.now()
		elapsed, over := wallClockOver(current, now)
		if !over || (current.State != store.JobRunning && current.State != store.JobBlocked) {
			return nil
		}
		text := fmt.Sprintf("the job reached its %s wall clock after %s unattended",
			current.MaxDuration, elapsed.Round(time.Second))
		current.State = store.JobDelivering
		current.Error = text
		if err := tx.Jobs().Update(ctx, current); err != nil {
			return err
		}
		if _, err := tx.Conversations().PutEvents(ctx, []store.Event{{
			ID: store.NewID(now), OrgID: current.OrgID, SessionID: current.SessionID,
			Source: store.SourceSpov, Name: MarkerBounded, Text: text, At: now,
		}}); err != nil {
			return err
		}
		note = text
		return nil
	})
	if errors.Is(err, store.ErrInvalidTransition) {
		s.warn("supervisor: job %s was decided while the pass ran; not ending it at its wall clock", job.ID)
		return false, nil
	}
	if err != nil || note == "" {
		return false, err
	}
	s.warn("supervisor: job %s: %s", job.ID, note)
	return true, nil
}

// stopBoundedRun stops the loop and the turn of a job delivered at its
// wall clock, before anything is collected. Best-effort: a sandbox that
// cannot be reached is collected as it is. A redelivery repeats it,
// which EndRun allows.
//
// @decision(DL-049)
func (s *Supervisor) stopBoundedRun(ctx context.Context, job store.Job) {
	if s.EndRun == nil {
		return
	}
	events, err := s.jobEvents(ctx, job)
	if err != nil {
		s.warn("supervisor: job %s: looking for the wall clock marker: %s", job.ID, redact.Error(err))
		return
	}
	if boundNote(events) == "" {
		return
	}
	if err := s.EndRun(ctx, job); err != nil {
		s.warn("supervisor: job %s: stopping its run at the wall clock: %s", job.ID, redact.Error(err))
	}
}

// wallClockOver reports how much of its clock the job has used across
// all its stretches and whether that has reached its MaxDuration. A job
// with no clock, or whose clock is stopped, is never over.
//
// @decision(DL-049)
func wallClockOver(job store.Job, now time.Time) (time.Duration, bool) {
	if job.MaxDuration <= 0 || job.ClockStartedAt == nil {
		return 0, false
	}
	elapsed := job.ClockElapsed(now)
	return elapsed, elapsed >= job.MaxDuration
}

// checkAbandoned concludes a job whose session is no longer live: the
// liveness backstop for the completion signal. It keys on the session's
// state, never on silence, and reads only the store. The target is
// abandonedState's: `delivering`, so the verdict judges the run.
//
// @decision(DL-045) @decision(DL-050) @decision(DL-018)
func (s *Supervisor) checkAbandoned(ctx context.Context, job store.Job) error {
	// Suspended jobs are checked too: with no sandbox left there is
	// nothing to release, and they would hold the owner's job quota.
	// This is only a pre-filter on the caller's copy, which may be a
	// pass behind; movedTo decides on the current row.
	switch job.State {
	case store.JobRunning, store.JobBlocked, store.JobSteered:
	default:
		return nil
	}
	sess, err := s.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.warn("supervisor: job %s: its session is gone; concluding the run", job.ID)
			_, err := s.movedTo(ctx, job, abandonedState,
				"the job's session disappeared before its loop reported finishing")
			return err
		}
		return fmt.Errorf("supervisor: reading session for job %s: %w", job.ID, err)
	}
	if sess.State.Live() {
		return nil
	}
	// A normal run announces its end, so reaching here means a timeout
	// mid-run or a lost announcement.
	s.warn("supervisor: job %s: the sandbox %s before the loop announced it had stopped",
		job.ID, sess.State)
	_, err = s.movedTo(ctx, job, abandonedState,
		"the sandbox "+string(sess.State)+" before the job's loop reported finishing")
	return err
}

// abandonedState is where checkAbandoned sends a job whose sandbox is
// gone: delivering, from every live state. A blocked job is judged
// too, and gate 3's veto on the unanswered permission makes the verdict
// a failure.
//
// @decision(DL-045) @decision(DL-049)
func abandonedState(store.JobState) store.JobState {
	return store.JobDelivering
}

// pauseLoop holds the plugin's continuation loop for a job the
// supervisor has just written `steered`, so `steered` always means the
// loop is held. Best-effort: a failed pause warns and leaves the row
// `steered`.
//
// @decision(DL-054) @decision(DL-045)
func (s *Supervisor) pauseLoop(ctx context.Context, job store.Job) {
	if s.Loop == nil {
		return
	}
	if err := s.Loop(ctx, auth.Principal{}, job, true); err != nil {
		s.warn("supervisor: job %s: pausing its loop after steering: %s", job.ID, redact.Error(err))
	}
}

// releaseLoop lets the plugin's loop continue after a blocked job was
// released. Best-effort: most blocked jobs have nothing held, and one
// left held is still ended by its wall clock (DL-049).
//
// @decision(DL-050) @decision(DL-054)
func (s *Supervisor) releaseLoop(ctx context.Context, job store.Job) {
	if s.Loop == nil {
		return
	}
	if err := s.Loop(ctx, auth.Principal{}, job, false); err != nil {
		s.warn("supervisor: job %s: releasing its loop after an answer: %s", job.ID, redact.Error(err))
	}
}

// move writes a job's state, checking the transition against the row as
// re-read inside the transaction. A refused transition means the row was
// decided under the pass (a cancel, say) and is warned about, not
// returned.
//
// @decision(DL-045)
func (s *Supervisor) move(ctx context.Context, job store.Job, to store.JobState, why string) error {
	_, err := s.moved(ctx, job, to, why)
	return err
}

// moved is move, reporting whether the row actually took the state.
// Callers that go on to spend money must check it.
//
// @decision(DL-045)
func (s *Supervisor) moved(ctx context.Context, job store.Job, to store.JobState, why string) (bool, error) {
	return s.movedTo(ctx, job, func(store.JobState) store.JobState { return to }, why)
}

// movedTo is moved for a caller whose target depends on the job's
// current state. pick runs inside the transaction on the row the
// transition is checked against; do not compute the target outside it.
//
// @decision(DL-045)
func (s *Supervisor) movedTo(ctx context.Context, job store.Job, pick func(store.JobState) store.JobState, why string) (bool, error) {
	wrote := false
	attempted := job.State
	// ended is the row as this call wrote it, and from its state before.
	var (
		ended store.Job
		from  store.JobState
	)
	err := s.Store.Tx(ctx, func(tx store.Store) error {
		current, err := tx.Jobs().Get(ctx, job.OrgID, job.ID)
		if err != nil {
			return err
		}
		to := pick(current.State)
		attempted = to
		if current.State == to {
			// Already there: another attempt got here first.
			wrote = true
			return nil
		}
		if isTerminalJob(current.State) {
			return store.ErrInvalidTransition
		}
		if !store.CanTransition(current.State, to) {
			return store.ErrInvalidTransition
		}
		from = current.State
		current.State = to
		if why != "" {
			current.Error = why
		}
		if err := tx.Jobs().Update(ctx, current); err != nil {
			return err
		}
		wrote, ended = true, current
		return nil
	})
	if errors.Is(err, store.ErrInvalidTransition) {
		s.warn("supervisor: job %s was decided while the pass ran; not writing %s", job.ID, attempted)
		return false, nil
	}
	// Only a move this call wrote is recorded.
	//
	// @decision(DL-011)
	switch {
	case err != nil || ended.ID == "":
	case isTerminalJob(ended.State):
		s.recordEnding(ctx, ended, endedBeforeJudging(from, ended.State), nil, 0)
	case from == store.JobQueued && ended.State == store.JobProvisioning:
		s.Metrics.JobQueued(ctx, s.now().Sub(ended.CreatedAt))
	}
	return wrote, err
}

// fail records why a job could not run. A failed write is warned about,
// not returned, because a River retry would provision again.
//
// @decision(DL-045)
func (s *Supervisor) fail(ctx context.Context, job store.Job, why string) {
	if err := s.move(ctx, job, store.JobFailed, why); err != nil {
		s.warn("supervisor: job %s: recording the failure: %s", job.ID, redact.Error(err))
	}
}

// truncationNote is appended to evidence that hit the cap, so the
// stored text says it is incomplete.
//
// @decision(DL-045)
const truncationNote = "\n\n[spov: the evidence was truncated here; it exceeded the size limit]\n"

// deliver collects what the job produced, then judges it. Collection
// comes first because gates 1 and 2 read what it writes. A missing
// report is the verdict's to classify, not a collection failure.
//
// @decision(DL-045) @decision(DL-050) @decision(DL-051)
func (s *Supervisor) deliver(ctx context.Context, job store.Job) error {
	// @decision(DL-011)
	started := s.now()
	// Collection runs once per job, so a redelivery does not read the
	// sandbox again. The evidence marker, written on every delivery, is
	// the record that it ran.
	//
	// @decision(DL-045) @decision(DL-018)
	done, err := s.alreadyDelivered(ctx, job)
	if err != nil {
		return err
	}
	if !done {
		// @decision(DL-049) @decision(DL-101)
		if job.Kind == store.JobKindSync {
			s.endSyncRun(ctx, job)
		} else {
			s.stopBoundedRun(ctx, job)
		}
		// Every kind leaves evidence; only a report leaves files.
		s.collectEvidence(ctx, job)
	}
	// Gated by its own marker rather than the evidence one, so a pass
	// that broke off mid-collection collects again.
	//
	// @decision(DL-051)
	if err := s.collectOut(ctx, job); err != nil {
		return err
	}
	// @decision(DL-101)
	if err := s.collectSync(ctx, job); err != nil {
		return err
	}
	// Both read the stored evidence and the forge, never the sandbox, so
	// a redelivery runs them again and can improve a member an earlier
	// pass could not check.
	//
	// @decision(DL-051) @decision(DL-050)
	s.recordDeclarations(ctx, job)
	s.checkPullRequests(ctx, job)
	// Re-read so the verdict sees the unfinished work recordDeclarations
	// wrote.
	job, err = s.Store.Jobs().Get(ctx, job.OrgID, job.ID)
	if err != nil {
		return fmt.Errorf("supervisor: job %s: re-reading before the verdict: %w", job.ID, err)
	}
	if isTerminalJob(job.State) {
		return nil
	}
	return s.concludeJob(ctx, job, started)
}

// concludeJob computes the verdict and writes the job's end.
//
// @decision(DL-050)
func (s *Supervisor) concludeJob(ctx context.Context, job store.Job, started time.Time) error {
	// The recorder's final read must come before the gates, so gate 3
	// sees the last turn. Best-effort; part of delivery's one sandbox
	// visit.
	//
	// @decision(DL-050) @decision(DL-017) @decision(DL-018)
	s.catchUpBeforeJudging(ctx, job)

	verdict, err := s.Judge(ctx, job)
	if err != nil {
		return err
	}
	ended, err := s.recordVerdict(ctx, job, verdict)
	if err != nil {
		return err
	}
	// Recorded only when this call wrote the ending.
	//
	// @decision(DL-011)
	if ended != nil {
		by := telemetry.EndedOther
		if events, err := s.jobEvents(ctx, job); err == nil {
			by = endedBy(events)
		}
		s.recordEnding(ctx, *ended, by, &verdict, s.now().Sub(started))
	}
	// The only thing that stops a finished job's sandbox. It runs after
	// the verdict is written, so a failure here cannot leave the job
	// `delivering` with its gates unread. A finished sync job's sandbox
	// is held for its state to be taken instead.
	//
	// @decision(DL-050) @decision(DL-101)
	if job.Kind == store.JobKindSync {
		return nil
	}
	s.terminate(ctx, job)
	return nil
}

// terminate stops a finished job's sandbox, best-effort. A failure is
// warned about, not returned: the job is already terminal, and the
// reconciler reaps a sandbox that outlives its session.
//
// @decision(DL-050) @decision(DL-003)
func (s *Supervisor) terminate(ctx context.Context, job store.Job) {
	if s.Terminate == nil {
		return
	}
	if err := s.Terminate(ctx, job); err != nil {
		s.warn("supervisor: job %s: stopping the sandbox: %s", job.ID, redact.Error(err))
	}
}

// collectEvidence stores the plugin's evidence file, whose last lines
// say why its loop stopped, as a spov marker on the session's event
// stream. Best-effort: missing evidence never fails a job.
//
// @decision(DL-045) @decision(DL-050) @decision(DL-018)
func (s *Supervisor) collectEvidence(ctx context.Context, job store.Job) {
	// Both are needed; a nil Mark would panic below.
	if s.Mark == nil {
		return
	}
	s.collectOutcome(ctx, job)
	if s.Evidence == nil {
		return
	}
	body, err := s.Evidence(ctx, job)
	if err != nil && !errors.Is(err, ErrFileTruncated) {
		// No file is ordinary on the backstop path, where the sandbox
		// is already gone; do not warn.
		if !errors.Is(err, ErrNoFile) && !errors.Is(err, ErrSandboxGone) {
			s.warn("supervisor: job %s: collecting evidence: %s", job.ID, redact.Error(err))
		}
		return
	}
	if len(body) == 0 {
		return
	}
	if errors.Is(err, ErrFileTruncated) {
		// The read keeps the head, so truncation loses the final line
		// saying why the loop stopped. Mark it so its absence is not
		// read as the loop never announcing.
		s.warn("supervisor: job %s: the evidence was truncated at %d bytes; the end of it was not read",
			job.ID, len(body))
		body = append(body, []byte(truncationNote)...)
	}
	if err := s.Mark(ctx, job, MarkerEvidence, string(body)); err != nil {
		s.warn("supervisor: job %s: recording evidence: %s", job.ID, redact.Error(err))
	}
}

// collectOutcome stores the plugin's outcome file as its own marker. A pr
// job whose sandbox had gone cannot say which pull requests it recorded,
// so that is marked a failed collection, which gate 2 reads as
// unverified rather than as a job that recorded none.
//
// @decision(DL-051) @decision(DL-050)
func (s *Supervisor) collectOutcome(ctx context.Context, job store.Job) {
	if s.Outcome == nil {
		return
	}
	body, err := s.Outcome(ctx, job)
	switch {
	case errors.Is(err, ErrSandboxGone):
		if job.Kind == store.JobKindPR {
			s.markCollectionFailed(ctx, job, errors.New("the sandbox had already stopped, so the pull requests it recorded could not be read"))
		}
		return
	case errors.Is(err, ErrNoFile):
		return
	case err != nil && !errors.Is(err, ErrFileTruncated):
		s.warn("supervisor: job %s: collecting the outcome: %s", job.ID, redact.Error(err))
		if job.Kind == store.JobKindPR {
			s.markCollectionFailed(ctx, job, err)
		}
		return
	}
	if len(body) == 0 {
		return
	}
	if err := s.Mark(ctx, job, MarkerOutcome, string(body)); err != nil {
		s.warn("supervisor: job %s: recording the outcome: %s", job.ID, redact.Error(err))
	}
}

// alreadyDelivered reports whether this job's delivery has already run,
// by looking for the marker delivery writes.
//
// @decision(DL-045)
func (s *Supervisor) alreadyDelivered(ctx context.Context, job store.Job) (bool, error) {
	// Scanned from the job's cursor: by delivery it sits at the end of
	// the run, and the marker is written after it.
	events, err := s.Store.Conversations().ListEvents(ctx, job.OrgID, job.SessionID, job.Cursor, FollowBatch)
	if err != nil {
		return false, fmt.Errorf("supervisor: job %s: checking for a previous delivery: %w", job.ID, err)
	}
	for _, e := range events {
		if e.Source == store.SourceSpov && e.Name == MarkerEvidence {
			return true, nil
		}
	}
	return false, nil
}

// MarkerCollectFailed records that spov could not read what a job left
// in its sandbox, as distinct from the job not producing it.
//
// @decision(DL-050)
const MarkerCollectFailed = store.SpovNamespace + "job.collect.failed"

// MarkerResumed is written when a human resumes a suspended job. The
// supervisor treats it as a boundary: a `user` item at or before it does
// not steer the job again. Do not replace it with a cursor jump; see
// DL-054. Only spov can write a spov-namespaced event, so a sandbox
// cannot forge one.
//
// @decision(DL-054) @decision(DL-045)
const MarkerResumed = store.SpovNamespace + "job.resumed"

// MarkerInterrupted is the server's record of having interrupted a job's
// turn, written after the sandbox answers. The supervisor reads it as a
// steer on its next pass, which pauses the loop. A sandbox cannot forge
// one.
//
// @decision(DL-053) @decision(DL-045)
const MarkerInterrupted = store.SpovNamespace + "job.interrupted"

// MarkerAnswered is the server's record of a person answering one of
// the job's permission requests through spov, written after the sandbox
// accepts the reply. Its Raw is {"requestID": "per_…"} (AnsweredRequest).
// The supervisor reads it as a release of a blocked job once no other
// request is unanswered. A sandbox cannot forge one.
//
// @decision(DL-054)
const MarkerAnswered = store.SpovNamespace + "job.answered"

// AnsweredRequest reads the permission request a MarkerAnswered row
// names, "" when it names none.
//
// @decision(DL-054)
func AnsweredRequest(raw []byte) string {
	var v struct {
		RequestID string `json:"requestID"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.RequestID
}

// markCollectionFailed writes MarkerCollectFailed, which gate 2 reads.
// Best-effort: without it gate 2 treats the deliverables as missing.
//
// @decision(DL-050)
func (s *Supervisor) markCollectionFailed(ctx context.Context, job store.Job, cause error) {
	if s.Mark == nil {
		return
	}
	if err := s.Mark(ctx, job, MarkerCollectFailed, redact.Error(cause)); err != nil {
		s.warn("supervisor: job %s: recording the failed collection: %s", job.ID, redact.Error(err))
	}
}

// catchUpBeforeJudging asks the recorder for one last read, so the
// verdict sees the run's final turn. Best-effort: the sandbox may be
// gone, and the job is then judged on what was stored.
//
// @decision(DL-050) @decision(DL-017)
func (s *Supervisor) catchUpBeforeJudging(ctx context.Context, job store.Job) {
	if s.CatchUp == nil {
		return
	}
	if err := s.CatchUp(ctx, job); err != nil {
		// Warned, not returned: judging stale rows beats staying at delivering.
		s.warn("supervisor: job %s: the final read before judging failed, so the verdict may miss the last turn: %s",
			job.ID, redact.Error(err))
	}
}

// SyntheticRole is the role the recorder stores for an inbox item spov
// enqueued: the message-side counterpart of the event's item type. A
// human's message never has it.
//
// @decision(DL-045)
const SyntheticRole = "synthetic"

// loopStoppedInMessages reports whether the plugin's stop marker is in
// the stored transcript: the recovery half of isLoopStopped. As on the
// event, only a synthetic whose text starts with the marker counts; the
// role stands in for the event name, which a message does not carry.
//
// @decision(DL-048) @decision(DL-045) @decision(DL-039)
func (s *Supervisor) loopStoppedInMessages(ctx context.Context, job store.Job) (bool, error) {
	msgs, err := s.jobMessages(ctx, job)
	if err != nil {
		return false, err
	}
	return StoppedIn(msgs), nil
}

// StoppedIn is the match itself, over messages already read. The resume
// path calls it too, so both decide "has the loop stopped" the same way;
// do not copy it.
//
// @decision(DL-054) @decision(DL-045)
func StoppedIn(msgs []store.Message) bool {
	for _, m := range msgs {
		if m.Role != SyntheticRole {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(m.Text), opencode.LoopStoppedMarker) {
			return true
		}
	}
	return false
}
