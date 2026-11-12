package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/session"
)

// TestAPanicInALaunchIsLoggedAndTheServerGoesOn: the launch runs on a
// goroutine the request started, which nothing else recovers, so one
// panic would end the process for every user.
//
// @decision(DL-135)
func TestAPanicInALaunchIsLoggedAndTheServerGoesOn(t *testing.T) {
	var (
		mu     sync.Mutex
		warned []string
	)
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Warn = func(s string) {
			mu.Lock()
			warned = append(warned, s)
			mu.Unlock()
		}
		d.Launch = func(context.Context, session.Options, session.StepReporter) (*session.Session, error) {
			panic("a pre-1.0 SDK said no")
		}
	})
	resp, body := f.do(http.MethodPost, "/api/v1/sessions", repoReq("alpha"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d: %s", resp.StatusCode, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := strings.Join(warned, "\n")
		mu.Unlock()
		if strings.Contains(got, "recovered from a panic") && strings.Contains(got, "a pre-1.0 SDK said no") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the panic was not logged: %q", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp, _ := f.do(http.MethodGet, "/api/v1/sessions", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("the server did not answer after the panic: %d", resp.StatusCode)
	}
}
