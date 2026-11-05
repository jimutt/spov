package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

const (
	// SidecarCheckInterval is how often every proxied sandbox's sidecar
	// is looked at.
	//
	// @decision(DL-034)
	SidecarCheckInterval = time.Minute
	// recreateTimeout bounds one sidecar's recreation: the forge calls,
	// the CA exec and the sidecar start together.
	recreateTimeout = 2 * time.Minute
)

// ProxyGaveOut is why a sandbox whose proxy sidecar died twice was ended.
const ProxyGaveOut = "the credential proxy sidecar stopped a second time, so the sandbox was ended"

// RunSidecarChecks runs CheckSidecars every SidecarCheckInterval until
// ctx ends. It runs where the reconciler runs, on one replica.
//
// @decision(DL-034)
func (s *Service) RunSidecarChecks(ctx context.Context) {
	for {
		s.CheckSidecars(ctx)
		jitter := time.Duration(rand.Int64N(int64(SidecarCheckInterval / 10)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(SidecarCheckInterval + jitter):
		}
	}
}

// CheckSidecars looks at the sidecar of every running session launched
// with the proxy. The first time one has exited, it is recreated with
// fresh credentials and a new CA; the second time, the sandbox is ended.
// Looking resets no idle clock (spike S-I3).
//
// @decision(DL-034)
func (s *Service) CheckSidecars(ctx context.Context) {
	// Without a proxy image nothing can be recreated, and this server
	// launched no sandbox that needs it.
	if s.deps.Provider == nil || s.deps.ProxyImage == nil {
		return
	}
	infos, err := s.deps.Provider.List(ctx, map[string]string{sandbox.TagSpov: "1", sandbox.TagProxy: "1"})
	if err != nil {
		s.deps.Warn(fmt.Sprintf("check proxy sidecars: list sandboxes: %s", redact.Error(err)))
		return
	}
	proxied := make(map[string]sandbox.Info, len(infos))
	for _, info := range infos {
		if info.State == sandbox.StateRunning {
			proxied[info.ID] = info
		}
	}
	if len(proxied) == 0 {
		return
	}
	sessions, err := s.deps.Store.Sessions().ListLive(ctx)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("check proxy sidecars: list sessions: %s", redact.Error(err)))
		return
	}
	for _, sess := range sessions {
		// A starting session's launch may not have started its sidecar
		// yet; the launch itself fails if it cannot.
		info, ok := proxied[sess.SandboxID]
		if sess.State != store.StateRunning || !ok {
			continue
		}
		if err := s.checkSidecar(ctx, sess, info); err != nil {
			s.deps.Warn(fmt.Sprintf("session %s: the proxy sidecar: %s", sess.ID, redact.Error(err)))
		}
	}
}

// checkSidecar recreates or ends one session's dead sidecar. A job that
// is not at work (provisioning, delivering) is left to its own path.
//
// @decision(DL-034)
func (s *Service) checkSidecar(ctx context.Context, sess store.Session, info sandbox.Info) error {
	job, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID)
	switch {
	case err == nil && !job.State.Terminal() && !jobAtWork(job.State):
		return nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	}
	h, err := s.handle(ctx, sess)
	if err != nil {
		return err
	}
	running, err := h.SidecarRunning(ctx, credproxy.SidecarName)
	if err != nil || running {
		return err
	}
	// A stopped sandbox's sidecar is not found either; that sandbox is
	// the reconciler's.
	now, err := h.Info(ctx)
	if err != nil || now.State != sandbox.StateRunning {
		return err
	}
	if info.Tags[sandbox.TagProxyRecreated] != "" {
		// Counted as failed only here, where the session is given up on.
		//
		// @decision(DL-011)
		s.deps.Metrics.ProxyRecovery(ctx, telemetry.OutcomeFailed)
		return s.endForProxy(ctx, sess)
	}
	s.mu.Lock()
	r := s.renewalFor(sess.ID)
	s.mu.Unlock()
	r.work.Lock()
	defer r.work.Unlock()
	if err := s.recreateSidecar(ctx, sess, h); err != nil {
		return fmt.Errorf("it had stopped, and recreating it failed: %w", err)
	}
	s.deps.Metrics.ProxyRecovery(ctx, telemetry.OutcomeRecreated)
	s.deps.Warn(fmt.Sprintf("session %s: the proxy sidecar had stopped; it was recreated with fresh credentials", sess.ID))
	return nil
}

// jobAtWork reports whether a job's sandbox is in use by the job, so a
// dead sidecar is the job's problem to fix. A provisioning job's launch
// and a delivering job's collection each end on their own.
func jobAtWork(state store.JobState) bool {
	switch state {
	case store.JobRunning, store.JobBlocked, store.JobSteered:
		return true
	default:
		return false
	}
}

