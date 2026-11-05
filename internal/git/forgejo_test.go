package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	forgejo "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
)

// fakeForgejoAPI stands in for a Forgejo server.
type fakeForgejoAPI struct {
	repos   []*forgejo.Repository
	listErr error
	// pulls is what the server holds, paged the same way repos are.
	pulls    []*forgejo.PullRequest
	pullErr  error
	pullOpts []forgejo.ListPullRequestsOptions
	// user and version answer Inspect; userStatus fails it with that
	// HTTP status.
	user       string
	version    string
	userStatus int
	// hidden are the repositories (owner/name) the token cannot see.
	hidden []string
}

func (f *fakeForgejoAPI) GetRepoContext(_ context.Context, owner, repo string) (*forgejo.Repository, *forgejo.Response, error) {
	if slices.Contains(f.hidden, owner+"/"+repo) {
		return nil, &forgejo.Response{Response: &http.Response{StatusCode: http.StatusNotFound}}, errors.New("not found")
	}
	return &forgejo.Repository{Name: repo}, nil, nil
}

func (f *fakeForgejoAPI) GetMyUserInfoContext(ctx context.Context) (*forgejo.User, *forgejo.Response, error) {
	if f.userStatus != 0 {
		return nil, &forgejo.Response{Response: &http.Response{StatusCode: f.userStatus}}, errors.New(http.StatusText(f.userStatus))
	}
	return &forgejo.User{UserName: f.user}, nil, nil
}

func (f *fakeForgejoAPI) ServerVersionContext(ctx context.Context) (string, *forgejo.Response, error) {
	if f.version == "" {
		return "", nil, errors.New("hidden")
	}
	return f.version, nil, nil
}

func (f *fakeForgejoAPI) ListMyReposContext(ctx context.Context, opt forgejo.ListReposOptions) ([]*forgejo.Repository, *forgejo.Response, error) {
	// A real request honours the context; the fake must too, or the
	// cancellation tests prove nothing.
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if f.listErr != nil {
		return nil, nil, f.listErr
	}
	start := (opt.Page - 1) * opt.PageSize
	if start >= len(f.repos) {
		return nil, nil, nil
	}
	end := min(start+opt.PageSize, len(f.repos))
	return f.repos[start:end], nil, nil
}

func (f *fakeForgejoAPI) ListRepoPullRequestsContext(ctx context.Context, owner, repo string, opt forgejo.ListPullRequestsOptions) ([]*forgejo.PullRequest, *forgejo.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f.pullOpts = append(f.pullOpts, opt)
	if f.pullErr != nil {
		return nil, nil, f.pullErr
	}
	start := (opt.Page - 1) * opt.PageSize
	if start >= len(f.pulls) {
		return nil, nil, nil
	}
	end := min(start+opt.PageSize, len(f.pulls))
	return f.pulls[start:end], nil, nil
}

func newTestForgejo(api forgejoAPI) *Forgejo {
	return &Forgejo{
		name:    "codefloe",
		kind:    KindForgejo,
		host:    "codefloe.com",
		baseURL: "https://codefloe.com",
		account: "alice",
		token:   storedToken,
		client:  api,
	}
}

// storedToken stands in for the repository-scoped token the user creates
// in the Forgejo UI and spov stores.
const storedToken = "gto-stored-scoped-token"

// oneRepo is a minimal selection.
var oneRepo = []Repo{{Owner: "alice", Name: "spov"}}

func repo(owner, name string, updated time.Time) *forgejo.Repository {
	return &forgejo.Repository{
		Owner:    &forgejo.User{UserName: owner},
		Name:     name,
		FullName: owner + "/" + name,
		CloneURL: "https://codefloe.com/" + owner + "/" + name + ".git",
		Updated:  updated,
	}
}

