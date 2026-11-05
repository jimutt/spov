package modelcat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/prep"
)

const doc = `{
 "anthropic": {"models": {
  "claude-new": {"id":"claude-new","name":"New","tool_call":true,"release_date":"2026-10-01",
    "modalities":{"input":["text","image"],"output":["text"]},"limit":{"context":200000,"output":64000}},
  "claude-old": {"id":"claude-old","tool_call":true,"release_date":"2025-01-01",
    "modalities":{"input":["text"],"output":["text"]}},
  "claude-beta": {"id":"claude-beta","tool_call":true,"status":"beta","release_date":"2026-10-02",
    "modalities":{"input":["text"],"output":["text"]}},
  "claude-gone": {"id":"claude-gone","tool_call":true,"status":"deprecated",
    "modalities":{"input":["text"],"output":["text"]}},
  "claude-notools": {"id":"claude-notools","tool_call":false,
    "modalities":{"input":["text"],"output":["text"]}},
  "claude-video": {"id":"claude-video","tool_call":true,
    "modalities":{"input":["text"],"output":["video"]}},
  "bad id": {"id":"bad id","tool_call":true,"modalities":{"input":["text"],"output":["text"]}}
 }},
 "somewhere-else": {"models": {"m": {"id":"m","tool_call":true,"modalities":{"input":["text"],"output":["text"]}}}}
}`

// Only models an agent can use are offered, newest first, beta marked,
// deprecated ones kept apart, for the providers spov carries.
//
// @decision(DL-013)
func TestParseKeepsWhatAnAgentCanUse(t *testing.T) {
	l, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range l.Providers["anthropic"] {
		ids = append(ids, m.ID)
	}
	if got := strings.Join(ids, ","); got != "claude-beta,claude-new,claude-old" {
		t.Errorf("anthropic offers %s", got)
	}
	if !l.Providers["anthropic"][0].Beta || l.Providers["anthropic"][1].Context != 200000 {
		t.Errorf("models = %+v", l.Providers["anthropic"])
	}
	if strings.Join(l.Deprecated["anthropic"], ",") != "claude-gone" {
		t.Errorf("deprecated = %v", l.Deprecated)
	}
	if _, ok := l.Providers["somewhere-else"]; ok {
		t.Error("a provider spov does not carry was kept")
	}
	if _, err := Parse([]byte("{")); err == nil {
		t.Error("a broken document parsed")
	}
}

// A listed model, a deprecated one and an unknown one under a known
// provider are told apart; the rest is prep's refusal.
//
// @decision(DL-013)
func TestLookupStandsOnTheList(t *testing.T) {
	l, _ := Parse([]byte(doc))
	for ref, want := range map[string]Standing{
		"anthropic/claude-new": Listed, "anthropic/claude-new#high": Listed,
		"anthropic/claude-gone": Deprecated, "anthropic/claude-next": Custom,
	} {
		_, _, got, err := l.Lookup(ref)
		if err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", ref, got, err, want)
		}
	}
	if _, _, _, err := l.Lookup("nosuch/x"); !errors.Is(err, prep.ErrNoSuchProvider) {
		t.Errorf("unknown provider: %v", err)
	}
	for _, bad := range []string{"anthropic/a b", "anthropic/" + strings.Repeat("x", MaxModelIDBytes+1), "anthropic/a\x00", "anthropic/a\u202eb"} {
		if _, _, _, err := l.Lookup(bad); !errors.Is(err, ErrNotAModelID) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// The snapshot loads, and every prep rung's family and literal are in
// it, so the ladder has a model to pick even before the first fetch.
//
// @decision(DL-013) @decision(DL-047)
func TestTheSnapshotHoldsEveryRung(t *testing.T) {
	l := Snapshot()
	if len(l.Providers) == 0 || l.AsOf.IsZero() {
		t.Fatalf("snapshot = %d providers, as of %v", len(l.Providers), l.AsOf)
	}
	for _, rung := range []struct {
		provider, model string
		family          *regexp.Regexp
	}{
		{"openrouter", prep.OpenRouterModel, prep.OpenRouterFamily},
		{"google", prep.GeminiModel, prep.GeminiFamily},
		{"openai", prep.OpenAIModel, prep.OpenAIFamily},
	} {
		if !l.Has(rung.provider, rung.model) {
			t.Errorf("the snapshot lacks %s/%s, the rung's fallback", rung.provider, rung.model)
		}
		if l.Newest(rung.provider, rung.family) == "" {
			t.Errorf("nothing in the snapshot matches %s's family", rung.provider)
		}
	}
}

type memPersist struct {
	mu    sync.Mutex
	list  List
	saved int
}

func (m *memPersist) LoadModels() (List, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list, len(m.list.Providers) > 0
}

func (m *memPersist) SaveModels(l List) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.list = l
	m.saved++
	return nil
}

