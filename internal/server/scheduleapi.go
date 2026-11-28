package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/store"
)

// The schedules' HTTP surface: hosted only, walled per owner as jobs are.
//
// @decision(DL-194)

// errSchedulesNeedAServer is a local server's answer: nothing fires
// while it sleeps.
var errSchedulesNeedAServer = errors.New("schedules need a hosted server: a local one does not run while your machine sleeps")

// errSkipped is a run now that skipped, with the skip's reason.
var errSkipped = errors.New("not run")

// scheduleNameRE is a schedule's name: what a path segment and a CLI
// argument carry without escaping.
var scheduleNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// nextRunsShown is how many coming slots a view lists.
const nextRunsShown = 3

// ScheduleRequest creates a schedule.
type ScheduleRequest struct {
	Name string `json:"name" minLength:"1" maxLength:"63" doc:"The schedule's name, unique among yours: lower-case letters, digits, dots, dashes and underscores"`
	Cron string `json:"cron" minLength:"1" maxLength:"128" doc:"Five cron fields (minute hour day-of-month month day-of-week), or @hourly, @daily, @weekly or @monthly; at most hourly"`
	Zone string `json:"zone,omitempty" maxLength:"64" doc:"The IANA time zone the cadence is read in; UTC when empty"`
	// Job is the job each run creates, as POST /api/v1/jobs takes it.
	Job JobRequest `json:"job"`
}

// SchedulePatch changes a schedule; an absent field is kept.
type SchedulePatch struct {
	Name *string     `json:"name,omitempty" maxLength:"63" doc:"A new name"`
	Cron *string     `json:"cron,omitempty" maxLength:"128" doc:"A new cadence"`
	Zone *string     `json:"zone,omitempty" maxLength:"64" doc:"A new time zone"`
	Job  *JobRequest `json:"job,omitempty" doc:"A new job request, replacing the old one whole"`
}

// ScheduleFireView is one run of a schedule.
type ScheduleFireView struct {
	Slot    time.Time `json:"slot" doc:"The cadence's time the run was for"`
	FiredAt time.Time `json:"fired_at"`
	Outcome string    `json:"outcome" enum:"started,skipped,failed,missed"`
	Reason  string    `json:"reason,omitempty"`
	JobID   string    `json:"job_id,omitempty" doc:"The job it started, or the one it waited on"`
	Missed  int       `json:"missed,omitempty" doc:"How many slots a missed entry stands for"`
}

// ScheduleView is a schedule as a client sees it; it holds no secret.
type ScheduleView struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	OwnerID      string             `json:"owner_id"`
	Cron         string             `json:"cron"`
	Zone         string             `json:"zone"`
	State        string             `json:"state" enum:"active,paused"`
	PausedReason string             `json:"paused_reason,omitempty"`
	NextRunAt    *time.Time         `json:"next_run_at,omitempty" doc:"The next slot; absent while paused"`
	NextRuns     []time.Time        `json:"next_runs" doc:"The next three slots"`
	Failures     int                `json:"failures" doc:"Failed runs in a row; three pause the schedule"`
	Job          JobRequest         `json:"job"`
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
	History      []ScheduleFireView `json:"history,omitempty" doc:"Its newest runs, newest first; only on a single read"`
	LastRun      *ScheduleFireView  `json:"last_run,omitempty" doc:"Its newest run"`
}

// JobScheduleView names the schedule that created a job.
//
// @decision(DL-194)
type JobScheduleView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// schedulesAvailable refuses on a server that fires none.
func (s *Service) schedulesAvailable() error {
	if s.deps.Local {
		return errSchedulesNeedAServer
	}
	if s.deps.Jobs == nil {
		return jobs.ErrJobsNeedAServer
	}
	return nil
}

// AdminScheduleRun is the audit row of an admin running a schedule that
// is not theirs.
//
// @decision(DL-194)
const AdminScheduleRun = "schedule.run"

