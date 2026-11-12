package server

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"golang.org/x/oauth2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// Login is the browser login flow: the routes, the session manager they
// write to, and the GitHub app they delegate to. It is nil in local
// mode, where the only credential is the bearer token.
//
// @decision(DL-007)
type Login struct {
	// OAuth exchanges GitHub's code for an identity.
	OAuth *auth.GitHubOAuth
	// Sessions is the scs manager whose cookie carries the login.
	Sessions *scs.SessionManager
	// Users is where a first login creates its row.
	Users store.UserStore
	// Allowlist decides who may log in at all.
	Allowlist *auth.Allowlist
	// Store is where a row from before accounts were recorded is bound,
	// in one transaction with the read of the address entry that allows
	// it. Nil binds nothing.
	//
	// @decision(DL-128)
	Store store.Store
	// Entries is the allowlist's table, where a sign-in refreshes an
	// account entry's login and name. Nil skips that.
	//
	// @decision(DL-128)
	Entries store.AllowlistStore
	// OrgID owns every user created here.
	OrgID string
	// Now supplies timestamps; nil means time.Now.
	Now func() time.Time
	// Warn receives one line per failed login; nil discards.
	Warn func(string)
}

// DefaultOrgID owns every user a hosted login creates. One org in v1;
// the column is there so that more become rows rather than a migration.
//
// @decision(DL-007)
const DefaultOrgID = "default"

// FlowLifetime bounds how long a person may take between /login and the
// callback.
//
// @decision(DL-007)
const FlowLifetime = 15 * time.Minute

// Session keys holding the in-progress flow. They live in the same scs
// session the login will land in, which scs renews on login, so a
// completed flow cannot be replayed with the pre-login token.
const (
	sessionKeyState    = "oauth_state"
	sessionKeyVerifier = "oauth_verifier"
	sessionKeyNext     = "oauth_next"
)

// LoginPaths are the routes the flow owns. They sit outside APIPrefix,
// so the API's authentication middleware never sees them; the browser
// guard still does.
//
// @decision(DL-007)
const (
	LoginPath    = "/login"
	CallbackPath = "/oauth/github/callback"
	LogoutPath   = "/logout"
)

// register mounts the flow on mux.
//
// @decision(DL-007)
func (l *Login) register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+LoginPath, l.handleLogin)
	mux.HandleFunc("GET "+CallbackPath, l.handleCallback)
	mux.HandleFunc("POST "+LogoutPath, l.handleLogout)
}

func (l *Login) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now().UTC()
}

func (l *Login) warn(msg string) {
	if l.Warn != nil {
		l.Warn(msg)
	}
}

