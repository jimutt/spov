package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// adminFixture has a non-admin caller and a second user to administer.
func adminFixture(t *testing.T, admin bool) *apiFixture {
	t.Helper()
	f := newAPIFixtureWith(t, func(d *Deps) {
		ctx := context.Background()
		for _, u := range []store.User{
			{ID: auth.LocalID, OrgID: auth.LocalID, Email: "me@example.com", Admin: admin},
			{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", OrgID: auth.LocalID, Email: "them@example.com"},
		} {
			if err := d.Store.Users().Create(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
	})
	f.auth.inner = auth.StaticAuthenticator{
		Principal: auth.Principal{UserID: auth.LocalID, OrgID: auth.LocalID, Admin: admin},
	}
	return f
}

// A quota is not a user's own to raise, or it would not be a limit.
func TestQuotaEditingNeedsAnAdmin(t *testing.T) {
	f := adminFixture(t, false)
	body := map[string]any{"quota": map[string]any{"max_concurrent_sessions": 99}}
	resp, out := f.do(http.MethodPatch, "/api/v1/users/01ARZ3NDEKTSV4RRFFQ69G5FAV", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PATCH as a non-admin = %d, want 403: %s", resp.StatusCode, out)
	}
	if resp, out := f.do(http.MethodGet, "/api/v1/users", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET as a non-admin = %d, want 403: %s", resp.StatusCode, out)
	}
}

// The most important case: a user must not be able to raise their own
// limit by patching themselves.
func TestAUserCannotRaiseTheirOwnQuota(t *testing.T) {
	f := adminFixture(t, false)
	body := map[string]any{"quota": map[string]any{"max_concurrent_sessions": 99}}
	resp, _ := f.do(http.MethodPatch, "/api/v1/users/"+auth.LocalID, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("patching oneself = %d, want 403", resp.StatusCode)
	}
	u, err := f.store.Users().Get(context.Background(), auth.LocalID, auth.LocalID)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Quota.Empty() {
		t.Errorf("the quota was changed: %+v", u.Quota)
	}
}

func TestAdminSetsAQuota(t *testing.T) {
	f := adminFixture(t, true)
	body := map[string]any{"quota": map[string]any{
		"max_concurrent_sessions": 9,
		"max_timeout":             "2h",
	}}
	resp, out := f.do(http.MethodPatch, "/api/v1/users/01ARZ3NDEKTSV4RRFFQ69G5FAV", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, out)
	}
	var view UserView
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatal(err)
	}
	if view.Quota.MaxConcurrentSessions != 9 {
		t.Errorf("effective quota = %+v", view.Quota)
	}
	if view.Quota.MaxTimeout != "2h0m0s" {
		t.Errorf("MaxTimeout = %q", view.Quota.MaxTimeout)
	}
	// It persists, and the override records only the pinned fields.
	u, err := f.store.Users().Get(context.Background(), auth.LocalID, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	if u.Quota.MaxConcurrentSessions == nil || *u.Quota.MaxConcurrentSessions != 9 {
		t.Errorf("stored override = %+v", u.Quota)
	}
	if u.Quota.MaxCPU != nil {
		t.Error("a field the admin did not name was pinned")
	}
}

// TestPatchingAUserCannotSetAdminRights: the field is refused rather
// than left as a second thing that looks like it sets admin rights,
// now that the allowlist has an API of its own.
//
// @decision(DL-007) @decision(DL-009)
func TestPatchingAUserCannotSetAdminRights(t *testing.T) {
	f := adminFixture(t, true)
	for _, admin := range []bool{true, false} {
		resp, out := f.do(http.MethodPatch, "/api/v1/users/01ARZ3NDEKTSV4RRFFQ69G5FAV",
			map[string]any{"admin": admin})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("PATCH admin=%v = %d, want 422: %s", admin, resp.StatusCode, out)
		}
	}
	// And the stored flag is untouched, so nothing half-applied.
	u, err := f.store.Users().Get(context.Background(), auth.LocalID, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	if u.Admin {
		t.Error("a refused request granted admin anyway")
	}
}

// A malformed duration is refused with a message naming the shape.
func TestAdminQuotaRejectsABadDuration(t *testing.T) {
	f := adminFixture(t, true)
	resp, out := f.do(http.MethodPatch, "/api/v1/users/01ARZ3NDEKTSV4RRFFQ69G5FAV",
		map[string]any{"quota": map[string]any{"max_timeout": "eight hours"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH = %d, want 422: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "8h") {
		t.Errorf("the error does not show the expected shape: %s", out)
	}
}

// Patching a user who does not exist is a 404, not a silent success.
func TestAdminPatchOfAnUnknownUser(t *testing.T) {
	f := adminFixture(t, true)
	resp, _ := f.do(http.MethodPatch, "/api/v1/users/01ARZ3NDEKTSV4RRFFQ69G5FZZ",
		map[string]any{"quota": map[string]any{"max_concurrent_sessions": 2}})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PATCH of an unknown user = %d, want 404", resp.StatusCode)
	}
}

// TestTheUserListingReportsTheEnforcedAdminFlag, not the stored column,
// which is a mirror written at each login.
//
// @decision(DL-007) @decision(DL-009)
func TestTheUserListingReportsTheEnforcedAdminFlag(t *testing.T) {
	f := adminFixture(t, true)
	ctx := context.Background()
	list, err := auth.NewAllowlist(ctx,
		func(context.Context) (auth.AllowlistSet, error) {
			// The allowlist says the opposite of every stored row:
			// the caller is not an admin, the other user is.
			return auth.AllowlistSet{Emails: map[string]bool{"me@example.com": false, "them@example.com": true}}, nil
		}, auth.AllowlistRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.deps.Login = &Login{Allowlist: list, OrgID: auth.LocalID}

	resp, out := f.do(http.MethodGet, "/api/v1/users", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET users = %d: %s", resp.StatusCode, out)
	}
	var body struct {
		Users []UserView `json:"users"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, u := range body.Users {
		got[u.Email] = u.Admin
	}
	if got["me@example.com"] {
		t.Error("a user the allowlist does not make an admin is reported as one")
	}
	if !got["them@example.com"] {
		t.Error("a user the allowlist makes an admin is not reported as one")
	}
}

// A quota that clears an override leaves the org defaults in force.
func TestAdminClearsAQuota(t *testing.T) {
	f := adminFixture(t, true)
	set := map[string]any{"quota": map[string]any{"max_concurrent_sessions": 9}}
	if resp, out := f.do(http.MethodPatch, "/api/v1/users/01ARZ3NDEKTSV4RRFFQ69G5FAV", set); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, out)
	}
	clear := map[string]any{"quota": map[string]any{}}
	resp, out := f.do(http.MethodPatch, "/api/v1/users/01ARZ3NDEKTSV4RRFFQ69G5FAV", clear)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, out)
	}
	u, err := f.store.Users().Get(context.Background(), auth.LocalID, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Quota.Empty() {
		t.Errorf("the override survived: %+v", u.Quota)
	}
}

// Usage on a server with no sessions is zero, not an error.
func TestUsageWithNoSessions(t *testing.T) {
	f := newAPIFixture(t)
	resp, out := f.do(http.MethodGet, "/api/v1/me/usage", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, out)
	}
	var view UsageView
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatal(err)
	}
	if view.Sessions != 0 || view.EstimatedCost != 0 {
		t.Errorf("usage = %+v, want zero", view)
	}
	if view.Since.Day() != 1 {
		t.Errorf("since = %s, want the first of the month", view.Since)
	}
}

// A session that stopped before the month contributes nothing.
func TestUsageIgnoresLastMonth(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Now = func() time.Time { return now }
		stopped := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
		sess := store.Session{
			ID: store.NewID(now), OrgID: auth.LocalID, OwnerID: auth.LocalID,
			Name: "old", State: store.StateStopped,
			Request:   store.SessionRequest{Sandbox: store.SandboxConfig{CPU: 1, MemoryMiB: 1024}},
			CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			StoppedAt: &stopped,
		}
		if err := d.Store.Sessions().Create(context.Background(), sess); err != nil {
			t.Fatal(err)
		}
	})
	_, out := f.do(http.MethodGet, "/api/v1/me/usage", nil)
	var view UsageView
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatal(err)
	}
	if view.Sessions != 0 {
		t.Errorf("last month's session was counted: %+v", view)
	}
}

func TestAdminListsUsers(t *testing.T) {
	f := adminFixture(t, true)
	resp, out := f.do(http.MethodGet, "/api/v1/users", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, out)
	}
	var got struct {
		Users []UserView `json:"users"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Users) != 2 {
		t.Fatalf("got %d users", len(got.Users))
	}
	// Every user carries a resolved quota, not just those with an
	// override.
	for _, u := range got.Users {
		if u.Quota.MaxConcurrentSessions == 0 {
			t.Errorf("user %s has an unresolved quota: %+v", u.Email, u.Quota)
		}
	}
}

// Usage is per user and labelled an estimate.
func TestUsageIsScopedAndLabelled(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Now = func() time.Time { return now }
		ctx := context.Background()
		mine := store.Session{
			ID: store.NewID(now), OrgID: auth.LocalID, OwnerID: auth.LocalID,
			Name: "mine", State: store.StateStopped,
			Request:   store.SessionRequest{Sandbox: store.SandboxConfig{CPU: 1, MemoryMiB: 1024}},
			CreatedAt: now.Add(-2 * time.Hour),
		}
		stopped := now.Add(-time.Hour)
		mine.StoppedAt = &stopped
		theirs := store.Session{
			ID: store.NewID(now), OrgID: auth.LocalID, OwnerID: "someone-else",
			Name: "theirs", State: store.StateStopped,
			Request:   store.SessionRequest{Sandbox: store.SandboxConfig{CPU: 8, MemoryMiB: 65536}},
			CreatedAt: now.Add(-10 * time.Hour),
		}
		theirs.StoppedAt = &stopped
		for _, s := range []store.Session{mine, theirs} {
			if err := d.Store.Sessions().Create(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
	})
	resp, out := f.do(http.MethodGet, "/api/v1/me/usage", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d: %s", resp.StatusCode, out)
	}
	var view UsageView
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatal(err)
	}
	if view.Sessions != 1 {
		t.Errorf("sessions = %d, want only the caller's", view.Sessions)
	}
	if view.RunningHours != 1 {
		t.Errorf("running hours = %v, want 1", view.RunningHours)
	}
	if !view.Estimated {
		t.Error("the usage is not labelled an estimate")
	}
	if !strings.Contains(string(out), `"estimated":true`) {
		t.Errorf("the response does not carry the estimate label: %s", out)
	}
}

// A session that started before the month counts only its hours inside
// it, so a long session is not billed twice.
func TestUsageCountsOnlyThisMonth(t *testing.T) {
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Now = func() time.Time { return now }
		sess := store.Session{
			ID: store.NewID(now), OrgID: auth.LocalID, OwnerID: auth.LocalID,
			Name: "long", State: store.StateRunning,
			Request:   store.SessionRequest{Sandbox: store.SandboxConfig{CPU: 1, MemoryMiB: 1024}},
			CreatedAt: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
		}
		if err := d.Store.Sessions().Create(context.Background(), sess); err != nil {
			t.Fatal(err)
		}
	})
	_, out := f.do(http.MethodGet, "/api/v1/me/usage", nil)
	var view UsageView
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatal(err)
	}
	// From 1 August to the 10th is 9 days, not the 21 since it started.
	if want := 9 * 24.0; view.RunningHours != want {
		t.Errorf("running hours = %v, want %v", view.RunningHours, want)
	}
}

