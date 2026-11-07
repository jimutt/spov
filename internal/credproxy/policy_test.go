package credproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// originForm makes a test request's line what a client sends inside a
// decrypted tunnel: the path, not the whole URL.
func originForm(r *http.Request) *http.Request {
	r.RequestURI = r.URL.RequestURI()
	return r
}

type policyCase struct {
	method, url string
	want        bool
}

func checkPolicy(t *testing.T, p Platform, cases []policyCase) {
	t.Helper()
	s, _ := newStore(t, p)
	for _, c := range cases {
		r := originForm(httptest.NewRequest(c.method, c.url, nil))
		if got := s.Rewrite(r); got != c.want {
			t.Errorf("%s %s: credential added = %v, want %v", c.method, c.url, got, c.want)
		}
	}
}

// A Forgejo token is added only to allowed endpoints of the session's
// repositories, under the forge's base path.
//
// @decision(DL-034)
func TestAForgejoTokenIsUsedOnlyOnTheSessionsRepositories(t *testing.T) {
	fj := Platform{Name: "fj", Kind: KindForgejo, Host: "git.example.com", User: "u", Token: "t",
		Repos: []string{"Acme/App"}, BasePath: "/forgejo"}
	const root = "https://git.example.com/forgejo"
	checkPolicy(t, fj, []policyCase{
		// git smart HTTP and LFS, with and without .git, any case.
		{"GET", root + "/acme/app.git/info/refs?service=git-upload-pack", true},
		{"POST", root + "/acme/app.git/git-upload-pack", true},
		{"POST", root + "/ACME/APP/git-receive-pack", true},
		{"POST", root + "/acme/app.git/info/lfs/objects/batch", true},
		{"PUT", root + "/acme/app.git/info/lfs/objects/0123abcd", true},
		{"POST", root + "/acme/app.git/info/lfs/objects/0123abcd/verify", true},
		{"POST", root + "/acme/app.git/info/lfs/objects/0123abcd/other", false},
		{"POST", root + "/acme/app.git/info/lfs/locks", false},
		// The REST API.
		{"GET", root + "/api/v1/repos/acme/app", true},
		{"POST", root + "/api/v1/repos/acme/app/pulls", true},
		{"POST", root + "/api/v1/repos/acme/app/issues/3/comments", true},
		{"PUT", root + "/api/v1/repos/acme/app/contents/README.md", true},
		{"GET", root + "/api/v1/repos/acme/app/statuses/abc", true},
		// tea pr create reads the labels first, and aborts on the 404 an
		// uncredentialed read of a private repository gets.
		{"GET", root + "/api/v1/repos/acme/app/labels", true},
		{"GET", root + "/api/v1/repos/acme/app/milestones", true},
		{"GET", root + "/api/v1/user", true},
		{"GET", root + "/api/v1/version", true},
		// Planting access, changing the repository, or another one.
		{"POST", root + "/api/v1/repos/acme/app/keys", false},
		{"POST", root + "/api/v1/repos/acme/app/hooks", false},
		{"PUT", root + "/api/v1/repos/acme/app/collaborators/eve", false},
		{"POST", root + "/api/v1/repos/acme/app/push_mirrors", false},
		{"POST", root + "/api/v1/repos/acme/app/transfer", false},
		{"PATCH", root + "/api/v1/repos/acme/app", false},
		{"DELETE", root + "/api/v1/repos/acme/app", false},
		{"POST", root + "/api/v1/repos/acme/app/statuses/abc", false},
		{"POST", root + "/api/v1/repos/acme/app/labels", false},
		{"DELETE", root + "/api/v1/repos/acme/app/milestones/1", false},
		{"GET", root + "/api/v1/repos/acme/other", false},
		{"GET", root + "/acme/other.git/info/refs", false},
		{"POST", root + "/api/v1/user/repos", false},
		{"GET", root + "/api/v1/user/keys", false},
		// Outside the base path, or a path that could be read two ways.
		{"GET", "https://git.example.com/acme/app.git/info/refs", false},
		{"GET", root + "/api/v1/repos/acme/app/../other", false},
		{"GET", root + "/api/v1/repos/acme/%61pp", false},
		{"GET", root + "//api/v1/repos/acme/app", false},
		{"GET", root + "/api/v1/repos/acme/app/", false},
		{"GET", root + "/acme/app.git/HEAD", false},
	})
}

