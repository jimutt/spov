package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// otherModel is a second reference under the SAME provider as testModel,
// so a test about precedence needs one key rather than two and cannot
// pass by accident on a key that is missing.
const otherModel = "anthropic/claude-opus-5"

// profileWith puts a user row in place before the server is built, the
// way the GitHub login path would have.
func profileWith(t *testing.T, u store.User) func(*Deps) {
	t.Helper()
	return func(d *Deps) {
		u.ID, u.OrgID = auth.LocalID, auth.LocalID
		if err := d.Store.Users().Create(context.Background(), u); err != nil {
			t.Fatal(err)
		}
		testModelKeys(d)
	}
}

// storedSessionModel reads back the model the create actually recorded.
// The row is the point: a launch reads the SESSION, so a resolution the
// row does not carry is a sandbox running on something the row denies.
func createNamed(t *testing.T, f *apiFixture, name, model string) (result, []byte) {
	t.Helper()
	return f.do(http.MethodPost, "/api/v1/sessions", SessionRequest{
		Name:  name,
		Repos: []RepoRef{{Platform: "fake", Owner: "acme", Name: "app"}},
		Model: model,
	})
}

func storedSessionModel(t *testing.T, f *apiFixture, body []byte) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	return sess.Request.Model
}

// A default is stored and reported back, and survives a re-read rather
// than only being echoed.
//
// @decision(DL-014)
func TestADefaultModelIsStoredAndReported(t *testing.T) {
	f := newAPIFixtureWith(t, profileWith(t, store.User{Email: "me@example.com"}))

	resp, body := f.do(http.MethodPatch, "/api/v1/me", map[string]string{"default_model": testModel})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, body)
	}
	var me MeView
	if err := json.Unmarshal(body, &me); err != nil {
		t.Fatal(err)
	}
	if me.DefaultModel != testModel {
		t.Errorf("the answer does not carry the default: %+v", me)
	}
	_, body = f.do(http.MethodGet, "/api/v1/me", nil)
	if !strings.Contains(string(body), testModel) {
		t.Errorf("the default did not persist: %s", body)
	}
}

// Clearing is how a user goes back to OpenCode's own default, so an
// empty value must be distinguishable from an omitted one.
//
// @decision(DL-014)
func TestClearingTheDefaultRestoresOpenCodesOwn(t *testing.T) {
	f := newAPIFixtureWith(t, profileWith(t, store.User{DefaultModel: testModel}))

	if resp, body := f.do(http.MethodPatch, "/api/v1/me",
		map[string]string{"git_name": "Alice"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("an unrelated patch: %d %s", resp.StatusCode, body)
	}
	if resp, body := createNamed(t, f, "before", ""); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a launch under the default: %d %s", resp.StatusCode, body)
	} else if got := storedSessionModel(t, f, body); got != testModel {
		t.Fatalf("an omitted default_model cleared the setting: stored %q", got)
	}

	if resp, body := f.do(http.MethodPatch, "/api/v1/me",
		map[string]string{"default_model": ""}); resp.StatusCode != http.StatusOK {
		t.Fatalf("clearing: %d %s", resp.StatusCode, body)
	}
	resp, body := createNamed(t, f, "after", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a launch after clearing: %d %s", resp.StatusCode, body)
	}
	if got := storedSessionModel(t, f, body); got != "" {
		t.Errorf("a cleared default still chose %q; clearing is the only way back to"+
			" OpenCode's own, so it has to work", got)
	}
}