// recreateSidecar mints or reads every platform's credential and the
// provider keys again and starts a new sidecar, with the sandbox's own
// CA, or with a new one the main container is made to trust when none is
// kept.
//
// @decision(DL-034) @decision(DL-021)
func (s *Service) recreateSidecar(ctx context.Context, sess store.Session, h sandbox.Handle) (err error) {
	// Resolved outside the timeout: the first use after a restart may
	// bake it.
	image, err := s.deps.ProxyImage(ctx)
	if err != nil {
		return fmt.Errorf("the proxy image: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, recreateTimeout)
	defer cancel()
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
	egress, err := s.egressFor(sess.Request.Egress, selections)
	if err != nil {
		return err
	}
	var (
		tokens  []git.PlatformToken
		revokes = map[string]func(context.Context) error{}
	)
	defer func() {
		if err != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			for _, revoke := range revokes {
				_ = revoke(rctx)
			}
		}
	}()
	for _, sel := range selections {
		if sel.Platform == nil || (len(sel.Repos) == 0 && !sel.All) {
			continue
		}
		name := sel.Platform.Name()
		tok, err := git.IssueFor(ctx, sel, sess.Request.Sandbox.Timeout)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if tok.Revoke != nil {
			revokes[name] = tok.Revoke
		}
		tokens = append(tokens, git.PlatformToken{
			Platform: name, Host: sel.Platform.Host(), Username: tok.Username, Secret: tok.Secret,
			Renewable: git.Renewable(sel.Platform), ExpiresAt: tok.ExpiresAt, All: sel.All,
		})
	}
	// The keys are resolved as a launch resolves them, now.
	//
	// @decision(DL-034) @decision(DL-042)
	keys, err := s.providerKeysFor(ctx, sess.OrgID, sess.OwnerID)
	if err != nil {
		return err
	}
	// The sandbox's own CA, which every process in it already trusts.
	//
	// @decision(DL-034)
	ps, err := session.NewProxySidecar(ctx, func(context.Context) (string, error) { return image, nil },
		tokens, selections, session.KeyProviders(keys, prep.ProviderAPIs()), egress, s.deps.Now(), s.proxyCAFor(ctx, sess))
	if err != nil {
		return err
	}
	if ps == nil {
		return errors.New("there is no forge token or provider key to give it")
	}
	// Tagged once the credentials are in hand, so the attempt that counts
	// is one that reaches the sandbox, and a failure from here on still
	// counts as the one.
	if err := h.AddTags(ctx, map[string]string{sandbox.TagProxyRecreated: "1"}); err != nil {
		return err
	}
	// A CA the sandbox does not trust yet is installed.
	//
	// @decision(DL-034)
	if !ps.Reused {
		res, err := h.ExecInput(ctx, credproxy.InstallCACommand(), []byte(ps.CACert))
		if err == nil && res.ExitCode != 0 {
			err = fmt.Errorf("exit %d: %s", res.ExitCode, truncateFrame(redact.Text(res.Stderr)))
		}
		if err != nil {
			return fmt.Errorf("installing the new CA: %w", err)
		}
	}
	if err := h.StartSidecar(ctx, ps.Sidecar); err != nil {
		return err
	}
	// The new tokens are in place, so the re-minted ones the dead sidecar
	// held are revoked, and the renewal pass keys on the new expiry.
	var earliest time.Time
	for _, t := range tokens {
		if revoke := revokes[t.Platform]; revoke != nil {
			s.hold(sess.ID, t.Platform, revoke, true)
		}
		if t.Renewable && !t.ExpiresAt.IsZero() && (earliest.IsZero() || t.ExpiresAt.Before(earliest)) {
			earliest = t.ExpiresAt
		}
	}
	if !earliest.IsZero() {
		if err := s.deps.Store.Sessions().SetGitToken(ctx, sess.OrgID, sess.ID, s.deps.Now(), earliest); err != nil {
			s.deps.Warn(fmt.Sprintf("session %s: record the recreated sidecar's token expiry: %s", sess.ID, redact.Error(err)))
		}
	}
	// The new sidecar holds the OpenCode sign-in as it is now, or none,
	// in which case no later token is written into it.
	//
	// @decision(DL-015)
	held := 0
	if token, gen := s.consoleKey(ctx, sess.OrgID, sess.OwnerID); gen > 0 && keys[prep.OpenCodeKeyEnv] == token {
		held = gen
	}
	if held != sess.OpenCodeGrant {
		if err := s.deps.Store.Sessions().SetOpenCodeGrant(ctx, sess.OrgID, sess.ID, held); err != nil {
			s.deps.Warn(fmt.Sprintf("session %s: record the recreated sidecar's OpenCode sign-in: %s", sess.ID, redact.Error(err)))
		}
	}
	return nil
}

// endForProxy ends a session whose sidecar died again. A job's session
// is ended through the job, with a marker its verdict fails it by.
//
// @decision(DL-034)
func (s *Service) endForProxy(ctx context.Context, sess store.Session) error {
	s.deps.Warn(fmt.Sprintf("session %s: %s", sess.ID, ProxyGaveOut))
	job, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID)
	switch {
	case err == nil && !job.State.Terminal():
		if err := s.MarkJobEvent(ctx, job, jobs.MarkerProxyFailed, ProxyGaveOut); err != nil {
			return err
		}
		return s.TerminateJobSandbox(ctx, job)
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	}
	// The reason is written only on a session this call stopped.
	if cur, err := s.deps.Store.Sessions().Get(ctx, sess.OrgID, sess.ID); err != nil || !cur.State.Live() {
		return err
	}
	if err := s.Delete(ctx, auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}, sess.ID); err != nil {
		return err
	}
	sess, err = s.deps.Store.Sessions().Get(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return err
	}
	sess.Error = ProxyGaveOut
	return s.deps.Store.Sessions().Update(ctx, sess)
}
