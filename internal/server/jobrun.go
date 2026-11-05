package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// The Service's half of the run loop: how a job's sandbox is launched and
// how its opening goal reaches it. The supervisor holds these as function
// fields, which `spov serve` wires to this file.
//
// @decision(DL-045)

// ProvisionJob launches a job's session synchronously, unlike
// Service.Create's fire-and-forget launch: the caller must know the sandbox
// came up before it issues the opening turn. The sandbox is tagged with the
// job's id, so a redelivered attempt adopts it rather than provisioning a
// second one.
//
// @decision(DL-045) @decision(DL-046)
func (s *Service) ProvisionJob(ctx context.Context, job store.Job) error {
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return fmt.Errorf("reading the job's session: %w", err)
	}
	// A session that already has a sandbox was launched by an earlier
	// attempt. Adoption is by tag rather than by this row, but the row
	// is the cheaper question and answering it first avoids a provider
	// call on every redelivery.
	if sess.State == store.StateRunning && sess.SandboxID != "" {
		return nil
	}

	log := newLaunchLog(s.deps.Now)
	s.mu.Lock()
	// Pruned as Service.Create does, or a long-running supervisor would
	// hold every launch log it ever made.
	s.pruneLogs(s.deps.Now())
	s.logs[sess.ID] = log
	s.mu.Unlock()

	// The launch runs on the caller's context: unlike Service.Create,
	// the supervisor is waiting for it and owns the timeout.
	s.launchJob(ctx, sess, job, log)

	// The launch records its own outcome on the row, which is the
	// truth the caller reads next. It is re-read rather than returned
	// so that a launch which committed on another replica, or which
	// failed after writing why, reports the same thing either way.
	after, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return fmt.Errorf("re-reading the job's session: %w", err)
	}
	switch after.State {
	case store.StateRunning:
		return nil
	case store.StateFailed, store.StateStopped:
		if after.Error != "" {
			return errors.New(after.Error)
		}
		return fmt.Errorf("the session %s without starting", after.State)
	default:
		return fmt.Errorf("the session is still %s", after.State)
	}
}

// launchJob is Service.launch with the job's options attached.
//
// @decision(DL-045) @decision(DL-046)
func (s *Service) launchJob(ctx context.Context, sess store.Session, job store.Job, log *launchLog) {
	s.launch(ctx, sess, log, telemetry.KindJob, func(opts *session.Options) {
		opts.Job = &session.JobOptions{
			ID:   job.ID,
			Kind: string(job.Kind),
			Goal: job.Goal,
			// Gate 1's command, read from the row because the launch
			// may run on another replica from the create.
			//
			// @decision(DL-050)
			CheckCommand: job.CheckCommand,
		}
	})
}

// OpenJobGoal issues a job's opening turn into its running sandbox. It is
// a lifecycle write made once per job, not a read path.
//
// @decision(DL-045) @decision(DL-046) @decision(DL-018)
func (s *Service) OpenJobGoal(ctx context.Context, job store.Job) error {
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return fmt.Errorf("reading the job's session: %w", err)
	}
	// The connection is minted for the job's owner, whom the sandbox was
	// launched for.
	p := auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	// @decision(DL-030)
	return s.withConnection(ctx, p, sess, func(conn Connection) error {
		conv, err := s.jobConversation(ctx, conn, sess)
		if err != nil {
			return err
		}
		return openJob(ctx, conn, conv, job)
	})
}

// jobConversation is the OpenCode conversation a job's goal is opened in,
// created if the store records none. Nothing else creates one in a fresh
// sandbox, so waiting for one would leave the job at `provisioning` until
// the sandbox idled out. The store is asked first because it answers on
// every pass after the first.
//
// @decision(DL-045) @decision(DL-017)
func (s *Service) jobConversation(ctx context.Context, conn Connection, sess store.Session) (string, error) {
	convs, err := s.deps.Store.Conversations().ListConversations(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return "", fmt.Errorf("reading the job's conversations: %w", err)
	}
	if len(convs) == 0 {
		// None recorded. Create one rather than wait: see above.
		id, err := createConversation(ctx, conn, sessionDir(sess))
		if err != nil {
			return "", fmt.Errorf("opening a conversation for the job: %w", err)
		}
		// Pinned on the conversation as well as in the config, because a
		// repo's own opencode.json discovered from the conversation's
		// location outranks the global file. From /workspace no repo
		// config is on the upward walk, so this matters only with a
		// landing, but it is sent regardless.
		//
		// @decision(DL-014)
		if err := setConversationModel(ctx, conn, id, sess.Request.Model); err != nil {
			s.deps.Warn(fmt.Sprintf("job %s: setting the model: %s", sess.ID, redact.Error(err)))
		}
		// Recorded immediately, so a retry of this pass finds it in the
		// store rather than creating a second one.
		if _, err := s.deps.Store.Conversations().UpsertConversation(ctx, store.Conversation{
			ID: store.NewID(s.deps.Now()), OrgID: sess.OrgID, SessionID: sess.ID, OpenCodeID: id,
		}); err != nil {
			// The conversation exists in the sandbox either way, and
			// the recorder will pick it up. Not fatal to the job.
			s.deps.Warn(fmt.Sprintf("job %s: recording the conversation: %s", sess.ID, redact.Error(err)))
		}
		return id, nil
	}
	// The oldest, which the job opened with; a later one is the agent's
	// own (DL-045). The store already lists oldest first (DL-025); the
	// scan stays so a store that regressed to id order cannot silently
	// reroute the goal.
	oldest := convs[0]
	for _, c := range convs[1:] {
		if c.CreatedAt.Before(oldest.CreatedAt) {
			oldest = c
		}
	}
	return oldest.OpenCodeID, nil
}

