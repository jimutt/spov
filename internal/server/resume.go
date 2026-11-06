package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// The two ways a resume brings a session back.
//
// @decision(DL-088) @decision(DL-089)
const (
	ResumeSnapshot   = "snapshot"
	ResumeTranscript = "transcript"
)

// SnapshotResumeWait is how long a resume waits for a snapshot still
// being taken.
//
// @decision(DL-088)
const SnapshotResumeWait = 60 * time.Second

// MarkerResumed is the seam a resume writes into the session's log.
//
// @decision(DL-087)
const MarkerResumed = store.SpovNamespace + "session.resumed"

// The refusals of a resume.
//
// @decision(DL-087)
var (
	// ErrNotResumable is a session a resume cannot reopen: live, a job's,
	// or one that never ran. A 409.
	ErrNotResumable = errors.New("server: the session cannot be resumed")
	// ErrSnapshotUnavailable is a snapshot that could not be collected in
	// time; the session can still resume from its transcript. A 409.
	ErrSnapshotUnavailable = errors.New("server: the session's snapshot could not be collected")
)

// ResumeRequest reopens a stopped session. Only the timeout may change;
// everything else is as launched.
//
// @decision(DL-087)
type ResumeRequest struct {
	// TimeoutSecs is the new sandbox's lifetime; 0 takes the launch's.
	TimeoutSecs int `json:"timeout_secs,omitempty" minimum:"0" doc:"The resumed sandbox's lifetime in seconds; omitted takes the one the session was launched with"`
	// Message is sent once the session runs, as a steer.
	Message *SteerRequest `json:"message,omitempty" doc:"A message to send once the session runs"`
	// SyncBundle is a synced session's base commit, from the checkout it
	// was launched from, for a transcript resume's fresh clone. Used for
	// this resume only and never stored.
	//
	// @decision(DL-098)
	SyncBundle string `json:"sync_bundle,omitempty" doc:"A base64 git bundle of a synced session's base commit, for a resume from the transcript; ignored otherwise"`
}

// ResumeView says how a stopped session would resume, for its owner.
//
// @decision(DL-088)
type ResumeView struct {
	Mode string `json:"mode" enum:"snapshot,transcript" doc:"snapshot brings the workspace back as it was; transcript clones the repositories fresh and restores the conversation"`
	// Pending is a snapshot still being taken, which a resume waits for.
	Pending bool `json:"pending,omitempty"`
	// Until is the latest the snapshot is kept; the cap may prune it
	// sooner.
	Until *time.Time `json:"until,omitempty"`
}

// resumeChoice is how sess would resume, from its runs, or false when it
// cannot. isJob is whether a job owns it.
//
// @decision(DL-087) @decision(DL-088)
func (s *Service) resumeChoice(sess store.Session, runs []store.Run, isJob bool, now time.Time) (ResumeView, *store.Run, bool) {
	if !s.deps.Persistent || isJob || sess.State.Live() {
		return ResumeView{}, nil, false
	}
	var last *store.Run
	for i := range runs {
		if runs[i].Ran() {
			last = &runs[i]
		}
	}
	if last == nil {
		return ResumeView{}, nil, false
	}
	retention := DefaultSnapshotRetention
	if s.deps.Snapshots != nil {
		retention = s.deps.Snapshots.Retention
	}
	usable := (last.SnapshotState == store.SnapshotKept || last.SnapshotState == store.SnapshotPending) &&
		last.OpenCode == s.deps.Defaults.OpenCodeVersion &&
		(last.EndedAt == nil || now.Sub(*last.EndedAt) < retention)
	if !usable {
		return ResumeView{Mode: ResumeTranscript}, nil, true
	}
	view := ResumeView{Mode: ResumeSnapshot, Pending: last.SnapshotState == store.SnapshotPending}
	if last.EndedAt != nil {
		until := last.EndedAt.Add(retention)
		view.Until = &until
	}
	return view, last, true
}

