package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

func intp(v int) *int                     { return &v }
func f64p(v float64) *float64             { return &v }
func durp(v time.Duration) *time.Duration { return &v }

func TestQuotaDefaults(t *testing.T) {
	got := Quotas{}.withDefaults()
	if got != DefaultQuotas {
		t.Errorf("withDefaults = %+v, want %+v", got, DefaultQuotas)
	}
}

// A deployment must not be able to promise more than the sandbox rules
// allow, or the quota would pass a request the create then refuses.
func TestQuotaTimeoutIsCappedBySandboxRules(t *testing.T) {
	got := Quotas{MaxTimeout: 48 * time.Hour}.withDefaults()
	if got.MaxTimeout != sandbox.MaxTimeout {
		t.Errorf("MaxTimeout = %s, want the %s hard cap", got.MaxTimeout, sandbox.MaxTimeout)
	}
	// The same cap applies through a per-user override.
	over := Quotas{}.effective(store.Quota{MaxTimeout: durp(48 * time.Hour)})
	if over.MaxTimeout != sandbox.MaxTimeout {
		t.Errorf("override MaxTimeout = %s, want the hard cap", over.MaxTimeout)
	}
}

// An override adjusts the fields it names and leaves the rest.
func TestQuotaOverrideIsPerField(t *testing.T) {
	org := Quotas{MaxConcurrentSessions: 2, MaxTimeout: time.Hour, MaxCPU: 1, MaxMemoryMiB: 1024}
	got := org.effective(store.Quota{MaxConcurrentSessions: intp(9)})
	if got.MaxConcurrentSessions != 9 {
		t.Errorf("MaxConcurrentSessions = %d, want the override", got.MaxConcurrentSessions)
	}
	if got.MaxCPU != 1 || got.MaxMemoryMiB != 1024 || got.MaxTimeout != time.Hour {
		t.Errorf("an override changed fields it did not name: %+v", got)
	}
}

func TestQuotaChecksRequest(t *testing.T) {
	q := Quotas{MaxConcurrentSessions: 3, MaxTimeout: 2 * time.Hour, MaxCPU: 2, MaxMemoryMiB: 4096}.withDefaults()
	for _, tc := range []struct {
		name string
		req  store.SessionRequest
		want string
	}{
		{"within", store.SessionRequest{Sandbox: store.SandboxConfig{
			CPU: 1, MemoryMiB: 2048, Timeout: time.Hour}}, ""},
		{"timeout", store.SessionRequest{Sandbox: store.SandboxConfig{
			CPU: 1, MemoryMiB: 2048, Timeout: 5 * time.Hour}}, "timeout"},
		{"cpu", store.SessionRequest{Sandbox: store.SandboxConfig{
			CPU: 8, MemoryMiB: 2048, Timeout: time.Hour}}, "cpu"},
		{"memory", store.SessionRequest{Sandbox: store.SandboxConfig{
			CPU: 1, MemoryMiB: 65536, Timeout: time.Hour}}, "memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := q.checkRequest(tc.req)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checkRequest = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("checkRequest accepted an over-limit request")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %q", err, tc.want)
			}
			// The limit itself must be named, so the user knows what to
			// ask for rather than guessing.
			if !strings.Contains(err.Error(), "your limit") {
				t.Errorf("err = %v, want it to name the limit", err)
			}
		})
	}
}

