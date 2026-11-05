package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
)

func TestParseRepoFlag(t *testing.T) {
	tests := []struct {
		in       string
		platform string
		owner    string
		name     string
		wantErr  bool
	}{
		{in: "alice/spov", owner: "alice", name: "spov"},
		{in: "codefloe:alice/spov", platform: "codefloe", owner: "alice", name: "spov"},
		{in: "  alice/spov  ", owner: "alice", name: "spov"},
		{in: "", wantErr: true},
		{in: "alice", wantErr: true},
		{in: "/spov", wantErr: true},
		{in: "alice/", wantErr: true},
		{in: "codefloe:", wantErr: true},
		{in: "gitlab:group/sub/project", platform: "gitlab", owner: "group/sub", name: "project"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseRepoFlag(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.Platform != tt.platform || got.Owner != tt.owner || got.Name != tt.name {
				t.Errorf("parsed %+v, want %s:%s/%s", got, tt.platform, tt.owner, tt.name)
			}
		})
	}
}

// A group path is a GitLab platform's only: on another kind the same
// flag is refused once its platform is known.
//
// @decision(DL-041)
func TestANestedRepoFlagNeedsAGitLabPlatform(t *testing.T) {
	cfg := config.Config{Platforms: []config.Platform{
		{Name: "gl", Kind: config.KindGitLab},
		{Name: "cf", Kind: config.KindForgejo, BaseURL: "https://codefloe.com"},
	}}
	refs, err := refsFromFlags(cfg, []string{"gl:group/sub/project"})
	if err != nil || len(refs) != 1 || refs[0].Owner != "group/sub" || refs[0].Name != "project" {
		t.Fatalf("refs = %+v, %v", refs, err)
	}
	if _, err := refsFromFlags(cfg, []string{"cf:group/sub/project"}); err == nil || !strings.Contains(err.Error(), "more than one segment") {
		t.Errorf("err = %v", err)
	}
	if got := cloneURL(cfg.Platforms[0], "group/sub", "project"); got != "https://gitlab.com/group/sub/project.git" {
		t.Errorf("clone URL = %q", got)
	}
}

// TestParseRepoFlagRejectsMalformedValues: a bad --repo must be reported
// in the flag's own terms.
func TestParseRepoFlagRejectsMalformedValues(t *testing.T) {
	for _, in := range []string{"a//b", "a/", "/b", "../x", "a/..", "-a/b", "a/b c", "a/\tb"} {
		t.Run(in, func(t *testing.T) {
			_, err := parseRepoFlag(in)
			if err == nil {
				t.Fatalf("accepted %q", in)
			}
			if !strings.Contains(err.Error(), "--repo") {
				t.Errorf("error %q does not mention --repo", err)
			}
		})
	}
}

func onePlatform() config.Config {
	cfg := config.Defaults()
	cfg.Platforms = []config.Platform{{Name: "codefloe", Kind: config.KindForgejo, BaseURL: "https://codefloe.com", Account: "alice"}}
	return cfg
}

