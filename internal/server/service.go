package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
	"github.com/jimutt/spov/internal/toolchain"
)

// Service owns sessions: it records them, runs their launches, keeps the
// live handles of the ones this process started, and ends them.
//
// @decision(DL-005)
type Service struct {
	deps Deps

	// pushes are the writes of a refreshed OpenCode sign-in into live
	// sidecars that a launch started; Close waits for them.
	//
	// @decision(DL-015)
	pushes sync.WaitGroup

	mu       sync.Mutex
	live     map[string]*session.Session   // by session id: launched by this process
	logs     map[string]*launchLog         // by session id: launches in progress or recently ended
	launches map[string]context.CancelFunc // by session id: launches in flight
	deleted  map[string]bool               // by session id: deleted while launching
	// imageUses is when this process last wrote each image's use.
	//
	// @decision(DL-016)
	imageUses map[string]time.Time
	// renewals is each session's re-mint state: when one was last tried,
	// and the re-minted tokens that may still be in its sandbox. Lazily
	// made; lost on restart, when the tokens expire on their own.
	//
	// @decision(DL-021)
	renewals map[string]*renewal
	// proxyCAs are the sessions' proxy CAs this process holds.
	//
	// @decision(DL-034)
	proxyCAs map[string]session.ProxyCA
	// passwords caches server passwords recovered from sandboxes this
	// process did not launch. In memory only, never stored, and bounded
	// so a long-lived replica cannot accumulate one per session it has
	// ever served.
	//
	// @decision(DL-030) @decision(DL-003)
	passwords *passwordCache
	// conns caches live connections for the server's OWN sandbox
	// requests, so a steering message or a job's pass does not issue a
	// day-long unrevocable credential of its own. In memory only and
	// bounded, for the same reasons passwords is. The /connect
	// endpoint does not consult it.
	//
	// @decision(DL-030)
	conns *connCache
	// messages is the per-session steering ceiling. In memory and per
	// replica; the durable mint limit is what bounds credentials.
	//
	// @decision(DL-019)
	messages *messageLimiter
	// firstSends serialises a session's first messages, which create
	// its conversation.
	//
	// @decision(DL-019)
	firstSends keyedLocks
	// OnStop runs before a running session's sandbox is terminated by
	// Delete, with the deleting principal, so the last of the
	// conversation can be read while there is still something to read.
	// Best-effort: its outcome does not affect the delete. Nil skips it.
	//
	// @decision(DL-017)
	OnStop func(ctx context.Context, p auth.Principal, sess store.Session)
	// OnStart runs when a launch commits `running`, and again on every
	// steering message (DL-019), so the recorder can attach at once
	// rather than at its next poll. It is a nudge rather than a start
	// hook: Recorder.Wake returns early on a session it already holds,
	// nil skips it, and a nudge that goes nowhere leaves the poll to
	// find the session.
	//
	// @decision(DL-017)
	OnStart func(sess store.Session)
}

// logRetention is how long a finished launch log is kept for late
// event streams before it is dropped.
const logRetention = 10 * time.Minute

// ErrNameInUse means a live session already has the requested name. It
// deliberately does not say whose: in a shared org the name of another
// user's session is not the caller's to learn.
//
// @decision(DL-006)
var ErrNameInUse = errors.New("server: that session name is not available")

// ErrSessionNotRunning means the session has no sandbox to act on.
var ErrSessionNotRunning = errors.New("server: the session is not running")

func newService(deps Deps) *Service {
	deps = deps.withDefaults()
	return &Service{deps: deps, live: map[string]*session.Session{}, logs: map[string]*launchLog{},
		launches: map[string]context.CancelFunc{}, deleted: map[string]bool{},
		imageUses: map[string]time.Time{},
		passwords: newPasswordCache(DefaultPasswordCacheSize),
		conns:     newConnCache(DefaultConnectionCacheSize),
		messages:  newMessageLimiter(MessagesPerMinute)}
}

