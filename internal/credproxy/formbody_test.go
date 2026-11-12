package credproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Forgejo, Gitea and GitLab read sudo and a token from a form body as
// well as from the query, so a request with a form body gets no
// credential on their hosts, whatever its path; the forge CLIs send
// JSON, and git and LFS their own types. GitHub reads neither.
//
// @decision(DL-135)
func TestAFormBodyGetsNoForgeCredential(t *testing.T) {
	fj := Platform{Name: "fj", Kind: KindForgejo, Host: "git.example.com", User: "u", Token: "t", Repos: []string{"acme/app"}}
	gt := fj
	gt.Name, gt.Kind = "gt", KindGitea
	gl := Platform{Name: "gl", Kind: KindGitLab, Host: "gitlab.example.com", User: "u", Token: "glpat-x", Repos: []string{"acme/app"}}
	gh := Platform{Name: "gh", Kind: KindGitHub, Host: "github.com", User: "u", Token: "github_pat_x", Repos: []string{"acme/app"}}
	const proj = "https://gitlab.example.com/api/v4/projects/acme%2Fapp"
	cases := []struct {
		p                        Platform
		method, url, ctype, body string
		want                     bool
	}{
		{fj, "POST", "https://git.example.com/api/v1/repos/acme/app/issues", "application/json", `{"title":"t"}`, true},
		{fj, "POST", "https://git.example.com/api/v1/repos/acme/app/issues", "application/x-www-form-urlencoded", "title=t&sudo=alice", false},
		{fj, "POST", "https://git.example.com/api/v1/repos/acme/app/issues", "Application/X-WWW-Form-Urlencoded; charset=utf-8", "title=t", false},
		{fj, "PATCH", "https://git.example.com/api/v1/repos/acme/app/issues/1", "multipart/form-data; boundary=x", "", false},
		{fj, "POST", "https://git.example.com/api/v1/repos/acme/app/issues/1/assets", "multipart/form-data; boundary=x", "", false},
		{fj, "POST", "https://git.example.com/acme/app.git/git-receive-pack", "application/x-git-receive-pack-request", "", true},
		{fj, "PUT", "https://git.example.com/acme/app.git/info/lfs/objects/0123abcd", "application/octet-stream", "", true},
		{gt, "PUT", "https://git.example.com/api/v1/repos/acme/app/contents/README.md", "application/x-www-form-urlencoded", "token=x", false},
		{gt, "PUT", "https://git.example.com/api/v1/repos/acme/app/contents/README.md", "application/json", "{}", true},
		{gl, "POST", proj + "/merge_requests", "application/json", "{}", true},
		{gl, "POST", proj + "/merge_requests", "application/x-www-form-urlencoded", "private_token=x", false},
		{gl, "PUT", proj + "/merge_requests/4", "multipart/form-data; boundary=x", "", false},
		{gl, "POST", "https://gitlab.example.com/api/graphql", "application/x-www-form-urlencoded", "query=x", false},
		{gl, "POST", "https://gitlab.example.com/acme/app.git/git-upload-pack", "application/x-git-upload-pack-request", "", true},
		{gh, "POST", "https://api.github.com/repos/acme/app/pulls", "application/x-www-form-urlencoded", "title=t", true},
	}
	for _, c := range cases {
		s, _ := newStore(t, c.p)
		r := originForm(httptest.NewRequest(c.method, c.url, strings.NewReader(c.body)))
		r.Header.Set("Content-Type", c.ctype)
		if got := s.Rewrite(r); got != c.want {
			t.Errorf("%s %s %s (%s): credential added = %v, want %v", c.p.Kind, c.method, c.url, c.ctype, got, c.want)
		}
	}
	// A second Content-Type is read too.
	s, _ := newStore(t, fj)
	r := originForm(httptest.NewRequest(http.MethodPost, "https://git.example.com/api/v1/repos/acme/app/issues", strings.NewReader("sudo=alice")))
	r.Header.Add("Content-Type", "application/json")
	r.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	if s.Rewrite(r) {
		t.Errorf("a form type in a second Content-Type header got the credential")
	}
}
