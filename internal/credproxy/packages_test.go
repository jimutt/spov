package credproxy

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func pkgReq(method, url string) *http.Request {
	return originForm(httptest.NewRequest(method, url, nil))
}

// A GitLab token reads the packages of the session's projects and of the
// groups above them, never publishes, and gets nothing on another
// project's or group's packages or on the instance-level endpoints.
//
// @decision(DL-078)
func TestAGitLabTokenReadsItsOwnPackages(t *testing.T) {
	gl := Platform{Name: "gl", Kind: KindGitLab, Host: "gitlab.com", User: "oauth2", Token: "glpat-real",
		Repos: []string{"acme/team/app"}, ProjectIDs: []string{"42"}}
	for _, url := range []string{
		"https://gitlab.com/api/v4/projects/acme%2Fteam%2Fapp/packages/maven/com/acme/lib/1.0/lib-1.0.jar",
		"https://gitlab.com/api/v4/projects/42/packages/npm/@acme%2Fpkg",
		"https://gitlab.com/api/v4/projects/42/packages/pypi/simple/acme-pkg",
		"https://gitlab.com/api/v4/groups/acme/-/packages/maven/com/acme/lib/maven-metadata.xml",
		"https://gitlab.com/api/v4/groups/acme%2Fteam/-/packages/nuget/index.json",
	} {
		if !gl.allows("gitlab.com", pkgReq(http.MethodGet, url)) {
			t.Errorf("GET %s was refused", url)
		}
	}
	for _, c := range []struct{ method, url string }{
		{http.MethodPut, "https://gitlab.com/api/v4/projects/42/packages/maven/com/acme/lib/1.0/lib-1.0.jar"},
		{http.MethodPost, "https://gitlab.com/api/v4/projects/42/packages/pypi"},
		{http.MethodDelete, "https://gitlab.com/api/v4/projects/42/packages/7"},
		{http.MethodGet, "https://gitlab.com/api/v4/projects/other%2Fapp/packages/maven/x.jar"},
		{http.MethodGet, "https://gitlab.com/api/v4/projects/43/packages/npm/x"},
		{http.MethodGet, "https://gitlab.com/api/v4/groups/other/-/packages/maven/x.jar"},
		{http.MethodGet, "https://gitlab.com/api/v4/groups/7/-/packages/maven/x.jar"},
		{http.MethodGet, "https://gitlab.com/api/v4/groups/acme%2Fteam%2Fapp%2Fmore/-/packages/x"},
		{http.MethodGet, "https://gitlab.com/api/v4/packages/npm/@acme%2Fpkg"},
		{http.MethodGet, "https://gitlab.com/api/v4/projects/42/packages/..%2F..%2Fother/x"},
		{http.MethodGet, "https://gitlab.com/api/v4/projects/42/packages/%2e%2e/x"},
		{http.MethodGet, "https://gitlab.com/api/v4/projects/42/packages/a%25252F/x"},
	} {
		if gl.allows("gitlab.com", pkgReq(c.method, c.url)) {
			t.Errorf("%s %s got the token", c.method, c.url)
		}
	}
}

// A Forgejo or Gitea token reads the packages of the session's
// repositories' owners, below the forge's base path, and never
// publishes.
//
// @decision(DL-078)
func TestAForgejoTokenReadsItsOwnersPackages(t *testing.T) {
	fj := forgejo
	fj.BasePath = "/forgejo"
	for _, url := range []string{
		"https://codefloe.com/forgejo/api/packages/o/npm/@o%2Fpkg",
		"https://codefloe.com/forgejo/api/packages/O/maven/com/o/lib/1.0/lib-1.0.pom",
		"https://codefloe.com/forgejo/api/packages/o/pypi/simple/pkg",
	} {
		if !fj.allows("codefloe.com", pkgReq(http.MethodGet, url)) {
			t.Errorf("GET %s was refused", url)
		}
	}
	for _, c := range []struct{ method, url string }{
		{http.MethodPut, "https://codefloe.com/forgejo/api/packages/o/generic/pkg/1.0/f.tgz"},
		{http.MethodGet, "https://codefloe.com/forgejo/api/packages/someone/npm/x"},
		{http.MethodGet, "https://codefloe.com/api/packages/o/npm/x"},
		{http.MethodGet, "https://codefloe.com/forgejo/api/packages/o"},
		{http.MethodGet, "https://codefloe.com/forgejo/api/packages/o/npm/%2e%2e"},
	} {
		if fj.allows("codefloe.com", pkgReq(c.method, c.url)) {
			t.Errorf("%s %s got the token", c.method, c.url)
		}
	}
	all := fj
	all.AllRepos = true
	if !all.allows("codefloe.com", pkgReq(http.MethodGet, "https://codefloe.com/forgejo/api/packages/someone/npm/x")) {
		t.Error("git access all did not lift the owner rule")
	}
	if all.allows("codefloe.com", pkgReq(http.MethodPut, "https://codefloe.com/forgejo/api/packages/someone/npm/x")) {
		t.Error("git access all lifted the read-only rule")
	}
}

// Only a classic PAT reaches GitHub Packages: its hosts are decrypted
// and it reads the packages of the session's owners there, as a bearer
// for npm and Basic otherwise. An App token or a fine-grained PAT gets no
// package host at all.
//
// @decision(DL-078)
func TestOnlyAClassicPATReadsGitHubPackages(t *testing.T) {
	classic := Platform{Name: "gh", Kind: KindGitHub, Host: "github.com", User: "alice", Token: "ghp_real", Repos: []string{"acme/app"}}
	for _, h := range GitHubPackageHosts {
		if !slices.Contains(classic.Hosts(), h) || !slices.Contains((SidecarConfig{Platforms: []Platform{classic}}).CAHosts(), h) {
			t.Errorf("%s is not one of a classic PAT's hosts", h)
		}
	}
	s := newKeyStore(t, SidecarConfig{Platforms: []Platform{classic}})
	for _, c := range []struct{ url, auth string }{
		{"https://npm.pkg.github.com/@acme%2Fpkg", "Bearer ghp_real"},
		{"https://maven.pkg.github.com/acme/app/com/acme/lib/1.0/lib-1.0.pom", basic("alice", "ghp_real")},
		{"https://nuget.pkg.github.com/acme/index.json", basic("alice", "ghp_real")},
	} {
		r := pkgReq(http.MethodGet, c.url)
		if !s.Rewrite(r) || r.Header.Get("Authorization") != c.auth {
			t.Errorf("GET %s: %q", c.url, r.Header.Get("Authorization"))
		}
	}
	for _, c := range []struct{ method, url string }{
		{http.MethodPut, "https://npm.pkg.github.com/@acme%2Fpkg"},
		{http.MethodGet, "https://npm.pkg.github.com/@other%2Fpkg"},
		{http.MethodGet, "https://npm.pkg.github.com/-/whoami"},
		{http.MethodGet, "https://maven.pkg.github.com/other/app/x.pom"},
	} {
		if s.Rewrite(pkgReq(c.method, c.url)) {
			t.Errorf("%s %s got the token", c.method, c.url)
		}
	}
	for _, other := range []Platform{
		{Name: "gh", Kind: KindGitHub, Host: "github.com", User: "x", Token: "ghs_app", Scoped: true},
		{Name: "gh", Kind: KindGitHub, Host: "github.com", User: "x", Token: "github_pat_fine", Repos: []string{"acme/app"}},
	} {
		if len(other.PackageHosts()) != 0 || slices.Contains(other.Hosts(), "npm.pkg.github.com") {
			t.Errorf("%s reaches a package host", other.Token)
		}
	}
}