// Create records a session and starts its launch in the background. It
// returns as soon as the session exists; the launch reports through the
// session's event log.
//
// @decision(DL-005)
func (s *Service) Create(ctx context.Context, p auth.Principal, req store.SessionRequest) (store.Session, error) {
	req = s.withDefaults(req)
	if err := s.validateRequest(req); err != nil {
		return store.Session{}, err
	}
	// One read of the profile answers both of the next two questions.
	//
	// @decision(DL-014)
	profile := s.profileFor(ctx, p.OrgID, p.UserID)
	// Resolved onto the request before the row is written, so the stored
	// request names the model that actually ran.
	//
	// @decision(DL-014)
	model, src := resolveModel(req.Model, profile)
	req.Model = model
	// Before the quota and before anything is provisioned: a model the
	// caller cannot run costs a 422 here and a whole sandbox anywhere
	// later.
	//
	// @decision(DL-013)
	modelNote, err := s.checkModel(ctx, p, req.Model, src)
	if err != nil {
		return store.Session{}, err
	}
	// @decision(DL-043)
	req.GitAccess, req.GitAccessPreferred = resolveGitAccess(req.GitAccess, req.GitAccessPreferred, profile.DefaultGitAccess)
	if err := s.refuseGitAccess(ctx, p.OrgID, p.UserID, req); err != nil {
		return store.Session{}, err
	}
	// @decision(DL-075)
	if err := s.refuseRoutedNames(ctx, p.OrgID, p.UserID, req.Routed); err != nil {
		return store.Session{}, err
	}
	// @decision(DL-077)
	if err := s.refuseRoutesWithoutProxy(ctx, p.OrgID, p.UserID, req.Routed); err != nil {
		return store.Session{}, err
	}
	// The caller's own limits, checked before anything billable. The
	// size limits refuse here; the concurrency limit is counted inside
	// the insert's transaction below, or two creates on different
	// replicas could both pass.
	//
	// @decision(DL-009)
	quota := s.quotaOf(profile)
	if err := quota.checkRequest(req); err != nil {
		// @decision(DL-011)
		s.deps.Metrics.QuotaRefused(ctx, telemetry.QuotaSize)
		return store.Session{}, err
	}
	now := s.deps.Now()
	sess := store.Session{
		ID:      store.NewID(now),
		OrgID:   p.OrgID,
		OwnerID: p.UserID,
		Name:    req.Name,
		State:   store.StateStarting,
		Request: req,
	}
	if err := s.createWithinQuota(ctx, sess, quota.MaxConcurrentSessions); err != nil {
		return store.Session{}, err
	}
	var seedBundle []byte
	if req.Sync != nil {
		seedBundle = req.Sync.Bundle
	}
	sess, _ = s.deps.Store.Sessions().Get(ctx, p.OrgID, sess.ID)
	if len(seedBundle) > 0 && sess.Request.Sync != nil {
		sess.Request.Sync.Bundle = seedBundle
	}

	log := newLaunchLog(s.deps.Now)
	// @decision(DL-013)
	if modelNote != "" {
		log.Note(modelNote)
		s.markModelNote(ctx, sess, modelNote)
	}
	// The launch outlives the request that asked for it.
	launchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.deps.LaunchTimeout)
	s.mu.Lock()
	s.pruneLogs(now)
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
		s.launch(launchCtx, sess, log, telemetry.KindInteractive, runPlan{})
	}()
	return sess, nil
}

// attributionUser is the tag value identifying who a sandbox belongs
// to. It is the session's owner, which every create sets from the
// authenticated principal. A session with no owner falls back to the
// local principal rather than an OS username, which the reconciler
// would reject on read (ownerFromTag).
//
// @decision(DL-009)
func attributionUser(ownerID string) string {
	if ownerID != "" {
		return ownerID
	}
	return auth.LocalID
}

// createWithinQuota counts the owner's live sessions and inserts the new
// one in a single transaction, so the count a create decides on is the
// count it commits against. A quota of zero or less is no limit;
// nothing configures one, but a stored override could.
//
// @decision(DL-009)
func (s *Service) createWithinQuota(ctx context.Context, sess store.Session, max int) error {
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		if max > 0 {
			n, err := tx.Sessions().CountLive(ctx, sess.OrgID, sess.OwnerID)
			if err != nil {
				return err
			}
			if n >= max {
				return fmt.Errorf("%w: you have %d of %d sessions running;"+
					" stop one with `spov stop` or ask an admin to raise your limit", ErrQuotaExceeded, n, max)
			}
		}
		return tx.Sessions().Create(ctx, sess)
	})
	if errors.Is(err, store.ErrConflict) {
		return ErrNameInUse
	}
	// @decision(DL-011)
	if errors.Is(err, ErrQuotaExceeded) {
		s.deps.Metrics.QuotaRefused(ctx, telemetry.QuotaSessions)
	}
	return err
}

// pruneLogs drops finished launch logs older than logRetention. Called
// with s.mu held.
func (s *Service) pruneLogs(now time.Time) {
	for id, log := range s.logs {
		if done, at := log.finished(); done && now.Sub(at) > logRetention {
			delete(s.logs, id)
		}
	}
}

// egressFor resolves a preset name to a policy, adds the hosts of the
// forges the session's selections use, and the server's extra domains.
//
// @decision(DL-031)
//
// @decision(DL-068)
func (s *Service) egressFor(req store.SessionRequest, selections []git.Selection) (sandbox.EgressPolicy, error) {
	policy, err := sandbox.EgressPreset(req.Egress)
	if err != nil {
		return sandbox.EgressPolicy{}, err
	}
	policy = policy.WithForgeHosts(forgeHosts(selections)...)
	policy = policy.WithDomains(s.deps.Defaults.EgressExtraDomains...)
	policy = policy.WithDomains(req.EgressExtraDomains...)
	if err := policy.Validate(); err != nil {
		return sandbox.EgressPolicy{}, err
	}
	return policy, nil
}

// forgeHosts are the hosts of the platforms a session's selections use.
//
// @decision(DL-031)
func forgeHosts(selections []git.Selection) []string {
	var out []string
	for _, sel := range selections {
		if sel.Platform != nil && (len(sel.Repos) > 0 || sel.All) {
			out = append(out, sel.Platform.Host())
		}
	}
	return out
}

