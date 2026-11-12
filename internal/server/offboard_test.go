package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs/jobstest"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

const (
	theirID    = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	theirEmail = "them@example.com"
)

// offboarded is what an off-boarding test sets up for them@example.com,
// beside one running session of the admin's own.
type offboarded struct {
	f *apiFixture
	// interactive is a running session with no job; heldSession is a
	// held sync job's; jobSession is a running job's; queuedSession is
	// a queued job's, starting with no sandbox; past is stopped history.
	interactive, jobSession, queuedSession, heldSession, past store.Session
	running, queued, held                                     store.Job
	mine                                                      store.Session
}

// failingStore makes every credential delete fail while fail is set,
// inside a transaction as well. The failure reads as a real store's
// does, so the off-boarding's own 500 is shown to be kept apart from the
// fixed answer an unexpected store error gets (DL-135).
type failingStore struct {
	store.Store
	fail *atomic.Bool
}

type failingCredentials struct {
	store.CredentialStore
	fail *atomic.Bool
}

func (f failingCredentials) Delete(ctx context.Context, orgID, userID, platform string) error {
	if f.fail.Load() {
		return errors.New("store: delete credential: dial tcp db.internal:5432: connection refused")
	}
	return f.CredentialStore.Delete(ctx, orgID, userID, platform)
}

func (f failingStore) Credentials() store.CredentialStore {
	return failingCredentials{CredentialStore: f.Store.Credentials(), fail: f.fail}
}

func (f failingStore) Tx(ctx context.Context, fn func(store.Store) error) error {
	return f.Store.Tx(ctx, func(tx store.Store) error { return fn(failingStore{Store: tx, fail: f.fail}) })
}

