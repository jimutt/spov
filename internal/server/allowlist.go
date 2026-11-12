package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// The two kinds of allowlist entry.
//
// @decision(DL-128)
const (
	EntryKindEmail  = "email"
	EntryKindGitHub = "github"
)

// AllowlistEntryView is one address or GitHub account that may log in.
//
// @decision(DL-007) @decision(DL-128)
type AllowlistEntryView struct {
	Kind  string `json:"kind" enum:"email,github" doc:"An address entry, or a GitHub account entry"`
	Email string `json:"email" doc:"The address, lower-cased; empty on an account entry"`
	// GithubLogin and Name are labels, refreshed when the account signs
	// in; GithubID is who the entry admits.
	GithubLogin string `json:"github_login,omitempty" doc:"The account's login as GitHub spells it"`
	GithubID    int64  `json:"github_id,omitempty" doc:"The account's numeric GitHub ID, which the entry admits"`
	Name        string `json:"name,omitempty" doc:"The account's display name, or the name of the user an address entry covers"`
	Admin       bool   `json:"admin" doc:"Whether this entry administers the org"`
	You         bool   `json:"you" doc:"Whether this entry covers the caller"`
	// CreatedAt is when the entry was first allowed, not when it was
	// last changed.
	CreatedAt time.Time `json:"created_at"`
	// LastLoginAt is the newest sign-in of a user the entry covers;
	// absent until one has signed in.
	LastLoginAt *time.Time `json:"last_login_at,omitempty" doc:"The newest sign-in of a user this entry covers"`
	// AlsoCoveredBy names, as a path would, the other entries that cover
	// a user this one covers: removing this entry ends nothing of theirs.
	// Unbound is an address entry covering a user row that no sign-in
	// has bound to an account yet: only an address entry admits that
	// row, so removing this one ends everything of theirs even if
	// their account has an entry.
	//
	// @decision(DL-128)
	Unbound       bool     `json:"unbound,omitempty" doc:"An address entry covering a user who has not signed in since accounts were recorded; removing it ends everything of theirs even if their account has an entry"`
	AlsoCoveredBy []string `json:"also_covered_by,omitempty" doc:"Other entries (an address or @login) that cover a user this entry covers; removing this one leaves that user their access"`
}

type allowlistOutput struct {
	Body struct {
		Entries []AllowlistEntryView `json:"entries"`
	}
}

type putAllowlistInput struct {
	Entry string `path:"entry" doc:"An address, a GitHub account written @login, or an existing account entry written id:<github_id>"`
	Body  struct {
		Admin bool `json:"admin" doc:"Whether this entry administers the org"`
	}
}

type allowlistEntryOutput struct {
	Body AllowlistEntryView
}

type deleteAllowlistInput struct {
	Entry string `path:"entry" doc:"The address, the GitHub account written @login, or the account entry written id:<github_id>, to remove"`
}

// entryRef is what a path names: an address, a GitHub login, or an
// account entry by its numeric ID.
//
// @decision(DL-128)
type entryRef struct {
	email string
	login string
	id    int64
}

func (r entryRef) String() string {
	switch {
	case r.id != 0:
		return idPrefix + strconv.FormatInt(r.id, 10)
	case r.login != "":
		return "@" + r.login
	}
	return r.email
}

// idPrefix names an account entry by its GitHub ID in a path. A login
// never has a colon, so the form cannot be read as one.
//
// @decision(DL-128)
const idPrefix = "id:"

// parseEntryRef reads a path segment. A leading "@" is a GitHub account,
// "id:" and digits an account entry by its ID; anything else must be an
// address.
//
// @decision(DL-128)
func parseEntryRef(s string) (entryRef, error) {
	s = strings.TrimSpace(s)
	if digits, ok := strings.CutPrefix(s, idPrefix); ok {
		id, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || id <= 0 || strings.Trim(digits, "0123456789") != "" {
			return entryRef{}, huma.Error422UnprocessableEntity(
				"an account entry is named id: and its numeric GitHub ID, such as id:583231")
		}
		return entryRef{id: id}, nil
	}
	if strings.HasPrefix(s, "@") {
		login, err := auth.ParseGitHubLogin(s)
		if err != nil {
			return entryRef{}, huma.Error422UnprocessableEntity(err.Error())
		}
		return entryRef{login: login}, nil
	}
	email, err := auth.ParseAllowlistEmail(s)
	if err != nil {
		return entryRef{}, huma.Error422UnprocessableEntity(err.Error() + "; a GitHub account is written @login")
	}
	return entryRef{email: email}, nil
}

