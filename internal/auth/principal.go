package auth

import (
	"context"
	"errors"
)

// Principal is who a request is acting as. Handlers read it from the
// request context and never see the credential that produced it.
//
// @decision(DL-006)
type Principal struct {
	// UserID and OrgID are ULIDs, or LocalID in local mode.
	UserID string
	OrgID  string
	Email  string
	// GitHubID is the GitHub account the user row is bound to, zero for
	// a row not yet bound and for every principal outside the login.
	//
	// @decision(DL-128)
	GitHubID int64
	Admin    bool
	// TokenID names the stored CLI token this request authenticated
	// with, so a handler can resolve "the token I am using" — what
	// `spov logout` revokes. It is a row id, never a credential, and is
	// empty for a session cookie.
	//
	// @decision(DL-006)
	TokenID string
}

// LocalID is the user and org of the single principal in local mode.
//
// @decision(DL-006)
const LocalID = "local"

// Local is the principal every local-mode request resolves to.
//
// @decision(DL-006)
func Local() Principal {
	return Principal{UserID: LocalID, OrgID: LocalID, Admin: true}
}

// RecorderID is the user the server's recorder acts as when it reads a
// sandbox. It is a name for the audit row, not an account.
//
// @decision(DL-017)
const RecorderID = "recorder"

// Recorder is the principal the recorder connects to a session with. It
// is an admin of the session's org so that Get resolves any owner's
// session, and it carries no credential.
//
// @decision(DL-017)
func Recorder(orgID string) Principal {
	return Principal{UserID: RecorderID, OrgID: orgID, Admin: true}
}

// CanAccess reports whether the principal may see and control a resource
// owned by ownerID: its owner may, and so may an admin.
//
// @decision(DL-006)
func (p Principal) CanAccess(ownerID string) bool {
	return p.Admin || (p.UserID != "" && p.UserID == ownerID)
}

// ErrUnauthenticated means the request carried no usable credential.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

type ctxKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// From returns the principal the middleware stored, and whether there is
// one. A handler behind the middleware always finds one.
//
// @decision(DL-006)
func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