// The source serves the snapshot until a fetch lands, revalidates with
// the ETag, keeps its list on a failure and backs off, and persists.
//
// @decision(DL-013)
func TestTheSourceRefreshesInTheBackground(t *testing.T) {
	var hits, fail atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() == 1 {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(doc))
	}))
	defer srv.Close()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	persist := &memPersist{}
	s := &Source{URL: srv.URL, Persist: persist, Now: func() time.Time { return now }}

	first, live := s.Models()
	if live || !first.Has("openai", prep.OpenAIModel) {
		t.Fatalf("the first read is not the snapshot: live=%v", live)
	}
	s.Wait()
	got, live := s.Models()
	if !live || !got.Has("anthropic", "claude-new") || got.ETag != `"v1"` || persist.saved != 1 {
		t.Fatalf("after the fetch: live=%v etag=%q saved=%d", live, got.ETag, persist.saved)
	}
	// Fresh: no fetch.
	s.Wait()
	if n := hits.Load(); n != 1 {
		t.Errorf("%d fetches while fresh", n)
	}
	// Stale: revalidated with a 304, the list kept and renewed.
	now = now.Add(Fresh + time.Minute)
	s.Models()
	s.Wait()
	if got, _ := s.Models(); got.AsOf != now || !got.Has("anthropic", "claude-new") {
		t.Errorf("after a 304: %v", got.AsOf)
	}
	// A failure keeps the list and backs off.
	fail.Store(1)
	now = now.Add(Fresh + time.Minute)
	s.Models()
	s.Wait()
	before := hits.Load()
	if got, live := s.Models(); !live || !got.Has("anthropic", "claude-new") {
		t.Error("a failed refresh dropped the list")
	}
	s.Wait()
	if hits.Load() != before {
		t.Error("a refresh was tried again inside the backoff")
	}
	// A new process starts from what was persisted.
	again := &Source{URL: srv.URL, Persist: persist, Now: func() time.Time { return now }}
	if got, live := again.Models(); !live || !got.Has("anthropic", "claude-new") {
		t.Error("the persisted list was not loaded")
	}
	again.Wait()
}

// A document that lists nothing spov carries is not kept.
//
// @decision(DL-013)
func TestAnEmptyDocumentIsNotKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"elsewhere":{"models":{}}}`))
	}))
	defer srv.Close()
	s := &Source{URL: srv.URL}
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("an empty document was accepted")
	}
	if got, live := s.Models(); live || len(got.Providers) == 0 {
		t.Error("the snapshot was replaced")
	}
	s.Wait()
}

// A persisted list older than the shipped snapshot is set aside, and a
// 304 to a request that named no version is not taken as a fresh list.
//
// @decision(DL-013)
func TestAStaleOrBogusListIsNotTaken(t *testing.T) {
	old := &memPersist{list: Fixed("anthropic/claude-ancient")}
	old.list.AsOf = Snapshot().AsOf.Add(-time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	s := &Source{URL: srv.URL, Persist: old}
	got, live := s.Models()
	s.Wait()
	if live || got.Has("anthropic", "claude-ancient") {
		t.Error("an older persisted list replaced the snapshot")
	}
	if err := (&Source{URL: srv.URL}).Refresh(context.Background()); err == nil {
		t.Error("a 304 to an unconditional request was accepted")
	}
	if got, live := s.Models(); live || len(got.Providers) == 0 {
		t.Error("the bogus 304 made the snapshot live")
	}
	s.Wait()
}

// A clipped string never ends inside a multi-byte character, and a short
// one is only trimmed.
func TestClipNeverSplitsACharacter(t *testing.T) {
	if got := clip("  short  ", 20); got != "short" {
		t.Errorf("clip = %q, want it trimmed", got)
	}
	// "é" is two bytes; cutting at 2 would land inside the second one.
	if got := clip("aéb", 2); got != "a" {
		t.Errorf("clip = %q, want a", got)
	}
	if got := clip("aéb", 3); got != "aé" {
		t.Errorf("clip = %q, want aé", got)
	}
}
