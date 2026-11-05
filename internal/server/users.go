package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
)

// UserView is a user as an admin sees them. It carries no credential
// and no token — those have their own endpoints, scoped to their owner.
//
// @decision(DL-009)
type UserView struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name,omitempty"`
	GithubLogin string     `json:"github_login,omitempty"`
	Admin       bool       `json:"admin"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	// Quota is the effective quota: the org defaults with this user's
	// override applied.
	Quota QuotaView `json:"quota"`
	// QuotaOverride is what is stored against the user, so an admin can
	// see which fields they have actually pinned.
	QuotaOverride QuotaOverrideView `json:"quota_override"`
}

type usersOutput struct {
	Body struct {
		Users []UserView `json:"users"`
	}
}

// QuotaInput is a quota as the API accepts it. It mirrors store.Quota
// but types the timeout as a string, since a time.Duration would be
// documented and validated as an integer count of nanoseconds.
//
// @decision(DL-009)
type QuotaInput struct {
	MaxConcurrentSessions *int `json:"max_concurrent_sessions,omitempty" doc:"Starting and running sessions at once"`
	// @decision(DL-045)
	MaxConcurrentJobs *int `json:"max_concurrent_jobs,omitempty" doc:"Unfinished autonomous jobs at once"`
	// @decision(DL-049)
	MaxJobDuration *string  `json:"max_job_duration,omitempty" doc:"Longest wall clock one job may run unattended, as a duration such as 2h"`
	MaxTimeout     *string  `json:"max_timeout,omitempty" doc:"Longest session lifetime, as a duration such as 8h"`
	MaxCPU         *float64 `json:"max_cpu,omitempty"`
	MaxMemoryMiB   *int     `json:"max_memory_mib,omitempty"`
}

// QuotaOverrideView is a stored override as the API reports it, defined
// from QuotaInput rather than repeated so that what the API reports is
// by construction what PATCH accepts.
//
// @decision(DL-005) @decision(DL-009)
type QuotaOverrideView QuotaInput

// overrideView renders a stored override for the wire.
//
// @decision(DL-005)
func overrideView(q store.Quota) QuotaOverrideView {
	out := QuotaOverrideView{
		MaxConcurrentSessions: q.MaxConcurrentSessions,
		MaxConcurrentJobs:     q.MaxConcurrentJobs,
		MaxCPU:                q.MaxCPU,
		MaxMemoryMiB:          q.MaxMemoryMiB,
	}
	if q.MaxTimeout != nil {
		d := q.MaxTimeout.String()
		out.MaxTimeout = &d
	}
	if q.MaxJobDuration != nil {
		d := q.MaxJobDuration.String()
		out.MaxJobDuration = &d
	}
	return out
}

// quota converts the input, refusing a timeout that is not a duration.
//
// @decision(DL-009)
func (q QuotaInput) quota() (store.Quota, error) {
	out := store.Quota{
		MaxConcurrentSessions: q.MaxConcurrentSessions,
		MaxConcurrentJobs:     q.MaxConcurrentJobs,
		MaxCPU:                q.MaxCPU,
		MaxMemoryMiB:          q.MaxMemoryMiB,
	}
	if q.MaxTimeout != nil {
		d, err := time.ParseDuration(*q.MaxTimeout)
		if err != nil {
			return store.Quota{}, fmt.Errorf("max_timeout %q is not a duration, such as 8h", *q.MaxTimeout)
		}
		out.MaxTimeout = &d
	}
	// @decision(DL-049)
	if q.MaxJobDuration != nil {
		d, err := time.ParseDuration(*q.MaxJobDuration)
		if err != nil || d < jobs.MinWallClock {
			return store.Quota{}, fmt.Errorf("max_job_duration %q is not a duration of at least %s, such as 2h",
				*q.MaxJobDuration, jobs.MinWallClock)
		}
		out.MaxJobDuration = &d
	}
	return out, nil
}

type patchUserInput struct {
	ID   string `path:"id"`
	Body struct {
		// Quota replaces the user's stored override wholesale. A field
		// left out of it is cleared, which is what makes a limit
		// removable; omitting Quota entirely leaves it untouched.
		Quota *QuotaInput `json:"quota,omitempty"`
	}
}

type patchUserOutput struct {
	Body UserView
}

// registerUsers serves the admin view of an org's users. Every
// operation requires an administrator: a quota is not a user's own to
// raise, or it would not be a limit.
//
// @decision(DL-009) @decision(DL-006)
func (s *Server) registerUsers() {
	register(s.api, huma.Operation{
		OperationID: "list-users", Method: http.MethodGet, Path: "/api/v1/users",
		Summary: "The org's users and their quotas", Tags: []string{"admin"},
	}, func(ctx context.Context, _ *struct{}) (*usersOutput, error) {
		p := principal(ctx)
		if err := s.requireAdmin(p); err != nil {
			return nil, err
		}
		users, err := s.deps.Store.Users().List(ctx, p.OrgID)
		if err != nil {
			return nil, toHumaError(err)
		}
		out := &usersOutput{}
		out.Body.Users = make([]UserView, 0, len(users))
		for _, u := range users {
			out.Body.Users = append(out.Body.Users, s.userView(u))
		}
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "update-user", Method: http.MethodPatch, Path: "/api/v1/users/{id}",
		Summary: "Set a user's quota", Tags: []string{"admin"},
		Description: "Administrator rights are not set here: they come from the allowlist. " +
			"Use PUT /api/v1/allowlist/{email}.",
	}, func(ctx context.Context, in *patchUserInput) (*patchUserOutput, error) {
		p := principal(ctx)
		if err := s.requireAdmin(p); err != nil {
			return nil, err
		}
		u, err := s.deps.Store.Users().Get(ctx, p.OrgID, in.ID)
		if err != nil {
			return nil, toHumaError(err)
		}
		if in.Body.Quota != nil {
			q, err := in.Body.Quota.quota()
			if err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			u.Quota = q
		}
		if err := s.deps.Store.Users().Update(ctx, u); err != nil {
			return nil, toHumaError(err)
		}
		return &patchUserOutput{Body: s.userView(u)}, nil
	})
}

// requireAdmin refuses a caller who is not an administrator.
//
// This one says forbidden rather than not-found, unlike a session
// someone does not own: an org's users are not secret from each other,
// and "you are not an admin" is the actionable answer.
//
// @decision(DL-009) @decision(DL-006)
func (s *Server) requireAdmin(p auth.Principal) error {
	if s.deps.Store == nil {
		return huma.Error501NotImplemented("this server has no users to administer")
	}
	if !p.Admin {
		return huma.Error403Forbidden("this needs an administrator")
	}
	return nil
}

// userView reports a user as an administrator when the allowlist says
// so, not when their stored row does. A server with no allowlist —
// local mode — has only the row.
//
// @decision(DL-007) @decision(DL-009)
func (s *Server) userView(u store.User) UserView {
	admin := u.Admin
	if s.deps.Login != nil && s.deps.Login.Allowlist != nil {
		admin, _ = s.deps.Login.Allowlist.Lookup(u.Email)
	}
	return UserView{
		ID:            u.ID,
		Email:         u.Email,
		Name:          u.Name,
		GithubLogin:   u.GithubLogin,
		Admin:         admin,
		LastLoginAt:   u.LastLoginAt,
		Quota:         s.deps.Quotas.effective(u.Quota).view(),
		QuotaOverride: overrideView(u.Quota),
	}
}
