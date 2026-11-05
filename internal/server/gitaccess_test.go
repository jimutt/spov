package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
)

// tierSwitch is a caller's resolved tier for the fake platform, which a
// test can change between calls.
type tierSwitch struct {
	mu   sync.Mutex
	tier Tier
}

func (s *tierSwitch) set(t Tier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tier = t
}

// userForge resolves the deployment's forge with the fake platform at
// the switch's tier and the other platform at the user tier.
func (s *tierSwitch) userForge(context.Context, string, string, TierPolicy) (Forge, []PlatformTier, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return nil, []PlatformTier{{Platform: "fake", Tier: s.tier}, {Platform: "other", Tier: TierUser}}, nil
}

// newAccessFixture is a hosted-shaped server whose fake platform is at
// tier, with a second platform, "other", that the caller holds their own
// credential for and selects nothing from.
func newAccessFixture(t *testing.T, tier Tier, adjust func(*Deps)) (*apiFixture, *tierSwitch) {
	t.Helper()
	tiers := &tierSwitch{tier: tier}
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.UserForge = tiers.userForge
		if adjust != nil {
			adjust(d)
		}
	})
	f.forge.other = git.NewFake()
	f.forge.other.PlatformName, f.forge.other.HostName = "other", "other.example.com"
	return f, tiers
}

func accessReq(access store.GitAccess, preferred bool) SessionRequest {
	req := repoReq("alpha")
	req.GitAccess, req.GitAccessPreferred = access, preferred
	return req
}

func (f *apiFixture) session(id string) store.Session {
	f.t.Helper()
	sess, err := f.store.Sessions().Get(context.Background(), auth.LocalID, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return sess
}

func accessOf(creds []store.SessionCredential) map[string]store.GitAccess {
	out := map[string]store.GitAccess{}
	for _, c := range creds {
		out[c.Platform] = c.Access
	}
	return out
}

// Git access all gives every platform the caller holds a credential for
// a token that reaches every repository, including one the session
// selected nothing from, and the session records and shows it.
//
// @decision(DL-043)
func TestGitAccessAllReachesEveryPlatformTheCallerHolds(t *testing.T) {
	f, _ := newAccessFixture(t, TierUser, nil)
	id := f.create(accessReq(store.GitAccessAll, false))
	f.events(id)
	sess := f.session(id)
	if sess.State != store.StateRunning {
		t.Fatalf("the launch ended %s: %s", sess.State, sess.Error)
	}
	if got := accessOf(sess.Credentials); got["fake"] != store.GitAccessAll || got["other"] != store.GitAccessAll {
		t.Errorf("recorded access %v", got)
	}
	if f.forge.platform.Wide() != 1 || f.forge.other.Wide() != 1 {
		t.Errorf("wide tokens minted: fake %d, other %d", f.forge.platform.Wide(), f.forge.other.Wide())
	}
	_, body := f.do(http.MethodGet, "/api/v1/sessions/"+id, nil)
	var view SessionView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.GitAccess != store.GitAccessAll || len(view.Credentials) != 2 || view.Credentials[1].Access != store.GitAccessAll {
		t.Errorf("the view shows %q and %+v", view.GitAccess, view.Credentials)
	}
}

// Repos, the default, mints for the session's repositories alone and
// leaves an unselected platform out.
//
// @decision(DL-043)
func TestGitAccessReposIsTheDefault(t *testing.T) {
	f, _ := newAccessFixture(t, TierUser, nil)
	id := f.create(repoReq("alpha"))
	f.events(id)
	sess := f.session(id)
	if sess.Request.GitAccess != store.GitAccessRepos {
		t.Errorf("the request recorded %q", sess.Request.GitAccess)
	}
	if got := accessOf(sess.Credentials); len(got) != 1 || got["fake"] != store.GitAccessRepos {
		t.Errorf("recorded access %v", got)
	}
	if f.forge.platform.Wide()+f.forge.other.Wide() != 0 {
		t.Error("a wide token was minted for repos")
	}
}

// All on the deployment's credential is refused when the launch named
// it, falls back to repos when it came from a preference, and applies
// when the deployment allows it.
//
// @decision(DL-043)
func TestAllOnAnOrgCredentialNeedsTheDeploymentsConsent(t *testing.T) {
	f, _ := newAccessFixture(t, TierOrg, nil)
	resp, body := f.do(http.MethodPost, "/api/v1/sessions", accessReq(store.GitAccessAll, false))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "cannot apply to fake") ||
		!strings.Contains(string(body), "SPOV_GIT_ALLOW_ALL") || strings.Contains(string(body), "other") {
		t.Errorf("an explicit all on the org credential: %d %s", resp.StatusCode, body)
	}
	if n := f.launched.Load(); n != 0 {
		t.Errorf("a refused create launched %d sandboxes", n)
	}

	id := f.create(accessReq(store.GitAccessAll, true))
	evs := f.events(id)
	noted := slices.ContainsFunc(evs, func(e event) bool {
		return e.Name == "note" && strings.Contains(string(e.Data), "git access: fake reach only")
	})
	if !noted {
		t.Error("the fallback was not noted in the launch log")
	}
	if got := accessOf(f.session(id).Credentials); got["fake"] != store.GitAccessRepos || got["other"] != store.GitAccessAll {
		t.Errorf("a preferred all recorded %v", got)
	}

	allowed, _ := newAccessFixture(t, TierOrg, func(d *Deps) { d.GitAllowAll = true })
	id = allowed.create(accessReq(store.GitAccessAll, false))
	allowed.events(id)
	if got := accessOf(allowed.session(id).Credentials); got["fake"] != store.GitAccessAll {
		t.Errorf("with the deployment's consent all recorded %v", got)
	}
}

