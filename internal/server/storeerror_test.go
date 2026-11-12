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

func (u unexpectedStore) Sessions() store.SessionStore {
	return unexpectedSessions{SessionStore: u.Store.Sessions()}
}

type unexpectedSessions struct {
	store.SessionStore
	timeout bool
}

func (u unexpectedSessions) List(context.Context, string, store.SessionFilter) ([]store.Session, error) {
	if u.timeout {
		return nil, fmt.Errorf("store: list sessions: failed to connect to `user=spov_app database=spov`: db.internal.example:5432: %w", context.DeadlineExceeded)
	}
	return nil, fmt.Errorf("store: list sessions: %w", errors.New("dial tcp db.internal.example:5432: connection refused for user spov_app"))
}

// timedOutStore is unexpectedStore whose listing times out, as a pool
// acquire or a dial does.
type timedOutStore struct{ store.Store }

func (u timedOutStore) Sessions() store.SessionStore {
	return unexpectedSessions{SessionStore: u.Store.Sessions(), timeout: true}
}

// TestATimedOutStoreCallIsNotShownToTheClient: a store call that ran out
// of time is still a 504, with the fixed message and a reference, on
// the API and in a stream's problem.
//
// @decision(DL-135)
func TestATimedOutStoreCallIsNotShownToTheClient(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.deps.Store = timedOutStore{f.store}
	f.srv.svc.deps.Store = timedOutStore{f.store}
	var logged strings.Builder
	was := internalLog
	internalLog = log.New(&logged, "", 0)
	t.Cleanup(func() { internalLog = was })

	resp, body := f.do(http.MethodGet, "/api/v1/sessions", nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("a timed-out store call answered %d, want 504: %s", resp.StatusCode, body)
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
	_, err := timedOutStore{f.store}.Sessions().List(context.Background(), "", store.SessionFilter{})
	if p := problemOf(err); p.Status != http.StatusGatewayTimeout || strings.Contains(p.Detail, "db.internal.example") || !strings.Contains(p.Detail, "reference") {
		t.Errorf("a stream's problem = %+v, want a 504 with the fixed message", p)
	}
	if got := toHumaError(fmt.Errorf("sandbox: exec: %w", context.DeadlineExceeded)); !strings.Contains(got.Error(), "sandbox: exec") {
		t.Errorf("a timeout that is not the store's lost its message: %v", got)
	}
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

// TestALaunchsStoreFailureIsStoredAsTheFixedMessage: the session row's
// error reaches its views, spov ls and the launch stream, so a store
// failure during a launch is stored and streamed as the fixed message,
// and the detail goes to the log.
//
// @decision(DL-135)
func TestALaunchsStoreFailureIsStoredAsTheFixedMessage(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.UserProviderKeys = func(context.Context, string, string) (map[string]string, error) {
			return nil, fmt.Errorf("store: read credentials: %w", errors.New("dial tcp db.internal.example:5432: connection refused for user spov_app"))
		}
	})
	var logged strings.Builder
	was := internalLog
	internalLog = log.New(&logged, "", 0)
	t.Cleanup(func() { internalLog = was })

	id := f.create(repoReq("alpha"))
	evs := f.events(id)
	last := evs[len(evs)-1]
	if last.Name != "error" || strings.Contains(string(last.Data), "db.internal.example") || !strings.Contains(string(last.Data), "reference") {
		t.Errorf("the launch stream ended with %s %s, want a failure with the fixed message", last.Name, last.Data)
	}
	sess := f.session(id)
	if sess.State != store.StateFailed || strings.Contains(sess.Error, "db.internal.example") || !strings.Contains(sess.Error, "reference") {
		t.Errorf("the row is %s with error %q, want failed with the fixed message", sess.State, sess.Error)
	}
	if !strings.Contains(logged.String(), "db.internal.example") {
		t.Errorf("the detail was not logged: %q", logged.String())
	}
}

// TestAJobsProvisioningStoreFailureIsHidden: what ProvisionJob returns
// becomes the job's error, so a store failure's text is the fixed
// message while the error still unwraps to its cause.
//
// @decision(DL-135)
func TestAJobsProvisioningStoreFailureIsHidden(t *testing.T) {
	var logged strings.Builder
	was := internalLog
	internalLog = log.New(&logged, "", 0)
	t.Cleanup(func() { internalLog = was })
	cause := fmt.Errorf("store: get session: %w", context.DeadlineExceeded)
	err := hideStoreError(fmt.Errorf("reading the job's session: %w", cause))
	if strings.Contains(err.Error(), "get session") || !strings.Contains(err.Error(), "reference") {
		t.Errorf("the error reads %q, want the fixed message", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the hidden error no longer unwraps to its cause")
	}
	plain := errors.New("the session failed without starting")
	if got := hideStoreError(plain); got.Error() != plain.Error() || !errors.Is(got, plain) {
		t.Errorf("an error that is not the store's was changed: %v", got)
	}
	if hideStoreError(nil) != nil {
		t.Errorf("no error became one")
	}
	if !strings.Contains(logged.String(), "get session") {
		t.Errorf("the detail was not logged: %q", logged.String())
	}
}
