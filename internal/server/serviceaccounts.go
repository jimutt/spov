package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// The audit actions of the service-account routes. None carries a
// secret: an account's name and author, a token's id and label.
//
// @decision(DL-140) @decision(DL-141)
const (
	AdminServiceAccountCreated    = "service_account.created"
	AdminServiceAccountUpdated    = "service_account.updated"
	AdminServiceAccountDeleted    = "service_account.deleted"
	AdminServiceAccountOffboarded = "service_account.offboarded"
	AdminServiceTokenCreated      = "service_account.token.created"
	AdminServiceTokenRevoked      = "service_account.token.revoked"
)

// The service-account limits.
//
// @decision(DL-140) @decision(DL-141)
const (
	// MaxServiceAccounts is how many service accounts that are not
	// deleted an organisation holds.
	MaxServiceAccounts = 100
	// MaxServiceTokens is how many usable tokens one account holds.
	MaxServiceTokens = 10
	// DefaultServiceTokenDays is a token's lifetime when the request
	// names none; MaxServiceTokenDays is the longest it may name.
	DefaultServiceTokenDays = 365
	MaxServiceTokenDays     = 1095
	// MaxServiceTokenLabel bounds a token's label, in characters.
	MaxServiceTokenLabel = 64
	// maxGitAuthor bounds the commit author's name and address.
	maxGitAuthor = 254
)

// serviceAccountName is the shape of a service account's name.
//
// @decision(DL-140)
var serviceAccountName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)

// ServiceAccountView is a service account as an admin sees it. It
// never carries a token.
//
// @decision(DL-140)
type ServiceAccountView struct {
	ID        string    `json:"id" doc:"The account's user ID, which PATCH /api/v1/users/{id} takes for its quota"`
	Name      string    `json:"name"`
	GitName   string    `json:"git_name" doc:"The name its commits are authored with"`
	GitEmail  string    `json:"git_email" doc:"The address its commits are authored with"`
	CreatedAt time.Time `json:"created_at"`
	// DisabledAt is set once the account is deleted; its name stays
	// taken.
	DisabledAt *time.Time `json:"disabled_at,omitempty" doc:"When the account was deleted; its name stays taken"`
	Tokens     int        `json:"tokens" doc:"How many of its tokens are usable now"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty" doc:"The newest use of any of its tokens"`
}

type serviceAccountsOutput struct {
	Body struct {
		Accounts []ServiceAccountView `json:"accounts"`
	}
}

type serviceAccountOutput struct {
	Body ServiceAccountView
}

type createServiceAccountInput struct {
	Body struct {
		Name     string  `json:"name" doc:"3 to 32 lower-case letters, digits and hyphens, starting with a letter and not ending with a hyphen"`
		GitName  *string `json:"git_name,omitempty" doc:"The name its commits are authored with; the account's name when absent"`
		GitEmail *string `json:"git_email,omitempty" doc:"The address its commits are authored with; <name>@service.invalid when absent"`
	}
}

type patchServiceAccountInput struct {
	Name string `path:"name"`
	Body struct {
		GitName  *string `json:"git_name,omitempty" doc:"The name its commits are authored with"`
		GitEmail *string `json:"git_email,omitempty" doc:"The address its commits are authored with"`
	}
}

type serviceAccountRef struct {
	Name string `path:"name"`
}

// ServiceTokenView is a service account's token as a listing shows it.
//
// @decision(DL-141)
type ServiceTokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name" doc:"The token's label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty" doc:"Absent on a token issued with no_expiry"`
}

type serviceTokensOutput struct {
	Body struct {
		Tokens []ServiceTokenView `json:"tokens"`
	}
}

type createServiceTokenInput struct {
	Name string `path:"name"`
	Body struct {
		Name          string `json:"name" doc:"A label for the token, at most 64 characters"`
		ExpiresInDays *int   `json:"expires_in_days,omitempty" doc:"Days until it expires, 1 to 1095; 365 when neither this nor no_expiry is given"`
		NoExpiry      bool   `json:"no_expiry,omitempty" doc:"Issue a token that never expires"`
	}
}

// IssuedServiceToken is the one answer that carries a token.
//
// @decision(DL-141)
type IssuedServiceToken struct {
	ServiceTokenView
	Token string `json:"token" doc:"The token, shown only here; the server keeps its hash"`
}

type createServiceTokenOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IssuedServiceToken
}

type serviceTokenRef struct {
	Name string `path:"name"`
	ID   string `path:"id"`
}

