package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// offerModes seeds the fixture org's seen reasoning modes for the
// deployment's pin, which the API fixture leaves empty.
func offerModes(t *testing.T, f *apiFixture, models map[string][]string) {
	t.Helper()
	if err := f.store.ModelModes().Keep(context.Background(), auth.LocalID, f.srv.deps.Defaults.OpenCodeVersion, models); err != nil {
		t.Fatal(err)
	}
}

// A mode in a request's model is checked against the modes the model is
// offered with, on both creates, and a refusal names the ones on offer;
// one the model is offered with launches with the mode.
//
// @decision(DL-187)
func TestALaunchsOwnModeIsCheckedAtCreate(t *testing.T) {
	f, q := jobModelFixture(t)
	offerModes(t, f, map[string][]string{testModel: {"low", "high"}})

	resp, body := createSessionWith(t, f, testModel+"#max")
	if resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(string(body), `\"max\" is not a reasoning mode `+testModel+" offers; it offers low, high") {
		t.Errorf("a session on a mode not offered: %d %s", resp.StatusCode, body)
	}
	req := jobRequest()
	req.Model = "anthropic/claude-opus-5#high"
	resp, body = f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "spov knows no reasoning modes for it") {
		t.Errorf("a job on a model with no known modes: %d %s", resp.StatusCode, body)
	}
	if len(q.args) != 0 || len(f.provider.Specs()) != 0 {
		t.Error("a refused mode reached the queue or a sandbox")
	}
	resp, body = createSessionWith(t, f, testModel+"#high")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a mode the model offers was refused: %d %s", resp.StatusCode, body)
	}
	if got := storedSessionModel(t, f, body); got != testModel+"#high" {
		t.Errorf("stored %q, want the mode on the model", got)
	}
}

// The reasoning field is the mode of the model the request resolves to,
// the caller's default included; it is checked like a mode in the model,
// and refused beside one, with no model, or when it is no mode.
//
// @decision(DL-187)
func TestTheReasoningFieldIsTheResolvedModelsMode(t *testing.T) {
	f := newAPIFixtureWith(t, profileWith(t, store.User{DefaultModel: testModel}))
	offerModes(t, f, map[string][]string{testModel: {"low", "high"}})
	create := func(model, reasoning string) (result, []byte) {
		return f.do(http.MethodPost, "/api/v1/sessions", SessionRequest{
			Repos: []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}}, Model: model, Reasoning: reasoning,
		})
	}
	resp, body := create("", "high")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a mode for the default model: %d %s", resp.StatusCode, body)
	}
	if got := storedSessionModel(t, f, body); got != testModel+"#high" {
		t.Errorf("stored %q, want the default's model with the mode", got)
	}
	for _, c := range []struct{ model, reasoning, want string }{
		{"", "max", `\"max\" is not a reasoning mode`},
		{testModel + "#low", "high", "not both"},
		{"", "bad mode", "is not a reasoning mode"},
	} {
		if resp, body := create(c.model, c.reasoning); resp.StatusCode != http.StatusUnprocessableEntity ||
			!strings.Contains(string(body), c.want) {
			t.Errorf("model %q, reasoning %q: %d %s, want a 422 saying %q", c.model, c.reasoning, resp.StatusCode, body, c.want)
		}
	}
	resp, body = create(testModel+"#bad mode", "")
	if resp.StatusCode != http.StatusUnprocessableEntity || strings.Contains(string(body), "bad mode") ||
		!strings.Contains(string(body), "reasoning mode is not one") {
		t.Errorf("a mode that is no id: %d %s, want a 422 that does not quote it", resp.StatusCode, body)
	}
	moded := newAPIFixtureWith(t, profileWith(t, store.User{DefaultModel: testModel + "#high"}))
	offerModes(t, moded, map[string][]string{testModel: {"low", "high"}})
	resp, body = moded.do(http.MethodPost, "/api/v1/sessions", SessionRequest{
		Repos: []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}}, Reasoning: "low",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a mode for a default that has one: %d %s", resp.StatusCode, body)
	}
	if got := storedSessionModel(t, moded, body); got != testModel+"#low" {
		t.Errorf("stored %q, want the request's mode in place of the default's", got)
	}
	jobs, _ := jobFixtureWith(t, profileWith(t, store.User{DefaultModel: testModel}))
	offerModes(t, jobs, map[string][]string{testModel: {"low", "high"}})
	req := jobRequest()
	req.Reasoning = "high"
	if resp, body := jobs.do(http.MethodPost, "/api/v1/jobs", req); resp.StatusCode != http.StatusAccepted {
		t.Errorf("a job's reasoning field: %d %s", resp.StatusCode, body)
	} else if got := storedJobModel(t, jobs, body); got != testModel+"#high" {
		t.Errorf("a job stored %q, want its default model with the mode", got)
	}
	req.Reasoning = "max"
	if resp, body := jobs.do(http.MethodPost, "/api/v1/jobs", req); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a job's reasoning field on a mode not offered: %d %s", resp.StatusCode, body)
	}
	none := newAPIFixtureWith(t, testModelKeys)
	if resp, body := none.do(http.MethodPost, "/api/v1/sessions", SessionRequest{
		Repos: []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}}, Reasoning: "high",
	}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "needs a model") {
		t.Errorf("a mode with no model resolved: %d %s", resp.StatusCode, body)
	}
}

