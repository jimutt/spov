package auth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// Authenticator turns a request into a principal.
//
// It returns ErrUnauthenticated when the request carries no usable
// credential, and another error for a credential that is present but
// wrong. Callers log the second kind, so its text must never contain the
// credential; neither kind is echoed to the client.
//
// @decision(DL-006)
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// ErrBadCredential is a credential that was presented and rejected.
var ErrBadCredential = errors.New("auth: bad credential")

// LocalAuthenticator accepts exactly one bearer token — the one the local
// server wrote to its state file — and yields the local principal.
//
// @decision(DL-006)
type LocalAuthenticator struct {
	token []byte
}

// NewLocal builds an authenticator for token. An empty token is refused,
// since it would accept an empty Authorization header.
func NewLocal(token string) (*LocalAuthenticator, error) {
	if token == "" {
		return nil, errors.New("auth: local token is empty")
	}
	return &LocalAuthenticator{token: []byte(token)}, nil
}

// Authenticate implements Authenticator.
func (a *LocalAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	presented, ok := bearer(r)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	if subtle.ConstantTimeCompare([]byte(presented), a.token) != 1 {
		return Principal{}, ErrBadCredential
	}
	return Local(), nil
}

// bearer extracts a Bearer token. Any other Authorization scheme is not a
// credential this package knows, so it counts as absent rather than wrong.
// More than one Authorization header is refused outright: a front proxy
// and this server could otherwise read different ones.
func bearer(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	fields := strings.Fields(values[0])
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return "", false
	}
	return fields[1], true
}

// StaticAuthenticator yields a fixed principal for every request. It is
// for tests of things behind the middleware.
//
// @decision(DL-006)
type StaticAuthenticator struct {
	Principal Principal
	// Err, when set, is returned instead.
	Err error
}

// Authenticate implements Authenticator.
func (a StaticAuthenticator) Authenticate(*http.Request) (Principal, error) {
	if a.Err != nil {
		return Principal{}, a.Err
	}
	return a.Principal, nil
}
