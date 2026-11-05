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
	// @decision(DL-007)
	admin, allowed, err := l.Allowlist.Live(ctx, identity.Email)
	if err != nil {
		l.warn("login: reading the allowlist: " + err.Error())
		l.deny(w, r, http.StatusBadGateway, "The login could not be completed.", "Try again in a moment.")
		return
	}
	if !allowed {
		// The address is named back to the person who just proved they
		// own it, so they can tell an operator which one to add.
		l.warn("login: refused " + identity.Email + ": not on the allowlist")
		l.deny(w, r, http.StatusForbidden,
			"The address "+identity.Email+" is not allowed to use this server.",
			"Ask an administrator to add it.")
		return
	}

	user, err := l.upsertUser(ctx, identity, admin)
	if err != nil {
		l.warn("login: " + err.Error())
		l.deny(w, r, http.StatusInternalServerError, "The login could not be recorded.", "Try again in a moment.")
		return
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

// upsertUser creates the user on first login and refreshes what GitHub
// owns on every one. The git identity is initialised once and then left
// alone: DL-044 lets the user change it, and re-copying the profile
// would silently undo that.
//
// @decision(DL-007)
func (l *Login) upsertUser(ctx context.Context, identity auth.GitHubIdentity, admin bool) (store.User, error) {
	now := l.now()
	existing, err := l.Users.ByEmail(ctx, l.OrgID, identity.Email)
	switch {
	case err == nil:
		existing.Name = identity.Name
		existing.GithubLogin = identity.Login
		existing.Admin = admin
		existing.LastLoginAt = &now
		if err := l.Users.Update(ctx, existing); err != nil {
			return store.User{}, fmt.Errorf("updating the user: %w", err)
		}
		return existing, nil
	case errors.Is(err, store.ErrNotFound):
	default:
		return store.User{}, fmt.Errorf("looking up the user: %w", err)
	}

	user := store.User{
		ID:          store.NewID(now),
		OrgID:       l.OrgID,
		Email:       identity.Email,
		Name:        identity.Name,
		GithubLogin: identity.Login,
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
		// Two first logins for the same address can both reach here;
		// the unique index on (org_id, lower(email)) lets exactly one
		// through. The loser reads the winner's row rather than
		// answering 500 to a login that is perfectly valid.
		if errors.Is(err, store.ErrConflict) {
			existing, getErr := l.Users.ByEmail(ctx, l.OrgID, identity.Email)
			if getErr == nil {
				return existing, nil
			}
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

// RevokeSessions deletes one address's browser sessions, so a re-added
// address does not resume an old session. It runs when an allowlist
// entry is deleted.
//
// @decision(DL-007)
func (l *Login) RevokeSessions(ctx context.Context, orgID, email string) error {
	if l.Users == nil || l.Sessions == nil {
		return nil
	}
	user, err := l.Users.ByEmail(ctx, orgID, email)
	if errors.Is(err, store.ErrNotFound) {
		// An address may be allowed before anyone logs in under it.
		return nil
	}
	if err != nil {
		return fmt.Errorf("looking up %s: %w", email, err)
	}
	// One pass over the sessions: Iterate reads every session row each
	// time it is called, and scs has no query by user.
	if err := l.Sessions.Iterate(ctx, func(ctx context.Context) error {
		if l.Sessions.GetString(ctx, auth.SessionKeyUserID) != user.ID {
			return nil
		}
		if err := l.Sessions.Destroy(ctx); err != nil {
			// One undeletable row must not strand the rest.
			l.warn("allowlist: revoking a session for " + email + ": " + err.Error())
		}
		return nil
	}); err != nil {
		return fmt.Errorf("revoking sessions: %w", err)
	}
	return nil
}
