package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/store"
)

// Admin actions on the organisation's routed credentials, for the audit
// trail.
//
// @decision(DL-074)
const (
	AdminRoutedStored  = "routed_credential.stored"
	AdminRoutedRemoved = "routed_credential.removed"
)

// SealedOrgRoutedCredentials keeps the organisation's routed credentials
// in org_credentials, keyed route:<name>, the route in Meta and bound
// into the seal under the organisation's own AAD.
//
// @decision(DL-074)
type SealedOrgRoutedCredentials struct {
	Store  store.Store
	Sealer *auth.Sealer
}

func orgRouteAAD(orgID string, r routedcred.Route) []byte {
	return routedcred.AAD(auth.OrgCredentialAAD(orgID, routedcred.Key(r.Name)), r)
}

func orgRouteRows(ctx context.Context, st store.OrgCredentialStore, orgID string) ([]store.OrgCredential, error) {
	rows, err := st.List(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var out []store.OrgCredential
	for _, r := range rows {
		if routedcred.IsKey(r.Platform) {
			out = append(out, r)
		}
	}
	return out, nil
}

// List reads the routes and opens nothing.
func (c *SealedOrgRoutedCredentials) List(ctx context.Context, orgID string) ([]RoutedCredentialView, error) {
	rows, err := orgRouteRows(ctx, c.Store.OrgCredentials(), orgID)
	if err != nil {
		return nil, err
	}
	out := make([]RoutedCredentialView, 0, len(rows))
	for _, row := range rows {
		r, err := routedcred.Decode(row.Meta)
		if err != nil {
			return nil, err
		}
		out = append(out, RoutedCredentialView{Route: r, UpdatedAt: row.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Route.Name < out[j].Route.Name })
	return out, nil
}

// Open is every one with its secret. A row whose route was changed
// beside its ciphertext does not open, and that is an error.
func (c *SealedOrgRoutedCredentials) Open(ctx context.Context, orgID string) ([]HeldRoute, error) {
	rows, err := orgRouteRows(ctx, c.Store.OrgCredentials(), orgID)
	if err != nil {
		return nil, err
	}
	var out []HeldRoute
	for _, row := range rows {
		r, err := routedcred.Decode(row.Meta)
		if err != nil {
			return nil, err
		}
		plain, err := c.Sealer.Open(row.Ciphertext, row.Nonce, row.KeyVersion, orgRouteAAD(orgID, r))
		if err != nil {
			return nil, fmt.Errorf("the organisation's routed credential %s: %w", r.Name, err)
		}
		out = append(out, HeldRoute{Route: r, Secret: string(plain)})
	}
	return out, nil
}

// Put seals and stores one inside tx, refusing one past the limit.
func (c *SealedOrgRoutedCredentials) Put(ctx context.Context, tx store.Store, orgID, actor string, r routedcred.Route, secret string) error {
	r = r.Normal()
	if err := checkRoutedCredential(r, secret); err != nil {
		return err
	}
	rows, err := orgRouteRows(ctx, tx.OrgCredentials(), orgID)
	if err != nil {
		return err
	}
	replaces := false
	for _, row := range rows {
		replaces = replaces || row.Platform == routedcred.Key(r.Name)
	}
	if !replaces && len(rows) >= routedcred.MaxPerOrg {
		return fmt.Errorf("%w: the organisation already holds %d routed credentials, the most it may", errInvalidRoute, routedcred.MaxPerOrg)
	}
	ct, nonce, version, err := c.Sealer.Seal([]byte(secret), orgRouteAAD(orgID, r))
	if err != nil {
		return err
	}
	return tx.OrgCredentials().Put(ctx, store.OrgCredential{
		ID: store.NewID(time.Now()), OrgID: orgID, Platform: routedcred.Key(r.Name),
		Ciphertext: ct, Nonce: nonce, KeyVersion: version, Meta: r.Encode(), UpdatedBy: actor,
	})
}

type orgRoutedOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		RoutedCredentials []RoutedCredentialView `json:"routed_credentials"`
	}
}