// registerServiceAccounts serves the admin routes for service accounts
// and their tokens.
//
// @decision(DL-140) @decision(DL-141)
func (s *Server) registerServiceAccounts() {
	errs := []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusNotImplemented}
	register(s.api, huma.Operation{
		OperationID: "create-service-account", Method: http.MethodPost, Path: "/api/v1/service-accounts",
		Summary: "Create a service account, which never signs in and authenticates with a token an admin issues",
		Tags:    []string{"admin"}, DefaultStatus: http.StatusCreated, Errors: errs,
	}, s.createServiceAccount)
	register(s.api, huma.Operation{
		OperationID: "list-service-accounts", Method: http.MethodGet, Path: "/api/v1/service-accounts",
		Summary: "The organisation's service accounts", Tags: []string{"admin"}, Errors: errs,
	}, s.listServiceAccounts)
	register(s.api, huma.Operation{
		OperationID: "update-service-account", Method: http.MethodPatch, Path: "/api/v1/service-accounts/{name}",
		Summary: "Change a service account's commit author", Tags: []string{"admin"}, Errors: errs,
	}, s.updateServiceAccount)
	register(s.api, huma.Operation{
		OperationID: "delete-service-account", Method: http.MethodDelete, Path: "/api/v1/service-accounts/{name}",
		Summary: "Delete a service account and end what it has running and stored",
		Description: "Disables the account, then cancels its unfinished jobs, stops its live sessions, revokes its" +
			" tokens and deletes its stored credentials, and writes an audit row. Its name stays taken and its" +
			" history is kept. If a step fails the answer says what is left, and the same delete retries it.",
		Tags: []string{"admin"}, DefaultStatus: http.StatusNoContent,
		Errors: append(errs, http.StatusInternalServerError),
	}, s.deleteServiceAccount)

	register(s.api, huma.Operation{
		OperationID: "create-service-token", Method: http.MethodPost, Path: "/api/v1/service-accounts/{name}/tokens",
		Summary: "Issue a token for a service account; the answer is the only place it is shown",
		Tags:    []string{"admin"}, DefaultStatus: http.StatusCreated, Errors: errs,
	}, s.createServiceToken)
	register(s.api, huma.Operation{
		OperationID: "list-service-tokens", Method: http.MethodGet, Path: "/api/v1/service-accounts/{name}/tokens",
		Summary: "A service account's tokens, never the tokens themselves", Tags: []string{"admin"}, Errors: errs,
	}, s.listServiceTokens)
	register(s.api, huma.Operation{
		OperationID: "revoke-service-token", Method: http.MethodDelete, Path: "/api/v1/service-accounts/{name}/tokens/{id}",
		Summary: "Revoke one of a service account's tokens", Tags: []string{"admin"},
		DefaultStatus: http.StatusNoContent, Errors: errs,
	}, s.revokeServiceToken)
}

// requireServiceAdmin is requireAllowlist's checks for the
// service-account routes, which a local server does not serve.
//
// @decision(DL-140)
func (s *Server) requireServiceAdmin(p auth.Principal) error {
	if s.deps.Local {
		return huma.Error501NotImplemented("a local server has no service accounts")
	}
	return s.requireAllowlist(p)
}

// serviceAccount reads the service account a path names, in the
// caller's organisation. A name of the wrong shape, a person's row and
// a row of another organisation are all not found.
//
// @decision(DL-140)
func serviceAccount(ctx context.Context, users store.UserStore, orgID, name string) (store.User, error) {
	notFound := huma.Error404NotFound("no service account is named " + fmt.Sprintf("%.64q", name))
	if !serviceAccountName.MatchString(name) {
		return store.User{}, notFound
	}
	u, err := users.ByEmail(ctx, orgID, auth.ServiceAccountAddress(name))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.User{}, notFound
	case err != nil:
		return store.User{}, err
	case u.Kind != store.UserService || u.OrgID != orgID:
		return store.User{}, notFound
	}
	return u, nil
}

// gitAuthor checks a commit author's name or address. An address must
// be a plain address.
//
// @decision(DL-140)
func gitAuthor(field, value string, address bool) (string, error) {
	v := strings.TrimSpace(value)
	switch {
	case v == "":
		return "", huma.Error422UnprocessableEntity(field + " may not be empty")
	case utf8.RuneCountInString(v) > maxGitAuthor || auth.CleanLabel(v) != v:
		return "", huma.Error422UnprocessableEntity(fmt.Sprintf(
			"%s must be at most %d characters with no control characters", field, maxGitAuthor))
	}
	if address {
		if a, err := mail.ParseAddress(v); err != nil || a.Address != v {
			return "", huma.Error422UnprocessableEntity(field + " must be a plain address, such as ci@example.com")
		}
	}
	return v, nil
}

