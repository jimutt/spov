package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Selection is one platform and the repos to bootstrap from it.
type Selection struct {
	Platform Platform
	Repos    []Repo
	// All asks for a token that reaches every repository the credential
	// does, not just Repos, which may then be empty.
	//
	// @decision(DL-043)
	All bool
}

// PlatformRepo is a repository together with the platform it came from,
// so a row from a merged picker can be mapped back to its platform.
//
// @decision(DL-028)
type PlatformRepo struct {
	Platform Platform
	Repo     Repo
}

// PlatformToken is one platform's credential, for configuring the forge
// CLIs inside the sandbox.
//
// @decision(DL-032)
type PlatformToken struct {
	// Platform is the configured platform name.
	Platform string
	// Host is the platform's hostname.
	Host string
	// Username the token belongs to.
	Username string
	// Secret is the token. It must never be logged.
	Secret string
	// Renewable says the server re-mints this token into the running
	// sandbox; ExpiresAt is when the current one lapses, zero if never.
	//
	// @decision(DL-021)
	Renewable bool
	ExpiresAt time.Time
	// All says the token reaches every repository the credential does,
	// so the proxy adds it for any repository on the host.
	//
	// @decision(DL-043)
	All bool
}

// Clone is one repository to clone in the sandbox, with the credential
// embedded in its URL. The URL carries a secret and must not be logged;
// use RedactURL when reporting it.
//
// @decision(DL-041)
type Clone struct {
	// Slug is the "owner/name" identifier, safe to log.
	Slug string
	// Dir is the directory name under the workspace root.
	Dir string
	// URL is the clone URL. It carries no credential: the sandbox
	// authenticates through a git credential store.
	URL string
	// Platform is the configured platform name, safe to log.
	Platform string
	// Host is the platform's hostname, safe to log.
	Host string
}

// Bootstrap mints one token per platform and returns the clone list plus a
// revoke function releasing every token it issued. The revoke function is
// returned even when Bootstrap fails partway. A selection with All gets
// a token even when it has no repositories to clone.
//
// @decision(DL-041) @decision(DL-043)
func Bootstrap(ctx context.Context, selections []Selection, ttl time.Duration) (result Result, revoke func(context.Context) error, err error) {
	r := &revoker{}
	revoke = r.revoke

	// Keyed by directory, valued by "<platform>:<slug>" so the same repo
	// name on two platforms disambiguates rather than colliding. The
	// reserved names start out taken.
	//
	// @decision(DL-037)
	dirs := map[string]string{}
	for _, d := range ReservedDirs {
		dirs[d] = reservedHolder
	}
	if err := oneCredentialPerHost(selections); err != nil {
		return Result{}, revoke, err
	}
	for _, sel := range selections {
		if len(sel.Repos) == 0 && !sel.All {
			continue
		}
		if sel.Platform == nil {
			return Result{}, revoke, fmt.Errorf("git: selection has no platform")
		}

		// A stored token is handed over as it is, so the forge is asked
		// first whether it can see each repository.
		//
		// @decision(DL-043)
		if checker, ok := sel.Platform.(RepoChecker); ok && !Renewable(sel.Platform) {
			if hidden := unreachable(ctx, checker, sel.Repos); hidden != "" {
				return Result{}, revoke, fmt.Errorf("git: the %s credential this launch uses cannot see %s, so it cannot be cloned;"+
					" give that credential access to it", sel.Platform.Name(), hidden)
			}
		}

		tok, err := IssueFor(ctx, sel, ttl)
		if err != nil {
			return Result{}, revoke, fmt.Errorf("git: %s: %w", sel.Platform.Name(), err)
		}
		r.add(tok.Revoke)

		for _, repo := range sel.Repos {
			dir, err := uniqueDir(dirs, sel.Platform.Name(), sel.Platform.Kind(), repo)
			if err != nil {
				return Result{}, revoke, err
			}
			// The URL carries no credential: the sandbox authenticates
			// through a git credential store instead, so `git remote -v`
			// cannot reveal the token and `tea` can match a repository to
			// its login.
			if err := requireHTTPS(repo.CloneURL); err != nil {
				return Result{}, revoke, fmt.Errorf("git: %s: %w", repo.Slug(), err)
			}
			result.Clones = append(result.Clones, Clone{
				Slug:     repo.Slug(),
				Dir:      dir,
				URL:      repo.CloneURL,
				Platform: sel.Platform.Name(),
				Host:     sel.Platform.Host(),
			})
		}
		result.Tokens = append(result.Tokens, PlatformToken{
			Platform:  sel.Platform.Name(),
			Host:      sel.Platform.Host(),
			Username:  tok.Username,
			Secret:    tok.Secret,
			Renewable: Renewable(sel.Platform),
			ExpiresAt: tok.ExpiresAt,
			All:       sel.All,
		})
	}
	if len(result.Clones) == 0 {
		return Result{}, revoke, fmt.Errorf("git: no repositories selected")
	}
	return result, revoke, nil
}

// oneCredentialPerHost refuses selections that would need two platforms'
// credentials on one host, before any token is minted.
//
// @decision(DL-034)
func oneCredentialPerHost(selections []Selection) error {
	byHost := map[string]string{}
	for _, sel := range selections {
		if sel.Platform == nil || (len(sel.Repos) == 0 && !sel.All) {
			continue
		}
		host, name := credentialHost(sel.Platform.Host()), sel.Platform.Name()
		if other, taken := byHost[host]; taken && other != name {
			return fmt.Errorf("git: platforms %q and %q are both on %s, and a session can hold only one credential per host;"+
				" choose repositories from one of them", other, name, host)
		}
		byHost[host] = name
	}
	return nil
}