func (s *Service) withDefaults(req store.SessionRequest) store.SessionRequest {
	d := s.deps.Defaults
	// Normalised here rather than per path: both creates come through
	// this, and a model that differs between them by padding alone
	// would be validated differently by DL-013's list check.
	//
	// @decision(DL-014)
	req.Model = strings.TrimSpace(req.Model)
	// An unknown value is left for validateRequest to refuse.
	//
	// @decision(DL-043)
	if a, err := store.ParseGitAccess(string(req.GitAccess)); err == nil {
		req.GitAccess = a
	}
	if req.Name == "" {
		req.Name = "spov-" + s.deps.Now().UTC().Format("20060102-150405")
	}
	// The deployment's own add-ons apply to a request that names none,
	// as its egress preset does.
	//
	// @decision(DL-033)
	if req.Image == "" && len(req.Addons) == 0 && len(req.Apt) == 0 {
		req.Addons, req.Apt = d.Addons, d.Apt
	}
	if sel, err := toolchain.Parse(req.Addons); err == nil {
		req.Addons = toolchain.Strings(sel)
	}
	if sandbox.ValidatePackages(req.Apt) == nil {
		req.Apt = sandbox.ExtraPackages(req.Apt)
	}
	if req.Image == "" {
		req.Image = d.Image
		if spec, ok := s.variantOf(req); ok {
			req.Image = spec.Name()
		}
	}
	if req.Egress == "" {
		req.Egress = d.Egress
	}
	sb := &req.Sandbox
	if sb.CPU == 0 {
		sb.CPU = d.Sandbox.CPU
	}
	if sb.MemoryMiB == 0 {
		sb.MemoryMiB = d.Sandbox.MemoryMiB
	}
	if sb.Timeout == 0 {
		sb.Timeout = d.Sandbox.Timeout
	}
	if sb.IdleTimeout == 0 {
		sb.IdleTimeout = d.Sandbox.IdleTimeout
	}
	return req
}

// maxPreferences bounds a request's credential preferences.
const maxPreferences = 32

// ErrInvalidRequest wraps a request the server refuses before doing any
// work; the API maps it to 422.
var ErrInvalidRequest = errors.New("server: invalid request")

// MaxEgressExtraDomains bounds the domains a request adds to its egress
// preset, beside the server's own.
//
// @decision(DL-068)
const MaxEgressExtraDomains = 16

// validateRequest refuses a request before anything is created,
// synchronously, so a bad value answers 422 rather than surfacing later
// as a failed launch.
//
// @decision(DL-005) @decision(DL-029)
func (s *Service) validateRequest(req store.SessionRequest) error {
	var problems []string
	if len(req.Repos) == 0 {
		problems = append(problems, "at least one repository is required")
	}
	for _, r := range req.Repos {
		if r.Owner == "" || r.Name == "" || (r.Platform == "" && req.Sync == nil) {
			problems = append(problems, "every repository needs platform, owner and name")
			break
		}
		if r.Platform == "" && req.Sync != nil {
			if err := git.CheckRepoPath(git.KindGitLab, r.Owner, r.Name); err != nil {
				problems = append(problems, err.Error())
				break
			}
		}
	}
	if len(req.Repos) == 1 && req.Repos[0].Platform == "" && req.Sync != nil && len(req.Sync.Bundle) == 0 {
		problems = append(problems, "a local-only sync launch without a platform requires a git bundle")
	}
	// @decision(DL-042)
	if len(req.CredentialPreference) > maxPreferences {
		problems = append(problems, "too many credential preferences")
	}
	for platform, tier := range req.CredentialPreference {
		if platform == "" || len(platform) > 100 || (Tier(tier) != TierUser && Tier(tier) != TierOrg) {
			problems = append(problems, "a credential preference names a platform and user or org")
			break
		}
	}
	if strings.ContainsAny(req.Name, " /\\") {
		problems = append(problems, "name must not contain spaces or slashes")
	}
	if _, err := s.egressFor(req, nil); err != nil {
		problems = append(problems, err.Error())
	}
	// @decision(DL-043)
	if _, err := store.ParseGitAccess(string(req.GitAccess)); err != nil {
		problems = append(problems, err.Error())
	}
	// @decision(DL-068)
	if len(req.EgressExtraDomains) > MaxEgressExtraDomains {
		problems = append(problems, fmt.Sprintf("%d extra egress domains; the limit is %d", len(req.EgressExtraDomains), MaxEgressExtraDomains))
	}
	// @decision(DL-027)
	if req.Sync != nil {
		if len(req.Repos) != 1 {
			problems = append(problems, "sync requires a single repository")
		}
		if err := git.ValidBranchName(req.Sync.Branch); err != nil {
			problems = append(problems, err.Error())
		}
		if !git.ValidCommitSHA(req.Sync.BaseCommit) {
			problems = append(problems, "sync base_commit must be a 40-character hex commit SHA")
		}
		if len(req.Sync.Bundle) > git.MaxSyncBundleBytes {
			problems = append(problems, fmt.Sprintf("sync bundle is %d bytes; the limit is %d", len(req.Sync.Bundle), git.MaxSyncBundleBytes))
		}
	}
	// The model travels into the sandbox's config through a shell
	// heredoc, so it is bounded here as the goal and the check are.
	//
	// @decision(DL-014)
	if len(req.Model) > MaxModelBytes {
		problems = append(problems, fmt.Sprintf(
			"the model is %d bytes; the limit is %d", len(req.Model), MaxModelBytes))
	}
	// The sandbox rules live in one place; a request is checked against
	// the same Validate the provider boundary uses, on a spec that is
	// otherwise complete.
	sb := req.Sandbox
	probe := sandbox.Spec{
		Image: req.Image, Command: []string{"true"},
		CPU: sb.CPU, MemoryMiB: sb.MemoryMiB, CPULimit: sb.CPULimit, MemoryLimitMiB: sb.MemoryLimitMiB,
		Timeout: sb.Timeout, IdleTimeout: sb.IdleTimeout,
		Regions: sb.Regions, Cloud: sb.Cloud, Workdir: sb.Workdir,
		InboundCIDRs: sb.InboundCIDRs, Egress: sandbox.EgressOpen(),
	}
	if err := probe.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	problems = append(problems, s.variantProblems(req)...)
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, strings.Join(problems, "; "))
	}
	return nil
}

