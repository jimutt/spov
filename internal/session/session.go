// Package session wires the sandbox, git and OpenCode packages into one
// launchable session.
//
// @decision(DL-001)
package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/toolchain"
)

// Options describes the session to launch.
type Options struct {
	// Name is the session name; also the sandbox name unless
	// SandboxName is set.
	Name string
	// SandboxName names the sandbox when it is not the session's name:
	// a resumed session's later runs.
	//
	// @decision(DL-087)
	SandboxName string
	// Run is the session's run this sandbox is, for its tag; 0 tags
	// none.
	//
	// @decision(DL-084) @decision(DL-085)
	Run int
	// ExitSnapshot asks for a snapshot of the filesystem when the
	// sandbox ends.
	//
	// @decision(DL-084)
	ExitSnapshot bool
	// SnapshotImage, when set, is the exit snapshot the sandbox boots
	// from, by its ID, instead of Image.
	//
	// @decision(DL-088)
	SnapshotImage string
	// Resume is the entrypoint's resume mode (opencode.ResumeSnapshot or
	// ResumeTranscript); empty for a first launch.
	//
	// @decision(DL-088) @decision(DL-089)
	Resume string
	// User launched the session, recorded as a tag. In hosted mode it
	// is their ULID; in local mode "local".
	//
	// @decision(DL-009)
	User string
	// Org owns the session, recorded as a tag alongside User.
	//
	// @decision(DL-009)
	Org string
	// Image is the published base image.
	Image string
	// Variant is the recipe of Image when it is an add-on variant, and
	// nil for the base image or one spov did not build. BuildVariant
	// lets the launch build it if nobody has.
	//
	// RebuildVariant builds it even though its name resolves, because
	// the image registry says it was deleted.
	//
	// @decision(DL-016)
	Variant        *sandbox.ImageSpec
	BuildVariant   bool
	RebuildVariant bool
	// Addons are the image's add-ons, for the entrypoint.
	//
	// @decision(DL-033)
	Addons []toolchain.Selection
	// Env are the session's own environment variables, set in the main
	// container beside spov's.
	//
	// @decision(DL-096)
	Env map[string]string
	// Selections are the repos to bootstrap, per platform.
	Selections []git.Selection
	// Landing is the workspace directory the server starts in. Empty
	// means the workspace root itself.
	//
	// @decision(DL-037)
	Landing string
	// Model is OpenCode's own provider/model identifier, written into
	// the sandbox's config. Empty leaves the key out.
	//
	// @decision(DL-014)
	Model string
	// ModelUnlisted declares Model in the sandbox's config, for a model
	// spov's list lacks.
	//
	// @decision(DL-038)
	ModelUnlisted bool

	CPU            float64
	MemoryMiB      int
	CPULimit       float64
	MemoryLimitMiB int
	Timeout        time.Duration
	IdleTimeout    time.Duration
	Regions        []string
	Cloud          string
	Workdir        string
	Port           int
	// TokenTTL is requested of platforms that mint expiring tokens.
	TokenTTL time.Duration
	// Egress is what the sandbox may reach. Required.
	//
	// @decision(DL-031)
	Egress sandbox.EgressPolicy
	// InboundCIDRs restricts who may reach the sandbox's edge.
	//
	// @decision(DL-030)
	InboundCIDRs []string
	// ProviderKeys are model provider credentials for the server.
	ProviderKeys map[string]string
	// ProviderAPIs name, for each provider key variable, its provider's
	// API host and header. With the proxy, a key named here is the
	// sidecar's and the main container holds a placeholder.
	//
	// @decision(DL-034)
	ProviderAPIs map[string]credproxy.ProviderAPI
	// Routes are the routed credentials the session holds. They need the
	// proxy: the main container holds only a placeholder for each, and a
	// launch without the proxy refuses them.
	//
	// @decision(DL-072) @decision(DL-077)
	Routes []credproxy.Route
	// Presets are the routes, secret-less, whose preset has a client
	// config the entrypoint writes with the placeholder.
	//
	// @decision(DL-076)
	Presets []routedcred.Route
	// Identity is the author commits made in the sandbox are attributed
	// to. Either half may be empty.
	//
	// @decision(DL-044)
	Identity git.Identity
	// Sync, when set on a single-repository launch, checks out the
	// caller's local branch and commit (applying Bundle when present)
	// inside the cloned repository before the OpenCode server is handed
	// out.
	//
	// @decision(DL-027)
	Sync *SyncSeed
	// Job, when set, makes this an autonomous job's session: the
	// sandbox is tagged with the job's id so a duplicate becomes a
	// re-attach, and the entrypoint writes the spov-goal plugin instead
	// of the interactive permission config.
	//
	// @decision(DL-039) @decision(DL-046) @decision(DL-040)
	Job *JobOptions
}

// SyncSeed describes the local git branch and commit a synced launch
// starts from, plus an optional git bundle of unpushed or full local
// commits. When Slug and Dir are set with no Selections, the repository
// is initialized directly from Bundle inside the sandbox without cloning
// from a forge.
//
// @decision(DL-027)
type SyncSeed struct {
	Branch     string
	BaseCommit string
	Bundle     []byte
	// AllowMissingBase leaves a clone that cannot reach BaseCommit on its
	// default branch instead of failing: a transcript resume's.
	//
	// @decision(DL-098)
	AllowMissingBase bool
	Slug             string
	Dir              string
	Host             string
}

