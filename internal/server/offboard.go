package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// AdminMemberOffboarded is the audit action an allowlist removal writes
// once it has ended what the person had running and held.
//
// @decision(DL-132)
const AdminMemberOffboarded = "allowlist.offboarded"

// offboardResult counts what an off-boarding ended, and names each step
// that did not finish.
//
// @decision(DL-132)
type offboardResult struct {
	JobsCancelled      int
	SessionsStopped    int
	TokensRevoked      int
	CredentialsDeleted int
	// Left names what is still in place, one phrase per failed step.
	Left []string
}

// summary is the counts as one line, for the audit row and the answer.
func (r offboardResult) summary() string {
	return fmt.Sprintf("%s, %s, %s, %s",
		plural(r.JobsCancelled, "job", "cancelled"), plural(r.SessionsStopped, "session", "stopped"),
		plural(r.TokensRevoked, "CLI token", "revoked"), plural(r.CredentialsDeleted, "credential", "deleted"))
}

func plural(n int, noun, verb string) string {
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s %s", n, noun, verb)
}

// Offboard ends what a person removed from the allowlist still has: it
// cancels their unfinished jobs, stops their live sessions (a held sync
// job's included), revokes their CLI tokens and deletes their stored
// credentials, then writes one audit row. p is the acting admin, so
// every note names them. Each step runs whatever the one before did;
// the error, when there is one, says what is left. Their history (the
// user row, past sessions, transcripts, reports) is kept.
//
// @decision(DL-132)
func (s *Service) Offboard(ctx context.Context, p auth.Principal, email string, user store.User) (offboardResult, error) {
	var r offboardResult
	left := func(what string, err error) {
		r.Left = append(r.Left, what)
		s.deps.Warn("allowlist: off-boarding " + email + ": " + what + ": " + redact.Error(err))
	}

	// Jobs first: a cancel stops the job's session by the path its
	// state calls for, which a plain stop of that session would not.
	unfinished, err := s.deps.Store.Jobs().List(ctx, user.OrgID, store.JobFilter{OwnerID: user.ID, States: store.JobCounts})
	if err != nil {
		left("their unfinished jobs could not be listed", err)
	}
	failedJobs := 0
	var lastJobErr error
	for _, job := range unfinished {
		switch err := s.CancelJob(ctx, p, job.ID); {
		case err == nil:
			r.JobsCancelled++
		case errors.Is(err, ErrJobNotCancellable):
			// It finished meanwhile; its session, if held, is stopped
			// below.
		default:
			failedJobs++
			lastJobErr = err
		}
	}
	if failedJobs > 0 {
		left(plural(failedJobs, "job", "not cancelled"), lastJobErr)
	}

	// Then every session still live: an interactive one, a held sync
	// job's (live with a finished job, which DeleteSession refuses), or
	// a cancelled job's whose stop failed inside CancelJob.
	sessions, err := s.deps.Store.Sessions().List(ctx, user.OrgID, store.SessionFilter{OwnerID: user.ID, States: store.LiveStates})
	if err != nil {
		left("their live sessions could not be listed", err)
	}
	failedSessions := 0
	var lastSessErr error
	for _, sess := range sessions {
		job, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			failedSessions++
			lastSessErr = err
			continue
		case !job.State.Terminal():
			// Its job's cancel failed and is counted above; stopping
			// the session alone would leave the job to run.
			continue
		}
		if err := s.deleteFor(ctx, p, sess.ID, telemetry.TerminationStopped); err != nil {
			failedSessions++
			lastSessErr = err
			continue
		}
		r.SessionsStopped++
	}
	if failedSessions > 0 {
		left(plural(failedSessions, "session", "not stopped"), lastSessErr)
	}

	// Revoked rather than only refused, so re-adding the address does
	// not bring one back.
	now := s.deps.Now()
	tokens, err := s.deps.Store.Tokens().ListByUser(ctx, user.OrgID, user.ID)
	if err != nil {
		left("their CLI tokens could not be listed", err)
	}
	failedTokens := 0
	var lastTokenErr error
	for _, t := range tokens {
		if t.RevokedAt != nil {
			continue
		}
		if err := s.deps.Store.Tokens().Revoke(ctx, t.OrgID, t.ID, now); err != nil {
			failedTokens++
			lastTokenErr = err
			continue
		}
		r.TokensRevoked++
	}
	if failedTokens > 0 {
		left(plural(failedTokens, "CLI token", "not revoked"), lastTokenErr)
	}

	// Every row: forge tokens, provider keys, routed credentials and
	// the OpenCode sign-in alike. Nothing caches one per user.
	creds, err := s.deps.Store.Credentials().ListByUser(ctx, user.OrgID, user.ID)
	if err != nil {
		left("their stored credentials could not be listed", err)
	}
	failedCreds := 0
	var lastCredErr error
	// The OpenCode sign-in goes inside its own lock, which a refresh
	// holds from its read to its write, so one in flight cannot store
	// the grant again once this has deleted it. Its rows in the listing
	// are then gone, which the loop below counts as deleted.
	//
	// @decision(DL-132) @decision(DL-015)
	if s.deps.ConsoleGrants != nil {
		err := s.deps.ConsoleGrants.Locked(ctx, user.OrgID, user.ID, func(cs ConsoleStore) error {
			if err := cs.Delete(ctx, user.OrgID, user.ID, ConsoleSlotAttempt); err != nil {
				return err
			}
			return cs.Delete(ctx, user.OrgID, user.ID, ConsoleSlotGrant)
		})
		if err != nil {
			failedCreds++
			lastCredErr = err
		}
	}
	for _, c := range creds {
		err := s.deps.Store.Credentials().Delete(ctx, c.OrgID, c.UserID, c.Platform)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			failedCreds++
			lastCredErr = err
			continue
		}
		r.CredentialsDeleted++
	}
	if failedCreds > 0 {
		left(plural(failedCreds, "stored credential", "not deleted"), lastCredErr)
	}
	// What they reached through those credentials is not kept either.
	//
	// @decision(DL-134)
	s.ForgetRepoListings(user.OrgID, user.ID)

	detail := email + ": " + r.summary()
	if len(r.Left) > 0 {
		detail += "; left: " + strings.Join(r.Left, ", ")
	}
	if err := s.deps.Store.AdminEvents().Append(ctx, store.AdminEvent{
		ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminMemberOffboarded, At: now,
		Detail: detail,
	}); err != nil {
		left("the audit row was not written", err)
	}
	if len(r.Left) > 0 {
		return r, fmt.Errorf("%s is removed from the allowlist, but not everything of theirs was ended: %s"+
			" (done: %s); send the same delete again to retry",
			email, strings.Join(r.Left, ", "), r.summary())
	}
	return r, nil
}