// launch provisions a session's sandbox and records the outcome on the
// row. The variadic mutators adjust the launch options just before the
// launcher is called; a job's session uses one to attach its
// JobOptions, and an ordinary session passes none.
//
// @decision(DL-005) @decision(DL-045)
func (s *Service) launch(ctx context.Context, sess store.Session, log *launchLog, kind telemetry.SessionKind, plan runPlan, with ...func(*session.Options)) {
	// The store is written from a context that no request owns.
	storeCtx := context.WithoutCancel(ctx)
	// @decision(DL-011)
	started := s.deps.Now()
	fail := func(err error) {
		s.deps.Warn(fmt.Sprintf("launch %s: %v", sess.ID, err))
		msg := redact.Error(err)
		t := s.deps.Now()
		sess.StoppedAt = &t
		s.mu.Lock()
		deleted := s.deleted[sess.ID]
		delete(s.deleted, sess.ID)
		s.mu.Unlock()
		// The delete may have been recorded by another replica, where
		// this process's map says nothing.
		//
		// @decision(DL-003)
		if !deleted {
			if got, err := s.deps.Store.Sessions().CancelRequested(storeCtx, sess.OrgID, sess.ID); err == nil {
				deleted = got
			}
		}
		if deleted {
			// The launch was cancelled by a delete; that is the outcome
			// the user asked for, not a failure.
			sess.State = store.StateStopped
			sess.Error = ""
			msg = "deleted while starting"
		} else {
			sess.State = store.StateFailed
			sess.Error = msg
			// @decision(DL-011)
			log.mu.Lock()
			step := launchStep(log.running)
			log.mu.Unlock()
			s.deps.Metrics.SessionLaunched(storeCtx, kind, false, step, s.deps.Now().Sub(started))
		}
		if uerr := s.deps.Store.Sessions().Update(storeCtx, sess); uerr != nil {
			s.deps.Warn(fmt.Sprintf("launch %s: record failure: %v", sess.ID, uerr))
		}
		s.endFailedRun(storeCtx, sess, plan, t)
		s.forgetSession(sess.ID)
		log.fail(ProblemView{Title: "Launch failed", Status: 502, Detail: msg})
	}

	// The session's owner, not the server, decides which credentials
	// reach the sandbox. The session carries the owner already, so the
	// launch needs no principal of its own.
	//
	// @decision(DL-042)
	forge, tiers, err := s.forgeFor(ctx, sess.OrgID, sess.OwnerID, policyFromRequest(sess.Request))
	if err != nil {
		fail(err)
		return
	}
	for _, t := range tiers {
		if t.Tier == TierNone {
			log.Note(fmt.Sprintf("no credential for %s: %s", t.Platform, t.Reason))
		}
	}
	var (
		selections []git.Selection
		denied     []string
	)
	localBundleOnly := len(sess.Request.Repos) == 1 && sess.Request.Repos[0].Platform == "" && sess.Request.Sync != nil
	if !localBundleOnly {
		// A launch that named all itself fails where a platform may not have
		// it; one that took all from a preference falls back and says so.
		//
		// @decision(DL-043)
		selections, denied, err = s.grantSelections(ctx, forge, tiers, sess.Request)
		if err != nil {
			// When a synced launch has a full bundle, missing platform credentials
			// fall back to bundle-only initialization inside the sandbox.
			//
			// @decision(DL-027)
			if sess.Request.Sync != nil && len(sess.Request.Sync.Bundle) > 0 && len(sess.Request.Repos) == 1 {
				log.Note(fmt.Sprintf("seeding %s/%s from your local git bundle without a forge credential (%v)",
					sess.Request.Repos[0].Owner, sess.Request.Repos[0].Name, redact.Error(err)))
				selections, denied = nil, nil
			} else {
				fail(err)
				return
			}
		}
		if len(denied) > 0 {
			if !sess.Request.GitAccessPreferred {
				fail(errors.New(gitAccessRefusal(denied)))
				return
			}
			log.Note(gitAccessFallback(denied))
		}
	}
	sb := sess.Request.Sandbox
	egress, err := s.egressFor(sess.Request, selections)
	if err != nil {
		fail(err)
		return
	}
	// The warning travels with the launch rather than being printed by
	// the client: only the server knows which keys it holds.
	//
	// @decision(DL-036) @decision(DL-002)
	// The user's own keys override the deployment's, per variable.
	//
	// @decision(DL-042)
	providerKeys, err := s.storedProviderKeys(ctx, sess.OrgID, sess.OwnerID)
	if err != nil {
		fail(err)
		return
	}
	// The OpenCode sign-in is refreshed now if it would lapse during
	// the session.
	//
	// @decision(DL-015)
	consoleToken, consoleGen, consoleNote, err := s.freshConsoleToken(ctx, sess.OrgID, sess.OwnerID, sb.Timeout)
	if err != nil {
		fail(err)
		return
	}
	if consoleNote != "" {
		log.Note(consoleNote)
	}
	providerKeys = withConsoleKey(providerKeys, consoleToken)
	// Read at launch, so a credential stored since needs no restart.
	//
	// @decision(DL-075) @decision(DL-073)
	routed, routedNotes, err := s.routesFor(ctx, sess)
	if err != nil {
		fail(err)
		return
	}
	for _, n := range routedNotes {
		log.Note(n)
	}
	proxied := make([]credproxy.Route, len(routed))
	presets := make([]routedcred.Route, len(routed))
	for i, c := range routed {
		proxied[i], presets[i] = c.Proxied(), c.Route
	}
	if len(providerKeys) == 0 {
		log.Note("no model provider key is configured (ANTHROPIC_API_KEY and friends), so the agent cannot reach a model")
	}
	// The author is the launching user's profile, then the server's
	// configured identity as the last resort.
	//
	// @decision(DL-044)
	identity := git.ResolveIdentity(
		git.Identity{Name: sess.Request.GitName, Email: sess.Request.GitEmail},
		func() git.Identity { return s.userIdentity(ctx, sess.OrgID, sess.OwnerID) },
		func() git.Identity { return s.deps.Identity })
	// The variant is derived again from this server's recipe: a job may
	// have waited through a deploy that changed the pin or the toolchain,
	// and the name stored at create would then name an image nobody
	// builds. Add-ons this server no longer offers fail the launch.
	//
	// @decision(DL-033) @decision(DL-016)
	addons, err := toolchain.Parse(sess.Request.Addons)
	if err != nil {
		fail(fmt.Errorf("the session's add-ons cannot be built by this server: %w", err))
		return
	}
	pick := variantPick{image: sess.Request.Image, addons: addons}
	if spec, ok := s.variantOf(sess.Request); ok {
		pick = s.pickVariant(ctx, spec, log.Note)
		// Its name still resolves, so the launch would only meet Modal's
		// NotFound.
		//
		// @decision(DL-016)
		if pick.rebuild && !s.deps.Defaults.BuildOnDemand {
			fail(fmt.Errorf("the image for these add-ons was pruned as unused, and this server does not build images on demand; "+
				"an operator rebuilds it with `%s`", session.BuildCommand(*pick.build)))
			return
		}
	} else if base := s.baseImage(); pick.image == base {
		// The base is never built on demand (DL-032), so a pruned one
		// is the operator's to rebuild.
		//
		// @decision(DL-016)
		if s.ImageDeleted(ctx, base) {
			fail(fmt.Errorf("the image %s was pruned as unused; an operator rebuilds it with `spov serve image build`", base))
			return
		}
		s.useImage(ctx, store.Image{Name: base, Kind: store.ImageBase})
	}
	opts := session.Options{
		Name: sess.Name,
		// Attribution is the session's owner, not the server's own
		// user.
		//
		// @decision(DL-009)
		User:       attributionUser(sess.OwnerID),
		Org:        attributionUser(sess.OrgID),
		Image:      pick.image,
		Selections: selections,
		// @decision(DL-033) @decision(DL-016)
		Variant:        pick.build,
		BuildVariant:   s.deps.Defaults.BuildOnDemand,
		RebuildVariant: pick.rebuild,
		Addons:         pick.addons,
		Landing:        sess.Request.Landing,
		Model:          sess.Request.Model,
		ModelUnlisted:  s.modelUnlisted(sess.Request.Model),
		CPU:            sb.CPU,
		MemoryMiB:      sb.MemoryMiB,
		CPULimit:       sb.CPULimit,
		MemoryLimitMiB: sb.MemoryLimitMiB,
		Timeout:        sb.Timeout,
		IdleTimeout:    sb.IdleTimeout,
		Regions:        sb.Regions,
		Cloud:          sb.Cloud,
		Workdir:        sb.Workdir,
		Port:           s.deps.Defaults.Port,
		TokenTTL:       sb.Timeout,
		Egress:         egress,
		InboundCIDRs:   sb.InboundCIDRs,
		ProviderKeys:   providerKeys,
		// @decision(DL-034)
		ProviderAPIs: prep.ProviderAPIs(),
		// @decision(DL-072) @decision(DL-075)
		Routes: proxied,
		// @decision(DL-076)
		Presets:  presets,
		Identity: identity,
	}
	// @decision(DL-027)
	if sess.Request.Sync != nil {
		var slug, dir string
		if len(sess.Request.Repos) == 1 {
			r := sess.Request.Repos[0]
			slug = r.Owner + "/" + r.Name
			dir = git.SingleRepoDir(r.Owner, r.Name)
		}
		opts.Sync = &session.SyncSeed{
			Branch:     sess.Request.Sync.Branch,
			BaseCommit: sess.Request.Sync.BaseCommit,
			Bundle:     sess.Request.Sync.Bundle,
			Slug:       slug,
			Dir:        dir,
		}
	}
	// @decision(DL-084) @decision(DL-085)
	opts.Run = plan.number()
	opts.ExitSnapshot = kind != telemetry.KindJob && s.deps.Snapshots != nil
	for _, fn := range with {
		fn(&opts)
	}
	live, err := s.deps.Launch(ctx, opts, log)
	if err != nil {
		fail(s.imageGone(storeCtx, pick, err))
		return
	}
	if live == nil {
		fail(errors.New("the launcher returned no session"))
		return
	}
	// A transcript resume imports the session's conversations through
	// the launcher's own connection, before the session is running.
	//
	// @decision(DL-089)
	if plan.origin() == store.RunFromTranscript {
		s.importConversations(ctx, sess, live, log)
	}
	// The launcher's proxy proved the path; the client that attaches
	// runs its own.
	live.CloseProxy()

	// A delete that arrived while the launch ran could not terminate a
	// sandbox that did not exist yet; it is terminated here instead.
	// Cancel and commit are mutually exclusive across replicas.
	//
	// @decision(DL-003)
	sess.State = store.StateRunning
	sess.SandboxID = live.Info.ID
	sess.Clones = live.Clones
	sess.Landing = live.Landing
	// @decision(DL-042)
	sess.Credentials = withTiers(storedCredentials(live.Credentials, true), tiers)
	// @decision(DL-075)
	sess.RoutedCredentials = nil
	for _, c := range routed {
		sess.RoutedCredentials = append(sess.RoutedCredentials, c.record())
	}
	if len(routed) > 0 {
		log.Note("the proxy holds routed credentials " + routedNames(routed))
	}
	// @decision(DL-033)
	sess.Booted = store.BootedImage{Image: pick.image, Addons: toolchain.Installed(pick.addons)}
	noteStoredTokens(log, live.Credentials)
	if len(live.ProxiedKeys) > 0 {
		log.Note(ProxiedKeysNote(live.ProxiedKeys))
	}
	run := s.runOf(sess, plan, live.Info.ID, pick.image, live.ExitSnapshot)
	cancelled, err := s.commitLaunch(storeCtx, sess, run)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("launch %s: record success: %v", sess.ID, err))
	}
	// @decision(DL-034)
	if !cancelled {
		s.keepProxyCA(storeCtx, sess, live.ProxyCA)
	}
	// When the renewable token lapses is what the renewal pass keys on.
	//
	// @decision(DL-021)
	if !cancelled && !live.RenewableExpiry.IsZero() {
		if err := s.deps.Store.Sessions().SetGitToken(storeCtx, sess.OrgID, sess.ID, s.deps.Now(), live.RenewableExpiry); err != nil {
			s.deps.Warn(fmt.Sprintf("launch %s: record the git token's expiry: %v", sess.ID, err))
		}
	}
	// Which grant the sidecar holds is what the renewal pass compares
	// a refreshed grant with. A sandbox without the sidecar cannot be
	// given a new token, so it records none.
	//
	// @decision(DL-015)
	if !cancelled && consoleGen > 0 && slices.Contains(live.ProxiedKeys, prep.OpenCodeKeyEnv) {
		if err := s.deps.Store.Sessions().SetOpenCodeGrant(storeCtx, sess.OrgID, sess.ID, consoleGen); err != nil {
			s.deps.Warn(fmt.Sprintf("launch %s: record the OpenCode sign-in it holds: %v", sess.ID, err))
		}
	}
	if cancelled {
		s.mu.Lock()
		delete(s.deleted, sess.ID)
		s.mu.Unlock()
		if err := live.Close(storeCtx); err != nil {
			s.deps.Warn(fmt.Sprintf("launch %s: terminate after delete: %v", sess.ID, err))
		}
		fail(errors.New("deleted while starting"))
		return
	}
	s.mu.Lock()
	s.live[sess.ID] = live
	// The launch is committed from here: a delete must take the
	// terminate path, not the cancel-the-launch path.
	delete(s.launches, sess.ID)
	s.mu.Unlock()

	// The recorder is told NOW rather than at its next poll. It runs
	// every ReconcileInterval (30 s), which is longer than a short
	// interactive session lives — so without this a person's whole
	// session can finish before anything is listening, and the volatile
	// stream replays nothing.
	//
	// After the cancelled check on purpose: a launch cancelled while
	// starting terminates its sandbox and never reaches here, so no
	// nudge is sent for a session that is already going away. And after
	// the commit, because Sync skips a session that is not `running`
	// with a sandbox id — a nudge sent earlier would find nothing to
	// attach to.
	//
	// @decision(DL-017)
	if s.OnStart != nil {
		s.OnStart(sess)
	}

	// @decision(DL-011)
	s.deps.Metrics.SessionLaunched(storeCtx, kind, true, "", s.deps.Now().Sub(started))
	log.ready(viewOf(sess, s.deps.Now()))
	// @decision(DL-087)
	if plan.number() > 1 {
		s.deliverFirstMessage(storeCtx, sess)
	}
}

