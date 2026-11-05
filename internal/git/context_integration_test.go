//go:build integration

package git

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestIntegrationForgejoContextCancels proves a per-call context actually
// interrupts the HTTP request, rather than only being checked between
// pages.
func TestIntegrationForgejoContextCancels(t *testing.T) {
	f := integrationForgejo(t)

	// A deadline far too short for a round trip.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	_, err := f.ListRepos(ctx)
	if err == nil {
		t.Fatal("a request with an expired context succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}

	// And a fresh context still works, so the client was not poisoned.
	ok, cancelOK := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelOK()
	if _, err := f.ListRepos(ok); err != nil {
		t.Errorf("a later request failed after a cancelled one: %v", err)
	}
}