// changeSchedule applies change to a schedule's row read again and held
// in a transaction, so a fire's, an off-boarding's or another edit's
// change made since the caller's read is kept.
//
// @decision(DL-194)
func (s *Service) changeSchedule(ctx context.Context, sc store.Schedule, change func(cur *store.Schedule)) (store.Schedule, error) {
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		cur, err := tx.Schedules().Lock(ctx, sc.OrgID, sc.ID)
		if err != nil {
			return err
		}
		change(&cur)
		return tx.Schedules().Update(ctx, cur)
	})
	if err != nil {
		return store.Schedule{}, err
	}
	return s.deps.Store.Schedules().Get(ctx, sc.OrgID, sc.ID)
}

// checkScheduleName refuses a name outside scheduleNameRE.
func checkScheduleName(name string) error {
	if !scheduleNameRE.MatchString(name) {
		return fmt.Errorf("%w: a schedule's name is 1 to 63 lower-case letters, digits, dots, dashes or underscores,"+
			" starting with a letter or digit", ErrInvalidRequest)
	}
	return nil
}

// CreateSchedule checks and stores a schedule of the caller's.
//
// @decision(DL-191) @decision(DL-194)
func (s *Service) CreateSchedule(ctx context.Context, p auth.Principal, in ScheduleRequest) (store.Schedule, error) {
	if err := s.schedulesAvailable(); err != nil {
		return store.Schedule{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if err := checkScheduleName(in.Name); err != nil {
		return store.Schedule{}, err
	}
	now := s.deps.Now().UTC()
	c, err := checkCadence(in.Cron, in.Zone, now)
	if err != nil {
		return store.Schedule{}, err
	}
	if err := s.CheckScheduledJob(ctx, p, in.Job); err != nil {
		return store.Schedule{}, err
	}
	raw, err := json.Marshal(in.Job)
	if err != nil {
		return store.Schedule{}, fmt.Errorf("encoding the job request: %w", err)
	}
	sc := store.Schedule{ID: store.NewID(now), OrgID: p.OrgID, OwnerID: p.UserID, Name: in.Name, Request: raw,
		Cron: strings.TrimSpace(in.Cron), Zone: zoneOrUTC(in.Zone), State: store.ScheduleActive, NextRunAt: c.next(now)}
	err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
		n, err := tx.Schedules().Count(ctx, p.OrgID, store.ScheduleFilter{OwnerID: p.UserID})
		if err != nil {
			return err
		}
		if n >= MaxSchedulesPerUser {
			return fmt.Errorf("%w: you have %d of %d schedules; delete one first", ErrQuotaExceeded, n, MaxSchedulesPerUser)
		}
		return tx.Schedules().Create(ctx, sc)
	})
	if errors.Is(err, store.ErrConflict) {
		return store.Schedule{}, fmt.Errorf("%w: you have a schedule named %s", ErrNameInUse, in.Name)
	}
	if err != nil {
		return store.Schedule{}, err
	}
	return s.deps.Store.Schedules().Get(ctx, p.OrgID, sc.ID)
}

// zoneOrUTC is a zone as stored: UTC when empty.
func zoneOrUTC(zone string) string {
	if zone = strings.TrimSpace(zone); zone == "" {
		return "UTC"
	}
	return zone
}

// GetSchedule resolves a ref (an id, or the caller's own name) to a
// schedule the caller may reach: its owner, or an admin. Anyone else is
// told it does not exist.
//
// @decision(DL-194) @decision(DL-006)
func (s *Service) GetSchedule(ctx context.Context, p auth.Principal, ref string) (store.Schedule, error) {
	if err := s.schedulesAvailable(); err != nil {
		return store.Schedule{}, err
	}
	ref = strings.TrimSpace(ref)
	sc, err := s.deps.Store.Schedules().GetByName(ctx, p.OrgID, p.UserID, ref)
	if errors.Is(err, store.ErrNotFound) {
		sc, err = s.deps.Store.Schedules().Get(ctx, p.OrgID, ref)
	}
	if err != nil {
		return store.Schedule{}, err
	}
	if !p.CanAccess(sc.OwnerID) {
		return store.Schedule{}, store.ErrNotFound
	}
	return sc, nil
}