// A GitHub PAT gets the same narrowing on REST and git, and GraphQL,
// which names no repository.
//
// @decision(DL-034)
func TestAGitHubPATIsNarrowedExceptOnGraphQL(t *testing.T) {
	pat := Platform{Name: "gh", Kind: KindGitHub, Host: "github.com", User: "u", Token: "github_pat_x",
		Repos: []string{"acme/app"}}
	checkPolicy(t, pat, []policyCase{
		{"GET", "https://github.com/acme/app.git/info/refs?service=git-receive-pack", true},
		{"POST", "https://github.com/acme/app.git/git-receive-pack", true},
		{"GET", "https://api.github.com/repos/acme/app", true},
		{"POST", "https://api.github.com/repos/acme/app/pulls", true},
		{"POST", "https://api.github.com/repos/acme/app/git/refs", true},
		{"GET", "https://api.github.com/repos/acme/app/check-runs/1", true},
		{"POST", "https://api.github.com/graphql", true},
		{"GET", "https://api.github.com/user", true},
		{"GET", "https://api.github.com/graphql", false},
		{"POST", "https://api.github.com/repos/acme/app/keys", false},
		{"POST", "https://api.github.com/repos/acme/app/actions/secrets/X", false},
		{"PUT", "https://api.github.com/repos/acme/app/actions/variables/X", false},
		{"GET", "https://api.github.com/repos/acme/other", false},
		{"POST", "https://uploads.github.com/repos/acme/app/releases/1/assets", false},
		{"GET", "https://github.com/acme/other.git/info/refs", false},
		{"GET", "https://api.github.com/repos/acme/app/branches/main", true},
		{"PUT", "https://api.github.com/repos/acme/app/branches/main/protection", false},
		{"DELETE", "https://api.github.com/repos/acme/app/branches/main/protection/enforce_admins", false},
		{"POST", "https://api.github.com/repos/acme/app/branches/main/rename", false},
		{"POST", "https://api.github.com/repos/acme/app/pulls/3/reviews", true},
		{"GET", "https://api.github.com/repos/acme/app/pulls/3/merge", true},
		{"PUT", "https://api.github.com/repos/acme/app/pulls/3/merge", false},
	})
	ghe := Platform{Name: "ghe", Kind: KindGitHub, Host: "ghe.example.com", User: "u", Token: "ghp_x",
		Repos: []string{"acme/app"}}
	checkPolicy(t, ghe, []policyCase{
		{"GET", "https://ghe.example.com/acme/app.git/info/refs", true},
		{"POST", "https://ghe.example.com/api/v3/repos/acme/app/pulls", true},
		{"POST", "https://ghe.example.com/api/graphql", true},
		{"POST", "https://ghe.example.com/api/v3/repos/acme/app/hooks", false},
		{"GET", "https://ghe.example.com/api/v3/repos/acme/other", false},
	})
}

