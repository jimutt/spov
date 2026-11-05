package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
)

// repoRef is a repository named on the command line.
type repoRef struct {
	Platform string
	Owner    string
	Name     string
}

// RepoChoice is a repository the picker offers: which platform it is on
// and what the server said about it. The platform is a name rather than
// a client, since the CLI holds no platform credential.
//
// @decision(DL-028)
type RepoChoice struct {
	Platform string
	Repo     git.Repo
	// MaybeReadOnly is the listing's mark: a public repository the
	// launch's fine-grained token can read and may not push to.
	//
	// @decision(DL-042)
	MaybeReadOnly bool
}

// parseRepoFlag accepts "owner/name" or "platform:owner/name". The owner
// may be a GitLab group path of several segments; the path splits at its
// last slash, and refsFromFlags checks the owner against the platform's
// kind once the platform is known.
//
// @decision(DL-023) @decision(DL-041)
func parseRepoFlag(s string) (repoRef, error) {
	var ref repoRef
	rest := strings.TrimSpace(s)
	if rest == "" {
		return ref, fmt.Errorf("empty --repo")
	}
	if platform, tail, ok := strings.Cut(rest, ":"); ok {
		if platform == "" {
			return ref, fmt.Errorf("--repo %q has an empty platform before the colon", s)
		}
		ref.Platform, rest = platform, tail
	}
	owner, name, err := git.SplitPath(rest)
	if err != nil {
		return ref, fmt.Errorf("--repo %q must be owner/name, optionally platform:owner/name", s)
	}
	// Report a malformed flag in the flag's own terms; leaving it to the
	// workspace-directory check produces a confusing error about path
	// separators much later.
	for _, seg := range strings.Split(owner, "/") {
		if err := validRepoComponent(seg); err != nil {
			return ref, fmt.Errorf("--repo %q: owner %w", s, err)
		}
	}
	if err := validRepoComponent(name); err != nil {
		return ref, fmt.Errorf("--repo %q: name %w", s, err)
	}
	ref.Owner, ref.Name = owner, name
	return ref, nil
}

// validRepoComponent checks one half of an owner/name pair. It is
// deliberately strict: the value becomes both a URL path segment and a
// directory in the sandbox workspace.
func validRepoComponent(s string) error {
	if s == "" {
		return fmt.Errorf("is empty")
	}
	if strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("%q contains a path separator", s)
	}
	if s == "." || s == ".." {
		return fmt.Errorf("%q is not a repository name", s)
	}
	if strings.HasPrefix(s, "-") {
		return fmt.Errorf("%q starts with '-'", s)
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%q contains whitespace or a control character", s)
		}
	}
	return nil
}

// resolveRepoRefs decides which repositories to bootstrap: repos named
// on the command line first, then the project config, and finally an
// interactive pick from what the server can see. The result names
// repositories; the server resolves them to platforms and credentials.
//
// @decision(DL-023) @decision(DL-041) @decision(DL-028)
func (e *Env) resolveRepoRefs(ctx context.Context, c *client.Client, cfg config.Config, flags []string) ([]server.RepoRef, error) {
	switch {
	case len(flags) > 0:
		return refsFromFlags(cfg, flags)
	case len(cfg.Repos) > 0:
		return refsFromConfig(cfg)
	default:
		return e.refsInteractively(ctx, c, cfg)
	}
}

// refsFromFlags resolves --repo values, defaulting the platform to the
// only configured one.
//
// @decision(DL-023)
func refsFromFlags(cfg config.Config, flags []string) ([]server.RepoRef, error) {
	out := make([]server.RepoRef, 0, len(flags))
	for _, raw := range flags {
		ref, err := parseRepoFlag(raw)
		if err != nil {
			return nil, err
		}
		platform, err := resolvePlatformName(cfg, ref.Platform)
		if err != nil {
			return nil, err
		}
		// A platform this config does not name is the server's to check.
		//
		// @decision(DL-041)
		if p, err := findPlatform(cfg, platform); err == nil {
			if err := git.CheckRepoPath(string(p.Kind), ref.Owner, ref.Name); err != nil {
				return nil, fmt.Errorf("--repo %s: %w", raw, err)
			}
		}
		out = append(out, server.RepoRef{Platform: platform, Owner: ref.Owner, Name: ref.Name})
	}
	return out, nil
}

// refsFromConfig names the configured repositories.
//
// @decision(DL-041)
func refsFromConfig(cfg config.Config) ([]server.RepoRef, error) {
	out := make([]server.RepoRef, 0, len(cfg.Repos))
	for _, r := range cfg.Repos {
		if _, err := findPlatform(cfg, r.Platform); err != nil {
			return nil, fmt.Errorf("repo %s: %w", r.Slug(), err)
		}
		out = append(out, server.RepoRef{Platform: r.Platform, Owner: r.Owner, Name: r.Name})
	}
	return out, nil
}