// commitLaunch records a finished launch as running, unless a delete
// asked for it to be cancelled first. The read and the write are one
// transaction so no delete can land between them. It reports whether the
// launch was cancelled rather than committed.
//
// @decision(DL-003)
func (s *Service) commitLaunch(ctx context.Context, sess store.Session, run store.Run) (bool, error) {
	var cancelled bool
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		var err error
		if cancelled, err = tx.Sessions().CancelRequested(ctx, sess.OrgID, sess.ID); err != nil {
			return err
		}
		if cancelled {
			return nil
		}
		if err := tx.Sessions().Update(ctx, sess); err != nil {
			return err
		}
		// @decision(DL-085)
		return recordRun(ctx, tx, run)
	})
	return cancelled, err
}

// Log returns a session's launch log, if this process ran the launch.
func (s *Service) Log(id string) (*launchLog, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.logs[id]
	return l, ok
}

// Get resolves a session the principal may see.
func (s *Service) Get(ctx context.Context, p auth.Principal, ref string) (store.Session, error) {
	sess, err := s.deps.Store.Sessions().Get(ctx, p.OrgID, ref)
	// A name no live session holds is the caller's newest stopped
	// session of that name, so a stopped one can be found to resume.
	//
	// @decision(DL-087)
	if errors.Is(err, store.ErrNotFound) && !store.ValidID(ref) {
		if found, ferr := s.deps.Store.Sessions().List(ctx, p.OrgID, store.SessionFilter{Name: ref, OwnerID: p.UserID, Limit: 1}); ferr == nil && len(found) == 1 {
			sess, err = found[0], nil
		}
	}
	if err != nil {
		return store.Session{}, err
	}
	if !p.CanAccess(sess.OwnerID) {
		// Not found rather than forbidden: the existence of another
		// user's session is not the caller's to learn.
		return store.Session{}, store.ErrNotFound
	}
	return sess, nil
}