// JobOptions is what a launch needs to know about the job it is running.
//
// @decision(DL-046)
type JobOptions struct {
	// ID is the job, and becomes the sandbox's job tag.
	ID string
	// Kind decides the permission allowlist.
	Kind string
	// Goal is the task text the plugin re-asserts.
	Goal string
	// CheckCommand is the repo's own check, run in the sandbox when the
	// loop stops so DL-050's gate 1 has an exit status rather than the
	// agent's account of one. Empty leaves gate 1 unverified.
	//
	// @decision(DL-050)
	CheckCommand string
}

// Session is a launched, healthy sandbox running an OpenCode server.
type Session struct {
	// Handle is the sandbox.
	Handle sandbox.Handle
	// Info describes it.
	Info sandbox.Info
	// URL is the loopback address every client uses to reach the
	// server. It is served by this process and dies with it.
	//
	// @decision(DL-030)
	URL string
	// Password authenticates to the server. Clients never see it: the
	// proxy presents it upstream on their behalf.
	//
	// @decision(DL-030)
	Password string
	// Health is what the server reported.
	Health opencode.Health
	// Clones lists what was bootstrapped, without credentials.
	Clones []string
	// Landing is the directory the server started in.
	Landing string
	// Credentials says what the sandbox holds for each platform, and
	// RenewableExpiry when the earliest renewable token lapses (zero when
	// none is renewable).
	//
	// @decision(DL-021) @decision(DL-043)
	Credentials     []Credential
	RenewableExpiry time.Time
	// ProxiedKeys are the provider key variables the proxy sidecar holds.
	//
	// @decision(DL-034)
	ProxiedKeys []string
	// ProxyCA is the CA the sandbox trusts for its proxy, for the server
	// to keep so a recreated sidecar can reuse it. Nil without a proxy.
	//
	// @decision(DL-034)
	ProxyCA *ProxyCA
	// ExitSnapshot reports whether the sandbox was asked for an exit
	// snapshot, which only a proxied one is.
	//
	// @decision(DL-084)
	ExitSnapshot bool

	// proxy fronts the sandbox. Closing the session closes it.
	//
	// @decision(DL-030)
	proxy *proxy.Proxy

	revokeTokens func(context.Context) error
}

// Launcher creates sessions. Its dependencies are interfaces so the whole
// flow can be tested without Modal or a git server.
//
// @decision(DL-001)
type Launcher struct {
	Sandboxes sandbox.Provider
	// NewClient builds a client for the server; defaults to the real one.
	NewClient func(url, password string) HealthChecker
	// Now supplies timestamps; defaults to time.Now.
	Now func() time.Time
	// NewPassword generates the session password; defaults to the real
	// generator.
	NewPassword func() (string, error)
	// HealthTimeout bounds the wait for the server to answer.
	HealthTimeout time.Duration
	// Progress receives human-readable progress lines. Nil discards them.
	Progress io.Writer
	// Steps receives structured step transitions for a drawn view. Nil
	// falls back to Progress, so a plain io.Writer still works.
	//
	// @decision(DL-024)
	Steps StepReporter
	// StartProxy brings up the loopback proxy fronting the sandbox. Nil
	// means the real one.
	//
	// @decision(DL-030)
	StartProxy func(ctx context.Context, h sandbox.Handle, port int, password string, warn func(string)) (*proxy.Proxy, error)
	// ProxyImage returns the credential proxy's image. When it is set,
	// every launch with a forge token gets the proxy sidecar and its main
	// container holds placeholders; nil launches with the tokens in the
	// main container.
	//
	// @decision(DL-034)
	ProxyImage func(ctx context.Context) (string, error)
	// RebakeProxyImage bakes the proxy image again after a sidecar failed
	// to start, and returns its name. Nil never re-bakes.
	//
	// @decision(DL-034)
	RebakeProxyImage func(ctx context.Context) (string, error)
	// Variants builds an add-on variant on first use. Nil never builds.
	//
	// @decision(DL-016)
	Variants *VariantBuilder
	// NameFreeInterval is how long the retry waits for a terminated
	// sandbox's name; zero means two seconds. A seam for tests.
	NameFreeInterval time.Duration
}

// Step names a phase of a launch. They are declared here rather than
// passed as strings so a view can lay them out before any of them run.
//
// @decision(DL-024)
type Step string

// The steps are what spov can actually observe. Cloning is not one of
// them: it happens inside the sandbox entrypoint, and the readiness
// probe only passes once the server is already listening, which is
// after the clones. A "cloning" step would be invented, not measured.
const (
	StepTokens  Step = "preparing tokens"
	StepSandbox Step = "starting sandbox"
	StepServer  Step = "waiting for server"
)

// MaxStepNameWidth bounds a step name so that the longest, plus a
// marker and a right-aligned timing, fits the drawn view's status block.
// A longer name would be silently truncated on screen.
//
// @decision(DL-024)
const MaxStepNameWidth = 20

// LaunchSteps is every step, in the order a launch performs them.
//
// @decision(DL-024)
func LaunchSteps() []Step { return []Step{StepTokens, StepSandbox, StepServer} }