// registerOrgRoutedCredentials serves /org/routed-credentials: every
// member reads them, an admin writes them.
//
// @decision(DL-074)
func (s *Server) registerOrgRoutedCredentials() {
	answer := func(ctx context.Context, orgID string) (*orgRoutedOutput, error) {
		views, err := s.deps.OrgRoutedCredentials.List(ctx, orgID)
		if err != nil {
			return nil, toHumaError(err)
		}
		out := &orgRoutedOutput{CacheControl: "no-store"}
		out.Body.RoutedCredentials = views
		if out.Body.RoutedCredentials == nil {
			out.Body.RoutedCredentials = []RoutedCredentialView{}
		}
		return out, nil
	}
	unavailable := func() error {
		if s.deps.OrgRoutedCredentials == nil {
			return huma.Error501NotImplemented("this server keeps no organisation routed credentials;" +
				" a local server has no organisation, and a hosted one needs SPOV_CREDENTIAL_KEY")
		}
		return nil
	}

	register(s.api, huma.Operation{
		OperationID: "list-org-routed-credentials", Method: http.MethodGet, Path: "/api/v1/org/routed-credentials",
		Summary: "The organisation's routed credentials, which every member's sessions get; never their secrets", Tags: []string{"credentials"},
		Errors: []int{http.StatusNotImplemented},
	}, func(ctx context.Context, _ *struct{}) (*orgRoutedOutput, error) {
		if err := unavailable(); err != nil {
			return nil, err
		}
		return answer(ctx, principal(ctx).OrgID)
	})

	register(s.api, huma.Operation{
		OperationID: "put-org-routed-credential", Method: http.MethodPut, Path: "/api/v1/org/routed-credentials/{name}",
		Summary: "Store one of the organisation's routed credentials", Tags: []string{"admin"},
		Errors: []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusNotImplemented},
	}, func(ctx context.Context, in *putRoutedCredentialInput) (*orgRoutedOutput, error) {
		if err := unavailable(); err != nil {
			return nil, err
		}
		p := principal(ctx)
		if err := s.requireAllowlist(p); err != nil {
			return nil, err
		}
		r := in.Body.Route
		if r.Name == "" {
			r.Name = in.Name
		}
		if r.Name != in.Name {
			return nil, huma.Error422UnprocessableEntity("the route names " + r.Name + ", not " + in.Name)
		}
		r = r.Normal()
		secret := strings.TrimSpace(in.Body.Secret)
		now := s.deps.Now()
		err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
			if err := liveAdminIn(ctx, tx, p); err != nil {
				return err
			}
			if err := s.deps.OrgRoutedCredentials.Put(ctx, tx, p.OrgID, actorOf(p), r, secret); err != nil {
				return err
			}
			return tx.AdminEvents().Append(ctx, store.AdminEvent{
				ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminRoutedStored, At: now,
				Detail: fmt.Sprintf("%s for %s", r.Name, strings.Join(r.Hosts, ", ")),
			})
		})
		if errors.Is(err, errInvalidRoute) {
			return nil, huma.Error422UnprocessableEntity(scrub(err.Error(), secret))
		}
		if err != nil {
			return nil, toHumaError(err)
		}
		return answer(ctx, p.OrgID)
	})

	register(s.api, huma.Operation{
		OperationID: "delete-org-routed-credential", Method: http.MethodDelete, Path: "/api/v1/org/routed-credentials/{name}",
		Summary: "Remove one of the organisation's routed credentials", Tags: []string{"admin"},
		Errors: []int{http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented},
	}, func(ctx context.Context, in *deleteRoutedCredentialInput) (*orgRoutedOutput, error) {
		if err := unavailable(); err != nil {
			return nil, err
		}
		p := principal(ctx)
		if err := s.requireAllowlist(p); err != nil {
			return nil, err
		}
		now := s.deps.Now()
		err := s.deps.Store.Tx(ctx, func(tx store.Store) error {
			if err := liveAdminIn(ctx, tx, p); err != nil {
				return err
			}
			if err := tx.OrgCredentials().Delete(ctx, p.OrgID, routedcred.Key(in.Name)); err != nil {
				return err
			}
			return tx.AdminEvents().Append(ctx, store.AdminEvent{
				ID: store.NewID(now), OrgID: p.OrgID, Actor: actorOf(p), Action: AdminRoutedRemoved, At: now,
				Detail: in.Name,
			})
		})
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("the organisation holds no routed credential named " + in.Name)
		}
		if err != nil {
			return nil, toHumaError(err)
		}
		return answer(ctx, p.OrgID)
	})
}
