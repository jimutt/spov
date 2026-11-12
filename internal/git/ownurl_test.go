package git

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAPullRequestURLIsKeptOnlyOnItsOwnForge: a forge's answer is stored
// as a link, so a scheme other than https (or http on a forge configured
// with http), another host or user information is dropped.
//
// @decision(DL-135)
func TestAPullRequestURLIsKeptOnlyOnItsOwnForge(t *testing.T) {
	for _, c := range []struct {
		raw, forge string
		want       bool
	}{
		{"https://codefloe.com/a/b/pulls/1", "https://codefloe.com", true},
		{"https://CODEFLOE.com/a/b/pulls/1", "https://codefloe.com", true},
		{"https://git.example:8443/a/b/pulls/1", "https://git.example:8443", true},
		{"http://git.example/a/b/pulls/1", "http://git.example", true},
		{"http://codefloe.com/a/b/pulls/1", "https://codefloe.com", false},
		{"javascript:alert(1)", "https://codefloe.com", false},
		{"data:text/html,x", "https://codefloe.com", false},
		{"//codefloe.com/a/b", "https://codefloe.com", false},
		{"https://evil.example/a/b/pulls/1", "https://codefloe.com", false},
		{"https://git.example:9/a", "https://git.example:8443", false},
		{"https://user:pw@codefloe.com/a", "https://codefloe.com", false},
		{"", "https://codefloe.com", false},
	} {
		if got := OwnURL(c.raw, c.forge) != ""; got != c.want {
			t.Errorf("OwnURL(%q, %q) kept = %v, want %v", c.raw, c.forge, got, c.want)
		}
	}
}

// TestAHostileForgeCannotPutAnotherAddressInAPullRequest: through the
// query itself, on a token-backed GitHub platform.
//
// @decision(DL-135)
func TestAHostileForgeCannotPutAnotherAddressInAPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"number":74,"html_url":"javascript:alert(1)","title":"t","state":"open"}]`))
	}))
	defer srv.Close()
	p, err := NewGitHubPAT(GitHubPATConfig{Name: "gh", APIURL: srv.URL, Token: "github_pat_secret"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.FindPullRequest(context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "impl/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Number != 74 || got.URL != "" {
		t.Fatalf("got %+v, want pull request 74 with no URL", got)
	}
}