// StepReporter receives launch progress as transitions rather than text,
// so a view can show which step is running and how long each took.
//
// @decision(DL-024)
type StepReporter interface {
	// Start marks a step as in progress.
	Start(Step)
	// Done marks the running step complete.
	Done(Step)
	// Note reports something outside the step sequence, such as a
	// warning during cleanup.
	Note(string)
}

// HealthChecker is the part of the OpenCode client a launch needs.
type HealthChecker interface {
	WaitHealthy(ctx context.Context, interval time.Duration) (opencode.Health, error)
}

// defaultHealthTimeout bounds the wait for a healthy server. The spike
// measured 3-4.5s from create to healthy; this absorbs a cold image pull.
//
// @decision(DL-025)
const defaultHealthTimeout = 3 * time.Minute

// Launch creates a sandbox, bootstraps the repos and waits for a healthy
// server. On any failure it tears down whatever it created, so a failed
// launch never leaves a billing sandbox or a live token behind.
//
// @decision(DL-029)
func (l *Launcher) Launch(ctx context.Context, opts Options) (s *Session, err error) {
	// A routed credential has no fallback into the main container, so
	// a launcher without the proxy refuses it before anything is minted.
	//
	// @decision(DL-077)
	if l.ProxyImage == nil && len(opts.Routes) > 0 {
		return nil, ErrRoutesNeedTheProxy
	}
	// @decision(DL-072)
	opts.Egress = opts.Egress.WithRouteHosts(routeHosts(opts.Routes)...)
	password, err := l.newPassword()
	if err != nil {
		return nil, err
	}

	// Before anything is minted: a build is minutes long and may fail.
	//
	// @decision(DL-016)
	if opts.Variant != nil && opts.BuildVariant && l.Variants != nil {
		if err := l.Variants.Ensure(ctx, opts.User, *opts.Variant, opts.RebuildVariant, l.progress); err != nil {
			return nil, err
		}
	}

	l.startStep(StepTokens)
	var (
		bootstrap    git.Result
		revokeTokens func(context.Context) error
	)
	if len(opts.Selections) > 0 || opts.Sync == nil || opts.Sync.Dir == "" {
		bootstrap, revokeTokens, err = git.Bootstrap(ctx, opts.Selections, opts.TokenTTL)
		if err != nil {
			// Bootstrap returns a revoker even on failure, so anything it
			// minted before failing is released here.
			l.cleanupTokens(ctx, revokeTokens)
			return nil, err
		}
		defer func() {
			if err != nil {
				l.cleanupTokens(ctx, revokeTokens)
			}
		}()
	}

	workspace := workspaceRepos(bootstrap, opts.Selections)
	entrypointLanding := opts.Landing
	if len(bootstrap.Clones) == 0 && opts.Sync != nil && opts.Sync.Dir != "" {
		host := opts.Sync.Host
		if host == "" {
			host = "local"
		}
		workspace = []opencode.WorkspaceRepo{{
			Slug: opts.Sync.Slug,
			Dir:  opts.Sync.Dir,
			Host: host,
			CLI:  opencode.CLINone,
		}}
		// The directory is created by applySyncSeed after the entrypoint
		// starts, so the entrypoint starts at /workspace and Session.Landing
		// records the synced repository directory.
		entrypointLanding = ""
	}
	entrypoint := opencode.Entrypoint{
		Clones:    toOpenCodeClones(bootstrap.Clones),
		Landing:   entrypointLanding,
		Port:      opts.Port,
		Workspace: workspace,
		Logins:    cliLogins(bootstrap.Tokens, opts.Selections),
		GitName:   opts.Identity.Name,
		GitEmail:  opts.Identity.Email,
		Model:     opts.Model,
		Addons:    opts.Addons,
		// @decision(DL-038)
		ModelUnlisted: opts.ModelUnlisted,
	}
	// Assigned after the entrypoint exists, so the plugin's directory is
	// the same one the entrypoint cd's to.
	//
	// @decision(DL-039) @decision(DL-037)
	entrypoint.Job = jobPlugin(opts.Job, workspace, entrypoint.WorkingDir())

	cred, err := l.proxyFor(ctx, bootstrap.Tokens, opts)
	if err != nil {
		return nil, err
	}
	entrypoint.Proxied = cred != nil
	// @decision(DL-078)
	if cred != nil {
		opts.Egress = opts.Egress.WithRouteHosts(cred.packageHosts...)
	}
	// @decision(DL-076)
	if cred != nil {
		entrypoint.Files = routedcred.PresetFiles(opts.Presets, func(name string) string {
			return cred.placeholders[credproxy.RouteEnvVar(name)]
		})
	}

	// @decision(DL-088) @decision(DL-089)
	entrypoint.Resume = opts.Resume
	command, err := entrypoint.Command()
	if err != nil {
		return nil, err
	}
	envConfig := opencode.EnvConfig{Password: password, ProviderKeys: opts.ProviderKeys, Addons: opts.Addons}
	env, err := envConfig.Env()
	if err != nil {
		return nil, err
	}
	secrets, err := envConfig.Secrets()
	if err != nil {
		return nil, err
	}
	// The forge CLIs read their tokens from the environment, so the
	// entrypoint script never contains them. With the proxy, what they
	// and OpenCode read is a placeholder, and the token or key is the
	// sidecar's alone.
	//
	// @decision(DL-034)
	for _, tok := range bootstrap.Tokens {
		secrets[gitTokenEnvVar(tok.Platform)] = tok.Secret
	}
	if cred != nil {
		maps.Copy(secrets, cred.placeholders)
		maps.Copy(env, credproxy.MainEnv())
		env[credproxy.EnvMainCA] = cred.caCert
	}
	// Last, so they are checked against everything the launch set.
	//
	// @decision(DL-096) @decision(DL-097)
	if err := opencode.WithUserEnv(env, secrets, opts.Env); err != nil {
		return nil, err
	}

	l.doneStep(StepTokens)
	l.startStep(StepSandbox)
	cloneSlugs := slugs(bootstrap.Clones)
	if len(cloneSlugs) == 0 && opts.Sync != nil && opts.Sync.Slug != "" {
		cloneSlugs = []string{opts.Sync.Slug}
	}
	spec := sandbox.Spec{
		Name:  cmp.Or(opts.SandboxName, opts.Name),
		Image: opts.Image,
		// @decision(DL-084) @decision(DL-088)
		ImageID: opts.SnapshotImage,
		// Only under the proxy, so a snapshot never holds a secret.
		ExitSnapshot: opts.ExitSnapshot && cred != nil,
		Command:      command,
		Env:          env,
		Secrets:      secrets,
		// No exposed ports: the sandbox has no public URL, and is
		// reachable only through a connect token.
		//
		// @decision(DL-030)
		ReadyPort:      opts.Port,
		CPU:            opts.CPU,
		MemoryMiB:      opts.MemoryMiB,
		CPULimit:       opts.CPULimit,
		MemoryLimitMiB: opts.MemoryLimitMiB,
		Timeout:        opts.Timeout,
		IdleTimeout:    opts.IdleTimeout,
		Regions:        opts.Regions,
		Cloud:          opts.Cloud,
		Workdir:        opts.Workdir,
		Tags: sandbox.BuildTags(sandbox.TagSet{
			Name: opts.Name, User: opts.User, Org: opts.Org,
			Repos: cloneSlugs, CreatedAt: l.now(),
			Job: jobID(opts.Job), Proxy: cred != nil, Run: opts.Run,
		}),
		Egress:       opts.Egress,
		InboundCIDRs: opts.InboundCIDRs,
		Sidecar:      cred.sidecarSpec(),
	}
	handle, err := l.Sandboxes.Create(ctx, spec)
	if err != nil && l.shouldRebake(ctx, spec, err) {
		handle, err = l.createAfterRebake(ctx, spec, err)
	}
	if err != nil && opts.Variant != nil {
		err = unpublishedVariant(*opts.Variant, opts.BuildVariant && l.Variants != nil, err)
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			l.cleanupSandbox(ctx, handle)
		}
	}()

	prox, err := l.startProxy(ctx, handle, opts.Port, password)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = prox.Close()
		}
	}()

	l.doneStep(StepSandbox)
	l.startStep(StepServer)
	healthCtx, cancel := context.WithTimeout(ctx, l.healthTimeout())
	defer cancel()

	// Goes through the proxy.
	//
	// @decision(DL-030)
	// No password: the proxy authenticates on the client's behalf.
	health, err := l.newClient(prox.URL, "").WaitHealthy(healthCtx, 250*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}

	// A snapshot already holds the repository as the session left it, so
	// seeding it again would reset the branch to the launch's base and
	// drop every commit made in the sandbox since.
	//
	// @decision(DL-027) @decision(DL-088) @decision(DL-098)
	if opts.Sync != nil && opts.Resume != opencode.ResumeSnapshot {
		seedDir := opts.Sync.Dir
		if len(bootstrap.Clones) == 1 {
			seedDir = bootstrap.Clones[0].Dir
		}
		seed := *opts.Sync
		seed.AllowMissingBase = opts.Resume == opencode.ResumeTranscript
		if seedDir != "" {
			err := applySyncSeed(ctx, handle, seedDir, seed)
			switch {
			case errors.Is(err, ErrSeedBaseMissing) && seed.AllowMissingBase:
				l.progress(fmt.Sprintf("the session started from %s at %s, which is not on the remote, so the workspace is on its default branch"+
					" without those commits; to bring them, stop the session and run spov resume inside the checkout", seed.Branch, shortCommit(seed.BaseCommit)))
			case err != nil:
				return nil, err
			}
		}
	}

	l.doneStep(StepServer)

	info, err := handle.Info(ctx)
	if err != nil {
		return nil, err
	}

	sessionLanding := entrypoint.WorkingDir()
	if len(bootstrap.Clones) == 0 && opts.Landing != "" {
		sessionLanding = path.Join(opencode.WorkspaceRoot, opts.Landing)
	}

	return &Session{
		Handle:          handle,
		Info:            info,
		URL:             prox.URL,
		Password:        password,
		Health:          health,
		Clones:          cloneSlugs,
		Landing:         sessionLanding,
		Credentials:     credentialsOf(bootstrap.Tokens, cred != nil, platformKinds(opts.Selections)),
		ProxiedKeys:     cred.keyNames(),
		ProxyCA:         cred.ca(),
		ExitSnapshot:    spec.ExitSnapshot,
		RenewableExpiry: renewableExpiry(bootstrap.Tokens),
		proxy:           prox,
		revokeTokens:    revokeTokens,
	}, nil
}

