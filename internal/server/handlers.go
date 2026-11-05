package server

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// SessionView is a session as the API shows it. It carries no secret.
//
// @decision(DL-005)
type SessionView struct {
	ID        string             `json:"id" doc:"ULID"`
	Name      string             `json:"name"`
	State     store.SessionState `json:"state" enum:"starting,running,stopped,failed"`
	OwnerID   string             `json:"owner_id"`
	SandboxID string             `json:"sandbox_id,omitempty"`
	Repos     []RepoRef          `json:"repos"`
	Clones    []string           `json:"clones,omitempty" doc:"Repositories cloned into the workspace, as owner/name"`
	Landing   string             `json:"landing,omitempty" doc:"Directory the server started in"`
	Model     string             `json:"model,omitempty" doc:"Model the session runs on, as provider/model; empty is OpenCode's default"`
	Error     string             `json:"error,omitempty"`
	// Image, Sandbox and Egress are the effective values the session was
	// created with, defaults applied, so a client can report what it is
	// paying for without knowing the server's configuration.
	//
	// @decision(DL-002)
	Image  string   `json:"image,omitempty"`
	Addons []string `json:"addons,omitempty" doc:"The image's add-ons as asked for, as name@version"`
	Apt    []string `json:"apt,omitempty" doc:"The image's extra Debian packages"`
	// BootedImage and InstalledAddons are what the sandbox got, which
	// can be an older build of the same add-ons than the newest.
	//
	// @decision(DL-033)
	BootedImage     string            `json:"booted_image,omitempty" doc:"The image the sandbox was created from"`
	InstalledAddons []string          `json:"installed_addons,omitempty" doc:"The add-ons the sandbox installs, at their exact versions, as name@version"`
	Sandbox         SandboxConfigView `json:"sandbox"`
	Egress          string            `json:"egress,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	StoppedAt       *time.Time        `json:"stopped_at,omitempty"`
	// EstimatedCost is this session's whole life in US dollars, from its
	// requested resources — a floor, never an invoice. It spans the
	// session, where /me/usage covers the calendar month, so a session
	// that started last month shows more here than it contributed to
	// that total.
	//
	// @decision(DL-055) @decision(DL-009)
	EstimatedCost float64 `json:"estimated_cost"`
	// LastMessage is the newest thing said in any of the session's
	// conversations, absent until a recorder has stored one.
	//
	// @decision(DL-018)
	LastMessage *MessageView `json:"last_message,omitempty"`
	// Job is the supervised job driving this session, if one is, on the
	// listing and on the single session. A session created by `spov fly`
	// has none and the field is absent.
	//
	// @decision(DL-060) @decision(DL-025)
	Job *SessionJobView `json:"job,omitempty"`
	// Credentials names what the sandbox was given for each platform:
	// the kind of credential, never anything about the secret.
	//
	// @decision(DL-043)
	Credentials []SandboxCredentialView `json:"credentials,omitempty" doc:"What the sandbox holds for each platform"`
	// GitAccess is what the launch asked for; each credential says what
	// its platform got.
	//
	// @decision(DL-043)
	GitAccess store.GitAccess `json:"git_access,omitempty" enum:"repos,all" doc:"The git access the launch asked for"`
	// Sync is the local branch and commit the session was seeded from,
	// when launched with --sync.
	//
	// @decision(DL-027)
	Sync *SessionSyncView `json:"sync,omitempty" doc:"The local git branch and commit the session started from"`
}

// SessionSyncView is the local branch and base commit a synced session
// started from.
//
// @decision(DL-027)
type SessionSyncView struct {
	Branch     string `json:"branch" minLength:"1"`
	BaseCommit string `json:"base_commit" minLength:"40" maxLength:"40"`
}

// SessionSyncRequest seeds a single-repository session from a local git
// branch and commit, with an optional base64-encoded git bundle of
// unpushed commits.
//
// @decision(DL-027)
type SessionSyncRequest struct {
	Branch     string `json:"branch" minLength:"1" doc:"Local git branch name to check out in the sandbox"`
	BaseCommit string `json:"base_commit" minLength:"40" maxLength:"40" doc:"40-character hex commit SHA of the local HEAD"`
	Bundle     string `json:"bundle,omitempty" doc:"Optional base64-encoded git bundle of commits ahead of the remote"`
}

// SyncExportView is the repository state exported from a running sandbox
// for local sync-back.
//
// @decision(DL-027)
type SyncExportView struct {
	Branch      string   `json:"branch,omitempty" doc:"Current branch in the sandbox; empty when detached"`
	BaseCommit  string   `json:"base_commit,omitempty" doc:"Commit SHA the session started from"`
	HeadCommit  string   `json:"head_commit" doc:"Current HEAD commit SHA in the sandbox"`
	Bundle      string   `json:"bundle,omitempty" doc:"Base64-encoded git bundle of commits between base_commit and head_commit"`
	WorktreeTar string   `json:"worktree_tar,omitempty" doc:"Base64-encoded gzipped tar of modified, staged and untracked non-ignored files"`
	Deleted     []string `json:"deleted,omitempty" doc:"Tracked repository-relative paths deleted in the working tree or index"`
}

// SandboxCredentialView is one platform's credential kind: `renewed` is a GitHub
// App token narrowed to the session's repositories and re-minted while it
// is in use; `stored` is a stored token, handed over as it is.
//
// @decision(DL-043)
type SandboxCredentialView struct {
	Platform string               `json:"platform"`
	Kind     store.CredentialKind `json:"kind" enum:"renewed,stored"`
	// @decision(DL-034)
	Holder store.CredentialHolder `json:"holder,omitempty" enum:"proxy,sandbox" doc:"Where the token is kept: the proxy sidecar, or the sandbox itself. Absent when not recorded."`
	Forge  string                 `json:"forge,omitempty" doc:"The platform's kind: github, forgejo, gitea or gitlab."`
	// @decision(DL-043)
	Access store.GitAccess `json:"access,omitempty" enum:"repos,all" doc:"How far the token reaches: the session's repositories, or every repository it can. Absent on a session recorded before this was, which is repos."`
	// @decision(DL-042)
	Tier string `json:"tier,omitempty" enum:"user,org" doc:"Whose credential it is: the user's own, or the organisation's shared one. Absent on a session recorded before this was."`
}

// SessionJobView is the little of a job a LIST needs: enough to draw a
// badge and link, and nothing more. It deliberately carries no `result`,
// `goal` or `verdict`.
//
// @decision(DL-060)
type SessionJobView struct {
	ID    string         `json:"id" doc:"ULID"`
	Kind  store.JobKind  `json:"kind" enum:"pr,report"`
	State store.JobState `json:"state" enum:"queued,provisioning,running,delivering,succeeded,failed,blocked,steered,cancelled"`
	// Partial marks a success that delivered part of what was asked,
	// as JobView.Partial does. A flag on succeeded, not a state.
	//
	// @decision(DL-060)
	Partial bool `json:"partial,omitempty"`
}

// sessionJobView is the job badge the session list and page draw.
//
// @decision(DL-060)
func sessionJobView(j store.Job) *SessionJobView {
	return &SessionJobView{ID: j.ID, Kind: j.Kind, State: j.State, Partial: partialOf(j.State, jobs.ParseGatesNote(j.Verdict))}
}

// RepoRef names a repository on a configured platform (or a local-only
// synced repository when Platform is empty).
type RepoRef struct {
	Platform string `json:"platform,omitempty"`
	Owner    string `json:"owner" minLength:"1"`
	Name     string `json:"name" minLength:"1"`
}

// SandboxConfigView is the resource shape a request may set. Unset fields
// take the server's defaults.
type SandboxConfigView struct {
	CPU            float64  `json:"cpu,omitempty" minimum:"0"`
	CPULimit       float64  `json:"cpu_limit,omitempty" minimum:"0"`
	MemoryMiB      int      `json:"memory_mib,omitempty" minimum:"0"`
	MemoryLimitMiB int      `json:"memory_limit_mib,omitempty" minimum:"0"`
	TimeoutSecs    int      `json:"timeout_secs,omitempty" minimum:"0"`
	IdleSecs       int      `json:"idle_timeout_secs,omitempty" minimum:"0"`
	Regions        []string `json:"regions,omitempty"`
	Cloud          string   `json:"cloud,omitempty"`
	Workdir        string   `json:"workdir,omitempty"`
	InboundCIDRs   []string `json:"inbound_cidrs,omitempty"`
	Persist        bool     `json:"persist,omitempty"`
}

// SessionRequest creates a session.
//
// @decision(DL-005)
type SessionRequest struct {
	Name    string            `json:"name,omitempty" maxLength:"63" doc:"Session and sandbox name; generated when empty"`
	Repos   []RepoRef         `json:"repos" minItems:"1"`
	Landing string            `json:"landing,omitempty" doc:"Workspace directory to start in; the workspace root when empty"`
	Image   string            `json:"image,omitempty"`
	Sandbox SandboxConfigView `json:"sandbox,omitempty"`
	Egress  string            `json:"egress,omitempty" doc:"Egress preset name"`
	// Model is OpenCode's own provider/model identifier. Omitted means
	// whatever OpenCode resolves.
	//
	// @decision(DL-014)
	Model string `json:"model,omitempty" doc:"Model to run on, as provider/model; OpenCode's default when empty"`
	// Git is the author of commits made in the sandbox. The client
	// resolves it, since only the client can read the user's own git
	// configuration; unset halves fall back to the server's identity.
	//
	// @decision(DL-002) @decision(DL-044)
	Git GitIdentityView `json:"git,omitempty"`
	// GitAccess is how far the sandbox's forge credentials reach. Empty
	// takes the caller's stored default, then repos.
	//
	// @decision(DL-043)
	GitAccess          store.GitAccess `json:"git_access,omitempty" enum:"repos,all" doc:"How far the forge credentials reach: the session's repositories, or every repository they can; the caller's default, then repos, when empty"`
	GitAccessPreferred bool            `json:"git_access_preferred,omitempty" doc:"The git access is a saved preference, such as the CLI's config: a platform that cannot have all gets repos rather than refusing the launch"`
	// @decision(DL-042)
	CredentialPreference map[string]string `json:"credential_preference,omitempty" doc:"Per configured platform, the credential tried first: user (your own, the default) or org (the organisation's); the other is used only when that one is absent"`
	// Addons and Apt ask for an image variant: curated add-ons as
	// `name[@version]`, and extra Debian packages. Neither combines with
	// Image.
	//
	// @decision(DL-033)
	Addons []string `json:"addons,omitempty" maxItems:"16" doc:"Curated add-ons as name or name@version, e.g. rust, java@21; the image is a variant of the base"`
	Apt    []string `json:"apt,omitempty" maxItems:"32" doc:"Extra Debian packages for the image variant"`
	// Sync seeds the sandbox repository from a local git branch and
	// commit (with an optional bundle of unpushed commits).
	//
	// @decision(DL-027)
	Sync *SessionSyncRequest `json:"sync,omitempty" doc:"Seed the repository from a local git branch and commit"`
}

// GitIdentityView is a commit author.
type GitIdentityView struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

func (r SessionRequest) toStore() (store.SessionRequest, error) {
	repos := make([]store.RepoRef, len(r.Repos))
	for i, ref := range r.Repos {
		repos[i] = store.RepoRef(ref)
	}
	sync, err := r.syncToStore()
	if err != nil {
		return store.SessionRequest{}, err
	}
	return store.SessionRequest{
		Name: r.Name, Repos: repos, Landing: r.Landing, Image: r.Image, Egress: r.Egress,
		Sandbox: r.Sandbox.toStore(), GitName: r.Git.Name, GitEmail: r.Git.Email,
		Model:     r.Model,
		GitAccess: r.GitAccess, GitAccessPreferred: r.GitAccessPreferred,
		// @decision(DL-042)
		CredentialPreference: r.CredentialPreference,
		Addons:               r.Addons, Apt: r.Apt,
		// @decision(DL-027)
		Sync: sync,
	}, nil
}

func (sb SandboxConfigView) toStore() store.SandboxConfig {
	return store.SandboxConfig{
		CPU: sb.CPU, CPULimit: sb.CPULimit, MemoryMiB: sb.MemoryMiB, MemoryLimitMiB: sb.MemoryLimitMiB,
		Timeout: time.Duration(sb.TimeoutSecs) * time.Second, IdleTimeout: time.Duration(sb.IdleSecs) * time.Second,
		Regions: sb.Regions, Cloud: sb.Cloud, Workdir: sb.Workdir, InboundCIDRs: sb.InboundCIDRs, Persist: sb.Persist,
	}
}

func sandboxViewOf(sb store.SandboxConfig) SandboxConfigView {
	return SandboxConfigView{
		CPU: sb.CPU, CPULimit: sb.CPULimit, MemoryMiB: sb.MemoryMiB, MemoryLimitMiB: sb.MemoryLimitMiB,
		TimeoutSecs: int(sb.Timeout / time.Second), IdleSecs: int(sb.IdleTimeout / time.Second),
		Regions: sb.Regions, Cloud: sb.Cloud, Workdir: sb.Workdir, InboundCIDRs: sb.InboundCIDRs, Persist: sb.Persist,
	}
}

// viewOf projects a stored session. now is the clock the cost is
// measured against, so a running session's figure advances with it.
//
// @decision(DL-055)
func viewOf(s store.Session, now time.Time) SessionView {
	repos := make([]RepoRef, len(s.Request.Repos))
	for i, ref := range s.Request.Repos {
		repos[i] = RepoRef(ref)
	}
	var syncView *SessionSyncView
	if s.Request.Sync != nil {
		syncView = &SessionSyncView{Branch: s.Request.Sync.Branch, BaseCommit: s.Request.Sync.BaseCommit}
	}
	return SessionView{
		ID: s.ID, Name: s.Name, State: s.State, OwnerID: s.OwnerID, SandboxID: s.SandboxID,
		Repos: repos, Clones: s.Clones, Landing: s.Landing, Error: s.Error,
		Model: s.Request.Model,
		Image: s.Request.Image, Sandbox: sandboxViewOf(s.Request.Sandbox), Egress: s.Request.Egress,
		Addons: s.Request.Addons, Apt: s.Request.Apt,
		BootedImage: s.Booted.Image, InstalledAddons: s.Booted.Addons,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, StoppedAt: s.StoppedAt,
		Credentials: credentialViews(s.Credentials),
		GitAccess:   s.Request.GitAccess,
		Sync:        syncView,
		EstimatedCost: round2(sandbox.EstimatedCost(
			s.Request.Sandbox.CPU, s.Request.Sandbox.MemoryMiB,
			// The session's whole billable life: the same helper the
			// monthly total uses, with the period opened at its own
			// creation.
			billableWithin(s, s.CreatedAt, now),
		)),
	}
}

// credentialViews projects a session's stored credential kinds.
//
// @decision(DL-043)
func credentialViews(creds []store.SessionCredential) []SandboxCredentialView {
	if len(creds) == 0 {
		return nil
	}
	out := make([]SandboxCredentialView, len(creds))
	for i, c := range creds {
		out[i] = SandboxCredentialView(c)
	}
	return out
}

// viewsOf projects a page of sessions and fills each one's last message
// from a single store query, so a list of N sessions costs one read and
// no sandbox read.
//
// A last message that cannot be read is omitted rather than fatal: the
// list is the session's state, and decoration must not withhold it.
//
// @decision(DL-018)
func (s *Server) viewsOf(ctx context.Context, p auth.Principal, sessions []store.Session, now time.Time) []SessionView {
	out := make([]SessionView, 0, len(sessions))
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, viewOf(sess, now))
		ids = append(ids, sess.ID)
	}
	if len(ids) == 0 {
		return out
	}
	last, err := s.deps.Store.Conversations().LastMessages(ctx, p.OrgID, ids)
	if err != nil {
		if ctx.Err() == nil {
			s.deps.Warn(fmt.Sprintf("last messages: %s", redact.Error(err)))
		}
	} else {
		for i, sess := range sessions {
			if m, ok := last[sess.ID]; ok {
				view := messageViewOf(m)
				out[i].LastMessage = &view
			}
		}
	}
	// The jobs driving these sessions, in one query for the page. A
	// failure here is warned about and dropped, not returned, so the read
	// above does not return early on failure either.
	//
	// @decision(DL-060) @decision(DL-018)
	jobs, err := s.deps.Store.Jobs().BySessions(ctx, p.OrgID, ids)
	if err != nil {
		if ctx.Err() == nil {
			s.deps.Warn(fmt.Sprintf("jobs by session: %s", redact.Error(err)))
		}
		return out
	}
	for i, sess := range sessions {
		if j, ok := jobs[sess.ID]; ok {
			out[i].Job = sessionJobView(j)
		}
	}
	return out
}

// sessionJob is the job driving one session, as the listing decorates
// its rows. A failed read is warned about and leaves the field out.
//
// @decision(DL-025) @decision(DL-060)
func (s *Server) sessionJob(ctx context.Context, sess store.Session) *SessionJobView {
	jobs, err := s.deps.Store.Jobs().BySessions(ctx, sess.OrgID, []string{sess.ID})
	if err != nil {
		if ctx.Err() == nil {
			s.deps.Warn(fmt.Sprintf("job of session %s: %s", sess.ID, redact.Error(err)))
		}
		return nil
	}
	j, ok := jobs[sess.ID]
	if !ok {
		return nil
	}
	return sessionJobView(j)
}

// decorationStamp reports what the page's decorated columns hold, as
// the feed's fingerprint. A read that fails is a constant rather than
// an empty stamp, so a store that is failing does not read as a list
// whose messages all vanished. The session's own fields are not in
// here: the feed already re-lists them.
//
// @decision(DL-060) @decision(DL-018)
func (s *Server) decorationStamp(ctx context.Context, p auth.Principal, sessions []store.Session) string {
	if len(sessions) == 0 {
		return ""
	}
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
	}
	last, err := s.deps.Store.Conversations().LastMessages(ctx, p.OrgID, ids)
	if err != nil {
		return "?"
	}
	jobs, err := s.deps.Store.Jobs().BySessions(ctx, p.OrgID, ids)
	if err != nil {
		return "?"
	}
	// The hash of nothing, so a session with neither decoration is
	// skipped rather than contributing a constant line to every stamp.
	empty := fnv.New64a().Sum64()
	var b strings.Builder
	for _, id := range ids {
		h := fnv.New64a()
		if m, ok := last[id]; ok {
			// The row itself, not only its identity: a message is
			// upserted on OpenCode's id, so the catch-up read's partial
			// reply and the finished one that replaces it are the same
			// row.
			//
			// hash.Hash never fails a write.
			_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s", m.ID, m.Role, m.At.UnixNano(), m.Text)
		}
		if j, ok := jobs[id]; ok {
			// Only what the listing shows.
			//
			// @decision(DL-060)
			v := sessionJobView(j)
			_, _ = fmt.Fprintf(h, "\x00job\x00%s\x00%s\x00%s\x00%t", v.ID, v.Kind, v.State, v.Partial)
		}
		if sum := h.Sum64(); sum != empty {
			fmt.Fprintf(&b, "%s:%x\n", id, sum)
		}
	}
	return b.String()
}

// RepoView is a repository the caller may bootstrap.
type RepoView struct {
	Platform string `json:"platform"`
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	// UpdatedAt orders a picker so recent work is first.
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	// Via names the credential tiers that reach it, on a hosted server;
	// a launch reaches it only through the tier its platform resolves
	// to (see the listing's platforms).
	//
	// @decision(DL-042)
	Via []Tier `json:"via,omitempty" enum:"user,org"`
	// MaybeReadOnly marks a public repository the caller's own
	// fine-grained token lists, which it may only read.
	//
	// @decision(DL-042)
	MaybeReadOnly bool `json:"maybe_read_only,omitempty" doc:"A public repository your fine-grained token can read, and can push to only if you gave it this repository"`
}

// MeView is the caller's principal.
type MeView struct {
	UserID string `json:"user_id"`
	OrgID  string `json:"org_id"`
	Email  string `json:"email,omitempty"`
	Admin  bool   `json:"admin"`
	// GitName and GitEmail author commits made in this user's sessions.
	//
	// @decision(DL-044)
	GitName  string `json:"git_name,omitempty"`
	GitEmail string `json:"git_email,omitempty"`
	// DefaultModel is the provider/model reference this caller's
	// launches run on when they name none. Empty leaves the choice to
	// OpenCode.
	//
	// @decision(DL-014)
	DefaultModel string `json:"default_model,omitempty"`
	// DefaultGitAccess and DefaultJobGitAccess are the git access the
	// caller's sessions and jobs get when the launch names none. Empty
	// is repos.
	//
	// @decision(DL-043)
	DefaultGitAccess    store.GitAccess `json:"default_git_access,omitempty" enum:"repos,all"`
	DefaultJobGitAccess store.GitAccess `json:"default_job_git_access,omitempty" enum:"repos,all"`
	// DefaultJobDuration is the wall clock the caller's jobs get when
	// they name none. Empty is the built-in default.
	//
	// @decision(DL-049)
	DefaultJobDuration string `json:"default_job_duration,omitempty" doc:"The wall clock jobs get when they name none, such as 2h"`
	// Quota is the caller's effective limits, so a client can say why a
	// create will be refused before making it.
	//
	// @decision(DL-009)
	Quota QuotaView `json:"quota"`
}

// patchMeInput updates the caller's profile. The fields are pointers so
// an omitted one is left alone and an empty one clears it.
//
// @decision(DL-062)
type patchMeInput struct {
	Body struct {
		GitName  *string `json:"git_name,omitempty" doc:"The name commits are authored with"`
		GitEmail *string `json:"git_email,omitempty" doc:"The e-mail commits are authored with"`
		// @decision(DL-014)
		DefaultModel *string `json:"default_model,omitempty" doc:"The model launches run on when they name none; empty clears it"`
		// @decision(DL-043)
		DefaultGitAccess    *string `json:"default_git_access,omitempty" doc:"The git access sessions get when they name none, repos or all; empty clears it"`
		DefaultJobGitAccess *string `json:"default_job_git_access,omitempty" doc:"The git access jobs get when they name none, repos or all; empty clears it"`
		// @decision(DL-049)
		DefaultJobDuration *string `json:"default_job_duration,omitempty" doc:"The wall clock jobs get when they name none, such as 2h; empty clears it"`
	}
}

type createInput struct {
	Body SessionRequest
}
type createOutput struct {
	Body struct {
		ID string `json:"id"`
	}
}
type refInput struct {
	Ref string `path:"ref" doc:"Session ID or name"`
}
type sessionOutput struct {
	Body SessionView
}

// listSessionsInput pages and filters the session listing.
//
// @decision(DL-012)
type listSessionsInput struct {
	Limit  int      `query:"limit" minimum:"0" doc:"How many to return, newest first: 50 when unset, at most 200"`
	Before string   `query:"before" doc:"Return only sessions older than this ID: the next field of the previous page"`
	State  []string `query:"state" doc:"Keep only sessions in these states, comma-separated"`
}

type listOutput struct {
	Body struct {
		Sessions []SessionView `json:"sessions"`
		// Next is the before value for the following page, empty when
		// nothing older matches; Total counts every match.
		//
		// @decision(DL-012)
		Next  string `json:"next,omitempty" doc:"Pass as before for the next page; absent on the last"`
		Total int    `json:"total" doc:"How many sessions match the filter in all"`
	}
}
type reposOutput struct {
	Body struct {
		Repos []RepoView `json:"repos"`
		// Warnings name the platforms that could not be listed, so a
		// short list is not mistaken for the whole picture.
		//
		// @decision(DL-028) @decision(DL-002)
		Warnings []string `json:"warnings,omitempty"`
		// Platforms says which tier supplied each platform's
		// credential, and why one is missing.
		//
		// @decision(DL-042)
		Platforms []PlatformTier `json:"platforms,omitempty"`
	}
}
type meOutput struct {
	Body MeView
}

// ConnectionView is a minted credential set. Every field is a secret or
// names one, so the response is never cached.
//
// @decision(DL-030)
type ConnectionView struct {
	URL             string    `json:"url" doc:"Edge endpoint for the session's port"`
	Token           string    `json:"token" doc:"Connect token, sent as the _modal_connect_token cookie"`
	User            string    `json:"user" doc:"Basic auth user for the OpenCode server"`
	Password        string    `json:"password" doc:"Basic auth password for the OpenCode server"`
	ExpiresAt       time.Time `json:"expires_at" doc:"When the token stops working"`
	IdleTimeoutSecs int       `json:"idle_timeout_secs,omitempty" doc:"Sandbox idle timeout, for pacing a heartbeat"`
}

type connectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ConnectionView
}

// ProblemTypeSessionStopped is the problem type for acting on a stopped
// session.
const ProblemTypeSessionStopped = "https://spov.dev/problems/session-stopped"

// asStatusError finds a huma error a handler produced, however it was
// wrapped on the way back.
//
// @decision(DL-007)
func asStatusError(err error) error {
	var se huma.StatusError
	if errors.As(err, &se) {
		return se
	}
	return nil
}

// toHumaError maps domain errors to problem responses. Provider messages
// pass through; credentials never appear in them.
//
// @decision(DL-005)
func toHumaError(err error) error {
	// A handler that decided the answer itself keeps it. Guards inside
	// a Store.Tx callback come back out through here, and mapping one
	// to 502 would report a refusal as a broken dependency. Checked
	// before the sentinels below, so a refusal that happens to wrap one
	// of them is still reported as the refusal.
	//
	// @decision(DL-007)
	if se := asStatusError(err); se != nil {
		return se
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound), errors.Is(err, sandbox.ErrNotFound):
		return huma.Error404NotFound("no such session")
	case errors.Is(err, ErrTooManyMints):
		return huma.Error429TooManyRequests(err.Error())
	// A quota is not a malformed request: it would succeed for someone
	// else, and for this user once a session ends.
	//
	// @decision(DL-009)
	case errors.Is(err, ErrQuotaExceeded):
		return huma.Error429TooManyRequests(err.Error())
	case errors.Is(err, ErrPasswordUnavailable), errors.Is(err, session.ErrNoPassword):
		// Never a fabricated password: it would authenticate nowhere and
		// fail at the proxy with a far less obvious message.
		//
		// @decision(DL-003)
		return huma.Error502BadGateway(redact.Error(err))
	case errors.Is(err, ErrNameInUse):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, ErrInvalidRequest):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, session.ErrStopped), errors.Is(err, ErrSessionNotRunning):
		e := huma.Error409Conflict(err.Error())
		if m, ok := e.(*huma.ErrorModel); ok {
			m.Type = ProblemTypeSessionStopped
		}
		return e
	case errors.Is(err, context.DeadlineExceeded):
		return huma.Error504GatewayTimeout(redact.Error(err))
	default:
		return huma.Error502BadGateway(redact.Error(err))
	}
}

func principal(ctx context.Context) auth.Principal {
	p, _ := auth.From(ctx)
	return p
}

func (s *Server) registerSessions() {
	register(s.api, huma.Operation{
		OperationID: "create-session", Method: http.MethodPost, Path: "/api/v1/sessions",
		Summary: "Create a session", DefaultStatus: http.StatusAccepted, Tags: []string{"sessions"},
	}, func(ctx context.Context, in *createInput) (*createOutput, error) {
		req, err := in.Body.toStore()
		if err != nil {
			return nil, toHumaError(err)
		}
		sess, err := s.svc.Create(ctx, principal(ctx), req)
		if err != nil {
			return nil, toHumaError(err)
		}
		out := &createOutput{}
		out.Body.ID = sess.ID
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "list-sessions", Method: http.MethodGet, Path: "/api/v1/sessions",
		Summary: "List sessions", Tags: []string{"sessions"},
	}, func(ctx context.Context, in *listSessionsInput) (*listOutput, error) {
		// @decision(DL-012)
		f, err := sessionFilterOf(in.State)
		if err != nil {
			return nil, err
		}
		size := pageSize(in.Limit)
		f.Before, f.Limit = strings.TrimSpace(in.Before), size+1
		sessions, err := s.svc.List(ctx, principal(ctx), f)
		if err != nil {
			return nil, toHumaError(err)
		}
		out := &listOutput{}
		sessions, out.Body.Next = pageOf(sessions, size, func(x store.Session) string { return x.ID })
		if out.Body.Total, err = s.svc.CountSessions(ctx, principal(ctx), f); err != nil {
			return nil, toHumaError(err)
		}
		out.Body.Sessions = s.viewsOf(ctx, principal(ctx), sessions, s.deps.Now())
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "get-session", Method: http.MethodGet, Path: "/api/v1/sessions/{ref}",
		Summary: "Get a session by ID or name", Tags: []string{"sessions"},
	}, func(ctx context.Context, in *refInput) (*sessionOutput, error) {
		sess, err := s.svc.Get(ctx, principal(ctx), in.Ref)
		if err != nil {
			return nil, toHumaError(err)
		}
		view := viewOf(sess, s.deps.Now())
		view.Job = s.sessionJob(ctx, sess)
		return &sessionOutput{Body: view}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "delete-session", Method: http.MethodDelete, Path: "/api/v1/sessions/{ref}",
		Summary: "Terminate a session's sandbox and revoke its tokens", DefaultStatus: http.StatusNoContent, Tags: []string{"sessions"},
	}, func(ctx context.Context, in *refInput) (*struct{}, error) {
		// DeleteSession, not Delete: a session a job owns is stopped
		// through the job, or the job outlives the stop.
		//
		// @decision(DL-045)
		if err := s.svc.DeleteSession(ctx, principal(ctx), in.Ref); err != nil {
			return nil, toHumaError(err)
		}
		return nil, nil
	})

	register(s.api, huma.Operation{
		OperationID: "connect-session", Method: http.MethodPost, Path: "/api/v1/sessions/{ref}/connect",
		Summary: "Mint connection credentials for a session", Tags: []string{"sessions"},
		Errors: []int{http.StatusTooManyRequests},
	}, func(ctx context.Context, in *refInput) (*connectOutput, error) {
		conn, err := s.svc.Connect(ctx, principal(ctx), in.Ref)
		if err != nil {
			return nil, toHumaError(err)
		}
		out := &connectOutput{}
		out.Body = ConnectionView{
			URL: conn.URL, Token: conn.Token, User: conn.User, Password: conn.Password,
			ExpiresAt: conn.ExpiresAt, IdleTimeoutSecs: int(conn.IdleTimeout / time.Second),
		}
		// Credentials must not be cached anywhere on the way back.
		out.CacheControl = "no-store"
		return out, nil
	})

	// @decision(DL-027)
	s.registerSyncExport()

	registerSSE(s.api, huma.Operation{
		OperationID: "session-events", Method: http.MethodGet, Path: "/api/v1/sessions/{ref}/events",
		Summary: "Stream a session's launch steps until it is ready or failed", Tags: []string{"sessions"},
	}, map[string]any{
		"step":  StepEvent{},
		"note":  NoteEvent{},
		"ready": ReadyEvent{},
		"error": ErrorEvent{},
	}, s.streamEvents)
}

// streamEvents replays a launch log from the beginning and follows it
// until its terminal event. A session whose launch this process did not
// run gets a single terminal event derived from its state.
//
// @decision(DL-005)
func (s *Server) streamEvents(ctx context.Context, in *refInput, send sse.Sender) {
	// A launch that ignores its context would otherwise hold the
	// stream open forever.
	ctx, cancel := context.WithTimeout(ctx, s.deps.LaunchTimeout+30*time.Second)
	defer cancel()
	sess, err := s.svc.Get(ctx, principal(ctx), in.Ref)
	if err != nil {
		_ = send.Data(ErrorEvent{Problem: ProblemView{Title: "Not Found", Status: 404, Detail: "no such session"}})
		return
	}
	log, ok := s.svc.Log(sess.ID)
	if !ok {
		s.streamFromStore(ctx, sess, send)
		return
	}
	from := 0
	for {
		events, done, wait := log.snapshot(from)
		for _, ev := range events {
			if err := send.Data(ev); err != nil {
				return
			}
		}
		from += len(events)
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-wait:
		}
	}
}

// listReposInput carries the CLI's preference, so the listing's
// platforms say which tier its launch would use.
//
// @decision(DL-042)
type listReposInput struct {
	PreferShared string `query:"prefer_shared" doc:"Comma-separated platform names whose launch prefers the organisation's credential"`
}

// preferShared is the tier policy a comma-separated platform list
// prefers the organisation's tier on.
//
// @decision(DL-042)
func preferShared(list string) TierPolicy {
	var p TierPolicy
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name != "" && len(name) <= 100 {
			if p.Prefer == nil {
				p.Prefer = map[string]Tier{}
			}
			p.Prefer[name] = TierOrg
		}
	}
	return p
}

// mergeTiers folds each tier's listing into one row per repository,
// naming the tiers that reach it, newest first as each listing was.
//
// @decision(DL-042)
func mergeTiers(tiered []TieredRepo) []RepoView {
	out := make([]RepoView, 0, len(tiered))
	at := map[string]int{}
	for _, r := range tiered {
		key := r.Platform.Name() + "\x00" + r.Repo.Owner + "\x00" + r.Repo.Name
		if i, ok := at[key]; ok {
			if !slices.Contains(out[i].Via, r.Tier) {
				out[i].Via = append(out[i].Via, r.Tier)
			}
			out[i].MaybeReadOnly = out[i].MaybeReadOnly || r.MaybeReadOnly
			continue
		}
		at[key] = len(out)
		out = append(out, RepoView{
			Platform: r.Platform.Name(), Owner: r.Repo.Owner, Name: r.Repo.Name,
			Private: r.Repo.Private, Archived: r.Repo.Archived, UpdatedAt: r.Repo.UpdatedAt, Via: []Tier{r.Tier},
			MaybeReadOnly: r.MaybeReadOnly,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

func (s *Server) registerRepos() {
	register(s.api, huma.Operation{
		OperationID: "list-repos", Method: http.MethodGet, Path: "/api/v1/repos",
		Summary: "List repositories across every configured platform", Tags: []string{"repos"},
	}, func(ctx context.Context, in *listReposInput) (*reposOutput, error) {
		// A repository is offered only through a credential that can
		// reach it, so the listing is the caller's own. Its platforms
		// are resolved as the caller's launch would be: personal-first,
		// or preferring the shared tier where the CLI says so.
		//
		// @decision(DL-042)
		p := principal(ctx)
		forge, tiers, err := s.svc.forgeFor(ctx, p.OrgID, p.UserID, preferShared(in.PreferShared))
		if err != nil {
			return nil, toHumaError(err)
		}
		out := &reposOutput{}
		out.Body.Platforms = tiers
		// Both tiers, each repository marked with the tiers that reach
		// it; the platforms say which one a browser launch uses.
		//
		// @decision(DL-042)
		if s.deps.UserRepos != nil && p.UserID != "" {
			tiered, warnings, err := s.deps.UserRepos(ctx, p.OrgID, p.UserID)
			if err != nil {
				return nil, toHumaError(err)
			}
			out.Body.Warnings = warnings
			out.Body.Repos = mergeTiers(tiered)
			return out, nil
		}
		repos, warnings, err := forge.ListRepos(ctx)
		if err != nil {
			return nil, toHumaError(err)
		}
		out.Body.Warnings = warnings
		out.Body.Repos = make([]RepoView, 0, len(repos))
		for _, r := range repos {
			out.Body.Repos = append(out.Body.Repos, RepoView{
				Platform: r.Platform.Name(), Owner: r.Repo.Owner, Name: r.Repo.Name,
				Private: r.Repo.Private, Archived: r.Repo.Archived, UpdatedAt: r.Repo.UpdatedAt,
			})
		}
		return out, nil
	})
}

func (s *Server) registerMe() {
	register(s.api, huma.Operation{
		OperationID: "me", Method: http.MethodGet, Path: "/api/v1/me",
		Summary: "The caller's identity", Tags: []string{"auth"},
	}, func(ctx context.Context, _ *struct{}) (*meOutput, error) {
		p := principal(ctx)
		return &meOutput{Body: s.meView(ctx, p)}, nil
	})

	// The git identity is the user's own to change: it is initialised
	// from their GitHub profile at first login and then left alone.
	//
	// @decision(DL-044)
	register(s.api, huma.Operation{
		OperationID: "update-me", Method: http.MethodPatch, Path: "/api/v1/me",
		Summary: "Update the caller's settings", Tags: []string{"auth"},
	}, func(ctx context.Context, in *patchMeInput) (*meOutput, error) {
		p := principal(ctx)
		if p.UserID == "" || s.deps.Store == nil {
			return nil, huma.Error501NotImplemented("this server has no user profiles to update")
		}
		// Checked before anything is written, against the same catalogue
		// and keys a launch asks about.
		//
		// @decision(DL-014)
		if in.Body.DefaultModel != nil {
			if _, err := s.svc.checkModel(ctx, p,
				strings.TrimSpace(*in.Body.DefaultModel), modelFromSettings); err != nil {
				return nil, toHumaError(err)
			}
		}
		// @decision(DL-043)
		gitAccess, err := parseAccessFields(in.Body.DefaultGitAccess, in.Body.DefaultJobGitAccess)
		if err != nil {
			return nil, toHumaError(err)
		}
		// @decision(DL-049)
		var jobDuration time.Duration
		if in.Body.DefaultJobDuration != nil {
			if jobDuration, err = parseJobDuration("default_job_duration", *in.Body.DefaultJobDuration); err != nil {
				return nil, toHumaError(err)
			}
		}
		// apply is every field this request named, and nothing else. It
		// is a function rather than a block because the row it is
		// applied to may have to be re-read: see the conflict below.
		apply := func(u store.User) store.User {
			if in.Body.GitName != nil {
				u.GitName = strings.TrimSpace(*in.Body.GitName)
			}
			if in.Body.GitEmail != nil {
				u.GitEmail = strings.TrimSpace(*in.Body.GitEmail)
			}
			// @decision(DL-014)
			if in.Body.DefaultModel != nil {
				u.DefaultModel = strings.TrimSpace(*in.Body.DefaultModel)
			}
			// @decision(DL-043)
			if in.Body.DefaultGitAccess != nil {
				u.DefaultGitAccess = gitAccess[0]
			}
			if in.Body.DefaultJobGitAccess != nil {
				u.DefaultJobGitAccess = gitAccess[1]
			}
			// @decision(DL-049)
			if in.Body.DefaultJobDuration != nil {
				u.DefaultJobDuration = jobDuration
			}
			return u
		}
		// A caller with no row yet gets one.
		//
		// @decision(DL-014)
		u, err := s.deps.Store.Users().Get(ctx, p.OrgID, p.UserID)
		fresh := errors.Is(err, store.ErrNotFound)
		switch {
		case fresh:
			u = store.User{ID: p.UserID, OrgID: p.OrgID, Email: p.Email, Admin: p.Admin}
		case err != nil:
			return nil, toHumaError(err)
		}
		if !fresh {
			if err := s.deps.Store.Users().Update(ctx, apply(u)); err != nil {
				return nil, toHumaError(err)
			}
			return &meOutput{Body: s.meView(ctx, p)}, nil
		}
		if err := s.deps.Store.Users().Create(ctx, apply(u)); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				return nil, toHumaError(err)
			}
			// Another save created the row between the read above and
			// this write. Patch what they wrote instead of overwriting
			// it with this request's struct, which carries only its own
			// fields.
			current, err := s.deps.Store.Users().Get(ctx, p.OrgID, p.UserID)
			if err != nil {
				return nil, toHumaError(err)
			}
			if err := s.deps.Store.Users().Update(ctx, apply(current)); err != nil {
				return nil, toHumaError(err)
			}
		}
		return &meOutput{Body: s.meView(ctx, p)}, nil
	})
}

// meView is the caller's principal, with the git identity and the
// default model from their stored profile when there is one.
//
// @decision(DL-044) @decision(DL-014)
func (s *Server) meView(ctx context.Context, p auth.Principal) MeView {
	v := MeView{UserID: p.UserID, OrgID: p.OrgID, Email: p.Email, Admin: p.Admin}
	// One read feeds both the identity and the quota: /me is the
	// endpoint a client polls, and two fetches of the same row would
	// also log the same failure twice.
	//
	// @decision(DL-009)
	if p.UserID == "" || s.deps.Store == nil {
		v.Quota = s.deps.Quotas.withDefaults().view()
		return v
	}
	u, err := s.deps.Store.Users().Get(ctx, p.OrgID, p.UserID)
	if err != nil {
		// A principal with no user row is ordinary — the bearer token
		// of local mode has none. Anything else is worth saying, since
		// the identity silently vanishing from /me looks like the user
		// never set one.
		if !errors.Is(err, store.ErrNotFound) {
			s.deps.Warn("git identity for " + p.UserID + ": " + err.Error())
		}
		v.Quota = s.deps.Quotas.withDefaults().view()
		return v
	}
	v.GitName, v.GitEmail = u.GitName, u.GitEmail
	// @decision(DL-014)
	v.DefaultModel = u.DefaultModel
	// @decision(DL-043)
	v.DefaultGitAccess, v.DefaultJobGitAccess = u.DefaultGitAccess, u.DefaultJobGitAccess
	// @decision(DL-049)
	if u.DefaultJobDuration > 0 {
		v.DefaultJobDuration = u.DefaultJobDuration.String()
	}
	v.Quota = s.deps.Quotas.effective(u.Quota).view()
	return v
}

// StorePollInterval is how often a replica that did not run a launch
// re-reads the session while streaming its events.
//
// @decision(DL-003)
const StorePollInterval = 2 * time.Second

// storePoll is the interval in force, overridable by a test.
//
// @decision(DL-003)
func (s *Server) storePoll() time.Duration {
	if s.deps.StorePoll > 0 {
		return s.deps.StorePoll
	}
	return StorePollInterval
}

// streamFromStore serves the event stream for a session whose launch is
// not this replica's. A finished session yields its one derived terminal
// event; a launch still in flight elsewhere yields a step snapshot and
// then the store's state, polled until it is terminal, so the CLI's
// Follow sees a valid stream from any replica.
//
// Shared event streams (Postgres LISTEN/NOTIFY) are deferred until a
// consumer needs the individual launch steps from another replica.
//
// @decision(DL-003)
func (s *Server) streamFromStore(ctx context.Context, sess store.Session, send sse.Sender) {
	if terminal, ok := terminalEvent(sess, s.deps.Now()); ok {
		_ = send.Data(terminal)
		return
	}
	// Something is launching this elsewhere. Say so as a step rather
	// than an error: the launch is healthy, just not observable here.
	// The individual steps belong to the launching replica's log, so
	// the one reported is where a launch spends most of its time.
	if err := send.Data(StepEvent{Step: string(session.StepSandbox), State: "started", At: s.deps.Now()}); err != nil {
		return
	}
	ticker := time.NewTicker(s.storePoll())
	defer ticker.Stop()
	for {
		// The store is read before the first wait: the snapshot the
		// caller handed us may already be stale, and a launch that
		// finished in that gap should not cost a poll interval.
		latest, err := s.svc.Get(ctx, principal(ctx), sess.ID)
		if err != nil {
			// The session went while we watched; the stream ends rather
			// than reporting a launch that no longer exists.
			return
		}
		if terminal, ok := terminalEvent(latest, s.deps.Now()); ok {
			_ = send.Data(terminal)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// terminalEvent derives the event a finished session would have ended
// its stream with. A session still starting has none.
//
// @decision(DL-003)
func terminalEvent(sess store.Session, now time.Time) (any, bool) {
	switch sess.State {
	case store.StateStarting:
		return nil, false
	case store.StateFailed, store.StateStopped:
		// A session that stopped before its stream ended did not become
		// usable, so the caller is told the launch failed — the same
		// reading the client's own polling fallback applies, so a
		// stream and a poll cannot disagree about one session.
		detail := sess.Error
		if detail == "" {
			detail = "the session is " + string(sess.State)
		}
		return ErrorEvent{Problem: ProblemView{Title: "Launch failed", Status: 502, Detail: detail}}, true
	default:
		return ReadyEvent{Session: viewOf(sess, now)}, true
	}
}

// parseAccessFields checks the two git access defaults a profile save
// may carry; an absent one is left empty.
//
// @decision(DL-043)
func parseAccessFields(fields ...*string) ([]store.GitAccess, error) {
	out := make([]store.GitAccess, len(fields))
	for i, f := range fields {
		if f == nil {
			continue
		}
		a, err := store.ParseGitAccess(*f)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
		out[i] = a
	}
	return out, nil
}