// SetJobLoopPaused holds or releases the plugin's continuation loop with
// `/spov-goal pause|resume`, so the plugin's loop.paused follows the job
// row's `steered` state. The command goes to the conversation the goal was
// posted into. It is a lifecycle write, once per act, and resets Modal's
// idle clock; nothing may poll it.
//
// @decision(DL-054) @decision(DL-039) @decision(DL-018)
func (s *Service) SetJobLoopPaused(ctx context.Context, p auth.Principal, job store.Job, paused bool) error {
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return fmt.Errorf("reading the job's session: %w", err)
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		// Nothing to hold or release. A resume of such a job is the
		// caller's error and is refused before reaching here; a pause
		// of one is the supervisor's best-effort write, and a sandbox
		// that has already gone is the case it tolerates.
		return fmt.Errorf("%w (state %s)", ErrSessionNotRunning, sess.State)
	}
	// The caller's principal, so the mint is audited against whoever
	// asked. The supervisor's own pause has none and falls back to the
	// session's owner, as other job-lifecycle writes do.
	//
	// @decision(DL-054) @decision(DL-030)
	if p.UserID == "" {
		p = auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	}
	// @decision(DL-030)
	return s.withConnection(ctx, p, sess, func(conn Connection) error {
		conv, err := s.jobConversation(ctx, conn, sess)
		if err != nil {
			return err
		}
		arg := LoopResume
		if paused {
			arg = LoopPause
		}
		return sendLoopCommand(ctx, conn, conv, arg)
	})
}

// EndJobRun stops a job's loop for good and interrupts its turn, for the
// supervisor at the job's wall clock. Unlike a human's Stop it sends
// `resume=false`, so nothing queued starts afterwards, and it writes no
// marker: the supervisor has already moved the job to delivering. Both
// calls are safe to repeat, which withConnection may do.
//
// @decision(DL-049) @decision(DL-053)
func (s *Service) EndJobRun(ctx context.Context, job store.Job) error {
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return fmt.Errorf("reading the job's session: %w", err)
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return fmt.Errorf("%w (state %s)", ErrSessionNotRunning, sess.State)
	}
	// The supervisor has no principal; the mint is audited against the
	// owner, as the supervisor's pause is.
	p := auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	return s.withConnection(ctx, p, sess, func(conn Connection) error {
		conv, err := s.jobConversation(ctx, conn, sess)
		if err != nil {
			return err
		}
		if err := sendLoopCommand(ctx, conn, conv, LoopBound); err != nil {
			return fmt.Errorf("stopping the loop: %w", err)
		}
		if _, err := sandboxPostJSON(ctx, conn, interruptURL(conn, conv, false), nil); err != nil {
			return fmt.Errorf("interrupting the turn: %w", err)
		}
		return nil
	})
}

// OpenJobOut opens a report job's out directory in its sandbox, for the
// collection at delivery. jobs.ErrNoFile when the sandbox has gone.
//
// @decision(DL-045) @decision(DL-051)
func (s *Service) OpenJobOut(ctx context.Context, job store.Job) (jobs.OutDir, error) {
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return nil, fmt.Errorf("reading the job's session: %w", err)
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return nil, jobs.ErrSandboxGone
	}
	return sandboxOut{s: s, p: auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}, sess: sess}, nil
}

// sandboxOut reads a job's out directory, each call through the cached
// connection, which re-runs the call after a 401 or 403 (DL-030). Both
// calls are reads, so a re-run is safe.
//
// @decision(DL-051) @decision(DL-030)
type sandboxOut struct {
	s    *Service
	p    auth.Principal
	sess store.Session
}

func (o sandboxOut) List(ctx context.Context, dir string) ([]jobs.OutEntry, error) {
	var out []jobs.OutEntry
	err := o.s.withConnection(ctx, o.p, o.sess, func(conn Connection) error {
		var lerr error
		out, lerr = listOutDir(ctx, conn, dir)
		return lerr
	})
	return out, err
}

