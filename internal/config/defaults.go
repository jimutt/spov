package config

import (
	"time"

	"github.com/jimutt/spov/internal/sandbox"
)

// Defaults returns the baseline configuration, the lowest layer of the
// merge order.
//
// @decision(DL-026)
func Defaults() Config {
	return Config{
		Version: SchemaVersion,
		Modal: Modal{
			App: "spov",
			// @decision(DL-016)
			BuildOnDemand: true,
		},
		Sandbox: Sandbox{
			CPU:         0.5,
			MemoryMiB:   2048,
			Timeout:     4 * time.Hour,
			IdleTimeout: 30 * time.Minute,
			// An interactive session is driven by someone at a
			// keyboard; a toolchain that cannot reach a registry is a
			// support problem rather than a security win. Autonomous
			// jobs will not default to this.
			//
			// @decision(DL-031)
			Egress: sandbox.PresetOpen,
		},
		OpenCode: OpenCode{
			Version: DefaultOpenCodeVersion,
			Port:    4096,
		},
		// @decision(DL-086)
		Snapshots: Snapshots{Enabled: true, Retention: 168 * time.Hour, MaxPerUser: 20},
		// @decision(DL-093)
		History: History{Retention: 720 * time.Hour, MaxSessionMiB: 1024},
		// Per-user limits for a hosted server. Three concurrent
		// sessions and eight hours are generous for one person and
		// still bound what any one user can spend.
		//
		// @decision(DL-009)
		Quotas: Quotas{
			MaxConcurrentSessions: 3,
			// Fewer jobs than sessions: a job runs unattended.
			//
			// @decision(DL-045)
			MaxConcurrentJobs: 2,
			// @decision(DL-049)
			MaxJobDuration: 4 * time.Hour,
			MaxTimeout:     8 * time.Hour,
			MaxCPU:         4,
			MaxMemoryMiB:   16384,
		},
	}
}

// DefaultOpenCodeVersion is the OpenCode release spov pins by default.
// Both the sandbox image and the local client are built against it.
// Raising it requires `spov serve image build` first, or a launch
// resolves an image nobody has published.
//
// @decision(DL-035) @decision(DL-032)
const DefaultOpenCodeVersion = "2.2.5"

// MinIdleTimeout is the floor the sandbox package enforces; re-exported
// so config errors can name it.
//
// @decision(DL-029)
const MinIdleTimeout = sandbox.MinIdleTimeout

// MaxSandboxTimeout is the hard ceiling spov enforces on sandbox lifetime.
//
// @decision(DL-029)
const MaxSandboxTimeout = 12 * time.Hour

// DefaultLaunchTimeout bounds a launch. The health wait alone is up to
// three minutes (DL-025); token minting and clones come before it.
//
// It lives here rather than in internal/server because internal/jobs
// derives its own bounds from it — deliberately, so a job's launch
// budget IS the ordinary one — and neither package may import the
// other.
//
// @decision(DL-046)
const DefaultLaunchTimeout = 5 * time.Minute
