package server

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/prep"
)

// ModelProviderView is one provider and the models spov knows it
// serves. The caller's key is never in it — only whether one exists.
//
// @decision(DL-013) @decision(DL-042)
type ModelProviderView struct {
	// Provider is OpenCode's own provider identifier.
	Provider string `json:"provider"`
	Name     string `json:"name"`
	// KeyEnv is the variable the key lives in, so a caller told they
	// have no key is also told what to set.
	KeyEnv string `json:"key_env"`
	// HaveKey reports whether a launch by THIS caller would carry a key
	// for this provider, from the deployment's own environment or the
	// caller's stored credentials.
	HaveKey bool `json:"have_key"`
	// SignIn names a sign-in that can supply the key in place of a stored
	// one: "opencode" for the OpenCode Console. It is empty when this
	// server cannot give a sign-in to sessions.
	SignIn string `json:"sign_in,omitempty" enum:"opencode"`
	// Models are `provider/model` references, newest first, ready to be
	// sent as a session's or job's model field.
	Models []string `json:"models"`
	// Beta are the references among Models that models.dev marks beta.
	//
	// @decision(DL-013)
	Beta []string `json:"beta,omitempty"`
}

// CatalogueView says where the model list came from.
//
// @decision(DL-013)
type CatalogueView struct {
	// Source is "live" for a list fetched from models.dev, "snapshot"
	// for the one shipped in spov.
	Source string `json:"source" enum:"live,snapshot"`
	// AsOf is when the list was fetched, or the snapshot made.
	AsOf time.Time `json:"as_of"`
}

type modelsOutput struct {
	Body struct {
		Providers []ModelProviderView `json:"providers"`
		Catalogue CatalogueView       `json:"catalogue"`
	}
}

// registerModels serves /models: what a caller can pick from, and which
// of it they can actually use. Every provider is listed, including ones
// the caller holds no key for.
//
// @decision(DL-013)
func (s *Server) registerModels() {
	register(s.api, huma.Operation{
		OperationID: "list-models", Method: http.MethodGet, Path: "/api/v1/models",
		Summary: "Models the caller can run a session or job on", Tags: []string{"models"},
	}, func(ctx context.Context, _ *struct{}) (*modelsOutput, error) {
		p := principal(ctx)
		held, err := s.svc.heldProviders(ctx, p.OrgID, p.UserID)
		if err != nil {
			return nil, toHumaError(err)
		}
		list, live := s.deps.Models.Models()
		out := &modelsOutput{}
		out.Body.Providers = []ModelProviderView{}
		// @decision(DL-013)
		out.Body.Catalogue = CatalogueView{Source: "snapshot", AsOf: list.AsOf}
		if live {
			out.Body.Catalogue.Source = "live"
		}
		for _, provider := range prep.Catalogue() {
			models := list.Providers[provider.ID]
			refs := make([]string, 0, len(models))
			var beta []string
			for _, m := range models {
				refs = append(refs, provider.Ref(m.ID))
				if m.Beta {
					beta = append(beta, provider.Ref(m.ID))
				}
			}
			out.Body.Providers = append(out.Body.Providers, ModelProviderView{
				Provider: provider.ID,
				Name:     provider.Name,
				KeyEnv:   provider.KeyEnv,
				HaveKey:  held[provider.KeyEnv],
				SignIn:   s.svc.signInFor(provider.KeyEnv),
				Models:   refs,
				Beta:     beta,
			})
		}
		return out, nil
	})
}

// heldProviders reports which provider variables a launch by this
// caller would carry a key for. It is on Service rather than Server so
// DL-013's check and this listing answer from the same function. It
// asks providerKeysFor, the same resolution a launch makes, and reduces
// it to presence immediately: the values are secrets and this is a read
// path serving a browser.
//
// @decision(DL-013) @decision(DL-008)
func (s *Service) heldProviders(ctx context.Context, orgID, userID string) (map[string]bool, error) {
	keys, err := s.storedProviderKeys(ctx, orgID, userID)
	if err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(keys)+1)
	for name, value := range keys {
		held[name] = value != ""
	}
	// A sign-in whose token has lapsed still counts: the launch
	// refreshes it.
	//
	// @decision(DL-015)
	if s.consoleStandingFor(ctx, orgID, userID) == consoleHeld {
		held[prep.OpenCodeKeyEnv] = true
	}
	return held, nil
}

// signInFor is the sign-in that can supply a provider's key, or "".
//
// @decision(DL-015)
func (s *Service) signInFor(keyEnv string) string {
	if keyEnv == prep.OpenCodeKeyEnv && s.consoleCarried() {
		return "opencode"
	}
	return ""
}
