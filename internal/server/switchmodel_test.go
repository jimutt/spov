package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// switchFixture is the steer fixture with testModels and an Anthropic
// key, so the check passes.
func switchFixture(t *testing.T, state store.JobState) *steerFix {
	t.Helper()
	f := newSteerFixtureAt(t, state, "ses_1")
	f.svc.deps.Models = FixedModels(testModels())
	f.svc.deps.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "sk-fixture"}
	return f
}

// A switch relays OpenCode's switchModel for the conversation, as the
// Model.Ref object, and records who switched.
//
// @decision(DL-014)
func TestASwitchRelaysTheModel(t *testing.T) {
	f := switchFixture(t, store.JobRunning)
	got, err := f.svc.SwitchModel(context.Background(), f.p, f.sess.ID, ModelSwitch{Model: "anthropic/claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 1 || !strings.HasSuffix(calls[0].path, "/api/session/ses_1/model") {
		t.Fatalf("the sandbox saw %+v", calls)
	}
	ref, _ := calls[0].body["model"].(map[string]any)
	if ref["providerID"] != "anthropic" || ref["id"] != "claude-opus-5" {
		t.Errorf("sent %v", calls[0].body)
	}
	if got.Model != "anthropic/claude-opus-5" || got.Conversation != "ses_1" {
		t.Errorf("switched = %+v", got)
	}
	if marks := markersOn(t, f.svc, f.sess, MarkerModelSwitched); len(marks) != 1 ||
		!strings.Contains(marks[0].Text, "claude-opus-5") {
		t.Errorf("markers = %+v", marks)
	}
}

// A switch takes the launch's checks: a retired model, a model with no
// key, an unlisted one other than the launch's own, and a job that
// cannot take a message are all refused before the sandbox is touched.
//
// @decision(DL-014) @decision(DL-038)
func TestASwitchIsCheckedLikeALaunch(t *testing.T) {
	f := switchFixture(t, store.JobRunning)
	ctx := context.Background()
	for _, model := range []string{retiredModel, "openai/gpt-6-astra", "anthropic/claude-new-unlisted", "nosuch/x"} {
		if _, err := f.svc.SwitchModel(ctx, f.p, f.sess.ID, ModelSwitch{Model: model}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want a refusal", model, err)
		}
	}
	blocked := switchFixture(t, store.JobBlocked)
	if _, err := blocked.svc.SwitchModel(ctx, blocked.p, blocked.sess.ID, ModelSwitch{Model: testModel}); !errors.Is(err, ErrJobNotSteerable) {
		t.Errorf("a blocked job: err = %v", err)
	}
	if n := len(f.calls()) + len(blocked.calls()); n != 0 {
		t.Errorf("a refused switch reached the sandbox: %d requests", n)
	}
}

// The launch's own unlisted model, which its config declared, can be
// switched back to.
//
// @decision(DL-014) @decision(DL-038)
func TestTheLaunchsOwnUnlistedModelCanBeSwitchedBackTo(t *testing.T) {
	f := switchFixture(t, store.JobRunning)
	sess := f.sess
	sess.Request.Model = "anthropic/claude-new-unlisted"
	if err := f.svc.deps.Store.Sessions().Update(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SwitchModel(context.Background(), f.p, f.sess.ID, ModelSwitch{Model: "anthropic/claude-new-unlisted#high"}); err != nil {
		t.Errorf("err = %v", err)
	}
}

// The log names the model a session.model.selected frame selects, in
// the shape spike S-M1 captured; nothing else carries one.
//
// @decision(DL-014)
func TestTheLogNamesTheSelectedModel(t *testing.T) {
	frame := func(raw string) store.Event {
		return store.Event{Source: store.SourceOpenCode, Name: eventModelSelected, Raw: []byte(raw)}
	}
	for raw, want := range map[string]string{
		`{"type":"session.model.selected","data":{"sessionID":"ses","model":{"id":"deepseek-flash","providerID":"deepseek"}}}`: "deepseek/deepseek-flash",
		`{"data":{"model":{"id":"acme/m","providerID":"openrouter","variant":"high"}}}`:                                        "openrouter/acme/m#high",
		`{"data":{"model":{"id":"m","providerID":"openai","variant":"default"}}}`:                                              "openai/m",
		`{"data":{"model":{"id":"","providerID":"openai"}}}`:                                                                   "",
		`{"data":{"model":{"id":"a b","providerID":"openai"}}}`:                                                                "",
	} {
		if got := storedModel(frame(raw)); got != want {
			t.Errorf("%s: %q, want %q", raw, got, want)
		}
	}
	other := frame(`{"data":{"model":{"id":"m","providerID":"openai"}}}`)
	other.Name = "session.step.started"
	if storedModel(other) != "" {
		t.Error("another event carried a model")
	}
}
