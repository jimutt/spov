package server

import (
	"context"
	"io/fs"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
	"github.com/jimutt/spov/internal/toolchain"
	"github.com/jimutt/spov/internal/webui"
)

// Forge resolves repositories on the configured git platforms. The CLI's
// picker logic satisfies it; tests use a fake.
//
// @decision(DL-005)
type Forge interface {
	// ListRepos lists every repository across every configured platform.
	// A platform that could not be listed is reported as a warning
	// rather than failing the whole listing; the error is for when no
	// platform answered.
	//
	// @decision(DL-028) @decision(DL-002)
	ListRepos(ctx context.Context) (repos []git.PlatformRepo, warnings []string, err error)
	// Selections turns repository references into per-platform selections
	// with live platform clients, ready for git.Bootstrap.
	Selections(ctx context.Context, refs []store.RepoRef) ([]git.Selection, error)
	// Platforms are the configured platforms the caller holds a
	// credential for, which a launch with git access all reaches.
	//
	// @decision(DL-043)
	Platforms(ctx context.Context) ([]git.Platform, error)
}

// AddonResolver resolves partial add-on versions. addonindex.Resolver is
// the real one.
//
// @decision(DL-033)
type AddonResolver interface {
	Resolve(ctx context.Context, sel []toolchain.Selection) []toolchain.Selection
}

// ModelCatalogue is the models a launch may name and the picker offers,
// and whether the list was fetched (true) or is the shipped snapshot.
// modelcat.Source is the real one.
//
// @decision(DL-013)
type ModelCatalogue interface {
	Models() (modelcat.List, bool)
}

// fixedModels is a ModelCatalogue that never changes.
type fixedModels struct {
	list modelcat.List
	live bool
}

func (f fixedModels) Models() (modelcat.List, bool) { return f.list, f.live }

// FixedModels is a catalogue of exactly this list, reported as fetched.
func FixedModels(l modelcat.List) ModelCatalogue { return fixedModels{list: l, live: true} }

// Launch starts a session and reports its steps. session.Launcher is the
// real one; tests substitute it.
type Launch func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error)

// Attach reconnects to a running sandbox. session.Attacher is the real one.
type Attach func(ctx context.Context, opts session.AttachOptions) (*session.Session, error)