// TestQuotaOverrideIsADurationOnTheWire: quota_override and
// quota.max_timeout must agree on the duration encoding.
//
// @decision(DL-005) @decision(DL-009)
func TestQuotaOverrideIsADurationOnTheWire(t *testing.T) {
	f := adminFixture(t, true)
	body := map[string]any{"quota": map[string]any{
		"max_concurrent_sessions": 9,
		"max_timeout":             "2h",
	}}
	resp, out := f.do(http.MethodPatch, "/api/v1/users/01ARZ3NDEKTSV4RRFFQ69G5FAV", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, out)
	}
	// Against the raw JSON, because the encoding is the whole point.
	var raw struct {
		Quota    map[string]any `json:"quota"`
		Override map[string]any `json:"quota_override"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	got, ok := raw.Override["max_timeout"].(string)
	if !ok {
		t.Fatalf("quota_override.max_timeout is %T (%v), want a duration string",
			raw.Override["max_timeout"], raw.Override["max_timeout"])
	}
	if got != "2h0m0s" || raw.Quota["max_timeout"] != got {
		t.Errorf("override = %q, effective = %v; the two encodings still differ",
			got, raw.Quota["max_timeout"])
	}
	// The override records what an admin pinned, so an unnamed field is
	// absent rather than a zero that reads as a pinned limit of none.
	if _, present := raw.Override["max_cpu"]; present {
		t.Errorf("an unpinned field is present: %v", raw.Override)
	}

	// The typed view agrees with the bytes.
	var view UserView
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatal(err)
	}
	if view.QuotaOverride.MaxTimeout == nil || *view.QuotaOverride.MaxTimeout != "2h0m0s" {
		t.Errorf("view override timeout = %v", view.QuotaOverride.MaxTimeout)
	}

	// And it round-trips: what comes back is what PATCH accepts.
	again := map[string]any{"quota": raw.Override}
	resp, out = f.do(http.MethodPatch, "/api/v1/users/01ARZ3NDEKTSV4RRFFQ69G5FAV", again)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-PATCH of the reported override = %d: %s", resp.StatusCode, out)
	}
	u, err := f.store.Users().Get(context.Background(), auth.LocalID, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	if u.Quota.MaxTimeout == nil || *u.Quota.MaxTimeout != 2*time.Hour {
		t.Errorf("stored timeout after the round trip = %v", u.Quota.MaxTimeout)
	}
	if u.Quota.MaxConcurrentSessions == nil || *u.Quota.MaxConcurrentSessions != 9 {
		t.Errorf("stored sessions after the round trip = %v", u.Quota.MaxConcurrentSessions)
	}
}

// TestEveryOverrideFieldSurvivesTheRoundTrip: overrideView and
// QuotaInput.quota() convert field by field, so a quota field added to
// store.Quota and forgotten in one of them would vanish from the API
// without a test failing. Reflection is what notices.
//
// @decision(DL-005)
func TestEveryOverrideFieldSurvivesTheRoundTrip(t *testing.T) {
	full := store.Quota{
		MaxConcurrentSessions: intp(9),
		MaxConcurrentJobs:     intp(4),
		MaxJobDuration:        durp(90 * time.Minute),
		MaxTimeout:            durp(2 * time.Hour),
		MaxCPU:                f64p(3),
		MaxMemoryMiB:          intp(2048),
	}
	// The fixture above must keep covering the stored form, or the rest
	// of this test passes over a field it never set.
	if got, want := reflect.TypeOf(full).NumField(), reflect.TypeOf(QuotaOverrideView{}).NumField(); got != want {
		t.Fatalf("store.Quota has %d fields and QuotaOverrideView %d; update both and this fixture", got, want)
	}
	view := reflect.ValueOf(overrideView(full))
	for i := range view.NumField() {
		if view.Field(i).IsNil() {
			t.Errorf("overrideView dropped %s", view.Type().Field(i).Name)
		}
	}
	// And what the API reports converts back to what it came from, which
	// is the round trip PATCH promises.
	back, err := QuotaInput(overrideView(full)).quota()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, full) {
		t.Errorf("round trip = %+v, want %+v", back, full)
	}
}

// An empty override is an object, not a null: a client that reads
// quota_override.max_cpu on an unpinned user must not have to guard the
// parent too.
//
// @decision(DL-005)
func TestAnEmptyQuotaOverrideIsAnObject(t *testing.T) {
	f := adminFixture(t, true)
	_, out := f.do(http.MethodGet, "/api/v1/users", nil)
	var raw struct {
		Users []struct {
			Override *map[string]any `json:"quota_override"`
		} `json:"users"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Users) == 0 {
		t.Fatal("no users listed")
	}
	for i, u := range raw.Users {
		if u.Override == nil {
			t.Errorf("user %d: quota_override is null, want {}", i)
		} else if len(*u.Override) != 0 {
			t.Errorf("user %d: quota_override = %v, want empty", i, *u.Override)
		}
	}
}
