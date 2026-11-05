package statefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/addonindex"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/storetest"
)

func TestTheStateFileIsAnImageStore(t *testing.T) {
	storetest.RunImages(t, func(t *testing.T) store.ImageStore {
		return &File{Path: filepath.Join(t.TempDir(), "spov", Name)}
	})
}

func TestTheStateFileOutlivesTheProcessAndHoldsBoth(t *testing.T) {
	path := filepath.Join(t.TempDir(), Name)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	first := &File{Path: path}
	if err := first.SaveAnswers(map[string]addonindex.Answer{"node@22": {Version: "22.23.3", At: at}}); err != nil {
		t.Fatal(err)
	}
	if err := first.Record(context.Background(), store.Image{Name: "v", Kind: store.ImageVariant, RequestKey: "k", PublishedAt: at}); err != nil {
		t.Fatal(err)
	}

	second := &File{Path: path}
	answers, _ := second.LoadAnswers()
	if answers["node@22"].Version != "22.23.3" {
		t.Fatalf("answers: %+v", answers)
	}
	if img, err := second.Newest(context.Background(), "k"); err != nil || img.Name != "v" {
		t.Fatalf("records: %+v, %v", img, err)
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || !fi.IsDir() {
		t.Fatal(err)
	}
}

func TestAnUnreadableStateFileReadsAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), Name)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	f := &File{Path: path, Warn: func(m string) { warnings = append(warnings, m) }}
	if answers, err := f.LoadAnswers(); err != nil || len(answers) != 0 {
		t.Fatalf("answers: %v, %v", answers, err)
	}
	if list, err := f.List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("list: %v, %v", list, err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "treated as empty") {
		t.Fatalf("one warning: %q", warnings)
	}
	// The next write replaces it.
	if err := f.SetPrunedAt(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "pruned_at") {
		t.Fatalf("rewritten: %s", raw)
	}
}

// The model list is kept beside the answers and images, and an empty
// file holds none.
//
// @decision(DL-013)
func TestTheStateFileKeepsTheModelList(t *testing.T) {
	path := filepath.Join(t.TempDir(), Name)
	f := &File{Path: path}
	if _, ok := f.LoadModels(); ok {
		t.Fatal("an empty file holds a model list")
	}
	if err := f.SaveAnswers(map[string]addonindex.Answer{"node@22": {Version: "22.1.0"}}); err != nil {
		t.Fatal(err)
	}
	want := modelcat.Fixed("anthropic/claude-sonnet-5")
	want.ETag = `"v1"`
	if err := f.SaveModels(want); err != nil {
		t.Fatal(err)
	}
	got, ok := (&File{Path: path}).LoadModels()
	if !ok || !got.Has("anthropic", "claude-sonnet-5") || got.ETag != `"v1"` {
		t.Errorf("models = %+v, %v", got, ok)
	}
	if answers, _ := f.LoadAnswers(); answers["node@22"].Version != "22.1.0" {
		t.Error("saving the models lost the answers")
	}
}
