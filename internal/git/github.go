package git

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// GitHubTokenLifetime is how long an installation token lives. GitHub
// fixes this at one hour and offers no way to extend it and no refresh
// mechanism.
//
// @decision(DL-041)
const GitHubTokenLifetime = time.Hour

// GitHubTokenUser is the git credential username for an installation
// token. GitHub documents no alternative.
//
// @decision(DL-041)
const GitHubTokenUser = "x-access-token"

// defaultGitHubAPI is the REST API root for github.com.
const defaultGitHubAPI = "https://api.github.com"

// githubPageSize is the listing page size; GitHub caps per_page at 100.
const githubPageSize = 100

// githubMaxPages bounds the listing loop, in case a server ignores the
// page parameter and keeps returning full pages.
const githubMaxPages = 100

// GitHub mints installation tokens for a GitHub App.
//
// @decision(DL-041)
type GitHub struct {
	name    string
	host    string
	apiURL  string
	account string
	appID   string
	key     *rsa.PrivateKey
	http    *http.Client
	// now supplies timestamps, so JWT signing can be tested.
	now func() time.Time

	mu sync.Mutex
	// installationID is resolved on first use and cached for the process.
	installationID int64
}

// GitHubConfig configures a GitHub App platform.
//
// @decision(DL-041)
type GitHubConfig struct {
	// Name is the platform name from config, used as the credential key.
	Name string
	// BaseURL is the web root; empty means https://github.com.
	BaseURL string
	// APIURL is the REST API root; empty is derived from BaseURL, and
	// defaults to https://api.github.com.
	APIURL string
	// Account is the user or organisation the App is installed on. It is
	// how spov finds the installation when InstallationID is unset.
	Account string
	// AppID is the App ID or client ID, used as the JWT issuer.
	AppID string
	// PrivateKeyPEM is the App's PEM-encoded RSA private key. It must
	// never be logged, and never leaves the user's machine.
	PrivateKeyPEM string
	// InstallationID skips discovery when known.
	InstallationID int64
	// HTTP overrides the HTTP client, for tests.
	HTTP *http.Client
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// NewGitHub builds a GitHub App platform. It performs no network calls:
// the App credentials are only exercised when a token is minted or repos
// are listed.
//
// @decision(DL-041)
func NewGitHub(cfg GitHubConfig) (*GitHub, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("git: github platform needs a name")
	}
	if cfg.AppID == "" {
		return nil, fmt.Errorf("git: github platform %q needs an app id", cfg.Name)
	}
	if cfg.PrivateKeyPEM == "" {
		return nil, fmt.Errorf("git: github platform %q needs an app private key", cfg.Name)
	}
	if cfg.Account == "" && cfg.InstallationID == 0 {
		return nil, fmt.Errorf("git: github platform %q needs an account or an installation id "+
			"so spov can find the App installation", cfg.Name)
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("git: github platform %q: %w", cfg.Name, err)
	}

	host := "github.com"
	if cfg.BaseURL != "" {
		host, err = hostFromURL(cfg.BaseURL)
		if err != nil {
			return nil, err
		}
	}
	apiURL, err := githubAPIURL(cfg.APIURL, cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("git: github platform %q: %w", cfg.Name, err)
	}

	g := &GitHub{
		name:           cfg.Name,
		host:           host,
		apiURL:         apiURL,
		account:        cfg.Account,
		appID:          cfg.AppID,
		key:            key,
		http:           cfg.HTTP,
		now:            cfg.Now,
		installationID: cfg.InstallationID,
	}
	if g.http == nil {
		g.http = &http.Client{Timeout: 30 * time.Second}
	}
	if g.now == nil {
		g.now = time.Now
	}
	return g, nil
}

// githubAPIURL resolves the REST API root. GitHub Enterprise serves it
// under /api/v3 on the same host, while github.com uses api.github.com.
func githubAPIURL(apiURL, baseURL string) (string, error) {
	switch {
	case apiURL != "":
		if _, err := hostFromURL(apiURL); err != nil {
			return "", err
		}
		return strings.TrimRight(apiURL, "/"), nil
	case baseURL == "", baseURL == "https://github.com", baseURL == "https://github.com/":
		return defaultGitHubAPI, nil
	default:
		return strings.TrimRight(baseURL, "/") + "/api/v3", nil
	}
}