// registerAllowlist serves the admin API for who may log in. Every
// operation needs an administrator, and none may leave the org without
// one.
//
// @decision(DL-007) @decision(DL-128)
func (s *Server) registerAllowlist() {
	register(s.api, huma.Operation{
		OperationID: "list-allowlist", Method: http.MethodGet, Path: "/api/v1/allowlist",
		Summary: "The addresses and GitHub accounts that may log in", Tags: []string{"admin"},
	}, func(ctx context.Context, _ *struct{}) (*allowlistOutput, error) {
		p := principal(ctx)
		if err := s.requireAllowlist(p); err != nil {
			return nil, err
		}
		views, err := s.allowlistViews(ctx, p)
		if err != nil {
			return nil, err
		}
		out := &allowlistOutput{}
		out.Body.Entries = views
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "put-allowlist-entry", Method: http.MethodPut, Path: "/api/v1/allowlist/{entry}",
		Summary: "Allow an address or a GitHub account to log in, or change its admin rights",
		Description: "The entry is an address, a GitHub account written @login, or an existing account entry" +
			" written id:<github_id>. A new account entry is always resolved with GitHub and stored by its" +
			" numeric ID; an existing one is matched by its stored login, or by its ID, without asking GitHub.",
		Tags: []string{"admin"},
		Errors: []int{http.StatusConflict, http.StatusUnprocessableEntity, http.StatusNotImplemented,
			http.StatusBadGateway},
	}, s.putAllowlistEntry)

	register(s.api, huma.Operation{
		OperationID: "delete-allowlist-entry", Method: http.MethodDelete, Path: "/api/v1/allowlist/{entry}",
		Summary: "Stop an address or a GitHub account logging in, and end what it has running and stored",
		Description: "Removes the entry. For each user it covered whom no other entry still covers, it ends" +
			" their browser sessions, cancels their unfinished jobs, stops their live sessions (a held sync" +
			" job's included), revokes their CLI tokens and deletes their stored credentials, then writes" +
			" an audit row. Their history is kept. Deleting an address or @login that is no longer on the" +
			" allowlist, but names a user who signed in and whom no entry covers, runs those steps again. An" +
			" account entry may be named id:<github_id>, which a retry matches against the user's account.",
		Tags:   []string{"admin"},
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}, s.deleteAllowlistEntry)
}

