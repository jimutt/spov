package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// The admin's view of the organisation's GitHub App: its status, a
// stored App set or replaced after a check, and removal. Admin only,
// hosted only; a deployment whose environment sets the App is shown and
// never changed here.
//
// @decision(DL-042)

// OrgGitHubPlatform is a configured GitHub platform an App serves.
//
// @decision(DL-042)
type OrgGitHubPlatform struct {
	Name    string
	BaseURL string
	// Account is the configured account the installation is found on.
	Account string
	// FromEnv is set when the deployment's environment sets the App.
	FromEnv bool
	// Env is the App the environment sets, when FromEnv.
	Env git.GitHubCredential
}

// OrgGitHub is what the server needs of the deployment's configuration
// to manage the App: its GitHub platforms, and a check of an App against
// one. The CLI's serve wiring is the real one.
//
// @decision(DL-042)
type OrgGitHub interface {
	Platforms() []OrgGitHubPlatform
	Check(ctx context.Context, platform OrgGitHubPlatform, cred git.GitHubCredential, account string) (git.AppStatus, error)
}

// OrgCheckTTL is how long a status check is reused, so the page does not
// mint a token on every view.
const OrgCheckTTL = time.Minute

// Admin actions written to the audit trail.
const (
	AdminGitHubAppStored  = "github_app.stored"
	AdminGitHubAppRemoved = "github_app.removed"
)

