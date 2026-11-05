package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// newRecoverFixture is a running session launched with the proxy, on a
// server that can recreate its sidecar.
func newRecoverFixture(t *testing.T) *renewFixture {
	t.Helper()
	return newRecoverFixtureWith(t, nil)
}

// newRecoverFixtureWith is newRecoverFixture with more of the deps set.
func newRecoverFixtureWith(t *testing.T, more func(*Deps)) *renewFixture {
	t.Helper()
	image := func(context.Context) (string, error) { return "spov-proxy-test", nil }
	api := newAPIFixtureWith(t, func(d *Deps) {
		d.Launch = NewLaunch(&session.Launcher{
			Sandboxes:     d.Provider,
			NewClient:     func(string, string) session.HealthChecker { return stubHealth{} },
			HealthTimeout: 5 * time.Second,
			ProxyImage:    image,
		})
		d.ProxyImage = image
		if more != nil {
			more(d)
		}
	})
	api.forge.platform.Renews = true
	f := &renewFixture{apiFixture: api, id: api.create(repoReq("alpha"))}
	f.events(f.id)
	return f
}

// The first time a sidecar is found stopped it is recreated with the CA
// the sandbox already trusts and fresh credentials, so nothing is
// installed in the main container; the second time the session is ended.
//
// @decision(DL-034)
func TestADeadSidecarIsRecreatedOnceThenTheSessionEnds(t *testing.T) {
	f := newRecoverFixture(t)
	svc := f.srv.svc
	h := f.handle()
	ctx := context.Background()

	svc.CheckSidecars(ctx)
	if got := h.StartedSidecars(); len(got) != 0 {
		t.Fatalf("a running sidecar was recreated: %v", got)
	}

	h.KillSidecar()
	before := f.row().GitTokenMintedAt
	svc.CheckSidecars(ctx)
	started := h.StartedSidecars()
	if len(started) != 1 || started[0].Name != credproxy.SidecarName || started[0].Image != "spov-proxy-test" {
		t.Fatalf("started %+v", started)
	}
	var cfg credproxy.SidecarConfig
	if err := json.Unmarshal([]byte(started[0].Secrets[credproxy.EnvConfig]), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Platforms) != 1 || !strings.HasPrefix(cfg.Platforms[0].Token, "fake-secret-") || !cfg.Platforms[0].Scoped {
		t.Errorf("the recreated sidecar holds %+v", cfg.Platforms)
	}
	// @decision(DL-034)
	if launched := f.launchCA(); launched == "" || started[0].Env[credproxy.EnvCACert] != launched {
		t.Error("the recreated sidecar does not use the CA the sandbox was launched trusting")
	}
	if slices.ContainsFunc(h.ExecLog(), func(c []string) bool { return slices.Equal(c, credproxy.InstallCACommand()) }) {
		t.Error("a CA was installed in the main container though the sandbox already trusts it")
	}
	info, _ := h.Info(ctx)
	if info.Tags[sandbox.TagProxyRecreated] != "1" || info.Tags[sandbox.TagProxy] != "1" || info.Tags[sandbox.TagSpov] != "1" {
		t.Errorf("tags %v", info.Tags)
	}
	if after := f.row().GitTokenMintedAt; after == nil || (before != nil && !after.After(*before)) {
		t.Errorf("the renewal pass does not know of the new token: %v then %v", before, after)
	}

	svc.CheckSidecars(ctx)
	if len(h.StartedSidecars()) != 1 {
		t.Error("a recreated sidecar that is running was recreated again")
	}

	h.KillSidecar()
	svc.CheckSidecars(ctx)
	row := f.row()
	if row.State.Live() || row.Error != ProxyGaveOut || !h.WasTerminated() || len(h.StartedSidecars()) != 1 {
		t.Errorf("after a second death: state %v, error %q, terminated %v", row.State, row.Error, h.WasTerminated())
	}
}

// A recreation that fails still counts as the one, so the next pass ends
// the session rather than trying again every minute.
//
// @decision(DL-034)
func TestAFailedRecreationCountsAsTheOne(t *testing.T) {
	f := newRecoverFixture(t)
	h := f.handle()
	f.provider.StartSidecarErr = context.DeadlineExceeded
	h.KillSidecar()
	f.srv.svc.CheckSidecars(context.Background())
	if f.row().State != store.StateRunning {
		t.Fatal("the first failure ended the session")
	}
	f.srv.svc.CheckSidecars(context.Background())
	if row := f.row(); row.State.Live() || !h.WasTerminated() {
		t.Errorf("the second pass left it %v", row.State)
	}
}