// refsInteractively lists what the server can bootstrap and asks the
// user to choose. This is what makes `spov fly` work in a directory with
// no project file.
//
// @decision(DL-023) @decision(DL-028)
func (e *Env) refsInteractively(ctx context.Context, c *client.Client, cfg config.Config) ([]server.RepoRef, error) {
	if e.SelectRepos == nil {
		return nil, fmt.Errorf("no repositories configured; pass --repo owner/name or add a `repos:` list to %s",
			config.ProjectFileName)
	}

	// @decision(DL-042)
	var shared []string
	for _, p := range cfg.Platforms {
		if p.Prefer == config.PreferShared {
			shared = append(shared, p.Name)
		}
	}
	repos, warnings, platforms, err := c.ListReposFor(ctx, shared)
	if err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	// A platform that could not be listed is reported but does not stop
	// the launch: the repositories the user wants may all be on the
	// ones that answered.
	//
	// @decision(DL-028)
	for _, w := range warnings {
		e.deferWarning("spov: warning: " + w)
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("no repositories to choose from on %s; check the platform tokens grant access to the repositories you want",
			strings.Join(platformNames(cfg), ", "))
	}
	// @decision(DL-042)
	repos, hidden := reachableFromHere(repos, platforms)
	for _, h := range hidden {
		e.deferWarning("spov: " + h)
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("no repository is reachable with the credential a launch from here uses")
	}
	choices := make([]RepoChoice, 0, len(repos))
	for _, r := range repos {
		choices = append(choices, RepoChoice{Platform: r.Platform, Repo: git.Repo{
			Owner: r.Owner, Name: r.Name, Private: r.Private, Archived: r.Archived, UpdatedAt: r.UpdatedAt,
		}, MaybeReadOnly: r.MaybeReadOnly})
	}

	chosen, err := e.SelectRepos(choices)
	if err != nil {
		return nil, err
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("no repositories selected")
	}
	out := make([]server.RepoRef, 0, len(chosen))
	for _, ch := range chosen {
		out = append(out, server.RepoRef{Platform: ch.Platform, Owner: ch.Repo.Owner, Name: ch.Repo.Name})
	}
	return out, nil
}

// reachableFromHere keeps the repositories a launch from this CLI can
// reach: those the tier its platform resolves to (under the preference
// it sent) lists. It says, per platform, how many it left out and why. A
// listing with no tiers (a local server) keeps everything.
//
// @decision(DL-042)
func reachableFromHere(repos []server.RepoView, platforms []server.PlatformTier) ([]server.RepoView, []string) {
	tier := map[string]server.PlatformTier{}
	for _, p := range platforms {
		tier[p.Platform] = p
	}
	var kept []server.RepoView
	left := map[string]int{}
	for _, r := range repos {
		if len(r.Via) == 0 || slices.Contains(r.Via, tier[r.Platform].Tier) {
			// The mark is the personal token's; a launch through the
			// organisation's credential reaches it with write access.
			//
			// @decision(DL-042)
			if tier[r.Platform].Tier != server.TierUser {
				r.MaybeReadOnly = false
			}
			kept = append(kept, r)
			continue
		}
		left[r.Platform]++
	}
	var notes []string
	for _, name := range slices.Sorted(maps.Keys(left)) {
		switch t := tier[name]; t.Tier {
		case server.TierUser:
			notes = append(notes, fmt.Sprintf("%d repositories on %s are reachable only with the organisation's credential;"+
				" set prefer: shared for %s in your config to launch on them", left[name], name, name))
		case server.TierOrg:
			notes = append(notes, fmt.Sprintf("%d repositories on %s are reachable only with your own credential;"+
				" remove prefer: shared for %s to launch on them", left[name], name, name))
		default:
			notes = append(notes, fmt.Sprintf("%d repositories on %s are left out: a launch from here has no credential for it (%s)",
				left[name], name, t.Reason))
		}
	}
	return kept, notes
}

// resolvePlatformName picks the platform a --repo value refers to,
// defaulting to the only configured one.
func resolvePlatformName(cfg config.Config, name string) (string, error) {
	if name != "" {
		return name, nil
	}
	switch len(cfg.Platforms) {
	case 1:
		return cfg.Platforms[0].Name, nil
	case 0:
		return "", fmt.Errorf("no git platforms configured; qualify the repo as <platform>:owner/name or add a `platforms:` entry to %s",
			config.ProjectFileName)
	default:
		return "", fmt.Errorf("several platforms are configured (%s); qualify the repo as <platform>:owner/name",
			strings.Join(platformNames(cfg), ", "))
	}
}

