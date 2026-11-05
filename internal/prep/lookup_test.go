package prep

import (
	"errors"
	"testing"
)

// TestLookupSaysWhichHalfIsWrong pins that the two failures stay
// distinguishable rather than collapsing into one "unusable model".
//
// @decision(DL-013)
func TestLookupSaysWhichHalfIsWrong(t *testing.T) {
	for _, tc := range []struct {
		name, ref string
		want      error
	}{
		{"no slash", "claude-sonnet-5", ErrNotAReference},
		{"empty", "", ErrNotAReference},
		{"no provider", "/claude-sonnet-5", ErrNotAReference},
		{"no model", "anthropic/", ErrNotAReference},
		{"empty variant", "anthropic/claude-sonnet-5#", ErrNotAReference},
		{"empty model with variant", "anthropic/#high", ErrNotAReference},
		{"unknown provider", "nosuchprovider/whatever", ErrNoSuchProvider},
		{"provider is a model", "claude-sonnet-5/anthropic", ErrNoSuchProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := LookupModel(tc.ref)
			if !errors.Is(err, tc.want) {
				t.Fatalf("LookupModel(%q) = %v, want %v", tc.ref, err, tc.want)
			}
		})
	}
}

// A model the list may not have still resolves to its provider, with
// the variant off it: whether it is listed is modelcat's question.
//
// @decision(DL-013)
func TestAnyModelUnderAKnownProviderResolves(t *testing.T) {
	p, model, err := LookupModel("anthropic/claude-sonnet-6#high")
	if err != nil || p.ID != "anthropic" || p.KeyEnv != "ANTHROPIC_API_KEY" || model != "claude-sonnet-6" {
		t.Errorf("got %+v %q %v", p, model, err)
	}
}
