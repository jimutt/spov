package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// ErrQuotaExceeded is a request refused by the caller's own limits. The
// API maps it to 429: the request is well-formed and would succeed for
// someone else, or for this user once a session ends.
//
// @decision(DL-009)
var ErrQuotaExceeded = errors.New("server: quota exceeded")

// QuotaView is the effective quota, as /me reports it. Every field is
// resolved — the org default with the user's override applied — so a
// client never has to know which layer supplied a number.
//
// @decision(DL-009)
type QuotaView struct {
	MaxConcurrentSessions int `json:"max_concurrent_sessions"`
	// MaxConcurrentJobs is a separate limit, not a share of the session
	// limit.
	//
	// @decision(DL-009)
	MaxConcurrentJobs int `json:"max_concurrent_jobs"`
	// MaxJobDuration is the longest wall clock one job may ask for.
	//
	// @decision(DL-049)
	MaxJobDuration string  `json:"max_job_duration"`
	MaxTimeout     string  `json:"max_timeout"`
	MaxCPU         float64 `json:"max_cpu"`
	MaxMemoryMiB   int     `json:"max_memory_mib"`
}

// Quotas are the org defaults a deployment configures. A zero field
// means the built-in default rather than "no limit": an unbounded
// default is how a hosted server gets a surprise bill.
//
// @decision(DL-009)
type Quotas struct {
	MaxConcurrentSessions int
	// MaxConcurrentJobs bounds a user's unfinished jobs.
	//
	// @decision(DL-009)
	MaxConcurrentJobs int
	// MaxJobDuration caps a job's wall clock.
	//
	// @decision(DL-049)
	MaxJobDuration time.Duration
	MaxTimeout     time.Duration
	MaxCPU         float64
	MaxMemoryMiB   int
}

// DefaultQuotas are the built-in limits, used for any field a
// deployment leaves unset.
//
// @decision(DL-009)
var DefaultQuotas = Quotas{
	MaxConcurrentSessions: 3,
	// Below the session limit: a job runs unattended.
	//
	// @decision(DL-009)
	MaxConcurrentJobs: 2,
	// @decision(DL-049)
	MaxJobDuration: 4 * time.Hour,
	MaxTimeout:     8 * time.Hour,
	MaxCPU:         4,
	MaxMemoryMiB:   16384,
}

// withDefaults fills unset fields and clamps the timeout to DL-029's
// sandbox hard cap, so the quota never promises what the create refuses.
//
// @decision(DL-009) @decision(DL-029)
func (q Quotas) withDefaults() Quotas {
	if q.MaxConcurrentSessions <= 0 {
		q.MaxConcurrentSessions = DefaultQuotas.MaxConcurrentSessions
	}
	// @decision(DL-009)
	if q.MaxConcurrentJobs <= 0 {
		q.MaxConcurrentJobs = DefaultQuotas.MaxConcurrentJobs
	}
	// @decision(DL-049)
	if q.MaxJobDuration <= 0 {
		q.MaxJobDuration = DefaultQuotas.MaxJobDuration
	}
	if q.MaxTimeout <= 0 {
		q.MaxTimeout = DefaultQuotas.MaxTimeout
	}
	if q.MaxTimeout > sandbox.MaxTimeout {
		q.MaxTimeout = sandbox.MaxTimeout
	}
	if q.MaxCPU <= 0 {
		q.MaxCPU = DefaultQuotas.MaxCPU
	}
	if q.MaxMemoryMiB <= 0 {
		q.MaxMemoryMiB = DefaultQuotas.MaxMemoryMiB
	}
	return q
}