// List returns the sessions the principal may see, newest first, the
// page f names.
//
// @decision(DL-012)
func (s *Service) List(ctx context.Context, p auth.Principal, f store.SessionFilter) ([]store.Session, error) {
	if !p.Admin {
		f.OwnerID = p.UserID
	}
	return s.deps.Store.Sessions().List(ctx, p.OrgID, f)
}

// CountSessions counts what List would return without its page.
//
// @decision(DL-012)
func (s *Service) CountSessions(ctx context.Context, p auth.Principal, f store.SessionFilter) (int, error) {
	if !p.Admin {
		f.OwnerID = p.UserID
	}
	return s.deps.Store.Sessions().Count(ctx, p.OrgID, f)
}

// StreamList is what the session-state stream sends: the newest limit
// sessions, then every live session older than them, newest first.
//
// @decision(DL-012)
func (s *Service) StreamList(ctx context.Context, p auth.Principal, limit int) ([]store.Session, error) {
	page, err := s.List(ctx, p, store.SessionFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	if len(page) < limit {
		// The whole history fits, so every live session is in it.
		return page, nil
	}
	live, err := s.List(ctx, p, store.SessionFilter{States: store.LiveStates, Before: page[len(page)-1].ID})
	if err != nil {
		return nil, err
	}
	return append(page, live...), nil
}

// Delete terminates the session's sandbox and revokes its tokens, and
// records it stopped only once that has succeeded, so a failed
// termination can be retried. A session that is already stopped is not
// an error. A session still launching is cancelled; its launch records
// the stop and terminates whatever it had created.
//
// @decision(DL-005)
func (s *Service) Delete(ctx context.Context, p auth.Principal, ref string) error {
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return err
	}
	if !sess.State.Live() {
		return nil
	}
	// A launch is only cancellable while it has not yet recorded a
	// sandbox. Once the store has one, cancelling would set a flag the
	// launch is already past reading, and the delete would report
	// success while the sandbox billed on.
	//
	// The signal is durable because the launch may be running on
	// another replica: an in-memory flag would be invisible there, and
	// the delete would mark the row stopped while that replica went on
	// to write it back as running with a live sandbox.
	//
	// @decision(DL-003)
	if sess.State == store.StateStarting && sess.SandboxID == "" {
		cancelled, err := s.deps.Store.Sessions().RequestCancel(ctx, sess.OrgID, sess.ID, s.deps.Now())
		if err != nil {
			return err
		}
		if cancelled {
			// Whoever is launching resolves the row: this replica
			// through the cancel below, another through the check it
			// makes before committing.
			s.mu.Lock()
			cancel, launching := s.launches[sess.ID]
			s.mu.Unlock()
			if launching {
				cancel()
			}
			return nil
		}
		// The launch committed between the read and the update, so the
		// terminate path below is the right one; re-read for the
		// sandbox it recorded.
		if sess, err = s.deps.Store.Sessions().Get(ctx, sess.OrgID, sess.ID); err != nil {
			return err
		}
		if !sess.State.Live() {
			return nil
		}
	}
	// @decision(DL-017)
	if s.OnStop != nil && sess.State == store.StateRunning && sess.SandboxID != "" {
		s.OnStop(ctx, p, sess)
	}

	s.mu.Lock()
	live := s.live[sess.ID]
	s.mu.Unlock()

	switch {
	case live != nil:
		if err := live.Close(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		delete(s.live, sess.ID)
		s.mu.Unlock()
	case sess.SandboxID != "":
		// Any replica can end any session: the sandbox ID is in the
		// store, so the handle is obtainable without the launching
		// process. The token revoker is not, but skipping it here
		// loses nothing durable (DL-041).
		//
		// @decision(DL-003)
		h, err := s.deps.Provider.Get(ctx, sess.SandboxID)
		if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return err
		}
		if err == nil {
			if err := h.Terminate(ctx); err != nil {
				return err
			}
		}
	}
	t := s.deps.Now()
	s.forgetSession(sess.ID)
	// @decision(DL-021)
	s.releaseRenewed(ctx, sess.ID)
	// Only the stop that moved the row counts; a concurrent one, or
	// the reconciler, may have got there first.
	//
	// @decision(DL-011)
	took, err := s.deps.Store.Sessions().MarkStopped(ctx, sess.OrgID, sess.ID, store.StateStopped, t, sess.Error)
	if err != nil {
		return err
	}
	if took {
		recordStop(ctx, s.deps.Metrics, s.deps.Store, sess, t, telemetry.TerminationStopped)
	}
	return nil
}

