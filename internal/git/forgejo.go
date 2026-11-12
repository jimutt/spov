package git

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	forgejo "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
)

// forgejoPageSize is the page size for repository listing.
const forgejoPageSize = 50

// forgejoMaxPages bounds the listing loop. A server that ignores the page
// parameter, or keeps returning full pages, would otherwise spin until it
// exhausted memory.
const forgejoMaxPages = 200

// forgejoAPI is the subset of the SDK used here, so the platform can be
// tested without a server.
type forgejoAPI interface {
	ListMyReposContext(ctx context.Context, opt forgejo.ListReposOptions) ([]*forgejo.Repository, *forgejo.Response, error)
	// @decision(DL-050)
	ListRepoPullRequestsContext(ctx context.Context, owner, repo string, opt forgejo.ListPullRequestsOptions) ([]*forgejo.PullRequest, *forgejo.Response, error)
	// @decision(DL-041)
	GetMyUserInfoContext(ctx context.Context) (*forgejo.User, *forgejo.Response, error)
	ServerVersionContext(ctx context.Context) (string, *forgejo.Response, error)
	// @decision(DL-043)
	GetRepoContext(ctx context.Context, owner, repo string) (*forgejo.Repository, *forgejo.Response, error)
}

// Forgejo talks to a Forgejo or Gitea instance. The two share this
// adapter and differ only in the kind they report.
//
// @decision(DL-041)
type Forgejo struct {
	name    string
	kind    string
	host    string
	baseURL string
	account string
	// token is the user-created, repository-scoped token handed to
	// sandboxes. It must never be logged.
	token  string
	client forgejoAPI
}

// ForgejoConfig configures a Forgejo platform.
type ForgejoConfig struct {
	// Name is the platform name from config, used as the credential key.
	Name string
	// BaseURL is the instance root, e.g. https://codefloe.com.
	BaseURL string
	// Account is the username the token belongs to.
	Account string
	// Token is a repository-scoped access token the user created. spov
	// stores it and hands it to sandboxes; it never mints its own.
	Token string
	// Kind is KindForgejo (the default) or KindGitea.
	//
	// @decision(DL-041)
	Kind string
}

// NewForgejo connects to a Forgejo instance.
//
// @decision(DL-041)
func NewForgejo(ctx context.Context, cfg ForgejoConfig) (*Forgejo, error) {
	kind := cfg.Kind
	switch kind {
	case "":
		kind = KindForgejo
	case KindForgejo, KindGitea:
	default:
		return nil, fmt.Errorf("git: %q is not a Forgejo or Gitea kind", kind)
	}
	if cfg.Name == "" {
		return nil, fmt.Errorf("git: %s platform needs a name", kind)
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("git: forgejo platform %q needs a base URL", cfg.Name)
	}
	if cfg.Account == "" {
		return nil, fmt.Errorf("git: forgejo platform %q needs an account", cfg.Name)
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("git: forgejo platform %q has no API token", cfg.Name)
	}
	host, err := hostFromURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	// The SDK's SetContext stores a client-wide default, so per-call
	// contexts are applied on a shallow copy instead (see sdkClient).
	sdk, err := forgejo.NewClient(cfg.BaseURL,
		forgejo.SetToken(cfg.Token),
	)
	if err != nil {
		return nil, fmt.Errorf("git: connect to %s: %w", cfg.BaseURL, err)
	}
	return &Forgejo{
		name:    cfg.Name,
		kind:    kind,
		host:    host,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		account: cfg.Account,
		token:   cfg.Token,
		client:  &sdkClient{client: sdk},
	}, nil
}

func (f *Forgejo) Name() string { return f.name }
func (f *Forgejo) Host() string { return f.host }
func (f *Forgejo) Kind() string { return f.kind }

// Inspect reads the token's own user and the server's version. A token
// limited to specific repositories may hold no read:user scope, so a 403
// on the user leaves the user unknown rather than refusing the token;
// the repository listing decides then.
//
// @decision(DL-041)
func (f *Forgejo) Inspect(ctx context.Context) (TokenInfo, error) {
	info := TokenInfo{}
	u, resp, err := f.client.GetMyUserInfoContext(ctx)
	switch {
	case err == nil:
		info.User = u.UserName
	case resp != nil && resp.Response != nil && resp.StatusCode == http.StatusForbidden:
	case resp != nil && resp.Response != nil && statusError(resp.StatusCode) != nil:
		return TokenInfo{}, fmt.Errorf("%w (%w)", statusError(resp.StatusCode), err)
	default:
		return TokenInfo{}, fmt.Errorf("git: read the token's user on %s: %w", f.host, err)
	}
	// The version is information, not a requirement: a server that
	// hides it is still usable.
	if v, _, err := f.client.ServerVersionContext(ctx); err == nil {
		info.ServerVersion = v
	}
	return info, nil
}