// handleLogin starts the flow: a fresh state nonce and PKCE verifier go
// into the session, and the browser is sent to GitHub.
//
// @decision(DL-007)
func (l *Login) handleLogin(w http.ResponseWriter, r *http.Request) {
	// An already-authenticated browser has nothing to do here.
	if l.Sessions.GetString(r.Context(), auth.SessionKeyUserID) != "" {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	state := oauth2.GenerateVerifier()
	verifier := oauth2.GenerateVerifier()
	l.Sessions.Put(r.Context(), sessionKeyState, state)
	l.Sessions.Put(r.Context(), sessionKeyVerifier, verifier)
	l.Sessions.Put(r.Context(), sessionKeyNext, safeNext(r.URL.Query().Get("next")))
	// A completed login calls RenewToken, which carries the deadline
	// forward, so handleCallback resets it to the full lifetime.
	//
	// @decision(DL-007)
	l.Sessions.SetDeadline(r.Context(), l.now().Add(FlowLifetime))
	http.Redirect(w, r, l.OAuth.AuthCodeURL(state, verifier), http.StatusSeeOther)
}

// handleCallback completes the flow.
//
// @decision(DL-007)
func (l *Login) handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()
	// GitHub reports a user who refused consent this way; it is not a
	// failure worth logging as one.
	if reason := query.Get("error"); reason != "" {
		// GitHub's own error code, which is not a secret and is the
		// only clue an operator gets for a misconfigured app:
		// redirect_uri_mismatch arrives this way, not just
		// access_denied.
		// Whoever calls the callback chooses this text, so it is
		// truncated and quoted rather than pasted into the log.
		l.warn(fmt.Sprintf("login: github refused the authorization: %.64q", reason))
		l.deny(w, r, http.StatusForbidden, "GitHub did not complete the login.",
			"If this keeps happening, the server's OAuth app may be misconfigured.")
		return
	}
	wantState := l.Sessions.PopString(ctx, sessionKeyState)
	verifier := l.Sessions.PopString(ctx, sessionKeyVerifier)
	next := l.Sessions.PopString(ctx, sessionKeyNext)
	if wantState == "" || verifier == "" {
		// No flow is in progress: a bookmarked callback, or a cookie
		// that was dropped between the redirect and the return.
		l.deny(w, r, http.StatusBadRequest, "This login has expired.", "Start again from the login page.")
		return
	}
	if state := query.Get("state"); state == "" || state != wantState {
		l.warn("login: the callback state did not match from " + r.RemoteAddr)
		l.deny(w, r, http.StatusBadRequest, "This login could not be verified.", "Start again from the login page.")
		return
	}
	code := query.Get("code")
	if code == "" {
		l.deny(w, r, http.StatusBadRequest, "GitHub returned no authorization code.", "")
		return
	}

	identity, err := l.OAuth.Exchange(ctx, code, verifier)
	if err != nil {
		if errors.Is(err, auth.ErrNoVerifiedEmail) {
			l.deny(w, r, http.StatusForbidden,
				"Your GitHub account has no primary, verified e-mail address.",
				"Verify your primary address on GitHub, then try again.")
			return
		}
		l.warn("login: " + err.Error())
		l.deny(w, r, http.StatusBadGateway, "The login could not be completed.", "Try again in a moment.")
		return
	}

	// The store, not the cached copy: an admin who has just added this
	// address should not have to tell the person to wait out a refresh
	// interval. This is once per browser login, not per request.
	//
	// @decision(DL-007) @decision(DL-128)
	admin, allowed, err := l.Allowlist.Live(ctx, identity.ID, identity.Email)
	if err != nil {
		l.warn("login: reading the allowlist: " + err.Error())
		l.deny(w, r, http.StatusBadGateway, "The login could not be completed.", "Try again in a moment.")
		return
	}
	who := "@" + identity.Login + " (" + identity.Email + ")"
	if !allowed {
		// The account and the address are named back to the person who
		// just proved they own them, so they can tell an operator either.
		l.warn(fmt.Sprintf("login: refused %q: not on the allowlist", who))
		l.deny(w, r, http.StatusForbidden,
			"The GitHub account "+who+" is not allowed to use this server.",
			"Ask an administrator to add the account or the address.")
		return
	}

	user, err := l.upsertUser(ctx, identity, admin)
	if errors.Is(err, errEarlierUser) {
		l.warn(fmt.Sprintf("login: refused %q: the address is an earlier user's, and no address entry allows it", who))
		l.deny(w, r, http.StatusForbidden,
			"The address "+identity.Email+" is recorded for an earlier user of this server.",
			"An administrator can connect your account to it by adding the address as an entry.")
		return
	}
	if errors.Is(err, errAddressTaken) {
		l.warn(fmt.Sprintf("login: refused %q: the address is recorded for another account", who))
		l.deny(w, r, http.StatusForbidden,
			"The address "+identity.Email+" belongs to another account on this server.",
			"Sign in with that account, or ask an administrator.")
		return
	}
	if err != nil {
		l.warn("login: " + err.Error())
		l.deny(w, r, http.StatusInternalServerError, "The login could not be recorded.", "Try again in a moment.")
		return
	}
	// The labels follow the profile; the ID is what the entry admits.
	//
	// @decision(DL-128)
	if l.Entries != nil {
		if err := l.Entries.Relabel(ctx, l.OrgID, identity.ID, identity.Login, identity.Name); err != nil {
			l.warn("login: refreshing the allowlist entry's labels: " + err.Error())
		}
	}

	// A new session token for the authenticated session: the one that
	// carried the OAuth state must not also be the logged-in one.
	if err := l.Sessions.RenewToken(ctx); err != nil {
		l.warn("login: renewing the session token: " + err.Error())
		l.deny(w, r, http.StatusInternalServerError, "The login could not be recorded.", "Try again in a moment.")
		return
	}
	l.Sessions.Put(ctx, auth.SessionKeyUserID, user.ID)
	// RenewToken carries the flow's short deadline forward, so the
	// logged-in session gets the real lifetime back.
	//
	// @decision(DL-007)
	l.Sessions.SetDeadline(ctx, l.now().Add(l.Sessions.Lifetime))
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

// errAddressTaken is a sign-in whose address is recorded on a row of
// another account, or on a row the account could not be bound to. The
// address never moves and is never shared.
//
// @decision(DL-128)
var errAddressTaken = errors.New("the address is recorded for another user")

// errEarlierUser is a sign-in whose address is on a row from before
// accounts were recorded that no address entry allows: the row is not
// bound, and no second row is made for the address.
//
// @decision(DL-128)
var errEarlierUser = errors.New("the address is recorded for an earlier user")

// upsertUser finds the account's row, binds a row from before accounts
// were recorded, or creates one, and refreshes what GitHub owns. The row
// is found by GitHub ID, and only when none has it, by address among
// rows with no account yet. The git identity is initialised once and
// then left alone: DL-044 lets the user change it, and re-copying the
// profile would silently undo that.
//
// @decision(DL-007) @decision(DL-128)
func (l *Login) upsertUser(ctx context.Context, identity auth.GitHubIdentity, admin bool) (store.User, error) {
	existing, err := l.Users.ByGithubID(ctx, l.OrgID, identity.ID)
	switch {
	case err == nil:
		return l.refreshUser(ctx, existing, identity, admin)
	case !errors.Is(err, store.ErrNotFound):
		return store.User{}, fmt.Errorf("looking up the user: %w", err)
	}
	byEmail, err := l.Users.ByEmail(ctx, l.OrgID, identity.Email)
	switch {
	case err == nil:
		return l.bindUser(ctx, byEmail, identity, admin)
	case !errors.Is(err, store.ErrNotFound):
		return store.User{}, fmt.Errorf("looking up the user: %w", err)
	}
	return l.createUser(ctx, identity, admin)
}

// refreshUser updates a row found by its account. Its address follows
// the account's current primary one, unless another row records that
// address.
//
// @decision(DL-128)
func (l *Login) refreshUser(ctx context.Context, u store.User, identity auth.GitHubIdentity, admin bool) (store.User, error) {
	now := l.now()
	if !strings.EqualFold(u.Email, identity.Email) {
		other, err := l.Users.ByEmail(ctx, l.OrgID, identity.Email)
		switch {
		case err == nil && other.ID != u.ID:
			return store.User{}, errAddressTaken
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return store.User{}, fmt.Errorf("looking up the address: %w", err)
		}
		u.Email = identity.Email
	}
	u.Name = identity.Name
	u.GithubLogin = identity.Login
	u.Admin = admin
	u.LastLoginAt = &now
	if err := l.Users.Update(ctx, u); err != nil {
		// The unique address index is the backstop for a race with
		// another row taking the address.
		if errors.Is(err, store.ErrConflict) {
			return store.User{}, errAddressTaken
		}
		return store.User{}, fmt.Errorf("updating the user: %w", err)
	}
	return u, nil
}

// bindUser binds a row found by address to the account that proved it,
// once. A row already bound to another account is never taken over.
//
// @decision(DL-128)
func (l *Login) bindUser(ctx context.Context, u store.User, identity auth.GitHubIdentity, admin bool) (store.User, error) {
	if u.GithubID != 0 && u.GithubID != identity.ID {
		return store.User{}, errAddressTaken
	}
	if l.Store == nil {
		return store.User{}, errEarlierUser
	}
	// Only an address entry admits the row's address, so only one
	// lets an account become the earlier user: an account entry alone
	// would hand that user's credentials and tokens to whoever holds the
	// address now. Read under the allowlist's lock, so a removal of the
	// address entry is either before the bind or after it.
	//
	// @decision(DL-128)
	err := l.Store.Tx(ctx, func(tx store.Store) error {
		al := tx.Allowlist()
		if _, err := al.CountAdmins(ctx, l.OrgID); err != nil {
			return err
		}
		if _, err := al.Get(ctx, l.OrgID, identity.Email); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errEarlierUser
			}
			return err
		}
		return tx.Users().BindGithub(ctx, l.OrgID, u.ID, identity.ID)
	})
	if err != nil {
		if errors.Is(err, errEarlierUser) {
			return store.User{}, errEarlierUser
		}
		if errors.Is(err, store.ErrConflict) {
			// Another sign-in bound it first: to this account is fine,
			// to any other is the refusal.
			if again, getErr := l.Users.ByGithubID(ctx, l.OrgID, identity.ID); getErr == nil && again.ID == u.ID {
				return l.refreshUser(ctx, again, identity, admin)
			}
			return store.User{}, errAddressTaken
		}
		return store.User{}, fmt.Errorf("binding the user to their GitHub account: %w", err)
	}
	u.GithubID = identity.ID
	return l.refreshUser(ctx, u, identity, admin)
}