// Resume reopens a stopped or failed interactive session in a new
// sandbox, from its newest snapshot when it can and from its transcript
// otherwise, and starts the launch in the background.
//
// @decision(DL-087) @decision(DL-088)
func (s *Service) Resume(ctx context.Context, p auth.Principal, ref string, req ResumeRequest) (store.Session, error) {
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return store.Session{}, err
	}
	// The session would run on its owner's credentials.
	if p.UserID != sess.OwnerID {
		return store.Session{}, huma.Error403Forbidden("only the session's owner can resume it")
	}
	if !s.deps.Persistent {
		return store.Session{}, huma.Error501NotImplemented("this server keeps its sessions in memory, so it cannot resume one")
	}
	if sess.State.Live() {
		return store.Session{}, fmt.Errorf("%w: it is %s", ErrNotResumable, sess.State)
	}
	isJob := false
	if _, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID); err == nil {
		isJob = true
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Session{}, err
	}
	if isJob {
		return store.Session{}, fmt.Errorf("%w: it is a job's session; a job is not resumed", ErrNotResumable)
	}
	all, err := s.deps.Store.Runs().List(ctx, sess.OrgID, []string{sess.ID})
	if err != nil {
		return store.Session{}, err
	}
	runs := all[sess.ID]
	choice, from, ok := s.resumeChoice(sess, runs, isJob, s.deps.Now())
	if !ok {
		return store.Session{}, fmt.Errorf("%w: it never ran; launch a new session instead", ErrNotResumable)
	}
	quota := s.quotaOf(s.profileFor(ctx, sess.OrgID, sess.OwnerID))
	timeout, err := resumeTimeout(sess.Request.Sandbox.Timeout, req.TimeoutSecs, quota.MaxTimeout)
	if err != nil {
		return store.Session{}, err
	}
	first, err := s.firstMessage(ctx, sess, req.Message)
	if err != nil {
		return store.Session{}, err
	}
	bundle, err := resumeBundle(sess, choice, req.SyncBundle)
	if err != nil {
		return store.Session{}, err
	}
	// A snapshot still being taken is waited for, never skipped: a
	// stop-and-resume would otherwise lose the work it exists to keep.
	plan := runPlan{N: nextRun(runs), Origin: store.RunFromTranscript}
	if choice.Mode == ResumeSnapshot {
		if choice.Pending {
			if err := s.collectForResume(ctx, from); err != nil {
				return store.Session{}, err
			}
		}
		plan.Origin, plan.From = store.RunFromSnapshot, from.Snapshot
	}
	name, err := s.freeName(ctx, sess)
	if err != nil {
		return store.Session{}, err
	}
	was := sess.Name
	sess, err = s.reopen(ctx, sess, name, timeout, plan, first, quota.MaxConcurrentSessions)
	if err != nil {
		return store.Session{}, err
	}
	s.markResumed(ctx, sess, plan, was)
	s.startResume(ctx, sess, plan, bundle)
	return sess, nil
}

// resumeBundle decodes a resume's sync bundle, kept only for a transcript
// resume of a synced session. A synced session with no platform has
// nothing to clone, so its transcript resume needs one.
//
// @decision(DL-098)
func resumeBundle(sess store.Session, choice ResumeView, encoded string) ([]byte, error) {
	if choice.Mode != ResumeTranscript || sess.Request.Sync == nil {
		return nil, nil
	}
	var bundle []byte
	if encoded != "" {
		var err error
		if bundle, err = base64.StdEncoding.DecodeString(encoded); err != nil {
			return nil, fmt.Errorf("%w: sync_bundle is not valid base64", ErrInvalidRequest)
		}
		if len(bundle) > git.MaxSyncBundleBytes {
			return nil, fmt.Errorf("%w: sync_bundle is %d bytes; the limit is %d", ErrInvalidRequest, len(bundle), git.MaxSyncBundleBytes)
		}
	}
	localOnly := len(sess.Request.Repos) == 1 && sess.Request.Repos[0].Platform == ""
	if localOnly && len(bundle) == 0 {
		return nil, fmt.Errorf("%w: its repository is only on the machine it was launched from, and it resumes from its transcript;"+
			" run spov resume inside that checkout", ErrInvalidRequest)
	}
	return bundle, nil
}