// A launch whose explicit all became impossible after its create, as a
// queued job's can, fails rather than falling back.
//
// @decision(DL-043)
func TestAnExplicitAllThatCannotApplyAtLaunchFails(t *testing.T) {
	f, tiers := newAccessFixture(t, TierUser, nil)
	sreq, err := accessReq(store.GitAccessAll, false).toStore()
	if err != nil {
		t.Fatal(err)
	}
	sess, err := f.srv.svc.Create(context.Background(), auth.Principal{OrgID: auth.LocalID, UserID: auth.LocalID}, sreq)
	if err != nil {
		t.Fatal(err)
	}
	// The tier changes between the create and a launch, as it can under a
	// queued job; the launch is run again on the stored request.
	f.events(sess.ID)
	tiers.set(TierOrg)
	log := newLaunchLog(f.srv.svc.deps.Now)
	stored := f.session(sess.ID)
	stored.State = store.StateStarting
	f.srv.svc.launch(context.Background(), stored, log, telemetry.KindInteractive)
	if got := f.session(sess.ID); got.State != store.StateFailed || !strings.Contains(got.Error, "cannot apply to fake") {
		t.Errorf("the launch ended %s: %q", got.State, got.Error)
	}
}

// The stored defaults: the interactive one applies to sessions as a
// preference, and a job takes only the job default.
//
// @decision(DL-043)
func TestAJobNeverTakesTheInteractiveDefault(t *testing.T) {
	f, _ := jobFixtureWith(t, func(d *Deps) { d.GitAllowAll = true })
	if resp, body := f.do(http.MethodPatch, "/api/v1/me", map[string]string{"default_git_access": "all"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, body)
	}
	id := f.create(repoReq("alpha"))
	f.events(id)
	if req := f.session(id).Request; req.GitAccess != store.GitAccessAll || !req.GitAccessPreferred {
		t.Errorf("a session took %q (preferred %v) from the default", req.GitAccess, req.GitAccessPreferred)
	}

	jobSession := func() store.SessionRequest {
		t.Helper()
		resp, body := f.do(http.MethodPost, "/api/v1/jobs", jobRequest())
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("create job: %d %s", resp.StatusCode, body)
		}
		var view JobView
		_ = json.Unmarshal(body, &view)
		return f.session(view.SessionID).Request
	}
	if got := jobSession(); got.GitAccess != store.GitAccessRepos {
		t.Errorf("a job took %q from the interactive default", got.GitAccess)
	}
	if resp, body := f.do(http.MethodPatch, "/api/v1/me", map[string]string{"default_job_git_access": "all"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, body)
	}
	if got := jobSession(); got.GitAccess != store.GitAccessAll || !got.GitAccessPreferred {
		t.Errorf("a job took %q (preferred %v) from its own default", got.GitAccess, got.GitAccessPreferred)
	}
}