// The burst ceilings count against the quota, not just the reservation.
//
// @decision(DL-009) @decision(DL-029)
func TestQuotaBoundsTheBurstCeilings(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Quotas = Quotas{MaxCPU: 1, MaxMemoryMiB: 2048}
	})
	for _, tc := range []struct {
		name string
		mut  func(*SessionRequest)
		want string
	}{
		{"cpu ceiling", func(r *SessionRequest) { r.Sandbox.CPULimit = 64 }, "cpu 64"},
		{"memory ceiling", func(r *SessionRequest) { r.Sandbox.MemoryLimitMiB = 131072 }, "memory 131072"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := repoReq("burst")
			req.Sandbox.CPU = 1
			req.Sandbox.MemoryMiB = 2048
			tc.mut(&req)
			resp, body := f.do(http.MethodPost, "/api/v1/sessions", req)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("create = %d, want 422: %s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("the error does not name the ceiling: %s", body)
			}
		})
	}
	// A ceiling within the quota is fine.
	req := repoReq("modest")
	req.Sandbox.CPU = 0.5
	req.Sandbox.CPULimit = 1
	req.Sandbox.MemoryMiB = 1024
	req.Sandbox.MemoryLimitMiB = 2048
	if resp, body := f.do(http.MethodPost, "/api/v1/sessions", req); resp.StatusCode != http.StatusAccepted {
		t.Errorf("a within-quota ceiling was refused: %d %s", resp.StatusCode, body)
	}
}

// The concurrency limit refuses a create, and says how to proceed.
func TestConcurrencyQuotaRefusesTheCreate(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Quotas = Quotas{MaxConcurrentSessions: 1}
	})
	f.create(repoReq("first"))

	resp, body := f.do(http.MethodPost, "/api/v1/sessions", repoReq("second"))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second create = %d, want 429: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "spov stop") {
		t.Errorf("the refusal does not say what to do: %s", body)
	}
}

// A stopped session frees the slot: the limit is on live sessions, not
// on sessions ever created.
func TestConcurrencyQuotaCountsOnlyLiveSessions(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Quotas = Quotas{MaxConcurrentSessions: 1}
	})
	id := f.create(repoReq("first"))
	f.events(id)
	if resp, body := f.do(http.MethodDelete, "/api/v1/sessions/"+id, nil); resp.StatusCode >= 300 {
		t.Fatalf("delete = %d: %s", resp.StatusCode, body)
	}
	if resp, body := f.do(http.MethodPost, "/api/v1/sessions", repoReq("second")); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second create after a delete = %d, want 202: %s", resp.StatusCode, body)
	}
}

// An over-limit size is refused before anything is created, with the
// limit named rather than the request silently shrunk.
func TestSizeQuotaRefusesBeforeCreating(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Quotas = Quotas{MaxCPU: 1}
	})
	req := repoReq("too-big")
	req.Sandbox.CPU = 8
	resp, body := f.do(http.MethodPost, "/api/v1/sessions", req)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422: %s", resp.StatusCode, body)
	}
	if f.launched.Load() != 0 {
		t.Error("an over-quota request reached the launcher")
	}
}

// A user's stored override is what applies to their sessions.
func TestQuotaOverrideAppliesToTheUser(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Quotas = Quotas{MaxConcurrentSessions: 1}
		if err := d.Store.Users().Create(context.Background(), store.User{
			ID: auth.LocalID, OrgID: auth.LocalID, Email: "me@example.com",
			Quota: store.Quota{MaxConcurrentSessions: intp(3)},
		}); err != nil {
			t.Fatal(err)
		}
	})
	for i, name := range []string{"a", "b", "c"} {
		if resp, body := f.do(http.MethodPost, "/api/v1/sessions", repoReq(name)); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("create %d = %d, want 202: %s", i, resp.StatusCode, body)
		}
	}
	// The fourth exceeds even the raised limit.
	if resp, _ := f.do(http.MethodPost, "/api/v1/sessions", repoReq("d")); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the fourth create = %d, want 429", resp.StatusCode)
	}
}

