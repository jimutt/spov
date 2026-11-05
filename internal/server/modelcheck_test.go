package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/store"
)

// createSessionWith posts a session create and returns the response, so
// a refusal can be read rather than being fatal. It is the one
// definition of what a session create looks like in these tests;
// createSessionWithModel builds on it for the cases that expect a
// success and want the id.
func createSessionWith(t *testing.T, f *apiFixture, model string) (result, []byte) {
	t.Helper()
	return f.do(http.MethodPost, "/api/v1/sessions", SessionRequest{
		Repos: []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}},
		Model: model,
	})
}

// A model models.dev retired is refused before a sandbox exists, on
// both create paths, because they validate separately: the sandbox could
// not resolve it (spike S-M1).
//
// @decision(DL-013)
func TestARetiredModelIsRefusedBeforeAnythingBillable(t *testing.T) {
	f, q := jobFixtureWith(t, testModelKeys)

	req := jobRequest()
	req.Model = retiredModel
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("a retired model was accepted on a job create")
	}
	if !strings.Contains(string(body), "claude-retired") || !strings.Contains(string(body), "retired") {
		t.Errorf("the refusal does not name the model and why: %s", body)
	}
	if len(q.args) != 0 {
		t.Error("a refused job reached the queue")
	}

	resp, body = createSessionWith(t, f, retiredModel)
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("a retired model was accepted on a session create")
	}
	if !strings.Contains(string(body), "claude-retired") {
		t.Errorf("the session refusal does not name the model: %s", body)
	}

	if n := len(f.provider.Specs()); n != 0 {
		t.Errorf("%d sandboxes were created for refused requests", n)
	}
	sessions, err := f.store.Sessions().List(context.Background(), auth.LocalID, store.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("%d session rows survive a refused create", len(sessions))
	}
}

// A model the list lacks, under a provider spov carries a key for,
// launches with a note in the session's log, on both create paths.
//
// @decision(DL-013)
func TestAnUnlistedModelLaunchesWithANote(t *testing.T) {
	f, q := jobFixtureWith(t, testModelKeys)
	resp, body := createSessionWith(t, f, "anthropic/claude-sonnet-6")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("an unlisted model was refused: %d %s", resp.StatusCode, body)
	}
	req := jobRequest()
	req.Model = "anthropic/claude-sonnet-6"
	if resp, body := f.do(http.MethodPost, "/api/v1/jobs", req); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("an unlisted model was refused on a job: %d %s", resp.StatusCode, body)
	}
	if len(q.args) != 1 {
		t.Error("the job was not queued")
	}
	sessions, err := f.store.Sessions().List(context.Background(), auth.LocalID, store.SessionFilter{})
	if err != nil || len(sessions) != 2 {
		t.Fatalf("%d sessions, %v", len(sessions), err)
	}
	for _, sess := range sessions {
		events, _ := f.store.Conversations().ListEvents(context.Background(), sess.OrgID, sess.ID, 0, 0)
		noted := false
		for _, e := range events {
			if e.Source == store.SourceSpov && e.Name == MarkerModelNote && strings.Contains(e.Text, "not in spov's model list") {
				noted = true
			}
		}
		if !noted {
			t.Errorf("session %s has no model note: %+v", sess.Name, events)
		}
	}
	// A listed model carries none.
	if note, err := f.srv.svc.checkModel(context.Background(), auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}, testModel, modelFromRequest); err != nil || note != "" {
		t.Errorf("a listed model: %q %v", note, err)
	}
}

// An unknown provider prefix is refused too, and says so rather than
// blaming the model half.
//
// @decision(DL-013)
func TestAnUnknownProviderIsRefused(t *testing.T) {
	f := modelFixture(t)
	resp, body := createSessionWith(t, f, "nosuchprovider/whatever")
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("an unknown provider was accepted")
	}
	if !strings.Contains(string(body), "provider") {
		t.Errorf("the refusal does not say the provider is the problem: %s", body)
	}
}

// A reference that is not provider/model at all says what the shape is,
// since a caller who typed a bare model name has no other clue.
//
// @decision(DL-013)
func TestAMalformedReferenceIsToldTheShape(t *testing.T) {
	f := modelFixture(t)
	resp, body := createSessionWith(t, f, "claude-sonnet-5")
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("a bare model name was accepted")
	}
	if !strings.Contains(string(body), "provider/model") {
		t.Errorf("the refusal does not name the shape: %s", body)
	}
}

