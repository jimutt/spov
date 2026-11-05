package git

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// checkedFake is a Fake whose forge says which repositories its
// credential cannot see.
type checkedFake struct {
	*Fake
	hidden  map[string]bool
	mu      sync.Mutex
	asked   []string
	failAll bool
}

func (c *checkedFake) CheckRepo(_ context.Context, repo Repo) error {
	c.mu.Lock()
	c.asked = append(c.asked, repo.Slug())
	c.mu.Unlock()
	if c.failAll {
		return errors.New("the forge did not answer")
	}
	if c.hidden[repo.Slug()] {
		return ErrRepoUnreachable
	}
	return nil
}

// A stored token that cannot see a repository refuses the launch before
// anything is minted or cloned, naming the repository, with git access
// all as with repos; an answer that settles nothing lets it go on, and a
// renewable token is not asked.
//
// @decision(DL-043)
func TestALaunchRefusesARepositoryItsTokenCannotSee(t *testing.T) {
	repos := []Repo{
		{Owner: "acme", Name: "app", CloneURL: "https://git.example.com/acme/app.git"},
		{Owner: "acme", Name: "hidden", CloneURL: "https://git.example.com/acme/hidden.git"},
	}
	c := &checkedFake{Fake: NewFake(), hidden: map[string]bool{"acme/hidden": true}}
	_, _, err := Bootstrap(context.Background(), []Selection{{Platform: c, Repos: repos}}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "cannot see acme/hidden") {
		t.Fatalf("Bootstrap = %v, want the hidden repository named", err)
	}
	if len(c.issued) != 0 {
		t.Error("a token was minted for a launch that was refused")
	}

	wide := &checkedFake{Fake: NewFake(), hidden: map[string]bool{"acme/hidden": true}}
	if _, _, err := Bootstrap(context.Background(), []Selection{{Platform: wide, Repos: repos, All: true}}, time.Hour); err == nil ||
		!strings.Contains(err.Error(), "cannot see acme/hidden") {
		t.Errorf("git access all skipped the check: %v", err)
	}

	unsure := &checkedFake{Fake: NewFake(), failAll: true}
	if _, _, err := Bootstrap(context.Background(), []Selection{{Platform: unsure, Repos: repos}}, time.Hour); err != nil {
		t.Errorf("a forge that did not answer refused the launch: %v", err)
	}

	renewable := &checkedFake{Fake: NewFake(), hidden: map[string]bool{"acme/hidden": true}}
	renewable.Renews = true
	if _, _, err := Bootstrap(context.Background(), []Selection{{Platform: renewable, Repos: repos}}, time.Hour); err != nil || len(renewable.asked) != 0 {
		t.Errorf("a renewable token was checked: %v, asked %v", err, renewable.asked)
	}
}

// Each stored-token client reads the one repository and says
// unreachable only for the forge's not-found (or, where it means the
// same, forbidden) answer.
//
// @decision(DL-043)
func TestEachForgeSaysWhenItsTokenCannotSeeARepository(t *testing.T) {
	status := map[string]int{"/repos/acme/app": 200, "/repos/acme/gone": 404, "/repos/acme/denied": 403, "/repos/acme/broken": 500}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := status[r.URL.Path]
		if code == 0 {
			code = 404
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"message":"x"}`))
	}))
	defer srv.Close()
	pat, err := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_secret"})
	if err != nil {
		t.Fatal(err)
	}
	// A 403 is GitHub's answer to a rate limit too, so it settles nothing.
	for name, want := range map[string]bool{"app": false, "gone": true, "denied": false, "broken": false} {
		got := errors.Is(pat.CheckRepo(context.Background(), Repo{Owner: "acme", Name: name}), ErrRepoUnreachable)
		if got != want {
			t.Errorf("GitHub %s: unreachable = %v, want %v", name, got, want)
		}
	}

	gl := gitlabServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v4/projects/group%2Fsub%2Fapp": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id":7}`)) },
	})
	if err := gl.CheckRepo(context.Background(), Repo{Owner: "group/sub", Name: "app"}); err != nil {
		t.Errorf("GitLab, a visible project: %v", err)
	}
	if err := gl.CheckRepo(context.Background(), Repo{Owner: "group/sub", Name: "gone"}); !errors.Is(err, ErrRepoUnreachable) {
		t.Errorf("GitLab, a hidden project: %v", err)
	}

	fj := newTestForgejo(&fakeForgejoAPI{hidden: []string{"acme/gone"}})
	if err := fj.CheckRepo(context.Background(), Repo{Owner: "acme", Name: "gone"}); !errors.Is(err, ErrRepoUnreachable) {
		t.Errorf("Forgejo, a hidden repository: %v", err)
	}
	if err := fj.CheckRepo(context.Background(), Repo{Owner: "acme", Name: "app"}); err != nil {
		t.Errorf("Forgejo, a visible repository: %v", err)
	}
}
