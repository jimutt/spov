package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// CredentialView describes one stored credential. The credential itself
// is never in it: the API can say a token exists and whom it proved to
// belong to, and nothing more.
//
// @decision(DL-008)
type CredentialView struct {
	Platform string `json:"platform"`
	Present  bool   `json:"present"`
	// VerifiedAs is the account the token proved it owns when stored.
	VerifiedAs string `json:"verified_as,omitempty"`
	// Scopes and ExpiresAt are what the forge said about the token when
	// it was stored. spov never renews or narrows a stored token.
	//
	// @decision(DL-041)
	Scopes    []string  `json:"scopes,omitempty" doc:"The token's scopes, when its forge reports them"`
	ExpiresAt time.Time `json:"expires_at,omitzero" doc:"When the token stops working, when its forge says"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

type credentialsOutput struct {
	Body struct {
		Credentials []CredentialView `json:"credentials"`
		// Enabled is false when the server has no credential key, so it
		// can hold nothing whoever is asking.
		//
		// @decision(DL-008)
		Enabled bool `json:"enabled"`
	}
}

type putCredentialInput struct {
	Platform string `path:"platform"`
	Body     struct {
		// Token is the credential. It is verified against the platform
		// before it is stored, and never returned by any endpoint.
		Token string `json:"token" required:"true" doc:"The credential to store"`
	}
}

type putCredentialOutput struct {
	Body CredentialView
}

type deleteCredentialInput struct {
	Platform string `path:"platform"`
}

// CredentialVerifier checks a credential against its platform before it
// is stored, and reports the account it belongs to. A credential that
// cannot be verified is refused: storing one that does not work turns a
// typo into a launch failure much later.
//
// @decision(DL-008) @decision(DL-041)
type CredentialVerifier interface {
	Verify(ctx context.Context, platform, secret string) (Verified, error)
}

// Verified is what a check learned about a credential: the account it
// belongs to and, where the forge says, its scopes and expiry.
//
// @decision(DL-041)
type Verified struct {
	As        string
	Scopes    []string
	ExpiresAt time.Time
}

// registerCredentials serves /me/credentials. Every operation is scoped
// to the calling principal: there is no path by which one user names
// another's credential.
//
// @decision(DL-008)
func (s *Server) registerCredentials() {
	register(s.api, huma.Operation{
		OperationID: "list-credentials", Method: http.MethodGet, Path: "/api/v1/me/credentials",
		Summary: "The caller's stored platform credentials", Tags: []string{"credentials"},
	}, func(ctx context.Context, _ *struct{}) (*credentialsOutput, error) {
		p := principal(ctx)
		out := &credentialsOutput{}
		// A server with no key holds nothing by construction, which is
		// not the same as a user who has stored nothing. Saying so
		// stops "no credentials" being read as "mine are missing".
		//
		// @decision(DL-008)
		out.Body.Enabled = s.credentialsEnabled() == nil
		views, err := s.deps.Credentials.List(ctx, p.OrgID, p.UserID)
		if err != nil {
			return nil, toHumaError(err)
		}
		out.Body.Credentials = views
		if out.Body.Credentials == nil {
			out.Body.Credentials = []CredentialView{}
		}
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "put-credential", Method: http.MethodPut, Path: "/api/v1/me/credentials/{platform}",
		Summary: "Store a credential for one platform", Tags: []string{"credentials"},
	}, func(ctx context.Context, in *putCredentialInput) (*putCredentialOutput, error) {
		p := principal(ctx)
		platform, err := CanonicalPlatform(in.Platform)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		secret := strings.TrimSpace(in.Body.Token)
		if secret == "" {
			return nil, huma.Error422UnprocessableEntity("the token is empty")
		}
		if err := s.credentialsEnabled(); err != nil {
			return nil, err
		}
		// Verified before it is stored, so a token that cannot work is
		// refused where the user can still fix it.
		//
		// @decision(DL-008)
		verified, err := s.verifyCredential(ctx, platform, secret)
		if err != nil {
			return nil, err
		}
		if err := s.deps.Credentials.Put(ctx, p.OrgID, p.UserID, platform, secret, verified); err != nil {
			return nil, toHumaError(err)
		}
		// What the user reaches has changed.
		//
		// @decision(DL-134)
		s.svc.ForgetRepoListings(p.OrgID, p.UserID)
		return &putCredentialOutput{Body: CredentialView{
			Platform:   platform,
			Present:    true,
			VerifiedAs: verified.As,
			Scopes:     verified.Scopes,
			ExpiresAt:  verified.ExpiresAt,
			UpdatedAt:  s.deps.Now(),
		}}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "delete-credential", Method: http.MethodDelete, Path: "/api/v1/me/credentials/{platform}",
		Summary: "Remove a stored credential", Tags: []string{"credentials"},
	}, func(ctx context.Context, in *deleteCredentialInput) (*struct{}, error) {
		p := principal(ctx)
		// Canonicalised, so a differently-spelled key removes the row
		// it names rather than reporting success and leaving a live
		// credential behind.
		//
		// @decision(DL-008)
		platform, err := CanonicalPlatform(in.Platform)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if err := s.credentialsEnabled(); err != nil {
			return nil, err
		}
		// Removing a credential that is not there is the outcome the
		// caller asked for, so it succeeds rather than 404s — and the
		// shared error mapper would call it "no such session".
		//
		// @decision(DL-008)
		err = s.deps.Credentials.Delete(ctx, p.OrgID, p.UserID, platform)
		// @decision(DL-134)
		s.svc.ForgetRepoListings(p.OrgID, p.UserID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, toHumaError(err)
		}
		return nil, nil
	})
}

// credentialsEnabled reports whether this deployment can store user
// credentials at all. Without a key the answer is a clear 501 rather
// than an encryption failure deep in the store.
//
// @decision(DL-008)
func (s *Server) credentialsEnabled() error {
	if s.deps.Credentials == nil || s.deps.Credentials.Sealer == nil {
		return huma.Error501NotImplemented("this server stores no user credentials:" +
			" it has no SPOV_CREDENTIAL_KEY configured")
	}
	return nil
}

// verifyCredential checks a token against its platform. The platform's
// own rejection message is scrubbed of the submitted credential before
// it goes anywhere.
//
// @decision(DL-008)
func (s *Server) verifyCredential(ctx context.Context, platform, secret string) (Verified, error) {
	if s.deps.Verifier == nil {
		return Verified{}, nil
	}
	verified, err := s.deps.Verifier.Verify(ctx, platform, secret)
	if err != nil {
		if errors.Is(err, ErrVerifyUnsupported) {
			return Verified{}, nil
		}
		return Verified{}, huma.Error422UnprocessableEntity(
			fmt.Sprintf("%s rejected the credential: %s", platform, scrub(err.Error(), secret)))
	}
	return verified, nil
}

// scrub removes a credential from text on its way out of the server.
// It mirrors the CLI's redactSecret: the same hazard, on the side that
// actually holds the secret.
//
// @decision(DL-008)
func scrub(text, secret string) string {
	// A short "secret" is not one, and replacing it would rewrite
	// unrelated words in the message.
	if len(secret) >= minRedactableSecret {
		text = strings.ReplaceAll(text, secret, "***")
	}
	return redact.Text(text)
}

// minRedactableSecret is the length below which a value is not treated
// as a credential worth scrubbing.
const minRedactableSecret = 8

// ErrVerifyUnsupported is a platform with nothing to verify against. A
// provider key has no cheap identity endpoint spov could call, so it is
// stored on the user's word.
//
// @decision(DL-008)
var ErrVerifyUnsupported = errors.New("verification is not supported for this platform")