// Close ends every launch this process started without terminating the
// sandboxes: it releases handles and proxies on shutdown.
func (s *Service) Close() {
	s.pushes.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, live := range s.live {
		_ = live.Detach()
		delete(s.live, id)
	}
	s.passwords.clear()
	// A cached connection is a live unrevocable credential, so it goes
	// with the passwords rather than surviving into whatever reads this
	// process's memory next.
	//
	// @decision(DL-030)
	s.conns.clear()
}

// noteStoredTokens says, once per platform, where a stored token went
// and that spov cannot check what it reaches. A renewable token is
// narrowed by the forge and needs no note.
//
// @decision(DL-043) @decision(DL-034)
func noteStoredTokens(log *launchLog, creds []session.Credential) {
	for _, c := range creds {
		switch {
		case c.Renewable:
		case c.Proxied:
			log.Note(ProxiedTokenNote(c.Platform, c.Forge, c.All))
		default:
			log.Note(StoredTokenNote(c.Platform))
		}
	}
}

// ProxiedKeysNote is the launch log's line naming the model provider
// keys the credential proxy holds.
//
// @decision(DL-034)
func ProxiedKeysNote(names []string) string {
	return "model provider keys held by spov's proxy, which sets each only on its provider's API: " +
		strings.Join(names, ", ")
}