// TokenScope describes the stored token. spov cannot read a token's own
// scoping back through the API, so this reports what the user was asked to
// configure rather than a verified fact.
//
// @decision(DL-041)

// CheckRepo asks the forge whether the token can see repo: a 404 or 403
// is ErrRepoUnreachable, and any other failure settles nothing.
//
// @decision(DL-043)
func (f *Forgejo) CheckRepo(ctx context.Context, repo Repo) error {
	ctx, cancel := context.WithTimeout(ctx, repoCheckTimeout)
	defer cancel()
	_, resp, err := f.client.GetRepoContext(ctx, repo.Owner, repo.Name)
	if err != nil && resp != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden) {
		return ErrRepoUnreachable
	}
	return nil
}

// FindPullRequest looks for the best pull request in any state whose head
// is branch, in the repository itself rather than a fork.
//
// It lists and matches rather than using the SDK's
// GetPullRequestByBaseAndHead, which needs the base branch: spov does not
// know a repository's default branch, and a job may target another one.
//
// @decision(DL-051)
func (f *Forgejo) FindPullRequest(ctx context.Context, repo RepoRef, branch string) (*PullRequest, error) {
	if strings.TrimSpace(branch) == "" {
		// Never a match-anything query.
		return nil, nil
	}
	var best *PullRequest
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if page > forgejoMaxPages {
			return nil, fmt.Errorf("git: %s returned more than %d pages of pull requests; it may be ignoring the page parameter",
				f.host, forgejoMaxPages)
		}
		// Every state: a merged pull request is verified too (DL-051).
		prs, _, err := f.client.ListRepoPullRequestsContext(ctx, repo.Owner, repo.Name, forgejo.ListPullRequestsOptions{
			ListOptions: forgejo.ListOptions{Page: page, PageSize: forgejoPageSize},
			State:       forgejo.StateAll,
		})
		if err != nil {
			return nil, fmt.Errorf("git: list pull requests on %s: %w", f.host, err)
		}
		for _, pr := range prs {
			if pr == nil || pr.Head == nil || pr.Head.Ref != branch {
				continue
			}
			// A fork's pull request from a branch of the same name is
			// not the job's. @decision(DL-051)
			if pr.Base != nil && pr.Head.RepoID != 0 && pr.Head.RepoID != pr.Base.RepoID {
				continue
			}
			var created time.Time
			if pr.Created != nil {
				created = *pr.Created
			}
			// HTMLURL is where a person goes; URL is the API's own
			// address for it and is no use in a sidebar.
			best = bestPullRequest(best, PullRequest{
				Number: int(pr.Index), URL: OwnURL(pr.HTMLURL, f.baseURL),
				Title: pr.Title, State: string(pr.State),
				Open: pr.State == forgejo.StateOpen, Merged: pr.HasMerged, Created: created,
			})
			if best.Open {
				return best, nil
			}
		}
		if len(prs) < forgejoPageSize {
			return best, nil
		}
	}
}

func (f *Forgejo) TokenScope() Scope {
	return Scope{
		PerRepo: false,
		Description: fmt.Sprintf("whatever the stored token for %s on %s grants; "+
			"create it with 'Specific repositories', write:repository and write:issue (tea reads labels when it opens a pull request) "+
			"to limit it to the repositories you want",
			f.account, f.host),
	}
}

