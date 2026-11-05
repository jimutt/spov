package session

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/sandbox"
)

func withProxy(f *fixture) {
	f.launcher.ProxyImage = func(context.Context) (string, error) { return "spov-proxy-test", nil }
}

// With the proxy, the forge token reaches the sidecar only. The main
// container holds a placeholder where the token was, is pointed at the
// proxy, and trusts a CA whose key it never sees.
//
// @decision(DL-034)
func TestAProxiedLaunchKeepsTheTokenOutOfTheMainContainer(t *testing.T) {
	f := newFixture(t)
	f.platform.PlatformName = "codefloe"
	f.platform.HostName = "codefloe.com"
	withProxy(f)
	f.opts.Egress = sandbox.EgressAgent()

	if _, err := f.launcher.Launch(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	spec := f.provider.Created[0]
	sc := spec.Sidecar
	if sc == nil {
		t.Fatal("no sidecar")
	}
	const real = "fake-secret-1"

	placeholder := spec.Secrets[gitTokenEnvVar("codefloe")]
	if placeholder == "" || placeholder == real {
		t.Errorf("the main container's token variable is %q", placeholder)
	}
	for k, v := range spec.Secrets {
		if strings.Contains(v, real) {
			t.Errorf("the real token reached the main container as %s", k)
		}
	}
	for k, v := range spec.Env {
		if strings.Contains(v, real) || strings.Contains(v, "PRIVATE KEY") {
			t.Errorf("a secret reached the main container's plain env as %s", k)
		}
	}
	script := strings.Join(spec.Command, " ")
	if strings.Contains(script, real) || !strings.Contains(script, "/dev/tcp/proxy/3128") {
		t.Error("the entrypoint holds the token or does not wait for the proxy")
	}
	for k, v := range credproxy.MainEnv() {
		if spec.Env[k] != v {
			t.Errorf("main env %s = %q, want %q", k, spec.Env[k], v)
		}
	}

	if sc.Name != credproxy.SidecarName || sc.Image != "spov-proxy-test" || sc.Command[0] != credproxy.BinaryPath {
		t.Errorf("sidecar = %+v", sc)
	}
	if sc.Egress.Preset != sandbox.PresetAgent {
		t.Errorf("the sidecar's egress is %q, want the session's", sc.Egress.Preset)
	}
	if spec.Env[credproxy.EnvMainCA] == "" || spec.Env[credproxy.EnvMainCA] != sc.Env[credproxy.EnvCACert] {
		t.Error("the main container does not trust the sidecar's CA")
	}
	if _, err := credproxy.LoadCA([]byte(sc.Env[credproxy.EnvCACert]), []byte(sc.Secrets[credproxy.EnvCAKey])); err != nil {
		t.Errorf("the sidecar's CA does not load: %v", err)
	}
	var cfg credproxy.SidecarConfig
	if err := json.Unmarshal([]byte(sc.Secrets[credproxy.EnvConfig]), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Platforms) != 1 || cfg.Platforms[0].Token != real || cfg.Platforms[0].Kind != credproxy.KindForgejo ||
		cfg.Platforms[0].Host != "codefloe.com" {
		t.Errorf("the sidecar's config = %+v", cfg)
	}
	// A stored token is used only on the session's repositories.
	//
	// @decision(DL-034)
	if p := cfg.Platforms[0]; p.Scoped || len(p.Repos) != 1 || p.Repos[0] != "acme/app" || p.BasePath != "" {
		t.Errorf("the sidecar's policy = scoped %v, repos %v, base %q", p.Scoped, p.Repos, p.BasePath)
	}
	if _, plain := sc.Env[credproxy.EnvConfig]; plain {
		t.Error("the sidecar's credentials were sent as plain metadata")
	}
}

// Without a proxy image, a launch is what it was: the token in the main
// container, no sidecar.
//
// @decision(DL-034)
func TestALaunchWithoutAProxyImageHasNoSidecar(t *testing.T) {
	f := newFixture(t)
	if _, err := f.launcher.Launch(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	spec := f.provider.Created[0]
	if spec.Sidecar != nil || spec.Env["HTTPS_PROXY"] != "" {
		t.Error("a launch without a proxy image got a proxy")
	}
	if spec.Secrets[gitTokenEnvVar(f.platform.Name())] != "fake-secret-1" {
		t.Error("the token did not reach the main container")
	}
}

// A proxy image that cannot be had fails the launch before any sandbox,
// and revokes what was minted.
//
// @decision(DL-034)
func TestALaunchFailsWhenTheProxyImageCannotBeHad(t *testing.T) {
	f := newFixture(t)
	f.launcher.ProxyImage = func(context.Context) (string, error) { return "", errors.New("bake failed") }
	if _, err := f.launcher.Launch(context.Background(), f.opts); err == nil || !strings.Contains(err.Error(), "bake failed") {
		t.Fatalf("err = %v", err)
	}
	if len(f.provider.Created) != 0 {
		t.Error("a sandbox was created without its proxy")
	}
	if f.platform.Revoked() == 0 {
		t.Error("the minted token was not revoked")
	}
}

// The image bakes the binary where the sidecar is told to run it.
//
// @decision(DL-034)
func TestTheProxyBinaryPathsAgree(t *testing.T) {
	if sandbox.ProxyBinaryPath != credproxy.BinaryPath {
		t.Errorf("the image puts the proxy at %s, the sidecar runs %s", sandbox.ProxyBinaryPath, credproxy.BinaryPath)
	}
}

// A Forgejo's base path is read off its clone URLs.
//
// @decision(DL-034)
func TestTheBasePathIsWhatPrecedesOwnerAndName(t *testing.T) {
	for url, want := range map[string]string{
		"https://git.example.com/acme/app.git":         "",
		"https://git.example.com/forgejo/Acme/App.git": "/forgejo",
		"https://git.example.com/a/b/acme/app":         "/a/b",
		"https://git.example.com/other/thing.git":      "",
		"::": "",
	} {
		if got := cloneBase(git.Repo{Owner: "acme", Name: "app", CloneURL: url}); got != want {
			t.Errorf("%s: %q, want %q", url, got, want)
		}
	}
}

// A sidecar that does not start on its image gets the image baked again
// and one more Create, which waits out the name the failed sandbox still
// holds; a second failure, or a failed re-bake, fails the launch.
//
// @decision(DL-034)
func TestASidecarThatWillNotStartIsRebakedOnce(t *testing.T) {
	f := newFixture(t)
	withProxy(f)
	f.provider.BrokenProxyImages = map[string]bool{"spov-proxy-test": true}
	f.provider.NameHeldFor = 2
	f.launcher.NameFreeInterval = time.Millisecond
	rebakes := 0
	f.launcher.RebakeProxyImage = func(context.Context) (string, error) {
		rebakes++
		return "spov-proxy-rebaked", nil
	}
	if _, err := f.launcher.Launch(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if rebakes != 1 || len(f.provider.Created) != 4 || f.provider.Created[3].Sidecar.Image != "spov-proxy-rebaked" {
		t.Errorf("rebakes %d, creates %d", rebakes, len(f.provider.Created))
	}

	f = newFixture(t)
	withProxy(f)
	f.provider.BrokenProxyImages = map[string]bool{"spov-proxy-test": true}
	f.launcher.RebakeProxyImage = func(context.Context) (string, error) { return "", errors.New("modal is down") }
	_, err := f.launcher.Launch(context.Background(), f.opts)
	if !errors.Is(err, sandbox.ErrSidecarStart) || !strings.Contains(err.Error(), "modal is down") {
		t.Errorf("a failed re-bake: %v", err)
	}
	if len(f.provider.Created) != 1 || f.platform.Revoked() == 0 {
		t.Errorf("creates %d, revoked %d", len(f.provider.Created), f.platform.Revoked())
	}

	f = newFixture(t)
	withProxy(f)
	f.provider.CreateErr = errors.New("quota")
	f.launcher.RebakeProxyImage = func(context.Context) (string, error) {
		t.Error("a failure that is not the sidecar's was re-baked")
		return "", nil
	}
	if _, err := f.launcher.Launch(context.Background(), f.opts); err == nil {
		t.Error("the launch succeeded")
	}
}

// basedFake is a platform served below a base path, as a Forgejo can be.
type basedFake struct{ *git.Fake }

func (basedFake) BasePath() string { return "/forgejo" }

// With git access all, the sidecar adds each token for any repository,
// a platform the session selected nothing from gets one too, and its
// base path comes from the platform.
//
// @decision(DL-043)
func TestAllGivesTheSidecarEveryRepository(t *testing.T) {
	f := newFixture(t)
	withProxy(f)
	other := git.NewFake()
	other.PlatformName, other.HostName = "other", "other.example.com"
	f.opts.Selections[0].All = true
	f.opts.Selections = append(f.opts.Selections, git.Selection{Platform: basedFake{other}, All: true})

	s, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	var cfg credproxy.SidecarConfig
	if err := json.Unmarshal([]byte(f.provider.Created[0].Sidecar.Secrets[credproxy.EnvConfig]), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Platforms) != 2 || !cfg.Platforms[0].AllRepos || !cfg.Platforms[1].AllRepos ||
		len(cfg.Platforms[1].Repos) != 0 || cfg.Platforms[1].BasePath != "/forgejo" {
		t.Errorf("the sidecar's platforms = %+v", cfg.Platforms)
	}
	if f.platform.Wide() != 1 || other.Wide() != 1 {
		t.Errorf("wide mints: %d and %d", f.platform.Wide(), other.Wide())
	}
	if len(s.Credentials) != 2 || !s.Credentials[0].All || !s.Credentials[1].All {
		t.Errorf("credentials = %+v", s.Credentials)
	}
	if len(s.Clones) != 1 {
		t.Errorf("clones = %v", s.Clones)
	}
}

var testAPIs = map[string]credproxy.ProviderAPI{
	"ANTHROPIC_API_KEY": {Host: "api.anthropic.com", Header: credproxy.HeaderAPIKey},
	"OPENAI_API_KEY":    {Host: "api.openai.com", Header: credproxy.HeaderBearer},
}

// sidecarConfig reads the config a launch gave its sidecar.
func sidecarConfig(t *testing.T, spec sandbox.Spec) credproxy.SidecarConfig {
	t.Helper()
	if spec.Sidecar == nil {
		t.Fatal("no sidecar")
	}
	var cfg credproxy.SidecarConfig
	if err := json.Unmarshal([]byte(spec.Sidecar.Secrets[credproxy.EnvConfig]), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// With the proxy, a provider key reaches the sidecar only, set on its
// provider's host, and the main container holds a placeholder with the
// key's prefix. A key the catalogue gives no API stays in the main
// container.
//
// @decision(DL-034)
func TestAProxiedLaunchKeepsTheProviderKeysOutOfTheMainContainer(t *testing.T) {
	f := newFixture(t)
	withProxy(f)
	f.opts.ProviderKeys = map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-api03-real", "OPENAI_API_KEY": "sk-proj-real", "OTHER_API_KEY": "other-real",
	}
	f.opts.ProviderAPIs = testAPIs

	s, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := f.provider.Created[0]
	for _, env := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if v := spec.Secrets[env]; v == "" || strings.Contains(v, "real") || !strings.Contains(v, "spovplaceholder") {
			t.Errorf("the main container's %s is %q", env, v)
		}
	}
	if !strings.HasPrefix(spec.Secrets["ANTHROPIC_API_KEY"], "sk-ant-api03-") {
		t.Errorf("the placeholder lost the key's prefix: %q", spec.Secrets["ANTHROPIC_API_KEY"])
	}
	if spec.Secrets["OTHER_API_KEY"] != "other-real" {
		t.Error("a key with no API did not reach the main container")
	}
	cfg := sidecarConfig(t, spec)
	want := []credproxy.Provider{
		{Env: "ANTHROPIC_API_KEY", ProviderAPI: testAPIs["ANTHROPIC_API_KEY"], Key: "sk-ant-api03-real"},
		{Env: "OPENAI_API_KEY", ProviderAPI: testAPIs["OPENAI_API_KEY"], Key: "sk-proj-real"},
	}
	if !reflect.DeepEqual(cfg.Providers, want) {
		t.Errorf("the sidecar's providers = %+v", cfg.Providers)
	}
	ca, err := credproxy.LoadCA([]byte(spec.Sidecar.Env[credproxy.EnvCACert]), []byte(spec.Sidecar.Secrets[credproxy.EnvCAKey]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Leaf("api.anthropic.com"); err != nil {
		t.Errorf("the CA cannot vouch for a provider's host: %v", err)
	}
	if !slices.Equal(s.ProxiedKeys, []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}) {
		t.Errorf("proxied keys = %v", s.ProxiedKeys)
	}
}

// tokenless is a platform whose repositories are public: it mints no
// token.
type tokenless struct{ *git.Fake }

func (tokenless) IssueSandboxToken(context.Context, []git.Repo, time.Duration) (git.Token, error) {
	return git.Token{}, nil
}

// A launch with a provider key and no forge token still gets the proxy,
// and one without a proxy image keeps its keys in the main container.
//
// @decision(DL-034)
func TestAProviderKeyAloneGetsTheProxy(t *testing.T) {
	f := newFixture(t)
	withProxy(f)
	f.opts.Selections[0].Platform = tokenless{f.platform}
	f.opts.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-real"}
	f.opts.ProviderAPIs = testAPIs
	if _, err := f.launcher.Launch(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	spec := f.provider.Created[0]
	if cfg := sidecarConfig(t, spec); len(cfg.Platforms) != 0 || len(cfg.Providers) != 1 {
		t.Errorf("the sidecar's config = %+v", cfg)
	}
	if spec.Env["HTTPS_PROXY"] == "" || spec.Tags[sandbox.TagProxy] == "" {
		t.Error("the main container was not pointed at the proxy, or the sandbox is not tagged")
	}

	f = newFixture(t)
	f.opts.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-real"}
	f.opts.ProviderAPIs = testAPIs
	s, err := f.launcher.Launch(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if spec := f.provider.Created[0]; spec.Sidecar != nil || spec.Secrets["ANTHROPIC_API_KEY"] != "sk-ant-api03-real" || len(s.ProxiedKeys) != 0 {
		t.Error("without a proxy image the key did not stay in the main container")
	}
}

// The proxy has a policy for exactly the kinds git knows, so no kind can
// be configured that the proxy would refuse or treat as another.
//
// @decision(DL-041)
func TestTheProxyHasAPolicyForEveryKind(t *testing.T) {
	if !slices.Equal(credproxy.Kinds, git.Kinds) {
		t.Errorf("credproxy.Kinds = %v, git.Kinds = %v", credproxy.Kinds, git.Kinds)
	}
}

// A GitLab project in a subgroup gets its base path read off its clone
// URL with the whole group path, and the sidecar carries the project's
// numeric ID.
//
// @decision(DL-041) @decision(DL-034)
func TestTheSidecarKnowsAGitLabProjectByPathAndID(t *testing.T) {
	if got := cloneBase(git.Repo{Owner: "group/sub", Name: "app", CloneURL: "https://git.example.com/gitlab/group/sub/app.git"}); got != "/gitlab" {
		t.Errorf("base = %q", got)
	}
	gl := &idPlatform{Fake: git.NewFake(), ids: map[string]string{"group/sub/app": "42"}}
	gl.PlatformName, gl.HostName, gl.PlatformKind = "gl", "gitlab.com", git.KindGitLab
	repo := git.Repo{Owner: "group/sub", Name: "app", CloneURL: "https://gitlab.com/group/sub/app.git"}
	sel := []git.Selection{{Platform: gl, Repos: []git.Repo{repo}}}
	tokens := []git.PlatformToken{{Platform: "gl", Host: "gitlab.com", Username: git.GitLabUser, Secret: "glpat-real"}}
	p, err := buildProxy(context.Background(), func(context.Context) (string, error) { return "img", nil },
		tokens, sel, nil, sandbox.EgressPolicy{}, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var cfg credproxy.SidecarConfig
	if err := json.Unmarshal([]byte(p.sidecar.Secrets[credproxy.EnvConfig]), &cfg); err != nil {
		t.Fatal(err)
	}
	got := cfg.Platforms[0]
	if got.Kind != credproxy.KindGitLab || !slices.Equal(got.Repos, []string{"group/sub/app"}) || !slices.Equal(got.ProjectIDs, []string{"42"}) {
		t.Errorf("platform = %+v", got)
	}
	if !strings.HasPrefix(p.placeholders[gitTokenEnvVar("gl")], "glpat-") {
		t.Errorf("placeholder = %q", p.placeholders[gitTokenEnvVar("gl")])
	}
}

// idPlatform is a fake platform whose repositories have numeric IDs.
type idPlatform struct {
	*git.Fake
	ids map[string]string
}

func (p *idPlatform) ProjectIDs(_ context.Context, repos []git.Repo) map[string]string { return p.ids }