// UpdateSchedule changes a schedule, checking what changed as a create
// would; a new cadence or zone moves the next slot from now. Only the
// owner may change the job, which is checked as theirs.
//
// @decision(DL-191) @decision(DL-194)
func (s *Service) UpdateSchedule(ctx context.Context, p auth.Principal, ref string, in SchedulePatch) (store.Schedule, error) {
	sc, err := s.GetSchedule(ctx, p, ref)
	if err != nil {
		return store.Schedule{}, err
	}
	now := s.deps.Now().UTC()
	var name, cronExpr, zone string
	var next time.Time
	var request []byte
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
		if err := checkScheduleName(name); err != nil {
			return store.Schedule{}, err
		}
	}
	if in.Cron != nil || in.Zone != nil {
		cronExpr, zone = sc.Cron, sc.Zone
		if in.Cron != nil {
			cronExpr = strings.TrimSpace(*in.Cron)
		}
		if in.Zone != nil {
			zone = zoneOrUTC(*in.Zone)
		}
		c, err := checkCadence(cronExpr, zone, now)
		if err != nil {
			return store.Schedule{}, err
		}
		next = c.next(now)
	}
	if in.Job != nil {
		if p.UserID != sc.OwnerID {
			return store.Schedule{}, fmt.Errorf("%w: only the schedule's owner may change the job it runs", ErrInvalidRequest)
		}
		if err := s.CheckScheduledJob(ctx, p, *in.Job); err != nil {
			return store.Schedule{}, err
		}
		if request, err = json.Marshal(*in.Job); err != nil {
			return store.Schedule{}, fmt.Errorf("encoding the job request: %w", err)
		}
	}
	got, err := s.changeSchedule(ctx, sc, func(cur *store.Schedule) {
		if name != "" {
			cur.Name = name
		}
		if !next.IsZero() {
			cur.Cron, cur.Zone, cur.NextRunAt = cronExpr, zone, next
		}
		if request != nil {
			cur.Request = request
		}
	})
	if errors.Is(err, store.ErrConflict) {
		return store.Schedule{}, fmt.Errorf("%w: the owner has a schedule named %s", ErrNameInUse, name)
	}
	return got, err
}

// SetSchedulePaused pauses or resumes a schedule. A resume starts from
// the next slot after now with no failures counted, and only while its
// owner may still sign in.
//
// @decision(DL-193) @decision(DL-194)
func (s *Service) SetSchedulePaused(ctx context.Context, p auth.Principal, ref string, paused bool) (store.Schedule, error) {
	sc, err := s.GetSchedule(ctx, p, ref)
	if err != nil {
		return store.Schedule{}, err
	}
	if paused {
		reason := "paused by " + actorOf(p)
		return s.changeSchedule(ctx, sc, func(cur *store.Schedule) {
			if cur.State == store.ScheduleActive {
				cur.State, cur.PausedReason = store.SchedulePaused, reason
			}
		})
	}
	if _, err := s.scheduleOwner(ctx, sc); err != nil {
		return store.Schedule{}, err
	}
	c, err := parseCadence(sc.Cron, sc.Zone)
	if err != nil {
		return store.Schedule{}, err
	}
	next := c.next(s.deps.Now().UTC())
	if next.IsZero() {
		return store.Schedule{}, fmt.Errorf("%w: its cadence fires no more; change it to resume it", ErrInvalidRequest)
	}
	return s.changeSchedule(ctx, sc, func(cur *store.Schedule) {
		if cur.Cron != sc.Cron || cur.Zone != sc.Zone {
			// Changed meanwhile: the change set its own next slot.
			next = cur.NextRunAt
		}
		cur.State, cur.PausedReason, cur.Failures, cur.NextRunAt = store.ScheduleActive, "", 0, next
	})
}