func newOffboarded(t *testing.T, adjust func(*Deps)) *offboarded {
	t.Helper()
	f := allowlistFixtureWith(t, true, map[string]bool{"me@example.com": true, theirEmail: false}, adjust)
	ctx := context.Background()
	if err := f.store.Users().Create(ctx, store.User{ID: theirID, OrgID: auth.LocalID, Email: theirEmail}); err != nil {
		t.Fatal(err)
	}
	o := &offboarded{f: f}
	session := func(owner, name string, state store.SessionState, withSandbox bool) store.Session {
		t.Helper()
		sess := store.Session{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: owner, Name: name, State: state}
		if withSandbox {
			h, err := f.provider.Create(ctx, sandbox.Spec{Name: name, Image: "i", Command: []string{"x"},
				CPU: 0.5, MemoryMiB: 1024, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
				Tags:   sandbox.BuildTags(sandbox.TagSet{Name: name, User: owner, CreatedAt: time.Now()}),
				Egress: sandbox.EgressOpen()})
			if err != nil {
				t.Fatal(err)
			}
			info, _ := h.Info(ctx)
			sess.SandboxID = info.ID
		}
		if err := f.store.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		return sess
	}
	job := func(sess store.Session, kind store.JobKind, state store.JobState) store.Job {
		t.Helper()
		j := store.Job{ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: sess.OwnerID, SessionID: sess.ID,
			Kind: kind, State: store.JobQueued, Goal: "work"}
		if err := f.store.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		for _, st := range jobstest.PathTo(state) {
			j.State = st
			if err := f.store.Jobs().Update(ctx, j); err != nil {
				t.Fatal(err)
			}
		}
		return j
	}
	o.interactive = session(theirID, "theirs", store.StateRunning, true)
	o.jobSession = session(theirID, "job-running", store.StateRunning, true)
	o.running = job(o.jobSession, store.JobKindReport, store.JobRunning)
	o.queuedSession = session(theirID, "job-queued", store.StateStarting, false)
	o.queued = job(o.queuedSession, store.JobKindReport, store.JobQueued)
	o.heldSession = session(theirID, "job-held", store.StateRunning, true)
	o.held = job(o.heldSession, store.JobKindSync, store.JobSucceeded)
	o.past = session(theirID, "old", store.StateStopped, false)
	o.mine = session(auth.LocalID, "mine", store.StateRunning, true)

	now := time.Now()
	for i, revoked := range []*time.Time{nil, &now} {
		if err := f.store.Tokens().Create(ctx, store.APIToken{ID: store.NewID(now), OrgID: auth.LocalID, UserID: theirID,
			Hash: []byte{byte(i)}, Name: "cli", RevokedAt: revoked}); err != nil {
			t.Fatal(err)
		}
	}
	for _, platform := range []string{"github", "route:npm", "opencode:grant"} {
		if err := f.store.Credentials().Put(ctx, store.UserCredential{ID: store.NewID(now), OrgID: auth.LocalID,
			UserID: theirID, Platform: platform, Ciphertext: []byte("sealed"), Nonce: []byte("n")}); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func (o *offboarded) session(t *testing.T, sess store.Session) store.Session {
	t.Helper()
	got, err := o.f.store.Sessions().Get(context.Background(), sess.OrgID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (o *offboarded) job(t *testing.T, j store.Job) store.Job {
	t.Helper()
	got, err := o.f.store.Jobs().Get(context.Background(), j.OrgID, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (o *offboarded) offboardEvents(t *testing.T) []store.AdminEvent {
	t.Helper()
	evs, err := o.f.store.AdminEvents().List(context.Background(), auth.LocalID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AdminEvent
	for _, e := range evs {
		if e.Action == AdminMemberOffboarded {
			out = append(out, e)
		}
	}
	return out
}

// assertEnded checks that nothing of theirs runs, can sign in again or
// is still stored, and that their history and the admin's own session
// are untouched.
func (o *offboarded) assertEnded(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, sess := range []store.Session{o.interactive, o.jobSession, o.queuedSession, o.heldSession} {
		if got := o.session(t, sess); got.State.Live() {
			t.Errorf("their session %s is still %s", sess.Name, got.State)
		}
	}
	for id, h := range o.f.provider.Handles() {
		info, _ := h.Info(ctx)
		if info.State == sandbox.StateRunning && id != o.mine.SandboxID {
			t.Errorf("their sandbox %s still runs", id)
		}
	}
	if o.f.provider.Running() != 1 {
		t.Errorf("%d sandboxes run, want only the admin's own", o.f.provider.Running())
	}
	if got := o.session(t, o.mine); got.State != store.StateRunning {
		t.Errorf("the admin's own session is %s", got.State)
	}
	if got := o.session(t, o.past); got.State != store.StateStopped {
		t.Errorf("their past session is %s, want it kept as it was", got.State)
	}
	for _, j := range []store.Job{o.running, o.queued} {
		if got := o.job(t, j); got.State != store.JobCancelled || !strings.Contains(got.Error, auth.LocalID) {
			t.Errorf("job %s is %s (%q), want cancelled by the admin", j.ID, got.State, got.Error)
		}
	}
	if got := o.job(t, o.held); got.State != store.JobSucceeded {
		t.Errorf("the held sync job is %s, want its outcome kept", got.State)
	}
	tokens, err := o.f.store.Tokens().ListByUser(ctx, auth.LocalID, theirID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokens {
		if tok.RevokedAt == nil {
			t.Errorf("token %s has no revoked_at", tok.ID)
		}
	}
	if creds, err := o.f.store.Credentials().ListByUser(ctx, auth.LocalID, theirID); err != nil || len(creds) != 0 {
		t.Errorf("their credentials = %+v, %v, want none", creds, err)
	}
	if _, err := o.f.store.Users().Get(ctx, auth.LocalID, theirID); err != nil {
		t.Errorf("their user row went: %v", err)
	}
}

// The PoC of the review's A-02, with the opposite assertions: removing a
// person from the allowlist ends their sandboxes and jobs, revokes their
// CLI tokens, deletes their credentials and audits it.
//
// @decision(DL-132)
func TestRemovingAPersonEndsWhatTheyHaveRunningAndStored(t *testing.T) {
	o := newOffboarded(t, nil)
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	o.assertEnded(t)

	evs := o.offboardEvents(t)
	if len(evs) != 1 {
		t.Fatalf("audit rows = %+v, want one", evs)
	}
	want := theirEmail + ": 2 jobs cancelled, 2 sessions stopped, 1 CLI token revoked, 3 credentials deleted"
	if evs[0].Detail != want || evs[0].Actor != "me@example.com" {
		t.Errorf("audit row = %+v, want %q by the admin", evs[0], want)
	}
	if strings.Contains(evs[0].Detail, "sealed") {
		t.Error("the audit row carries a credential")
	}

	// Re-adding the address brings back none of the tokens.
	if resp, out := o.f.do(http.MethodPut, "/api/v1/allowlist/"+theirEmail, map[string]any{"admin": false}); resp.StatusCode != http.StatusOK {
		t.Fatalf("re-add = %d: %s", resp.StatusCode, out)
	}
	tokens, _ := o.f.store.Tokens().ListByUser(context.Background(), auth.LocalID, theirID)
	for _, tok := range tokens {
		if tok.Usable(time.Now()) {
			t.Errorf("token %s works again once the address is re-added", tok.ID)
		}
	}
}

// A step that fails leaves the others to run, and the answer says the
// address is removed, what is left and that the same delete retries,
// which then finishes the job and answers 204.
//
// @decision(DL-132)
func TestAFailedOffboardingStepStillRunsTheOthersAndIsRetried(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	o := newOffboarded(t, func(d *Deps) { d.Store = failingStore{Store: d.Store, fail: &fail} })

	resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("DELETE with a failing step = %d: %s", resp.StatusCode, out)
	}
	for _, want := range []string{"is removed from the allowlist", "3 stored credentials not deleted", "send the same delete again"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the answer does not say %q: %s", want, out)
		}
	}
	// @decision(DL-135)
	if strings.Contains(string(out), "db.internal") || strings.Contains(string(out), "internal error (reference") {
		t.Errorf("the answer carries the store's detail or the generic 500 in place of the off-boarding's: %s", out)
	}
	if _, err := o.f.store.Allowlist().Get(context.Background(), auth.LocalID, theirEmail); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the entry survived a failed step: %v", err)
	}
	// Everything but the credentials was still done.
	if got := o.session(t, o.heldSession); got.State.Live() {
		t.Error("a failed credential step kept the sessions alive")
	}
	tokens, _ := o.f.store.Tokens().ListByUser(context.Background(), auth.LocalID, theirID)
	for _, tok := range tokens {
		if tok.RevokedAt == nil {
			t.Error("a failed credential step left a token unrevoked")
		}
	}
	if evs := o.offboardEvents(t); len(evs) != 1 || !strings.Contains(evs[0].Detail, "left: 3 stored credentials not deleted") {
		t.Errorf("audit rows = %+v, want one naming what is left", evs)
	}

	fail.Store(false)
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the retry = %d: %s", resp.StatusCode, out)
	}
	o.assertEnded(t)
	evs := o.offboardEvents(t)
	if len(evs) != 2 || !strings.Contains(evs[0].Detail, "3 credentials deleted") {
		t.Errorf("audit rows after the retry = %+v", evs)
	}
}

// A delete of an address already off the allowlist, but whose person
// signed in, runs the off-boarding again; one who never did is a 404.
//
// @decision(DL-132)
func TestDeletingARemovedAddressOffboardsItAgain(t *testing.T) {
	o := newOffboarded(t, nil)
	if err := o.f.store.Allowlist().Delete(context.Background(), auth.LocalID, theirEmail); err != nil {
		t.Fatal(err)
	}
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE of a removed address with a user row = %d: %s", resp.StatusCode, out)
	}
	o.assertEnded(t)
	if resp, _ := o.f.do(http.MethodDelete, "/api/v1/allowlist/nobody@example.com", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE of an address with no entry and no user = %d, want 404", resp.StatusCode)
	}
}

// A held sync job's session is live with a finished job, which an
// ordinary stop refuses; a removal ends it all the same.
//
// @decision(DL-132)
func TestRemovingAPersonEndsTheirHeldSyncJobsSandbox(t *testing.T) {
	o := newOffboarded(t, nil)
	if !store.SyncHeld(o.held, o.heldSession) {
		t.Fatal("the fixture's sync job is not held, so this proves nothing")
	}
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	if got := o.session(t, o.heldSession); got.State.Live() {
		t.Errorf("the held session is %s", got.State)
	}
	h := o.f.provider.Handles()[o.heldSession.SandboxID]
	if info, _ := h.Info(context.Background()); info.State == sandbox.StateRunning {
		t.Error("the held sync job's sandbox still runs")
	}
}

// An address nobody signed in under has nothing to end: only the entry
// goes, and nothing is audited.
//
// @decision(DL-132)
func TestRemovingAnAddressNobodySignedInUnderRemovesOnlyTheEntry(t *testing.T) {
	f := allowlistFixture(t, true, map[string]bool{"me@example.com": true, "new@example.com": false})
	if resp, out := f.do(http.MethodDelete, "/api/v1/allowlist/new@example.com", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	if evs, _ := f.store.AdminEvents().List(context.Background(), auth.LocalID, 10); len(evs) != 0 {
		t.Errorf("audit rows = %+v, want none", evs)
	}
}

// A removal forgets the person's cached repository listings with their
// credentials, as a credential delete through the API does.
//
// @decision(DL-132) @decision(DL-134)
func TestRemovingAPersonForgetsTheirRepositoryListings(t *testing.T) {
	o := newOffboarded(t, nil)
	cache := o.f.srv.svc.repos
	theirs := repoKey{org: auth.LocalID, user: theirID, tier: "user"}
	mine := repoKey{org: auth.LocalID, user: auth.LocalID, tier: "user"}
	cache.put(theirs, repoEntry{})
	cache.put(mine, repoEntry{})
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	if _, ok := cache.get(theirs); ok {
		t.Error("their cached listing outlived the removal")
	}
	if _, ok := cache.get(mine); !ok {
		t.Error("the removal dropped another user's listing")
	}
}

// TestOffboardingDeletesTheOpenCodeSignInUnderItsLock is DL-132: the
// console rows go inside the sign-in's own lock, so a refresh that held
// it when the removal began cannot write the grant back afterwards.
//
// @decision(DL-132) @decision(DL-015)
func TestOffboardingDeletesTheOpenCodeSignInUnderItsLock(t *testing.T) {
	grants := &MemoryConsoleStore{}
	o := newOffboarded(t, func(d *Deps) { d.ConsoleGrants = grants })
	ctx := context.Background()
	if err := grants.Put(ctx, auth.LocalID, theirID, ConsoleSlotAttempt, "pending"); err != nil {
		t.Fatal(err)
	}
	holding, refreshed := make(chan struct{}), make(chan error, 1)
	go func() {
		refreshed <- grants.Locked(ctx, auth.LocalID, theirID, func(cs ConsoleStore) error {
			close(holding)
			// The console answers while the removal waits.
			time.Sleep(50 * time.Millisecond)
			return cs.Put(ctx, auth.LocalID, theirID, ConsoleSlotGrant, "a refreshed grant")
		})
	}()
	<-holding
	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	for _, slot := range []string{ConsoleSlotGrant, ConsoleSlotAttempt} {
		if v, _ := grants.Get(ctx, auth.LocalID, theirID, slot); v != "" {
			t.Errorf("their OpenCode %s outlived the removal: %q", slot, v)
		}
	}
}

// TestOffboardingDeletesBrowserStoredCredentialsAndKeepsTemplates: a
// routed credential the person stored from the browser, a preset's as
// the form sends it, is a row of CredentialStore.ListByUser and is
// deleted with the rest. Their sandbox templates hold no secret, only
// settings and credential names, so they are kept as history is, and
// one that names the deleted credential is refused at launch.
//
// @decision(DL-132) @decision(DL-118) @decision(DL-119)
func TestOffboardingDeletesBrowserStoredCredentialsAndKeepsTemplates(t *testing.T) {
	templates := &store.FileTemplates{}
	o := newOffboarded(t, func(d *Deps) {
		d.RoutedCredentials = &SealedRoutedCredentials{Store: d.Store.Credentials(), Sealer: testSealer(t)}
		d.Templates = templates
	})
	admin := o.f.auth.inner
	o.f.auth.inner = auth.StaticAuthenticator{Principal: auth.Principal{UserID: theirID, OrgID: auth.LocalID, Email: theirEmail}}
	for path, body := range map[string]any{
		"/api/v1/me/routed-credentials/team-npm": map[string]any{"secret": "npm_THEIR_SECRET", "route": map[string]any{
			"name": "team-npm", "preset": "npm", "url": "https://art.acme.com/api/npm/team-a/", "scope": "@acme"}},
		"/api/v1/me/templates/work": map[string]any{"description": "their work", "credentials": []string{"team-npm"},
			"env": map[string]string{"STAGE": "dev"}},
	} {
		if resp, out := o.f.do(http.MethodPut, path, body); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s as them = %d: %s", path, resp.StatusCode, out)
		}
	}
	o.f.auth.inner = admin

	if resp, out := o.f.do(http.MethodDelete, "/api/v1/allowlist/"+theirEmail, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", resp.StatusCode, out)
	}
	o.assertEnded(t)
	if evs := o.offboardEvents(t); len(evs) != 1 || !strings.Contains(evs[0].Detail, "4 credentials deleted") {
		t.Errorf("audit rows = %+v, want the browser's routed credential counted with the other three", evs)
	}
	kept, err := templates.Get(context.Background(), auth.LocalID, theirID, "work")
	if err != nil || kept.Description != "their work" {
		t.Errorf("their template = %+v, %v, want it kept", kept, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", kept), "npm_THEIR_SECRET") {
		t.Error("the template holds the credential's secret")
	}
	missing := missingCredentials(kept, nil)
	if len(missing) != 1 || missing[0] != "team-npm" {
		t.Errorf("the kept template's missing credentials = %v, want the deleted one", missing)
	}
}