// Deps is everything the server needs. Every field is an interface or a
// function so the whole server runs against fakes in tests.
//
// @decision(DL-005)
type Deps struct {
	Store    store.Store
	Provider sandbox.Provider
	Forge    Forge
	Launch   Launch
	Attach   Attach
	Auth     auth.Authenticator
	// Defaults fill a SessionRequest's unset fields.
	Defaults Defaults
	// ProviderKeys are model provider credentials passed to every sandbox.
	ProviderKeys map[string]string
	// Identity is the git author configured in every sandbox.
	Identity git.Identity
	// ProxyImage is the credential proxy's image, for recreating a
	// sidecar that has exited. Nil recreates none.
	//
	// @decision(DL-034)
	ProxyImage func(ctx context.Context) (string, error)
	// ProxyImageName is the current proxy image's name, which pruning
	// never deletes. Empty when there is no proxy binary.
	//
	// @decision(DL-016)
	ProxyImageName string
	// Snapshots, when set, gives every interactive sandbox under the
	// proxy an exit snapshot and keeps it as these settings say. Nil
	// takes none: snapshots are off, there is no proxy, or the server
	// keeps its sessions in memory and could not resume them.
	//
	// @decision(DL-084) @decision(DL-086)
	Snapshots *SnapshotSettings
	// Images is the image registry. Nil keeps none, and a launch boots
	// the variant its request names.
	//
	// @decision(DL-016)
	Images store.ImageStore
	// Resolver resolves partial add-on versions to exact ones. Nil
	// resolves none.
	//
	// @decision(DL-033)
	Resolver AddonResolver
	// OrgApps holds the org's stored GitHub Apps, and OrgGitHub the
	// deployment's GitHub platforms and a check of an App. Either nil
	// turns the admin's App routes off (local mode).
	//
	// @decision(DL-042)
	OrgApps   *OrgApps
	OrgGitHub OrgGitHub
	// Models is the model list. Nil is the shipped snapshot.
	//
	// @decision(DL-013)
	Models ModelCatalogue
	// Builder asks Modal about published images and deletes pruned ones;
	// Variants is the launcher's builder, which builds in the background
	// too. Nil for a provider that cannot build.
	//
	// @decision(DL-016)
	Builder  sandbox.ImageBuilder
	Variants *session.VariantBuilder
	// User is recorded as the sandbox's user tag.
	User string
	// Now supplies timestamps; nil means time.Now.
	Now func() time.Time
	// Warn receives one line per problem; nil discards.
	Warn func(string)
	// Metrics records the control plane's metrics; nil records nothing.
	//
	// @decision(DL-011)
	Metrics *telemetry.Metrics
	// LaunchTimeout bounds a launch from request to healthy. Zero means
	// config.DefaultLaunchTimeout.
	LaunchTimeout time.Duration
	// StorePoll is how often a replica that did not run a launch
	// re-reads the session while streaming its events. Zero means
	// StorePollInterval.
	//
	// It is a seam so a test can exercise the polling without paying
	// its interval in real time: the interval is a production-latency
	// choice, and a test that waits it out is measuring the clock
	// rather than the handler.
	//
	// @decision(DL-003)
	StorePoll time.Duration
	// PublicURL is the externally visible origin, when the server sits
	// behind a proxy. Empty means the listen address is the origin.
	//
	// @decision(DL-010)
	PublicURL string
	// Ready, when set, is consulted by /readyz in addition to the
	// store's ping: hosted mode reports not-ready until its migrations
	// are at head.
	//
	// @decision(DL-010)
	Ready func(context.Context) error
	// Jobs, when set, supervises autonomous jobs. Nil in local mode:
	// the supervisor is a River worker and River needs Postgres, while
	// local mode is one process against no database. Local autonomous
	// jobs are a non-goal, so a local server refuses a job and says why.
	//
	// @decision(DL-046)
	Jobs *jobs.Supervisor
	// Login, when set, serves the browser login routes and carries the
	// session manager whose cookie the CookieAuthenticator reads. Nil
	// in local mode, where the bearer token is the only credential.
	//
	// @decision(DL-007)
	Login *Login
	// Device, when set, serves the CLI device flow: the endpoints a
	// terminal polls and the page a human approves on. Nil in local
	// mode, which needs no login at all.
	//
	// @decision(DL-006)
	Device *Device
	// BrowserAuth resolves *only* a browser session, and is what the
	// approval page authenticates with. Deliberately narrower than
	// Auth, so a CLI token cannot approve a device flow.
	//
	// @decision(DL-006)
	BrowserAuth auth.Authenticator
	// Credentials holds each user's own platform credentials, sealed.
	// Nil, or present with no sealer, means this deployment has no
	// user tier and every session uses the org credentials below.
	//
	// @decision(DL-008)
	Credentials *CredentialStore
	// RoutedCredentials keeps each user's routed credentials. Nil means
	// the deployment can hold none: a hosted server without a credential
	// key.
	//
	// @decision(DL-073)
	RoutedCredentials RoutedCredentialSource
	// OrgRoutedCredentials keeps the organisation's routed credentials,
	// which an admin defines. Nil means the deployment has none: a local
	// server, or a hosted one without a credential key.
	//
	// @decision(DL-074)
	OrgRoutedCredentials *SealedOrgRoutedCredentials
	// Verifier checks a credential against its platform before it is
	// stored. Nil stores whatever is given, unverified.
	//
	// @decision(DL-008)
	Verifier CredentialVerifier
	// UserForge resolves repositories using one user's own credentials,
	// falling back to the org Forge above. Nil means every user gets
	// the org Forge, which is local mode and any hosted deployment
	// where nobody has stored a credential.
	//
	// @decision(DL-042)
	//
	// The policy says which tier each platform may resolve to (DL-042).
	UserForge func(ctx context.Context, orgID, userID string, policy TierPolicy) (Forge, []PlatformTier, error)
	// UserRepos lists the repositories each of one user's credential
	// tiers reaches, platform by platform. Nil lists through UserForge
	// alone (local mode).
	//
	// @decision(DL-042)
	UserRepos func(ctx context.Context, orgID, userID string) ([]TieredRepo, []string, error)
	// Console is the OpenCode Console's device flow, and ConsoleGrants
	// where each user's sign-in is kept: the local credential store in
	// local mode, the sealed store in hosted mode. Either nil turns the
	// OpenCode sign-in off.
	//
	// @decision(DL-015)
	Console       ConsoleClient
	ConsoleGrants ConsoleStore
	// UserProviderKeys resolves model provider keys for one user, the
	// user's own overriding the deployment's per variable.
	//
	// @decision(DL-042)
	UserProviderKeys func(ctx context.Context, orgID, userID string) (map[string]string, error)
	// GitAllowAll lets git access all apply to the deployment's own
	// credentials, not only to a user's. Local mode sets it, since every
	// credential there is the user's.
	//
	// @decision(DL-043)
	GitAllowAll bool
	// Quotas are the org's per-user limits. The zero value means the
	// built-in defaults; a user's stored quota overrides any subset.
	//
	// @decision(DL-009)
	Quotas Quotas
	// WebUI is the built web app. Nil means the copy embedded in this
	// binary, which is a placeholder unless the build ran
	// scripts/build-web.sh first.
	//
	// @decision(DL-055)
	WebUI fs.FS
}