func TestResolveRepoRefsPrefersFlags(t *testing.T) {
	cfg := onePlatform()
	cfg.Repos = []config.Repo{{Platform: "codefloe", Owner: "alice", Name: "other"}}
	refs, err := (&Env{}).resolveRepoRefs(context.Background(), nil, cfg, []string{"alice/spov"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != (server.RepoRef{Platform: "codefloe", Owner: "alice", Name: "spov"}) {
		t.Errorf("refs = %+v; --repo must win over the configured list", refs)
	}
}

func TestResolveRepoRefsFallsBackToConfig(t *testing.T) {
	cfg := onePlatform()
	cfg.Repos = []config.Repo{{Platform: "codefloe", Owner: "alice", Name: "configured"}}
	refs, err := (&Env{}).resolveRepoRefs(context.Background(), nil, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "configured" {
		t.Errorf("refs = %+v", refs)
	}
}

func TestResolveRepoRefsRejectsAConfiguredRepoOnAnUnknownPlatform(t *testing.T) {
	cfg := onePlatform()
	cfg.Repos = []config.Repo{{Platform: "gh", Owner: "alice", Name: "x"}}
	_, err := (&Env{}).resolveRepoRefs(context.Background(), nil, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "codefloe") {
		t.Errorf("err = %v, want the configured platforms listed", err)
	}
}

// TestResolveRepoRefsAsksWhenNothingIsConfigured is what makes
// `spov fly` work in a directory with no project file: the server lists
// what it can reach and the user picks.
//
// @decision(DL-023) @decision(DL-028)
func TestResolveRepoRefsAsksWhenNothingIsConfigured(t *testing.T) {
	f := newAPIFixtureWithConfig(t, platformOnlyConfig)
	updated := time.Now().Add(-48 * time.Hour)
	f.platform.Repos = []git.Repo{
		{Owner: "alice", Name: "spov", Private: true, UpdatedAt: updated},
		{Owner: "alice", Name: "gadgets"},
	}
	var offered []RepoChoice
	f.env.SelectRepos = func(repos []RepoChoice) ([]RepoChoice, error) {
		offered = repos
		return repos[1:], nil
	}
	refs, err := f.env.resolveRepoRefs(context.Background(), f.env.Client, onePlatform(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(offered) != 2 {
		t.Fatalf("the picker was offered %d repositories, want all of them", len(offered))
	}
	// What the picker shows survives the trip through the API.
	if offered[0].Platform != "codefloe" || !offered[0].Repo.Private || offered[0].Repo.UpdatedAt.IsZero() {
		t.Errorf("offered[0] = %+v, want platform, privacy and age", offered[0])
	}
	if len(refs) != 1 || refs[0].Name != "gadgets" || refs[0].Platform != "codefloe" {
		t.Errorf("refs = %+v, want only what the user picked", refs)
	}
}

// TestResolveRepoRefsWithoutAPicker: a scripted run must fail with a
// clear message rather than hang on a prompt nobody can answer.
func TestResolveRepoRefsWithoutAPicker(t *testing.T) {
	_, err := (&Env{}).resolveRepoRefs(context.Background(), nil, onePlatform(), nil)
	if err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Errorf("err = %v, want the non-interactive alternative offered", err)
	}
}

func TestResolveRepoRefsWithoutPlatforms(t *testing.T) {
	cfg := config.Defaults()
	_, err := (&Env{}).resolveRepoRefs(context.Background(), nil, cfg, []string{"alice/spov"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"no git platforms configured", "<platform>:owner/name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// A qualified reference needs no configured platform: the server
	// may know one the client does not.
	refs, err := (&Env{}).resolveRepoRefs(context.Background(), nil, cfg, []string{"gh:alice/spov"})
	if err != nil || len(refs) != 1 || refs[0].Platform != "gh" {
		t.Errorf("refs = %+v, err = %v", refs, err)
	}
}

func TestResolveRepoRefsWithSeveralPlatforms(t *testing.T) {
	cfg := onePlatform()
	cfg.Platforms = append(cfg.Platforms, config.Platform{Name: "gh", Kind: config.KindGitHub, Account: "alice"})
	_, err := (&Env{}).resolveRepoRefs(context.Background(), nil, cfg, []string{"alice/spov"})
	if err == nil || !strings.Contains(err.Error(), "<platform>:owner/name") {
		t.Errorf("err = %v, want it to say how to qualify", err)
	}
	refs, err := (&Env{}).resolveRepoRefs(context.Background(), nil, cfg, []string{"gh:alice/spov", "codefloe:alice/x"})
	if err != nil || len(refs) != 2 || refs[0].Platform != "gh" || refs[1].Platform != "codefloe" {
		t.Errorf("refs = %+v, err = %v", refs, err)
	}
}

func TestResolveRepoRefsPropagatesPickerErrors(t *testing.T) {
	f := newAPIFixtureWithConfig(t, platformOnlyConfig)
	f.env.SelectRepos = func([]RepoChoice) ([]RepoChoice, error) { return nil, errors.New("cancelled") }
	if _, err := f.env.resolveRepoRefs(context.Background(), f.env.Client, onePlatform(), nil); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v", err)
	}
	f.env.SelectRepos = func([]RepoChoice) ([]RepoChoice, error) { return nil, nil }
	if _, err := f.env.resolveRepoRefs(context.Background(), f.env.Client, onePlatform(), nil); err == nil || !strings.Contains(err.Error(), "no repositories selected") {
		t.Errorf("err = %v", err)
	}
}

// TestResolveRepoRefsWithNoRepositories: showing a picker with nothing
// in it is worse than a message saying what to check.
func TestResolveRepoRefsWithNoRepositories(t *testing.T) {
	f := newAPIFixtureWithConfig(t, platformOnlyConfig)
	f.platform.Repos = nil
	f.platform.ListErr = git.ErrNoRepos
	f.env.SelectRepos = func([]RepoChoice) ([]RepoChoice, error) { t.Fatal("the picker was shown"); return nil, nil }
	_, err := f.env.resolveRepoRefs(context.Background(), f.env.Client, onePlatform(), nil)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("err = %v, want it to suggest what to check", err)
	}
}

// TestServerListsRepositoriesAcrossPlatforms is the server half: the
// configured platforms are listed together, and one that fails is
// reported without hiding the others.
//
// @decision(DL-028)
func TestServerListsRepositoriesAcrossPlatforms(t *testing.T) {
	f := newAPIFixtureWithConfig(t, `platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
  - name: gh
    kind: github
    account: alice
`)
	gh := git.NewFake()
	gh.PlatformName = "gh"
	gh.ListErr = errors.New("token expired")
	fakes := map[string]*git.Fake{"codefloe": f.platform, "gh": gh}
	f.serverEnv.NewPlatform = func(_ context.Context, p config.Platform, _ string) (git.Platform, error) {
		return fakes[p.Name], nil
	}
	key, _ := credentials.PlatformKey("gh")
	_ = f.creds.Set(key, "t")
	f.http.Close()
	f.startServer()

	repos, warnings, err := f.deps.Forge.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("one failing platform aborted the listing: %v", err)
	}
	if len(repos) != 1 || repos[0].Platform.Name() != "codefloe" {
		t.Errorf("repos = %+v, want only the platform that answered", repos)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "gh") || !strings.Contains(warnings[0], "token expired") {
		t.Errorf("warnings = %v", warnings)
	}
	if !strings.Contains(f.serverErr.String(), "token expired") {
		t.Errorf("the failure was not logged on the server:\n%s", f.serverErr.String())
	}

	// And the warning reaches the user at the client, where they are.
	f.env.Client = client.New(f.http.URL, f.token)
	var picked bool
	f.env.SelectRepos = func(repos []RepoChoice) ([]RepoChoice, error) { picked = true; return repos, nil }
	var errBuf strings.Builder
	f.env.Err = &errBuf
	if _, err := f.env.resolveRepoRefs(context.Background(), f.env.Client, onePlatform(), nil); err != nil || !picked {
		t.Fatalf("resolve: %v (picked %v)", err, picked)
	}
	if !strings.Contains(errBuf.String(), "token expired") {
		t.Errorf("the client was not warned:\n%s", errBuf.String())
	}

	f.platform.ListErr = errors.New("down too")
	if _, _, err := f.deps.Forge.ListRepos(context.Background()); err == nil {
		t.Fatal("expected an error when no platform could be listed")
	}
}

// TestServerSelectionsGroupByPlatform: the request names repositories;
// the server turns them into one selection per platform, in first-seen
// order, with clone URLs derived from the platform.
//
// @decision(DL-041) @decision(DL-028)
func TestServerSelectionsGroupByPlatform(t *testing.T) {
	f := newAPIFixtureWithConfig(t, `platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
  - name: gh
    kind: github
    account: alice
`)
	key, _ := credentials.PlatformKey("gh")
	_ = f.creds.Set(key, "t")
	cfg, _ := f.serverEnv.config()
	sels, err := f.serverEnv.selectionsFor(context.Background(), cfg.Config, []server.RepoRef{
		{Platform: "gh", Owner: "a", Name: "one"},
		{Platform: "codefloe", Owner: "b", Name: "two"},
		{Platform: "gh", Owner: "a", Name: "three"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sels) != 2 || len(sels[0].Repos) != 2 || len(sels[1].Repos) != 1 {
		t.Fatalf("selections = %+v", sels)
	}
	if got := sels[0].Repos[0].CloneURL; got != "https://github.com/a/one.git" {
		t.Errorf("GitHub clone URL = %q", got)
	}
	if got := sels[1].Repos[0].CloneURL; got != "https://codefloe.com/b/two.git" {
		t.Errorf("Forgejo clone URL = %q", got)
	}

	if _, err := f.serverEnv.selectionsFor(context.Background(), cfg.Config, []server.RepoRef{{Platform: "nope", Owner: "a", Name: "b"}}); err == nil {
		t.Error("an unknown platform was accepted")
	}
	if _, err := f.serverEnv.selectionsFor(context.Background(), config.Defaults(), nil); err == nil || !strings.Contains(err.Error(), "platforms") {
		t.Errorf("err = %v, want no platforms reported", err)
	}
}

func TestCloneURL(t *testing.T) {
	tests := []struct {
		p    config.Platform
		want string
	}{
		{config.Platform{Kind: config.KindForgejo, BaseURL: "https://codefloe.com/"}, "https://codefloe.com/o/n.git"},
		{config.Platform{Kind: config.KindGitHub}, "https://github.com/o/n.git"},
		{config.Platform{Kind: config.KindGitHub, BaseURL: "https://ghe.example"}, "https://ghe.example/o/n.git"},
	}
	for _, tt := range tests {
		if got := cloneURL(tt.p, "o", "n"); got != tt.want {
			t.Errorf("cloneURL = %q, want %q", got, tt.want)
		}
	}
}

// TestRepoDetail: the detail beside a row is what makes one repository
// distinguishable from another with the same name.
func TestRepoDetail(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	detail := repoDetail(RepoChoice{Repo: git.Repo{Private: true, UpdatedAt: now.Add(-3 * 24 * time.Hour)}}, now)
	for _, want := range []string{"private", "3d ago"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not contain %q", detail, want)
		}
	}
	if archived := repoDetail(RepoChoice{Repo: git.Repo{Archived: true}}, now); !strings.Contains(archived, "archived") {
		t.Errorf("detail %q does not flag an archived repo", archived)
	}
}

func TestHumanAge(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{10 * time.Minute, "just now"},
		{5 * time.Hour, "5h ago"},
		{3 * 24 * time.Hour, "3d ago"},
		{60 * 24 * time.Hour, "2mo ago"},
		{2 * 365 * 24 * time.Hour, "2y ago"},
	}
	for _, tt := range tests {
		if got := humanAge(tt.in); got != tt.want {
			t.Errorf("humanAge(%s) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSelectReposInteractivelyRejectsAnEmptyList(t *testing.T) {
	if _, err := SelectReposInteractively(nil); err == nil {
		t.Fatal("an empty list produced a form the user cannot satisfy")
	}
}

// TestFlySaysWhatTheSandboxHolds: the line is the server's to give,
// since only it knows the platforms, and it reaches the terminal through
// the launch stream. A renewable GitHub App token gets no line and no
// expiry warning, since the server re-mints it.
//
// @decision(DL-043) @decision(DL-021) @decision(DL-034)
func TestFlySaysWhatTheSandboxHolds(t *testing.T) {
	f := newAPIFixture(t)
	_, stderr, err := runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "stored "+f.platform.PlatformName+" token is held by spov's proxy") {
		t.Errorf("no stored-token line:\n%s", stderr)
	}

	f = newAPIFixtureWithConfig(t, defaultConfig+"sandbox:\n  timeout: 4h\n")
	f.platform.PlatformKind = "github"
	f.platform.HostName = "github.com"
	f.platform.Renews = true
	_, stderr, err = runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "expires after") || strings.Contains(stderr, "stored "+f.platform.PlatformName+" token") {
		t.Errorf("a renewable token was warned about:\n%s", stderr)
	}
}