func (g *GitHub) Name() string { return g.name }
func (g *GitHub) Host() string { return g.host }
func (g *GitHub) Kind() string { return "github" }

// TokenScope describes what a minted token grants. Unlike Forgejo's
// stored token, this is a property of what spov itself requests rather
// than an unverified claim about the user's configuration.
//
// @decision(DL-041)
func (g *GitHub) TokenScope() Scope {
	return Scope{
		PerRepo: true,
		Description: fmt.Sprintf("an installation token limited to the selected repositories, "+
			"expiring after %s; it can never exceed what the App is installed on", GitHubTokenLifetime),
	}
}

// ListRepos returns the repositories the App installation can access.
//
// @decision(DL-041)
func (g *GitHub) ListRepos(ctx context.Context) ([]Repo, error) {
	// Listing needs an installation token, and it cannot be narrowed:
	// the point is to discover which repositories exist. It is revoked
	// on the way out rather than left live for its full hour, since it
	// is the broadest credential spov ever holds.
	token, err := g.mintToken(ctx, nil, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if token.Revoke == nil {
			return
		}
		// Revocation is best-effort: the listing succeeded or failed on
		// its own merits, and the token expires regardless.
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = token.Revoke(revokeCtx)
	}()

	var out []Repo
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if page > githubMaxPages {
			return nil, fmt.Errorf("git: %s returned more than %d pages of repositories; "+
				"it may be ignoring the page parameter", g.host, githubMaxPages)
		}

		var body struct {
			TotalCount   int              `json:"total_count"`
			Repositories []githubRepoJSON `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", githubPageSize, page)
		if err := g.do(ctx, http.MethodGet, path, "token "+token.Secret, nil, &body); err != nil {
			return nil, fmt.Errorf("git: list repositories on %s: %w", g.host, err)
		}

		for _, r := range body.Repositories {
			out = append(out, r.toRepo())
		}
		// total_count lets the loop stop exactly, rather than spending a
		// request on an empty page whenever the count is a multiple of
		// the page size. A short page still ends it, in case the server
		// does not report a count.
		if len(body.Repositories) < githubPageSize || len(out) >= body.TotalCount {
			break
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("git: %s: %w", g.host, ErrNoRepos)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// githubPullJSON is the little of a pull request a verdict needs.
//
// @decision(DL-050)
type githubPullJSON struct {
	Number   int     `json:"number"`
	HTMLURL  string  `json:"html_url"`
	Title    string  `json:"title"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
	// @decision(DL-051)
	CreatedAt time.Time `json:"created_at"`
}

// findPullRequestWith asks GitHub for the pull requests in any state
// whose head is branch, using an already-minted token, and returns the
// best of them (open, then merged, then closed). GitHub filters by head
// server-side (head=owner:branch), so unlike Forgejo this needs no
// client-side matching; one page of the newest is enough for one branch.
//
// @decision(DL-050) @decision(DL-051)
func findPullRequestWith(
	ctx context.Context,
	do func(ctx context.Context, method, path, auth string, body, out any) error,
	host, authz string, repo RepoRef, branch string,
) (*PullRequest, error) {
	if strings.TrimSpace(branch) == "" {
		// Never a match-anything query.
		return nil, nil
	}
	var body []githubPullJSON
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=all&head=%s:%s&per_page=30",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name),
		url.QueryEscape(repo.Owner), url.QueryEscape(branch))
	if err := do(ctx, http.MethodGet, path, authz, nil, &body); err != nil {
		return nil, fmt.Errorf("git: find pull request on %s: %w", host, err)
	}
	var best *PullRequest
	for _, pr := range body {
		best = bestPullRequest(best, PullRequest{
			Number: pr.Number, URL: pr.HTMLURL, Title: pr.Title, State: pr.State,
			Open: pr.State == "open", Merged: pr.MergedAt != nil && *pr.MergedAt != "", Created: pr.CreatedAt,
		})
	}
	return best, nil
}

// FindPullRequest is gate 2 for a pr job on a GitHub App platform. It
// mints an installation token scoped to the one repository and revokes it
// on the way out.
//
// @decision(DL-051)
func (g *GitHub) FindPullRequest(ctx context.Context, repo RepoRef, branch string) (*PullRequest, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, nil
	}
	token, err := g.mintToken(ctx, []Repo{{Owner: repo.Owner, Name: repo.Name}}, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if token.Revoke == nil {
			return
		}
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = token.Revoke(revokeCtx)
	}()
	return findPullRequestWith(ctx, g.do, g.host, "token "+token.Secret, repo, branch)
}