// TieredRepo is a repository and the tier whose credential listed it.
//
// @decision(DL-042)
type TieredRepo struct {
	git.PlatformRepo
	Tier Tier
	// MaybeReadOnly is set on a public repository a fine-grained token
	// lists: it can read any, and push only to one it was given, which
	// GitHub does not say.
	//
	// @decision(DL-042)
	MaybeReadOnly bool
}

// PlatformTier records which tier supplied one platform's credential.
// It travels to the client and onto the session; the credential does
// not.
//
// @decision(DL-042)
type PlatformTier struct {
	Platform string `json:"platform"`
	Tier     Tier   `json:"tier"`
	// Reason explains a TierNone, so a user missing a platform is told
	// what to do rather than just shown a short list.
	Reason string `json:"reason,omitempty"`
}

// Defaults are the server's sandbox defaults, from its configuration.
type Defaults struct {
	Image string
	// OpenCodeVersion is the OpenCode release this deployment pins,
	// published unauthenticated at OpenCodePinPath.
	//
	// @decision(DL-035)
	OpenCodeVersion string
	Port            int
	Sandbox         store.SandboxConfig
	// Egress is the preset name a request that names none gets.
	//
	// @decision(DL-031)
	Egress string
	// EgressExtraDomains are added to whatever preset is chosen, for a
	// deployment whose toolchain needs a host the presets lack.
	//
	// @decision(DL-029)
	EgressExtraDomains []string
	// BuildOnDemand lets a launch build an add-on variant nobody has
	// published.
	//
	// @decision(DL-016)
	BuildOnDemand bool
	// Addons and Apt are the deployment's own, applied to a request that
	// names neither and no image.
	//
	// @decision(DL-033)
	Addons []string
	Apt    []string
}

// NewLaunch adapts a session.Launcher.
func NewLaunch(l *session.Launcher) Launch {
	return func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error) {
		launcher := *l
		launcher.Steps = steps
		launcher.Progress = nil
		return launcher.Launch(ctx, opts)
	}
}

// NewAttach adapts a session.Attacher.
func NewAttach(a *session.Attacher) Attach {
	return func(ctx context.Context, opts session.AttachOptions) (*session.Session, error) {
		return a.Attach(ctx, opts)
	}
}

// withDefaults fills in what a caller may leave zero. It is idempotent,
// so both the server and the service it owns can apply it.
//
// @decision(DL-005)
func (d Deps) withDefaults() Deps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Warn == nil {
		d.Warn = func(string) {}
	}
	if d.LaunchTimeout == 0 {
		d.LaunchTimeout = config.DefaultLaunchTimeout
	}
	if d.WebUI == nil {
		d.WebUI = webui.Dist()
	}
	if d.Models == nil {
		d.Models = fixedModels{list: modelcat.Snapshot()}
	}
	return d
}
