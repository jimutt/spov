package git

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// TokenInfo is what a forge says about a stored token: whose it is, and
// on a forge that reports them, its scopes and expiry. The token itself
// is never in it.
//
// @decision(DL-041)
type TokenInfo struct {
	// User is the account the token authenticates as.
	User string
	// Scopes are the token's own scopes, when the forge reports them.
	Scopes []string
	// ExpiresAt is when the token stops working, zero when it does not
	// or the forge does not say.
	ExpiresAt time.Time
	// ServerVersion is the forge's own version string, when it gave one.
	ServerVersion string
}

// The three ways a credential check fails that are not a network
// problem, kept apart so the person adding a token is told which one
// they have.
//
// @decision(DL-041)
var (
	// ErrBadToken is a token the forge does not accept: wrong, revoked
	// or expired.
	ErrBadToken = errors.New("the forge does not accept this token; it may be mistyped, revoked or expired")
	// ErrMissingPermission is a token the forge accepts that lacks a
	// permission spov relies on.
	ErrMissingPermission = errors.New("the token lacks a permission it needs here")
	// ErrUnsupportedServer is a forge that does not have an endpoint or
	// version spov relies on.
	ErrUnsupportedServer = errors.New("this server lacks an endpoint or version these checks rely on")
)

// inspector is a platform that can describe its own token.
type inspector interface {
	Inspect(ctx context.Context) (TokenInfo, error)
}

// Inspect asks the platform about its token. A platform that cannot
// answer gives ErrUnsupported.
//
// @decision(DL-041)
func Inspect(ctx context.Context, p Platform) (TokenInfo, error) {
	i, ok := p.(inspector)
	if !ok {
		return TokenInfo{}, ErrUnsupported
	}
	return i.Inspect(ctx)
}

// statusError maps an HTTP status from a token check to the error a
// person can act on, or nil for a status that is not one of the three.
//
// @decision(DL-041)
func statusError(code int) error {
	switch code {
	case http.StatusUnauthorized:
		return ErrBadToken
	case http.StatusForbidden:
		return ErrMissingPermission
	case http.StatusNotFound:
		return ErrUnsupportedServer
	}
	return nil
}
