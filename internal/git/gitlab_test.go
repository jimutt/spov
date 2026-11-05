package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const gitlabToken = "glpat-stored-token-value"

// gitlabServer answers the API calls the adapter makes, by escaped path,
// and refuses a request without the token.
func gitlabServer(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) *GitLab {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+gitlabToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
			return
		}
		h, ok := routes[r.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Not Found"}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	g, err := NewGitLab(GitLabConfig{Name: "gitlab", BaseURL: srv.URL, Token: gitlabToken})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func write(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

// A project in a subgroup keeps its whole group path as its owner.
//
// @decision(DL-041)
func TestGitLabListsProjectsUnderTheirWholeGroupPath(t *testing.T) {
	g := gitlabServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v4/projects": write(`[
			{"id":1,"path":"old","http_url_to_repo":"https://gitlab.com/me/old.git","visibility":"public",
			 "last_activity_at":"2026-01-01T00:00:00Z","namespace":{"full_path":"me"}},
			{"id":2,"path":"project","http_url_to_repo":"https://gitlab.com/group/sub/project.git","visibility":"private",
			 "last_activity_at":"2026-09-01T00:00:00Z","namespace":{"full_path":"group/sub"}}]`),
	})
	repos, err := g.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Slug() != "group/sub/project" || !repos[0].Private || repos[1].Private {
		t.Fatalf("repos = %+v", repos)
	}
	if repos[0].CloneURL != "https://gitlab.com/group/sub/project.git" {
		t.Errorf("clone URL = %q", repos[0].CloneURL)
	}
}

// Gate 2 asks the project by its encoded path and counts only a merge
// request from the project itself.
//
// @decision(DL-051)
func TestGitLabFindsAnOpenMergeRequestFromTheProjectItself(t *testing.T) {
	var asked string
	g := gitlabServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v4/projects/group%2Fsub%2Fproject/merge_requests": func(w http.ResponseWriter, r *http.Request) {
			asked = r.URL.RawQuery
			_, _ = w.Write([]byte(`[
				{"iid":3,"web_url":"https://gitlab.com/x/3","title":"from a fork","state":"opened",
				 "source_branch":"spov/job","source_project_id":9,"target_project_id":2},
				{"iid":4,"web_url":"https://gitlab.com/group/sub/project/-/merge_requests/4","title":"the job's","state":"opened",
				 "source_branch":"spov/job","source_project_id":2,"target_project_id":2}]`))
		},
	})
	pr, err := g.FindPullRequest(context.Background(), RepoRef{Owner: "group/sub", Name: "project"}, "spov/job")
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.Number != 4 || pr.State != "opened" {
		t.Fatalf("pr = %+v", pr)
	}
	// Every state: a merged merge request is verified too (DL-051).
	if !strings.Contains(asked, "source_branch=spov%2Fjob") || !strings.Contains(asked, "state=all") {
		t.Errorf("query = %q", asked)
	}
	if !pr.Open {
		t.Errorf("an opened merge request is not open: %+v", pr)
	}
	none, err := g.FindPullRequest(context.Background(), RepoRef{Owner: "group/sub", Name: "project"}, "")
	if none != nil || err != nil {
		t.Errorf("an empty branch matched: %+v, %v", none, err)
	}
}

// The check reports the user, scopes and expiry, refuses a token
// without the api scope, and names a bad token as one, with the token
// in no error.
//
// @decision(DL-041)
func TestGitLabInspectReportsTheTokenAndWhatItLacks(t *testing.T) {
	self := `{"scopes":["api","read_user"],"expires_at":"2026-11-30"}`
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"/api/v4/user":                        write(`{"username":"spov-bot"}`),
		"/api/v4/personal_access_tokens/self": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(self)) },
		"/api/v4/version":                     write(`{"version":"18.4.1-ee"}`),
	}
	g := gitlabServer(t, routes)
	info, err := g.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	if info.User != "spov-bot" || !info.ExpiresAt.Equal(want) || info.ServerVersion != "18.4.1-ee" || len(info.Scopes) != 2 {
		t.Errorf("info = %+v", info)
	}

	self = `{"scopes":["read_repository"]}`
	_, err = g.Inspect(context.Background())
	if !errors.Is(err, ErrMissingPermission) || !strings.Contains(err.Error(), "read_repository") {
		t.Errorf("err = %v", err)
	}

	bad, _ := NewGitLab(GitLabConfig{Name: "gitlab", BaseURL: g.baseURL, Token: "glpat-wrong-token-value"})
	_, err = bad.Inspect(context.Background())
	if !errors.Is(err, ErrBadToken) || strings.Contains(err.Error(), "glpat-") {
		t.Errorf("err = %v", err)
	}

	delete(routes, "/api/v4/personal_access_tokens/self")
	_, err = g.Inspect(context.Background())
	if !errors.Is(err, ErrUnsupportedServer) || strings.Contains(err.Error(), "404 404") {
		t.Errorf("err = %v", err)
	}
}

// A 404 outside the token check is a missing project, never an
// unsupported server.
//
// @decision(DL-041)
func TestAMissingProjectIsNotAnUnsupportedServer(t *testing.T) {
	g := gitlabServer(t, nil)
	_, err := g.FindPullRequest(context.Background(), RepoRef{Owner: "gone", Name: "project"}, "b")
	if err == nil || errors.Is(err, ErrUnsupportedServer) || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v", err)
	}
	if got := truncateRunes("ååå", 3); got != "å" {
		t.Errorf("truncateRunes = %q", got)
	}
}

// The stored token goes to the sandbox with GitLab's git username.
//
// @decision(DL-041)
func TestGitLabHandsOutItsStoredToken(t *testing.T) {
	g, err := NewGitLab(GitLabConfig{Name: "gitlab", Token: gitlabToken})
	if err != nil {
		t.Fatal(err)
	}
	if g.Host() != "gitlab.com" || g.Kind() != KindGitLab || g.BasePath() != "" {
		t.Errorf("host %q kind %q base %q", g.Host(), g.Kind(), g.BasePath())
	}
	tok, err := g.IssueSandboxToken(context.Background(), []Repo{{Owner: "a/b", Name: "c"}}, time.Hour)
	if err != nil || tok.Username != GitLabUser || tok.Secret != gitlabToken || tok.Revoke != nil {
		t.Errorf("token = %+v, %v", tok, err)
	}
	if Renewable(g) {
		t.Error("a stored GitLab token is renewable")
	}
	self, _ := NewGitLab(GitLabConfig{Name: "x", BaseURL: "https://git.example.com/gitlab/", Token: "t"})
	if self.BasePath() != "/gitlab" || self.Host() != "git.example.com" {
		t.Errorf("self-managed: base %q host %q", self.BasePath(), self.Host())
	}
	if _, err := NewGitLab(GitLabConfig{Name: "x"}); err == nil {
		t.Error("a platform without a token was built")
	}
}

// A namespace of several segments is a GitLab thing only, and every
// segment is held to the same alphabet.
//
// @decision(DL-041)
func TestCheckRepoPath(t *testing.T) {
	for _, c := range []struct {
		kind, owner, name string
		ok                bool
	}{
		{KindGitLab, "group/sub", "project", true},
		{KindGitHub, "acme", ".github", true},
		{KindGitHub, "group/sub", "project", false},
		{KindForgejo, "a/b", "c", false},
		{KindGitLab, "group//sub", "p", false},
		{KindGitLab, "group/../sub", "p", false},
		{KindGitLab, "group", "-p", false},
		{KindGitLab, "group", "p%2F", false},
		{KindGitLab, "", "p", false},
		{KindGitea, "me", "a b", false},
	} {
		err := CheckRepoPath(c.kind, c.owner, c.name)
		if (err == nil) != c.ok {
			t.Errorf("%s %s/%s: err = %v", c.kind, c.owner, c.name, err)
		}
	}
	for path, want := range map[string]string{"a/b/c": "a/b|c", "a/b": "a|b", "a": "", "/b": "", "a/": ""} {
		owner, name, err := SplitPath(path)
		got := ""
		if err == nil {
			got = fmt.Sprintf("%s|%s", owner, name)
		}
		if got != want {
			t.Errorf("SplitPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// A project's numeric ID is read by its encoded path; one that cannot be
// read is left out.
//
// @decision(DL-034)
func TestGitLabProjectIDsAreReadByPath(t *testing.T) {
	g := gitlabServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v4/projects/group%2Fsub%2Fproject": write(`{"id":42}`),
	})
	sel := Selection{Platform: g, Repos: []Repo{{Owner: "group/sub", Name: "project"}, {Owner: "gone", Name: "x"}}}
	if got := ProjectIDs(context.Background(), sel); len(got) != 1 || got[0] != "42" {
		t.Errorf("ids = %v", got)
	}
	if got := ProjectIDs(context.Background(), Selection{Platform: NewFake(), Repos: sel.Repos}); got != nil {
		t.Errorf("a platform without IDs gave %v", got)
	}
}

// A merged merge request is found merged.
//
// @decision(DL-051)
func TestGitLabFindsAMergedMergeRequest(t *testing.T) {
	g := gitlabServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v4/projects/group%2Fproject/merge_requests": write(`[
			{"iid":7,"web_url":"https://gitlab.com/group/project/-/merge_requests/7","state":"closed",
			 "source_branch":"spov/x","source_project_id":2,"target_project_id":2},
			{"iid":6,"web_url":"https://gitlab.com/group/project/-/merge_requests/6","state":"merged",
			 "source_branch":"spov/x","source_project_id":2,"target_project_id":2}]`),
	})
	pr, err := g.FindPullRequest(context.Background(), RepoRef{Owner: "group", Name: "project"}, "spov/x")
	if err != nil || pr == nil || pr.Number != 6 || !pr.Merged {
		t.Fatalf("pr = %+v, %v; want !6 merged", pr, err)
	}
}
