package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// unexpectedStore is a store whose session listing fails the way a
// database driver does, with the connection's details in the text.
type unexpectedStore struct{ store.Store }

func (u unexpectedStore) Sessions() store.SessionStore { return unexpectedSessions{u.Store.Sessions()} }

type unexpectedSessions struct{ store.SessionStore }

func (unexpectedSessions) List(context.Context, string, store.SessionFilter) ([]store.Session, error) {
	return nil, fmt.Errorf("store: list sessions: %w", errors.New("dial tcp db.internal.example:5432: connection refused for user spov_app"))
}

// TestAnUnexpectedStoreErrorIsNotShownToTheClient: the text of a
// database failure names its host and user, so the client gets a fixed
// 500 and the log gets the detail.
//
// @decision(DL-135)
func TestAnUnexpectedStoreErrorIsNotShownToTheClient(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.deps.Store = unexpectedStore{f.store}
	f.srv.svc.deps.Store = unexpectedStore{f.store}
	var logged strings.Builder
	was := internalLog
	internalLog = log.New(&logged, "", 0)
	t.Cleanup(func() { internalLog = was })

	resp, body := f.do(http.MethodGet, "/api/v1/sessions", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a store failure answered %d, want 500: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "db.internal.example") || strings.Contains(string(body), "spov_app") {
		t.Errorf("the response carries the database's details: %s", body)
	}
	if !strings.Contains(string(body), "reference") {
		t.Errorf("the response has no reference to find the log line by: %s", body)
	}
	if !strings.Contains(logged.String(), "db.internal.example") {
		t.Errorf("the detail was not logged: %q", logged.String())
	}
}

// TestADeliberateStoreErrorKeepsItsMessage: a sentinel the store raises
// on purpose, and any error that is not the store's, are still shown.
//
// @decision(DL-135)
func TestADeliberateStoreErrorKeepsItsMessage(t *testing.T) {
	for _, err := range []error{
		store.ErrConflict,
		fmt.Errorf("creating: %w", store.ErrInvalidJob),
		errors.New("modal: image not found"),
	} {
		if unexpectedStoreError(err) {
			t.Errorf("%v was treated as an unexpected store error", err)
		}
	}
	if got := toHumaError(errors.New("modal: image not found")); !strings.Contains(got.Error(), "image not found") {
		t.Errorf("a provider error lost its message: %v", got)
	}
	if !unexpectedStoreError(fmt.Errorf("listing: %w", fmt.Errorf("store: begin: %w", errors.New("x")))) {
		t.Error("a wrapped store error was not recognised")
	}
}