func (o sandboxOut) Read(ctx context.Context, path string, max int64) ([]byte, error) {
	var out []byte
	err := o.s.withConnection(ctx, o.p, o.sess, func(conn Connection) error {
		var rerr error
		out, rerr = readOutFile(ctx, conn, path, max)
		return rerr
	})
	// A truncated read returns the bytes it did read with the error.
	return out, err
}

// CollectJobEvidence reads the plugin's own record of the run, whose last
// lines say why its loop stopped. It is read once, at delivery, in the same
// visit as the reports. Do not read it on a timer to detect the end of a
// run; see DL-045. Nothing here parses it; the verdict does.
//
// @decision(DL-045) @decision(DL-018) @decision(DL-050)
func (s *Service) CollectJobEvidence(ctx context.Context, job store.Job) ([]byte, error) {
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return nil, fmt.Errorf("reading the job's session: %w", err)
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return nil, jobs.ErrSandboxGone
	}
	p := auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	var out []byte
	// @decision(DL-030)
	err = s.withConnection(ctx, p, sess, func(conn Connection) error {
		// An absolute path, so the out directory's location does not
		// apply.
		var rerr error
		out, rerr = readEvidence(ctx, conn, EvidencePath)
		return rerr
	})
	return out, err
}

// CollectJobOutcome reads the plugin's outcome file, the copy of the lines
// that decide a job's outcome, in the same delivery visit as the evidence.
//
// @decision(DL-051) @decision(DL-045) @decision(DL-018)
func (s *Service) CollectJobOutcome(ctx context.Context, job store.Job) ([]byte, error) {
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return nil, fmt.Errorf("reading the job's session: %w", err)
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return nil, jobs.ErrSandboxGone
	}
	p := auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	var out []byte
	// @decision(DL-030)
	err = s.withConnection(ctx, p, sess, func(conn Connection) error {
		var rerr error
		out, rerr = readEvidence(ctx, conn, OutcomePath)
		return rerr
	})
	return out, err
}

// MarkJobEvent writes a spov-sourced marker onto a job's session event
// stream. store.Event.Validate refuses a `spov.` name from any other
// source, so a sandbox cannot forge one.
//
// @decision(DL-045) @decision(DL-018)
func (s *Service) MarkJobEvent(ctx context.Context, job store.Job, name, text string) error {
	now := s.deps.Now()
	_, err := s.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: job.OrgID, SessionID: job.SessionID,
		Source: store.SourceSpov, Name: name, Text: text, At: now,
	}})
	return err
}

// AdoptJobSandbox records a sandbox found by tag onto the job's session.
// Without it the reconciler would adopt the sandbox as an interactive
// session, and one sandbox would count against both quotas.
//
// @decision(DL-045) @decision(DL-046) @decision(DL-004)
func (s *Service) AdoptJobSandbox(ctx context.Context, job store.Job, sandboxID string) error {
	// A failed look leaves the adoption as it was; the sidecar pass
	// catches a missing proxy a minute later.
	//
	// @decision(DL-034)
	if s.deps.Provider != nil {
		ended, err := s.endAdoptedWithoutProxy(ctx, job, sandboxID)
		if ended {
			return err
		}
		if err != nil {
			s.deps.Warn(fmt.Sprintf("job %s: adopt: checking the proxy sidecar: %s", job.ID, redact.Error(err)))
		}
	}
	creds, renewable := s.adoptedCredentials(ctx, job)
	adopted := false
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		sess, err := tx.Sessions().Get(ctx, job.OrgID, job.SessionID)
		if err != nil {
			return err
		}
		if sess.SandboxID == sandboxID && sess.State == store.StateRunning {
			return nil
		}
		if !sess.State.Live() {
			// Someone stopped it while this pass ran. Recording a
			// sandbox onto a stopped session would resurrect a row the
			// delete path has already concluded.
			return nil
		}
		sess.SandboxID = sandboxID
		sess.State = store.StateRunning
		sess.Credentials = creds
		adopted = true
		return tx.Sessions().Update(ctx, sess)
	})
	// The launch that minted the sandbox's token is gone, and with it
	// the token's expiry, so a renewable one is marked due now: the job is
	// unfinished, so the next renewal pass re-mints it.
	//
	// @decision(DL-021)
	if err == nil && adopted && renewable {
		now := s.deps.Now()
		if err := s.deps.Store.Sessions().SetGitToken(ctx, job.OrgID, job.SessionID, now, now); err != nil {
			s.deps.Warn(fmt.Sprintf("job %s: adopt: record the git token as due: %s", job.ID, redact.Error(err)))
		}
	}
	return err
}

