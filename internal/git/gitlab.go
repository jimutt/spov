package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// GitLabUser is the username git sends with a GitLab token. GitLab
// ignores it for a personal access token, but git needs one.
//
// @decision(DL-041)
const GitLabUser = "oauth2"

// GitLabScope is the token scope spov needs: glab and merge requests
// use the API, and git push needs write_repository, which api implies.
//
// @decision(DL-041)
const GitLabScope = "api"

// gitlabPageSize is the page size for listings; 100 is GitLab's most.
const gitlabPageSize = 100

// gitlabMaxPages bounds the listing loops, as forgejoMaxPages does.
const gitlabMaxPages = 200

// GitLab is a GitLab platform on gitlab.com or a self-managed instance,
// backed by a stored personal access token.
//
// @decision(DL-041)
type GitLab struct {
	name    string
	host    string
	baseURL string
	apiURL  string
	// token is the stored personal access token. It must never be
	// logged and never appears in an error.
	token string
	http  *http.Client
}

// GitLabConfig configures a GitLab platform.
//
// @decision(DL-041)
type GitLabConfig struct {
	// Name is the platform name, used as the credential key.
	Name string
	// BaseURL is the instance root; empty means https://gitlab.com.
	BaseURL string
	// Token is a personal access token. It must never be logged.
	Token string
	// HTTP overrides the HTTP client, for tests.
	HTTP *http.Client
}

// NewGitLab builds a GitLab platform. It makes no network call.
//
// @decision(DL-041)
func NewGitLab(cfg GitLabConfig) (*GitLab, error) {
	if cfg.Name == "" {
		return nil, errors.New("git: gitlab platform needs a name")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("git: gitlab platform %q needs a token", cfg.Name)
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://gitlab.com"
	}
	host, err := hostFromURL(base)
	if err != nil {
		return nil, err
	}
	g := &GitLab{name: cfg.Name, host: host, baseURL: base, apiURL: base + "/api/v4", token: cfg.Token, http: cfg.HTTP}
	if g.http == nil {
		g.http = &http.Client{Timeout: 30 * time.Second}
	}
	return g, nil
}

func (g *GitLab) Name() string { return g.name }
func (g *GitLab) Host() string { return g.host }
func (g *GitLab) Kind() string { return KindGitLab }

// BasePath is the path below the host the instance is served under,
// empty at the root.
//
// @decision(DL-034)
func (g *GitLab) BasePath() string {
	u, err := url.Parse(g.baseURL)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

// TokenScope reports the user's claim: a personal access token reaches
// every project its owner can, and spov cannot narrow it.
//
// @decision(DL-041)
func (g *GitLab) TokenScope() Scope {
	return Scope{
		PerRepo: false,
		Description: "the stored GitLab token, which reaches every project its account can;" +
			" use an account with access to only the projects its sessions should reach",
	}
}

// gitlabProject is the part of a project spov reads.
type gitlabProject struct {
	ID             int64     `json:"id"`
	Path           string    `json:"path"`
	HTTPURLToRepo  string    `json:"http_url_to_repo"`
	Visibility     string    `json:"visibility"`
	Archived       bool      `json:"archived"`
	LastActivityAt time.Time `json:"last_activity_at"`
	Namespace      struct {
		FullPath string `json:"full_path"`
	} `json:"namespace"`
}

// ListRepos returns the projects the token's account is a member of,
// newest activity first. A project's namespace is its whole group path.
//
// @decision(DL-041)
func (g *GitLab) ListRepos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if page > gitlabMaxPages {
			return nil, fmt.Errorf("git: %s returned more than %d pages of projects; it may be ignoring the page parameter",
				g.host, gitlabMaxPages)
		}
		var body []gitlabProject
		path := fmt.Sprintf("/projects?membership=true&order_by=last_activity_at&per_page=%d&page=%d", gitlabPageSize, page)
		if err := g.do(ctx, path, &body); err != nil {
			return nil, fmt.Errorf("git: list projects on %s: %w", g.host, err)
		}
		for _, p := range body {
			out = append(out, Repo{
				Owner:     p.Namespace.FullPath,
				Name:      p.Path,
				CloneURL:  p.HTTPURLToRepo,
				Private:   p.Visibility != "public",
				Archived:  p.Archived,
				UpdatedAt: p.LastActivityAt,
			})
		}
		if len(body) < gitlabPageSize {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("git: %s: %w", g.host, ErrNoRepos)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// IssueSandboxToken hands back the stored token. There is nothing to
// mint and nothing to revoke.
//
// @decision(DL-041)
func (g *GitLab) IssueSandboxToken(ctx context.Context, repos []Repo, _ time.Duration) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	if len(repos) == 0 {
		return Token{}, fmt.Errorf("git: %s: no repositories selected", g.host)
	}
	return Token{Username: GitLabUser, Secret: g.token}, nil
}

// IssueWideToken hands back the stored token, which reaches whatever
// its account can.
//
// @decision(DL-043)
func (g *GitLab) IssueWideToken(ctx context.Context, _ time.Duration) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	return Token{Username: GitLabUser, Secret: g.token}, nil
}