// RenewsTokens is true: an installation token lasts an hour and is
// minted again while the session is in use.
//
// @decision(DL-021)
func (g *GitHub) RenewsTokens() bool { return true }

// IssueSandboxToken mints an installation token narrowed to repos. It
// expires after an hour; the server mints another into a session that is
// still in use, rather than putting the App private key inside the
// sandbox.
//
// @decision(DL-041) @decision(DL-021)
func (g *GitHub) IssueSandboxToken(ctx context.Context, repos []Repo, _ time.Duration) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	if len(repos) == 0 {
		return Token{}, fmt.Errorf("git: %s: no repositories selected", g.host)
	}
	// The requested TTL is ignored deliberately: GitHub fixes the
	// lifetime at one hour and offers no way to ask for another.
	return g.mintToken(ctx, repos, true)
}

// IssueWideToken mints an installation token for every repository the
// installation covers, still narrowed to SandboxPermissions.
//
// @decision(DL-043) @decision(DL-034)
func (g *GitHub) IssueWideToken(ctx context.Context, _ time.Duration) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	return g.mintToken(ctx, nil, true)
}

// SandboxPermissions are what a sandbox's App token asks for, each
// capped at what the installation was granted. None of them can add a
// key, a webhook or a secret.
//
// @decision(DL-034)
var SandboxPermissions = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"issues":        "write",
	"metadata":      "read",
}

// mintToken creates an installation token, narrowed to repos when given,
// and to SandboxPermissions when narrow is set.
//
// @decision(DL-041) @decision(DL-034)
func (g *GitHub) mintToken(ctx context.Context, repos []Repo, narrow bool) (Token, error) {
	installID, err := g.resolveInstallation(ctx)
	if err != nil {
		return Token{}, err
	}
	jwt, err := g.jwt()
	if err != nil {
		return Token{}, err
	}

	request := map[string]any{}
	if len(repos) > 0 {
		names := make([]string, 0, len(repos))
		for _, r := range repos {
			names = append(names, r.Name)
		}
		// Only `repositories` is sent. GitHub documents `repository_ids`
		// as an alternative and does not say whether sending both is an
		// error, so spov does not find out.
		request["repositories"] = names
	}
	if narrow {
		request["permissions"] = g.sandboxPermissions(ctx, installID, jwt)
	}

	var body struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installID)
	// No body at all for an unnarrowed token, as before.
	var payload any
	if len(request) > 0 {
		payload = request
	}
	if err := g.do(ctx, http.MethodPost, path, "Bearer "+jwt, payload, &body); err != nil {
		return Token{}, fmt.Errorf("git: mint an installation token for %s: %w", g.host, err)
	}
	if body.Token == "" {
		return Token{}, fmt.Errorf("git: %s returned an empty installation token", g.host)
	}

	expires, err := time.Parse(time.RFC3339, body.ExpiresAt)
	if err != nil {
		// A token that works but whose expiry cannot be read is still
		// usable; fall back to the documented lifetime rather than
		// failing the launch.
		expires = g.now().Add(GitHubTokenLifetime)
	}

	secret := body.Token
	return Token{
		Username:  GitHubTokenUser,
		Secret:    secret,
		ExpiresAt: expires,
		Revoke: func(ctx context.Context) error {
			return g.revoke(ctx, secret)
		},
	}, nil
}

// sandboxPermissions is SandboxPermissions capped at the installation's
// own grants: one it lacks is left out and a write it holds only as read
// is asked as read, since asking for more fails the mint. When the
// installation cannot be read, SandboxPermissions is asked as it is.
//
// @decision(DL-034)
func (g *GitHub) sandboxPermissions(ctx context.Context, installID int64, jwt string) map[string]string {
	var body struct {
		Permissions map[string]string `json:"permissions"`
	}
	path := fmt.Sprintf("/app/installations/%d", installID)
	if err := g.do(ctx, http.MethodGet, path, "Bearer "+jwt, nil, &body); err != nil || len(body.Permissions) == 0 {
		return maps.Clone(SandboxPermissions)
	}
	out := map[string]string{}
	for name, want := range SandboxPermissions {
		have, ok := body.Permissions[name]
		if !ok {
			continue
		}
		if want == "write" && have == "read" {
			want = "read"
		}
		out[name] = want
	}
	if len(out) == 0 {
		return maps.Clone(SandboxPermissions)
	}
	return out
}

