// Package modelcat is the list of models spov offers: models.dev's,
// through the mirror the pinned OpenCode reads, filtered to what an
// agent can use, over the static provider list in prep.Catalogue. The
// providers decide where a key may go and never come from here.
//
// @decision(DL-013)
package modelcat

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jimutt/spov/internal/prep"
)

// MaxModelIDBytes bounds a model id taken from the download, and a
// custom one named at launch.
//
// @decision(DL-013)
const MaxModelIDBytes = 200

// maxNameBytes bounds a model's display name from the download.
const maxNameBytes = 200

// Model is one model spov offers, with what the picker and the sandbox
// need of it.
//
// @decision(DL-013)
type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Released string `json:"released,omitempty"`
	Beta     bool   `json:"beta,omitempty"`
	Context  int    `json:"context,omitempty"`
	Output   int    `json:"output,omitempty"`
}

// List is the models of every provider spov carries, newest first, and
// the ones models.dev marks deprecated, which a sandbox cannot resolve.
//
// @decision(DL-013)
type List struct {
	Providers  map[string][]Model  `json:"providers"`
	Deprecated map[string][]string `json:"deprecated,omitempty"`
	// AsOf is when the list was fetched, or the snapshot was made.
	AsOf time.Time `json:"as_of"`
	// ETag revalidates a fetched list; a snapshot has none.
	ETag string `json:"etag,omitempty"`
}

// Standing is what a lookup found for a model under a known provider.
//
// @decision(DL-013)
type Standing int

const (
	// Listed is a model in the list.
	Listed Standing = iota
	// Custom is a model the list does not have, under a provider it
	// does: launched with a note.
	Custom
	// Deprecated is a model models.dev marks deprecated: refused, since
	// the sandbox cannot resolve it (spike S-M1).
	Deprecated
)

// ErrNotAModelID is an id that is not plausibly one: empty, too long,
// or holding whitespace or control characters.
//
// @decision(DL-013)
var ErrNotAModelID = errors.New("modelcat: not a model id")

// ValidID reports whether id can be a model id. It is checked on the
// download and on a custom name, since both are written into a sandbox's
// config (DL-038).
//
// @decision(DL-013)
func ValidID(id string) error {
	if id == "" || len(id) > MaxModelIDBytes {
		return fmt.Errorf("%w: %d bytes", ErrNotAModelID, len(id))
	}
	for _, r := range id {
		// Format characters (U+202E and the like) would let an id show
		// as another in the picker.
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '#' {
			return fmt.Errorf("%w: %q", ErrNotAModelID, id)
		}
	}
	return nil
}

// Lookup resolves a provider/model[#variant] reference: the provider it
// names, the model without its variant, and where the list stands on
// it. A reference that is not one, or names no provider spov carries, is
// prep.LookupModel's error.
//
// @decision(DL-013)
func (l List) Lookup(ref string) (prep.Provider, string, Standing, error) {
	p, model, err := prep.LookupModel(ref)
	if err != nil {
		return p, model, 0, err
	}
	if err := ValidID(model); err != nil {
		return p, model, 0, err
	}
	if l.Has(p.ID, model) {
		return p, model, Listed, nil
	}
	for _, d := range l.Deprecated[p.ID] {
		if d == model {
			return p, model, Deprecated, nil
		}
	}
	return p, model, Custom, nil
}

// Has reports whether the list offers this model.
func (l List) Has(provider, model string) bool {
	for _, m := range l.Providers[provider] {
		if m.ID == model {
			return true
		}
	}
	return false
}

// Newest is the id of the newest model of a provider that matches the
// family and is not beta, or "" when none does.
//
// @decision(DL-047)
func (l List) Newest(provider string, family *regexp.Regexp) string {
	for _, m := range l.Providers[provider] {
		if !m.Beta && family.MatchString(m.ID) {
			return m.ID
		}
	}
	return ""
}

// The parts of a models.dev entry spov reads.
type devProvider struct {
	Models map[string]devModel `json:"models"`
}

type devModel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ToolCall   bool   `json:"tool_call"`
	Status     string `json:"status"`
	Released   string `json:"release_date"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

// releasedShape is the date models.dev writes, YYYY-MM-DD or YYYY-MM.
var releasedShape = regexp.MustCompile(`^\d{4}-\d{2}(-\d{2})?$`)

// Parse reads models.dev's document into the list, for the providers in
// prep.Catalogue only: each model that takes and produces text, can call
// tools and is not deprecated, newest first. A deprecated one is kept by
// id alone. An entry that fails ValidID is dropped.
//
// @decision(DL-013)
func Parse(raw []byte) (List, error) {
	var doc map[string]devProvider
	if err := json.Unmarshal(raw, &doc); err != nil {
		return List{}, fmt.Errorf("modelcat: reading models.dev: %w", err)
	}
	out := List{Providers: map[string][]Model{}, Deprecated: map[string][]string{}}
	for _, p := range prep.Catalogue() {
		dp, ok := doc[p.ID]
		if !ok {
			continue
		}
		for key, m := range dp.Models {
			// OpenCode resolves a model by its key; an entry whose id
			// says otherwise is not one spov can name.
			if m.ID != "" && m.ID != key {
				continue
			}
			id := key
			if ValidID(id) != nil {
				continue
			}
			if m.Status == "deprecated" {
				out.Deprecated[p.ID] = append(out.Deprecated[p.ID], id)
				continue
			}
			if !m.ToolCall || !hasText(m.Modalities.Input) || !hasText(m.Modalities.Output) {
				continue
			}
			released := m.Released
			if !releasedShape.MatchString(released) {
				released = ""
			}
			out.Providers[p.ID] = append(out.Providers[p.ID], Model{
				ID: id, Name: clip(m.Name, maxNameBytes), Released: released, Beta: m.Status == "beta",
				Context: max(m.Limit.Context, 0), Output: max(m.Limit.Output, 0),
			})
		}
	}
	for id := range out.Providers {
		sortNewest(out.Providers[id])
	}
	for id := range out.Deprecated {
		sort.Strings(out.Deprecated[id])
	}
	return out, nil
}

// sortNewest orders by release date, newest first, then by id, so two
// parses of one document agree.
func sortNewest(ms []Model) {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Released != ms[j].Released {
			return ms[i].Released > ms[j].Released
		}
		return ms[i].ID < ms[j].ID
	})
}

func hasText(modes []string) bool {
	for _, m := range modes {
		if m == "text" {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// Fixed is a list of exactly these provider/model references, for a test
// or a deployment that wants no download. Each is listed with no date.
func Fixed(refs ...string) List {
	out := List{Providers: map[string][]Model{}}
	for _, ref := range refs {
		provider, model, ok := strings.Cut(ref, "/")
		if !ok {
			continue
		}
		out.Providers[provider] = append(out.Providers[provider], Model{ID: model})
	}
	return out
}