// ListRepos returns the repositories the account can access, newest
// activity first.
//
// @decision(DL-041)
func (f *Forgejo) ListRepos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if page > forgejoMaxPages {
			return nil, fmt.Errorf("git: %s returned more than %d pages of repositories; it may be ignoring the page parameter",
				f.host, forgejoMaxPages)
		}
		repos, _, err := f.client.ListMyReposContext(ctx, forgejo.ListReposOptions{
			ListOptions: forgejo.ListOptions{Page: page, PageSize: forgejoPageSize},
		})
		if err != nil {
			return nil, fmt.Errorf("git: list repositories on %s: %w", f.host, err)
		}
		for _, r := range repos {
			if r == nil || r.Owner == nil {
				continue
			}
			out = append(out, Repo{
				Owner:     r.Owner.UserName,
				Name:      r.Name,
				CloneURL:  r.CloneURL,
				Private:   r.Private,
				Archived:  r.Archived,
				UpdatedAt: r.Updated,
			})
		}
		if len(repos) < forgejoPageSize {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("git: %s: %w", f.host, ErrNoRepos)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// IssueSandboxToken returns the stored repository-scoped token. Forgejo
// cannot mint a token from another token (POST /users/{u}/tokens accepts
// only password Basic auth), so spov stores a token the user created
// instead of minting one. The token belongs to the user and outlives the
// sandbox, so Revoke is nil; rotation happens in the Forgejo UI.
//
// @decision(DL-041)
func (f *Forgejo) IssueSandboxToken(ctx context.Context, repos []Repo, _ time.Duration) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	if len(repos) == 0 {
		return Token{}, fmt.Errorf("git: %s: no repositories selected", f.host)
	}
	return Token{
		Username: f.account,
		Secret:   f.token,
	}, nil
}

// BasePath is the path below the host the forge is served under, such
// as "/forgejo", empty at the root.
//
// @decision(DL-043)
func (f *Forgejo) BasePath() string {
	u, err := url.Parse(f.baseURL)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

// IssueWideToken hands back the stored token, which reaches whatever it
// was created for.
//
// @decision(DL-043)
func (f *Forgejo) IssueWideToken(ctx context.Context, _ time.Duration) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	return Token{Username: f.account, Secret: f.token}, nil
}

// hostFromURL extracts the hostname git credentials are bound to.
func hostFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("git: parse base URL %q: %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("git: base URL %q must be http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("git: base URL %q has no host", raw)
	}
	return u.Host, nil
}

var _ Platform = (*Forgejo)(nil)

// sdkClient adapts the Forgejo SDK to forgejoAPI, applying a per-request
// context rather than the one the client was constructed with.
type sdkClient struct {
	// mu serialises requests, since SetContext sets the context on the
	// shared client.
	mu     sync.Mutex
	client *forgejo.Client
}

// ListMyReposContext scopes one request to ctx.
func (c *sdkClient) ListMyReposContext(ctx context.Context, opt forgejo.ListReposOptions) ([]*forgejo.Repository, *forgejo.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := forgejo.SetContext(ctx)(c.client); err != nil {
		return nil, nil, fmt.Errorf("set request context: %w", err)
	}
	return c.client.ListMyRepos(opt)
}

// GetRepoContext is the same trick for one repository.
//
// @decision(DL-043)
func (c *sdkClient) GetRepoContext(ctx context.Context, owner, repo string) (*forgejo.Repository, *forgejo.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := forgejo.SetContext(ctx)(c.client); err != nil {
		return nil, nil, fmt.Errorf("set request context: %w", err)
	}
	return c.client.GetRepo(owner, repo)
}

// ListRepoPullRequestsContext is the same trick for pull requests.
//
// @decision(DL-050)
func (c *sdkClient) ListRepoPullRequestsContext(ctx context.Context, owner, repo string, opt forgejo.ListPullRequestsOptions) ([]*forgejo.PullRequest, *forgejo.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := forgejo.SetContext(ctx)(c.client); err != nil {
		return nil, nil, fmt.Errorf("set request context: %w", err)
	}
	return c.client.ListRepoPullRequests(owner, repo, opt)
}

// GetMyUserInfoContext is the same trick for the token's user.
//
// @decision(DL-041)
func (c *sdkClient) GetMyUserInfoContext(ctx context.Context) (*forgejo.User, *forgejo.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := forgejo.SetContext(ctx)(c.client); err != nil {
		return nil, nil, fmt.Errorf("set request context: %w", err)
	}
	return c.client.GetMyUserInfo()
}

// ServerVersionContext is the same trick for the server's version.
//
// @decision(DL-041)
func (c *sdkClient) ServerVersionContext(ctx context.Context) (string, *forgejo.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := forgejo.SetContext(ctx)(c.client); err != nil {
		return "", nil, fmt.Errorf("set request context: %w", err)
	}
	return c.client.ServerVersion()
}