// A GitHub PAT is also given its credential on read-only Actions,
// releases and search, so gh run, gh workflow, gh release and gh search
// are not anonymous, and still on none of the writes, the secrets or
// another repository's.
//
// @decision(DL-107)
func TestAGitHubPATGetsReadOnlyActionsReleasesAndSearch(t *testing.T) {
	pat := Platform{Name: "gh", Kind: KindGitHub, Host: "github.com", User: "u", Token: "github_pat_x",
		Repos: []string{"acme/app"}}
	checkPolicy(t, pat, []policyCase{
		{"GET", "https://api.github.com/repos/acme/app/actions/runs?per_page=20", true},
		{"GET", "https://api.github.com/repos/acme/app/actions/runs/7", true},
		{"GET", "https://api.github.com/repos/acme/app/actions/runs/7/jobs", true},
		{"GET", "https://api.github.com/repos/acme/app/actions/runs/7/logs", true},
		{"HEAD", "https://api.github.com/repos/acme/app/actions/runs/7/logs", true},
		{"GET", "https://api.github.com/repos/acme/app/actions/jobs/9/logs", true},
		{"GET", "https://api.github.com/repos/acme/app/actions/workflows", true},
		{"GET", "https://api.github.com/repos/acme/app/actions/workflows/3/runs", true},
		{"GET", "https://api.github.com/repos/acme/app/actions/artifacts", true},
		{"GET", "https://api.github.com/repos/acme/app/actions/runs/7/artifacts", true},
		{"GET", "https://api.github.com/repos/acme/app/releases", true},
		{"GET", "https://api.github.com/repos/acme/app/releases/tags/v1", true},
		{"GET", "https://api.github.com/repos/acme/app/releases/assets/5", true},
		{"GET", "https://api.github.com/search/issues?q=x", true},
		{"GET", "https://api.github.com/search/code?q=x", true},
		// Writes are refused: a rerun, a cancel, a dispatch, a release.
		{"POST", "https://api.github.com/repos/acme/app/actions/runs/7/rerun", false},
		{"POST", "https://api.github.com/repos/acme/app/actions/runs/7/cancel", false},
		{"POST", "https://api.github.com/repos/acme/app/actions/workflows/3/dispatches", false},
		{"DELETE", "https://api.github.com/repos/acme/app/actions/runs/7", false},
		{"POST", "https://api.github.com/repos/acme/app/releases", false},
		{"DELETE", "https://api.github.com/repos/acme/app/releases/1", false},
		{"POST", "https://api.github.com/search/issues", false},
		// The rest of Actions stays refused by absence.
		{"GET", "https://api.github.com/repos/acme/app/actions/secrets", false},
		{"GET", "https://api.github.com/repos/acme/app/actions/variables", false},
		{"GET", "https://api.github.com/repos/acme/app/actions/runners", false},
		{"GET", "https://api.github.com/repos/acme/app/actions/permissions", false},
		{"GET", "https://api.github.com/repos/acme/app/actions/oidc/customization/sub", false},
		{"GET", "https://api.github.com/repos/acme/app/actions/caches", false},
		{"GET", "https://api.github.com/repos/acme/app/actions", false},
		// Only the session's repositories, and no deeper search path.
		{"GET", "https://api.github.com/repos/acme/other/actions/runs", false},
		{"GET", "https://api.github.com/repos/acme/other/releases", false},
		{"GET", "https://api.github.com/search", false},
		{"GET", "https://api.github.com/search/issues/extra", false},
		{"GET", "https://uploads.github.com/repos/acme/app/releases/1/assets", false},
		// The path is checked by whole segments, after cleaning.
		{"GET", "https://api.github.com/repos/acme/app/actions/runs/../secrets", false},
		{"GET", "https://api.github.com/repos/acme/app/actions/runs/%2e%2e/secrets", false},
		{"GET", "https://api.github.com/repos/acme/app/actions%2Fsecrets", false},
		{"GET", "https://api.github.com/repos/acme/app/actions/runs/", false},
		{"GET", "https://api.github.com/repos/acme/app/releases//1", false},
		{"GET", "https://api.github.com/search/issues/", false},
		{"GET", "https://api.github.com/repos/acme/app/actionsx/runs", false},
		{"GET", "https://api.github.com/repos/acme/app/actions/runsx", false},
		{"GET", "https://api.github.com/repos/acme/app/releasesx", false},
		{"HEAD", "https://api.github.com/search/issues?q=x", true},
		{"GET", "https://github.com/search/issues", false},
		{"GET", "https://github.com/acme/app/actions/runs", false},
	})
	ghe := Platform{Name: "ghe", Kind: KindGitHub, Host: "ghe.example.com", User: "u", Token: "ghp_x",
		Repos: []string{"acme/app"}}
	checkPolicy(t, ghe, []policyCase{
		{"GET", "https://ghe.example.com/api/v3/repos/acme/app/actions/runs", true},
		{"GET", "https://ghe.example.com/api/v3/repos/acme/app/releases", true},
		{"GET", "https://ghe.example.com/api/v3/search/issues?q=x", true},
		{"POST", "https://ghe.example.com/api/v3/repos/acme/app/actions/runs/1/rerun", false},
		{"GET", "https://ghe.example.com/api/v3/repos/acme/app/actions/secrets", false},
		{"GET", "https://ghe.example.com/api/v3/repos/acme/other/actions/runs", false},
		{"GET", "https://ghe.example.com/repos/acme/app/actions/runs", false},
		{"GET", "https://ghe.example.com/search/issues", false},
	})
	// Under git access all, any repository's Actions are read, and
	// nothing else widens.
	all := pat
	all.AllRepos = true
	checkPolicy(t, all, []policyCase{
		{"GET", "https://api.github.com/repos/acme/other/actions/runs", true},
		{"GET", "https://api.github.com/repos/acme/other/actions/secrets", false},
		{"POST", "https://api.github.com/repos/acme/other/actions/runs/7/rerun", false},
	})
}