// The profile's two defaults are checked, stored, shown and cleared.
//
// @decision(DL-043)
func TestTheGitAccessDefaultsAreSavedThroughTheProfile(t *testing.T) {
	f := newAPIFixture(t)
	resp, body := f.do(http.MethodPatch, "/api/v1/me", map[string]string{"default_git_access": "everything"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("an unknown value: %d %s", resp.StatusCode, body)
	}
	resp, body = f.do(http.MethodPatch, "/api/v1/me", map[string]string{"default_git_access": "all", "default_job_git_access": "repos"})
	var me MeView
	if err := json.Unmarshal(body, &me); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, body)
	}
	if me.DefaultGitAccess != store.GitAccessAll || me.DefaultJobGitAccess != store.GitAccessRepos {
		t.Errorf("saved %+v", me)
	}
	_, body = f.do(http.MethodPatch, "/api/v1/me", map[string]string{"default_git_access": ""})
	me = MeView{}
	_ = json.Unmarshal(body, &me)
	if me.DefaultGitAccess != "" || me.DefaultJobGitAccess != store.GitAccessRepos {
		t.Errorf("clearing one left %+v", me)
	}
}

// A re-mint follows what the session recorded: wide for a platform
// launched with all, and narrow again once its credential has become
// the deployment's, which may not have all.
//
// @decision(DL-043) @decision(DL-021)
func TestARemintKeepsTheRecordedAccessWhileItMayApply(t *testing.T) {
	f, tiers := newAccessFixture(t, TierUser, nil)
	f.forge.platform.Renews = true
	id := f.create(accessReq(store.GitAccessAll, false))
	f.events(id)
	rf := &renewFixture{apiFixture: f, id: id}
	sess := rf.row()
	if err := f.srv.svc.renewGitToken(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if got := f.forge.platform.Wide(); got != 2 {
		t.Errorf("wide mints after one renewal: %d, want 2", got)
	}
	tiers.set(TierOrg)
	if err := f.srv.svc.renewGitToken(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if got := f.forge.platform.Wide(); got != 2 {
		t.Errorf("the deployment's credential was re-minted wide: %d", got)
	}
	if req := f.forge.platform.Requested(); len(req[len(req)-1]) != 1 {
		t.Errorf("the narrow re-mint asked for %v", req[len(req)-1])
	}
	if got := accessOf(rf.row().Credentials); got["fake"] != store.GitAccessRepos || got["other"] != store.GitAccessAll {
		t.Errorf("after narrowing the session records %v", got)
	}
}

// A recreated sidecar holds the platforms and access the launch did,
// including a platform that was selected for nothing, and narrows one
// whose credential has since become the deployment's.
//
// @decision(DL-043) @decision(DL-034)
func TestARecreatedSidecarKeepsTheRecordedAccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		narrowed bool
	}{{"kept", false}, {"narrowed", true}} {
		t.Run(tc.name, func(t *testing.T) { testRecreatedAccess(t, tc.narrowed) })
	}
}

func testRecreatedAccess(t *testing.T, narrowed bool) {
	image := func(context.Context) (string, error) { return "spov-proxy-test", nil }
	f, tiers := newAccessFixture(t, TierUser, func(d *Deps) {
		d.Launch = NewLaunch(&session.Launcher{
			Sandboxes: d.Provider, NewClient: func(string, string) session.HealthChecker { return stubHealth{} },
			HealthTimeout: 5 * time.Second, ProxyImage: image,
		})
		d.ProxyImage = image
	})
	id := f.create(accessReq(store.GitAccessAll, false))
	f.events(id)
	rf := &renewFixture{apiFixture: f, id: id}
	h := rf.handle()
	h.KillSidecar()
	if narrowed {
		tiers.set(TierOrg)
	}
	f.srv.svc.CheckSidecars(context.Background())
	started := h.StartedSidecars()
	if len(started) != 1 {
		t.Fatalf("started %d sidecars", len(started))
	}
	var cfg credproxy.SidecarConfig
	if err := json.Unmarshal([]byte(started[0].Secrets[credproxy.EnvConfig]), &cfg); err != nil {
		t.Fatal(err)
	}
	all := map[string]bool{}
	for _, p := range cfg.Platforms {
		all[p.Name] = p.AllRepos
	}
	if len(all) != 2 || all["fake"] == narrowed || !all["other"] {
		t.Errorf("the recreated sidecar holds %v", all)
	}
}
