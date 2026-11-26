package modelcat

import (
	_ "embed"
	"encoding/json"
	"slices"
	"sync"
)

//go:embed variants.json
var modesSnapshot []byte

// Modes is the pinned OpenCode build's reasoning modes, as the binary
// lists them: each model's variant ids in OpenCode's order, keyed
// provider/model. A model with none is absent.
//
// @decision(DL-185)
type Modes struct {
	// OpenCode is the build the modes were read from.
	OpenCode string              `json:"opencode"`
	Models   map[string][]string `json:"models"`
}

// For is a model's modes, a copy, or nil when it has none.
func (m Modes) For(ref string) []string {
	return slices.Clone(m.Models[ref])
}

var modesOnce = sync.OnceValue(func() Modes {
	var m Modes
	if err := json.Unmarshal(modesSnapshot, &m); err != nil {
		panic("modelcat: the embedded reasoning modes are not a list: " + err.Error())
	}
	if m.Models == nil {
		m.Models = map[string][]string{}
	}
	return m
})

// SnapshotModes is the build the shipped modes were read from and one
// model's modes among them, a copy, or nil when it has none.
//
// @decision(DL-185)
func SnapshotModes(ref string) (string, []string) {
	m := modesOnce()
	return m.OpenCode, m.For(ref)
}

// ModeSnapshot is the reasoning modes shipped in the binary, written
// from the pinned build by TestIntegrationWriteTheVariantSnapshot in
// internal/server.
//
// @decision(DL-185)
func ModeSnapshot() Modes {
	m := modesOnce()
	out := Modes{OpenCode: m.OpenCode, Models: make(map[string][]string, len(m.Models))}
	for ref, modes := range m.Models {
		out.Models[ref] = slices.Clone(modes)
	}
	return out
}
