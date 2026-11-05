package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/prep"
)

// listModels drives the endpoint and decodes it.
func listModels(t *testing.T, f *apiFixture) []ModelProviderView {
	t.Helper()
	resp, body := f.do(http.MethodGet, "/api/v1/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/models = %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Providers []ModelProviderView `json:"providers"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	return out.Providers
}

// The endpoint answers with every provider, whether or not the caller
// holds its key. A provider omitted for want of a key is an empty list
// with no explanation of why.
//
// @decision(DL-013)
func TestTheModelListNamesEveryProviderIncludingUnavailableOnes(t *testing.T) {
	f := newAPIFixture(t)
	got := listModels(t, f)

	if len(got) != len(prep.Catalogue()) {
		t.Fatalf("got %d providers, want the catalogue's %d", len(got), len(prep.Catalogue()))
	}
	for _, p := range got {
		if p.Provider == "" || p.KeyEnv == "" || p.Name == "" {
			t.Errorf("provider is missing an identifier: %+v", p)
		}
		if len(p.Models) == 0 {
			t.Errorf("provider %q lists no models, so it cannot be picked from", p.Provider)
		}
	}
	// The fixture configures no provider keys, so nothing is available
	// and the list is still complete.
	for _, p := range got {
		if p.HaveKey {
			t.Errorf("provider %q reports a key the fixture never configured", p.Provider)
		}
	}
}

// A model reaches the API as the `provider/model` string DL-014's field
// takes. OpenRouter's model IDs contain slashes of their own, so this
// pins that every reader splits on the first one.
//
// @decision(DL-013) @decision(DL-014)
func TestAModelIsListedAsTheReferenceTheModelFieldTakes(t *testing.T) {
	f := newAPIFixture(t)
	for _, p := range listModels(t, f) {
		for _, ref := range p.Models {
			if !strings.HasPrefix(ref, p.Provider+"/") {
				t.Errorf("model %q is not prefixed with its provider %q", ref, p.Provider)
			}
			// Through modelRef itself, not a second copy of its split.
			// It is what builds the Model.Ref object the sandbox
			// accepts, it lives in this package, and two readers each
			// pinned to their own literal drift together silently.
			m, ok := modelRef(ref)
			if !ok {
				t.Errorf("modelRef refused %q, so the sandbox would refuse it too", ref)
				continue
			}
			if m["providerID"] != p.Provider {
				t.Errorf("modelRef(%q) providerID = %v, want %q", ref, m["providerID"], p.Provider)
			}
			if m["id"] == "" || m["id"] == nil {
				t.Errorf("modelRef(%q) carries no id", ref)
			}
		}
	}
	// OpenRouter is the case that makes the rule worth testing.
	var openrouter ModelProviderView
	for _, p := range listModels(t, f) {
		if p.Provider == "openrouter" {
			openrouter = p
		}
	}
	if openrouter.Provider == "" {
		t.Fatal("no openrouter provider, so the multi-slash case is untested")
	}
	var multi bool
	for _, ref := range openrouter.Models {
		if strings.Count(ref, "/") > 1 {
			multi = true
		}
	}
	if !multi {
		t.Error("no openrouter model has a slash in its own ID; the case this test exists for is gone")
	}
}

// have_key reports what a launch would carry, which is the deployment's
// own environment as well as the caller's stored credentials.
//
// @decision(DL-013) @decision(DL-008)
func TestHaveKeyFollowsWhatALaunchWouldCarry(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "sk-deployment-secret"}
	})
	var anthropic, openai ModelProviderView
	for _, p := range listModels(t, f) {
		switch p.Provider {
		case "anthropic":
			anthropic = p
		case "openai":
			openai = p
		}
	}
	if !anthropic.HaveKey {
		t.Error("anthropic reports no key, but the deployment exports one and a launch would carry it")
	}
	if openai.HaveKey {
		t.Error("openai reports a key nobody configured")
	}
}