func findPlatform(cfg config.Config, name string) (config.Platform, error) {
	for _, p := range cfg.Platforms {
		if p.Name == name {
			return p, nil
		}
	}
	return config.Platform{}, fmt.Errorf("unknown platform %q; configured platforms are %s",
		name, strings.Join(platformNames(cfg), ", "))
}

func platformNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.Platforms))
	for _, p := range cfg.Platforms {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

// The rest of this file is the server side: `spov serve` resolves
// repository references to platform clients with credentials.

// selectionsFor turns repository references into per-platform
// selections with live platform clients, ready for git.Bootstrap. It
// preserves the order platforms first appear so the result is stable.
//
// @decision(DL-041) @decision(DL-028) @decision(DL-027)
func (e *Env) selectionsFor(ctx context.Context, cfg config.Config, refs []server.RepoRef) ([]git.Selection, error) {
	if len(refs) == 1 && refs[0].Platform == "" {
		return nil, nil
	}
	if len(cfg.Platforms) == 0 {
		return nil, fmt.Errorf("no git platforms configured; add a `platforms:` entry to %s or %s",
			config.ProjectFileName, e.userConfigPathForMessage())
	}
	var (
		order  []string
		byName = map[string]*git.Selection{}
	)
	for _, ref := range refs {
		p, err := findPlatform(cfg, ref.Platform)
		if err != nil {
			return nil, err
		}
		// @decision(DL-041)
		if err := git.CheckRepoPath(string(p.Kind), ref.Owner, ref.Name); err != nil {
			return nil, fmt.Errorf("repository %s/%s: %w", ref.Owner, ref.Name, err)
		}
		sel, seen := byName[p.Name]
		if !seen {
			platform, err := e.platform(ctx, p)
			if err != nil {
				return nil, err
			}
			order = append(order, p.Name)
			sel = &git.Selection{Platform: platform}
			byName[p.Name] = sel
		}
		// Clone URLs are not in the request, so derive them from the
		// platform.
		sel.Repos = append(sel.Repos, git.Repo{Owner: ref.Owner, Name: ref.Name, CloneURL: cloneURL(p, ref.Owner, ref.Name)})
	}
	out := make([]git.Selection, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, nil
}

// listAllRepos lists every configured platform concurrently. It returns
// the repositories that could be listed and a message for each platform
// that could not, and fails only when no platform could be listed at
// all — with every error reported, since a single one would be a guess
// at which mattered.
//
// @decision(DL-028)
func (e *Env) listAllRepos(ctx context.Context, cfg config.Config) ([]git.PlatformRepo, []string, error) {
	if len(cfg.Platforms) == 0 {
		return nil, nil, fmt.Errorf("no git platforms configured; add a `platforms:` entry to %s or %s",
			config.ProjectFileName, e.userConfigPathForMessage())
	}
	type result struct {
		repos []git.PlatformRepo
		fail  string
		err   error
	}

	results := make([]result, len(cfg.Platforms))
	var wg sync.WaitGroup
	for i, p := range cfg.Platforms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repos, err := e.listRepos(ctx, p)
			if err != nil {
				results[i] = result{fail: listFailure(p, err), err: err}
				return
			}
			out := make([]git.PlatformRepo, 0, len(repos.Repos))
			for _, r := range repos.Repos {
				out = append(out, git.PlatformRepo{Platform: repos.Platform, Repo: r})
			}
			results[i] = result{repos: out}
		}()
	}
	wg.Wait()

	var (
		available []git.PlatformRepo
		failures  []string
		errs      []error
	)
	for _, r := range results {
		available = append(available, r.repos...)
		if r.fail != "" {
			failures = append(failures, r.fail)
			errs = append(errs, r.err)
		}
	}
	// When nothing could be listed there is no picker to fall back to,
	// so this is the error the user sees: it carries the explanation
	// built for each platform, and still wraps the underlying errors so
	// callers can test for ErrNoRepos.
	if len(errs) == len(cfg.Platforms) {
		return nil, nil, fmt.Errorf("%s (%w)", strings.Join(failures, "; "), errors.Join(errs...))
	}
	return available, failures, nil
}

// listRepos lists one platform's repositories.
//
// @decision(DL-028)
func (e *Env) listRepos(ctx context.Context, p config.Platform) (git.Selection, error) {
	platform, err := e.platform(ctx, p)
	if err != nil {
		return git.Selection{}, err
	}
	repos, err := platform.ListRepos(ctx)
	if err != nil {
		return git.Selection{}, err
	}
	return git.Selection{Platform: platform, Repos: repos}, nil
}