// revoke releases an installation token early. GitHub authenticates this
// with the token being revoked, so a token can only revoke itself.
//
// @decision(DL-041)
func (g *GitHub) revoke(ctx context.Context, secret string) error {
	if err := g.do(ctx, http.MethodDelete, "/installation/token", "token "+secret, nil, nil); err != nil {
		return fmt.Errorf("git: revoke the installation token for %s: %w", g.host, err)
	}
	return nil
}

// resolveInstallation finds the App installation, caching it for the
// process. Discovery needs the JWT rather than an installation token.
//
// @decision(DL-041)
func (g *GitHub) resolveInstallation(ctx context.Context) (int64, error) {
	// The lock is held across the lookup, not just around the cache
	// read: releasing it first lets concurrent callers all miss and all
	// run the same two API calls against GitHub's rate limit. Discovery
	// is two requests once per process, so serialising it costs nothing
	// that matters.
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.installationID != 0 {
		return g.installationID, nil
	}

	jwt, err := g.jwt()
	if err != nil {
		return 0, err
	}

	// A user and an organisation are different endpoints and spov is not
	// told which the account is, so it tries both. A response that
	// carries no id counts as a miss like an error does: a 2xx with an
	// unexpected shape must not short-circuit the other endpoint.
	account := url.PathEscape(g.account)
	var firstErr error
	for _, path := range []string{
		"/users/" + account + "/installation",
		"/orgs/" + account + "/installation",
	} {
		var body struct {
			ID int64 `json:"id"`
		}
		err := g.do(ctx, http.MethodGet, path, "Bearer "+jwt, nil, &body)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if body.ID == 0 {
			continue
		}

		g.installationID = body.ID
		return body.ID, nil
	}

	if firstErr != nil {
		return 0, fmt.Errorf("git: no GitHub App installation found for %q on %s; "+
			"install the App on that account, or set the installation id (%w)", g.account, g.host, firstErr)
	}
	return 0, fmt.Errorf("git: %s reported no App installation for %q; "+
		"install the App on that account, or set the installation id", g.host, g.account)
}

func (g *GitHub) jwt() (string, error) {
	token, err := appJWT(g.key, g.appID, g.now())
	if err != nil {
		return "", fmt.Errorf("git: github platform %q: %w", g.name, err)
	}
	return token, nil
}

// do performs one API call. authorization carries a secret, so failures
// report the status and GitHub's message but never the request headers.
//
// @decision(DL-041)
func (g *GitHub) do(ctx context.Context, method, path, authorization string, request, out any) error {
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
	req.Header.Set("Authorization", authorization)
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

// maxErrorBody bounds how much of an error response is read, so a server
// returning an HTML page cannot put a megabyte into an error string.
const maxErrorBody = 8 << 10

// githubError turns a non-2xx response into an error carrying GitHub's
// own message, which is usually the actionable part.
func githubError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))

	var parsed struct {
		Message string `json:"message"`
	}
	text := resp.Status
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
		text = resp.Status + ": " + parsed.Message
	}
	return &githubStatusError{code: resp.StatusCode, text: text}
}

// githubStatusError is a response GitHub refused: its status and its own
// message.
type githubStatusError struct {
	code int
	text string
}

func (e *githubStatusError) Error() string { return e.text }

// githubRepoJSON is the subset of GitHub's repository object spov uses.
type githubRepoJSON struct {
	Name     string `json:"name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	CloneURL string `json:"clone_url"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
	UpdatedAt time.Time `json:"updated_at"`
	PushedAt  time.Time `json:"pushed_at"`
}

func (r githubRepoJSON) toRepo() Repo {
	// The picker orders by recency, and pushed_at reflects actual work
	// where updated_at also moves for metadata changes like a star.
	updated := r.PushedAt
	if updated.IsZero() || r.UpdatedAt.After(updated) {
		updated = r.UpdatedAt
	}
	return Repo{
		Owner:     r.Owner.Login,
		Name:      r.Name,
		CloneURL:  r.CloneURL,
		Private:   r.Private,
		Archived:  r.Archived,
		UpdatedAt: updated,
	}
}

var _ Platform = (*GitHub)(nil)