// The likelier of the two errors: a name that is perfectly valid and
// unusable because the caller holds no key for it. Every provider's
// models are in the list while a caller holds one or two keys.
//
// @decision(DL-013)
func TestAModelWithNoKeyIsRefusedAndNamesTheVariable(t *testing.T) {
	f, q := jobFixture(t) // no provider keys at all
	req := jobRequest()
	req.Model = testModel
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", req)
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("a model the caller has no key for was accepted")
	}
	if !strings.Contains(string(body), "ANTHROPIC_API_KEY") {
		t.Errorf("the refusal does not name the variable to set: %s", body)
	}
	if !strings.Contains(string(body), "spov credentials add ") {
		t.Errorf("the refusal does not name the command that sets it: %s", body)
	}
	if len(q.args) != 0 {
		t.Error("a refused job reached the queue")
	}
}

// The two refusals have different fixes and must keep reporting
// differently. One message covering both tells the reader neither.
//
// @decision(DL-013)
func TestTheTwoRefusalsAreNotTheSameMessage(t *testing.T) {
	unusable := modelFixture(t)
	_, unknownModel := createSessionWith(t, unusable, retiredModel)

	unkeyed := newAPIFixture(t)
	_, noKey := createSessionWith(t, unkeyed, testModel)

	// Stated both ways round: each message must say its own thing and
	// must not say the other's. A collapse that kept one wording would
	// otherwise pass on the half it kept.
	if !strings.Contains(string(unknownModel), "retired") {
		t.Errorf("the retired-model refusal does not say so: %s", unknownModel)
	}
	if strings.Contains(string(unknownModel), "ANTHROPIC_API_KEY") {
		t.Errorf("an unknown model was reported as a missing key: %s", unknownModel)
	}
	// The provider branch is a catalogue answer too, and its wording is
	// the easiest of the three to drift into the key one: "no provider
	// spov carries a key for" describes the OTHER refusal's fix and
	// sends the reader to `spov login` for a model that does not exist.
	_, unknownProvider := createSessionWith(t, unusable, "cohere/command-a")
	// The rule is mechanical so it cannot be drifted past: a catalogue
	// refusal does not say "key" at all. "no provider spov carries a
	// key for" leaks nothing and still sends the reader to `spov login`
	// for a provider that does not exist.
	for _, body := range [][]byte{unknownModel, unknownProvider} {
		if strings.Contains(strings.ToLower(string(body)), "key") {
			t.Errorf("a catalogue refusal talks about a key, which is the other"+
				" refusal's fix: %s", body)
		}
	}
	if !strings.Contains(string(noKey), "ANTHROPIC_API_KEY") {
		t.Errorf("the missing-key refusal does not name the variable: %s", noKey)
	}
	if strings.Contains(string(noKey), "serves no model") {
		t.Errorf("a missing key was reported as an unknown model: %s", noKey)
	}
}

// A caller's own stored key is enough: the check asks the same
// resolution a launch does, so a model it accepts is one the sandbox
// will be handed a key for.
//
// @decision(DL-013) @decision(DL-042)
func TestAStoredKeyIsEnoughToPassTheCheck(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.UserProviderKeys = func(context.Context, string, string) (map[string]string, error) {
			return map[string]string{"ANTHROPIC_API_KEY": "sk-the-users-own"}, nil
		}
	})
	resp, body := createSessionWith(t, f, testModel)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a stored key did not pass the check: %d %s", resp.StatusCode, body)
	}
}

// An empty model means OpenCode's own default. It must not need a key.
//
// @decision(DL-013) @decision(DL-014)
func TestAnEmptyModelSkipsBothChecks(t *testing.T) {
	f := newAPIFixture(t) // no provider keys at all
	resp, body := createSessionWith(t, f, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("an omitted model was refused: %d %s", resp.StatusCode, body)
	}
}

// A `#variant` reference is accepted.
//
// @decision(DL-013) @decision(DL-014)
func TestAVariantIsAcceptedByTheCheck(t *testing.T) {
	f := modelFixture(t)
	resp, body := createSessionWith(t, f, testModel+"#high")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a variant reference was refused: %d %s", resp.StatusCode, body)
	}
}

// LookupModel and modelRef are two parsers and must agree on what a
// reference is.
//
// @decision(DL-013) @decision(DL-014)
func TestTheCheckAndTheSandboxAgreeOnWhatAReferenceIs(t *testing.T) {
	list := modelcat.Snapshot()
	for _, p := range prep.Catalogue() {
		for _, m := range list.Providers[p.ID] {
			for _, ref := range []string{p.Ref(m.ID), p.Ref(m.ID) + "#high"} {
				if _, _, err := prep.LookupModel(ref); err != nil {
					t.Errorf("the check refuses %q: %v", ref, err)
					continue
				}
				if _, ok := modelRef(ref); !ok {
					t.Errorf("the check admits %q but the sandbox endpoint cannot be told it", ref)
				}
			}
		}
	}
	// And the other direction on the shapes modelRef refuses: one the
	// check admitted would be accepted at create time and dropped at
	// the only place that can set it.
	for _, ref := range []string{
		"claude-sonnet-5", "/claude-sonnet-5", "anthropic/", "anthropic/claude-sonnet-5#", "anthropic/#high",
	} {
		if _, ok := modelRef(ref); ok {
			t.Fatalf("modelRef accepts %q; this test's premise is gone", ref)
		}
		if _, _, err := prep.LookupModel(ref); err == nil {
			t.Errorf("the check admits %q, which the sandbox endpoint refuses", ref)
		}
	}
}