// DeleteSchedule removes a schedule and its history; its jobs stay.
//
// @decision(DL-194)
func (s *Service) DeleteSchedule(ctx context.Context, p auth.Principal, ref string) error {
	sc, err := s.GetSchedule(ctx, p, ref)
	if err != nil {
		return err
	}
	return s.deps.Store.Schedules().Delete(ctx, sc.OrgID, sc.ID)
}

// RunSchedule fires a schedule now, as its owner, under the same rules
// as a slot; a skip or a failure is returned as the error it is.
//
// @decision(DL-192) @decision(DL-194)
func (s *Service) RunSchedule(ctx context.Context, p auth.Principal, ref string) (string, error) {
	sc, err := s.GetSchedule(ctx, p, ref)
	if err != nil {
		return "", err
	}
	now := s.deps.Now().UTC()
	fired := s.fireSchedule(ctx, sc, now, true)
	switch {
	case fired.Outcome == store.FireStarted:
		if p.UserID != sc.OwnerID {
			if err := s.deps.Store.AdminEvents().Append(ctx, store.AdminEvent{
				ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminScheduleRun, At: now,
				Detail: fmt.Sprintf("schedule %s (%s) of %s, job %s", sc.Name, sc.ID, sc.OwnerID, fired.JobID),
			}); err != nil {
				s.deps.Warn(fmt.Sprintf("schedule %s: the audit row of an admin's run was not written: %s", sc.ID, clientText(err)))
			}
		}
		return fired.JobID, nil
	case fired.err != nil:
		// A store fault is a 500 with a reference, the limit a 429 and a
		// refusal a 422, by the error's own kind.
		return "", fired.err
	case fired.Outcome == store.FireSkipped:
		return "", fmt.Errorf("%w: %s", errSkipped, fired.Reason)
	default:
		return "", fmt.Errorf("%w: %s", ErrInvalidRequest, fired.Reason)
	}
}

// ListSchedules is a page of the caller's schedules, or with all, an
// admin's view of the org's.
//
// @decision(DL-194)
func (s *Service) ListSchedules(ctx context.Context, p auth.Principal, all bool, f store.ScheduleFilter) ([]store.Schedule, int, error) {
	if err := s.schedulesAvailable(); err != nil {
		return nil, 0, err
	}
	f.OwnerID = p.UserID
	if all && p.Admin {
		f.OwnerID = ""
	}
	list, err := s.deps.Store.Schedules().List(ctx, p.OrgID, f)
	if err != nil {
		return nil, 0, err
	}
	n, err := s.deps.Store.Schedules().Count(ctx, p.OrgID, f)
	return list, n, err
}

// scheduleViewOf is a schedule's view, with its history when given.
//
// @decision(DL-194)
func scheduleViewOf(sc store.Schedule, history []store.ScheduleFire) ScheduleView {
	v := ScheduleView{ID: sc.ID, Name: sc.Name, OwnerID: sc.OwnerID, Cron: sc.Cron, Zone: sc.Zone, State: string(sc.State),
		PausedReason: sc.PausedReason, Failures: sc.Failures, CreatedAt: sc.CreatedAt, UpdatedAt: sc.UpdatedAt, NextRuns: []time.Time{}}
	_ = json.Unmarshal(sc.Request, &v.Job)
	if sc.State == store.ScheduleActive {
		next := sc.NextRunAt
		v.NextRunAt = &next
		if c, err := parseCadence(sc.Cron, sc.Zone); err == nil {
			for t := next; !t.IsZero() && len(v.NextRuns) < nextRunsShown; t = c.next(t) {
				v.NextRuns = append(v.NextRuns, t)
			}
		}
	}
	for _, f := range history {
		v.History = append(v.History, ScheduleFireView{Slot: f.Slot, FiredAt: f.FiredAt, Outcome: string(f.Outcome),
			Reason: f.Reason, JobID: f.JobID, Missed: f.Missed})
	}
	if len(v.History) > 0 {
		last := v.History[0]
		v.LastRun = &last
	}
	return v
}