// serviceAccountView is an account as the API shows it, with its
// tokens counted at now.
//
// @decision(DL-140)
func serviceAccountView(u store.User, tokens []store.APIToken, now time.Time) ServiceAccountView {
	v := ServiceAccountView{
		ID: u.ID, Name: u.Name, GitName: u.GitName, GitEmail: u.GitEmail, CreatedAt: u.CreatedAt, DisabledAt: u.DisabledAt,
	}
	for _, t := range tokens {
		if t.Usable(now) {
			v.Tokens++
		}
		if t.LastUsedAt != nil && (v.LastUsedAt == nil || t.LastUsedAt.After(*v.LastUsedAt)) {
			at := *t.LastUsedAt
			v.LastUsedAt = &at
		}
	}
	return v
}

// createServiceAccount makes the row, inside a transaction that checks
// the caller is still an admin and counts the organisation's accounts.
//
// @decision(DL-140)
func (s *Server) createServiceAccount(ctx context.Context, in *createServiceAccountInput) (*serviceAccountOutput, error) {
	p := principal(ctx)
	if err := s.requireServiceAdmin(p); err != nil {
		return nil, err
	}
	name := in.Body.Name
	if !serviceAccountName.MatchString(name) {
		return nil, huma.Error422UnprocessableEntity("a service account's name is 3 to 32 lower-case letters, digits" +
			" and hyphens, starting with a letter and not ending with a hyphen")
	}
	address := auth.ServiceAccountAddress(name)
	gitName, gitEmail := name, address
	var err error
	if in.Body.GitName != nil {
		if gitName, err = gitAuthor("git_name", *in.Body.GitName, false); err != nil {
			return nil, err
		}
	}
	if in.Body.GitEmail != nil {
		if gitEmail, err = gitAuthor("git_email", *in.Body.GitEmail, true); err != nil {
			return nil, err
		}
	}
	now := s.deps.Now()
	u := store.User{
		ID: store.NewID(now), OrgID: p.OrgID, Email: address, Name: name, Kind: store.UserService,
		GitName: gitName, GitEmail: gitEmail, CreatedAt: now,
	}
	err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
		if err := liveAdminIn(ctx, tx, p); err != nil {
			return err
		}
		if _, err := tx.Users().ByEmail(ctx, p.OrgID, address); err == nil {
			return huma.Error409Conflict("a service account is already named " + name + "; a deleted one keeps its name")
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		users, err := tx.Users().List(ctx, p.OrgID)
		if err != nil {
			return err
		}
		live := 0
		for _, other := range users {
			if other.Kind == store.UserService && other.DisabledAt == nil {
				live++
			}
		}
		if live >= MaxServiceAccounts {
			return huma.Error409Conflict(fmt.Sprintf("the organisation already has %d service accounts, the most it"+
				" may hold; delete one first", MaxServiceAccounts))
		}
		if err := tx.Users().Create(ctx, u); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return huma.Error409Conflict("a service account is already named " + name)
			}
			return err
		}
		return tx.AdminEvents().Append(ctx, store.AdminEvent{
			ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminServiceAccountCreated, At: now,
			Detail: "service account " + name + ", author " + gitName + " <" + gitEmail + ">",
		})
	})
	if err != nil {
		return nil, toHumaError(err)
	}
	stored, err := s.deps.Store.Users().Get(ctx, p.OrgID, u.ID)
	if err != nil {
		return nil, toHumaError(err)
	}
	return &serviceAccountOutput{Body: serviceAccountView(stored, nil, now)}, nil
}

// listServiceAccounts lists every service account of the caller's
// organisation, deleted ones included.
//
// @decision(DL-140)
func (s *Server) listServiceAccounts(ctx context.Context, _ *struct{}) (*serviceAccountsOutput, error) {
	p := principal(ctx)
	if err := s.requireServiceAdmin(p); err != nil {
		return nil, err
	}
	users, err := s.deps.Store.Users().List(ctx, p.OrgID)
	if err != nil {
		return nil, toHumaError(err)
	}
	now := s.deps.Now()
	out := &serviceAccountsOutput{}
	out.Body.Accounts = []ServiceAccountView{}
	for _, u := range users {
		if u.Kind != store.UserService {
			continue
		}
		tokens, err := s.deps.Store.Tokens().ListByUser(ctx, p.OrgID, u.ID)
		if err != nil {
			return nil, toHumaError(err)
		}
		out.Body.Accounts = append(out.Body.Accounts, serviceAccountView(u, tokens, now))
	}
	return out, nil
}