func TestForgejoListRepos(t *testing.T) {
	now := time.Now()
	api := &fakeForgejoAPI{repos: []*forgejo.Repository{
		repo("alice", "old", now.Add(-48*time.Hour)),
		repo("alice", "new", now),
		repo("acme", "mid", now.Add(-time.Hour)),
	}}

	got, err := newTestForgejo(api).ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d repos", len(got))
	}
	// Newest activity first, so the interactive picker shows recent work
	// at the top.
	want := []string{"alice/new", "acme/mid", "alice/old"}
	for i, w := range want {
		if got[i].Slug() != w {
			t.Errorf("repo %d = %q, want %q", i, got[i].Slug(), w)
		}
	}
	if got[0].CloneURL != "https://codefloe.com/alice/new.git" {
		t.Errorf("clone URL = %q", got[0].CloneURL)
	}
}

func TestForgejoListReposPaginates(t *testing.T) {
	var repos []*forgejo.Repository
	for i := range forgejoPageSize + 7 {
		repos = append(repos, repo("alice", fmt.Sprintf("r%d", i), time.Now()))
	}
	api := &fakeForgejoAPI{repos: repos}

	got, err := newTestForgejo(api).ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(repos) {
		t.Errorf("got %d repos, want %d; the second page was not fetched", len(got), len(repos))
	}
}

