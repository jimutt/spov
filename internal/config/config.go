// Package config loads spov configuration from three layers: a user-level
// file, a project-level file and SPOV_* environment variables.
//
// @decision(DL-026)
package config

import (
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/prep"
)

// SchemaVersion is the only config schema version spov currently understands.
const SchemaVersion = 1

// PlatformKind identifies the forge product a git platform runs.
//
// @decision(DL-041)
type PlatformKind string

// The kinds are git's, so the config and the adapters cannot disagree.
//
// @decision(DL-041)
const (
	KindGitHub  PlatformKind = git.KindGitHub
	KindForgejo PlatformKind = git.KindForgejo
	KindGitea   PlatformKind = git.KindGitea
	KindGitLab  PlatformKind = git.KindGitLab
)

// Config is the fully merged spov configuration.
type Config struct {
	Version   int        `koanf:"version"`
	Modal     Modal      `koanf:"modal"`
	Sandbox   Sandbox    `koanf:"sandbox"`
	OpenCode  OpenCode   `koanf:"opencode"`
	Git       Git        `koanf:"git"`
	Platforms []Platform `koanf:"platforms"`
	Repos     []Repo     `koanf:"repos"`
	// Landing names the repo (by Repo.Dir) the OpenCode server starts in.
	// Empty starts it at the workspace root.
	//
	// @decision(DL-037)
	Landing string `koanf:"landing"`
	// Server names the spov server the CLI talks to.
	//
	// @decision(DL-002)
	Server Server `koanf:"server"`
	// Quotas are the hosted server's per-user defaults. They mean
	// nothing to the CLI; `spov serve` is what reads them.
	//
	// @decision(DL-009)
	Quotas Quotas `koanf:"quotas"`
	// Prep configures the control plane's own model call, which
	// prepares a job's goal before its sandbox exists. Empty means the
	// provider is detected from the environment, which is what a
	// zero-configuration deployment gets. It means nothing to the CLI;
	// `spov serve` is what reads it.
	//
	// @decision(DL-047)
	Prep prep.Config `koanf:"prep"`
	// Metrics configures the server's Prometheus listener. OTLP export
	// is configured by the standard OTEL_* variables instead.
	//
	// @decision(DL-011)
	Metrics Metrics `koanf:"metrics"`
}

// Metrics is what `spov serve` exports. Empty exports nothing.
//
// @decision(DL-011)
type Metrics struct {
	// PrometheusListen is the address of a /metrics listener separate
	// from the API's, such as 127.0.0.1:9464.
	PrometheusListen string `koanf:"prometheus_listen" json:"prometheus_listen" yaml:"prometheus_listen"`
}

// Quotas bound what one user may ask a hosted server for. They are the
// org defaults; a user's stored quota overrides any subset of them.
//
// Zero means "use the built-in default" rather than "no limit": a
// deployment that wants a limit lifted raises it rather than removing
// it, since an unbounded default is how a hosted server gets a surprise
// bill.
//
// @decision(DL-009)
type Quotas struct {
	// MaxConcurrentSessions bounds a user's starting + running sessions.
	MaxConcurrentSessions int `koanf:"max_concurrent_sessions" json:"max_concurrent_sessions,omitempty" yaml:"max_concurrent_sessions,omitempty"`
	// MaxConcurrentJobs bounds a user's unfinished autonomous jobs,
	// separately from their sessions.
	//
	// @decision(DL-045)
	MaxConcurrentJobs int `koanf:"max_concurrent_jobs" json:"max_concurrent_jobs,omitempty" yaml:"max_concurrent_jobs,omitempty"`
	// MaxJobDuration caps how long one job may run unattended.
	//
	// @decision(DL-049)
	MaxJobDuration time.Duration `koanf:"max_job_duration" json:"max_job_duration,omitempty" yaml:"max_job_duration,omitempty"`
	// MaxTimeout bounds a requested session lifetime, itself capped by
	// the 12 h hard limit of DL-029.
	MaxTimeout time.Duration `koanf:"max_timeout" json:"max_timeout,omitempty" yaml:"max_timeout,omitempty"`
	// MaxCPU and MaxMemoryMiB bound requested resources.
	MaxCPU       float64 `koanf:"max_cpu" json:"max_cpu,omitempty" yaml:"max_cpu,omitempty"`
	MaxMemoryMiB int     `koanf:"max_memory_mib" json:"max_memory_mib,omitempty" yaml:"max_memory_mib,omitempty"`
}

// Server is where the CLI finds its spov server. Empty means none is
// configured; the flag and SPOV_SERVER take precedence over it.
//
// @decision(DL-002)
type Server struct {
	URL string `koanf:"url" json:"url,omitempty" yaml:"url,omitempty"`
}

// Modal holds Modal account/workspace selection. Tokens are never stored
// here; they come from the credential store or MODAL_TOKEN_* env vars.
type Modal struct {
	Environment string `koanf:"environment" json:"environment,omitempty" yaml:"environment,omitempty"`
	App         string `koanf:"app" json:"app" yaml:"app"`
	// BuildOnDemand lets a launch build an add-on variant nobody has
	// published yet. The base image is never built on demand.
	//
	// @decision(DL-016)
	BuildOnDemand bool `koanf:"build_on_demand" json:"build_on_demand" yaml:"build_on_demand"`
}

