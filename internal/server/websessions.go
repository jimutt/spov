package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"sort"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
)

// WebSessionView is one browser login, as the API shows it. The token
// itself never appears: the ID is a prefix, enough to tell two sessions
// apart and useless as a credential.
//
// @decision(DL-007)
type WebSessionView struct {
	ID      string    `json:"id" doc:"An opaque handle for this login, not its cookie"`
	Current bool      `json:"current" doc:"Whether this is the session making the request"`
	Expires time.Time `json:"expires_at"`
}

type webSessionsOutput struct {
	Body struct {
		Sessions []WebSessionView `json:"sessions"`
	}
}

// tokenID is the handle the API exposes for a browser login: the first 12
// characters of its key in the session store, which identify a row and
// are useless as a credential. It takes a store key, never a cookie's
// value; see storeKey.
//
// @decision(DL-007) @decision(DL-127)
func tokenID(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12]
}

// storeKey is the key the session store holds for a cookie's token:
// scs's own hash of it when the manager hashes tokens, which a hosted
// server always does. Inside Iterate, Sessions.Token is already this key.
//
// @decision(DL-007) @decision(DL-127)
func (l *Login) storeKey(token string) string {
	if token == "" || !l.Sessions.HashTokenInStore {
		return token
	}
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// destroyIterated deletes the session Iterate is on, by its store key.
// Sessions.Destroy would hash that key a second time and delete nothing.
//
// @decision(DL-007) @decision(DL-127)
func (l *Login) destroyIterated(ctx context.Context) error {
	key := l.Sessions.Token(ctx)
	if c, ok := l.Sessions.Store.(interface {
		DeleteCtx(context.Context, string) error
	}); ok {
		return c.DeleteCtx(ctx, key)
	}
	return l.Sessions.Store.Delete(key)
}

// registerWebSessions serves a user's browser logins. It exists only in
// hosted mode: local mode has no cookie.
//
// @decision(DL-007)
func (s *Server) registerWebSessions() {
	register(s.api, huma.Operation{
		OperationID: "list-web-sessions", Method: http.MethodGet, Path: "/api/v1/me/sessions",
		Summary: "The caller's browser logins", Tags: []string{"auth"},
	}, func(ctx context.Context, _ *struct{}) (*webSessionsOutput, error) {
		p := principal(ctx)
		found, err := s.deps.Login.sessionsOf(ctx, p.UserID)
		if err != nil {
			return nil, toHumaError(err)
		}
		current := tokenID(s.deps.Login.storeKey(s.deps.Login.Sessions.Token(ctx)))
		out := &webSessionsOutput{}
		out.Body.Sessions = make([]WebSessionView, 0, len(found))
		for _, f := range found {
			out.Body.Sessions = append(out.Body.Sessions, WebSessionView{
				ID: f.id, Current: f.id == current, Expires: f.expires,
			})
		}
		sort.Slice(out.Body.Sessions, func(i, j int) bool {
			return out.Body.Sessions[i].Expires.Before(out.Body.Sessions[j].Expires)
		})
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "revoke-web-session", Method: http.MethodDelete, Path: "/api/v1/me/sessions/{id}",
		Summary: "Revoke a browser login", Tags: []string{"auth"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	},
	) (*struct{}, error) {
		p := principal(ctx)
		found, err := s.deps.Login.revoke(ctx, p.UserID, in.ID)
		if err != nil {
			return nil, toHumaError(err)
		}
		if !found {
			// An unguessable handle that is not this user's is reported
			// as absent rather than refused: "not yours" would confirm
			// it exists.
			return nil, huma.Error404NotFound("no such session")
		}
		return nil, nil
	})
}

// webSession is one row of the session store, reduced to what the API
// shows.
type webSession struct {
	id      string
	expires time.Time
}

// sessionsOf lists a user's live browser logins.
//
// @decision(DL-007)
func (l *Login) sessionsOf(ctx context.Context, userID string) ([]webSession, error) {
	var found []webSession
	err := l.Sessions.Iterate(ctx, func(ctx context.Context) error {
		if l.Sessions.GetString(ctx, auth.SessionKeyUserID) != userID {
			return nil
		}
		found = append(found, webSession{
			id:      tokenID(l.Sessions.Token(ctx)),
			expires: l.Sessions.Deadline(ctx),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// revoke destroys one of a user's browser logins and reports whether it
// found one. A session belonging to someone else is not found, so the
// caller cannot tell it apart from one that does not exist.
//
// @decision(DL-007)
func (l *Login) revoke(ctx context.Context, userID, id string) (bool, error) {
	revoked := false
	err := l.Sessions.Iterate(ctx, func(ctx context.Context) error {
		if revoked || tokenID(l.Sessions.Token(ctx)) != id {
			return nil
		}
		if l.Sessions.GetString(ctx, auth.SessionKeyUserID) != userID {
			return nil
		}
		revoked = true
		return l.destroyIterated(ctx)
	})
	if err != nil {
		return false, err
	}
	return revoked, nil
}
