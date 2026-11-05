package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/modelcat"
)

const doc = `{"anthropic":{"models":{"m":{"id":"m","tool_call":true,"modalities":{"input":["text"],"output":["text"]}}}}}`

// The snapshot is the parsed list, dated by the day, from a file or a
// URL; a document with nothing spov carries writes nothing.
//
// @decision(DL-013)
func TestTheSnapshotIsWrittenFromTheDocument(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "api.json"), filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(src, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	now := time.Date(2026, 10, 19, 15, 4, 5, 0, time.UTC)
	if err := run([]string{src, dst}, &out, now); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(dst)
	if !strings.Contains(string(raw), `"as_of": "2026-10-19T00:00:00Z"`) || !strings.Contains(out.String(), "1 models") {
		t.Errorf("wrote %s, said %s", raw, out.String())
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(doc)) }))
	defer srv.Close()
	if err := run([]string{srv.URL, dst}, &out, now); err != nil {
		t.Errorf("from a URL: %v", err)
	}
	empty := filepath.Join(dir, "empty.json")
	_ = os.WriteFile(empty, []byte(`{}`), 0o600)
	if err := run([]string{empty, dst}, &out, now); err == nil {
		t.Error("an empty document was written")
	}
	if err := run([]string{src}, &out, now); err == nil {
		t.Error("a missing argument was accepted")
	}
	_ = modelcat.Snapshot()
}
