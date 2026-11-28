package modelcat

import (
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/prep"
)

// The reasoning modes shipped in the binary are the pinned build's, so a
// bump cannot land without writing them again, and they name only
// providers spov carries, models whose ids are ids, and some modes.
//
//	SPOV_PROBE=1 go test -tags integration ./internal/server/ -run WriteTheVariantSnapshot -v
//
// @decision(DL-185)
func TestTheVariantSnapshotIsThePinnedBuilds(t *testing.T) {
	m := ModeSnapshot()
	if m.OpenCode != config.DefaultOpenCodeVersion {
		t.Fatalf("the reasoning modes are OpenCode %q's, the pin is %q: write them again", m.OpenCode, config.DefaultOpenCodeVersion)
	}
	carried := map[string]bool{}
	for _, p := range prep.Catalogue() {
		carried[p.ID] = true
	}
	if len(m.Models) == 0 {
		t.Fatal("the snapshot holds no model")
	}
	for ref, modes := range m.Models {
		provider, model, _ := strings.Cut(ref, "/")
		if !carried[provider] || ValidID(model) != nil {
			t.Errorf("%q is not a carried provider's model", ref)
		}
		if len(modes) == 0 {
			t.Errorf("%s is listed with no mode", ref)
		}
		for _, v := range modes {
			if ValidID(v) != nil || v == "default" {
				t.Errorf("%s lists %q", ref, v)
			}
		}
	}
	for ref := range m.Models {
		got := m.For(ref)
		got[0] = "changed"
		if version, modes := SnapshotModes(ref); version != m.OpenCode || modes[0] == "changed" {
			t.Errorf("a caller changed the snapshot's %s: %v", ref, modes)
		}
		break
	}
}