// resumeTimeout is the resumed sandbox's lifetime: an explicit one,
// refused over the limit, or the launch's, cut to it.
//
// @decision(DL-087) @decision(DL-009)
func resumeTimeout(launched time.Duration, secs int, limit time.Duration) (time.Duration, error) {
	if secs > 0 {
		asked := time.Duration(secs) * time.Second
		if limit > 0 && asked > limit {
			return 0, fmt.Errorf("%w: a timeout of %s is over your limit of %s", ErrInvalidRequest, asked, limit)
		}
		return asked, nil
	}
	if limit > 0 && launched > limit {
		return limit, nil
	}
	return launched, nil
}

// firstMessage checks a resume's message as a steer is checked, and
// encodes it for the run.
//
// @decision(DL-087) @decision(DL-019)
func (s *Service) firstMessage(ctx context.Context, sess store.Session, msg *SteerRequest) ([]byte, error) {
	if msg == nil {
		return nil, nil
	}
	if _, _, _, _, err := validSteer(*msg); err != nil {
		return nil, err
	}
	conversation, err := s.steerInto(ctx, sess, msg.Conversation)
	if err != nil && !errors.Is(err, ErrNoConversation) {
		return nil, err
	}
	m := *msg
	m.Conversation = conversation
	return json.Marshal(m)
}

// collectForResume collects a pending snapshot itself, waiting at most
// SnapshotResumeWait, and records what it found.
//
// @decision(DL-088)
func (s *Service) collectForResume(ctx context.Context, run *store.Run) error {
	outcome, kept := s.settleSnapshot(ctx, run, SnapshotResumeWait)
	// The wait ran out: the person is offered the transcript next, not
	// asked to wait again.
	if outcome == "" {
		run.SnapshotState, outcome = store.SnapshotFailed, "failed"
	}
	if err := s.deps.Store.Runs().Update(context.WithoutCancel(ctx), *run); err != nil {
		return err
	}
	s.deps.Metrics.SnapshotCollected(ctx, outcome)
	if !kept {
		return fmt.Errorf("%w; resume again to start from its transcript instead, with the repositories cloned fresh", ErrSnapshotUnavailable)
	}
	return nil
}

func nextRun(runs []store.Run) int {
	n := 0
	for _, r := range runs {
		n = max(n, r.N)
	}
	return n + 1
}

// freeName is the session's name, or the first free <name>-N when a
// live session has taken it since.
//
// @decision(DL-087)
func (s *Service) freeName(ctx context.Context, sess store.Session) (string, error) {
	for i := 1; i < 100; i++ {
		name := sess.Name
		if i > 1 {
			name = fmt.Sprintf("%s-%d", sess.Name, i)
		}
		other, err := s.deps.Store.Sessions().Get(ctx, sess.OrgID, name)
		switch {
		case errors.Is(err, store.ErrNotFound), err == nil && (other.ID == sess.ID || !other.State.Live()):
			return name, nil
		case err != nil:
			return "", err
		}
	}
	return "", ErrNameInUse
}