// Sandbox holds default sandbox sizing and lifetime.
type Sandbox struct {
	CPU       float64 `koanf:"cpu"`
	MemoryMiB int     `koanf:"memory_mib"`
	// CPULimit and MemoryLimitMiB are hard ceilings; zero means none.
	//
	// @decision(DL-029)
	CPULimit       float64       `koanf:"cpu_limit"`
	MemoryLimitMiB int           `koanf:"memory_limit_mib"`
	Timeout        time.Duration `koanf:"timeout"`
	IdleTimeout    time.Duration `koanf:"idle_timeout"`
	Image          string        `koanf:"image"`
	// Addons are curated add-ons as `name[@version]`, and Apt extra
	// Debian packages; either makes the session's image a variant of the
	// base. Neither combines with Image, which names an image spov did
	// not build.
	//
	// @decision(DL-033)
	Addons []string `koanf:"addons"`
	Apt    []string `koanf:"apt"`
	// Regions and Cloud pin where the sandbox runs; empty lets the
	// provider choose.
	//
	// @decision(DL-029)
	Regions []string `koanf:"regions"`
	Cloud   string   `koanf:"cloud"`
	// Workdir is the directory the entrypoint starts in.
	//
	// @decision(DL-029)
	Workdir string `koanf:"workdir"`
	// Egress names the egress preset; EgressExtraDomains are added to
	// it for a toolchain the presets do not cover.
	//
	// @decision(DL-031) @decision(DL-029)
	Egress             string   `koanf:"egress"`
	EgressExtraDomains []string `koanf:"egress_extra_domains"`
	// InboundCIDRs restricts who may reach the sandbox's edge.
	//
	// @decision(DL-030)
	InboundCIDRs []string `koanf:"inbound_cidrs"`
	// GPU is recognised only to refuse it: spov uses the v2 sandbox
	// backend, which has none. Ignoring it silently would look like a
	// feature that does not work.
	//
	// @decision(DL-029)
	GPU string `koanf:"gpu"`
	// GitAccess and JobGitAccess are the git access `spov fly` and
	// `spov jobs create` ask for when no flag names one: "repos" or "all". The
	// server treats them as preferences.
	//
	// @decision(DL-043)
	GitAccess    string `koanf:"git_access"`
	JobGitAccess string `koanf:"job_git_access"`
	// JobMaxDuration is the wall clock `spov jobs create` asks for when no
	// flag names one. The server treats it as a preference and clamps it
	// to the caller's limit.
	//
	// @decision(DL-049)
	JobMaxDuration time.Duration `koanf:"job_max_duration"`
	// Sync makes `spov fly` seed the sandbox from the current local branch
	// and sync the resulting repository state back on exit when neither
	// --sync nor --no-sync is given.
	//
	// @decision(DL-027)
	Sync bool `koanf:"sync"`
}

// OpenCode pins the OpenCode build used on both ends of a session.
type OpenCode struct {
	Version string `koanf:"version" json:"version" yaml:"version"`
	Port    int    `koanf:"port" json:"port" yaml:"port"`
}

// Git is the identity commits made inside the sandbox are attributed to.
// Empty fields fall back to the launching machine's git config.
//
// @decision(DL-044)
type Git struct {
	Name  string `koanf:"name" json:"name,omitempty" yaml:"name,omitempty"`
	Email string `koanf:"email" json:"email,omitempty" yaml:"email,omitempty"`
	// AllowAll, read by `spov serve` (SPOV_GIT_ALLOW_ALL), lets git access
	// all apply to the deployment's own credentials.
	//
	// @decision(DL-043)
	AllowAll bool `koanf:"allow_all" json:"allow_all,omitempty" yaml:"allow_all,omitempty"`
}

// Platform is a configured git platform account. Credentials are resolved
// by Name from the credential store, never read from config files.
type Platform struct {
	Name    string       `koanf:"name" json:"name" yaml:"name"`
	Kind    PlatformKind `koanf:"kind" json:"kind" yaml:"kind"`
	BaseURL string       `koanf:"base_url" json:"base_url,omitempty" yaml:"base_url,omitempty"`
	Account string       `koanf:"account" json:"account,omitempty" yaml:"account,omitempty"`
	// Prefer is which credential a CLI launch tries first on a hosted
	// server: "personal" (the default, your own) or "shared" (the
	// organisation's). The other is used only when the preferred one is
	// absent. User file only, as platforms are.
	//
	// @decision(DL-042)
	Prefer string `koanf:"prefer" json:"prefer,omitempty" yaml:"prefer,omitempty"`
}

// The two credential preferences a platform may name.
//
// @decision(DL-042)
const (
	PreferPersonal = "personal"
	PreferShared   = "shared"
)

// Repo is one repository to bootstrap into a sandbox.
type Repo struct {
	Platform string `koanf:"platform" json:"platform" yaml:"platform"`
	Owner    string `koanf:"owner" json:"owner" yaml:"owner"`
	Name     string `koanf:"name" json:"name" yaml:"name"`
	// Dir is the directory name under the sandbox workspace root.
	// Empty means use Name.
	Dir string `koanf:"dir" json:"dir,omitempty" yaml:"dir,omitempty"`
}

// Dirname returns the workspace directory name for the repo.
func (r Repo) Dirname() string {
	if r.Dir != "" {
		return r.Dir
	}
	return r.Name
}

// Slug is the "owner/name" identifier of the repo.
func (r Repo) Slug() string {
	return r.Owner + "/" + r.Name
}
