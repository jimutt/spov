package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

const (
	// RenewWithin is how close to its expiry a session's renewable git
	// token is minted again.
	//
	// @decision(DL-021)
	RenewWithin = 15 * time.Minute
	// RenewInterval is how often the renewal pass runs.
	//
	// @decision(DL-021)
	RenewInterval = time.Minute
	// renewTimeout bounds one session's re-mint: the forge calls and the
	// execs together.
	renewTimeout = 45 * time.Second
)

// renewal is one session's re-mint state in this process.
//
// @decision(DL-021)
type renewal struct {
	// work serialises what writes credentials into the session's
	// sandbox: a re-mint and a sidecar's recreation (DL-034), which would
	// otherwise revoke each other's tokens.
	work sync.Mutex
	// attempted is when a re-mint was last tried, successful or not.
	// Activity must be newer than this as well as newer than the stored
	// mint, or a refresh that keeps failing would write into an idle
	// sandbox every minute.
	attempted time.Time
	// held are, per platform, the revokers of re-minted tokens that may
	// still be in the sandbox. A token is revoked only once a later one
	// for its platform is known to have been written in.
	held map[string][]func(context.Context) error
	// warnedNone is set once the session is found to have no renewable
	// platform left, so that is said once.
	warnedNone bool
	// failureNoted is set once a failed re-mint is noted in the
	// session's log, and cleared by a successful one, so a run of
	// failures is noted once.
	//
	// @decision(DL-021)
	failureNoted bool
	// pushFailed is when writing a refreshed OpenCode sign-in into the
	// session's sidecar last failed.
	//
	// @decision(DL-015)
	pushFailed time.Time
}

// RunRenewals runs RenewGitTokens every RenewInterval until ctx ends,
// and then writes a refreshed OpenCode sign-in into the sessions behind
// on it. It runs where the reconciler runs, on one replica.
//
// @decision(DL-021)
func (s *Service) RunRenewals(ctx context.Context) {
	for {
		s.RenewGitTokens(ctx)
		// @decision(DL-015)
		s.pushOpenCodeGrants(ctx, nil)
		jitter := time.Duration(rand.Int64N(int64(RenewInterval / 10)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(RenewInterval + jitter):
		}
	}
}

// RenewGitTokens re-mints the renewable git token of every live session
// whose token is due and which is in use, and releases the re-minted
// tokens of sessions that are no longer live. It reads the store, and
// touches a sandbox only to write a due token into it.
//
// @decision(DL-021)
func (s *Service) RenewGitTokens(ctx context.Context) {
	sessions, err := s.deps.Store.Sessions().ListLive(ctx)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("renew git tokens: list sessions: %s", redact.Error(err)))
		return
	}
	live := map[string]bool{}
	now := s.deps.Now()
	for _, sess := range sessions {
		live[sess.ID] = true
		if !renewalDue(sess, now) {
			continue
		}
		inUse, err := s.inUse(ctx, sess)
		if err != nil {
			s.deps.Warn(fmt.Sprintf("renew git token for %s: %s", sess.ID, redact.Error(err)))
			continue
		}
		if !inUse {
			// Due but idle: not re-minted, so the session can idle out.
			//
			// @decision(DL-011)
			s.deps.Metrics.TokenRenewal(ctx, telemetry.OutcomeSkipped)
			continue
		}
		// @decision(DL-011)
		if err := s.renewGitToken(ctx, sess); err != nil {
			s.deps.Warn(fmt.Sprintf("renew git token for %s: %s", sess.ID, redact.Error(err)))
			s.deps.Metrics.TokenRenewal(ctx, telemetry.OutcomeFailed)
			s.noteRenewalFailed(ctx, sess, err)
		} else {
			s.deps.Metrics.TokenRenewal(ctx, telemetry.OutcomeRenewed)
			s.mu.Lock()
			s.renewalFor(sess.ID).failureNoted = false
			s.mu.Unlock()
		}
	}
	s.releaseEnded(ctx, live)
}

// releaseEnded releases the re-mint state of every session not in live,
// which must be every live session.
//
// @decision(DL-021) @decision(DL-015)
func (s *Service) releaseEnded(ctx context.Context, live map[string]bool) {
	s.mu.Lock()
	var gone []string
	for id := range s.renewals {
		if !live[id] {
			gone = append(gone, id)
		}
	}
	s.mu.Unlock()
	for _, id := range gone {
		s.releaseRenewed(ctx, id)
	}
}