// scheduleViewWithLast is a schedule's view with its newest run, as a
// listing shows it.
//
// @decision(DL-194)
func (s *Server) scheduleViewWithLast(ctx context.Context, sc store.Schedule) ScheduleView {
	fires, _ := s.deps.Store.Schedules().Fires(ctx, sc.OrgID, sc.ID, 1)
	v := scheduleViewOf(sc, fires)
	v.History = nil
	return v
}

// scheduleNames names the schedules jobs came from, one read each of
// the distinct ones on a page.
//
// @decision(DL-194)
func (s *Service) scheduleNames(ctx context.Context, orgID string, list []store.Job) map[string]string {
	out := map[string]string{}
	for _, j := range list {
		if j.ScheduleID == "" {
			continue
		}
		if _, seen := out[j.ScheduleID]; seen {
			continue
		}
		out[j.ScheduleID] = ""
		if sc, err := s.deps.Store.Schedules().Get(ctx, orgID, j.ScheduleID); err == nil {
			out[j.ScheduleID] = sc.Name
		}
	}
	return out
}

type scheduleRefInput struct {
	Ref string `path:"ref" doc:"The schedule's name, or its id"`
}

type scheduleOutput struct {
	Body ScheduleView
}

type createScheduleInput struct {
	Body ScheduleRequest
}

type updateScheduleInput struct {
	Ref  string `path:"ref" doc:"The schedule's name, or its id"`
	Body SchedulePatch
}

type listSchedulesInput struct {
	All    bool   `query:"all" doc:"An admin's: every schedule of the organisation"`
	Limit  int    `query:"limit" minimum:"0" doc:"How many to return, newest first: 50 when unset, at most 200"`
	Before string `query:"before" doc:"Return only schedules older than this ID: the next field of the previous page"`
}

type listSchedulesOutput struct {
	Body struct {
		Schedules []ScheduleView `json:"schedules"`
		Next      string         `json:"next,omitempty" doc:"Pass as before for the next page; absent on the last"`
		Total     int            `json:"total"`
	}
}

type runScheduleOutput struct {
	Body struct {
		JobID string `json:"job_id"`
	}
}

// toScheduleHumaError maps a schedule's errors, then a job's.
func toScheduleHumaError(err error) error {
	switch {
	case asStatusError(err) != nil:
		return asStatusError(err)
	// A missing row on these routes is the schedule, not the session the
	// shared mapper would name.
	case errors.Is(err, store.ErrNotFound):
		return huma.Error404NotFound("no such schedule")
	case errors.Is(err, errSchedulesNeedAServer):
		return huma.Error501NotImplemented(err.Error())
	case errors.Is(err, errSkipped):
		return huma.Error409Conflict(err.Error())
	}
	return toJobHumaError(err)
}