// effective layers a user's stored override over the org defaults, then
// re-applies the caps: an override is a per-user adjustment, not a way
// past the hard ceiling.
//
// @decision(DL-009)
func (q Quotas) effective(over store.Quota) Quotas {
	out := q.withDefaults()
	if over.MaxConcurrentSessions != nil && *over.MaxConcurrentSessions > 0 {
		out.MaxConcurrentSessions = *over.MaxConcurrentSessions
	}
	// @decision(DL-009)
	if over.MaxConcurrentJobs != nil && *over.MaxConcurrentJobs > 0 {
		out.MaxConcurrentJobs = *over.MaxConcurrentJobs
	}
	// @decision(DL-049)
	if over.MaxJobDuration != nil && *over.MaxJobDuration > 0 {
		out.MaxJobDuration = *over.MaxJobDuration
	}
	if over.MaxTimeout != nil && *over.MaxTimeout > 0 {
		out.MaxTimeout = *over.MaxTimeout
	}
	if over.MaxCPU != nil && *over.MaxCPU > 0 {
		out.MaxCPU = *over.MaxCPU
	}
	if over.MaxMemoryMiB != nil && *over.MaxMemoryMiB > 0 {
		out.MaxMemoryMiB = *over.MaxMemoryMiB
	}
	if out.MaxTimeout > sandbox.MaxTimeout {
		out.MaxTimeout = sandbox.MaxTimeout
	}
	return out
}

// MaxSessionAge is the longest a session may legitimately run under
// these quotas, which is what the reconciler's reaping backstop bounds
// against. A per-user override above it only delays the reap; the
// sandbox's own Timeout is the last resort either way.
//
// @decision(DL-009)
func (q Quotas) MaxSessionAge() time.Duration {
	return q.withDefaults().MaxTimeout
}

func (q Quotas) view() QuotaView {
	e := q.withDefaults()
	return QuotaView{
		MaxConcurrentSessions: e.MaxConcurrentSessions,
		MaxConcurrentJobs:     e.MaxConcurrentJobs,
		MaxJobDuration:        e.MaxJobDuration.String(),
		MaxTimeout:            e.MaxTimeout.String(),
		MaxCPU:                e.MaxCPU,
		MaxMemoryMiB:          e.MaxMemoryMiB,
	}
}

// checkRequest refuses, rather than clamps, a request that asks for more
// than the quota allows, naming each limit exceeded.
//
// @decision(DL-009)
func (q Quotas) checkRequest(req store.SessionRequest) error {
	var problems []string
	if req.Sandbox.Timeout > q.MaxTimeout {
		problems = append(problems, fmt.Sprintf("timeout %s exceeds your limit of %s",
			req.Sandbox.Timeout, q.MaxTimeout))
	}
	// Compared as max(request, ceiling); see DL-009 on why.
	//
	// @decision(DL-009) @decision(DL-029)
	if cpu := max(req.Sandbox.CPU, req.Sandbox.CPULimit); cpu > q.MaxCPU {
		problems = append(problems, fmt.Sprintf("cpu %g exceeds your limit of %g",
			cpu, q.MaxCPU))
	}
	if mem := max(req.Sandbox.MemoryMiB, req.Sandbox.MemoryLimitMiB); mem > q.MaxMemoryMiB {
		problems = append(problems, fmt.Sprintf("memory %d MiB exceeds your limit of %d MiB",
			mem, q.MaxMemoryMiB))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, strings.Join(problems, "; "))
	}
	return nil
}

// quotaFor resolves the caller's effective quota: the org defaults with
// their stored override applied. A principal with no user row, such as
// local mode's bearer token, gets the defaults.
//
// Nothing in production calls it; the creates use profileFor and quotaOf
// instead so the profile is read once per create. Kept for the tests.
//
// @decision(DL-009) @decision(DL-014)
func (s *Service) quotaFor(ctx context.Context, orgID, userID string) Quotas {
	return s.quotaOf(s.profileFor(ctx, orgID, userID))
}

// quotaOf applies a profile's stored override to the org defaults. An
// absent profile is the zero User, whose empty override asserts nothing.
//
// @decision(DL-009)
func (s *Service) quotaOf(u store.User) Quotas {
	return s.deps.Quotas.effective(u.Quota)
}