// The endpoint is a read path serving a browser and the keys it asks
// about are secrets. It reduces them to presence and must never carry
// a value.
//
// @decision(DL-013)
func TestTheModelListCarriesNoKeyValue(t *testing.T) {
	const secret = "sk-deployment-secret-value"
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": secret}
	})
	resp, body := f.do(http.MethodGet, "/api/v1/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("the response carries a provider key: %s", body)
	}
}

// A caller's own stored key counts too: the endpoint consults
// UserProviderKeys and does not read the deployment's map alone. The
// merge itself is tested in hostedCredentials.providerKeysFor; this
// pins only that the endpoint asks.
//
// @decision(DL-013) @decision(DL-042)
func TestAStoredKeyMakesItsProviderAvailable(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.UserProviderKeys = func(context.Context, string, string) (map[string]string, error) {
			return map[string]string{"GROQ_API_KEY": "gsk-users-own"}, nil
		}
	})
	var groq ModelProviderView
	for _, p := range listModels(t, f) {
		if p.Provider == "groq" {
			groq = p
		}
	}
	if !groq.HaveKey {
		t.Error("a stored GROQ key did not make groq available")
	}
}

// An empty value is not a key. The resolution returns a map that may
// carry blank entries, and a blank one would mark a provider available
// and fail at launch.
//
// @decision(DL-013)
func TestABlankKeyIsNotAKey(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.ProviderKeys = map[string]string{"XAI_API_KEY": ""}
	})
	for _, p := range listModels(t, f) {
		if p.Provider == "xai" && p.HaveKey {
			t.Error("a blank XAI_API_KEY was reported as a key the caller holds")
		}
	}
}

// have_key is per-caller information, so the endpoint must be behind
// authentication. A route that silently fell outside the prefix would
// answer 200 and report which providers the deployment has configured.
//
// @decision(DL-013)
func TestTheModelListRefusesAnUnauthenticatedCaller(t *testing.T) {
	f := newAPIFixture(t)
	resp, body := f.do(http.MethodGet, "/api/v1/models", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/models unauthenticated = %d, want 401: %s", resp.StatusCode, body)
	}
	for _, p := range prep.Catalogue() {
		if strings.Contains(string(body), p.KeyEnv) {
			t.Errorf("the 401 names %s, telling an anonymous caller what the deployment holds", p.KeyEnv)
		}
	}
}

// The listing carries the live list newest first, its beta marks and
// where it came from; the shipped snapshot says so.
//
// @decision(DL-013)
func TestTheModelListSaysWhereItCameFrom(t *testing.T) {
	list := modelcat.List{Providers: map[string][]modelcat.Model{
		"anthropic": {{ID: "claude-next", Beta: true}, {ID: "claude-sonnet-5"}},
	}, AsOf: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}
	f := newAPIFixtureWith(t, func(d *Deps) { d.Models = FixedModels(list) })
	resp, body := f.do(http.MethodGet, "/api/v1/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	var out struct {
		Providers []ModelProviderView `json:"providers"`
		Catalogue CatalogueView       `json:"catalogue"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Catalogue.Source != "live" || !out.Catalogue.AsOf.Equal(list.AsOf) {
		t.Errorf("catalogue = %+v", out.Catalogue)
	}
	for _, p := range out.Providers {
		if p.Provider != "anthropic" {
			continue
		}
		if strings.Join(p.Models, ",") != "anthropic/claude-next,anthropic/claude-sonnet-5" ||
			strings.Join(p.Beta, ",") != "anthropic/claude-next" {
			t.Errorf("anthropic = %+v", p)
		}
	}
	snap := newAPIFixture(t)
	_, body = snap.do(http.MethodGet, "/api/v1/models", nil)
	if !strings.Contains(string(body), `"source":"snapshot"`) {
		t.Errorf("the default list is not reported as the snapshot: %.200s", body)
	}
}
