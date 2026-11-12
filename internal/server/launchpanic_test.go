package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// warnings collects what a server warns, for a test to wait on.
type warnings struct {
	mu  sync.Mutex
	got []string
}

func (w *warnings) warn(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, s)
}

// waitFor fails unless every one of parts is in one warning within
// five seconds.
func (w *warnings) waitFor(t *testing.T, parts ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		got := strings.Join(w.got, "\n")
		w.mu.Unlock()
		all := true
		for _, p := range parts {
			all = all && strings.Contains(got, p)
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the warnings lack %q: %q", parts, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// launchEndsInPanic reads the session's launch stream, which must end
// within five seconds, and checks it ended as a failed launch whose
// detail is the fixed message, and that the row is failed.
func launchEndsInPanic(t *testing.T, f *apiFixture, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.http.URL+"/api/v1/sessions/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("the launch stream did not end: %v", err)
	}
	evs := parseEvents(raw)
	if len(evs) == 0 || evs[len(evs)-1].Name != "error" {
		t.Fatalf("the launch stream = %s, want it to end with an error", raw)
	}
	var last ErrorEvent
	_ = json.Unmarshal(evs[len(evs)-1].Data, &last)
	if last.Problem.Title != "Launch failed" || last.Problem.Status != http.StatusInternalServerError ||
		!strings.Contains(last.Problem.Detail, "internal error (reference ") || strings.Contains(last.Problem.Detail, "SDK") {
		t.Errorf("the stream ended with %+v, want a 500 with the fixed message", last.Problem)
	}
	if sess := f.session(id); sess.State != store.StateFailed || !strings.Contains(sess.Error, "internal error (reference ") {
		t.Errorf("the row is %s with error %q, want failed with the fixed message", sess.State, sess.Error)
	}
}

// TestAPanicInALaunchIsLoggedAndTheServerGoesOn: the launch runs on a
// goroutine the request started, which nothing else recovers, so one
// panic would end the process for every user. The launch ends as a
// failed one does: its stream ends and its row is failed, so it holds
// no quota place.
//
// @decision(DL-135)
func TestAPanicInALaunchIsLoggedAndTheServerGoesOn(t *testing.T) {
	w := &warnings{}
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Warn = w.warn
		d.Launch = func(context.Context, session.Options, session.StepReporter) (*session.Session, error) {
			panic("a pre-1.0 SDK said no")
		}
	})
	resp, body := f.do(http.MethodPost, "/api/v1/sessions", repoReq("alpha"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d: %s", resp.StatusCode, body)
	}
	var view SessionView
	_ = json.Unmarshal(body, &view)
	w.waitFor(t, "recovered from a panic", "a pre-1.0 SDK said no")
	launchEndsInPanic(t, f, view.ID)
	if resp, _ := f.do(http.MethodGet, "/api/v1/sessions", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("the server did not answer after the panic: %d", resp.StatusCode)
	}
}

// TestAPanicInAResumeEndsItAsAFailedLaunch: a resume's launch is
// recovered the same way.
//
// @decision(DL-135) @decision(DL-087)
func TestAPanicInAResumeEndsItAsAFailedLaunch(t *testing.T) {
	w := &warnings{}
	var panicking atomic.Bool
	f := resumable(t, func(d *Deps) {
		d.Snapshots = nil
		d.Warn = w.warn
		launch := d.Launch
		d.Launch = func(ctx context.Context, opts session.Options, steps session.StepReporter) (*session.Session, error) {
			if panicking.Load() {
				panic("a resume's SDK said no")
			}
			return launch(ctx, opts, steps)
		}
	})
	panicking.Store(true)
	if code, _, body := f.resume(map[string]any{}); code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", code, body)
	}
	w.waitFor(t, "recovered from a panic", "a resume's SDK said no")
	launchEndsInPanic(t, f.apiFixture, f.id)
}
