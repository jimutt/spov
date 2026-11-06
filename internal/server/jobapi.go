package server

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// The job API. `spov jobs create` is a client of it and gains no privileged
// path of its own, so there is one place a job can be created and one
// place it can be refused.
//
// @decision(DL-045) @decision(DL-002)

// JobRequest creates a job. A job always owns a fresh session, so it
// carries the session's settings rather than a session id.
//
// @decision(DL-045)
type JobRequest struct {
	// Goal is the task, in the user's own words. The job row owns it;
	// the session's first turn is a rendering of it.
	//
	// @decision(DL-045)
	Goal string `json:"goal" minLength:"1" doc:"What the job should accomplish"`
	// Kind decides what is delivered and what the sandbox may do.
	//
	// @decision(DL-045) @decision(DL-040)
	Kind store.JobKind `json:"kind" enum:"pr,report" doc:"What the job delivers"`
	// Repos are the repositories the job's session is bootstrapped
	// with, exactly as a session create takes them.
	Repos   []RepoRef `json:"repos" minItems:"1"`
	Landing string    `json:"landing,omitempty" doc:"Workspace directory to start in; the workspace root when empty"`
	Image   string    `json:"image,omitempty"`
	// Addons and Apt ask for an image variant: curated add-ons as
	// `name[@version]`, and extra Debian packages. Neither combines with
	// Image.
	//
	// @decision(DL-033)
	Addons []string `json:"addons,omitempty" maxItems:"16" doc:"Curated add-ons as name or name@version, e.g. rust, java@21; the image is a variant of the base"`
	Apt    []string `json:"apt,omitempty" maxItems:"32" doc:"Extra Debian packages for the image variant"`
	// Sandbox is named as it is on a session create, not "session": one
	// shape with two names would give the generated TypeScript client
	// two names for the same thing.
	Sandbox SandboxConfigView `json:"sandbox,omitempty"`
	Egress  string            `json:"egress,omitempty" doc:"Egress preset name"`
	// @decision(DL-068)
	EgressExtraDomains []string `json:"egress_extra_domains,omitempty" maxItems:"16" doc:"Domains added to the egress preset (not with open), at most 16"`
	// Git authors the commits a job makes. Halves left empty fall back
	// to the caller's profile, then the server's configured identity.
	//
	// @decision(DL-002) @decision(DL-044)
	Git GitIdentityView `json:"git,omitempty"`
	// Check is the repo's own check command, run in the sandbox when
	// the run ends; its exit status is gate 1 of the verdict. It runs at
	// the session's directory, /workspace unless a landing is given, so a
	// relative path is workspace-relative (./spov/scripts/check.sh).
	// Empty leaves gate 1 unverified; a command that is set but cannot be
	// run (exit 126/127) vetoes success.
	//
	// @decision(DL-050) @decision(DL-037)
	Check string `json:"check,omitempty" doc:"Command whose exit status verifies the work, run at the workspace root, e.g. ./repo/scripts/check.sh"`
	// Model is OpenCode's own provider/model identifier. Omitted
	// resolves to the caller's stored default, then OpenCode's own
	// (DL-014).
	//
	// @decision(DL-014)
	Model string `json:"model,omitempty" doc:"Model to run on, as provider/model; OpenCode's default when empty"`
	// MaxDuration is how long the job may run unattended. Empty is the
	// caller's profile default, then 1h, clamped to the caller's limit.
	// MaxDurationPreferred marks it as a default from the client's
	// config, which is clamped to the limit rather than refused.
	//
	// @decision(DL-049)
	MaxDuration          string `json:"max_duration,omitempty" doc:"How long the job may run unattended, as a duration such as 45m; your default, else 1h, when empty"`
	MaxDurationPreferred bool   `json:"max_duration_preferred,omitempty" doc:"max_duration is the client's configured default: clamped to the limit rather than refused"`
	// GitAccess is how far the sandbox's forge credentials reach. Empty
	// takes the caller's job default, never the interactive one, then
	// repos.
	//
	// @decision(DL-043)
	GitAccess          store.GitAccess `json:"git_access,omitempty" enum:"repos,all" doc:"How far the forge credentials reach: the job's repositories, or every repository they can; the caller's job default, then repos, when empty"`
	GitAccessPreferred bool            `json:"git_access_preferred,omitempty" doc:"The git access is a saved preference, such as the CLI's config: a platform that cannot have all gets repos rather than refusing the job"`
	// @decision(DL-042)
	CredentialPreference map[string]string `json:"credential_preference,omitempty" doc:"Per configured platform, the credential tried first: user (your own, the default) or org (the organisation's)"`
	// Credentials, NoCredentials, CredentialsPreferred and Needs say
	// which routed credentials the launch attaches.
	//
	// @decision(DL-075)
	Credentials          []string `json:"credentials,omitempty" maxItems:"96" doc:"Attach exactly these routed credentials, asked-only ones included"`
	NoCredentials        bool     `json:"no_credentials,omitempty" doc:"Attach no routed credential"`
	CredentialsPreferred []string `json:"credentials_preferred,omitempty" maxItems:"96" doc:"The configuration's narrowing: keep only these of the default, or none for none; it never adds one"`
	Needs                []string `json:"needs,omitempty" maxItems:"16" doc:"Hosts the project needs; the launch notes any no routed credential covers"`
}