// Forgejo and Gitea have their own Actions API and get none of the
// GitHub reads.
//
// @decision(DL-107)
func TestForgejoAndGiteaGetNoneOfTheGitHubActionsReads(t *testing.T) {
	for _, kind := range []string{KindForgejo, KindGitea} {
		p := Platform{Name: "f", Kind: kind, Host: "forge.example.com", User: "u", Token: "t",
			Repos: []string{"acme/app"}}
		checkPolicy(t, p, []policyCase{
			{"GET", "https://forge.example.com/api/v1/repos/acme/app/actions/runs", false},
			{"GET", "https://forge.example.com/api/v1/repos/acme/app/releases", false},
			{"GET", "https://forge.example.com/api/v1/search/issues", false},
			{"GET", "https://forge.example.com/api/v1/repos/acme/app/pulls", true},
		})
	}
}

// A GitHub App token is narrowed by GitHub, so it is added anywhere on
// its hosts.
//
// @decision(DL-034)
func TestAGitHubAppTokenIsAddedOnAnyPath(t *testing.T) {
	checkPolicy(t, github, []policyCase{
		{http.MethodPost, "https://api.github.com/repos/acme/app/hooks", true},
		{http.MethodPost, "https://api.github.com/graphql", true},
		{http.MethodGet, "https://github.com/anyone/anything.git/info/refs", true},
	})
}

// A request line in absolute form never gets a credential, even inside a
// decrypted tunnel.
//
// @decision(DL-034)
func TestAnAbsoluteFormRequestGetsNoCredential(t *testing.T) {
	s, _ := newStore(t, forgejo)
	r := httptest.NewRequest("GET", "https://codefloe.com/api/v1/repos/o/r", nil)
	r.RequestURI = "https://codefloe.com/api/v1/repos/o/r"
	if s.Rewrite(r) {
		t.Error("an absolute-form request got a credential")
	}
	if got := s.Secrets(httptest.NewRequest("GET", "https://codefloe.com/x", nil)); len(got) != 2 || got[0] != "f00d" {
		t.Errorf("secrets %v", got)
	}
}

// With all repositories, a stored token is added for any repository on
// the host, still only on the allowed endpoints; a platform may then
// name no repository at all.
//
// @decision(DL-043)
func TestAllRepositoriesLiftsOnlyTheRepositoryRule(t *testing.T) {
	fj := Platform{Name: "fj", Kind: KindForgejo, Host: "git.example.com", User: "u", Token: "t", AllRepos: true}
	if err := (SidecarConfig{Platforms: []Platform{fj}}).Validate(); err != nil {
		t.Fatalf("a platform with all repositories and none named is refused: %v", err)
	}
	const root = "https://git.example.com"
	checkPolicy(t, fj, []policyCase{
		{"POST", root + "/other/thing.git/git-receive-pack", true},
		{"POST", root + "/api/v1/repos/other/thing/pulls", true},
		{"POST", root + "/api/v1/repos/other/thing/keys", false},
		{"PATCH", root + "/api/v1/repos/other/thing", false},
		{"POST", root + "/api/v1/user/repos", false},
	})
	if err := (SidecarConfig{Platforms: []Platform{{Name: "fj", Kind: KindForgejo, Host: "git.example.com", User: "u", Token: "t"}}}).Validate(); err == nil {
		t.Error("a platform with neither repositories nor all of them was accepted")
	}
}

