package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/safego"
	"github.com/jimutt/spov/internal/store"
)

// A model's reasoning modes are OpenCode's variants of it, read by the
// recorder from the running sandbox once per run and served from the
// store.
//
// @decision(DL-159) @decision(DL-160)

// MaxModelListBytes bounds the model list read from a sandbox,
// MaxModelVariantsBytes what is kept of it, as JSON, MaxModelVariants
// the variants kept of one model, and MaxModelListReads the reads one
// process makes of one run's list.
//
// @decision(DL-159)
const (
	MaxModelListBytes     = 4 << 20
	MaxModelVariantsBytes = 256 << 10
	MaxModelVariants      = 64
	MaxModelListReads     = 3
)

// ModelListWaits is how many more times one read asks again, a
// ModelListPause apart, while the sandbox lists no model at all: from
// 2.4 OpenCode builds its list a few seconds after it starts.
//
// @decision(DL-183)
const (
	ModelListWaits = 10
	ModelListPause = time.Second
)

// errNoModelsListed is a model list that names no model, which is one
// not built yet.
var errNoModelsListed = errors.New("the sandbox lists no model yet")

// modelListPath is OpenCode's model list: every model the sandbox can
// run, each with its variants.
//
// @decision(DL-159)
const modelListPath = "/api/model"

// variantsRead is how one process's reads of a session's model list
// stand, for the sandbox it names.
//
// @decision(DL-159)
type variantsRead struct {
	sandbox  string
	running  bool
	done     bool
	failures int
}

// startModelVariants reads the run's model list beside the attach, so
// neither the catch-up nor a session at its storage limit waits for it
// or loses it: one read at a time per session, none once one is kept,
// and at most MaxModelListReads by this process, each at an attach. It
// outlives the span, bounded by SandboxReadTimeout, and ends with Run.
//
// @decision(DL-159) @decision(DL-018)
func (r *Recorder) startModelVariants(ctx context.Context, c *capture) {
	sess, conn := c.sess, c.conn
	r.mu.Lock()
	st := r.variants[sess.ID]
	if st == nil || st.sandbox != sess.SandboxID {
		st = &variantsRead{sandbox: sess.SandboxID}
		r.variants[sess.ID] = st
	}
	if st.running || st.done || st.failures >= MaxModelListReads {
		r.mu.Unlock()
		return
	}
	st.running = true
	parent := r.runCtx
	r.mu.Unlock()
	if parent == nil {
		parent = context.WithoutCancel(ctx)
	}
	r.wg.Go(func() {
		rctx, cancel := context.WithTimeout(parent, SandboxReadTimeout)
		defer cancel()
		var err error
		for wait := 0; ; wait++ {
			if safego.Do("recorder model list of session "+sess.ID, r.warn, func() { err = r.readModelVariants(rctx, sess, conn) }) {
				err = errors.New("the read panicked")
			}
			// @decision(DL-183)
			if !errors.Is(err, errNoModelsListed) || wait == ModelListWaits || r.sleep(rctx, ModelListPause) != nil {
				break
			}
		}
		r.mu.Lock()
		st.running = false
		if err == nil {
			st.done = true
		} else {
			st.failures++
		}
		first := st.failures == 1
		r.mu.Unlock()
		if err != nil && first {
			r.warn(fmt.Sprintf("recorder %s: the reasoning modes were not read: %s", sess.ID, err))
		}
	})
}

// readModelVariants keeps what the sandbox says its models offer,
// unless the store holds this sandbox's list already. A session that
// no longer runs on the sandbox is not an error: there is nothing left
// to keep it for.
//
// @decision(DL-159)
func (r *Recorder) readModelVariants(ctx context.Context, sess store.Session, conn Connection) error {
	kept, err := r.store.Sessions().ModelVariants(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return fmt.Errorf("read the store: %s", redact.Error(err))
	}
	if kept.SandboxID == sess.SandboxID {
		return nil
	}
	body, err := r.fetchBounded(ctx, conn, modelListPath, MaxModelListBytes)
	if err != nil {
		return errors.New(redact.Error(err))
	}
	models, err := parseModelVariants(body)
	if err != nil {
		return err
	}
	return keepModesOf(ctx, r.store, r.openCode, r.warn, sess, models)
}