func TestForgejoListReposErrors(t *testing.T) {
	t.Run("api failure", func(t *testing.T) {
		api := &fakeForgejoAPI{listErr: errors.New("503 service unavailable")}
		_, err := newTestForgejo(api).ListRepos(context.Background())
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "codefloe.com") {
			t.Errorf("error %q does not name the host", err)
		}
	})

	t.Run("no repositories", func(t *testing.T) {
		_, err := newTestForgejo(&fakeForgejoAPI{}).ListRepos(context.Background())
		if !errors.Is(err, ErrNoRepos) {
			t.Errorf("err = %v, want ErrNoRepos", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newTestForgejo(&fakeForgejoAPI{}).ListRepos(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("repos with no owner are skipped", func(t *testing.T) {
		api := &fakeForgejoAPI{repos: []*forgejo.Repository{
			{Name: "orphan"},
			repo("alice", "real", time.Now()),
			nil,
		}}
		got, err := newTestForgejo(api).ListRepos(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Slug() != "alice/real" {
			t.Errorf("repos = %+v", got)
		}
	})
}

// TestForgejoIssueSandboxTokenReturnsTheStoredToken: spov cannot mint a
// token from another token, so it hands over the one the user created.
//
// @decision(DL-041)
func TestForgejoIssueSandboxTokenReturnsTheStoredToken(t *testing.T) {
	f := newTestForgejo(&fakeForgejoAPI{})

	tok, err := f.IssueSandboxToken(context.Background(), oneRepo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Username != "alice" || tok.Secret != storedToken {
		t.Errorf("token username = %q, secret matches stored = %v", tok.Username, tok.Secret == storedToken)
	}
	// The token belongs to the user, so spov must not claim it can revoke
	// or expire it.
	if tok.Revoke != nil {
		t.Error("spov did not create this token and cannot revoke it; Revoke must be nil")
	}
	if !tok.ExpiresAt.IsZero() {
		t.Error("spov does not know when a user-created token expires; ExpiresAt must be zero")
	}
}

func TestForgejoIssueSandboxTokenRejects(t *testing.T) {
	f := newTestForgejo(&fakeForgejoAPI{})

	t.Run("no repositories selected", func(t *testing.T) {
		if _, err := f.IssueSandboxToken(context.Background(), nil, 0); err == nil {
			t.Error("an empty selection was accepted")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.IssueSandboxToken(ctx, oneRepo, 0); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
}

// TestForgejoTokenScopeIsHonestAboutUncertainty: spov cannot read a
// token's own scoping back through the API, so it must not claim the
// sandbox is limited to the selected repositories.
//
// @decision(DL-041)
func TestForgejoTokenScopeIsHonestAboutUncertainty(t *testing.T) {
	scope := newTestForgejo(&fakeForgejoAPI{}).TokenScope()
	if scope.PerRepo {
		t.Error("spov cannot verify a stored token's scoping; PerRepo must not claim it did")
	}
	for _, want := range []string{"Specific repositories", "write:repository", "write:issue", "alice", "codefloe.com"} {
		if !strings.Contains(scope.Description, want) {
			t.Errorf("description %q does not mention %q", scope.Description, want)
		}
	}
}

func TestNewForgejoValidates(t *testing.T) {
	base := ForgejoConfig{Name: "codefloe", BaseURL: "https://codefloe.com", Account: "alice", Token: storedToken}
	tests := []struct {
		name   string
		mutate func(*ForgejoConfig)
		want   string
	}{
		{"no name", func(c *ForgejoConfig) { c.Name = "" }, "name"},
		{"no base url", func(c *ForgejoConfig) { c.BaseURL = "" }, "base URL"},
		{"no account", func(c *ForgejoConfig) { c.Account = "" }, "account"},
		{"no token", func(c *ForgejoConfig) { c.Token = "" }, "API token"},
		{"bad base url scheme", func(c *ForgejoConfig) { c.BaseURL = "ftp://x" }, "http or https"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			_, err := NewForgejo(context.Background(), cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			if cfg.Token != "" && strings.Contains(err.Error(), cfg.Token) {
				t.Errorf("the error leaked the API token: %q", err)
			}
		})
	}
}

func TestHostFromURL(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://codefloe.com", "codefloe.com", false},
		{"https://codefloe.com/", "codefloe.com", false},
		{"http://localhost:3000", "localhost:3000", false},
		{"ftp://x.com", "", true},
		{"not a url", "", true},
		{"https://", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := hostFromURL(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("host = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestForgejoNameAndHost(t *testing.T) {
	f := newTestForgejo(&fakeForgejoAPI{})
	if f.Name() != "codefloe" || f.Host() != "codefloe.com" {
		t.Errorf("Name/Host = %q/%q", f.Name(), f.Host())
	}
}

// endlessForgejoAPI always returns a full page, mimicking a server that
// ignores the page parameter.
type endlessForgejoAPI struct {
	calls int
	fakeForgejoAPI
}

func (e *endlessForgejoAPI) ListMyReposContext(_ context.Context, opt forgejo.ListReposOptions) ([]*forgejo.Repository, *forgejo.Response, error) {
	e.calls++
	page := make([]*forgejo.Repository, opt.PageSize)
	for i := range page {
		page[i] = repo("alice", fmt.Sprintf("r%d-%d", e.calls, i), time.Now())
	}
	return page, nil, nil
}

// The pull-request listing pages the same way and needs the same cap:
// it matches client-side, so a server ignoring the page parameter would
// spin forever on a branch that has no pull request.
//
// @decision(DL-050)
func (e *endlessForgejoAPI) ListRepoPullRequestsContext(_ context.Context, _, _ string, opt forgejo.ListPullRequestsOptions) ([]*forgejo.PullRequest, *forgejo.Response, error) {
	e.calls++
	page := make([]*forgejo.PullRequest, opt.PageSize)
	for i := range page {
		page[i] = &forgejo.PullRequest{
			Index: int64(i), Head: &forgejo.PRBranchInfo{Ref: "not-the-one"},
		}
	}
	return page, nil, nil
}

// TestForgejoListReposIsBounded: without a cap a server that keeps
// returning full pages spins until it exhausts memory.
//
// @decision(DL-041)
func TestForgejoListReposIsBounded(t *testing.T) {
	api := &endlessForgejoAPI{}
	f := newTestForgejo(api)

	_, err := f.ListRepos(context.Background())
	if err == nil {
		t.Fatal("an endless listing was accepted")
	}
	if !strings.Contains(err.Error(), "ignoring the page parameter") {
		t.Errorf("err = %v", err)
	}
	if api.calls > forgejoMaxPages+1 {
		t.Errorf("made %d calls, want at most %d", api.calls, forgejoMaxPages+1)
	}
}

// The pull-request query, which is gate 2 of a pr job's verdict.
//
// @decision(DL-050)

func pull(number int, head, state, htmlURL string) *forgejo.PullRequest {
	return &forgejo.PullRequest{
		Index: int64(number), Title: "a title", State: forgejo.StateType(state),
		HTMLURL: htmlURL, Head: &forgejo.PRBranchInfo{Ref: head},
	}
}

func TestForgejoFindsThePullRequestForABranch(t *testing.T) {
	api := &fakeForgejoAPI{pulls: []*forgejo.PullRequest{
		pull(11, "other-work", "open", "https://codefloe.com/alice/spov/pulls/11"),
		pull(12, "impl/widgets", "open", "https://codefloe.com/alice/spov/pulls/12"),
	}}
	got, err := newTestForgejo(api).FindPullRequest(
		context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "impl/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("found no pull request for a branch that has one")
	}
	if got.Number != 12 {
		t.Errorf("matched the wrong pull request: %+v", got)
	}
	// The API's own `url` is no use to a person; the sidebar renders
	// this as a link they click.
	if got.URL != "https://codefloe.com/alice/spov/pulls/12" {
		t.Errorf("reported %q, want the browser URL", got.URL)
	}
	// Every state is asked for, so a merged one can be found (DL-051).
	if len(api.pullOpts) == 0 || api.pullOpts[0].State != forgejo.StateAll {
		t.Errorf("did not ask for pull requests in every state: %+v", api.pullOpts)
	}
	if !got.Open {
		t.Errorf("an open pull request was not reported open: %+v", got)
	}
}

// Of a branch's pull requests the open one wins, then a merged one, then
// a closed one, whatever their order.
//
// @decision(DL-051)
func TestForgejoPrefersOpenThenMergedThenClosed(t *testing.T) {
	head := &forgejo.PRBranchInfo{Ref: "spov/x"}
	closed := &forgejo.PullRequest{Index: 1, Head: head, State: forgejo.StateClosed}
	merged := &forgejo.PullRequest{Index: 2, Head: head, State: forgejo.StateClosed, HasMerged: true}
	open := &forgejo.PullRequest{Index: 3, Head: head, State: forgejo.StateOpen}
	for _, tc := range []struct {
		pulls []*forgejo.PullRequest
		want  int
	}{
		{[]*forgejo.PullRequest{closed, merged, open}, 3},
		{[]*forgejo.PullRequest{closed, merged}, 2},
		{[]*forgejo.PullRequest{closed}, 1},
		// A fork's merged pull request from a same-named branch is not
		// the repository's own.
		{[]*forgejo.PullRequest{closed, {Index: 9, State: forgejo.StateClosed, HasMerged: true,
			Head: &forgejo.PRBranchInfo{Ref: "spov/x", RepoID: 7}, Base: &forgejo.PRBranchInfo{RepoID: 1}}}, 1},
	} {
		got, err := newTestForgejo(&fakeForgejoAPI{pulls: tc.pulls}).FindPullRequest(
			context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "spov/x")
		if err != nil || got == nil || got.Number != tc.want {
			t.Errorf("found %+v, %v; want #%d", got, err, tc.want)
		}
	}
}

// TestForgejoReportsNoPullRequestRatherThanTheFirstOne is the one that
// matters most: gate 2 FAILS a job on this answer, so a query that fell
// back to "whichever pull request exists" would pass a job that opened
// none, on the strength of somebody else's.
func TestForgejoReportsNoPullRequestRatherThanTheFirstOne(t *testing.T) {
	api := &fakeForgejoAPI{pulls: []*forgejo.PullRequest{
		pull(11, "someone-elses-branch", "open", "https://codefloe.com/x/1"),
	}}
	got, err := newTestForgejo(api).FindPullRequest(
		context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "impl/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("reported a pull request from another branch as this job's: %+v", got)
	}
}

// TestForgejoWithNoBranchAsksNothing holds the other half. An empty
// branch is "spov does not know what this job delivered", and a query
// with no head filter would answer it with whatever exists.
func TestForgejoWithNoBranchAsksNothing(t *testing.T) {
	api := &fakeForgejoAPI{pulls: []*forgejo.PullRequest{
		pull(11, "anything", "open", "https://codefloe.com/x/1"),
	}}
	got, err := newTestForgejo(api).FindPullRequest(
		context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("answered a question it was not asked: %+v", got)
	}
	if len(api.pullOpts) != 0 {
		t.Errorf("made %d request(s) with no branch to match", len(api.pullOpts))
	}
}

// TestForgejoFindPullRequestIsBounded: the query matches client-side, so
// a server that ignores the page parameter would spin forever on a
// branch with no pull request.
func TestForgejoFindPullRequestIsBounded(t *testing.T) {
	api := &endlessForgejoAPI{}
	_, err := newTestForgejo(api).FindPullRequest(
		context.Background(), RepoRef{Owner: "alice", Name: "spov"}, "never-matches")
	if err == nil {
		t.Fatal("a server that ignores paging did not stop the query")
	}
	if api.calls > forgejoMaxPages+1 {
		t.Errorf("made %d requests against a cap of %d", api.calls, forgejoMaxPages)
	}
}

func TestForgejoFindPullRequestHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newTestForgejo(&fakeForgejoAPI{}).FindPullRequest(
		ctx, RepoRef{Owner: "alice", Name: "spov"}, "impl/widgets")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A stored token for every repository is the stored token, and the
// forge's base path is what its base URL puts below the host.
//
// @decision(DL-043)
func TestForgejoWideTokenAndBasePath(t *testing.T) {
	f := newTestForgejo(nil)
	tok, err := f.IssueWideToken(context.Background(), time.Hour)
	if err != nil || tok.Secret != f.token || tok.Username != f.account {
		t.Errorf("wide token %+v, %v", tok, err)
	}
	for base, want := range map[string]string{"https://codefloe.com": "", "https://example.com/forgejo/": "/forgejo"} {
		f.baseURL = strings.TrimRight(base, "/")
		if got := f.BasePath(); got != want {
			t.Errorf("BasePath of %s = %q, want %q", base, got, want)
		}
	}
}

// The check reads the token's own user, not the configured account, and
// tells a refused token from a missing permission.
//
// @decision(DL-041)
func TestForgejoInspectReadsTheTokensOwnUser(t *testing.T) {
	ctx := context.Background()
	info, err := newTestForgejo(&fakeForgejoAPI{user: "bot", version: "11.0.1+gitea-1.22.0"}).Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.User != "bot" || info.ServerVersion != "11.0.1+gitea-1.22.0" {
		t.Errorf("info = %+v", info)
	}
	for code, want := range map[int]error{401: ErrBadToken, 404: ErrUnsupportedServer} {
		_, err := newTestForgejo(&fakeForgejoAPI{userStatus: code}).Inspect(ctx)
		if !errors.Is(err, want) || !strings.Contains(err.Error(), http.StatusText(code)) {
			t.Errorf("status %d: err = %v, want %v with the forge's message", code, err, want)
		}
	}
	// A token limited to specific repositories may not read its user;
	// that leaves the user unknown and the listing to decide.
	info, err = newTestForgejo(&fakeForgejoAPI{userStatus: 403}).Inspect(ctx)
	if err != nil || info.User != "" {
		t.Errorf("403: %+v, %v", info, err)
	}
}

// Gitea is its own kind on the shared adapter, and nothing else is.
//
// @decision(DL-041)
func TestTheForgejoAdapterServesForgejoAndGitea(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.22.0"}`))
	}))
	defer srv.Close()
	for _, kind := range []string{"", KindForgejo, KindGitea, KindGitHub} {
		f, err := NewForgejo(ctx, ForgejoConfig{Name: "x", BaseURL: srv.URL, Account: "a", Token: "t", Kind: kind})
		switch kind {
		case KindGitHub:
			if err == nil {
				t.Errorf("kind %q was accepted", kind)
			}
		case "":
			if err != nil || f.Kind() != KindForgejo {
				t.Errorf("empty kind: %v, %v", f, err)
			}
		default:
			if err != nil || f.Kind() != kind {
				t.Errorf("kind %q: %v, %v", kind, f, err)
			}
		}
	}
}
