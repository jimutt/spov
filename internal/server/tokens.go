package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// TokenView is an issued CLI token as the API shows it. The token
// itself was shown once, in the terminal that asked for it, and is
// never recoverable from here — only its hash is stored.
//
// @decision(DL-006)
type TokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	// Current marks the token that authenticated this request, when one
	// did; a browser session sees no current token.
	Current bool `json:"current"`
}

type tokensOutput struct {
	Body struct {
		Tokens []TokenView `json:"tokens"`
	}
}

// CurrentToken is the id a caller may use in place of a token's own, to
// mean "the one I am authenticating with".
//
// @decision(DL-006)
const CurrentToken = "current"

// registerTokens serves a user's CLI tokens.
//
// @decision(DL-006)
func (s *Server) registerTokens() {
	register(s.api, huma.Operation{
		OperationID: "list-tokens", Method: http.MethodGet, Path: "/api/v1/me/tokens",
		Summary: "The caller's CLI tokens", Tags: []string{"auth"},
	}, func(ctx context.Context, _ *struct{}) (*tokensOutput, error) {
		p := principal(ctx)
		found, err := s.deps.Store.Tokens().ListByUser(ctx, p.OrgID, p.UserID)
		if err != nil {
			return nil, toHumaError(err)
		}
		current := p.TokenID
		out := &tokensOutput{}
		out.Body.Tokens = make([]TokenView, 0, len(found))
		for _, t := range found {
			// A revoked token is gone as far as its owner is concerned;
			// the row is kept so an expiry sweep can reap it.
			if t.RevokedAt != nil {
				continue
			}
			out.Body.Tokens = append(out.Body.Tokens, TokenView{
				ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt,
				LastUsedAt: t.LastUsedAt, ExpiresAt: t.ExpiresAt,
				Current: t.ID == current,
			})
		}
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "revoke-token", Method: http.MethodDelete, Path: "/api/v1/me/tokens/{id}",
		Summary:       "Revoke a CLI token",
		Description:   "Pass `current` to revoke the token authenticating this request, which is what `spov logout` does.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	},
	) (*struct{}, error) {
		p := principal(ctx)
		id := in.ID
		if id == CurrentToken {
			if id = p.TokenID; id == "" {
				return nil, huma.Error404NotFound("this request is not authenticated with a CLI token")
			}
		}
		// Scoped to the caller's own org and user: Revoke takes the org,
		// and the ownership check below covers the user.
		rec, err := s.deps.Store.Tokens().ByID(ctx, p.OrgID, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, huma.Error404NotFound("no such token")
			}
			return nil, toHumaError(err)
		}
		// Someone else's token is reported as absent rather than
		// refused, so the id cannot be probed.
		if rec.UserID != p.UserID {
			return nil, huma.Error404NotFound("no such token")
		}
		// A service account's own revoke is audited as an admin's is,
		// with the account as actor.
		//
		// @decision(DL-141)
		if p.Service {
			return nil, s.revokeOwnServiceToken(ctx, p, rec)
		}
		if err := s.deps.Store.Tokens().Revoke(ctx, p.OrgID, id, s.deps.Now()); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, huma.Error404NotFound("no such token")
			}
			return nil, toHumaError(err)
		}
		return nil, nil
	})
}

// revokeOwnServiceToken revokes a service account's own token and
// writes its audit row in one transaction. A token already revoked is
// revoked again with no second row.
//
// @decision(DL-141)
func (s *Server) revokeOwnServiceToken(ctx context.Context, p auth.Principal, rec store.APIToken) error {
	now := s.deps.Now()
	err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Tokens().Revoke(ctx, p.OrgID, rec.ID, now); err != nil {
			return err
		}
		if rec.RevokedAt != nil {
			return nil
		}
		name := strings.TrimSuffix(p.Email, "@"+auth.ServiceAccountDomain)
		if u, err := tx.Users().Get(ctx, p.OrgID, p.UserID); err == nil && u.Name != "" {
			name = u.Name
		}
		return tx.AdminEvents().Append(ctx, store.AdminEvent{
			ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminServiceTokenRevoked, At: now,
			Detail: "token " + rec.ID + " (" + rec.Name + ") of service account " + name + ", by the account itself",
		})
	})
	if errors.Is(err, store.ErrNotFound) {
		return huma.Error404NotFound("no such token")
	}
	return toHumaError(err)
}