// keepModesOf keeps a list read from the session's sandbox for that
// sandbox, and lays it over its org's seen modes; a session that no
// longer runs there is not an error. The recorder and an opening that
// read the list first both keep it here.
//
// @decision(DL-159) @decision(DL-186) @decision(DL-188)
func keepModesOf(ctx context.Context, st store.Store, pin string, warn func(string), sess store.Session, models map[string][]string) error {
	err := st.Sessions().SetModelVariants(ctx, sess.OrgID, sess.ID, sess.SandboxID, models)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("keep them: %s", redact.Error(err))
	}
	keepSeenModes(ctx, st, pin, warn, sess, models)
	return nil
}

// keepSeenModes lays a run's list over its org's seen modes when the
// run's sandbox ran the deployment's pinned OpenCode, keeping only the
// providers spov carries. A failure is logged and changes nothing else.
//
// @decision(DL-186)
func keepSeenModes(ctx context.Context, st store.Store, pin string, warn func(string), sess store.Session, models map[string][]string) {
	if pin == "" {
		return
	}
	run, err := st.Runs().Current(ctx, sess.OrgID, sess.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		warn(fmt.Sprintf("session %s: the organisation's reasoning modes were not kept: %s", sess.ID, redact.Error(err)))
	}
	if err != nil || run.SandboxID != sess.SandboxID || run.OpenCode != pin {
		return
	}
	carried := map[string]bool{}
	for _, p := range prep.Catalogue() {
		carried[p.ID] = true
	}
	kept := map[string][]string{}
	for ref, modes := range models {
		if provider, _, _ := strings.Cut(ref, "/"); carried[provider] {
			kept[ref] = modes
		}
	}
	if len(kept) == 0 {
		return
	}
	if err := st.ModelModes().Keep(ctx, sess.OrgID, run.OpenCode, kept); err != nil {
		warn(fmt.Sprintf("session %s: the organisation's reasoning modes were not kept: %s", sess.ID, redact.Error(err)))
	}
}

// ModelModes is the reasoning modes a launch on this deployment is
// offered for a provider/model reference: the newest its org's
// sandboxes reported on the pinned build, else the snapshot shipped
// with that build, else none. A store error is returned, never
// answered from the snapshot, so a caller does not refuse a mode the
// org's sandboxes reported.
//
// @decision(DL-186) @decision(DL-185)
func (s *Service) ModelModes(ctx context.Context, orgID, ref string) ([]string, error) {
	lookup, err := s.modeLookup(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return lookup(ref), nil
}

// parseModelVariants reads OpenCode's model list into each model's
// variant ids, in OpenCode's order, keyed provider/model. A model with
// none, an id that is not one, a variant named default and a repeat are
// left out, and a model keeps at most MaxModelVariants; a body over
// MaxModelListBytes, one that is not a model list and a result over
// MaxModelVariantsBytes are errors, the last found as it grows.
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
	// @decision(DL-183)
	if len(*list.Data) == 0 {
		return nil, errNoModelsListed
	}
	out := map[string][]string{}
	// size counts what the kept list's JSON takes: {} and, per model,
	// "ref":[] and a comma, and per variant "id" and a comma.
	size := 2
	tooLarge := fmt.Errorf("the reasoning modes are over %d bytes", MaxModelVariantsBytes)
	for _, m := range *list.Data {
		if modelcat.ValidID(m.ID) != nil || modelcat.ValidID(m.ProviderID) != nil || strings.Contains(m.ProviderID, "/") {
			continue
		}
		ref := m.ProviderID + "/" + m.ID
		if _, seen := out[ref]; seen {
			continue
		}
		var modes []string
		seen := map[string]bool{}
		for _, v := range m.Variants {
			if len(modes) == MaxModelVariants {
				break
			}
			if seen[v.ID] || v.ID == "default" || modelcat.ValidID(v.ID) != nil || len(ref)+1+len(v.ID) > MaxModelBytes {
				continue
			}
			seen[v.ID] = true
			modes = append(modes, v.ID)
		}
		if len(modes) == 0 {
			continue
		}
		size += len(ref) + 6
		for _, v := range modes {
			size += len(v) + 3
		}
		if size > MaxModelVariantsBytes {
			return nil, tooLarge
		}
		out[ref] = modes
	}
	if kept, _ := json.Marshal(out); len(kept) > MaxModelVariantsBytes {
		return nil, tooLarge
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
		return fmt.Errorf("%w: %s; this session holds no list of its models' reasoning modes", ErrInvalidRequest, refusal)
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