// The endpoint creates the caller's row when there is none (DL-014).
//
// @decision(DL-014)
func TestPatchMeCreatesTheProfileWhenThereIsNone(t *testing.T) {
	f := modelFixture(t) // no user row at all
	ctx := context.Background()
	if _, err := f.store.Users().Get(ctx, auth.LocalID, auth.LocalID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the fixture already has a profile (%v), so this test proves nothing", err)
	}

	resp, body := f.do(http.MethodPatch, "/api/v1/me", map[string]string{
		"default_model": testModel, "git_name": "Alice",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH on a profile-less caller = %d: %s", resp.StatusCode, body)
	}
	u, err := f.store.Users().Get(ctx, auth.LocalID, auth.LocalID)
	if err != nil {
		t.Fatalf("no row was created: %v", err)
	}
	if u.DefaultModel != testModel {
		t.Errorf("the created row lost the default: %+v", u)
	}
	// The git identity is settable in local mode for the same reason.
	if u.GitName != "Alice" {
		t.Errorf("the created row lost the git identity: %+v", u)
	}
}

// A setting that cannot work is refused where it is chosen. The
// alternative is a 422 at some later launch, about a model the caller
// did not name and may not remember choosing.
//
// @decision(DL-014)
func TestAnUnusableDefaultIsRefusedWhereItIsChosen(t *testing.T) {
	f := modelFixture(t)
	resp, body := f.do(http.MethodPatch, "/api/v1/me",
		map[string]string{"default_model": retiredModel})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("storing a retired model answered %d, want 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "retired") {
		t.Errorf("the refusal does not say what is wrong: %s", body)
	}
	// Its own sentence, not a launch's. The check is shared; the
	// wording around it is not, and a save told "cannot be run" reads
	// as though something had already been launched.
	if !strings.Contains(string(body), "cannot be your default model") {
		t.Errorf("a settings refusal does not read as one: %s", body)
	}
	if strings.Contains(string(body), "on this launch") {
		t.Errorf("a settings refusal advises about a launch: %s", body)
	}
	if _, err := f.store.Users().Get(context.Background(), auth.LocalID, auth.LocalID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused save wrote a profile row anyway: %v", err)
	}
}

// A launch that names no model runs on the stored default, on both
// paths, and the rows say so.
//
// @decision(DL-014)
func TestALaunchWithNoModelRunsOnTheStoredDefault(t *testing.T) {
	f, q := jobFixtureWith(t, profileWith(t, store.User{DefaultModel: testModel}))

	resp, body := createSessionWith(t, f, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("session create: %d %s", resp.StatusCode, body)
	}
	if got := storedSessionModel(t, f, body); got != testModel {
		t.Errorf("the session ran on %q, want the stored default %q", got, testModel)
	}

	resp, body = f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("job create: %d %s", resp.StatusCode, body)
	}
	var view JobView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Model != testModel {
		t.Errorf("the job view lost the default: %q", view.Model)
	}
	job, err := f.store.Jobs().Get(context.Background(), auth.LocalID, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Model != testModel {
		t.Errorf("the job row ran on %q, want %q", job.Model, testModel)
	}
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Request.Model != testModel {
		t.Errorf("the job's session request ran on %q, want %q", sess.Request.Model, testModel)
	}
	if len(q.args) != 1 {
		t.Errorf("%d jobs reached the queue", len(q.args))
	}
}

// A per-launch model is the more specific instruction and the more
// recent one. Anything else makes --model unreliable, which is worse
// than not having it.
//
// @decision(DL-014)
func TestTheRequestBeatsTheStoredDefault(t *testing.T) {
	f := newAPIFixtureWith(t, profileWith(t, store.User{DefaultModel: testModel}))
	resp, body := createSessionWith(t, f, otherModel)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if got := storedSessionModel(t, f, body); got != otherModel {
		t.Errorf("the stored default overrode the request: got %q, want %q", got, otherModel)
	}
}

// A default validated at write can go stale. The refusal must say which
// rung supplied the model and where to change it, since the caller
// named nothing and the setting is reachable only from a browser.
//
// @decision(DL-014)
func TestAStaleDefaultIsRefusedAndSaysWhereItCameFrom(t *testing.T) {
	// The row is written directly, as one stored before the key was
	// removed would be.
	f := newAPIFixtureWith(t, func(d *Deps) {
		if err := d.Store.Users().Create(context.Background(), store.User{
			ID: auth.LocalID, OrgID: auth.LocalID, DefaultModel: testModel,
		}); err != nil {
			t.Fatal(err)
		}
	})
	resp, body := createSessionWith(t, f, "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a stale default answered %d, want 422: %s", resp.StatusCode, body)
	}
	if n := len(f.provider.Specs()); n != 0 {
		t.Errorf("%d sandboxes were created for a stale default", n)
	}
	if !strings.Contains(string(body), "your default model") {
		t.Errorf("the refusal does not say the model came from the caller's settings,"+
			" so it reads as a spov bug: %s", body)
	}
	if !strings.Contains(string(body), "Settings") {
		t.Errorf("the refusal does not say where the fix is: %s", body)
	}

	// And the other direction: a model the caller DID name must not be
	// reported as their stored default. One wording covering both tells
	// the reader neither, exactly as the two DL-013 refusals do.
	_, named := createSessionWith(t, f, "anthropic/claude-sonnet-6")
	if strings.Contains(string(named), "your default model") {
		t.Errorf("a model the caller typed was blamed on their settings: %s", named)
	}
}

// A caller with no profile row has no default and no warning. A bearer
// token in local mode legitimately has none, and getting this wrong
// fails every local-mode launch.
//
// @decision(DL-014)
func TestACallerWithNoProfileLaunchesOnOpenCodesOwn(t *testing.T) {
	var warnings []string
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Warn = func(s string) { warnings = append(warnings, s) }
	})
	resp, body := createSessionWith(t, f, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a profile-less caller was refused: %d %s", resp.StatusCode, body)
	}
	if got := storedSessionModel(t, f, body); got != "" {
		t.Errorf("a caller with no profile got model %q", got)
	}
	for _, w := range warnings {
		if strings.Contains(w, "profile") {
			t.Errorf("a missing profile was warned about, which is noise on every"+
				" local-mode launch: %s", w)
		}
	}
}

// countingStore counts the reads of a user row, so the one-read rule
// has something holding it.
type countingStore struct {
	store.Store
	gets *atomic.Int32
}

func (c countingStore) Users() store.UserStore {
	return countingUsers{c.Store.Users(), c.gets}
}

type countingUsers struct {
	store.UserStore
	gets *atomic.Int32
}