// Credential is what a sandbox was given for one platform: a token the
// server renews, or a stored one.
//
// @decision(DL-043)
type Credential struct {
	Platform  string
	Renewable bool
	// Proxied says the token is held by the proxy sidecar.
	//
	// @decision(DL-034)
	Proxied bool
	// Forge is the platform's kind.
	Forge string
	// All says the token reaches every repository the credential does.
	//
	// @decision(DL-043)
	All bool
}

func credentialsOf(tokens []git.PlatformToken, proxied bool, kinds map[string]string) []Credential {
	out := make([]Credential, 0, len(tokens))
	for _, t := range tokens {
		// proxyFor holds only a token that exists.
		held := proxied && t.Secret != ""
		out = append(out, Credential{Platform: t.Platform, Renewable: t.Renewable, Proxied: held, Forge: kinds[t.Platform], All: t.All})
	}
	return out
}

// renewableExpiry is when the first renewable token lapses, which is
// when the session's re-mint falls due.
//
// @decision(DL-021)
func renewableExpiry(tokens []git.PlatformToken) time.Time {
	var first time.Time
	for _, t := range tokens {
		if !t.Renewable || t.ExpiresAt.IsZero() {
			continue
		}
		if first.IsZero() || t.ExpiresAt.Before(first) {
			first = t.ExpiresAt
		}
	}
	return first
}