// putAllowlistEntry adds an entry or changes its admin rights. An
// account entry is resolved before the transaction, so the lock is
// never held across a call to GitHub.
//
// @decision(DL-007) @decision(DL-128)
func (s *Server) putAllowlistEntry(ctx context.Context, in *putAllowlistInput) (*allowlistEntryOutput, error) {
	p := principal(ctx)
	if err := s.requireAllowlist(p); err != nil {
		return nil, err
	}
	ref, err := parseEntryRef(in.Entry)
	if err != nil {
		return nil, err
	}
	var account store.AllowlistEntry
	switch {
	case ref.login != "":
		account, err = s.resolveAccountEntry(ctx, p, ref)
		if err != nil {
			return nil, err
		}
	case ref.id != 0:
		account.GithubID = ref.id
	}
	var stored store.AllowlistEntry
	err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
		al := tx.Allowlist()
		// Count before reading the entry: CountAdmins takes the org's
		// advisory lock, which must also cover the read of cur. Do
		// not move it into the guard; see DL-007.
		//
		// @decision(DL-007)
		admins, err := al.CountAdmins(ctx, p.OrgID)
		if err != nil {
			return err
		}
		mine, err := callerEntries(ctx, al, p)
		if err != nil {
			return err
		}
		if err := requireAdminEntry(mine); err != nil {
			return err
		}
		entry := store.AllowlistEntry{OrgID: p.OrgID, Email: ref.email, Admin: in.Body.Admin}
		var cur store.AllowlistEntry
		if ref.email != "" {
			cur, err = al.Get(ctx, p.OrgID, ref.email)
		} else {
			cur, err = al.GetAccount(ctx, p.OrgID, account.GithubID)
			// By ID only an existing entry is changed, and keeps its
			// labels; a new one is added by login, which GitHub resolves.
			//
			// @decision(DL-128)
			if ref.id != 0 {
				if errors.Is(err, store.ErrNotFound) {
					return huma.Error422UnprocessableEntity(ref.String() + " is not on the allowlist; add an account" +
						" by its @login, which is looked up on GitHub")
				}
				account = cur
			}
			entry.GithubID, entry.GithubLogin, entry.GithubName = account.GithubID, account.GithubLogin, account.GithubName
		}
		switch {
		case errors.Is(err, store.ErrNotFound):
			// A new entry; there is nothing to demote.
		case err != nil:
			return err
		case cur.Admin && !in.Body.Admin:
			if err := guardLastAdmin(losesOwnAdmin(cur, mine), admins, "remove your own admin rights"); err != nil {
				return err
			}
		}
		if err := al.Put(ctx, entry); err != nil {
			return err
		}
		// Read back inside the transaction: created_at is the
		// store's, kept from the existing row, and a read after
		// the commit would report someone else's later change as
		// the result of this call.
		if ref.email != "" {
			stored, err = al.Get(ctx, p.OrgID, ref.email)
		} else {
			stored, err = al.GetAccount(ctx, p.OrgID, account.GithubID)
		}
		return err
	})
	if err != nil {
		return nil, toHumaError(err)
	}
	s.allowlistChanged(ctx)
	entries, users, err := s.allowlistAndUsers(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	return &allowlistEntryOutput{Body: allowlistView(stored, entries, users, p)}, nil
}

// resolveAccountEntry is the account an "@login" PUT names: the entry
// whose stored login it is, without asking GitHub; else GitHub's answer,
// for an entry that does not exist yet. A user row's login is never
// used, since it can be the stale label of another account. The labels
// returned are what the entry will carry.
//
// @decision(DL-128)
func (s *Server) resolveAccountEntry(ctx context.Context, p auth.Principal, ref entryRef) (store.AllowlistEntry, error) {
	al := s.deps.Store.Allowlist()
	existing, err := accountEntry(ctx, al, p.OrgID, ref)
	switch {
	case err == nil:
		return existing, nil
	case !errors.Is(err, store.ErrNotFound):
		return store.AllowlistEntry{}, toHumaError(err)
	}
	// Only a caller who is an admin in the store may make the server
	// call GitHub; the transaction checks it again.
	mine, err := callerEntries(ctx, al, p)
	if err != nil {
		return store.AllowlistEntry{}, toHumaError(err)
	}
	if err := requireAdminEntry(mine); err != nil {
		return store.AllowlistEntry{}, err
	}
	a, err := auth.ResolveAccount(ctx, s.deps.GitHubAccounts, ref.login)
	if err != nil {
		return store.AllowlistEntry{}, lookupError(ref, err)
	}
	return store.AllowlistEntry{GithubID: a.ID, GithubLogin: a.Login, GithubName: a.Name}, nil
}

// lookupError says why an account could not be resolved, in the words
// the person adding it needs.
//
// @decision(DL-128)
func lookupError(ref entryRef, err error) error {
	switch {
	case errors.Is(err, auth.ErrNoSuchAccount):
		return huma.Error422UnprocessableEntity("GitHub knows no account named " + ref.String())
	case errors.Is(err, auth.ErrNotAPersonAccount):
		return huma.Error422UnprocessableEntity(ref.String() +
			" is an organisation's account on GitHub, not a person's; add each person instead")
	case errors.Is(err, auth.ErrGitHubRateLimited):
		return huma.Error502BadGateway(
			"GitHub is limiting how often this server may ask about accounts; try again later")
	case errors.Is(err, auth.ErrGitHubUnavailable):
		return huma.Error502BadGateway("GitHub could not be asked about " + ref.String() + "; try again")
	}
	return toHumaError(err)
}