// Gitea takes Forgejo's policy, and a kind the proxy has no policy for
// gets no credential on any path, even one another kind would allow.
//
// @decision(DL-041)
func TestGiteaSharesForgejosPolicyAndAnUnknownKindGetsNothing(t *testing.T) {
	gt := Platform{Name: "gt", Kind: KindGitea, Host: "gitea.com", User: "u", Token: "t", Repos: []string{"me/app"}}
	checkPolicy(t, gt, []policyCase{
		{"POST", "https://gitea.com/me/app.git/git-receive-pack", true},
		{"POST", "https://gitea.com/api/v1/repos/me/app/pulls", true},
		{"POST", "https://gitea.com/api/v1/repos/other/app/pulls", false},
		{"DELETE", "https://gitea.com/api/v1/repos/me/app", false},
	})
	for _, kind := range []string{"bitbucket", "GitHub", ""} {
		odd := gt
		odd.Kind = kind
		for _, u := range []string{"https://gitea.com/me/app.git/git-receive-pack", "https://gitea.com/api/v1/repos/me/app"} {
			if odd.allows("gitea.com", originForm(httptest.NewRequest("POST", u, nil))) {
				t.Errorf("kind %q: %s allowed", kind, u)
			}
		}
		if odd.authorization("gitea.com", "/me/app.git/info/refs") != "" {
			t.Errorf("kind %q got an authorization header", kind)
		}
		if err := (SidecarConfig{Platforms: []Platform{odd}}).Validate(); err == nil {
			t.Errorf("kind %q was accepted", kind)
		}
	}
}

