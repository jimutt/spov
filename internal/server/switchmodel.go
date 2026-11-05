package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/store"
)

// Switching a running session's model: a per-act write like a message,
// relayed to OpenCode's switchModel. The current model is read back
// from the durable session.model.selected frame, never from here.
//
// @decision(DL-014)

// MarkerModelSwitched records who switched a session's model, and to
// which.
//
// @decision(DL-014) @decision(DL-058)
const MarkerModelSwitched = store.SpovNamespace + "session.model.switched"

// eventModelSelected is OpenCode's durable record of a conversation's
// model, written on a switch: data.model {id, providerID, variant?}.
const eventModelSelected = "session.model.selected"

// ModelSwitch names the model to switch to.
//
// @decision(DL-014)
type ModelSwitch struct {
	// Model is a provider/model[#variant] reference.
	Model string `json:"model" minLength:"1" maxLength:"256" doc:"The model to switch to, as provider/model"`
	// Conversation is the OpenCode session to switch; it may be omitted
	// only when the session has exactly one.
	Conversation string `json:"conversation,omitempty" doc:"OpenCode conversation id; required when the session has more than one"`
}

// SwitchedModel is the switch the sandbox took.
type SwitchedModel struct {
	Model        string `json:"model"`
	Conversation string `json:"conversation"`
}

// SwitchModel switches a running session's model.
//
// @decision(DL-014)
func (s *Service) SwitchModel(ctx context.Context, p auth.Principal, ref string, req ModelSwitch) (SwitchedModel, error) {
	model := strings.TrimSpace(req.Model)
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return SwitchedModel{}, err
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return SwitchedModel{}, fmt.Errorf("%w (state %s)", ErrSessionNotRunning, sess.State)
	}
	isJob, err := s.steerableJob(ctx, sess)
	if err != nil {
		return SwitchedModel{}, err
	}
	if model == "" {
		return SwitchedModel{}, fmt.Errorf("%w: a switch needs a model", ErrInvalidRequest)
	}
	// The same check a launch makes, then: a model the list lacks only
	// if it is the launch's own, which the sandbox's config declared.
	//
	// @decision(DL-038)
	// The key half asks about the session's owner, whose keys the
	// sandbox was launched with, not an admin acting on it.
	owner := auth.Principal{OrgID: sess.OrgID, UserID: sess.OwnerID}
	if _, err := s.checkModel(ctx, owner, model, modelFromRequest); err != nil {
		return SwitchedModel{}, err
	}
	if s.modelUnlisted(model) && withoutVariant(model) != withoutVariant(sess.Request.Model) {
		return SwitchedModel{}, fmt.Errorf("%w: %s is not in spov's model list, and such a model can be chosen only"+
			" when a session is launched", ErrInvalidRequest, model)
	}
	conversation, err := s.steerInto(ctx, sess, req.Conversation)
	first := errors.Is(err, ErrNoConversation) && !isJob
	if err != nil && !first {
		return SwitchedModel{}, err
	}
	if err := s.mintBudgetLeft(ctx, sess, ErrTooManyMessages); err != nil {
		return SwitchedModel{}, err
	}
	if !s.messages.take(sess.ID, s.deps.Now()) {
		return SwitchedModel{}, ErrTooManyMessages
	}
	err = s.withConnection(ctx, p, sess, func(conn Connection) error {
		// @decision(DL-019)
		if first && conversation == "" {
			id, err := s.firstConversation(ctx, conn, sess)
			if err != nil {
				return err
			}
			conversation = id
		}
		return setConversationModel(ctx, conn, conversation, model)
	})
	if err != nil {
		return SwitchedModel{}, err
	}
	s.recordAsk(ctx, sess, MarkerModelSwitched, "model switched to "+model+" by "+cancelledBy(p), nil)
	// @decision(DL-017)
	if s.OnStart != nil {
		s.OnStart(sess)
	}
	return SwitchedModel{Model: model, Conversation: conversation}, nil
}

// withoutVariant drops a reference's #variant.
func withoutVariant(ref string) string {
	model, _, _ := strings.Cut(ref, "#")
	return model
}

// storedModel re-derives the model a session.model.selected frame names,
// as provider/model[#variant], "" for any other row.
//
// @decision(DL-014) @decision(DL-018)
func storedModel(ev store.Event) string {
	if ev.Source != store.SourceOpenCode || ev.Name != eventModelSelected || len(ev.Raw) == 0 {
		return ""
	}
	var frame struct {
		Data struct {
			Model struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
				Variant    string `json:"variant"`
			} `json:"model"`
		} `json:"data"`
	}
	if json.Unmarshal(ev.Raw, &frame) != nil {
		return ""
	}
	m := frame.Data.Model
	if m.ID == "" || m.ProviderID == "" || modelcat.ValidID(m.ID) != nil ||
		modelcat.ValidID(m.ProviderID) != nil || strings.Contains(m.ProviderID, "/") {
		return ""
	}
	ref := m.ProviderID + "/" + m.ID
	if m.Variant != "" && m.Variant != "default" {
		ref += "#" + m.Variant
	}
	if len(ref) > MaxModelBytes {
		return ""
	}
	return ref
}

type switchModelInput struct {
	Ref  string `path:"ref" doc:"Session ID or name"`
	Body ModelSwitch
}

type switchModelOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SwitchedModel
}

// registerSwitchModel adds the switch route.
//
// @decision(DL-014)
func (s *Server) registerSwitchModel() {
	register(s.api, huma.Operation{
		OperationID: "switch-model", Method: http.MethodPost, Path: "/api/v1/sessions/{ref}/model",
		Summary: "Switch a running session's model", DefaultStatus: http.StatusAccepted,
		Tags: []string{"sessions"}, Errors: []int{http.StatusConflict, http.StatusTooManyRequests},
	}, func(ctx context.Context, in *switchModelInput) (*switchModelOutput, error) {
		out, err := s.svc.SwitchModel(ctx, principal(ctx), in.Ref, in.Body)
		if err != nil {
			return nil, toSteerHumaError(err)
		}
		return &switchModelOutput{CacheControl: "no-store", Body: out}, nil
	})
}