// deleteAllowlistEntry removes an entry and off-boards each user it
// covered whom no other entry still covers.
//
// @decision(DL-007) @decision(DL-128) @decision(DL-132)
func (s *Server) deleteAllowlistEntry(ctx context.Context, in *deleteAllowlistInput) (*struct{}, error) {
	p := principal(ctx)
	if err := s.requireAllowlist(p); err != nil {
		return nil, err
	}
	ref, err := parseEntryRef(in.Entry)
	if err != nil {
		return nil, err
	}
	var leaving []store.User
	err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
		al := tx.Allowlist()
		// Count before reading the entry: CountAdmins takes the org's
		// advisory lock, which must also cover the read of cur. Do
		// not move it into the guard; see DL-007.
		//
		// @decision(DL-007)
		admins, err := al.CountAdmins(ctx, p.OrgID)
		if err != nil {
			return err
		}
		mine, err := callerEntries(ctx, al, p)
		if err != nil {
			return err
		}
		if err := requireAdminEntry(mine); err != nil {
			return err
		}
		cur, err := findEntry(ctx, al, p.OrgID, ref)
		if errors.Is(err, store.ErrNotFound) {
			leaving, err = retryOffboarding(ctx, tx, p.OrgID, ref)
			return err
		}
		if err != nil {
			return err
		}
		if cur.Admin {
			if err := guardLastAdmin(losesOwnAdmin(cur, mine), admins, "remove yourself"); err != nil {
				return err
			}
		}
		covered, err := coveredUsers(ctx, tx.Users(), cur)
		if err != nil {
			return err
		}
		if cur.GithubID != 0 {
			err = al.DeleteAccount(ctx, p.OrgID, cur.GithubID)
		} else {
			err = al.Delete(ctx, p.OrgID, cur.Email)
		}
		if err != nil {
			return err
		}
		// Inside the transaction, so another entry removed at the same
		// time cannot leave a person covered by neither and ended by
		// neither.
		//
		// @decision(DL-128)
		leaving, err = uncovered(ctx, al, covered)
		return err
	})
	if err != nil {
		return nil, toHumaError(err)
	}
	s.allowlistChanged(ctx)
	if len(leaving) == 0 {
		return nil, nil
	}
	// The per-request check already locks them out; this deletes
	// their web sessions too, so a re-added person does not resume
	// an old one.
	//
	// @decision(DL-007)
	if s.revokeSessions != nil {
		if err := s.revokeSessions(ctx, leaving); err != nil {
			// The entry is gone, which is what was asked for. A
			// session that outlived it is inert within one refresh
			// interval, so this is a warning, not a failed delete.
			s.deps.Warn("allowlist: revoking sessions for " + ref.String() + ": " + err.Error())
		}
	}
	// After the commit, so the person is locked out before anything
	// of theirs is ended. Each step runs even when one fails.
	//
	// @decision(DL-132)
	var failed []string
	for _, u := range leaving {
		if _, err := s.svc.Offboard(ctx, p, describeUser(u), u); err != nil {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) > 0 {
		return nil, huma.Error500InternalServerError(strings.Join(failed, "; "))
	}
	return nil, nil
}

