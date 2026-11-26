package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

// modeLookup answers ModelModes for any number of references from one
// read of the org's seen modes.
//
// @decision(DL-186) @decision(DL-187)
func (s *Service) modeLookup(ctx context.Context, orgID string) (func(ref string) []string, error) {
	pin := s.deps.Defaults.OpenCodeVersion
	seen, err := s.deps.Store.ModelModes().Seen(ctx, orgID, pin)
	if err != nil {
		return nil, err
	}
	return func(ref string) []string {
		if modes, ok := seen[ref]; ok {
			return slices.Clone(modes)
		}
		if version, modes := modelcat.SnapshotModes(ref); version == pin {
			return modes
		}
		return nil
	}, nil
}

// resolveMode folds a request's reasoning field into the resolved model
// and says whether the request named the mode, in either place: only
// such a mode is checked at create. A reasoning of "default" is the
// model's own default, any mode the model carries dropped.
//
// @decision(DL-187)
func resolveMode(model, reasoning string, src modelSource) (string, bool, error) {
	reasoning = strings.TrimSpace(reasoning)
	_, _, named := strings.Cut(model, "#")
	if reasoning == "" {
		return model, named && src != modelFromProfile, nil
	}
	switch {
	case reasoning == DefaultReasoning && model == "":
		return "", false, nil
	case model == "":
		return "", false, fmt.Errorf("%w: a reasoning mode needs a model: name one, or store a default model", ErrInvalidRequest)
	case named && src == modelFromRequest:
		return "", false, fmt.Errorf("%w: the model names a reasoning mode already; send it in model or in reasoning, not both",
			ErrInvalidRequest)
	case modelcat.ValidID(reasoning) != nil:
		return "", false, fmt.Errorf("%w: %q is not a reasoning mode", ErrInvalidRequest, reasoning)
	}
	// The request's mode takes the place of a default's own.
	bare, _, _ := strings.Cut(model, "#")
	if reasoning == DefaultReasoning {
		return bare, false, nil
	}
	return bare + "#" + reasoning, true, nil
}

// DefaultReasoning is the reasoning field's word for the model's own
// default, which a mode may never be called (DL-159 drops one).
//
// @decision(DL-190)
const DefaultReasoning = "default"

// checkMode refuses a mode the model is not offered with, in DL-160's
// words, naming the modes it is offered with.
//
// @decision(DL-187)
func (s *Service) checkMode(ctx context.Context, orgID, model string) error {
	bare, mode, named := strings.Cut(model, "#")
	if !named {
		return nil
	}
	if modelcat.ValidID(mode) != nil {
		return fmt.Errorf("%w: the model's reasoning mode is not one: at most %d bytes, with no spaces, control characters or #",
			ErrInvalidRequest, modelcat.MaxModelIDBytes)
	}
	lookup, err := s.modeLookup(ctx, orgID)
	if err != nil {
		return fmt.Errorf("reading the reasoning modes of %s: %w", bare, err)
	}
	modes := lookup(bare)
	if slices.Contains(modes, mode) {
		return nil
	}
	why := "; spov knows no reasoning modes for it"
	if len(modes) > 0 {
		why = "; it offers " + strings.Join(modes, ", ")
	}
	return fmt.Errorf("%w: %q is not a reasoning mode %s offers%s", ErrInvalidRequest, mode, bare, why)
}

// MarkerModeDropped records, in a session's log, that a conversation
// opened on its model's default because its sandbox does not offer the
// mode it was given.
//
// @decision(DL-188)
const MarkerModeDropped = store.SpovNamespace + "session.model.mode.dropped"

// openingModel is the model a new conversation is set to: the session's
// own when it names no mode or its sandbox offers that mode, else the
// model without it, with MarkerModeDropped written. A sandbox refusing
// the connection's credential is returned, so the opening is tried
// again on a new one.
//
// @decision(DL-188)
func (s *Service) openingModel(ctx context.Context, conn Connection, sess store.Session) (string, error) {
	model := sess.Request.Model
	bare, mode, named := strings.Cut(model, "#")
	// A malformed reference is left for setConversationModel to refuse.
	if !named || bare == "" || mode == "" {
		return model, nil
	}
	var offered map[string][]string
	read, valid := false, modelcat.ValidID(mode) == nil
	if valid {
		var err error
		if offered, read, err = s.sandboxModes(ctx, conn, sess); err != nil {
			return "", err
		}
		if read && slices.Contains(offered[bare], mode) {
			return model, nil
		}
	}
	text := mode + " is not a reasoning mode " + bare + " offers in this sandbox; it runs on the model's default"
	switch {
	case !valid:
		// Never quoted: it is no id.
		text = "The reasoning mode given for " + bare + " is not one; it runs on the model's default"
	case !read:
		text = "The reasoning modes " + bare + " offers in this sandbox could not be read; it runs on the model's default, not " + mode
	case len(offered[bare]) > 0:
		text += " (it offers " + strings.Join(offered[bare], ", ") + ")"
	}
	now := s.deps.Now()
	if _, err := s.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerModeDropped, Text: text + ".", At: now,
	}}); err != nil {
		s.deps.Warn(fmt.Sprintf("session %s: recording the dropped reasoning mode: %s", sess.ID, redact.Error(err)))
	}
	return bare, nil
}

// sandboxModes is what the session's sandbox offers: the list the store
// holds for it, else one read now as the recorder reads it, waiting out
// a list not built yet, and kept as the recorder keeps it. Only a
// refused credential is an error; any other failure is a list not read.
//
// @decision(DL-188) @decision(DL-183)
func (s *Service) sandboxModes(ctx context.Context, conn Connection, sess store.Session) (map[string][]string, bool, error) {
	if kept, err := s.deps.Store.Sessions().ModelVariants(ctx, sess.OrgID, sess.ID); err == nil &&
		kept.SandboxID != "" && kept.SandboxID == sess.SandboxID {
		return kept.Models, true, nil
	}
	pause := s.deps.ModelListPause
	if pause <= 0 {
		pause = ModelListPause
	}
	rctx, cancel := context.WithTimeout(ctx, SandboxReadTimeout)
	defer cancel()
	for wait := 0; ; wait++ {
		body, err := sandboxGetBounded(rctx, conn.URL+modelListPath, conn.Token, conn.User, conn.Password, MaxModelListBytes)
		if errors.Is(err, ErrSandboxUnauthorized) {
			return nil, false, err
		}
		if err != nil {
			return nil, false, nil
		}
		models, err := parseModelVariants(body)
		if err == nil {
			if err := keepModesOf(ctx, s.deps.Store, s.deps.Defaults.OpenCodeVersion, s.deps.Warn, sess, models); err != nil {
				s.deps.Warn(fmt.Sprintf("session %s: keeping the reasoning modes: %s", sess.ID, err))
			}
			return models, true, nil
		}
		if !errors.Is(err, errNoModelsListed) || wait == ModelListWaits || sleepFor(rctx, pause) != nil {
			return nil, false, nil
		}
	}
}