// A GitLab token is added only to allowed endpoints of the session's
// projects: the project by its encoded whole path or a recorded ID, git
// below a path of any depth, and nothing that would merge.
//
// @decision(DL-034) @decision(DL-041)
func TestAGitLabTokenIsUsedOnlyOnTheSessionsProjects(t *testing.T) {
	gl := Platform{Name: "gl", Kind: KindGitLab, Host: "gitlab.com", User: "oauth2", Token: "glpat-x",
		Repos: []string{"group/sub/project"}, ProjectIDs: []string{"42"}}
	const api = "https://gitlab.com/api/v4"
	const proj = api + "/projects/group%2Fsub%2Fproject"
	checkPolicy(t, gl, []policyCase{
		// git, at any depth, with and without .git, and GitLab's LFS.
		{"GET", "https://gitlab.com/group/sub/project.git/info/refs?service=git-upload-pack", true},
		{"POST", "https://gitlab.com/group/sub/project.git/git-receive-pack", true},
		{"POST", "https://gitlab.com/Group/Sub/Project/git-upload-pack", true},
		{"POST", "https://gitlab.com/group/sub/project.git/info/lfs/objects/batch", true},
		{"PUT", "https://gitlab.com/group/sub/project.git/gitlab-lfs/objects/0123abcd/12", true},
		{"GET", "https://gitlab.com/group/sub/project.git/gitlab-lfs/objects/0123abcd", true},
		{"DELETE", "https://gitlab.com/group/sub/project.git/gitlab-lfs/objects/0123abcd", false},
		{"POST", "https://gitlab.com/group/project.git/git-receive-pack", false},
		{"POST", "https://gitlab.com/sub/project.git/git-receive-pack", false},
		{"POST", "https://gitlab.com/group/sub/project/other.git/git-receive-pack", false},
		{"GET", "https://gitlab.com/group/sub/project/-/raw/main/README.md", false},
		// The REST API by encoded path or recorded ID.
		{"GET", proj, true},
		{"PUT", proj, false},
		{"DELETE", proj, false},
		{"POST", proj + "/merge_requests", true},
		{"PUT", proj + "/merge_requests/4", true},
		{"POST", proj + "/merge_requests/4/notes", true},
		{"GET", api + "/projects/group%2fsub%2fproject/merge_requests?source_branch=x", true},
		{"POST", api + "/projects/42/merge_requests", true},
		{"POST", api + "/projects/43/merge_requests", false},
		{"POST", proj + "/issues/3/notes", true},
		{"POST", proj + "/repository/commits", true},
		{"PUT", proj + "/repository/files/src%2Fmain.go", true},
		{"GET", proj + "/repository/files/src%2Fmain.go/raw?ref=main", true},
		{"GET", proj + "/repository/branches/feature%2Fx", true},
		{"DELETE", proj + "/repository/branches/feature%2Fx", false},
		{"GET", proj + "/repository/tree?path=src", true},
		{"GET", proj + "/labels", true},
		{"POST", proj + "/labels", false},
		{"GET", proj + "/pipelines", true},
		// Merging and the steps toward it are refused.
		{"PUT", proj + "/merge_requests/4/merge", false},
		{"POST", proj + "/merge_requests/4/approve", false},
		{"PUT", proj + "/merge_requests/4/rebase", false},
		// Absent sections are refused.
		{"POST", proj + "/hooks", false},
		{"GET", proj + "/variables", false},
		{"POST", proj + "/members", false},
		{"POST", proj + "/access_tokens", false},
		{"PUT", proj + "/protected_branches", false},
		{"POST", proj + "/transfer", false},
		// The repository-less reads, and GraphQL by POST only.
		{"GET", api + "/user", true},
		{"GET", api + "/version", true},
		{"GET", api + "/personal_access_tokens/self", true},
		{"DELETE", api + "/personal_access_tokens/self", false},
		{"GET", api + "/projects", false},
		{"POST", "https://gitlab.com/api/graphql", true},
		{"GET", "https://gitlab.com/api/graphql", false},
		// Another project, however it is spelled.
		{"GET", api + "/projects/group%2Fother", false},
		{"GET", api + "/projects/group%2Fsub%2F..%2Fsub%2Fproject", false},
		{"GET", api + "/projects/group%252Fsub%252Fproject", false},
		{"GET", api + "/projects/group%2Fsub%2Fproject%00", false},
		{"GET", api + "/projects/group%2F%2Fsub%2Fproject", false},
		{"GET", api + "/projects/group%2Fsub%2Fproject.git", false},
		// Escapes outside the project and a named section's one segment.
		{"GET", proj + "/merge_requests%2F4", false},
		{"GET", proj + "/repository/files/a%252Fb/raw", false},
		{"GET", proj + "/repository/files/..%2Fx", false},
		{"GET", proj + "/repository/files/a/raw%2Fx", false},
		// A credential in the query decides nothing.
		{"GET", proj + "?private_token=x", false},
		{"GET", proj + "?access_token=x", false},
		{"GET", proj + "?a=b;private_token=x", false},
		{"GET", proj + "?PRIVATE_TOKEN=x", false},
		{"GET", proj + "?sudo=root", false},
		// A format suffix is the same route to GitLab, so a dot is refused
		// where a name is not expected.
		{"PUT", proj + "/merge_requests/4/merge.json", false},
		{"POST", proj + "/merge_requests/4/approve.json", false},
		{"PUT", proj + "/merge_requests/4.json", false},
		{"GET", proj + "/labels.json", false},
		{"GET", proj + "/repository/archive.zip", true},
		{"POST", proj + "/repository/archive.zip", false},
		// Writes on one merge request or issue: edit it, and its notes and
		// discussions; nothing else, and never delete it.
		{"POST", proj + "/merge_requests", true},
		{"PUT", proj + "/merge_requests", false},
		{"DELETE", proj + "/merge_requests/4", false},
		{"PUT", proj + "/issues/3", true},
		{"DELETE", proj + "/issues/3", false},
		{"POST", proj + "/issues/3/discussions/abc/notes", true},
		{"POST", proj + "/issues/3/move", false},
		{"POST", proj + "/issues/3/clone", false},
		{"POST", proj + "/merge_requests/4/pipelines", false},
		{"GET", proj + "/merge_requests/4/pipelines", true},
		{"POST", proj + "/merge_requests/4/approval_rules", false},
		// A named segment is decoded once and may not climb.
		{"GET", proj + "/repository/files/%2E%2E%2fx/raw", false},
		{"GET", proj + "/repository/files/docs%2fREADME.md/raw", true},
		// GitLab's LFS transfer path is the only extra git path.
		{"GET", "https://gitlab.com/group/sub/project.git/gitlab-lfs/objects/info/refs", false},
	})
	all := gl
	all.Repos, all.ProjectIDs, all.AllRepos = nil, nil, true
	checkPolicy(t, all, []policyCase{
		{"POST", api + "/projects/99/merge_requests", true},
		{"POST", api + "/projects/other%2Fproject/merge_requests", true},
		{"POST", proj + "/hooks", false},
	})
}