// ProjectIDs are the numeric IDs of repos, keyed by slug, for the proxy:
// GitLab's API names a project by its ID as well as its path. A project
// that cannot be read is left out, and its ID then gets no credential.
//
// @decision(DL-034)
func (g *GitLab) ProjectIDs(ctx context.Context, repos []Repo) map[string]string {
	// A slow GitLab must not hold up a launch for long: the lookups run
	// together, under one short deadline.
	ctx, cancel := context.WithTimeout(ctx, projectIDTimeout)
	defer cancel()
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = map[string]string{}
	)
	for _, r := range repos {
		wg.Go(func() {
			var p struct {
				ID int64 `json:"id"`
			}
			if err := g.do(ctx, "/projects/"+ProjectPath(RepoRef{Owner: r.Owner, Name: r.Name}), &p); err == nil && p.ID > 0 {
				mu.Lock()
				out[r.Slug()] = strconv.FormatInt(p.ID, 10)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

// CheckRepo asks GitLab whether the token can see the project: a 404 is
// ErrRepoUnreachable (GitLab answers a project it hides with 404), and
// any other failure settles nothing.
//
// @decision(DL-043)
func (g *GitLab) CheckRepo(ctx context.Context, repo Repo) error {
	ctx, cancel := context.WithTimeout(ctx, repoCheckTimeout)
	defer cancel()
	var p struct {
		ID int64 `json:"id"`
	}
	err := g.do(ctx, "/projects/"+ProjectPath(RepoRef{Owner: repo.Owner, Name: repo.Name}), &p)
	var status *gitlabStatusError
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return ErrRepoUnreachable
	}
	return nil
}

// projectIDTimeout bounds the project ID lookups of one launch.
const projectIDTimeout = 5 * time.Second

// projectIDer is a platform whose API also names a repository by a
// numeric ID.
type projectIDer interface {
	ProjectIDs(ctx context.Context, repos []Repo) map[string]string
}

// ProjectIDs are a selection's numeric repository IDs, when its platform
// has them.
//
// @decision(DL-034)
func ProjectIDs(ctx context.Context, sel Selection) []string {
	p, ok := sel.Platform.(projectIDer)
	if !ok || len(sel.Repos) == 0 {
		return nil
	}
	ids := p.ProjectIDs(ctx, sel.Repos)
	out := make([]string, 0, len(ids))
	for _, r := range sel.Repos {
		if id, ok := ids[r.Slug()]; ok {
			out = append(out, id)
		}
	}
	return out
}

// ProjectPath is a project's API identifier: its full path, encoded as
// one segment.
//
// @decision(DL-041)
func ProjectPath(repo RepoRef) string {
	return url.PathEscape(repo.Owner + "/" + repo.Name)
}

// FindPullRequest is gate 2 on GitLab: the best merge request in any
// state whose source is branch, from the project itself rather than a
// fork.
//
// @decision(DL-050) @decision(DL-051)
func (g *GitLab) FindPullRequest(ctx context.Context, repo RepoRef, branch string) (*PullRequest, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, nil
	}
	var best *PullRequest
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if page > gitlabMaxPages {
			return nil, fmt.Errorf("git: %s returned more than %d pages of merge requests; it may be ignoring the page parameter",
				g.host, gitlabMaxPages)
		}
		var body []struct {
			IID             int       `json:"iid"`
			WebURL          string    `json:"web_url"`
			Title           string    `json:"title"`
			State           string    `json:"state"`
			SourceBranch    string    `json:"source_branch"`
			SourceProjectID int64     `json:"source_project_id"`
			TargetProjectID int64     `json:"target_project_id"`
			CreatedAt       time.Time `json:"created_at"`
		}
		// Every state: a merged merge request is verified too (DL-051).
		path := fmt.Sprintf("/projects/%s/merge_requests?state=all&source_branch=%s&per_page=%d&page=%d",
			ProjectPath(repo), url.QueryEscape(branch), gitlabPageSize, page)
		if err := g.do(ctx, path, &body); err != nil {
			return nil, fmt.Errorf("git: list merge requests on %s: %w", g.host, err)
		}
		for _, mr := range body {
			if mr.SourceBranch != branch || mr.SourceProjectID != mr.TargetProjectID {
				continue
			}
			best = bestPullRequest(best, PullRequest{Number: mr.IID, URL: mr.WebURL, Title: mr.Title, State: mr.State,
				Open: mr.State == "opened", Merged: mr.State == "merged", Created: mr.CreatedAt})
			if best.Open {
				return best, nil
			}
		}
		if len(body) < gitlabPageSize {
			return best, nil
		}
	}
}

// Inspect reads the token's scopes and expiry, then its user and the
// server's version. The scopes come first because they answer for any
// token, while /user needs one of the read scopes, so a token without
// api is refused for that and not for an unreadable user.
//
// @decision(DL-041)
func (g *GitLab) Inspect(ctx context.Context) (TokenInfo, error) {
	var self struct {
		Scopes    []string `json:"scopes"`
		ExpiresAt string   `json:"expires_at"`
	}
	if err := g.do(ctx, "/personal_access_tokens/self", &self); err != nil {
		return TokenInfo{}, checkError(err)
	}
	info := TokenInfo{Scopes: self.Scopes}
	if self.ExpiresAt != "" {
		if at, err := time.Parse(time.DateOnly, self.ExpiresAt); err == nil {
			info.ExpiresAt = at
		}
	}
	if !slices.Contains(self.Scopes, GitLabScope) {
		return info, fmt.Errorf("%w: it needs the %s scope and has %s", ErrMissingPermission, GitLabScope, scopeList(self.Scopes))
	}
	var user struct {
		Username string `json:"username"`
	}
	if err := g.do(ctx, "/user", &user); err != nil {
		return TokenInfo{}, checkError(err)
	}
	info.User = user.Username
	var version struct {
		Version string `json:"version"`
	}
	if err := g.do(ctx, "/version", &version); err == nil {
		info.ServerVersion = version.Version
	}
	return info, nil
}

// scopeList is a token's scopes for a message.
func scopeList(scopes []string) string {
	if len(scopes) == 0 {
		return "none"
	}
	return strings.Join(scopes, ", ")
}

// do issues one GET to the API and decodes its body into out. The token
// is a header and never appears in a URL or an error.
//
// @decision(DL-041)
func (g *GitLab) do(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return gitlabError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// gitlabStatusError is a response GitLab refused, with its own short
// message and never the body as it is.
type gitlabStatusError struct {
	code int
	msg  string
}

func (e *gitlabStatusError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("GitLab answered %d", e.code)
	}
	return "GitLab answered " + e.msg
}

// gitlabError reads a failed response's message. GitLab's own messages
// start with the status ("401 Unauthorized"); one that does not gets it
// added.
//
// @decision(DL-041)
func gitlabError(resp *http.Response) error {
	var body struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	msg := body.Error
	if s, ok := body.Message.(string); ok && s != "" {
		msg = s
	}
	msg = truncateRunes(msg, 200)
	code := strconv.Itoa(resp.StatusCode)
	if msg != "" && !strings.HasPrefix(msg, code) {
		msg = code + " " + msg
	}
	return &gitlabStatusError{code: resp.StatusCode, msg: msg}
}

// checkError is a token check's error: a refused response becomes the
// failure a person can act on. Only the token check maps statuses so; a
// 404 elsewhere is a missing project, not an unsupported server.
//
// @decision(DL-041)
func checkError(err error) error {
	var se *gitlabStatusError
	if errors.As(err, &se) {
		if mapped := statusError(se.code); mapped != nil {
			return fmt.Errorf("%w (%s)", mapped, se.msg)
		}
	}
	return err
}

// truncateRunes cuts s to at most n bytes without splitting a rune.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

var _ Platform = (*GitLab)(nil)