// PATCH /me is the endpoint a user may call on themselves. It must not
// be a way to grant admin or raise a quota, whatever the body carries.
//
// @decision(DL-009)
func TestPatchMeCannotGrantAdminOrQuota(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Quotas = Quotas{MaxConcurrentSessions: 1}
		if err := d.Store.Users().Create(context.Background(), store.User{
			ID: auth.LocalID, OrgID: auth.LocalID, Email: "me@example.com",
		}); err != nil {
			t.Fatal(err)
		}
	})
	// Every field an attacker might hope binds. The schema refuses the
	// whole request rather than quietly ignoring them, which is the
	// stronger answer.
	body := map[string]any{
		"git_name": "Alice",
		"admin":    true,
		"quota":    map[string]any{"max_concurrent_sessions": 99},
	}
	if resp, out := f.do(http.MethodPatch, "/api/v1/me", body); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH with extra fields = %d, want 422: %s", resp.StatusCode, out)
	}
	u, err := f.store.Users().Get(context.Background(), auth.LocalID, auth.LocalID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Admin {
		t.Error("PATCH /me granted admin")
	}
	if !u.Quota.Empty() {
		t.Errorf("PATCH /me set a quota: %+v", u.Quota)
	}

	// The legitimate fields still work, and still touch nothing else.
	if resp, out := f.do(http.MethodPatch, "/api/v1/me",
		map[string]any{"git_name": "Alice"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, out)
	}
	if u, err = f.store.Users().Get(context.Background(), auth.LocalID, auth.LocalID); err != nil {
		t.Fatal(err)
	}
	if u.GitName != "Alice" || u.Admin || !u.Quota.Empty() {
		t.Errorf("user after a legitimate patch = %+v", u)
	}
}

// /me reports the effective quota, so a client can say why a create
// will be refused before making it.
func TestMeReportsTheEffectiveQuota(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Quotas = Quotas{MaxConcurrentSessions: 2, MaxTimeout: 3 * time.Hour, MaxCPU: 2, MaxMemoryMiB: 8192}
	})
	_, body := f.do(http.MethodGet, "/api/v1/me", nil)
	var me MeView
	if err := json.Unmarshal(body, &me); err != nil {
		t.Fatal(err)
	}
	if me.Quota.MaxConcurrentSessions != 2 {
		t.Errorf("quota = %+v", me.Quota)
	}
	if me.Quota.MaxTimeout != "3h0m0s" {
		t.Errorf("MaxTimeout = %q", me.Quota.MaxTimeout)
	}
}

func TestQuotaRoundTripsThroughJSON(t *testing.T) {
	in := store.Quota{
		MaxConcurrentSessions: intp(5),
		MaxTimeout:            durp(90 * time.Minute),
		MaxCPU:                f64p(2.5),
		MaxMemoryMiB:          intp(8192),
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	// The duration is a string, not nanoseconds: this is what an admin
	// types and what the config file uses.
	if !strings.Contains(string(raw), `"1h30m0s"`) {
		t.Errorf("encoded = %s, want a duration string", raw)
	}
	var out store.Quota
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if *out.MaxTimeout != 90*time.Minute || *out.MaxConcurrentSessions != 5 ||
		*out.MaxCPU != 2.5 || *out.MaxMemoryMiB != 8192 {
		t.Errorf("round trip = %+v", out)
	}
}

// The reaper bounds against the org's longest legitimate session.
func TestMaxSessionAge(t *testing.T) {
	if got := (Quotas{MaxTimeout: 2 * time.Hour}).MaxSessionAge(); got != 2*time.Hour {
		t.Errorf("MaxSessionAge = %s", got)
	}
	// Unset falls to the default, never to zero — zero would disable the
	// backstop on a deployment that simply did not configure quotas.
	if got := (Quotas{}).MaxSessionAge(); got != DefaultQuotas.MaxTimeout {
		t.Errorf("MaxSessionAge with no config = %s, want the default", got)
	}
}

// An override of zero or less is not a limit of nothing: it is ignored,
// so a half-written row cannot lock a user out of creating anything.
func TestQuotaOverrideIgnoresNonPositiveValues(t *testing.T) {
	org := Quotas{MaxConcurrentSessions: 2, MaxTimeout: time.Hour, MaxCPU: 1, MaxMemoryMiB: 1024}
	got := org.effective(store.Quota{
		MaxConcurrentSessions: intp(0),
		MaxTimeout:            durp(0),
		MaxCPU:                f64p(0),
		MaxMemoryMiB:          intp(0),
	})
	if got != org.withDefaults() {
		t.Errorf("zero overrides changed the quota: %+v", got)
	}
}

// A principal with no user row — local mode's bearer token — gets the
// org defaults rather than an error.
func TestQuotaForAnUnknownUser(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Quotas = Quotas{MaxConcurrentSessions: 5}
	})
	got := f.srv.Service().quotaFor(context.Background(), auth.LocalID, "nobody")
	if got.MaxConcurrentSessions != 5 {
		t.Errorf("quota = %+v, want the org defaults", got)
	}
	// And with no user id at all.
	if got := f.srv.Service().quotaFor(context.Background(), auth.LocalID, ""); got.MaxConcurrentSessions != 5 {
		t.Errorf("quota with no user = %+v", got)
	}
}