// StoredTokenNote is the launch log's line for a platform whose stored
// token goes into the sandbox unchanged.
//
// @decision(DL-043)
func StoredTokenNote(platform string) string {
	return fmt.Sprintf("the sandbox gets the stored %s token as it is: it reaches whatever it was created to reach, "+
		"and spov cannot check that (scope it to specific repositories when you create it)", platform)
}

// ProxiedTokenNote is the launch log's line for a stored token the
// credential proxy holds. all is git access all, where the proxy uses it
// on any repository.
//
// @decision(DL-034) @decision(DL-043)
func ProxiedTokenNote(platform, forge string, all bool) string {
	if all {
		return fmt.Sprintf("the stored %s token is held by spov's proxy and used on every repository it reaches "+
			"(git access all)", platform)
	}
	graphql := ""
	switch forge {
	case "github":
		graphql = " (except GitHub's GraphQL API, which names no repository)"
	// @decision(DL-034)
	case "gitlab":
		graphql = " (except GitLab's GraphQL API, which names no project)"
	}
	return fmt.Sprintf("the stored %s token is held by spov's proxy and used only for this session's repositories%s, "+
		"but spov cannot check what the token itself reaches (scope it to specific repositories when you create it)",
		platform, graphql)
}

// withTiers records on each credential the tier its platform resolved to.
//
// @decision(DL-042)
func withTiers(creds []store.SessionCredential, tiers []PlatformTier) []store.SessionCredential {
	for i := range creds {
		for _, t := range tiers {
			if t.Platform == creds[i].Platform && (t.Tier == TierUser || t.Tier == TierOrg) {
				creds[i].Tier = string(t.Tier)
			}
		}
	}
	return creds
}

// storedCredentials is the launch's credentials as the session row keeps
// them: the kind, where it is held and how far it reaches, never the
// secret. known is false for an adopted sandbox, whose holder is not
// recorded.
//
// @decision(DL-043) @decision(DL-034)
func storedCredentials(creds []session.Credential, known bool) []store.SessionCredential {
	out := make([]store.SessionCredential, 0, len(creds))
	for _, c := range creds {
		kind := store.CredentialStored
		if c.Renewable {
			kind = store.CredentialRenewed
		}
		var holder store.CredentialHolder
		if known {
			holder = store.CredentialInSandbox
			if c.Proxied {
				holder = store.CredentialInProxy
			}
		}
		access := store.GitAccessRepos
		if c.All {
			access = store.GitAccessAll
		}
		out = append(out, store.SessionCredential{Platform: c.Platform, Kind: kind, Holder: holder, Forge: c.Forge, Access: access})
	}
	return out
}
