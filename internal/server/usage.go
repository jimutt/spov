package server

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// UsageView is what a user has spent this calendar month. Every figure
// is an estimate from the requested resources and elapsed wall-clock,
// and can only be a floor on the real invoice.
//
// @decision(DL-009) @decision(DL-029)
type UsageView struct {
	// Since is the start of the period, the first of the current month.
	Since time.Time `json:"since"`
	// Sessions is how many sessions ran in the period.
	Sessions int `json:"sessions"`
	// RunningHours is the total wall-clock across them.
	RunningHours float64 `json:"running_hours"`
	// EstimatedCost is in US dollars, and is a floor rather than a bill.
	EstimatedCost float64 `json:"estimated_cost"`
	// Estimated is always true, so a client cannot render this as a
	// settled figure by omission.
	Estimated bool `json:"estimated"`
}

type usageOutput struct {
	Body UsageView
}

// registerUsage serves /me/usage.
//
// @decision(DL-009)
func (s *Server) registerUsage() {
	register(s.api, huma.Operation{
		OperationID: "me-usage", Method: http.MethodGet, Path: "/api/v1/me/usage",
		Summary: "The caller's estimated usage this month", Tags: []string{"usage"},
	}, func(ctx context.Context, _ *struct{}) (*usageOutput, error) {
		p := principal(ctx)
		view, err := s.svc.usageFor(ctx, p.OrgID, p.UserID, s.deps.Now())
		if err != nil {
			return nil, toHumaError(err)
		}
		return &usageOutput{Body: view}, nil
	})
}

// usageFor sums a user's sessions for the calendar month containing now.
//
// A session that started before the month and is still running counts
// only its hours inside it, so the months of a long session do not each
// claim the whole of it.
//
// @decision(DL-009)
func (s *Service) usageFor(ctx context.Context, orgID, userID string, now time.Time) (UsageView, error) {
	since := monthStart(now)
	view := UsageView{Since: since, Estimated: true}
	if s.deps.Store == nil {
		return view, nil
	}
	sessions, err := s.deps.Store.Sessions().List(ctx, orgID, store.SessionFilter{OwnerID: userID})
	if err != nil {
		return UsageView{}, err
	}
	for _, sess := range sessions {
		d := billableWithin(sess, since, now)
		if d <= 0 {
			continue
		}
		view.Sessions++
		view.RunningHours += d.Hours()
		view.EstimatedCost += sandbox.EstimatedCost(sess.Request.Sandbox.CPU, sess.Request.Sandbox.MemoryMiB, d)
	}
	view.RunningHours = round2(view.RunningHours)
	view.EstimatedCost = round2(view.EstimatedCost)
	return view, nil
}

// billableWithin is how long a session ran inside [since, now].
//
// @decision(DL-009)
func billableWithin(sess store.Session, since, now time.Time) time.Duration {
	// A row with no creation time cannot be aged. Both stores backfill
	// it, so this is unreachable today — but it is the one input that
	// turns a bad row into a wildly wrong cost, since the zero time is
	// before every period and would bill from the first of the month.
	if sess.CreatedAt.IsZero() {
		return 0
	}
	start := sess.CreatedAt
	if start.Before(since) {
		start = since
	}
	end := now
	if sess.StoppedAt != nil && sess.StoppedAt.Before(end) {
		end = *sess.StoppedAt
	}
	if !end.After(start) {
		return 0
	}
	return end.Sub(start)
}

// monthStart is the first instant of the calendar month containing t,
// in UTC. Billing periods are the deployment's, not the viewer's.
func monthStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
