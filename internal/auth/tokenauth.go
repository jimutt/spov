package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// TouchInterval is how stale last_used_at may be. Writing it on every
// request would turn a read-only API call into a write.
//
// @decision(DL-006)
const TouchInterval = time.Minute

// TokenAuthenticator resolves a `spov_pat_` bearer token to its user.
// The allowlist is re-checked on every request, exactly as the cookie
// authenticator does (DL-007), so revoking someone's access does not
// depend on which credential they hold.
//
// @decision(DL-006)
type TokenAuthenticator struct {
	Tokens    store.TokenStore
	Users     store.UserStore
	Allowlist *Allowlist
	// Now supplies timestamps; nil means time.Now.
	Now func() time.Time
	// Warn receives one line per problem that is not the caller's
	// fault; nil discards. It never receives a credential.
	Warn func(string)
}

// Authenticate implements Authenticator.
//
// @decision(DL-006)
func (a *TokenAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	presented, ok := bearer(r)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	// A bearer that is not a spov token is not this authenticator's to
	// judge: the chain may hold another that recognises it.
	token, err := ParseToken(presented)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}

	ctx := r.Context()
	rec, err := a.Tokens.ByHash(ctx, HashToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Well formed and unknown: a revoked token that was
			// deleted, or a guess. Presented and wrong, so the chain
			// stops here rather than trying the next authenticator.
			return Principal{}, fmt.Errorf("%w: no such token", ErrBadCredential)
		}
		return Principal{}, fmt.Errorf("auth: looking up the token: %w", err)
	}
	now := a.now()
	if !rec.Usable(now) {
		return Principal{}, fmt.Errorf("%w: the token is revoked or expired", ErrBadCredential)
	}

	user, err := a.Users.Get(ctx, rec.OrgID, rec.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Principal{}, fmt.Errorf("%w: the token's user is gone", ErrBadCredential)
		}
		return Principal{}, fmt.Errorf("auth: loading the token's user: %w", err)
	}
	admin, allowed := a.Allowlist.Lookup(user.Email)
	if !allowed {
		return Principal{}, fmt.Errorf("%w: %s is no longer on the allowlist", ErrBadCredential, user.Email)
	}

	a.touch(ctx, rec, now)
	return Principal{UserID: user.ID, OrgID: user.OrgID, Email: user.Email, Admin: admin, TokenID: rec.ID}, nil
}

// touch records use, at most once a TouchInterval. A failure here is not
// the caller's problem: the request is authenticated either way.
//
// @decision(DL-006)
func (a *TokenAuthenticator) touch(ctx context.Context, rec store.APIToken, now time.Time) {
	if rec.LastUsedAt != nil && now.Sub(*rec.LastUsedAt) < TouchInterval {
		return
	}
	if err := a.Tokens.Touch(ctx, rec.OrgID, rec.ID, now); err != nil && a.Warn != nil {
		a.Warn("auth: recording token use: " + err.Error())
	}
}

func (a *TokenAuthenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now().UTC()
}