// updateServiceAccount changes the commit author of an account that is
// not deleted.
//
// @decision(DL-140)
func (s *Server) updateServiceAccount(ctx context.Context, in *patchServiceAccountInput) (*serviceAccountOutput, error) {
	p := principal(ctx)
	if err := s.requireServiceAdmin(p); err != nil {
		return nil, err
	}
	var gitName, gitEmail string
	var err error
	if in.Body.GitName != nil {
		if gitName, err = gitAuthor("git_name", *in.Body.GitName, false); err != nil {
			return nil, err
		}
	}
	if in.Body.GitEmail != nil {
		if gitEmail, err = gitAuthor("git_email", *in.Body.GitEmail, true); err != nil {
			return nil, err
		}
	}
	now := s.deps.Now()
	var u store.User
	err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
		if err := liveAdminIn(ctx, tx, p); err != nil {
			return err
		}
		if u, err = serviceAccount(ctx, tx.Users(), p.OrgID, in.Name); err != nil {
			return err
		}
		if u.DisabledAt != nil {
			return huma.Error409Conflict("the service account " + u.Name + " is deleted")
		}
		if in.Body.GitName != nil {
			u.GitName = gitName
		}
		if in.Body.GitEmail != nil {
			u.GitEmail = gitEmail
		}
		if err := tx.Users().Update(ctx, u); err != nil {
			return err
		}
		return tx.AdminEvents().Append(ctx, store.AdminEvent{
			ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminServiceAccountUpdated, At: now,
			Detail: "service account " + u.Name + ", author " + u.GitName + " <" + u.GitEmail + ">",
		})
	})
	if err != nil {
		return nil, toHumaError(err)
	}
	tokens, err := s.deps.Store.Tokens().ListByUser(ctx, p.OrgID, u.ID)
	if err != nil {
		return nil, toHumaError(err)
	}
	return &serviceAccountOutput{Body: serviceAccountView(u, tokens, now)}, nil
}

// deleteServiceAccount disables the account and audits it in one
// transaction, then off-boards it after the commit, so its tokens are
// refused before anything of its is ended. A delete of an account
// already deleted runs the off-boarding again, which is the retry.
//
// @decision(DL-140) @decision(DL-132)
func (s *Server) deleteServiceAccount(ctx context.Context, in *serviceAccountRef) (*struct{}, error) {
	p := principal(ctx)
	if err := s.requireServiceAdmin(p); err != nil {
		return nil, err
	}
	now := s.deps.Now()
	var u store.User
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		if err := liveAdminIn(ctx, tx, p); err != nil {
			return err
		}
		var err error
		if u, err = serviceAccount(ctx, tx.Users(), p.OrgID, in.Name); err != nil {
			return err
		}
		if u.DisabledAt != nil {
			return nil
		}
		if err := tx.Users().Disable(ctx, p.OrgID, u.ID, now); err != nil {
			return err
		}
		return tx.AdminEvents().Append(ctx, store.AdminEvent{
			ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminServiceAccountDeleted, At: now,
			Detail: "service account " + u.Name,
		})
	})
	if err != nil {
		return nil, toHumaError(err)
	}
	if _, err := s.svc.Offboard(ctx, p, "service account "+u.Name, u, offboardService); err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	return nil, nil
}

