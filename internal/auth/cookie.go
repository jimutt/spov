package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jimutt/spov/internal/store"
)

// SessionKeyUserID is the scs key holding the logged-in user's ID. It is
// the only thing a session carries: everything else is read from the
// store on each request, so an edited user or a revoked allowlist entry
// takes effect immediately.
//
// @decision(DL-007)
const SessionKeyUserID = "user_id"

// SessionReader is the part of an scs.SessionManager the authenticator
// uses. Narrow so tests need no session manager.
//
// @decision(DL-007)
type SessionReader interface {
	// GetString returns the value stored under key, or "" when the
	// request carries no session.
	GetString(ctx context.Context, key string) string
}

// CookieAuthenticator resolves the browser's session cookie to a
// principal. The cookie itself is an opaque scs token; the identity
// comes from the store, and the allowlist is re-checked on every
// request, so removing a line locks the user out on their next request
// rather than at session expiry.
//
// @decision(DL-007)
type CookieAuthenticator struct {
	Sessions  SessionReader
	Users     store.UserStore
	Allowlist *Allowlist
	// OrgID is the org logins belong to. One org in v1 (DL-007).
	OrgID string
}

// Authenticate implements Authenticator.
//
// @decision(DL-007)
func (a *CookieAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	userID := a.Sessions.GetString(r.Context(), SessionKeyUserID)
	if userID == "" {
		return Principal{}, ErrUnauthenticated
	}
	user, err := a.Users.Get(r.Context(), a.OrgID, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The session outlived its user. It is a credential that
			// was presented and is no longer good, not an absent one.
			return Principal{}, fmt.Errorf("%w: the session's user is gone", ErrBadCredential)
		}
		return Principal{}, fmt.Errorf("auth: loading the session's user: %w", err)
	}
	// @decision(DL-128)
	admin, ok := a.Allowlist.Lookup(user.GithubID, user.Email)
	if !ok {
		return Principal{}, fmt.Errorf("%w: %s is no longer on the allowlist", ErrBadCredential, user.Email)
	}
	// The allowlist is the authority on admin, not the stored row: an
	// operator edits the file, and the change must not wait for a login.
	return Principal{
		UserID: user.ID, OrgID: user.OrgID, Email: user.Email, GitHubID: user.GithubID, Admin: admin,
	}, nil
}

// Chain tries each authenticator in order and returns the first that
// produces a principal or a definite rejection. Only ErrUnauthenticated
// — no credential of that kind was presented — moves on to the next.
//
// @decision(DL-007)
type Chain []Authenticator

// Authenticate implements Authenticator.
//
// @decision(DL-007)
func (c Chain) Authenticate(r *http.Request) (Principal, error) {
	for _, a := range c {
		p, err := a.Authenticate(r)
		if !errors.Is(err, ErrUnauthenticated) {
			return p, err
		}
	}
	return Principal{}, ErrUnauthenticated
}