func (c countingUsers) Get(ctx context.Context, orgID, id string) (store.User, error) {
	c.gets.Add(1)
	return c.UserStore.Get(ctx, orgID, id)
}

// The quota override and the default model are on the same row, and a
// create must read it once for both.
//
// @decision(DL-014) @decision(DL-009)
func TestTheProfileIsReadOncePerCreate(t *testing.T) {
	var gets atomic.Int32
	// A job create, because nothing launches from it: the recording
	// queue holds the job, so the count is the request's own reads and
	// not a race with a launch goroutine. The session create resolves
	// through the same two calls.
	//
	// The supervisor is built before this adjust runs and keeps the
	// unwrapped store, which does not matter here: it never runs, and
	// both point at the same memory store.
	f, _ := jobFixtureWith(t, func(d *Deps) {
		if err := d.Store.Users().Create(context.Background(), store.User{
			ID: auth.LocalID, OrgID: auth.LocalID, DefaultModel: testModel,
		}); err != nil {
			t.Fatal(err)
		}
		testModelKeys(d)
		d.Store = countingStore{d.Store, &gets}
	})
	gets.Store(0)
	resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if n := gets.Load(); n != 1 {
		t.Errorf("a create read the profile %d times, want 1", n)
	}
}

// The resolved model is bounded, and the refusal does not echo it: the
// launch paths check the request's length before the profile supplies
// anything.
//
// @decision(DL-014)
func TestAnOverLongModelIsRefusedWithoutBeingEchoed(t *testing.T) {
	huge := "anthropic/" + strings.Repeat("x", MaxModelBytes)

	f := modelFixture(t)
	resp, body := f.do(http.MethodPatch, "/api/v1/me", map[string]string{"default_model": huge})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("storing an over-long model answered %d, want 422: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), strings.Repeat("x", 64)) {
		t.Errorf("the refusal echoes the value it refused for being too long: %s", body)
	}
	if !strings.Contains(string(body), "default model") {
		t.Errorf("the refusal does not say which value is too long: %s", body)
	}

	// And on a launch, where the value came from a row nothing else
	// bounds.
	launch := newAPIFixtureWith(t, func(d *Deps) {
		if err := d.Store.Users().Create(context.Background(), store.User{
			ID: auth.LocalID, OrgID: auth.LocalID, DefaultModel: huge,
		}); err != nil {
			t.Fatal(err)
		}
		testModelKeys(d)
	})
	resp, body = createSessionWith(t, launch, "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an over-long stored default answered %d, want 422: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), strings.Repeat("x", 64)) {
		t.Errorf("a launch echoes the over-long stored default: %s", body)
	}
	if n := len(launch.provider.Specs()); n != 0 {
		t.Errorf("%d sandboxes were created", n)
	}
}

// Whitespace around a stored reference is never meaningful, and the row
// is read straight out of the store rather than through withDefaults.
//
// @decision(DL-014)
func TestAStoredDefaultIsTrimmedBeforeItIsUsed(t *testing.T) {
	f := newAPIFixtureWith(t, profileWith(t, store.User{DefaultModel: "  " + testModel + "\n"}))
	resp, body := createSessionWith(t, f, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a padded default was refused: %d %s", resp.StatusCode, body)
	}
	if got := storedSessionModel(t, f, body); got != testModel {
		t.Errorf("the stored request carries %q, want %q", got, testModel)
	}
}

// racingUsers loses the create race exactly once: it writes the winner's
// row and reports the conflict this request would really have seen.
type racingUsers struct {
	store.UserStore
	winner store.User
	raced  bool
}

func (r *racingUsers) Create(ctx context.Context, u store.User) error {
	if r.raced {
		return r.UserStore.Create(ctx, u)
	}
	r.raced = true
	if err := r.UserStore.Create(ctx, r.winner); err != nil {
		return err
	}
	return store.ErrConflict
}

type racingStore struct {
	store.Store
	users *racingUsers
}

func (r racingStore) Users() store.UserStore { return r.users }

// Two first saves race; the loser must re-read and re-apply rather than
// write the struct it already built, or it silently drops whatever the
// winner set.
//
// @decision(DL-014)
func TestAFirstSaveThatLosesTheRaceDoesNotClobberTheWinner(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		testModelKeys(d)
		d.Store = racingStore{d.Store, &racingUsers{
			UserStore: d.Store.Users(),
			winner: store.User{
				ID: auth.LocalID, OrgID: auth.LocalID, GitName: "The Winner",
			},
		}}
	})
	resp, body := f.do(http.MethodPatch, "/api/v1/me", map[string]string{"default_model": testModel})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the losing save answered %d: %s", resp.StatusCode, body)
	}
	u, err := f.store.Users().Get(context.Background(), auth.LocalID, auth.LocalID)
	if err != nil {
		t.Fatal(err)
	}
	if u.DefaultModel != testModel {
		t.Errorf("the losing save did not land: %+v", u)
	}
	if u.GitName != "The Winner" {
		t.Errorf("the losing save overwrote a field it never named: %+v", u)
	}
}