// createUser makes the row on a first login.
//
// @decision(DL-007) @decision(DL-128)
func (l *Login) createUser(ctx context.Context, identity auth.GitHubIdentity, admin bool) (store.User, error) {
	now := l.now()
	user := store.User{
		ID:          store.NewID(now),
		OrgID:       l.OrgID,
		Email:       identity.Email,
		Name:        identity.Name,
		GithubLogin: identity.Login,
		GithubID:    identity.ID,
		Admin:       admin,
		GitName:     identity.Name,
		GitEmail:    identity.Email,
		LastLoginAt: &now,
		CreatedAt:   now,
	}
	if user.GitName == "" {
		user.GitName = identity.Login
	}
	if err := l.Users.Create(ctx, user); err != nil {
		// Two first logins for the same account can both reach here;
		// the unique indexes let exactly one through. The loser reads
		// the winner's row rather than answering 500 to a login that is
		// perfectly valid, and a row of another account is the refusal.
		if errors.Is(err, store.ErrConflict) {
			if existing, getErr := l.Users.ByGithubID(ctx, l.OrgID, identity.ID); getErr == nil {
				return existing, nil
			}
			return store.User{}, errAddressTaken
		}
		return store.User{}, fmt.Errorf("creating the user: %w", err)
	}
	return user, nil
}