// NoProxyOnAdopt is why an adopted sandbox without its proxy sidecar was
// ended.
const NoProxyOnAdopt = "the sandbox an earlier attempt launched has no credential proxy sidecar, so it was ended"

// endAdoptedWithoutProxy ends a sandbox launched with the proxy whose
// sidecar is not running, because the launch that made it stopped before
// starting one, and fails the job's session as a failed launch. It
// reports whether it did.
//
// @decision(DL-034)
func (s *Service) endAdoptedWithoutProxy(ctx context.Context, job store.Job, sandboxID string) (bool, error) {
	h, err := s.deps.Provider.Get(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	info, err := h.Info(ctx)
	if err != nil {
		return false, err
	}
	if info.Tags[sandbox.TagProxy] != "1" {
		return false, nil
	}
	running, err := h.SidecarRunning(ctx, credproxy.SidecarName)
	if err != nil || running {
		return false, err
	}
	// Not adopted whatever happens next: the sidecar pass would recreate
	// what a launch never finished.
	if err := h.Terminate(ctx); err != nil {
		return true, err
	}
	now := s.deps.Now()
	return true, s.deps.Store.Tx(ctx, func(tx store.Store) error {
		sess, err := tx.Sessions().Get(ctx, job.OrgID, job.SessionID)
		if err != nil || !sess.State.Live() {
			return err
		}
		sess.State = store.StateFailed
		sess.Error = NoProxyOnAdopt
		sess.StoppedAt = &now
		return tx.Sessions().Update(ctx, sess)
	})
}

// adoptedCredentials works out what an adopted sandbox was given, from
// the owner's forge as it is now. A forge that cannot answer leaves the
// credentials unrecorded rather than failing the adoption.
//
// @decision(DL-043) @decision(DL-021)
func (s *Service) adoptedCredentials(ctx context.Context, job store.Job) ([]store.SessionCredential, bool) {
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		return nil, false
	}
	// The tiers the launch recorded, never personal-first afresh.
	//
	// @decision(DL-042)
	forge, tiers, err := s.forgeFor(ctx, sess.OrgID, sess.OwnerID, policyOf(sess))
	if err != nil || forge == nil {
		return nil, false
	}
	// As the launch that made the sandbox granted them.
	//
	// @decision(DL-043)
	selections, _, err := s.grantSelections(ctx, forge, tiers, sess.Request)
	if err != nil {
		return nil, false
	}
	var (
		creds     []session.Credential
		renewable bool
	)
	for _, sel := range selections {
		if sel.Platform == nil || (len(sel.Repos) == 0 && !sel.All) {
			continue
		}
		r := git.Renewable(sel.Platform)
		renewable = renewable || r
		creds = append(creds, session.Credential{Platform: sel.Platform.Name(), Renewable: r, Forge: sel.Platform.Kind(), All: sel.All})
	}
	return withTiers(storedCredentials(creds, false), tiers), renewable
}

// TerminateJobSandbox stops a finished job's sandbox through Delete, so it
// ends as an interactive session's does and any replica can do it. The
// principal is the job's owner, never an admin, so the server has no path
// that terminates anything by id. An already-stopped session is not an
// error: it is how a job reaches delivery through the abandoned backstop.
//
// @decision(DL-050) @decision(DL-003)
func (s *Service) TerminateJobSandbox(ctx context.Context, job store.Job) error {
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if !sess.State.Live() {
		return nil
	}
	p := auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	return s.Delete(ctx, p, sess.ID)
}

// JobOwnerForge resolves the forge gate 2's pull-request query runs under:
// the one the job's owner launched with, through the launch's own forgeFor
// and the tiers it recorded (DL-042).
// A deployment with no forge resolves to nil rather than an error, which
// gate 2 reads as "cannot answer"; an error would warn on every job. A nil
// Forge converts to a nil jobs.Forge.
//
// @decision(DL-050) @decision(DL-042)
func (s *Service) JobOwnerForge(ctx context.Context, sess store.Session) (jobs.Forge, error) {
	forge, _, err := s.forgeFor(ctx, sess.OrgID, sess.OwnerID, policyOf(sess))
	if err != nil {
		return nil, err
	}
	return forge, nil
}

// CatchUpJobSession asks the recorder to read the job's session once more,
// so the verdict covers the final turn, which nothing else re-reads. It is
// part of delivery's one lifecycle visit, not a read path. A session that
// is no longer live is skipped.
//
// @decision(DL-050) @decision(DL-017) @decision(DL-018)
func (s *Service) CatchUpJobSession(ctx context.Context, job store.Job) error {
	if s.OnStop == nil {
		return nil
	}
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return nil
	}
	// The job's owner, never an admin, as in TerminateJobSandbox.
	s.OnStop(ctx, auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}, sess)
	return nil
}