// reopen moves the session back to starting in one conditional write,
// within the owner's session quota, and inserts its next run.
//
// @decision(DL-087) @decision(DL-009)
func (s *Service) reopen(ctx context.Context, sess store.Session, name string, timeout time.Duration, plan runPlan, first []byte, max int) (store.Session, error) {
	now := s.deps.Now()
	var out store.Session
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		cur, err := tx.Sessions().Get(ctx, sess.OrgID, sess.ID)
		if err != nil {
			return err
		}
		if cur.State.Live() {
			return fmt.Errorf("%w: it is already %s", ErrNotResumable, cur.State)
		}
		if max > 0 {
			n, err := tx.Sessions().CountLive(ctx, cur.OrgID, cur.OwnerID)
			if err != nil {
				return err
			}
			if n >= max {
				return fmt.Errorf("%w: you have %d of %d sessions running;"+
					" stop one with `spov stop` or ask an admin to raise your limit", ErrQuotaExceeded, n, max)
			}
		}
		cur.State, cur.StoppedAt, cur.Error, cur.SandboxID, cur.Name = store.StateStarting, nil, "", "", name
		cur.Request.Sandbox.Timeout = timeout
		cur.Request.Sandbox.IdleTimeout = min(cur.Request.Sandbox.IdleTimeout, timeout)
		cur.CancelRequestedAt = nil
		if err := tx.Sessions().Update(ctx, cur); err != nil {
			return err
		}
		state := store.SnapshotPending
		if plan.Origin == store.RunFromTranscript {
			state = store.SnapshotNone
		}
		if err := tx.Runs().Insert(ctx, store.Run{
			OrgID: cur.OrgID, SessionID: cur.ID, OwnerID: cur.OwnerID, N: plan.N,
			BootedImageID: plan.From, Origin: plan.Origin, StartedAt: now, SnapshotState: state, FirstMessage: first,
		}); err != nil {
			return err
		}
		out = cur
		return nil
	})
	if errors.Is(err, store.ErrConflict) {
		return store.Session{}, ErrNameInUse
	}
	if errors.Is(err, ErrQuotaExceeded) {
		s.deps.Metrics.QuotaRefused(ctx, telemetry.QuotaSessions)
	}
	return out, err
}

// markResumed writes the resume's seam into the session's log.
//
// @decision(DL-087)
func (s *Service) markResumed(ctx context.Context, sess store.Session, plan runPlan, was string) {
	how := "from its snapshot: the workspace is as it was when it stopped"
	if plan.Origin == store.RunFromTranscript {
		how = "from its transcript: the repositories are cloned fresh, and uncommitted changes are gone"
	}
	text := fmt.Sprintf("resumed %s (run %d)", how, plan.N)
	if was != sess.Name {
		text += fmt.Sprintf("; another session had taken the name %s, so it is now %s", was, sess.Name)
	}
	now := s.deps.Now()
	if _, err := s.deps.Store.Conversations().PutEvents(context.WithoutCancel(ctx), []store.Event{{
		ID: store.NewID(now), OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerResumed, Text: text, At: now,
	}}); err != nil {
		s.deps.Warn(fmt.Sprintf("resume %s: record the seam: %s", sess.ID, redact.Error(err)))
	}
}

// startResume runs the resumed run's launch in the background, as
// Create runs a launch.
//
// @decision(DL-087)
func (s *Service) startResume(ctx context.Context, sess store.Session, plan runPlan, bundle []byte) {
	log := newLaunchLog(s.deps.Now)
	launchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.resumeLaunchTimeout(plan))
	s.mu.Lock()
	s.pruneLogs(s.deps.Now())
	s.logs[sess.ID] = log
	s.launches[sess.ID] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.launches, sess.ID)
			s.mu.Unlock()
		}()
		s.launch(launchCtx, sess, log, telemetry.KindInteractive, plan, func(opts *session.Options) {
			opts.SandboxName = fmt.Sprintf("%s-run%d", sess.Name, plan.N)
			opts.SnapshotImage = plan.From
			opts.Resume = opencode.ResumeTranscript
			if plan.Origin == store.RunFromSnapshot {
				opts.Resume = opencode.ResumeSnapshot
			}
			// @decision(DL-098)
			if opts.Sync != nil && len(bundle) > 0 {
				opts.Sync.Bundle = bundle
			}
		})
	}()
}