// retryOffboarding is a delete that names no entry. A user it names (by
// address, or by the login on a row bound to an account) whom no entry
// covers is off-boarded again, which is how a partial removal is
// retried; anything else is a 404.
//
// @decision(DL-132) @decision(DL-128)
func retryOffboarding(ctx context.Context, tx store.Store, orgID string, ref entryRef) ([]store.User, error) {
	var candidates []store.User
	switch {
	case ref.email != "" || ref.id != 0:
		var u store.User
		var err error
		if ref.id != 0 {
			u, err = tx.Users().ByGithubID(ctx, orgID, ref.id)
		} else {
			u, err = tx.Users().ByEmail(ctx, orgID, ref.email)
		}
		switch {
		case err == nil:
			candidates = []store.User{u}
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	default:
		rows, err := tx.Users().ByGithubLogin(ctx, orgID, ref.login)
		if err != nil {
			return nil, err
		}
		if len(rows) > 1 {
			return nil, ambiguousLogin(ref, nil)
		}
		candidates = rows
	}
	leaving, err := uncovered(ctx, tx.Allowlist(), candidates)
	if err != nil {
		return nil, err
	}
	if len(leaving) == 0 {
		// Not "no such session", which is what the shared mapper
		// says for a missing row.
		return nil, huma.Error404NotFound(ref.String() + " is not on the allowlist")
	}
	return leaving, nil
}

// findEntry reads the entry a path names.
//
// @decision(DL-128)
func findEntry(ctx context.Context, al store.AllowlistStore, orgID string, ref entryRef) (store.AllowlistEntry, error) {
	switch {
	case ref.email != "":
		return al.Get(ctx, orgID, ref.email)
	case ref.id != 0:
		return al.GetAccount(ctx, orgID, ref.id)
	}
	return accountEntry(ctx, al, orgID, ref)
}

// accountEntry is the account entry whose stored login a path names,
// compared case-insensitively. The login is a label, so two entries can
// carry it after a rename; that is refused rather than guessed.
//
// @decision(DL-128)
func accountEntry(ctx context.Context, al store.AllowlistStore, orgID string, ref entryRef) (store.AllowlistEntry, error) {
	rows, err := al.ByLogin(ctx, orgID, ref.login)
	if err != nil {
		return store.AllowlistEntry{}, err
	}
	switch len(rows) {
	case 0:
		return store.AllowlistEntry{}, store.ErrNotFound
	case 1:
		return rows[0], nil
	}
	return store.AllowlistEntry{}, ambiguousLogin(ref, rows)
}

// ambiguousLogin refuses a login more than one entry, or user, carries.
// For entries it names each by its id: form, which reaches it.
//
// @decision(DL-128)
func ambiguousLogin(ref entryRef, rows []store.AllowlistEntry) error {
	if len(rows) == 0 {
		return huma.Error409Conflict("more than one user of this server carries the GitHub login " + ref.String() +
			", because an account was renamed; name the entry by its address or id:<github_id> instead")
	}
	names := make([]string, 0, len(rows))
	for _, e := range rows {
		name := idPrefix + strconv.FormatInt(e.GithubID, 10)
		if e.GithubName != "" {
			name += " (" + e.GithubName + ")"
		}
		names = append(names, name)
	}
	return huma.Error409Conflict("more than one entry carries the GitHub login " + ref.String() +
		", because an account was renamed: " + strings.Join(names, " and ") + "; name the one you mean by its id: form")
}

// coveredUsers are the user rows an entry covers: the row with its
// address, or the row bound to its account.
//
// @decision(DL-128)
func coveredUsers(ctx context.Context, users store.UserStore, e store.AllowlistEntry) ([]store.User, error) {
	var u store.User
	var err error
	if e.GithubID != 0 {
		u, err = users.ByGithubID(ctx, e.OrgID, e.GithubID)
	} else {
		u, err = users.ByEmail(ctx, e.OrgID, e.Email)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return []store.User{u}, nil
}

// uncovered keeps the users no entry covers.
//
// @decision(DL-128)
func uncovered(ctx context.Context, al store.AllowlistStore, users []store.User) ([]store.User, error) {
	var out []store.User
	for _, u := range users {
		entries, err := entriesCovering(ctx, al, u.OrgID, u.GithubID, u.Email)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			out = append(out, u)
		}
	}
	return out, nil
}

// entriesCovering reads the entries that cover one account and address.
// A githubID of zero is a row not bound to an account yet.
//
// @decision(DL-128)
func entriesCovering(ctx context.Context, al store.AllowlistStore, orgID string, githubID int64, email string) ([]store.AllowlistEntry, error) {
	var out []store.AllowlistEntry
	if email != "" {
		e, err := al.Get(ctx, orgID, email)
		switch {
		case err == nil:
			out = append(out, e)
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}
	if githubID != 0 {
		e, err := al.GetAccount(ctx, orgID, githubID)
		switch {
		case err == nil:
			out = append(out, e)
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}
	return out, nil
}

// callerEntries reads the caller's own entries from the store, inside
// the caller's transaction after CountAdmins has taken the org's lock.
//
// @decision(DL-007) @decision(DL-128)
func callerEntries(ctx context.Context, al store.AllowlistStore, p auth.Principal) ([]store.AllowlistEntry, error) {
	return entriesCovering(ctx, al, p.OrgID, p.GitHubID, p.Email)
}

// requireLiveAdmin refuses a caller whose own entries no longer grant
// admin, reading the store rather than the cached Principal.Admin. It
// must run inside the caller's transaction, after CountAdmins has taken
// the org's lock.
//
// @decision(DL-007) @decision(DL-128)
func requireLiveAdmin(ctx context.Context, al store.AllowlistStore, p auth.Principal) error {
	mine, err := callerEntries(ctx, al, p)
	if err != nil {
		return err
	}
	return requireAdminEntry(mine)
}

// requireAdminEntry refuses a caller none of whose own entries, as
// callerEntries read them, grants admin.
//
// @decision(DL-007) @decision(DL-128)
func requireAdminEntry(mine []store.AllowlistEntry) error {
	for _, e := range mine {
		if e.Admin {
			return nil
		}
	}
	return huma.Error403Forbidden(
		"your administrator rights have been removed; this credential is still cached as an admin" +
			" but the allowlist is not")
}

// losesOwnAdmin reports whether demoting or removing target would leave
// the caller with no admin entry: it is one of their entries, and no
// other admin entry covers them.
//
// @decision(DL-128)
func losesOwnAdmin(target store.AllowlistEntry, mine []store.AllowlistEntry) bool {
	covers, other := false, false
	for _, e := range mine {
		switch {
		case sameEntry(e, target):
			covers = true
		case e.Admin:
			other = true
		}
	}
	return covers && !other
}

func sameEntry(a, b store.AllowlistEntry) bool {
	if a.GithubID != 0 || b.GithubID != 0 {
		return a.GithubID == b.GithubID
	}
	return sameEmail(a.Email, b.Email)
}

// guardLastAdmin refuses a change that removes the caller's own last
// admin entry (self), and any change that would leave the org with no
// admin entry. admins is the count read at the top of the caller's
// transaction. The zero-admin rule is unreachable through the handlers
// today and is kept on purpose; it is tested directly.
//
// @decision(DL-007) @decision(DL-128)
func guardLastAdmin(self bool, admins int, what string) error {
	if self {
		return huma.Error422UnprocessableEntity("you cannot " + what + "; ask another admin")
	}
	if admins <= 1 {
		return huma.Error422UnprocessableEntity(
			"this is the org's last administrator; grant admin to someone else first")
	}
	return nil
}

// requireAllowlist refuses a caller who is not an administrator, a
// server with no login (and so no allowlist, as in local mode), and a
// caller outside the org the allowlist governs, such as the shared
// bearer token's "local" org.
//
// @decision(DL-007)
func (s *Server) requireAllowlist(p auth.Principal) error {
	if err := s.requireAdmin(p); err != nil {
		return err
	}
	if s.deps.Login == nil {
		return huma.Error501NotImplemented(
			"this server has no allowlist: it has no browser login, so there is nobody to allow")
	}
	if p.OrgID != s.deps.Login.OrgID {
		return huma.Error403Forbidden(
			"this credential is not in the org the allowlist governs, so it could only edit an allowlist" +
				" nothing reads; use a browser session or a token from `spov login`")
	}
	return nil
}

// allowlistChanged refreshes this replica's cached copy, so the admin
// who made the change sees it on their next request. Other replicas
// catch up on their own refresh timer.
//
// @decision(DL-007)
func (s *Server) allowlistChanged(ctx context.Context) {
	if s.deps.Login == nil || s.deps.Login.Allowlist == nil {
		return
	}
	// Refresh has already said why and kept the previous list.
	_ = s.deps.Login.Allowlist.Refresh(ctx)
}

func sameEmail(a, b string) bool {
	return a != "" && strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// describeUser names a user in an audit row and an answer.
//
// @decision(DL-128)
func describeUser(u store.User) string {
	if u.GithubID != 0 && u.GithubLogin != "" {
		return "@" + u.GithubLogin + " (" + u.Email + ")"
	}
	return u.Email
}

// allowlistAndUsers reads the org's entries and users, which every view
// is drawn from.
//
// @decision(DL-128)
func (s *Server) allowlistAndUsers(ctx context.Context, orgID string) ([]store.AllowlistEntry, []store.User, error) {
	entries, err := s.deps.Store.Allowlist().List(ctx, orgID)
	if err != nil {
		return nil, nil, toHumaError(err)
	}
	users, err := s.deps.Store.Users().List(ctx, orgID)
	if err != nil {
		return nil, nil, toHumaError(err)
	}
	return entries, users, nil
}

// allowlistViews is the whole list as the API shows it.
//
// @decision(DL-128)
func (s *Server) allowlistViews(ctx context.Context, p auth.Principal) ([]AllowlistEntryView, error) {
	entries, users, err := s.allowlistAndUsers(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	out := make([]AllowlistEntryView, 0, len(entries))
	for _, e := range entries {
		out = append(out, allowlistView(e, entries, users, p))
	}
	return out, nil
}

// entryCovers reports whether e covers the user row u: the row with its
// address, or the row bound to its account.
//
// @decision(DL-128)
func entryCovers(e store.AllowlistEntry, u store.User) bool {
	if e.GithubID != 0 {
		return u.GithubID == e.GithubID
	}
	return sameEmail(u.Email, e.Email)
}

// entryName is how the API and the page name an entry in a path.
func entryName(e store.AllowlistEntry) string {
	if e.GithubID != 0 {
		return "@" + e.GithubLogin
	}
	return e.Email
}

// allowlistView is an entry as the API shows it, with what the users it
// covers say about it: their newest sign-in, and the other entries that
// cover them too, which removing this one leaves in place.
//
// @decision(DL-128)
func allowlistView(e store.AllowlistEntry, entries []store.AllowlistEntry, users []store.User, p auth.Principal) AllowlistEntryView {
	v := AllowlistEntryView{
		Kind: EntryKindEmail, Email: e.Email, Admin: e.Admin, CreatedAt: e.CreatedAt,
		You: (e.GithubID != 0 && e.GithubID == p.GitHubID) || (e.GithubID == 0 && sameEmail(e.Email, p.Email)),
	}
	if e.GithubID != 0 {
		v.Kind, v.GithubID, v.GithubLogin, v.Name = EntryKindGitHub, e.GithubID, e.GithubLogin, e.GithubName
	}
	for _, u := range users {
		if !entryCovers(e, u) {
			continue
		}
		if e.GithubID == 0 && u.GithubID == 0 {
			v.Unbound = true
		}
		for _, o := range entries {
			// An account entry cannot admit an unbound row, so it is
			// no cover for one.
			if !sameEntry(o, e) && entryCovers(o, u) && !slices.Contains(v.AlsoCoveredBy, entryName(o)) {
				v.AlsoCoveredBy = append(v.AlsoCoveredBy, entryName(o))
			}
		}
		if u.LastLoginAt == nil {
			continue
		}
		if v.LastLoginAt == nil || u.LastLoginAt.After(*v.LastLoginAt) {
			at := *u.LastLoginAt
			v.LastLoginAt = &at
			if e.GithubID == 0 {
				v.Name = u.Name
			}
		}
	}
	return v
}
