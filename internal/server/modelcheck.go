package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/store"
)

// modelSource says where a model came from, so a refusal can say which
// rung supplied it.
//
// @decision(DL-014)
type modelSource int

const (
	// modelFromRequest is a model this launch named.
	modelFromRequest modelSource = iota
	// modelFromProfile is the caller's stored default, supplied because
	// the launch named none.
	modelFromProfile
	// modelFromSettings is a model being stored as a default, refused
	// where the person choosing it can see why.
	modelFromSettings
)

// resolveModel picks what a launch runs on: the request wins, then the
// caller's stored default, then OpenCode's own.
//
// @decision(DL-014)
func resolveModel(requested string, profile store.User) (string, modelSource) {
	// Both are trimmed: the profile is read straight from a row, which
	// nothing else trims.
	if requested = strings.TrimSpace(requested); requested != "" {
		return requested, modelFromRequest
	}
	return strings.TrimSpace(profile.DefaultModel), modelFromProfile
}

// modelNoun names the value a refusal is about without quoting it, for
// the one refusal that must not echo what it was given.
//
// @decision(DL-014)
func modelNoun(src modelSource) string {
	switch src {
	case modelFromProfile:
		return "your default model"
	case modelFromSettings:
		return "that default model"
	default:
		return "the model"
	}
}

// checkModel refuses a model a launch could not run, before anything
// billable exists. Both creates call it, after the request has been
// validated and before the quota is taken; the settings save calls it
// before storing a default. "No such provider" and "no key" must keep
// distinct messages. A model the list lacks, under a provider spov
// carries, passes with the note returned (DL-013).
//
// @decision(DL-013) @decision(DL-014)
func (s *Service) checkModel(ctx context.Context, p auth.Principal, model string, src modelSource) (string, error) {
	// An empty resolved model is OpenCode's own default and skips the check.
	//
	// @decision(DL-014)
	if model == "" {
		return "", nil
	}
	// Bounded here, before any refusal quotes it, because a
	// profile-supplied model never passes validateRequest. This refusal
	// names no reference.
	//
	// @decision(DL-014)
	if len(model) > MaxModelBytes {
		return "", fmt.Errorf("%w: %s is %d bytes; the limit is %d",
			ErrInvalidRequest, modelNoun(src), len(model), MaxModelBytes)
	}
	list, _ := s.deps.Models.Models()
	provider, name, standing, err := list.Lookup(model)
	switch {
	case errors.Is(err, prep.ErrNoSuchProvider):
		// A catalogue refusal never mentions a key.
		return "", refuseModel(src, model, fmt.Sprintf(
			"%q names no provider spov knows; GET /api/v1/models lists them", model))
	case errors.Is(err, modelcat.ErrNotAModelID):
		return "", refuseModel(src, model, fmt.Sprintf(
			"%q is not a model id: at most %d bytes, with no spaces, control characters or #",
			model, modelcat.MaxModelIDBytes))
	case errors.Is(err, prep.ErrNotAReference):
		return "", refuseModel(src, model, fmt.Sprintf(
			"%q is not a provider/model reference, such as anthropic/claude-sonnet-5", model))
	case err != nil:
		// Refusals are matched by name above, so any other failure is
		// returned as an error rather than a 422.
		return "", fmt.Errorf("resolving %s against the model list: %w", model, err)
	case standing == modelcat.Deprecated:
		// The sandbox cannot resolve it (spike S-M1).
		//
		// @decision(DL-013) @decision(DL-038)
		return "", refuseModel(src, model, fmt.Sprintf(
			"%s retired %q; GET /api/v1/models lists what it serves", provider.Name, name))
	}
	// Presence only: the key values must never reach a client-visible error.
	held, err := s.heldProviders(ctx, p.OrgID, p.UserID)
	if err != nil {
		// Neither a refusal nor a pass: reported as the error it is.
		return "", fmt.Errorf("resolving the provider keys for %s: %w", model, err)
	}
	if !held[provider.KeyEnv] {
		// A sign-in that cannot be used says so, apart from a missing key.
		//
		// @decision(DL-015)
		if provider.KeyEnv == prep.OpenCodeKeyEnv {
			if why := consoleRefusal(s.consoleStandingFor(ctx, p.OrgID, p.UserID)); why != "" {
				return "", refuseModel(src, model, why)
			}
		}
		return "", refuseModel(src, model, fmt.Sprintf("%s is configured with no %s; %s",
			provider.Name, provider.KeyEnv, s.keyAdvice(provider)))
	}
	if standing == modelcat.Custom {
		return customModelNote(model), nil
	}
	return "", nil
}

// customModelNote is what a launch on a model the list lacks says.
//
// @decision(DL-013)
func customModelNote(model string) string {
	return model + " is not in spov's model list; launching on it anyway"
}

// refuseModel wraps a reason in the sentence that suits where the model
// came from. A refused stored default also names --model, since the
// setting itself is browser-only.
//
// @decision(DL-014)
func refuseModel(src modelSource, model, reason string) error {
	switch src {
	case modelFromProfile:
		return fmt.Errorf("%w: %s is your default model and cannot be run: %s."+
			" Change your default in Settings, or name another model on this launch",
			ErrInvalidRequest, model, reason)
	case modelFromSettings:
		return fmt.Errorf("%w: %s cannot be your default model: %s",
			ErrInvalidRequest, model, reason)
	default:
		return fmt.Errorf("%w: %s", ErrInvalidRequest, reason)
	}
}

// keyAdvice says how to get a key in front of this deployment. A hosted
// server (UserProviderKeys wired) reads stored keys per request; a local
// one snapshots them at startup, so the advice there names a restart.
//
// @decision(DL-013) @decision(DL-042)
func (s *Service) keyAdvice(p prep.Provider) string {
	// @decision(DL-015)
	if p.KeyEnv == prep.OpenCodeKeyEnv && s.consoleCarried() {
		return "connect OpenCode with `spov login opencode` or in Settings, or store a key with `spov credentials add " + p.KeyEnv + "`"
	}
	if s.deps.UserProviderKeys != nil {
		return fmt.Sprintf("store one with `spov credentials add %s`", p.KeyEnv)
	}
	return fmt.Sprintf("export %s or store one with `spov credentials add %s`,"+
		" then restart the server — it reads both once, at startup", p.KeyEnv, p.KeyEnv)
}

// MarkerModelNote records, in a session's log, that it launched on a
// model spov's list lacks.
//
// @decision(DL-013) @decision(DL-058)
const MarkerModelNote = store.SpovNamespace + "session.model.custom"

// markModelNote writes MarkerModelNote; a failure only warns.
//
// @decision(DL-013)
func (s *Service) markModelNote(ctx context.Context, sess store.Session, note string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SandboxReadTimeout)
	defer cancel()
	now := s.deps.Now()
	if _, err := s.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerModelNote, Text: note, At: now,
	}}); err != nil {
		s.deps.Warn(fmt.Sprintf("session %s: recording the model note: %v", sess.ID, err))
	}
}

// modelUnlisted reports whether a launch's model must be declared in the
// sandbox's config: one the list does not hold, read at launch, since the
// list may have changed since the create. Declaring a listed model is
// harmless (spike S-M1); leaving an unlisted one out is not.
//
// @decision(DL-038)
func (s *Service) modelUnlisted(model string) bool {
	if model == "" {
		return false
	}
	list, _ := s.deps.Models.Models()
	_, _, standing, err := list.Lookup(model)
	return err == nil && standing == modelcat.Custom
}