// GitLab's API gets a bearer token, under its base path too, and git
// gets Basic auth; a placeholder keeps the glpat- prefix.
//
// @decision(DL-034)
func TestGitLabTakesABearerOnItsAPIAndBasicForGit(t *testing.T) {
	gl := Platform{Name: "gl", Kind: KindGitLab, Host: "git.example.com", User: "oauth2", Token: "glpat-x",
		Repos: []string{"a/b"}, BasePath: "/gitlab"}
	if got := gl.authorization("git.example.com", "/gitlab/api/v4/projects/a%2Fb"); got != "Bearer glpat-x" {
		t.Errorf("api = %q", got)
	}
	if got := gl.authorization("git.example.com", "/gitlab/api/graphql"); got != "Bearer glpat-x" {
		t.Errorf("graphql = %q", got)
	}
	if got := gl.authorization("git.example.com", "/gitlab/a/b.git/info/refs"); got != basic("oauth2", "glpat-x") {
		t.Errorf("git = %q", got)
	}
	checkPolicy(t, gl, []policyCase{
		{"POST", "https://git.example.com/gitlab/a/b.git/git-receive-pack", true},
		{"POST", "https://git.example.com/a/b.git/git-receive-pack", false},
		{"GET", "https://git.example.com/gitlab/api/v4/projects/a%2Fb", true},
		{"GET", "https://git.example.com/api/v4/projects/a%2Fb", false},
	})
	ph, err := Placeholder("glpat-real")
	if err != nil || !strings.HasPrefix(ph, "glpat-spovplaceholder") {
		t.Errorf("placeholder = %q, %v", ph, err)
	}
}

// Two platforms with a credential on one host are refused, since the
// proxy chooses by host.
//
// @decision(DL-034)
func TestTwoPlatformsCannotShareAHost(t *testing.T) {
	a := Platform{Name: "a", Kind: KindGitLab, Host: "gitlab.com", User: "oauth2", Token: "t", Repos: []string{"x/y"}}
	b := a
	b.Name = "b"
	err := SidecarConfig{Platforms: []Platform{a, b}}.Validate()
	if err == nil || !strings.Contains(err.Error(), "gitlab.com") {
		t.Errorf("err = %v", err)
	}
	gh := Platform{Name: "gh", Kind: KindGitHub, Host: "github.com", User: "x", Token: "t", Scoped: true}
	api := Platform{Name: "ghe", Kind: KindGitHub, Host: "api.github.com", User: "x", Token: "t", Scoped: true}
	if err := (SidecarConfig{Platforms: []Platform{gh, api}}).Validate(); err == nil {
		t.Error("a platform on api.github.com sat beside github.com's")
	}
}