// credentialHost is a host as the proxy keys a credential on it:
// lowercased, without the default port, and with GitHub's API and upload
// hosts read as github.com.
//
// @decision(DL-034)
func credentialHost(host string) string {
	h := strings.TrimSuffix(strings.ToLower(host), ":443")
	switch h {
	case "api.github.com", "uploads.github.com":
		return "github.com"
	}
	return h
}

// Result is what a bootstrap produced: the repositories to clone, and the
// per-platform tokens needed to configure the forge CLIs.
//
// @decision(DL-041) @decision(DL-032)
type Result struct {
	Clones []Clone
	Tokens []PlatformToken
}

// ReservedDirs are the workspace directories spov itself owns: a report
// job's output directory and the scratchpad. No clone takes either
// name. opencode's entrypoint holds the same two names, and
// TestTheReservedDirectoriesAgree keeps them together.
//
// @decision(DL-037)
var ReservedDirs = []string{"out", ".scratch"}

// reservedHolder is what holds a reserved name in uniqueDir's map; no
// "<platform>:<slug>" can equal it.
const reservedHolder = "spov:reserved"

// uniqueDir assigns each repo a workspace directory, disambiguating two
// repos with the same name by prefixing the owner. A namespace of
// several segments is joined with '-'.
//
// @decision(DL-041)
func uniqueDir(taken map[string]string, platform, kind string, repo Repo) (string, error) {
	if repo.Name == "" || repo.Owner == "" {
		return "", fmt.Errorf("git: repository %q is missing an owner or name", repo.Slug())
	}
	// Repository names come from a remote API, and the result becomes a
	// directory under the sandbox workspace root. Validate here rather
	// than relying on the entrypoint's own check, which runs much later
	// and has slightly different rules.
	if err := CheckRepoPath(kind, repo.Owner, repo.Name); err != nil {
		return "", fmt.Errorf("git: repository %s: %w", repo.Slug(), err)
	}
	owner := strings.ReplaceAll(repo.Owner, "/", "-")
	id := platform + ":" + repo.Slug()
	candidates := []string{
		repo.Name,
		owner + "-" + repo.Name,
		platform + "-" + owner + "-" + repo.Name,
	}
	for _, dir := range candidates {
		holder, clash := taken[dir]
		if !clash {
			if err := validDirComponent(dir); err != nil {
				return "", fmt.Errorf("git: repository %s: %w", repo.Slug(), err)
			}
			taken[dir] = id
			return dir, nil
		}
		if holder == id {
			return "", fmt.Errorf("git: repository %s is selected twice", repo.Slug())
		}
	}
	return "", fmt.Errorf("git: cannot find a free workspace directory for %s; %q is already taken",
		repo.Slug(), candidates[len(candidates)-1])
}

// validDirComponent rejects anything that must not appear in a workspace
// directory name: path separators, traversal, control characters, and a
// leading dash that a later command would read as an option.
//
// @decision(DL-041)
func validDirComponent(s string) error {
	if s == "" {
		return errors.New("is empty")
	}
	if s == "." || s == ".." {
		return fmt.Errorf("%q is a path traversal component", s)
	}
	if strings.HasPrefix(s, "-") {
		return fmt.Errorf("%q starts with '-' and would be read as a command option", s)
	}
	if strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("%q contains a path separator", s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%q contains a control character", s)
		}
		if unicode.IsSpace(r) {
			return fmt.Errorf("%q contains whitespace", s)
		}
	}
	return nil
}

// revoker collects the revocation functions of every token a Bootstrap
// minted. Its revoke method runs each at most once, so a deferred revoke
// plus an explicit one on an error path does not double-revoke.
//
// @decision(DL-041)
type revoker struct {
	mu    sync.Mutex
	funcs []func(context.Context) error
	done  bool
}

func (r *revoker) add(fn func(context.Context) error) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.funcs = append(r.funcs, fn)
}

// revoke releases every collected token, reporting the first failure but
// attempting all of them: one failure must not strand the rest.
func (r *revoker) revoke(ctx context.Context) error {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return nil
	}
	r.done = true
	funcs := r.funcs
	r.funcs = nil
	r.mu.Unlock()

	var firstErr error
	for _, fn := range funcs {
		if err := fn(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// requireHTTPS rejects clone URLs the credential store cannot serve.
//
// @decision(DL-041)
func requireHTTPS(cloneURL string) error {
	if !strings.HasPrefix(cloneURL, "https://") {
		return fmt.Errorf("clone URL %q is not https", cloneURL)
	}
	return nil
}

// unreachable asks about every repository at once and names the first,
// in selection order, the forge says the credential cannot see; "" when
// none.
//
// @decision(DL-043)
func unreachable(ctx context.Context, checker RepoChecker, repos []Repo) string {
	hidden := make([]bool, len(repos))
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Go(func() { hidden[i] = errors.Is(checker.CheckRepo(ctx, repo), ErrRepoUnreachable) })
	}
	wg.Wait()
	for i, h := range hidden {
		if h {
			return repos[i].Slug()
		}
	}
	return ""
}
