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
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/store"
)

// RoutedCredentialView describes one routed credential: its route, never
// its secret.
//
// @decision(DL-073)
type RoutedCredentialView struct {
	Route routedcred.Route `json:"route"`
	// Missing is set when the route is known but its secret could not be
	// found, as when a local index names a credential the keyring lost.
	Missing   bool      `json:"missing,omitempty" doc:"The secret is gone; add the credential again"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// RoutedCredentialSource keeps one user's routed credentials: sealed in
// the database when hosted, in this machine's store and index locally.
//
// @decision(DL-073)
type RoutedCredentialSource interface {
	List(ctx context.Context, orgID, userID string) ([]RoutedCredentialView, error)
	Put(ctx context.Context, orgID, userID string, r routedcred.Route, secret string) error
	// Delete reports whether there was anything to remove.
	Delete(ctx context.Context, orgID, userID, name string) (bool, error)
	// Open is every routed credential the user holds, with its secret.
	// One that cannot be opened is an error, never a miss.
	Open(ctx context.Context, orgID, userID string) ([]HeldRoute, error)
}

// HeldRoute is a routed credential with its secret, in memory only while
// a launch resolves it.
//
// @decision(DL-073)
type HeldRoute struct {
	routedcred.Route
	Secret string
}

// Proxied is the route as the sidecar holds it.
func (h HeldRoute) Proxied() credproxy.Route { return h.WithSecret(h.Secret) }

// SealedRoutedCredentials is the hosted source: rows of user_credentials
// keyed route:<name>, the secret sealed under an AAD that binds the
// route beside it.
//
// @decision(DL-073)
type SealedRoutedCredentials struct {
	Store  store.CredentialStore
	Sealer *auth.Sealer
	Now    func() time.Time
}

func (c *SealedRoutedCredentials) aad(orgID, userID string, r routedcred.Route) []byte {
	return routedcred.AAD(auth.CredentialAAD(orgID, userID, routedcred.Key(r.Name)), r)
}

// List reads the routes and opens nothing.
func (c *SealedRoutedCredentials) List(ctx context.Context, orgID, userID string) ([]RoutedCredentialView, error) {
	recs, err := c.rows(ctx, orgID, userID)
	if err != nil {
		return nil, err
	}
	out := make([]RoutedCredentialView, 0, len(recs))
	for _, rec := range recs {
		r, err := routedcred.Decode(rec.Route)
		if err != nil {
			return nil, err
		}
		out = append(out, RoutedCredentialView{Route: r, UpdatedAt: rec.UpdatedAt})
	}
	return out, nil
}

func (c *SealedRoutedCredentials) rows(ctx context.Context, orgID, userID string) ([]store.UserCredential, error) {
	recs, err := c.Store.ListByUser(ctx, orgID, userID)
	if err != nil {
		return nil, err
	}
	var out []store.UserCredential
	for _, rec := range recs {
		if routedcred.IsKey(rec.Platform) {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Put seals the secret with its route and stores both, replacing the
// row of the same name.
func (c *SealedRoutedCredentials) Put(ctx context.Context, orgID, userID string, r routedcred.Route, secret string) error {
	if c.Sealer == nil {
		return auth.ErrNoKey
	}
	r = r.Normal()
	if err := checkRoutedCredential(r, secret); err != nil {
		return err
	}
	recs, err := c.rows(ctx, orgID, userID)
	if err != nil {
		return err
	}
	replaces := false
	for _, rec := range recs {
		replaces = replaces || rec.Platform == routedcred.Key(r.Name)
	}
	if !replaces && len(recs) >= routedcred.MaxPerUser {
		return fmt.Errorf("%w: you already hold %d routed credentials, the most a user may", errInvalidRoute, routedcred.MaxPerUser)
	}
	ct, nonce, version, err := c.Sealer.Seal([]byte(secret), c.aad(orgID, userID, r))
	if err != nil {
		return err
	}
	now := c.now()
	return c.Store.Put(ctx, store.UserCredential{
		ID: store.NewID(now), OrgID: orgID, UserID: userID, Platform: routedcred.Key(r.Name),
		Ciphertext: ct, Nonce: nonce, KeyVersion: version, Route: r.Encode(),
		CreatedAt: now, UpdatedAt: now,
	})
}

// Delete removes one routed credential.
func (c *SealedRoutedCredentials) Delete(ctx context.Context, orgID, userID, name string) (bool, error) {
	err := c.Store.Delete(ctx, orgID, userID, routedcred.Key(name))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Open is every routed credential with its secret. A row whose route
// was changed beside its ciphertext does not open, and that is an error.
func (c *SealedRoutedCredentials) Open(ctx context.Context, orgID, userID string) ([]HeldRoute, error) {
	if c.Sealer == nil {
		return nil, nil
	}
	recs, err := c.rows(ctx, orgID, userID)
	if err != nil {
		return nil, err
	}
	var out []HeldRoute
	for _, rec := range recs {
		r, err := routedcred.Decode(rec.Route)
		if err != nil {
			return nil, err
		}
		plain, err := c.Sealer.Open(rec.Ciphertext, rec.Nonce, rec.KeyVersion, c.aad(orgID, userID, r))
		if err != nil {
			return nil, fmt.Errorf("routed credential %s: %w", r.Name, err)
		}
		out = append(out, HeldRoute{Route: r, Secret: string(plain)})
	}
	return out, nil
}

func (c *SealedRoutedCredentials) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

// LocalRoutedCredentials is a local server's source: its one user's
// routed credentials, in this machine's store and the index beside the
// state file.
//
// @decision(DL-073)
type LocalRoutedCredentials struct{ Local *routedcred.Local }

// List reads the index, and says which secrets the store has lost.
func (l LocalRoutedCredentials) List(context.Context, string, string) ([]RoutedCredentialView, error) {
	held, err := l.Local.List()
	if err != nil {
		return nil, err
	}
	out := make([]RoutedCredentialView, 0, len(held))
	for _, h := range held {
		out = append(out, RoutedCredentialView{Route: h.Route, Missing: h.Missing})
	}
	return out, nil
}

// Put stores the secret in this machine's store and the route in the
// index.
func (l LocalRoutedCredentials) Put(_ context.Context, _, _ string, r routedcred.Route, secret string) error {
	r = r.Normal()
	if err := checkRoutedCredential(r, secret); err != nil {
		return err
	}
	if err := l.Local.Put(r, secret); err != nil {
		if errors.Is(err, routedcred.ErrFull) {
			return fmt.Errorf("%w: %w", errInvalidRoute, err)
		}
		return err
	}
	return nil
}

// Delete removes one.
func (l LocalRoutedCredentials) Delete(_ context.Context, _, _, name string) (bool, error) {
	return l.Local.Delete(name)
}

// Open reads each secret. One the index names and the store lacks is
// left out; the listing says so.
func (l LocalRoutedCredentials) Open(context.Context, string, string) ([]HeldRoute, error) {
	held, err := l.Local.List()
	if err != nil {
		return nil, err
	}
	var out []HeldRoute
	for _, h := range held {
		if h.Missing {
			continue
		}
		secret, err := l.Local.Secret(h.Name)
		if err != nil {
			return nil, fmt.Errorf("routed credential %s: %w", h.Name, err)
		}
		out = append(out, HeldRoute{Route: h.Route, Secret: secret})
	}
	return out, nil
}

// errInvalidRoute marks a refusal the caller can fix, answered 422.
var errInvalidRoute = errors.New("invalid routed credential")

// checkRoutedCredential checks a route and its secret before either is
// stored.
//
// @decision(DL-072) @decision(DL-073)
func checkRoutedCredential(r routedcred.Route, secret string) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: %w", errInvalidRoute, err)
	}
	if err := r.WithSecret(secret).Validate(); err != nil {
		return fmt.Errorf("%w: %s", errInvalidRoute, strings.TrimPrefix(err.Error(), "credproxy: "))
	}
	return nil
}

type routedCredentialsOutput struct {
	Body struct {
		RoutedCredentials []RoutedCredentialView `json:"routed_credentials"`
	}
}

type putRoutedCredentialInput struct {
	Name string `path:"name"`
	Body struct {
		// Secret is never returned by any endpoint.
		Secret string           `json:"secret" required:"true" doc:"The credential to store"`
		Route  routedcred.Route `json:"route" required:"true"`
	}
}

type putRoutedCredentialOutput struct {
	Body RoutedCredentialView
}

type deleteRoutedCredentialInput struct {
	Name string `path:"name"`
}

// registerRoutedCredentials serves /me/routed-credentials, scoped to the
// calling principal as /me/credentials is.
//
// @decision(DL-073)
func (s *Server) registerRoutedCredentials() {
	register(s.api, huma.Operation{
		OperationID: "list-routed-credentials", Method: http.MethodGet, Path: "/api/v1/me/routed-credentials",
		Summary: "The caller's routed credentials, without their secrets", Tags: []string{"credentials"},
	}, func(ctx context.Context, _ *struct{}) (*routedCredentialsOutput, error) {
		src, err := s.routedCredentials()
		if err != nil {
			return nil, err
		}
		p := principal(ctx)
		views, err := src.List(ctx, p.OrgID, p.UserID)
		if err != nil {
			return nil, toHumaError(err)
		}
		sort.Slice(views, func(i, j int) bool { return views[i].Route.Name < views[j].Route.Name })
		out := &routedCredentialsOutput{}
		out.Body.RoutedCredentials = views
		if out.Body.RoutedCredentials == nil {
			out.Body.RoutedCredentials = []RoutedCredentialView{}
		}
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "put-routed-credential", Method: http.MethodPut, Path: "/api/v1/me/routed-credentials/{name}",
		Summary: "Store a routed credential, replacing its route and secret together", Tags: []string{"credentials"},
	}, func(ctx context.Context, in *putRoutedCredentialInput) (*putRoutedCredentialOutput, error) {
		src, err := s.routedCredentials()
		if err != nil {
			return nil, err
		}
		r := in.Body.Route
		if r.Name == "" {
			r.Name = in.Name
		}
		if r.Name != in.Name {
			return nil, huma.Error422UnprocessableEntity("the route names " + r.Name + ", not " + in.Name)
		}
		p := principal(ctx)
		secret := strings.TrimSpace(in.Body.Secret)
		if err := src.Put(ctx, p.OrgID, p.UserID, r, secret); err != nil {
			if errors.Is(err, errInvalidRoute) {
				return nil, huma.Error422UnprocessableEntity(scrub(err.Error(), secret))
			}
			return nil, toHumaError(err)
		}
		return &putRoutedCredentialOutput{Body: RoutedCredentialView{Route: r.Normal(), UpdatedAt: s.deps.Now()}}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "delete-routed-credential", Method: http.MethodDelete, Path: "/api/v1/me/routed-credentials/{name}",
		Summary: "Remove a routed credential", Tags: []string{"credentials"},
	}, func(ctx context.Context, in *deleteRoutedCredentialInput) (*struct{}, error) {
		src, err := s.routedCredentials()
		if err != nil {
			return nil, err
		}
		p := principal(ctx)
		// Removing one that is not there is what was asked for, as for
		// /me/credentials.
		//
		// @decision(DL-008)
		if _, err := src.Delete(ctx, p.OrgID, p.UserID, in.Name); err != nil {
			return nil, toHumaError(err)
		}
		return nil, nil
	})
}

// routedCredentials is the deployment's source, or a 501 when it can hold
// none.
//
// @decision(DL-073)
func (s *Server) routedCredentials() (RoutedCredentialSource, error) {
	if s.deps.RoutedCredentials == nil {
		return nil, huma.Error501NotImplemented("this server stores no routed credentials:" +
			" it has no SPOV_CREDENTIAL_KEY configured")
	}
	return s.deps.RoutedCredentials, nil
}