// Close terminates the sandbox and revokes the session's git tokens.
//
// @decision(DL-029)
func (s *Session) Close(ctx context.Context) error {
	s.closeProxy()
	var firstErr error
	if s.Handle != nil {
		if err := s.Handle.Terminate(ctx); err != nil {
			firstErr = err
		}
	}
	if s.revokeTokens != nil {
		if err := s.revokeTokens(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CloseProxy stops the session's loopback proxy and nothing else. The
// server uses it once a launch is healthy: the proxy it started exists
// only to prove the path, and the client that attaches runs its own.
//
// @decision(DL-005)
func (s *Session) CloseProxy() { s.closeProxy() }

// Detach leaves the sandbox running and releases local resources. Tokens
// are deliberately not revoked: the session is still using them.
//
// @decision(DL-029)
func (s *Session) Detach() error {
	s.closeProxy()
	if s.Handle == nil {
		return nil
	}
	return s.Handle.Detach()
}

// cleanupSandbox terminates a sandbox after a failed launch. It uses a
// context detached from the caller's, so a cancelled launch still stops
// the billing.
//
// @decision(DL-029)
func (l *Launcher) cleanupSandbox(ctx context.Context, h sandbox.Handle) {
	if h == nil {
		return
	}
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := h.Terminate(cleanCtx); err != nil {
		l.progress(fmt.Sprintf("warning: could not terminate the sandbox after a failed launch: %v", err))
	}
}

func (l *Launcher) cleanupTokens(ctx context.Context, revoke func(context.Context) error) {
	if revoke == nil {
		return
	}
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := revoke(cleanCtx); err != nil {
		l.progress(fmt.Sprintf("warning: could not revoke git tokens: %v", err))
	}
}

// startProxy brings up the proxy for a launched sandbox.
//
// @decision(DL-030)
func (l *Launcher) startProxy(ctx context.Context, h sandbox.Handle, port int, password string) (*proxy.Proxy, error) {
	start := l.StartProxy
	if start == nil {
		start = StartProxy
	}
	return start(ctx, h, port, password, l.progress)
}

func (l *Launcher) newPassword() (string, error) {
	if l.NewPassword != nil {
		return l.NewPassword()
	}
	return opencode.GeneratePassword()
}

func (l *Launcher) newClient(url, password string) HealthChecker {
	if l.NewClient != nil {
		return l.NewClient(url, password)
	}
	return opencode.NewClient(url, password)
}

func (l *Launcher) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Launcher) healthTimeout() time.Duration {
	if l.HealthTimeout > 0 {
		return l.HealthTimeout
	}
	return defaultHealthTimeout
}

// progress reports a note outside the step sequence.
func (l *Launcher) progress(msg string) {
	if l.Steps != nil {
		l.Steps.Note(msg)
		return
	}
	if l.Progress == nil {
		return
	}
	_, _ = fmt.Fprintln(l.Progress, msg)
}

// startStep and doneStep drive a view when one is attached, and fall
// back to the plain progress lines when it is not.
//
// @decision(DL-024)
func (l *Launcher) startStep(step Step) {
	if l.Steps != nil {
		l.Steps.Start(step)
		return
	}
	if l.Progress != nil {
		_, _ = fmt.Fprintf(l.Progress, "%s...\n", step)
	}
}

func (l *Launcher) doneStep(step Step) {
	if l.Steps != nil {
		l.Steps.Done(step)
	}
}

// toOpenCodeClones converts bootstrap output into entrypoint clones.
func toOpenCodeClones(clones []git.Clone) []opencode.Clone {
	out := make([]opencode.Clone, len(clones))
	for i, c := range clones {
		out[i] = opencode.Clone{URL: c.URL, Dir: c.Dir}
	}
	return out
}

// slugs lists the repo identifiers, which are safe to log and to tag.
func slugs(clones []git.Clone) []string {
	out := make([]string, len(clones))
	for i, c := range clones {
		out[i] = c.Slug
	}
	return out
}

// gitTokenEnvVar names the environment variable holding a platform's
// token inside the sandbox.
//
// @decision(DL-032)
func gitTokenEnvVar(platform string) string {
	var b strings.Builder
	b.WriteString("SPOV_GIT_TOKEN_")
	for _, r := range strings.ToUpper(platform) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// workspaceRepos describes the cloned repositories for the generated
// AGENTS.md, pairing each with the CLI that talks to its platform.
//
// @decision(DL-032)
func workspaceRepos(bootstrap git.Result, selections []git.Selection) []opencode.WorkspaceRepo {
	kinds := platformKinds(selections)
	out := make([]opencode.WorkspaceRepo, 0, len(bootstrap.Clones))
	for _, c := range bootstrap.Clones {
		out = append(out, opencode.WorkspaceRepo{
			Slug:     c.Slug,
			Dir:      c.Dir,
			Platform: c.Platform,
			Host:     c.Host,
			CLI:      opencode.CLIForKind(kinds[c.Platform]),
		})
	}
	return out
}

// cliLogins builds the forge CLI logins, skipping platforms spov has no
// CLI for.
//
// @decision(DL-032)
func cliLogins(tokens []git.PlatformToken, selections []git.Selection) []opencode.CLILogin {
	kinds := platformKinds(selections)
	_, bases := proxyRepos(selections)
	var out []opencode.CLILogin
	for _, t := range tokens {
		cli := opencode.CLIForKind(kinds[t.Platform])
		if cli == opencode.CLINone {
			continue
		}
		login := opencode.CLILogin{
			CLI:      cli,
			Name:     t.Platform,
			URL:      "https://" + t.Host,
			User:     t.Username,
			TokenEnv: gitTokenEnvVar(t.Platform),
		}
		// @decision(DL-032)
		if cli == opencode.CLIGlab {
			login.BasePath = bases[t.Platform]
		}
		out = append(out, login)
	}
	return out
}

// platformKinds maps platform names to their kind, which decides the CLI.
func platformKinds(selections []git.Selection) map[string]string {
	kinds := make(map[string]string, len(selections))
	for _, sel := range selections {
		if sel.Platform != nil {
			kinds[sel.Platform.Name()] = sel.Platform.Kind()
		}
	}
	return kinds
}

// proxyRepos maps each platform to the session's repositories on it, as
// "owner/name", and to the path its clone URLs sit under, which is a
// Forgejo's base path. A platform with no repositories, which only a
// selection for all of them has, takes the path from the platform.
//
// @decision(DL-034) @decision(DL-043)
func proxyRepos(selections []git.Selection) (map[string][]string, map[string]string) {
	repos := map[string][]string{}
	bases := map[string]string{}
	for _, sel := range selections {
		if sel.Platform == nil {
			continue
		}
		name := sel.Platform.Name()
		if b, ok := sel.Platform.(interface{ BasePath() string }); ok && len(sel.Repos) == 0 {
			bases[name] = b.BasePath()
		}
		for _, r := range sel.Repos {
			repos[name] = append(repos[name], r.Slug())
			if _, ok := bases[name]; !ok {
				bases[name] = cloneBase(r)
			}
		}
	}
	return repos, bases
}

// cloneBase is the path in r's clone URL before /owner/name, empty when
// the URL cannot be read that way.
func cloneBase(r git.Repo) string {
	u, err := url.Parse(r.CloneURL)
	if err != nil {
		return ""
	}
	p := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	tail := "/" + r.Owner + "/" + r.Name
	if len(p) < len(tail) || !strings.EqualFold(p[len(p)-len(tail):], tail) {
		return ""
	}
	return p[:len(p)-len(tail)]
}

// shouldRebake reports whether a failed Create is one a fresh proxy image
// might fix: the sidecar did not start, and nothing is left running.
//
// @decision(DL-034)
func (l *Launcher) shouldRebake(ctx context.Context, spec sandbox.Spec, err error) bool {
	return spec.Sidecar != nil && l.RebakeProxyImage != nil && ctx.Err() == nil &&
		errors.Is(err, sandbox.ErrSidecarStart) && !errors.Is(err, sandbox.ErrStillRunning)
}

// createAfterRebake re-bakes the proxy image and creates the sandbox once
// more. A failed re-bake reports the original failure beside its own.
//
// @decision(DL-034)
func (l *Launcher) createAfterRebake(ctx context.Context, spec sandbox.Spec, cause error) (sandbox.Handle, error) {
	l.progress("the proxy sidecar did not start; baking its image again and retrying once")
	image, err := l.RebakeProxyImage(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w (baking the proxy image again also failed: %w)", cause, err)
	}
	sc := *spec.Sidecar
	sc.Image = image
	spec.Sidecar = &sc
	h, err := l.Sandboxes.Create(ctx, spec)
	// The failed sandbox holds the name for a few seconds after it was
	// terminated (spike S-I4).
	for wait := 0; err != nil && errors.Is(err, sandbox.ErrNameInUse) && wait < nameFreeWaits; wait++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(l.nameFreeInterval()):
		}
		h, err = l.Sandboxes.Create(ctx, spec)
	}
	if err != nil {
		return nil, fmt.Errorf("%w (after baking the proxy image again; the first attempt: %w)", err, cause)
	}
	return h, nil
}

// nameFreeWaits bounds how often the retry waits for the failed
// sandbox's name, NameFreeInterval apart.
const nameFreeWaits = 15

// nameFreeInterval is the wait between those attempts.
func (l *Launcher) nameFreeInterval() time.Duration {
	if l.NameFreeInterval > 0 {
		return l.NameFreeInterval
	}
	return 2 * time.Second
}

// ErrNoSession is returned when a named session cannot be found.
var ErrNoSession = errors.New("session: not found")

// jobPlugin builds the sandbox plugin for a job launch, and nil for an
// interactive one.
//
// @decision(DL-039) @decision(DL-040)
func jobPlugin(j *JobOptions, workspace []opencode.WorkspaceRepo, dir string) *opencode.Plugin {
	if j == nil {
		return nil
	}
	return &opencode.Plugin{
		Kind:         j.Kind,
		Goal:         j.Goal,
		CheckCommand: j.CheckCommand,
		Permissions:  opencode.PermissionsForKind(j.Kind),
		Workspace:    workspace,
		// The SAME directory the entrypoint cd's to, so the check's cwd
		// cannot disagree with where the server actually starts.
		//
		// @decision(DL-037)
		Dir: dir,
	}
}

// jobID is the job this launch runs, empty for an interactive session.
//
// @decision(DL-046)
func jobID(j *JobOptions) string {
	if j == nil {
		return ""
	}
	return j.ID
}

// credentialProxy is one launch's proxy sidecar: what the sidecar is
// given, and what the main container holds instead of the tokens.
//
// @decision(DL-034)
type credentialProxy struct {
	sidecar      sandbox.Sidecar
	caCert       string
	caKey        string
	placeholders map[string]string
	keys         []string
	// packageHosts are the forges' package hosts a token is set on,
	// which the main container's egress must allow too.
	//
	// @decision(DL-078)
	packageHosts []string
	// reused is set when the CA is one given to buildProxy.
	reused bool
}

// ProxyCA is a sandbox's proxy CA, as PEM.
//
// @decision(DL-034)
type ProxyCA struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// ca is the proxy's CA, or nil without a proxy.
//
// @decision(DL-034)
func (p *credentialProxy) ca() *ProxyCA {
	if p == nil {
		return nil
	}
	return &ProxyCA{Cert: p.caCert, Key: p.caKey}
}

// keyNames are the provider key variables the sidecar holds.
//
// @decision(DL-034)
func (p *credentialProxy) keyNames() []string {
	if p == nil {
		return nil
	}
	return p.keys
}

// sidecarSpec is the sidecar to create, or nil without a proxy.
func (p *credentialProxy) sidecarSpec() *sandbox.Sidecar {
	if p == nil {
		return nil
	}
	sc := p.sidecar
	return &sc
}

// proxyFor builds the launch's proxy sidecar, or returns nil when the
// launcher has no proxy image or the launch holds no forge token.
//
// @decision(DL-034)
func (l *Launcher) proxyFor(ctx context.Context, tokens []git.PlatformToken, opts Options) (*credentialProxy, error) {
	if l.ProxyImage == nil {
		return nil, nil
	}
	return buildProxy(ctx, l.ProxyImage, tokens, opts.Selections, KeyProviders(opts.ProviderKeys, opts.ProviderAPIs),
		opts.Routes, opts.Egress, l.now(), nil)
}

// routeHosts are every host of routes, in order.
func routeHosts(routes []credproxy.Route) []string {
	var out []string
	for _, r := range routes {
		out = append(out, r.Hosts...)
	}
	return out
}

// ErrRoutesNeedTheProxy refuses a launch holding routed credentials on a
// launcher without the proxy.
//
// @decision(DL-077)
var ErrRoutesNeedTheProxy = errors.New("session: routed credentials need the credential proxy, which this build lacks")

// KeyProviders are the provider keys the proxy holds: each key whose
// variable has an API, trimmed, in variable order. A key without one
// stays in the main container.
//
// @decision(DL-034)
func KeyProviders(keys map[string]string, apis map[string]credproxy.ProviderAPI) []credproxy.Provider {
	var out []credproxy.Provider
	for _, env := range slices.Sorted(maps.Keys(keys)) {
		api, ok := apis[env]
		key := strings.TrimSpace(keys[env])
		if !ok || key == "" {
			continue
		}
		out = append(out, credproxy.Provider{Env: env, ProviderAPI: api, Key: key})
	}
	return out
}

// ProxySidecar is a proxy sidecar for a sandbox that already runs, and
// the CA certificate its main container must trust. Reused says the CA
// is the one the sandbox already trusts, so there is nothing to install.
//
// @decision(DL-034)
type ProxySidecar struct {
	Sidecar sandbox.Sidecar
	CACert  string
	Reused  bool
	// CA is the sidecar's CA, for the server to keep.
	CA *ProxyCA
}

// NewProxySidecar builds a proxy sidecar for tokens and provider keys,
// as a launch does, for a sandbox whose sidecar has exited. It reuses ca,
// the sandbox's own, when that covers every host the sidecar decrypts,
// and makes a new CA otherwise. Nil when there is neither a token nor a
// key.
//
// @decision(DL-034)
func NewProxySidecar(ctx context.Context, image func(context.Context) (string, error), tokens []git.PlatformToken,
	selections []git.Selection, providers []credproxy.Provider, routes []credproxy.Route, egress sandbox.EgressPolicy,
	now time.Time, ca *ProxyCA,
) (*ProxySidecar, error) {
	p, err := buildProxy(ctx, image, tokens, selections, providers, routes, egress, now, ca)
	if err != nil || p == nil {
		return nil, err
	}
	return &ProxySidecar{Sidecar: p.sidecar, CACert: p.caCert, Reused: p.reused, CA: p.ca()}, nil
}

// buildProxy makes the sidecar's config from the tokens and provider
// keys, and the CA for this sandbox alone, naming only the forge and
// provider hosts.
//
// @decision(DL-034)
func buildProxy(ctx context.Context, imageFor func(context.Context) (string, error), tokens []git.PlatformToken,
	selections []git.Selection, providers []credproxy.Provider, routes []credproxy.Route, egress sandbox.EgressPolicy,
	now time.Time, reuse *ProxyCA,
) (*credentialProxy, error) {
	kinds := platformKinds(selections)
	repos, bases := proxyRepos(selections)
	// Only a stored token the proxy narrows needs the IDs.
	//
	// @decision(DL-034)
	narrowed := map[string]bool{}
	for _, t := range tokens {
		narrowed[t.Platform] = t.Secret != "" && !t.Renewable && !t.All
	}
	ids := map[string][]string{}
	for _, sel := range selections {
		if sel.Platform != nil && narrowed[sel.Platform.Name()] {
			ids[sel.Platform.Name()] = git.ProjectIDs(ctx, sel)
		}
	}
	// A recreated sidecar is given the egress without its routes' hosts,
	// so they are added here as they are at launch.
	//
	// @decision(DL-072)
	egress = egress.WithRouteHosts(routeHosts(routes)...)
	cfg := credproxy.SidecarConfig{}
	placeholders := make(map[string]string, len(tokens))
	for _, t := range tokens {
		// A platform with no token, such as public repositories only,
		// has nothing for the proxy to hold.
		if t.Secret == "" {
			continue
		}
		// A renewable token is a GitHub App's, which GitHub narrows to
		// the session's repositories itself.
		//
		// @decision(DL-034)
		cfg.Platforms = append(cfg.Platforms, credproxy.Platform{
			Name: t.Platform, Kind: kinds[t.Platform], Host: t.Host, User: t.Username, Token: t.Secret,
			Scoped: t.Renewable, Repos: repos[t.Platform], BasePath: bases[t.Platform],
			// @decision(DL-043)
			AllRepos: t.All,
			// @decision(DL-034)
			ProjectIDs: ids[t.Platform],
		})
		ph, err := credproxy.Placeholder(t.Secret)
		if err != nil {
			return nil, err
		}
		placeholders[gitTokenEnvVar(t.Platform)] = ph
	}
	var keys []string
	for _, p := range providers {
		ph, err := credproxy.KeyPlaceholder(p.Key)
		if err != nil {
			return nil, err
		}
		cfg.Providers = append(cfg.Providers, p)
		placeholders[p.Env] = ph
		keys = append(keys, p.Env)
	}
	// @decision(DL-072)
	for _, r := range routes {
		ph, err := credproxy.Placeholder(r.Secret)
		if err != nil {
			return nil, err
		}
		cfg.Routes = append(cfg.Routes, r)
		placeholders[credproxy.RouteEnvVar(r.Name)] = ph
	}
	// @decision(DL-078)
	var packageHosts []string
	for _, p := range cfg.Platforms {
		packageHosts = append(packageHosts, p.PackageHosts()...)
	}
	egress = egress.WithRouteHosts(packageHosts...)
	if len(cfg.Platforms) == 0 && len(cfg.Providers) == 0 && len(cfg.Routes) == 0 {
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	image, err := imageFor(ctx)
	if err != nil {
		return nil, fmt.Errorf("session: the credential proxy's image: %w", err)
	}
	// @decision(DL-034)
	var caCert, caKey []byte
	reused := reuse != nil && credproxy.CACovers([]byte(reuse.Cert), cfg.CAHosts(), now)
	if reused {
		caCert, caKey = []byte(reuse.Cert), []byte(reuse.Key)
	} else if caCert, caKey, err = credproxy.NewCA(cfg.CAHosts(), now); err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	config, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return &credentialProxy{
		sidecar: sandbox.Sidecar{
			Name:    credproxy.SidecarName,
			Image:   image,
			Command: []string{credproxy.BinaryPath},
			Env:     map[string]string{credproxy.EnvCACert: string(caCert)},
			Secrets: map[string]string{credproxy.EnvCAKey: string(caKey), credproxy.EnvConfig: string(config)},
			Egress:  egress,
		},
		caCert:       string(caCert),
		caKey:        string(caKey),
		placeholders: placeholders,
		keys:         keys,
		packageHosts: packageHosts,
		reused:       reused,
	}, nil
}
