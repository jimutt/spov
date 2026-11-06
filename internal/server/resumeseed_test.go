package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// syncedResumable is a stopped, synced session that resumes from its
// transcript, recording each launch's sync seed.
func syncedResumable(t *testing.T, localOnly bool) (*renewFixture, func() *session.SyncSeed) {
	t.Helper()
	var mu sync.Mutex
	var seeds []*session.SyncSeed
	f := resumable(t, func(d *Deps) {
		d.Snapshots = nil
		inner := d.Launch
		d.Launch = func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error) {
			mu.Lock()
			seeds = append(seeds, opts.Sync)
			mu.Unlock()
			return inner(ctx, opts, steps)
		}
	})
	sess := f.row()
	sess.Request.Sync = &store.SessionSync{Branch: "main", BaseCommit: strings.Repeat("d", 40)}
	if localOnly {
		sess.Request.Repos[0].Platform = ""
	}
	if err := f.store.Sessions().Update(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return f, func() *session.SyncSeed {
		mu.Lock()
		defer mu.Unlock()
		return seeds[len(seeds)-1]
	}
}

// A transcript resume seeds its fresh clone from the bundle the CLI
// sent, for that launch only.
//
// @decision(DL-098)
func TestATranscriptResumeCarriesTheCLIsBundle(t *testing.T) {
	f, last := syncedResumable(t, false)
	bundle := []byte("a bundle of the base")
	if code, _, body := f.resume(map[string]any{"sync_bundle": base64.StdEncoding.EncodeToString(bundle)}); code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", code, body)
	}
	f.waitRunning()
	seed := last()
	if seed == nil || string(seed.Bundle) != string(bundle) || seed.BaseCommit != strings.Repeat("d", 40) {
		t.Errorf("the resumed launch's seed = %+v", seed)
	}
	if stored := f.row().Request.Sync; stored == nil || len(stored.Bundle) != 0 {
		t.Errorf("the bundle was stored: %+v", stored)
	}
}

// A synced session with no platform has nothing to clone, so its
// transcript resume needs the bundle; a bundle that is not one is
// refused too.
//
// @decision(DL-098)
func TestAResumeRefusesWhatItCannotSeed(t *testing.T) {
	f, _ := syncedResumable(t, true)
	code, _, body := f.resume(map[string]any{})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "spov resume inside that checkout") {
		t.Errorf("a local-only resume without a bundle: %d %s", code, body)
	}
	if code, _, body := f.resume(map[string]any{"sync_bundle": "not base64!"}); code != http.StatusUnprocessableEntity {
		t.Errorf("a bad bundle: %d %s", code, body)
	}
}