// The check asks for the keys themselves and its other exit is a
// client-visible message. Nothing it produces may carry one.
//
// @decision(DL-013)
func TestNoRefusalCarriesAKeyValue(t *testing.T) {
	const secret = "sk-the-deployments-own-secret"
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": secret}
	})
	for _, model := range []string{
		"anthropic/claude-sonnet-6", "nosuchprovider/x", "claude-sonnet-5", "openai/gpt-6-astra",
	} {
		_, body := createSessionWith(t, f, model)
		if strings.Contains(string(body), secret) {
			t.Fatalf("a refusal of %q carries a provider key: %s", model, body)
		}
	}
}

// A resolution that fails is neither a refusal nor a pass; it is
// reported as what it is, and nothing is created.
//
// @decision(DL-013)
func TestAFailedKeyResolutionRefusesWithoutBlamingTheCaller(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.UserProviderKeys = func(context.Context, string, string) (map[string]string, error) {
			return nil, errors.New("the credential store is unreachable")
		}
	})
	resp, body := createSessionWith(t, f, testModel)
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("the create went ahead without knowing whether the caller has a key")
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("a store failure answered %d, want 502: reporting it as an invalid"+
			" request blames the caller for spov's outage, and any other status is"+
			" a drift nothing else would notice: %s", resp.StatusCode, body)
	}
	if n := len(f.provider.Specs()); n != 0 {
		t.Errorf("%d sandboxes were created", n)
	}
}

// The model check runs before the quota: a caller both at their limit
// and with a bad model is told about the model, which they can fix now.
//
// @decision(DL-013) @decision(DL-009)
func TestTheModelIsCheckedBeforeTheQuota(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Quotas.MaxConcurrentSessions = 1
	})
	if resp, body := createSessionWith(t, f, ""); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the first create: %d %s", resp.StatusCode, body)
	}
	// The owner is now at their limit, so a good model would be 429.
	if resp, body := createSessionWith(t, f, ""); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the quota did not refuse a second session (%d), so this test proves nothing: %s",
			resp.StatusCode, body)
	}
	resp, body := createSessionWith(t, f, "anthropic/claude-sonnet-6")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a bad model at the quota answered %d, want 422: the quota was checked first: %s",
			resp.StatusCode, body)
	}
}

// The fix for a missing key is not the same sentence in both modes.
//
// @decision(DL-013) @decision(DL-042)
func TestTheKeyAdviceSuitsTheDeploymentItIsGivenOn(t *testing.T) {
	local := newAPIFixture(t)
	_, localBody := createSessionWith(t, local, testModel)
	if !strings.Contains(string(localBody), "restart") {
		t.Errorf("a local server's refusal does not mention the restart its snapshot needs,"+
			" so following its own advice leaves the caller refused: %s", localBody)
	}

	hosted := newAPIFixtureWith(t, func(d *Deps) {
		d.UserProviderKeys = func(context.Context, string, string) (map[string]string, error) {
			return map[string]string{}, nil
		}
	})
	_, hostedBody := createSessionWith(t, hosted, testModel)
	if strings.Contains(string(hostedBody), "restart") {
		t.Errorf("a hosted server tells the caller to restart it; it resolves stored keys"+
			" per request and there is nothing to restart: %s", hostedBody)
	}
	// Both still name the command that stores one.
	for _, body := range [][]byte{localBody, hostedBody} {
		if !strings.Contains(string(body), "spov credentials add ANTHROPIC_API_KEY") {
			t.Errorf("the refusal does not name the command that stores a key: %s", body)
		}
	}
}

// An unlisted model reaches the sandbox declared under its provider; a
// listed one is not declared.
//
// @decision(DL-038)
func TestAnUnlistedModelIsDeclaredInTheSandbox(t *testing.T) {
	for model, declared := range map[string]bool{"anthropic/claude-sonnet-6": true, testModel: false} {
		f := modelFixture(t)
		createSessionWithModel(t, f, model)
		waitFor(t, "the session to be launched", func() bool { return len(f.provider.Specs()) > 0 })
		script := strings.Join(f.provider.Specs()[0].Command, " ")
		if got := strings.Contains(script, `"providers"`); got != declared {
			t.Errorf("%s: declared = %v, want %v:\n%s", model, got, declared, script)
		}
	}
}