// registerSchedules serves /api/v1/schedules.
//
// @decision(DL-194)
func (s *Server) registerSchedules() {
	tags := []string{"schedules"}
	register(s.api, huma.Operation{
		OperationID: "create-schedule", Method: http.MethodPost, Path: "/api/v1/schedules",
		Summary: "Create a schedule", DefaultStatus: http.StatusCreated, Tags: tags,
		Errors: []int{http.StatusConflict, http.StatusTooManyRequests, http.StatusNotImplemented},
	}, func(ctx context.Context, in *createScheduleInput) (*scheduleOutput, error) {
		sc, err := s.svc.CreateSchedule(ctx, principal(ctx), in.Body)
		if err != nil {
			return nil, toScheduleHumaError(err)
		}
		return &scheduleOutput{Body: s.scheduleViewWithLast(ctx, sc)}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "list-schedules", Method: http.MethodGet, Path: "/api/v1/schedules",
		Summary: "List schedules", Tags: tags, Errors: []int{http.StatusNotImplemented},
	}, func(ctx context.Context, in *listSchedulesInput) (*listSchedulesOutput, error) {
		size := pageSize(in.Limit)
		f := store.ScheduleFilter{Before: strings.TrimSpace(in.Before), Limit: size + 1}
		list, total, err := s.svc.ListSchedules(ctx, principal(ctx), in.All, f)
		if err != nil {
			return nil, toScheduleHumaError(err)
		}
		out := &listSchedulesOutput{}
		list, out.Body.Next = pageOf(list, size, func(sc store.Schedule) string { return sc.ID })
		out.Body.Total = total
		out.Body.Schedules = make([]ScheduleView, 0, len(list))
		for _, sc := range list {
			out.Body.Schedules = append(out.Body.Schedules, s.scheduleViewWithLast(ctx, sc))
		}
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "get-schedule", Method: http.MethodGet, Path: "/api/v1/schedules/{ref}",
		Summary: "Get a schedule and its runs", Tags: tags, Errors: []int{http.StatusNotImplemented},
	}, func(ctx context.Context, in *scheduleRefInput) (*scheduleOutput, error) {
		sc, err := s.svc.GetSchedule(ctx, principal(ctx), in.Ref)
		if err != nil {
			return nil, toScheduleHumaError(err)
		}
		fires, err := s.deps.Store.Schedules().Fires(ctx, sc.OrgID, sc.ID, 0)
		if err != nil {
			return nil, toScheduleHumaError(err)
		}
		return &scheduleOutput{Body: scheduleViewOf(sc, fires)}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "update-schedule", Method: http.MethodPatch, Path: "/api/v1/schedules/{ref}",
		Summary: "Change a schedule", Tags: tags, Errors: []int{http.StatusConflict, http.StatusNotImplemented},
	}, func(ctx context.Context, in *updateScheduleInput) (*scheduleOutput, error) {
		sc, err := s.svc.UpdateSchedule(ctx, principal(ctx), in.Ref, in.Body)
		if err != nil {
			return nil, toScheduleHumaError(err)
		}
		return &scheduleOutput{Body: s.scheduleViewWithLast(ctx, sc)}, nil
	})

	for _, op := range []struct {
		id, path, summary string
		paused            bool
	}{
		{"pause-schedule", "/api/v1/schedules/{ref}/pause", "Pause a schedule", true},
		{"resume-schedule", "/api/v1/schedules/{ref}/resume", "Resume a schedule from its next slot", false},
	} {
		register(s.api, huma.Operation{
			OperationID: op.id, Method: http.MethodPost, Path: op.path, Summary: op.summary, Tags: tags,
			Errors: []int{http.StatusNotImplemented},
		}, func(ctx context.Context, in *scheduleRefInput) (*scheduleOutput, error) {
			sc, err := s.svc.SetSchedulePaused(ctx, principal(ctx), in.Ref, op.paused)
			if err != nil {
				return nil, toScheduleHumaError(err)
			}
			return &scheduleOutput{Body: s.scheduleViewWithLast(ctx, sc)}, nil
		})
	}

	register(s.api, huma.Operation{
		OperationID: "run-schedule", Method: http.MethodPost, Path: "/api/v1/schedules/{ref}/run",
		Summary: "Run a schedule now", DefaultStatus: http.StatusAccepted, Tags: tags,
		Errors: []int{http.StatusConflict, http.StatusTooManyRequests, http.StatusNotImplemented},
	}, func(ctx context.Context, in *scheduleRefInput) (*runScheduleOutput, error) {
		id, err := s.svc.RunSchedule(ctx, principal(ctx), in.Ref)
		if err != nil {
			return nil, toScheduleHumaError(err)
		}
		out := &runScheduleOutput{}
		out.Body.JobID = id
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "delete-schedule", Method: http.MethodDelete, Path: "/api/v1/schedules/{ref}",
		Summary: "Delete a schedule and its runs' history", DefaultStatus: http.StatusNoContent, Tags: tags,
		Errors: []int{http.StatusNotImplemented},
	}, func(ctx context.Context, in *scheduleRefInput) (*struct{}, error) {
		if err := s.svc.DeleteSchedule(ctx, principal(ctx), in.Ref); err != nil {
			return nil, toScheduleHumaError(err)
		}
		return nil, nil
	})
}