// The write half of attribution: a launched sandbox carries its owner
// and org, which the reconciler reads back.
//
// @decision(DL-009)
func TestLaunchTagsTheOwnerAndOrg(t *testing.T) {
	f := newAPIFixture(t)
	id := f.create(repoReq("attributed"))
	f.events(id)

	live, err := f.provider.List(context.Background(), sandbox.OwnedTags())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("got %d sandboxes", len(live))
	}
	tags := live[0].Tags
	if tags[sandbox.TagUser] != auth.LocalID {
		t.Errorf("user tag = %q, want the session owner", tags[sandbox.TagUser])
	}
	if tags[sandbox.TagOrg] != auth.LocalID {
		t.Errorf("org tag = %q, want the session org", tags[sandbox.TagOrg])
	}
	// The round trip: what was written is what adopt will accept.
	if !ownerFromTag(tags[sandbox.TagUser]) {
		t.Errorf("the user tag %q would be rejected on adopt", tags[sandbox.TagUser])
	}
	r := &Reconciler{Store: f.store, DefaultOrg: auth.LocalID}
	if !r.orgFromTag(tags[sandbox.TagOrg]) {
		t.Errorf("the org tag %q would be rejected on adopt", tags[sandbox.TagOrg])
	}
}

// The fallback for a session with no owner must still be a principal
// the reader accepts, or it would look like attribution and attribute
// nothing.
func TestAttributionFallsBackToAPrincipal(t *testing.T) {
	if got := attributionUser(""); !ownerFromTag(got) {
		t.Errorf("attributionUser(\"\") = %q, which adopt would reject", got)
	}
	if got := attributionUser("01ARZ3NDEKTSV4RRFFQ69G5FAV"); got != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("attributionUser dropped a real owner: %q", got)
	}
}

// A stored override that is *lower* than the org default reduces the
// limit, not only raises it.
func TestQuotaOverrideCanLowerTheLimit(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Quotas = Quotas{MaxConcurrentSessions: 5}
		if err := d.Store.Users().Create(context.Background(), store.User{
			ID: auth.LocalID, OrgID: auth.LocalID, Email: "me@example.com",
			Quota: store.Quota{MaxConcurrentSessions: intp(1)},
		}); err != nil {
			t.Fatal(err)
		}
	})
	f.create(repoReq("only-one"))
	if resp, body := f.do(http.MethodPost, "/api/v1/sessions", repoReq("second")); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a lowered quota did not apply: %d %s", resp.StatusCode, body)
	}
}

// An empty quota is the common case and must stay empty rather than
// becoming a set of zero limits, which would allow nothing at all.
func TestEmptyQuotaAssertsNothing(t *testing.T) {
	var q store.Quota
	if !q.Empty() {
		t.Error("the zero quota is not empty")
	}
	org := Quotas{MaxConcurrentSessions: 2, MaxTimeout: time.Hour, MaxCPU: 1, MaxMemoryMiB: 1024}
	if got := org.effective(q); got != org.withDefaults() {
		t.Errorf("an empty override changed the quota: %+v", got)
	}
}