// renewalDue reports whether a session's renewable token is within
// RenewWithin of its expiry, or past it. A session with no renewable
// token never is.
//
// @decision(DL-021)
func renewalDue(sess store.Session, now time.Time) bool {
	return sess.State == store.StateRunning && sess.SandboxID != "" &&
		sess.GitTokenExpiresAt != nil && sess.GitTokenExpiresAt.Sub(now) <= RenewWithin
}

// renewalFor returns the session's re-mint state, making it if needed.
// The caller holds s.mu.
func (s *Service) renewalFor(id string) *renewal {
	if s.renewals == nil {
		s.renewals = map[string]*renewal{}
	}
	r := s.renewals[id]
	if r == nil {
		r = &renewal{held: map[string][]func(context.Context) error{}}
		s.renewals[id] = r
	}
	return r
}

// inUse reports whether anything happened in a session since its current
// token was minted and since the last re-mint was tried: an unfinished
// job, a stored event other than the recorder's own markers, or a
// connect minted for someone other than the recorder. A re-mint writes
// into the sandbox, which resets its idle clock, so an idle session is
// never re-minted.
//
// @decision(DL-021)
func (s *Service) inUse(ctx context.Context, sess store.Session) (bool, error) {
	job, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID)
	switch {
	case err == nil && !job.State.Terminal():
		return true, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return false, err
	}
	if sess.GitTokenMintedAt == nil {
		return false, nil
	}
	since := *sess.GitTokenMintedAt
	s.mu.Lock()
	if r := s.renewals[sess.ID]; r != nil && r.attempted.After(since) {
		since = r.attempted
	}
	s.mu.Unlock()
	// @decision(DL-021)
	ev, err := s.deps.Store.Conversations().LastEventExcept(ctx, sess.OrgID, sess.ID, []string{MarkerGap, MarkerStopped, MarkerConversationLimit, MarkerRenewalFailed,
		// @decision(DL-075)
		MarkerRoutedLeftOut})
	switch {
	case err == nil && ev.CreatedAt.After(since):
		return true, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return false, err
	}
	mints, err := s.deps.Store.Mints().ListBySession(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return false, err
	}
	for _, m := range mints {
		if m.UserID != auth.RecorderID && m.CreatedAt.After(since) {
			return true, nil
		}
	}
	return false, nil
}

// renewGitToken mints a new token for each of the session's renewable
// platforms and writes it into the sandbox, on the exec's standard input.
// Each platform stands alone: one that fails does not undo another. The
// row's expiry moves on only when every platform succeeded, so a failure
// stays due and is tried again after the next activity.
//
// @decision(DL-021)
func (s *Service) renewGitToken(ctx context.Context, sess store.Session) error {
	ctx, cancel := context.WithTimeout(ctx, renewTimeout)
	defer cancel()
	s.mu.Lock()
	r := s.renewalFor(sess.ID)
	r.attempted = s.deps.Now()
	s.mu.Unlock()
	r.work.Lock()
	defer r.work.Unlock()

	// The tiers the launch recorded, so a renewal never switches whose
	// credential the sandbox holds.
	//
	// @decision(DL-042)
	forge, tiers, err := s.forgeFor(ctx, sess.OrgID, sess.OwnerID, policyOf(sess))
	if err != nil {
		return err
	}
	// @decision(DL-043)
	selections, err := s.recordedSelections(ctx, forge, tiers, sess)
	if err != nil {
		return err
	}
	h, err := s.handle(ctx, sess)
	if err != nil {
		return err
	}
	// The tag, not whether the sidecar answers, decides where a token
	// goes: a proxied sandbox whose sidecar has died must not get its
	// token written into the main container.
	//
	// @decision(DL-034)
	info, err := h.Info(ctx)
	if err != nil {
		return err
	}
	proxied := info.Tags[sandbox.TagProxy] == "1"
	var (
		failures []error
		earliest time.Time
		renewed  int
	)
	for _, sel := range selections {
		if sel.Platform == nil || (len(sel.Repos) == 0 && !sel.All) || !git.Renewable(sel.Platform) {
			continue
		}
		renewed++
		name := sel.Platform.Name()
		tok, err := git.IssueFor(ctx, sel, sess.Request.Sandbox.Timeout)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
			continue
		}
		gh := opencode.CLIForKind(sel.Platform.Kind()) == opencode.CLIGh
		res, err := writeToken(ctx, h, proxied, name, sel.Platform.Host(), tok.Username, gh, tok.Secret)
		if err == nil && res.ExitCode != 0 {
			err = fmt.Errorf("the refresh exited %d: %s", res.ExitCode, truncateFrame(redact.Text(res.Stderr)))
		}
		if err != nil {
			// The refresh may have written the token before it failed,
			// so it is held rather than revoked, as is the one before it.
			s.hold(sess.ID, name, tok.Revoke, false)
			failures = append(failures, fmt.Errorf("%s: writing the token into the sandbox: %w", name, err))
			continue
		}
		s.hold(sess.ID, name, tok.Revoke, true)
		if !tok.ExpiresAt.IsZero() && (earliest.IsZero() || tok.ExpiresAt.Before(earliest)) {
			earliest = tok.ExpiresAt
		}
	}
	if renewed == 0 {
		s.mu.Lock()
		r := s.renewalFor(sess.ID)
		warned := r.warnedNone
		r.warnedNone = true
		s.mu.Unlock()
		if warned {
			return nil
		}
		return errors.New("no renewable platform is configured for this session any more")
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	if earliest.IsZero() {
		return nil
	}
	return s.deps.Store.Sessions().SetGitToken(ctx, sess.OrgID, sess.ID, s.deps.Now(), earliest)
}