// handleLogout destroys the session and its row.
//
// @decision(DL-007)
func (l *Login) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := l.Sessions.Destroy(r.Context()); err != nil {
		l.warn("logout: " + err.Error())
		http.Error(w, "could not log out", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// safeNext keeps a post-login redirect on this origin. Anything else —
// an absolute URL, a scheme-relative "//host" — is dropped rather than
// corrected, since a login that lands somewhere unexpected is worse than
// one that lands at the root.
//
// @decision(DL-007)
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	// A parse failure, or anything that carries its own origin, is not
	// a path on this server.
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return u.RequestURI()
}

// deny renders the reason a login failed. It is a page rather than JSON
// because the audience is a browser that followed a redirect.
//
// @decision(DL-007)
func (l *Login) deny(w http.ResponseWriter, _ *http.Request, status int, reason, remedy string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = denyPage.Execute(w, struct{ Reason, Remedy, LoginPath string }{reason, remedy, LoginPath})
}

// denyPage is the page a failed login renders.
var denyPage = template.Must(template.New("deny").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>spov</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font:16px/1.5 system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;color:#111}
main{max-width:32rem;padding:2rem}h1{font-size:1.25rem}p{color:#444}</style></head>
<body><main><h1>spov could not log you in</h1>
<p>{{.Reason}}</p>{{if .Remedy}}<p>{{.Remedy}}</p>{{end}}
<p><a href="{{.LoginPath}}">Try again</a></p></main></body></html>
`))

// isLoginPath reports whether a path belongs to the login flow. Those
// routes are unauthenticated: they are how a browser gets a credential.
//
// @decision(DL-007)
func isLoginPath(p string) bool {
	switch p {
	case LoginPath, CallbackPath, LogoutPath:
		return true
	}
	return false
}

// RevokeSessions deletes the given users' browser sessions, so a person
// put back on the allowlist does not resume an old session. It runs when
// an allowlist entry is deleted, for each user the entry covered whom no
// other entry still covers.
//
// @decision(DL-007) @decision(DL-128)
func (l *Login) RevokeSessions(ctx context.Context, users []store.User) error {
	if l.Sessions == nil || len(users) == 0 {
		return nil
	}
	ids := make(map[string]bool, len(users))
	for _, u := range users {
		ids[u.ID] = true
	}
	// One pass over the sessions: Iterate reads every session row each
	// time it is called, and scs has no query by user.
	if err := l.Sessions.Iterate(ctx, func(ctx context.Context) error {
		id := l.Sessions.GetString(ctx, auth.SessionKeyUserID)
		if !ids[id] {
			return nil
		}
		if err := l.destroyIterated(ctx); err != nil {
			// One undeletable row must not strand the rest.
			l.warn("allowlist: revoking a session of user " + id + ": " + err.Error())
		}
		return nil
	}); err != nil {
		return fmt.Errorf("revoking sessions: %w", err)
	}
	return nil
}
