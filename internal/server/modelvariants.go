package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// A model's reasoning modes are OpenCode's variants of it, read by the
// recorder from the running sandbox once per run and served from the
// store.
//
// @decision(DL-159) @decision(DL-160)

// MaxModelListBytes bounds the model list read from a sandbox, and
// MaxModelVariantsBytes what is kept of it, as JSON.
//
// @decision(DL-159)
const (
	MaxModelListBytes     = 4 << 20
	MaxModelVariantsBytes = 256 << 10
)

// modelListPath is OpenCode's model list: every model the sandbox can
// run, each with its variants.
//
// @decision(DL-159)
const modelListPath = "/api/model"

// readModelVariants keeps what the sandbox says its models offer, once
// per run: nothing is read when the store holds the list of this
// sandbox, or when this process already tried it. A failure is logged
// and the capture goes on without it.
//
// @decision(DL-159)
func (r *Recorder) readModelVariants(ctx context.Context, c *capture) {
	sess := c.sess
	kept, err := r.store.Sessions().ModelVariants(ctx, sess.OrgID, sess.ID)
	if err != nil {
		r.warn(fmt.Sprintf("recorder %s: the reasoning modes were not read: read the store: %s", sess.ID, redact.Error(err)))
		return
	}
	if kept.SandboxID == sess.SandboxID || !r.firstVariantsRead(sess) {
		return
	}
	body, err := r.fetch(ctx, c.conn, modelListPath)
	if err != nil {
		r.warn(fmt.Sprintf("recorder %s: the reasoning modes were not read: %s", sess.ID, redact.Error(err)))
		return
	}
	models, err := parseModelVariants(body)
	if err != nil {
		r.warn(fmt.Sprintf("recorder %s: the reasoning modes were not read: %s", sess.ID, err))
		return
	}
	if err := r.store.Sessions().SetModelVariants(ctx, sess.OrgID, sess.ID, sess.SandboxID, models); err != nil {
		r.warn(fmt.Sprintf("recorder %s: the reasoning modes were not kept: %s", sess.ID, redact.Error(err)))
	}
}

// firstVariantsRead reports whether this process has not yet tried to
// read the session's sandbox's model list, and marks it tried.
//
// @decision(DL-159)
func (r *Recorder) firstVariantsRead(sess store.Session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.variantsTried[sess.ID] == sess.SandboxID {
		return false
	}
	r.variantsTried[sess.ID] = sess.SandboxID
	return true
}

// parseModelVariants reads OpenCode's model list into each model's
// variant ids, in OpenCode's order, keyed provider/model. A model with
// none, an id that is not one, a variant named default and a repeat are
// left out; a body over MaxModelListBytes, one that is not a model list
// and a result over MaxModelVariantsBytes are errors.
//
// @decision(DL-159)
func parseModelVariants(body []byte) (map[string][]string, error) {
	if len(body) > MaxModelListBytes {
		return nil, fmt.Errorf("the model list is over %d bytes", MaxModelListBytes)
	}
	var list struct {
		Data *[]struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
			Variants   []struct {
				ID string `json:"id"`
			} `json:"variants"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Data == nil {
		return nil, errors.New("the sandbox's answer is not a model list")
	}
	out := map[string][]string{}
	for _, m := range *list.Data {
		if modelcat.ValidID(m.ID) != nil || modelcat.ValidID(m.ProviderID) != nil || strings.Contains(m.ProviderID, "/") {
			continue
		}
		ref := m.ProviderID + "/" + m.ID
		if _, seen := out[ref]; seen {
			continue
		}
		var modes []string
		for _, v := range m.Variants {
			if modelcat.ValidID(v.ID) != nil || v.ID == "default" || slices.Contains(modes, v.ID) ||
				len(ref)+1+len(v.ID) > MaxModelBytes {
				continue
			}
			modes = append(modes, v.ID)
		}
		if len(modes) > 0 {
			out[ref] = modes
		}
	}
	if kept, _ := json.Marshal(out); len(kept) > MaxModelVariantsBytes {
		return nil, fmt.Errorf("the reasoning modes are over %d bytes", MaxModelVariantsBytes)
	}
	return out, nil
}

// ModelVariantsView is what a session's sandbox offers as each model's
// reasoning modes.
//
// @decision(DL-160)
type ModelVariantsView struct {
	Read   bool                `json:"read" doc:"Whether the session's current sandbox has listed its models' reasoning modes"`
	Models map[string][]string `json:"models" doc:"Each model that has reasoning modes, as provider/model, to its modes in OpenCode's order; empty while read is false"`
}

// ModelVariants answers what the session's current sandbox offers, from
// the store: nothing read while the list kept is another sandbox's.
//
// @decision(DL-160)
func (s *Service) ModelVariants(ctx context.Context, p auth.Principal, ref string) (ModelVariantsView, error) {
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return ModelVariantsView{}, err
	}
	models, read, err := s.sessionVariants(ctx, sess)
	if err != nil {
		return ModelVariantsView{}, err
	}
	if models == nil {
		models = map[string][]string{}
	}
	return ModelVariantsView{Read: read, Models: models}, nil
}

// sessionVariants is the list kept for the session's current sandbox,
// and whether there is one.
//
// @decision(DL-160)
func (s *Service) sessionVariants(ctx context.Context, sess store.Session) (map[string][]string, bool, error) {
	kept, err := s.deps.Store.Sessions().ModelVariants(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return nil, false, err
	}
	if sess.SandboxID == "" || kept.SandboxID != sess.SandboxID {
		return nil, false, nil
	}
	return kept.Models, true, nil
}

// checkVariant refuses a #variant the session's current sandbox does
// not list for its model. A reference without one is not checked.
//
// @decision(DL-160)
func (s *Service) checkVariant(ctx context.Context, sess store.Session, ref string) error {
	model, variant, ok := strings.Cut(ref, "#")
	if !ok {
		return nil
	}
	models, read, err := s.sessionVariants(ctx, sess)
	if err != nil {
		return err
	}
	if read && slices.Contains(models[model], variant) {
		return nil
	}
	refusal := fmt.Sprintf("%s is not a reasoning mode %s offers in this session", variant, model)
	switch modes := models[model]; {
	case !read:
		return fmt.Errorf("%w: %s; this session has not listed its models' reasoning modes yet", ErrInvalidRequest, refusal)
	case len(modes) == 0:
		return fmt.Errorf("%w: %s; it offers none", ErrInvalidRequest, refusal)
	default:
		return fmt.Errorf("%w: %s; it offers %s", ErrInvalidRequest, refusal, strings.Join(modes, ", "))
	}
}

type modelVariantsInput struct {
	Ref string `path:"ref" doc:"Session ID or name"`
}

type modelVariantsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ModelVariantsView
}

// registerModelVariants adds the route that serves a session's models'
// reasoning modes.
//
// @decision(DL-160)
func (s *Server) registerModelVariants() {
	register(s.api, huma.Operation{
		OperationID: "session-model-variants", Method: http.MethodGet, Path: "/api/v1/sessions/{ref}/model-variants",
		Summary: "List the reasoning modes each model offers in a session", Tags: []string{"sessions"},
	}, func(ctx context.Context, in *modelVariantsInput) (*modelVariantsOutput, error) {
		out, err := s.svc.ModelVariants(ctx, principal(ctx), in.Ref)
		if err != nil {
			return nil, toHumaError(err)
		}
		return &modelVariantsOutput{CacheControl: "no-store", Body: out}, nil
	})
}