// OrgGitHubView is one platform's App, as an admin sees it. The private
// key is never in it.
//
// @decision(DL-042)
type OrgGitHubView struct {
	Platform string `json:"platform"`
	BaseURL  string `json:"base_url,omitempty"`
	// Source says where the App comes from: the deployment's environment,
	// one stored here, or none.
	Source         string     `json:"source" enum:"environment,stored,none"`
	AppID          string     `json:"app_id,omitempty"`
	Account        string     `json:"account,omitempty"`
	InstallationID int64      `json:"installation_id,omitempty"`
	UpdatedBy      string     `json:"updated_by,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
	Check          *AppCheck  `json:"check,omitempty"`
}

// AppCheck is the latest check of the App: what it reaches and may do,
// or why it failed.
//
// @decision(DL-042)
type AppCheck struct {
	At          time.Time         `json:"at"`
	Error       string            `json:"error,omitempty"`
	Account     string            `json:"account,omitempty"`
	Permissions map[string]string `json:"permissions,omitempty"`
	// Missing are the permissions sandboxes ask for that it lacks, as
	// name:level.
	Missing   []string `json:"missing,omitempty"`
	RepoCount int      `json:"repo_count"`
	Repos     []string `json:"repos,omitempty"`
}

// AdminEventView is one audited change.
type AdminEventView struct {
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// OrgGitHubApp is what an admin stores: the App and the account whose
// installation it uses.
type OrgGitHubApp struct {
	AppID      string `json:"app_id" minLength:"1" maxLength:"64" doc:"The App's ID or client ID"`
	PrivateKey string `json:"private_key" minLength:"1" maxLength:"4096" doc:"The App's PEM private key; never returned"`
	Account    string `json:"account,omitempty" maxLength:"100" doc:"The user or organisation the App is installed on; the platform's configured account when empty"`
}

type orgGitHubOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Platforms []OrgGitHubView  `json:"platforms"`
		Events    []AdminEventView `json:"events"`
	}
}

type orgGitHubPutInput struct {
	Platform string `path:"platform" doc:"The configured platform's name"`
	Body     OrgGitHubApp
}

type orgGitHubDeleteInput struct {
	Platform string `path:"platform" doc:"The configured platform's name"`
}

// checkCache keeps status checks per org and platform for OrgCheckTTL.
type checkCache struct {
	mu   sync.Mutex
	byID map[string]AppCheck
}

func (c *checkCache) get(key string, now time.Time) (AppCheck, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.byID[key]
	return v, ok && now.Sub(v.At) < OrgCheckTTL
}

func (c *checkCache) put(key string, v AppCheck) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = map[string]AppCheck{}
	}
	c.byID[key] = v
}

func (c *checkCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byID, key)
}

// checkOf turns a check's outcome into its view.
func checkOf(st git.AppStatus, err error, at time.Time) AppCheck {
	if err != nil {
		return AppCheck{At: at, Error: redact.Error(err)}
	}
	return AppCheck{At: at, Account: st.Account, Permissions: st.Permissions, Missing: st.Missing,
		RepoCount: st.RepoCount, Repos: st.Repos}
}

// requireOrgAdmin is requireAllowlist's checks for the App routes: an
// admin of the org the deployment's login governs, on a server that can
// hold an App at all.
//
// @decision(DL-042) @decision(DL-007)
func (s *Server) requireOrgAdmin(p auth.Principal) error {
	if s.deps.OrgGitHub == nil || s.deps.OrgApps == nil {
		return huma.Error501NotImplemented("this server keeps no organisation GitHub App; a local server uses your own credentials")
	}
	return s.requireAllowlist(p)
}

// orgPlatform finds a configured GitHub platform by name.
func (s *Server) orgPlatform(name string) (OrgGitHubPlatform, error) {
	for _, p := range s.deps.OrgGitHub.Platforms() {
		if p.Name == name {
			return p, nil
		}
	}
	return OrgGitHubPlatform{}, huma.Error404NotFound(fmt.Sprintf("no GitHub platform called %q is configured", name))
}

// orgGitHubViews builds each platform's view, checking the App it has
// (through the cache).
//
// @decision(DL-042)
func (s *Server) orgGitHubViews(ctx context.Context, orgID string) ([]OrgGitHubView, error) {
	out := []OrgGitHubView{}
	for _, plat := range s.deps.OrgGitHub.Platforms() {
		v := OrgGitHubView{Platform: plat.Name, BaseURL: plat.BaseURL, Source: "none"}
		var cred git.GitHubCredential
		account := plat.Account
		switch {
		case plat.FromEnv:
			v.Source, cred = "environment", plat.Env
			v.AppID, v.Account = cred.AppID, plat.Account
		default:
			app, found, err := s.deps.OrgApps.Get(ctx, orgID, plat.Name)
			// A row that cannot be opened is shown as such, so it can be
			// replaced or removed here.
			if errors.Is(err, ErrOrgAppUnreadable) {
				v.Source = "stored"
				v.Check = &AppCheck{At: s.deps.Now(), Error: "the stored App cannot be opened (was the credential key changed?); replace or remove it"}
				out = append(out, v)
				continue
			}
			if err != nil {
				return nil, err
			}
			if !found {
				out = append(out, v)
				continue
			}
			v.Source, cred = "stored", app.Credential
			v.AppID, v.Account, v.InstallationID = app.Meta.AppID, app.Meta.Account, app.Meta.InstallationID
			v.UpdatedBy = app.UpdatedBy
			at := app.UpdatedAt
			v.UpdatedAt = &at
			if app.Meta.Account != "" {
				account = app.Meta.Account
			}
		}
		key := orgID + "\x00" + plat.Name
		check, ok := s.checks.get(key, s.deps.Now())
		if !ok {
			st, err := s.deps.OrgGitHub.Check(ctx, plat, cred, account)
			check = checkOf(st, err, s.deps.Now())
			s.checks.put(key, check)
		}
		v.Check = &check
		if v.InstallationID == 0 && check.Error == "" {
			v.Account = check.Account
		}
		out = append(out, v)
	}
	return out, nil
}

// adminEvents lists the org's latest audited changes.
func (s *Server) adminEvents(ctx context.Context, orgID string) ([]AdminEventView, error) {
	evs, err := s.deps.Store.AdminEvents().List(ctx, orgID, 10)
	if err != nil {
		return nil, err
	}
	out := make([]AdminEventView, 0, len(evs))
	for _, e := range evs {
		out = append(out, AdminEventView{Actor: e.Actor, Action: e.Action, Detail: e.Detail, At: e.At})
	}
	return out, nil
}

// registerOrgGitHub serves the App routes.
//
// @decision(DL-042)
func (s *Server) registerOrgGitHub() {
	answer := func(ctx context.Context, orgID string) (*orgGitHubOutput, error) {
		views, err := s.orgGitHubViews(ctx, orgID)
		if err != nil {
			return nil, toHumaError(err)
		}
		events, err := s.adminEvents(ctx, orgID)
		if err != nil {
			return nil, toHumaError(err)
		}
		out := &orgGitHubOutput{CacheControl: "no-store"}
		out.Body.Platforms, out.Body.Events = views, events
		return out, nil
	}

	register(s.api, huma.Operation{
		OperationID: "get-org-github", Method: http.MethodGet, Path: "/api/v1/org/github",
		Summary: "The organisation's GitHub App: where it comes from, what it reaches, who changed it", Tags: []string{"admin"},
		Errors: []int{http.StatusNotImplemented},
	}, func(ctx context.Context, _ *struct{}) (*orgGitHubOutput, error) {
		p := principal(ctx)
		if err := s.requireOrgAdmin(p); err != nil {
			return nil, err
		}
		return answer(ctx, p.OrgID)
	})

	register(s.api, huma.Operation{
		OperationID: "put-org-github", Method: http.MethodPut, Path: "/api/v1/org/github/{platform}",
		Summary: "Check and store the organisation's GitHub App for a platform", Tags: []string{"admin"},
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusNotImplemented},
	}, func(ctx context.Context, in *orgGitHubPutInput) (*orgGitHubOutput, error) {
		p := principal(ctx)
		if err := s.requireOrgAdmin(p); err != nil {
			return nil, err
		}
		plat, err := s.orgPlatform(in.Platform)
		if err != nil {
			return nil, err
		}
		if plat.FromEnv {
			return nil, huma.Error409Conflict("the deployment's environment sets this App; change it there, or remove it there to manage it here")
		}
		cred := git.GitHubCredential{AppID: strings.TrimSpace(in.Body.AppID), PrivateKey: strings.TrimSpace(in.Body.PrivateKey)}
		account := strings.TrimSpace(in.Body.Account)
		if account == "" {
			account = plat.Account
		}
		if account == "" {
			return nil, huma.Error422UnprocessableEntity("name the account the App is installed on; this platform configures none")
		}
		// Checked before anything is stored: a stored App is one that
		// worked.
		st, err := s.deps.OrgGitHub.Check(ctx, plat, cred, account)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("the App did not check out, so nothing was stored: " + redact.Error(err))
		}
		cred.InstallationID = st.InstallationID
		meta := OrgAppMeta{AppID: cred.AppID, Account: account, InstallationID: st.InstallationID}
		row, err := s.deps.OrgApps.Sealed(p.OrgID, plat.Name, actorOf(p), cred, meta)
		if err != nil {
			return nil, toHumaError(err)
		}
		now := s.deps.Now()
		err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
			if err := liveAdminIn(ctx, tx, p); err != nil {
				return err
			}
			if err := tx.OrgCredentials().Put(ctx, row); err != nil {
				return err
			}
			return tx.AdminEvents().Append(ctx, store.AdminEvent{
				ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminGitHubAppStored, At: now,
				Detail: fmt.Sprintf("App %s on %s for %s (installation %d)", cred.AppID, account, plat.Name, st.InstallationID),
			})
		})
		s.deps.OrgApps.Forget(p.OrgID, plat.Name)
		if err != nil {
			return nil, toHumaError(err)
		}
		s.checks.put(p.OrgID+"\x00"+plat.Name, checkOf(st, nil, now))
		return answer(ctx, p.OrgID)
	})

	register(s.api, huma.Operation{
		OperationID: "delete-org-github", Method: http.MethodDelete, Path: "/api/v1/org/github/{platform}",
		Summary: "Remove the organisation's stored GitHub App for a platform", Tags: []string{"admin"},
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusNotImplemented},
	}, func(ctx context.Context, in *orgGitHubDeleteInput) (*orgGitHubOutput, error) {
		p := principal(ctx)
		if err := s.requireOrgAdmin(p); err != nil {
			return nil, err
		}
		plat, err := s.orgPlatform(in.Platform)
		if err != nil {
			return nil, err
		}
		if plat.FromEnv {
			return nil, huma.Error409Conflict("the deployment's environment sets this App; remove it there")
		}
		now := s.deps.Now()
		err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
			if err := liveAdminIn(ctx, tx, p); err != nil {
				return err
			}
			if err := tx.OrgCredentials().Delete(ctx, p.OrgID, plat.Name); err != nil {
				return err
			}
			return tx.AdminEvents().Append(ctx, store.AdminEvent{
				ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminGitHubAppRemoved, At: now,
				Detail: "the App for " + plat.Name,
			})
		})
		s.deps.OrgApps.Forget(p.OrgID, plat.Name)
		s.checks.drop(p.OrgID + "\x00" + plat.Name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("no App is stored for " + plat.Name)
		}
		if err != nil {
			return nil, toHumaError(err)
		}
		return answer(ctx, p.OrgID)
	})
}

// liveAdminIn re-checks, inside the write's transaction, that the caller
// is still an admin, after taking the org's lock as the allowlist's own
// writes do.
//
// @decision(DL-042) @decision(DL-007)
func liveAdminIn(ctx context.Context, tx store.Store, p auth.Principal) error {
	if _, err := tx.Allowlist().CountAdmins(ctx, p.OrgID); err != nil {
		return err
	}
	return requireLiveAdmin(ctx, tx.Allowlist(), p)
}

// actorOf names who acted, for the audit trail: the e-mail where the
// principal has one.
func actorOf(p auth.Principal) string {
	if p.Email != "" {
		return p.Email
	}
	return cancelledBy(p)
}