// A mode the caller's stored default carries is not checked at create:
// the opening decides (DL-188), so a pin that drops it refuses nothing.
// Storing such a default is checked, where its owner chooses it.
//
// @decision(DL-187) @decision(DL-189)
func TestADefaultsModeIsLeftToTheOpeningAndCheckedWhenStored(t *testing.T) {
	f := newAPIFixtureWith(t, profileWith(t, store.User{DefaultModel: testModel + "#gone"}))
	offerModes(t, f, map[string][]string{testModel: {"low", "high"}})
	resp, body := createSessionWith(t, f, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a default with a mode no longer offered was refused: %d %s", resp.StatusCode, body)
	}
	if got := storedSessionModel(t, f, body); got != testModel+"#gone" {
		t.Errorf("stored %q", got)
	}
	resp, body = f.do(http.MethodPatch, "/api/v1/me", map[string]string{"default_model": testModel + "#max"})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "it offers low, high") {
		t.Errorf("storing a default on a mode not offered: %d %s", resp.StatusCode, body)
	}
	if resp, body := f.do(http.MethodPatch, "/api/v1/me", map[string]string{"default_model": testModel + "#low"}); resp.StatusCode != http.StatusOK {
		t.Errorf("storing a default on an offered mode: %d %s", resp.StatusCode, body)
	}
}

// The model list gives each provider's listed models that have modes
// their modes, and leaves the others out.
//
// @decision(DL-187)
func TestTheModelListOffersEachModelsModes(t *testing.T) {
	f := modelFixture(t)
	offerModes(t, f, map[string][]string{testModel: {"low", "high"}, "anthropic/unlisted": {"max"}})
	resp, body := f.do(http.MethodGet, "/api/v1/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /models = %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Providers []ModelProviderView `json:"providers"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Providers {
		want := map[string][]string(nil)
		if p.Provider == "anthropic" {
			want = map[string][]string{testModel: {"low", "high"}}
		}
		if !reflect.DeepEqual(p.Variants, want) {
			t.Errorf("%s offers %v, want %v", p.Provider, p.Variants, want)
		}
	}
}