// JobView is a job as the API shows it.
//
// @decision(DL-045)
type JobView struct {
	ID      string         `json:"id" doc:"ULID"`
	Kind    store.JobKind  `json:"kind" enum:"pr,report"`
	State   store.JobState `json:"state" enum:"queued,provisioning,running,delivering,succeeded,failed,blocked,steered,cancelled"`
	OwnerID string         `json:"owner_id"`
	// SessionID is the ordinary session the job runs in, so a client
	// can reach its transcript, its log and `spov attach` for free.
	//
	// @decision(DL-045)
	SessionID string `json:"session_id"`
	Goal      string `json:"goal"`
	// Model is what the job was asked to run on, so a finished job's
	// verdict is comparable against another run's.
	//
	// @decision(DL-014)
	Model string `json:"model,omitempty" doc:"Model the job runs on, as provider/model; empty is OpenCode's default"`
	// Deliverables is the job's inventory, filled in by delivery. It
	// never carries a report's body, which comes from the file endpoint.
	//
	// @decision(DL-051) @decision(DL-060)
	Deliverables []DeliverableView `json:"deliverables" doc:"What the job delivered: its pull requests or reports, each with what was found about it. Report bodies are read from GET /api/v1/jobs/{id}/file."`
	// Partial is a success whose deliverable gate is partial.
	//
	// @decision(DL-050)
	Partial bool `json:"partial,omitempty" doc:"The job succeeded with part of its deliverables, or with unfinished work"`
	// Unfinished is what the agent declared it left undone.
	//
	// @decision(DL-050)
	Unfinished string `json:"unfinished,omitempty" doc:"What the agent said it left unfinished"`
	// Error is the terminal failure, or what a parked job is waiting
	// on. Redacted of anything secret before it is stored.
	Error string `json:"error,omitempty"`
	// Verdict is what the gates found, one line per gate. A succeeded
	// job has one too, which is why it is not folded into Error.
	//
	// @decision(DL-050)
	Verdict string `json:"verdict,omitempty" doc:"What each completion gate found"`
	// Gates is the same verdict as structure, parsed back out of the
	// text by jobViewOf so a client can draw each gate on its own. Do not
	// store it in a column of its own or parse it in the browser; see
	// DL-057.
	//
	// @decision(DL-057) @decision(DL-050)
	Gates     []jobs.GateResult `json:"gates,omitempty" doc:"Each completion gate's finding"`
	Attempt   int               `json:"attempt"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	StartedAt *time.Time        `json:"started_at,omitempty"`
	StoppedAt *time.Time        `json:"stopped_at,omitempty"`
	// MaxDuration, ClockUsed and ClockStartedAt are the wall clock: its
	// length, the time used by finished stretches, and when the current
	// stretch began, absent while the clock is stopped. A counting job's
	// clock runs out at ClockStartedAt + MaxDuration - ClockUsed.
	//
	// @decision(DL-049)
	MaxDuration    string     `json:"max_duration,omitempty" doc:"How long the job may run unattended"`
	ClockUsed      string     `json:"clock_used,omitempty" doc:"Unattended time used before the current stretch"`
	ClockStartedAt *time.Time `json:"clock_started_at,omitempty" doc:"When the current unattended stretch began; absent while the clock is stopped"`
}

// DeliverableView is one member of a job's inventory.
//
// @decision(DL-051)
type DeliverableView struct {
	Kind   store.DeliverableKind   `json:"kind" enum:"pr,report"`
	Status store.DeliverableStatus `json:"status" enum:"verified,missing,unverified,over_limit,withdrawn"`
	Detail string                  `json:"detail,omitempty" doc:"What was found about it, for a person"`
	// Platform, Repository and Branch say where a pull request is from;
	// Number and URL are set once the forge answered.
	Platform string `json:"platform,omitempty" doc:"The configured platform a pull request's repository is on"`
	// Forge decides the wording: a merge request on GitLab.
	//
	// @decision(DL-051)
	Forge      string `json:"forge,omitempty" doc:"The platform's kind: github, forgejo, gitea or gitlab, where a pull request is a merge request"`
	Repository string `json:"repository,omitempty" doc:"A pull request's repository, as its whole path (owner/name, or a GitLab group path and name)"`
	Branch     string `json:"branch,omitempty"`
	Number     int    `json:"number,omitempty" doc:"The pull request's number"`
	URL        string `json:"url,omitempty" doc:"The pull request's page"`
	// Title is a pull request's title or a report's first heading.
	Title string `json:"title,omitempty"`
	// Path and Size are a report's; Path is what the file endpoint takes.
	Path string `json:"path,omitempty" doc:"A report's path under the job's out directory"`
	Size int64  `json:"size,omitempty" doc:"A report's size in bytes"`
}

// deliverableViewOf projects one member.
//
// @decision(DL-051)
func deliverableViewOf(d store.Deliverable) DeliverableView {
	v := DeliverableView{
		Kind: d.Kind, Status: d.Status, Detail: d.Detail, Platform: d.Platform, Forge: d.Forge,
		Branch: d.Branch, Number: d.Number, URL: d.URL, Title: d.Title, Path: d.Path, Size: d.Size,
	}
	if d.Owner != "" || d.Name != "" {
		v.Repository = d.Owner + "/" + d.Name
	}
	return v
}

// jobViewOf projects a stored job with its inventory. The cursor is
// deliberately not on the view: it is the supervisor's bookkeeping and
// means nothing to a client.
//
// @decision(DL-045) @decision(DL-051)
func jobViewOf(j store.Job, members []store.Deliverable) JobView {
	v := JobView{
		ID: j.ID, Kind: j.Kind, State: j.State, OwnerID: j.OwnerID,
		SessionID: j.SessionID, Goal: j.Goal, Model: j.Model, Error: j.Error, Verdict: j.Verdict,
		Unfinished:   j.Unfinished,
		Deliverables: make([]DeliverableView, 0, len(members)),
		Gates:        jobs.ParseGatesNote(j.Verdict),
		Attempt:      j.Attempt, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
		StartedAt: j.StartedAt, StoppedAt: j.StoppedAt,
		ClockStartedAt: j.ClockStartedAt,
	}
	if j.MaxDuration > 0 {
		v.MaxDuration = j.MaxDuration.String()
	}
	// @decision(DL-049)
	if j.ClockUsed > 0 {
		v.ClockUsed = j.ClockUsed.String()
	}
	for _, d := range members {
		v.Deliverables = append(v.Deliverables, deliverableViewOf(d))
	}
	v.Partial = partialOf(j.State, v.Gates)
	return v
}

// partialOf reports whether a job is a partial success: succeeded, with
// a deliverable gate that is partial. A flag on the succeeded state,
// never a state of its own.
//
// @decision(DL-050) @decision(DL-060)
func partialOf(state store.JobState, gates []jobs.GateResult) bool {
	if state != store.JobSucceeded {
		return false
	}
	for _, g := range gates {
		if g.Gate == jobs.GateDeliverable && g.Outcome == jobs.GatePartial {
			return true
		}
	}
	return false
}

// jobViews projects jobs with their inventories, read in one query.
//
// @decision(DL-051) @decision(DL-060)
func (s *Server) jobViews(ctx context.Context, list []store.Job) ([]JobView, error) {
	ids := make([]string, 0, len(list))
	for _, j := range list {
		ids = append(ids, j.ID)
	}
	members, err := s.svc.JobDeliverables(ctx, principal(ctx), ids)
	if err != nil {
		return nil, err
	}
	out := make([]JobView, 0, len(list))
	for _, j := range list {
		out = append(out, jobViewOf(j, members[j.ID]))
	}
	return out, nil
}

// jobView is jobViews for one job.
func (s *Server) jobView(ctx context.Context, j store.Job) (JobView, error) {
	views, err := s.jobViews(ctx, []store.Job{j})
	if err != nil {
		return JobView{}, err
	}
	return views[0], nil
}

type createJobInput struct {
	Body JobRequest
}
type jobOutput struct {
	Body JobView
}
type jobIDInput struct {
	ID string `path:"id" doc:"Job ID"`
}

// listJobsInput narrows the listing. State is one comma-separated
// parameter (?state=queued,running): given the repeated form, huma keeps
// the first value and silently drops the rest. A new filter on a set must
// use the comma form too. Session lets the web app find one session's
// job without listing them all.
//
// @decision(DL-057) @decision(DL-045)
type listJobsInput struct {
	State   []string `query:"state" doc:"Keep only jobs in these states, comma-separated"`
	Session string   `query:"session" doc:"Keep only the job of this session"`
	// @decision(DL-012)
	Limit  int    `query:"limit" minimum:"0" doc:"How many to return, newest first: 50 when unset, at most 200"`
	Before string `query:"before" doc:"Return only jobs older than this ID: the next field of the previous page"`
}
type listJobsOutput struct {
	Body struct {
		Jobs []JobView `json:"jobs"`
		// @decision(DL-012)
		Next  string `json:"next,omitempty" doc:"Pass as before for the next page; absent on the last"`
		Total int    `json:"total" doc:"How many jobs match the filter in all"`
	}
}

// toJobHumaError maps the job errors that are not already covered by
// toHumaError.
//
// @decision(DL-045)
func toJobHumaError(err error) error {
	switch {
	case err == nil:
		return nil
	// Local mode has no supervisor. 501 rather than 404, so a client
	// does not conclude its URL was wrong.
	//
	// @decision(DL-046)
	case errors.Is(err, jobs.ErrJobsNeedAServer):
		return huma.Error501NotImplemented(err.Error())
	// A job that has already finished is not a bad request and not a
	// missing one: it is a state conflict, the same reading a stopped
	// session gets.
	case errors.Is(err, ErrJobNotCancellable):
		return huma.Error409Conflict(err.Error())
	// The same reading for a resume of a job that is not suspended: the
	// job exists and the caller may see it, but its state does not
	// admit the act.
	//
	// @decision(DL-054)
	case errors.Is(err, ErrJobNotResumable):
		return huma.Error409Conflict(err.Error())
	}
	return toHumaError(err)
}

func (s *Server) registerJobs() {
	register(s.api, huma.Operation{
		OperationID: "create-job", Method: http.MethodPost, Path: "/api/v1/jobs",
		Summary: "Create an autonomous job", DefaultStatus: http.StatusAccepted, Tags: []string{"jobs"},
		Errors: []int{http.StatusTooManyRequests, http.StatusNotImplemented},
	}, func(ctx context.Context, in *createJobInput) (*jobOutput, error) {
		job, err := s.svc.CreateJob(ctx, principal(ctx), in.Body)
		if err != nil {
			return nil, toJobHumaError(err)
		}
		// A new job has delivered nothing yet.
		return &jobOutput{Body: jobViewOf(job, nil)}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "list-jobs", Method: http.MethodGet, Path: "/api/v1/jobs",
		Summary: "List jobs", Tags: []string{"jobs"},
	}, func(ctx context.Context, in *listJobsInput) (*listJobsOutput, error) {
		f, err := jobFilterOf(in.State)
		if err != nil {
			return nil, toJobHumaError(err)
		}
		f.SessionID = strings.TrimSpace(in.Session)
		// @decision(DL-012)
		size := pageSize(in.Limit)
		f.Before, f.Limit = strings.TrimSpace(in.Before), size+1
		jobs, err := s.svc.ListJobs(ctx, principal(ctx), f)
		if err != nil {
			return nil, toJobHumaError(err)
		}
		out := &listJobsOutput{}
		jobs, out.Body.Next = pageOf(jobs, size, func(j store.Job) string { return j.ID })
		if out.Body.Total, err = s.svc.CountJobs(ctx, principal(ctx), f); err != nil {
			return nil, toJobHumaError(err)
		}
		if out.Body.Jobs, err = s.jobViews(ctx, jobs); err != nil {
			return nil, toJobHumaError(err)
		}
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "get-job", Method: http.MethodGet, Path: "/api/v1/jobs/{id}",
		Summary: "Get a job", Tags: []string{"jobs"},
	}, func(ctx context.Context, in *jobIDInput) (*jobOutput, error) {
		job, err := s.svc.GetJob(ctx, principal(ctx), in.ID)
		if err != nil {
			return nil, toJobHumaError(err)
		}
		view, err := s.jobView(ctx, job)
		if err != nil {
			return nil, toJobHumaError(err)
		}
		return &jobOutput{Body: view}, nil
	})

	// One collected file: a report or an image a report links. A store
	// read, never the sandbox's. The type is the one stored at
	// collection, and nothing the agent wrote can make it run in this
	// origin.
	//
	// @decision(DL-051) @decision(DL-063) @decision(DL-018)
	register(s.api, huma.Operation{
		OperationID: "get-job-file", Method: http.MethodGet, Path: "/api/v1/jobs/{id}/file",
		Summary: "Read a file a job delivered", Tags: []string{"jobs"},
	}, func(ctx context.Context, in *jobFileInput) (*huma.StreamResponse, error) {
		p := principal(ctx)
		f, err := s.svc.GetJobFile(ctx, p, in.ID, in.Path)
		if err != nil {
			return nil, toJobHumaError(err)
		}
		// A report that links collected images downloads as a zip of it
		// and them, so its links still work once saved. The zip is
		// streamed one file at a time.
		//
		// @decision(DL-051)
		var images []store.JobFile
		if in.Download {
			if images, err = s.svc.ReportImages(ctx, p, in.ID, f); err != nil {
				return nil, toJobHumaError(err)
			}
		}
		return &huma.StreamResponse{Body: func(hc huma.Context) {
			hc.SetHeader("X-Content-Type-Options", "nosniff")
			hc.SetHeader("Content-Security-Policy", "sandbox; default-src 'none'")
			// A collected file is stored once and never replaced.
			hc.SetHeader("Cache-Control", "private, max-age=31536000, immutable")
			name := downloadName(in.ID, f.Path)
			if len(images) == 0 {
				hc.SetHeader("Content-Type", f.ContentType)
				if in.Download {
					hc.SetHeader("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
				}
				_, _ = hc.BodyWriter().Write(f.Body)
				return
			}
			hc.SetHeader("Content-Type", "application/zip")
			hc.SetHeader("Content-Disposition", mime.FormatMediaType("attachment",
				map[string]string{"filename": strings.TrimSuffix(name, ".md") + ".zip"}))
			if err := s.svc.WriteReportBundle(ctx, p, in.ID, f, images, hc.BodyWriter()); err != nil {
				s.svc.deps.Warn("job " + in.ID + ": writing the report bundle: " + redact.Error(err))
			}
		}}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "resume-job", Method: http.MethodPost, Path: "/api/v1/jobs/{id}/resume",
		Summary: "Hand a suspended job back to its loop", DefaultStatus: http.StatusNoContent,
		Tags: []string{"jobs"}, Errors: []int{http.StatusConflict},
	}, func(ctx context.Context, in *jobIDInput) (*struct{}, error) {
		if err := s.svc.ResumeJob(ctx, principal(ctx), in.ID); err != nil {
			return nil, toJobHumaError(err)
		}
		return nil, nil
	})

	register(s.api, huma.Operation{
		OperationID: "cancel-job", Method: http.MethodDelete, Path: "/api/v1/jobs/{id}",
		Summary: "Cancel a job and terminate its sandbox", DefaultStatus: http.StatusNoContent,
		Tags: []string{"jobs"}, Errors: []int{http.StatusConflict},
	}, func(ctx context.Context, in *jobIDInput) (*struct{}, error) {
		if err := s.svc.CancelJob(ctx, principal(ctx), in.ID); err != nil {
			return nil, toJobHumaError(err)
		}
		return nil, nil
	})
}

type jobFileInput struct {
	ID       string `path:"id" doc:"Job ID"`
	Path     string `query:"path" required:"true" doc:"The file's path under the job's out directory, as a deliverable's path or a report's image link names it"`
	Download bool   `query:"download" doc:"Answer as an attachment to save"`
}

// downloadName is a saved file's name: the job's ID and the file's path,
// flattened.
//
// @decision(DL-051)
func downloadName(jobID, p string) string {
	return "spov-" + jobID + "-" + strings.ReplaceAll(p, "/", "-")
}

// jobFilterOf turns the query's state names into a filter, refusing one
// the machine does not have — a typo would otherwise return an empty
// list, which reads as "no such jobs" rather than "no such state".
//
// @decision(DL-045)
func jobFilterOf(states []string) (store.JobFilter, error) {
	f := store.JobFilter{}
	seen := map[store.JobState]bool{}
	for _, group := range states {
		// Split on commas here as well as relying on huma's parsing. A
		// repeated ?state= is not recoverable, since huma keeps only its
		// first value; see listJobsInput.
		for _, raw := range strings.Split(group, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			st := store.JobState(raw)
			if !store.ValidJobState(st) {
				return f, huma.Error422UnprocessableEntity("no such job state: " + raw)
			}
			if seen[st] {
				continue
			}
			seen[st] = true
			f.States = append(f.States, st)
		}
	}
	return f, nil
}