// resumeLaunchTimeout is a resume's launch budget: a transcript resume
// also imports its conversations.
//
// @decision(DL-089)
func (s *Service) resumeLaunchTimeout(plan runPlan) time.Duration {
	if plan.Origin == store.RunFromTranscript {
		return s.deps.LaunchTimeout + ImportBudget
	}
	return s.deps.LaunchTimeout
}

// ImportBudget bounds a transcript resume's conversation imports.
//
// @decision(DL-089)
const ImportBudget = 2 * time.Minute

// deliverFirstMessage sends the message a resume carried, once the
// session runs, and clears it from the run.
//
// @decision(DL-087)
func (s *Service) deliverFirstMessage(ctx context.Context, sess store.Session) {
	run, err := s.deps.Store.Runs().Current(ctx, sess.OrgID, sess.ID)
	if err != nil || len(run.FirstMessage) == 0 {
		return
	}
	var msg SteerRequest
	if err := json.Unmarshal(run.FirstMessage, &msg); err != nil {
		s.deps.Warn(fmt.Sprintf("resume %s: the first message is unreadable: %v", sess.ID, err))
		return
	}
	owner := auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	if _, err := s.Steer(ctx, owner, sess.ID, msg); err != nil {
		s.deps.Warn(fmt.Sprintf("resume %s: send the first message: %s", sess.ID, redact.Error(err)))
		s.noteFirstMessage(ctx, sess, "the message sent with the resume could not be delivered: "+redact.Error(err))
	}
	run.FirstMessage = nil
	if err := s.deps.Store.Runs().Update(ctx, run); err != nil {
		s.deps.Warn(fmt.Sprintf("resume %s: clear the first message: %s", sess.ID, redact.Error(err)))
	}
}

// MarkerFirstMessageFailed notes a resume's message that never reached
// the agent.
//
// @decision(DL-087)
const MarkerFirstMessageFailed = store.SpovNamespace + "session.resume.message_failed"

func (s *Service) noteFirstMessage(ctx context.Context, sess store.Session, text string) {
	now := s.deps.Now()
	if _, err := s.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerFirstMessageFailed, Text: text, At: now,
	}}); err != nil {
		s.deps.Warn(fmt.Sprintf("resume %s: note the failed message: %s", sess.ID, redact.Error(err)))
	}
}

// resumeInput and resumeOutput are the resume operation's shapes.
type resumeInput struct {
	Ref  string `path:"ref" doc:"Session ID or name"`
	Body ResumeRequest
}

type resumeOutput struct {
	Body SessionView
}

// registerResume serves POST /sessions/{ref}/resume.
//
// @decision(DL-087)
func (s *Server) registerResume() {
	register(s.api, huma.Operation{
		OperationID: "resume-session", Method: http.MethodPost, Path: "/api/v1/sessions/{ref}/resume",
		Summary: "Reopen a stopped session in a new sandbox", DefaultStatus: http.StatusAccepted, Tags: []string{"sessions"},
		// @decision(DL-098)
		MaxBodyBytes: MaxSessionBodyBytes,
	}, func(ctx context.Context, in *resumeInput) (*resumeOutput, error) {
		p := principal(ctx)
		sess, err := s.svc.Resume(ctx, p, in.Ref, in.Body)
		if err != nil {
			return nil, toResumeHumaError(err)
		}
		views := []SessionView{viewOf(sess, s.deps.Now())}
		s.withRuns(ctx, p, []store.Session{sess}, views, s.deps.Now())
		return &resumeOutput{Body: views[0]}, nil
	})
}

// toResumeHumaError maps the resume's refusals toHumaError does not.
//
// @decision(DL-087)
func toResumeHumaError(err error) error {
	switch {
	case errors.Is(err, ErrNotResumable), errors.Is(err, ErrSnapshotUnavailable):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, ErrNoConversation):
		return huma.Error409Conflict(err.Error())
	}
	return toSteerHumaError(err)
}