// A conversation opens on its mode when its sandbox offers it, else on
// the model's default with a note saying which, and why.
//
// @decision(DL-188)
func TestAConversationOpensOnAModeOnlyWhereTheSandboxOffersIt(t *testing.T) {
	notBuilt := `{"location":{"directory":"/workspace"},"data":[]}`
	for name, c := range map[string]struct {
		model string
		lists []string
		kept  map[string][]string
		sent  string
		note  string
	}{
		"offered":              {"anthropic/claude-sonnet-5#thinking", []string{offersThinking}, nil, "anthropic/claude-sonnet-5#thinking", ""},
		"offered once built":   {"anthropic/claude-sonnet-5#thinking", []string{notBuilt, notBuilt, offersThinking}, nil, "anthropic/claude-sonnet-5#thinking", ""},
		"kept for the sandbox": {"anthropic/claude-sonnet-5#high", nil, map[string][]string{"anthropic/claude-sonnet-5": {"high"}}, "anthropic/claude-sonnet-5#high", ""},
		"not offered":          {"anthropic/claude-sonnet-5#max", []string{offersThinking}, nil, "anthropic/claude-sonnet-5", "max is not a reasoning mode anthropic/claude-sonnet-5 offers in this sandbox; it runs on the model's default (it offers low, thinking)."},
		"unreadable":           {"anthropic/claude-sonnet-5#max", nil, nil, "anthropic/claude-sonnet-5", "could not be read; it runs on the model's default, not max."},
		"a mode that is no id": {"anthropic/claude-sonnet-5#bad mode", nil, nil, "anthropic/claude-sonnet-5", "The reasoning mode given for anthropic/claude-sonnet-5 is not one; it runs on the model's default."},
		"no mode":              {"anthropic/claude-sonnet-5", nil, nil, "anthropic/claude-sonnet-5", ""},
	} {
		t.Run(name, func(t *testing.T) {
			m := newModelSandbox(t)
			m.lists = c.lists
			s := testService(t, func(d *Deps) {
				d.ModelListPause = time.Millisecond
				d.Defaults.OpenCodeVersion = "2.4.2"
			})
			sess := jobSessionWithModel(t, s, c.model)
			ctx := context.Background()
			if err := s.deps.Store.Runs().Insert(ctx, store.Run{OrgID: sess.OrgID, SessionID: sess.ID, OwnerID: sess.OwnerID, N: 1,
				SandboxID: sess.SandboxID, OpenCode: "2.4.2", Origin: store.RunFromLaunch, StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if c.kept != nil {
				if err := s.deps.Store.Sessions().SetModelVariants(ctx, sess.OrgID, sess.ID, sess.SandboxID, c.kept); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.jobConversation(ctx, Connection{URL: m.srv.URL}, sess); err != nil {
				t.Fatal(err)
			}
			if len(m.models) != 1 || m.models[0] != c.sent {
				t.Errorf("sent %v, want %q", m.models, c.sent)
			}
			if c.kept != nil && m.called(modelListPath) {
				t.Error("a list kept for the sandbox was read again")
			}
			events, err := s.deps.Store.Conversations().ListEvents(ctx, sess.OrgID, sess.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var notes []string
			for _, e := range events {
				if e.Name == MarkerModeDropped && e.Source == store.SourceSpov {
					notes = append(notes, e.Text)
				}
			}
			switch {
			case c.note == "" && len(notes) != 0:
				t.Errorf("noted %q", notes)
			case c.note != "" && (len(notes) != 1 || !strings.Contains(notes[0], c.note)):
				t.Errorf("noted %q, want one saying %q", notes, c.note)
			}
			if name == "offered once built" {
				if got, _ := s.deps.Store.Sessions().ModelVariants(ctx, sess.OrgID, sess.ID); got.SandboxID != sess.SandboxID {
					t.Error("the list read at the opening was not kept for the sandbox")
				}
				if seen, _ := s.deps.Store.ModelModes().Seen(ctx, sess.OrgID, "2.4.2"); !reflect.DeepEqual(seen["anthropic/claude-sonnet-5"], []string{"low", "thinking"}) {
					t.Errorf("the list read at the opening did not reach the org's seen modes: %v", seen)
				}
			}
		})
	}
}

// resolveMode says a mode was named by the request only when it was.
//
// @decision(DL-187)
func TestOnlyAModeTheRequestNamesIsChecked(t *testing.T) {
	for _, c := range []struct {
		model, reasoning string
		src              modelSource
		want             string
		named            bool
	}{
		{"p/m", "", modelFromRequest, "p/m", false},
		{"p/m#high", "", modelFromRequest, "p/m#high", true},
		{"p/m#high", "", modelFromProfile, "p/m#high", false},
		{"p/m", "high", modelFromProfile, "p/m#high", true},
		{"p/m", " high ", modelFromRequest, "p/m#high", true},
		// The request's mode takes the place of the default's own.
		{"p/m#high", "low", modelFromProfile, "p/m#low", true},
		// "default" is the model's own default.
		{"p/m#high", "default", modelFromProfile, "p/m", false},
		{"p/m", "default", modelFromRequest, "p/m", false},
		{"", "default", modelFromProfile, "", false},
	} {
		got, named, err := resolveMode(c.model, c.reasoning, c.src)
		if err != nil || got != c.want || named != c.named {
			t.Errorf("resolveMode(%q, %q, %v) = %q, %v, %v; want %q, %v", c.model, c.reasoning, c.src, got, named, err, c.want, c.named)
		}
	}
}

// The marker's name is the one the page draws as a quiet note.
//
// @decision(DL-188)
func TestTheDroppedModeMarkerIsANoteOnThePage(t *testing.T) {
	src, err := os.ReadFile("../../web/src/lib/timeline.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"`+MarkerModeDropped+`"`) {
		t.Errorf("web/src/lib/timeline.ts does not name %s among its notes", MarkerModeDropped)
	}
}

// A sandbox refusing the credential while the opening reads its list
// fails the opening, so it is tried again on a new credential, rather
// than dropping the mode.
//
// @decision(DL-188)
func TestARefusedCredentialAtTheOpeningsReadIsNotADroppedMode(t *testing.T) {
	m := newModelSandbox(t)
	m.lists = []string{"401"}
	s := testService(t, nil)
	sess := jobSessionWithModel(t, s, "anthropic/claude-sonnet-5#thinking")
	_, err := s.jobConversation(context.Background(), Connection{URL: m.srv.URL}, sess)
	if !errors.Is(err, ErrSandboxUnauthorized) {
		t.Fatalf("the opening returned %v, want the refused credential", err)
	}
	if len(m.models) != 0 {
		t.Errorf("a model was set: %v", m.models)
	}
}

// storedJobModel reads back the model a job create recorded on its
// session's request.
func storedJobModel(t *testing.T, f *apiFixture, body []byte) string {
	t.Helper()
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.SessionID == "" {
		t.Fatalf("the job view: %v %s", err, body)
	}
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, out.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return sess.Request.Model
}
