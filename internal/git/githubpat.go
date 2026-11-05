package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// GitHubPATUser is the username git wants alongside a personal access
// token. GitHub ignores it, but it must not be empty.
//
// @decision(DL-042)
const GitHubPATUser = "x-access-token"

// GitHubPAT is a GitHub platform backed by one user's personal access
// token rather than an App installation: the user tier of DL-042, weaker
// than the App because spov did not mint the token, cannot narrow it and
// cannot revoke it.
//
// @decision(DL-042) @decision(DL-041)
type GitHubPAT struct {
	name   string
	host   string
	apiURL string
	token  string
	http   *http.Client
}

// GitHubPATConfig configures a token-backed GitHub platform.
//
// @decision(DL-042)
type GitHubPATConfig struct {
	// Name is the platform name, used as the credential key.
	Name string
	// BaseURL is the web root; empty means https://github.com.
	BaseURL string
	// APIURL is the REST API root; empty is derived from BaseURL.
	APIURL string
	// Token is the user's personal access token. It must never be
	// logged and never appears in an error.
	Token string
	// HTTP overrides the HTTP client, for tests.
	HTTP *http.Client
}

// NewGitHubPAT builds a token-backed GitHub platform. It makes no
// network call: the token is exercised by ListRepos and by Verify.
//
// @decision(DL-042)
func NewGitHubPAT(cfg GitHubPATConfig) (*GitHubPAT, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("git: github platform needs a name")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("git: github platform %q needs a token", cfg.Name)
	}
	host := "github.com"
	if cfg.BaseURL != "" {
		var err error
		if host, err = hostFromURL(cfg.BaseURL); err != nil {
			return nil, err
		}
	}
	apiURL, err := githubAPIURL(cfg.APIURL, cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("git: github platform %q: %w", cfg.Name, err)
	}
	g := &GitHubPAT{name: cfg.Name, host: host, apiURL: apiURL, token: cfg.Token, http: cfg.HTTP}
	if g.http == nil {
		g.http = &http.Client{Timeout: 30 * time.Second}
	}
	return g, nil
}

func (g *GitHubPAT) Name() string { return g.name }
func (g *GitHubPAT) Host() string { return g.host }
func (g *GitHubPAT) Kind() string { return "github" }

// TokenScope reports the user's claim rather than a verified fact: spov
// did not create this token and cannot read its scoping back.
//
// @decision(DL-042) @decision(DL-041)
func (g *GitHubPAT) TokenScope() Scope {
	return Scope{
		PerRepo: false,
		Description: "your own GitHub token, with whatever repositories you granted it;" +
			" spov cannot narrow or revoke it",
	}
}

// ListRepos returns the repositories the token can reach.
//
// @decision(DL-042)
func (g *GitHubPAT) ListRepos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if page > githubMaxPages {
			return nil, fmt.Errorf("git: %s returned more than %d pages of repositories; "+
				"it may be ignoring the page parameter", g.host, githubMaxPages)
		}
		var body []githubRepoJSON
		path := fmt.Sprintf("/user/repos?per_page=%d&page=%d&affiliation=owner,collaborator,organization_member",
			githubPageSize, page)
		if err := g.do(ctx, http.MethodGet, path, nil, &body); err != nil {
			return nil, fmt.Errorf("git: list repositories on %s: %w", g.host, err)
		}
		for _, r := range body {
			out = append(out, r.toRepo())
		}
		if len(body) < githubPageSize {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("git: %s: %w", g.host, ErrNoRepos)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// IssueSandboxToken hands back the user's own token. There is nothing
// to mint and nothing to revoke, so Revoke is nil and the token carries
// no expiry spov knows of.
//
// @decision(DL-042)
func (g *GitHubPAT) IssueSandboxToken(ctx context.Context, repos []Repo, _ time.Duration) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	if len(repos) == 0 {
		return Token{}, fmt.Errorf("git: %s: no repositories selected", g.host)
	}
	return Token{Username: GitHubPATUser, Secret: g.token}, nil
}

// IssueWideToken hands back the user's own token, which reaches whatever
// they gave it.
//
// @decision(DL-043)
func (g *GitHubPAT) IssueWideToken(ctx context.Context, _ time.Duration) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	return Token{Username: GitHubPATUser, Secret: g.token}, nil
}

// Login is the account the token belongs to, for display after a
// credential is stored.
//
// @decision(DL-042)
func (g *GitHubPAT) Login(ctx context.Context) (string, error) {
	var body struct {
		Login string `json:"login"`
	}
	if err := g.do(ctx, http.MethodGet, "/user", nil, &body); err != nil {
		return "", err
	}
	if body.Login == "" {
		// A fine-grained token without the "read:user" grant answers
		// 200 with no login. The token still works for repositories,
		// so this is not a failure — there is simply no name to show.
		return "", nil
	}
	return body.Login, nil
}

// FindPullRequest is gate 2 for a pr job on a token-backed GitHub
// platform. It shares GitHub App's query and differs only in the
// credential: the stored token, already scoped by whoever created it.
//
// @decision(DL-051)
func (g *GitHubPAT) FindPullRequest(ctx context.Context, repo RepoRef, branch string) (*PullRequest, error) {
	return findPullRequestWith(
		ctx,
		// The auth argument is unused here: do sets the stored token
		// itself, so there is nothing for the caller to pass.
		func(ctx context.Context, method, path, _ string, body, out any) error {
			return g.do(ctx, method, path, body, out)
		},
		g.host, "", repo, branch,
	)
}

// do issues one authenticated request. The token is set as a header and
// never appears in a URL or an error.
//
// @decision(DL-042)
func (g *GitHubPAT) do(ctx context.Context, method, path string, request, out any) error {
	var payload io.Reader
	if request != nil {
		encoded, err := json.Marshal(request)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.apiURL+path, payload)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if request != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return githubError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// CheckRepo asks GitHub whether the token can see repo: a 404 is
// ErrRepoUnreachable, and any other answer settles nothing, a 403 among
// them, since GitHub answers a rate limit with one.
//
// @decision(DL-043)
func (g *GitHubPAT) CheckRepo(ctx context.Context, repo Repo) error {
	ctx, cancel := context.WithTimeout(ctx, repoCheckTimeout)
	defer cancel()
	err := g.do(ctx, http.MethodGet, "/repos/"+url.PathEscape(repo.Owner)+"/"+url.PathEscape(repo.Name), nil, nil)
	var status *githubStatusError
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return ErrRepoUnreachable
	}
	return nil
}

// IsFineGrainedGitHubPAT reports whether a GitHub token is a
// fine-grained personal access token, by its prefix.
//
// @decision(DL-042)
func IsFineGrainedGitHubPAT(secret string) bool {
	return strings.HasPrefix(strings.TrimSpace(secret), "github_pat_")
}

// LooksLikeGitHubPAT reports whether a credential is a personal access
// token rather than the JSON App credential of DL-041. It is what lets
// one platform entry accept either tier.
//
// @decision(DL-042)
func LooksLikeGitHubPAT(secret string) bool {
	s := strings.TrimSpace(secret)
	if s == "" || strings.HasPrefix(s, "{") {
		return false
	}
	for _, prefix := range []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