// createServiceToken issues a token for an account that is not
// deleted and holds fewer than MaxServiceTokens usable ones. The token
// is DL-006's, stored by its hash in the same table.
//
// @decision(DL-141) @decision(DL-006)
func (s *Server) createServiceToken(ctx context.Context, in *createServiceTokenInput) (*createServiceTokenOutput, error) {
	p := principal(ctx)
	if err := s.requireServiceAdmin(p); err != nil {
		return nil, err
	}
	label := strings.TrimSpace(in.Body.Name)
	switch {
	case label == "":
		return nil, huma.Error422UnprocessableEntity("name the token: a label of at most 64 characters")
	case utf8.RuneCountInString(label) > MaxServiceTokenLabel || auth.CleanLabel(label) != label:
		return nil, huma.Error422UnprocessableEntity(fmt.Sprintf(
			"a token's label is at most %d characters with no control characters", MaxServiceTokenLabel))
	}
	now := s.deps.Now()
	var expires *time.Time
	switch {
	case in.Body.NoExpiry && in.Body.ExpiresInDays != nil:
		return nil, huma.Error422UnprocessableEntity("give expires_in_days or no_expiry, not both")
	case !in.Body.NoExpiry:
		days := DefaultServiceTokenDays
		if in.Body.ExpiresInDays != nil {
			days = *in.Body.ExpiresInDays
		}
		if days < 1 || days > MaxServiceTokenDays {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf(
				"expires_in_days is 1 to %d", MaxServiceTokenDays))
		}
		at := now.Add(time.Duration(days) * 24 * time.Hour)
		expires = &at
	}
	token, err := auth.NewToken()
	if err != nil {
		return nil, toHumaError(err)
	}
	rec := store.APIToken{
		ID: store.NewID(now), OrgID: p.OrgID, Hash: auth.HashToken(token), Name: label, ExpiresAt: expires, CreatedAt: now,
	}
	err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
		if err := liveAdminIn(ctx, tx, p); err != nil {
			return err
		}
		u, err := serviceAccount(ctx, tx.Users(), p.OrgID, in.Name)
		if err != nil {
			return err
		}
		if u.DisabledAt != nil {
			return huma.Error409Conflict("the service account " + u.Name + " is deleted")
		}
		held, err := tx.Tokens().ListByUser(ctx, p.OrgID, u.ID)
		if err != nil {
			return err
		}
		usable := 0
		for _, t := range held {
			if t.Usable(now) {
				usable++
			}
		}
		if usable >= MaxServiceTokens {
			return huma.Error409Conflict(fmt.Sprintf("the service account %s already holds %d usable tokens, the"+
				" most it may; revoke one first", u.Name, MaxServiceTokens))
		}
		rec.UserID = u.ID
		if err := tx.Tokens().Create(ctx, rec); err != nil {
			return err
		}
		return tx.AdminEvents().Append(ctx, store.AdminEvent{
			ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminServiceTokenCreated, At: now,
			Detail: "token " + rec.ID + " (" + label + ") for service account " + u.Name,
		})
	})
	if err != nil {
		return nil, toHumaError(err)
	}
	return &createServiceTokenOutput{
		CacheControl: "no-store",
		Body: IssuedServiceToken{
			ServiceTokenView: ServiceTokenView{ID: rec.ID, Name: rec.Name, CreatedAt: rec.CreatedAt, ExpiresAt: rec.ExpiresAt},
			Token:            token,
		},
	}, nil
}

// listServiceTokens lists an account's tokens that are not revoked.
//
// @decision(DL-141)
func (s *Server) listServiceTokens(ctx context.Context, in *serviceAccountRef) (*serviceTokensOutput, error) {
	p := principal(ctx)
	if err := s.requireServiceAdmin(p); err != nil {
		return nil, err
	}
	u, err := serviceAccount(ctx, s.deps.Store.Users(), p.OrgID, in.Name)
	if err != nil {
		return nil, toHumaError(err)
	}
	found, err := s.deps.Store.Tokens().ListByUser(ctx, p.OrgID, u.ID)
	if err != nil {
		return nil, toHumaError(err)
	}
	out := &serviceTokensOutput{}
	out.Body.Tokens = []ServiceTokenView{}
	for _, t := range found {
		if t.RevokedAt != nil {
			continue
		}
		out.Body.Tokens = append(out.Body.Tokens, ServiceTokenView{
			ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, ExpiresAt: t.ExpiresAt,
		})
	}
	return out, nil
}

// revokeServiceToken revokes one token of the account the path names.
// A token of anyone else, or one already revoked, is not found.
//
// @decision(DL-141)
func (s *Server) revokeServiceToken(ctx context.Context, in *serviceTokenRef) (*struct{}, error) {
	p := principal(ctx)
	if err := s.requireServiceAdmin(p); err != nil {
		return nil, err
	}
	now := s.deps.Now()
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		if err := liveAdminIn(ctx, tx, p); err != nil {
			return err
		}
		u, err := serviceAccount(ctx, tx.Users(), p.OrgID, in.Name)
		if err != nil {
			return err
		}
		noToken := huma.Error404NotFound("the service account " + u.Name + " holds no such token")
		rec, err := tx.Tokens().ByID(ctx, p.OrgID, in.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return noToken
		case err != nil:
			return err
		case rec.UserID != u.ID || rec.RevokedAt != nil:
			return noToken
		}
		if err := tx.Tokens().Revoke(ctx, p.OrgID, rec.ID, now); err != nil {
			return err
		}
		return tx.AdminEvents().Append(ctx, store.AdminEvent{
			ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminServiceTokenRevoked, At: now,
			Detail: "token " + rec.ID + " (" + rec.Name + ") of service account " + u.Name,
		})
	})
	if err != nil {
		return nil, toHumaError(err)
	}
	return nil, nil
}
