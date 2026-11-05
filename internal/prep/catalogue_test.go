package prep

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/credproxy"
)

// Every provider in the catalogue is complete enough to be offered.
//
// @decision(DL-013)
func TestEveryProviderIsUsable(t *testing.T) {
	seenID := map[string]bool{}
	seenEnv := map[string]Provider{}
	for _, p := range Catalogue() {
		if p.ID == "" || p.KeyEnv == "" || p.Name == "" {
			t.Errorf("incomplete provider: %+v", p)
		}
		if seenID[p.ID] {
			t.Errorf("provider %q is listed twice", p.ID)
		}
		// Two providers may share a key only when they share where it
		// is used, since the proxy holds one entry per key (DL-015).
		if prev, ok := seenEnv[p.KeyEnv]; ok && !reflect.DeepEqual(prev.API, p.API) {
			t.Errorf("%s is claimed by %s and %s with different APIs", p.KeyEnv, prev.ID, p.ID)
		}
		seenID[p.ID], seenEnv[p.KeyEnv] = true, p
		if !strings.HasSuffix(p.KeyEnv, "_API_KEY") {
			// CanonicalPlatform refuses anything else, so a variable
			// named otherwise could never be stored against it.
			t.Errorf("%s cannot be stored as a credential; those end in _API_KEY", p.KeyEnv)
		}
	}
}

// Every provider names an API host and header the proxy accepts, and no
// two share a host, so a key is never set on another provider's.
//
// @decision(DL-034)
func TestEveryProviderHasAnAPITheProxyAccepts(t *testing.T) {
	var cfg credproxy.SidecarConfig
	for env, api := range ProviderAPIs() {
		cfg.Providers = append(cfg.Providers, credproxy.Provider{Env: env, ProviderAPI: api, Key: "k"})
	}
	if len(cfg.Providers) != len(ProviderKeyEnvs()) {
		t.Errorf("%d APIs for %d key variables", len(cfg.Providers), len(ProviderKeyEnvs()))
	}
	if err := cfg.Validate(); err != nil {
		t.Error(err)
	}
}

// A reference splits on the FIRST slash, because an OpenRouter model ID
// contains slashes of its own.
//
// @decision(DL-013) @decision(DL-014)
func TestAReferenceResolvesBackToItsProvider(t *testing.T) {
	for _, p := range Catalogue() {
		got, model, err := LookupModel(p.Ref("some-model"))
		if err != nil || got.ID != p.ID || model != "some-model" {
			t.Errorf("%q resolved to %q/%q (%v)", p.Ref("some-model"), got.ID, model, err)
		}
	}
	got, model, err := LookupModel("openrouter/anthropic/claude-sonnet-5")
	if err != nil || got.ID != "openrouter" || model != "anthropic/claude-sonnet-5" {
		t.Errorf("openrouter/anthropic/claude-sonnet-5 resolved to %q/%q (%v)", got.ID, model, err)
	}
}