// A sandbox launched without the proxy has no sidecar to look for.
//
// @decision(DL-034)
func TestASandboxWithoutTheProxyIsLeftAlone(t *testing.T) {
	f := newRenewFixture(t)
	f.srv.svc.CheckSidecars(context.Background())
	if h := f.handle(); len(h.StartedSidecars()) != 0 || h.WasTerminated() {
		t.Error("a sandbox with no proxy was touched")
	}
}

// A job whose sidecar dies a second time is failed with a reason naming
// the proxy, through its own marker.
//
// @decision(DL-034)
func TestAJobWhoseSidecarDiesTwiceIsEndedThroughTheJob(t *testing.T) {
	ctx := context.Background()
	provider := sandbox.NewFake()
	s := testService(t, func(d *Deps) {
		d.Provider = provider
		d.ProxyImage = func(context.Context) (string, error) { return "spov-proxy-test", nil }
	})
	h, err := provider.Create(ctx, proxiedJobSpec())
	if err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(ctx)
	job := seedJobSession(t, s, store.StateRunning, info.ID)
	if err := h.AddTags(ctx, map[string]string{sandbox.TagProxyRecreated: "1"}); err != nil {
		t.Fatal(err)
	}
	// Queued and provisioning jobs are left to their own path.
	s.CheckSidecars(ctx)
	if h.(*sandbox.FakeHandle).WasTerminated() {
		t.Fatal("a queued job's sandbox was ended")
	}
	for _, st := range []store.JobState{store.JobProvisioning, store.JobRunning} {
		job.State = st
		if err := s.deps.Store.Jobs().Update(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	s.CheckSidecars(ctx)
	evs, err := s.deps.Store.Conversations().ListEvents(ctx, job.OrgID, job.SessionID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := slices.ContainsFunc(evs, func(e store.Event) bool {
		return e.Source == store.SourceSpov && e.Name == jobs.MarkerProxyFailed && e.Text == ProxyGaveOut
	})
	if !found || !h.(*sandbox.FakeHandle).WasTerminated() {
		t.Errorf("marker %v, terminated %v", found, h.(*sandbox.FakeHandle).WasTerminated())
	}
}

// An adopted job sandbox launched with the proxy but holding no sidecar
// is ended, and its session fails as a failed launch.
//
// @decision(DL-034)
func TestAnAdoptedSandboxWithoutItsSidecarIsEnded(t *testing.T) {
	ctx := context.Background()
	provider := sandbox.NewFake()
	s := testService(t, func(d *Deps) { d.Provider = provider })
	h, err := provider.Create(ctx, proxiedJobSpec())
	if err != nil {
		t.Fatal(err)
	}
	fh := h.(*sandbox.FakeHandle)
	info, _ := h.Info(ctx)
	job := seedJobSession(t, s, store.StateStarting, "")
	fh.KillSidecar()
	if err := s.AdoptJobSandbox(ctx, job, info.ID); err != nil {
		t.Fatal(err)
	}
	sess, err := s.deps.Store.Sessions().Get(ctx, job.OrgID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State != store.StateFailed || sess.Error != NoProxyOnAdopt || !fh.WasTerminated() {
		t.Errorf("state %v, error %q, terminated %v", sess.State, sess.Error, fh.WasTerminated())
	}
}

// proxiedJobSpec is a sandbox launched with the proxy whose sidecar is
// not running.
func proxiedJobSpec() sandbox.Spec {
	return sandbox.Spec{
		Name: "job-sb", Image: "img", Command: []string{"x"}, Egress: sandbox.EgressOpen(),
		CPU: 0.5, MemoryMiB: 512, Timeout: time.Hour, IdleTimeout: 10 * time.Minute,
		Tags: sandbox.BuildTags(sandbox.TagSet{Name: "job-sb", Proxy: true}),
	}
}

// A recreation that fails after minting revokes what it minted, and a
// sidecar that cannot be looked at is left alone.
//
// @decision(DL-034) @decision(DL-021)
func TestAFailedRecreationRevokesWhatItMinted(t *testing.T) {
	f := newRecoverFixture(t)
	h := f.handle()
	h.KillSidecar()

	f.provider.SidecarCheckErr = context.DeadlineExceeded
	f.srv.svc.CheckSidecars(context.Background())
	if info, _ := h.Info(context.Background()); info.Tags[sandbox.TagProxyRecreated] != "" || len(h.StartedSidecars()) != 0 {
		t.Fatal("a sidecar that could not be looked at was acted on")
	}
	f.provider.SidecarCheckErr = nil

	revoked := f.forge.platform.Revoked()
	f.provider.StartSidecarErr = context.DeadlineExceeded
	f.srv.svc.CheckSidecars(context.Background())
	if got := f.forge.platform.Revoked(); got != revoked+1 {
		t.Errorf("revoked %d tokens after a failed recreation, want the one it minted", got-revoked)
	}
}

// A provider key goes to the sidecar, the main container holds a
// placeholder, and the launch log names the key. A recreated sidecar gets
// the keys as they resolve then.
//
// @decision(DL-034)
func TestProviderKeysAreHeldByTheSidecarAndResolvedAgainOnRecreation(t *testing.T) {
	image := func(context.Context) (string, error) { return "spov-proxy-test", nil }
	keys := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-first"}
	api := newAPIFixtureWith(t, func(d *Deps) {
		d.Launch = NewLaunch(&session.Launcher{
			Sandboxes:     d.Provider,
			NewClient:     func(string, string) session.HealthChecker { return stubHealth{} },
			HealthTimeout: 5 * time.Second,
			ProxyImage:    image,
		})
		d.ProxyImage = image
		d.UserProviderKeys = func(context.Context, string, string) (map[string]string, error) { return keys, nil }
	})
	f := &renewFixture{apiFixture: api, id: api.create(repoReq("alpha"))}
	noted := false
	for _, e := range f.events(f.id) {
		if strings.Contains(string(e.Data), ProxiedKeysNote([]string{"ANTHROPIC_API_KEY"})) {
			noted = true
		}
	}
	if !noted {
		t.Error("the launch log does not name the key the proxy holds")
	}
	spec := f.provider.Created[0]
	if v := spec.Secrets["ANTHROPIC_API_KEY"]; !strings.HasPrefix(v, "sk-ant-api03-spovplaceholder") {
		t.Errorf("the main container's key is %q", v)
	}
	var cfg credproxy.SidecarConfig
	if err := json.Unmarshal([]byte(spec.Sidecar.Secrets[credproxy.EnvConfig]), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Key != "sk-ant-api03-first" || cfg.Providers[0].Host != "api.anthropic.com" {
		t.Errorf("the sidecar holds %+v", cfg.Providers)
	}

	keys = map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-second"}
	h := f.handle()
	h.KillSidecar()
	f.srv.svc.CheckSidecars(context.Background())
	started := h.StartedSidecars()
	if len(started) != 1 {
		t.Fatalf("started %d sidecars", len(started))
	}
	cfg = credproxy.SidecarConfig{}
	if err := json.Unmarshal([]byte(started[0].Secrets[credproxy.EnvConfig]), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Key != "sk-ant-api03-second" {
		t.Errorf("the recreated sidecar holds %+v", cfg.Providers)
	}
}

// launchCA is the CA certificate the session's sandbox was created
// trusting.
func (f *renewFixture) launchCA() string {
	for _, spec := range f.provider.Specs() {
		if ca := spec.Env[credproxy.EnvMainCA]; ca != "" {
			return ca
		}
	}
	return ""
}

// With a credential key the CA is kept sealed in the store, apart from
// the session's row, so a server that did not launch the sandbox reuses
// it too.
//
// @decision(DL-034)
func TestAKeptCASurvivesARestartSealed(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, auth.KeyBytes))
	sealer, err := auth.NewSealer(map[int]string{1: key})
	if err != nil {
		t.Fatal(err)
	}
	f := newRecoverFixtureWith(t, func(d *Deps) {
		d.Credentials = &CredentialStore{Store: d.Store.Credentials(), Sealer: sealer}
	})
	ctx := context.Background()
	sealed, err := f.store.Sessions().ProxyCA(ctx, auth.LocalID, f.id)
	if err != nil {
		t.Fatalf("the launch's CA was not kept: %v", err)
	}
	if strings.Contains(string(sealed.Ciphertext), "PRIVATE KEY") || strings.Contains(string(sealed.Ciphertext), "CERTIFICATE") {
		t.Fatal("the CA is stored in the clear")
	}
	// Another process holds nothing in memory.
	svc := f.srv.svc
	svc.mu.Lock()
	svc.proxyCAs = nil
	svc.mu.Unlock()
	h := f.handle()
	h.KillSidecar()
	svc.CheckSidecars(ctx)
	started := h.StartedSidecars()
	if len(started) != 1 || started[0].Env[credproxy.EnvCACert] != f.launchCA() {
		t.Fatalf("the recreated sidecar does not reuse the sealed CA: %d started", len(started))
	}
}

// A server that kept no CA for the sandbox makes a new one and installs
// it in the main container.
//
// @decision(DL-034)
func TestARecreationWithNoKeptCAInstallsANewOne(t *testing.T) {
	f := newRecoverFixture(t)
	svc := f.srv.svc
	svc.mu.Lock()
	svc.proxyCAs = nil
	svc.mu.Unlock()
	h := f.handle()
	h.KillSidecar()
	svc.CheckSidecars(context.Background())
	started := h.StartedSidecars()
	if len(started) != 1 {
		t.Fatalf("started %d sidecars", len(started))
	}
	if started[0].Env[credproxy.EnvCACert] == f.launchCA() {
		t.Error("a CA nobody kept was reused")
	}
	execs, inputs := h.ExecLog(), h.ExecInputs()
	i := slices.IndexFunc(execs, func(c []string) bool { return slices.Equal(c, credproxy.InstallCACommand()) })
	if i < 0 || string(inputs[i]) != started[0].Env[credproxy.EnvCACert] {
		t.Error("the main container was not given the new sidecar's CA")
	}
}

// A sealed CA opens only for the session it was sealed for: a row copied
// under another session's ID is not used.
//
// @decision(DL-034)
func TestAKeptCAOpensOnlyForItsOwnSession(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, auth.KeyBytes))
	sealer, err := auth.NewSealer(map[int]string{1: key})
	if err != nil {
		t.Fatal(err)
	}
	f := newRecoverFixtureWith(t, func(d *Deps) {
		d.Credentials = &CredentialStore{Store: d.Store.Credentials(), Sealer: sealer}
	})
	ctx := context.Background()
	other := f.create(repoReq("beta"))
	f.events(other)
	sealed, err := f.store.Sessions().ProxyCA(ctx, auth.LocalID, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Sessions().SetProxyCA(ctx, auth.LocalID, other, sealed); err != nil {
		t.Fatal(err)
	}
	svc := f.srv.svc
	row, err := f.store.Sessions().Get(ctx, auth.LocalID, other)
	if err != nil {
		t.Fatal(err)
	}
	if ca := svc.proxyCAFor(ctx, row); ca != nil {
		t.Error("another session's sealed CA opened for this one")
	}
	own, _ := f.store.Sessions().Get(ctx, auth.LocalID, f.id)
	if ca := svc.proxyCAFor(ctx, own); ca == nil || ca.Cert != f.launchCA() {
		t.Error("the session's own sealed CA did not open")
	}
}

// A kept CA that does not cover every host the new sidecar decrypts is
// not reused: a new CA is made and installed.
//
// @decision(DL-034)
func TestAKeptCAThatMissesAHostIsReplaced(t *testing.T) {
	f := newRecoverFixture(t)
	cert, key, err := credproxy.NewCA([]string{"unrelated.example"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	svc := f.srv.svc
	svc.mu.Lock()
	svc.proxyCAs[f.id] = session.ProxyCA{Cert: string(cert), Key: string(key)}
	svc.mu.Unlock()
	h := f.handle()
	h.KillSidecar()
	svc.CheckSidecars(context.Background())
	started := h.StartedSidecars()
	if len(started) != 1 || started[0].Env[credproxy.EnvCACert] == string(cert) {
		t.Fatal("a CA that does not cover the sidecar's hosts was reused")
	}
	if !slices.ContainsFunc(h.ExecLog(), func(c []string) bool { return slices.Equal(c, credproxy.InstallCACommand()) }) {
		t.Error("the replacement CA was not installed")
	}
}