// hold records a re-minted token that may be in the session's sandbox.
// When written is true the token is known to be in place, so every token
// held before it for the platform is revoked.
//
// @decision(DL-021)
func (s *Service) hold(id, platform string, revoke func(context.Context) error, written bool) {
	s.mu.Lock()
	r := s.renewalFor(id)
	var superseded []func(context.Context) error
	if written {
		superseded = r.held[platform]
		r.held[platform] = nil
	}
	if revoke != nil {
		r.held[platform] = append(r.held[platform], revoke)
	}
	s.mu.Unlock()
	for _, fn := range superseded {
		if err := fn(context.Background()); err != nil {
			s.deps.Warn(fmt.Sprintf("renew git token for %s: revoke a superseded token: %s", id, redact.Error(err)))
		}
	}
}

// releaseRenewed revokes every re-minted token a session may hold and
// forgets its re-mint state.
//
// @decision(DL-021)
func (s *Service) releaseRenewed(ctx context.Context, id string) {
	s.mu.Lock()
	r := s.renewals[id]
	delete(s.renewals, id)
	s.mu.Unlock()
	if r == nil {
		return
	}
	for _, fns := range r.held {
		for _, fn := range fns {
			if err := fn(context.WithoutCancel(ctx)); err != nil {
				s.deps.Warn(fmt.Sprintf("session %s: revoke a re-minted git token: %s", id, redact.Error(err)))
			}
		}
	}
}

// writeToken gives a re-minted token to the sandbox's credential proxy.
// A sandbox launched without the proxy holds its tokens in the main
// container's files, so there the refresh rewrites those instead. The
// token goes on stdin either way.
//
// @decision(DL-021) @decision(DL-034)
func writeToken(ctx context.Context, h sandbox.Handle, proxied bool, platform, host, user string, gh bool, token string) (sandbox.ExecResult, error) {
	stdin := []byte(token + "\n")
	if proxied {
		return h.SidecarExecInput(ctx, credproxy.SidecarName, credproxy.SetTokenCommand(platform), stdin)
	}
	return h.ExecInput(ctx, opencode.RefreshArgs(host, user, gh), stdin)
}

// MarkerRenewalFailed records, in a session's log, that its git token
// could not be renewed and when the one in the sandbox stops working.
//
// @decision(DL-021) @decision(DL-058)
const MarkerRenewalFailed = store.SpovNamespace + "session.credentials.renewal.failed"

// noteRenewalFailed writes MarkerRenewalFailed, once per run of failed
// re-mints; a failure to write only warns.
//
// @decision(DL-021)
func (s *Service) noteRenewalFailed(ctx context.Context, sess store.Session, cause error) {
	s.mu.Lock()
	r := s.renewalFor(sess.ID)
	noted := r.failureNoted
	r.failureNoted = true
	s.mu.Unlock()
	if noted {
		return
	}
	now := s.deps.Now()
	reason := strings.Join(strings.Fields(redact.Error(cause)), " ")
	text := "Could not renew the git token (" + reason + ")"
	if exp := sess.GitTokenExpiresAt; exp != nil {
		verb := "ends"
		if !exp.After(now) {
			verb = "ended"
		}
		text += "; git access through it " + verb + " at " + exp.UTC().Format("15:04") + " UTC"
	}
	if _, err := s.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerRenewalFailed, Text: text + ".", At: now,
	}}); err != nil {
		s.deps.Warn(fmt.Sprintf("session %s: recording the failed renewal: %v", sess.ID, err))
	}
}