// listFailure explains why one platform could not be listed, in terms
// the user can act on.
//
// @decision(DL-028)
func listFailure(p config.Platform, err error) string {
	if errors.Is(err, git.ErrNoRepos) {
		return fmt.Sprintf("the %s account %q has no repositories spov can see; "+
			"check the stored token grants access to the repositories you want", p.Kind, p.Account)
	}
	return fmt.Sprintf("could not list %s repositories: %v", p.Name, err)
}

// platform builds one git platform from config plus its stored token.
//
// @decision(DL-041) @decision(DL-008)
func (e *Env) platform(ctx context.Context, p config.Platform) (git.Platform, error) {
	token, err := e.platformToken(ctx, p)
	if err != nil {
		return nil, err
	}
	if e.NewPlatform != nil {
		return e.NewPlatform(ctx, p, token)
	}
	return newPlatform(ctx, p, token)
}

// platformToken resolves one platform's credential. In hosted mode a
// resolver is installed that answers per user, tier by tier; without
// one the credential is this machine's stored token.
//
// @decision(DL-042) @decision(DL-008)
func (e *Env) platformToken(ctx context.Context, p config.Platform) (string, error) {
	if e.PlatformToken != nil {
		return e.PlatformToken(ctx, p)
	}
	if e.Credentials == nil {
		return "", fmt.Errorf("no credential store configured")
	}
	key, err := credentials.PlatformKey(p.Name)
	if err != nil {
		return "", err
	}
	token, err := e.Credentials.Get(key)
	if err != nil {
		return "", fmt.Errorf("no stored token for platform %q: %w", p.Name, err)
	}
	return token, nil
}

// newPlatform builds the real git platform for a configured entry.
//
// @decision(DL-041)
func newPlatform(ctx context.Context, p config.Platform, token string) (git.Platform, error) {
	switch p.Kind {
	case config.KindForgejo, config.KindGitea:
		return git.NewForgejo(ctx, git.ForgejoConfig{
			Name:    p.Name,
			BaseURL: p.BaseURL,
			Account: p.Account,
			Token:   token,
			Kind:    string(p.Kind),
		})
	// @decision(DL-041)
	case config.KindGitLab:
		return git.NewGitLab(git.GitLabConfig{
			Name:    p.Name,
			BaseURL: p.BaseURL,
			Token:   token,
		})
	case config.KindGitHub:
		// A user's own credential is a personal access token, not the
		// App's JSON: one platform entry serves both tiers.
		//
		// @decision(DL-042)
		if git.LooksLikeGitHubPAT(token) {
			return git.NewGitHubPAT(git.GitHubPATConfig{
				Name:    p.Name,
				BaseURL: p.BaseURL,
				Token:   token,
			})
		}
		cred, err := git.ParseGitHubCredential(token)
		if err != nil {
			return nil, fmt.Errorf("platform %q: %w", p.Name, err)
		}
		return git.NewGitHub(git.GitHubConfig{
			Name:           p.Name,
			BaseURL:        p.BaseURL,
			Account:        p.Account,
			AppID:          cred.AppID,
			PrivateKeyPEM:  cred.PrivateKey,
			InstallationID: cred.InstallationID,
		})
	default:
		return nil, fmt.Errorf("platform %q has unknown kind %q", p.Name, p.Kind)
	}
}

// cloneURL derives the HTTPS clone URL for a repo on a platform. A
// GitLab owner may be a group path, which is the URL's path as it is.
//
// @decision(DL-041)
func cloneURL(p config.Platform, owner, name string) string {
	return fmt.Sprintf("%s/%s/%s.git", platformBase(p), owner, name)
}

// platformBase is a platform's web root: its base_url, else the public
// instance of a kind that has one.
//
// @decision(DL-041)
func platformBase(p config.Platform) string {
	if base := trimSlash(p.BaseURL); base != "" {
		return base
	}
	switch p.Kind {
	case config.KindGitHub:
		return "https://github.com"
	case config.KindGitLab:
		return "https://gitlab.com"
	}
	return ""
}

// platformHost is the host a platform's credential is bound to.
//
// @decision(DL-041)
func platformHost(p config.Platform) (string, error) {
	return git.HostFromURL(platformBase(p))
}

// userConfigPathForMessage names the user-level config file for error
// messages, falling back to a readable placeholder.
func (e *Env) userConfigPathForMessage() string {
	if path, err := config.UserConfigPath(e.Environ); err == nil {
		return path
	}
	return "~/.config/spov/config.yaml"
}

// humanAge renders how long ago something happened, coarsely.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return "just now"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/24/365))
	}
}
