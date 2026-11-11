package credproxy

import (
	"net/http"
	"slices"
	"strings"
)

// The endpoint policy for a stored token, a Forgejo token or a GitHub
// PAT: its credential is added only to an allowed endpoint of one of
// the session's repositories. A GitHub App token is narrowed by GitHub
// itself and is added on any path of its hosts.
//
// @decision(DL-034)

// repoSections are the parts of a repository's REST API a stored token
// is used on, by the first path segment after /repos/<owner>/<repo>, and
// whether any method is allowed there or only reads. Keys, hooks,
// collaborators, invitations, transfer, settings, Actions secrets and
// variables, and push mirrors are refused by being absent. Branches are
// read only, since GitHub keeps branch protection below them and git
// creates branches itself; merging a pull request is refused below.
// Labels and milestones are read only: tea reads a repository's labels
// before it creates a pull request, and setting one on an issue or pull
// request goes through issues.
//
// @decision(DL-034)
var repoSections = map[string]bool{
	"contents":     true,
	"git":          true,
	"branches":     false,
	"commits":      true,
	"pulls":        true,
	"issues":       true,
	"statuses":     false,
	"check-runs":   false,
	"check-suites": false,
	"compare":      false,
	"raw":          false,
	"labels":       false,
	"milestones":   false,
}

// gitServices are the git smart HTTP and LFS paths after <owner>/<repo>.
//
// @decision(DL-034)
var gitServices = [][]string{
	{"info", "refs"},
	{"git-upload-pack"},
	{"git-receive-pack"},
	{"info", "lfs", "objects", "batch"},
}

// allows reports whether the platform's credential is added to a request
// for host and the request's path.
//
// @decision(DL-034)
func (p Platform) allows(host string, r *http.Request) bool {
	if p.Scoped {
		return true
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	// The forge's package registry reads its own escapes.
	//
	// @decision(DL-078)
	if handled, allowed := p.allowsForgePackages(host, r); handled {
		return allowed
	}
	// GitLab encodes a project's path into one segment, so it splits the
	// path itself rather than refusing every escape.
	//
	// @decision(DL-034)
	if p.Kind == KindGitLab {
		return p.allowsGitLab(r, read)
	}
	segs, ok := cleanSegments(r)
	if !ok {
		return false
	}
	switch p.Kind {
	case KindForgejo, KindGitea:
		return p.allowsForgejo(segs, read)
	case KindGitHub:
		return p.allowsGitHub(host, segs, read, r.Method)
	}
	// A kind without a policy gets no credential anywhere.
	//
	// @decision(DL-041)
	return false
}

// allowsForgejo is the policy for Forgejo and Gitea, which share their
// API's paths: the API under api/v1 and git below the base path.
//
// @decision(DL-034) @decision(DL-041)
func (p Platform) allowsForgejo(segs []string, read bool) bool {
	base, ok := cleanBase(p.BasePath)
	if !ok || !hasPrefix(segs, base) {
		return false
	}
	segs = segs[len(base):]
	if len(segs) > 1 && segs[0] == "api" && segs[1] == "v1" {
		return p.allowsAPI(segs[2:], read)
	}
	return p.allowsGit(segs)
}

// allowsGitHub is the policy for GitHub. github.com serves git and
// api.github.com the API; GitHub Enterprise serves git at its root and
// the API under /api/v3.
//
// @decision(DL-034)
func (p Platform) allowsGitHub(host string, segs []string, read bool, method string) bool {
	switch {
	case host == "api.github.com":
		return p.allowsGitHubAPI(segs, read, method)
	case host == "uploads.github.com":
		return false
	case host == "github.com":
		return p.allowsGit(segs)
	case len(segs) > 1 && segs[0] == "api" && segs[1] == "v3":
		return p.allowsGitHubAPI(segs[2:], read, method)
	case len(segs) == 2 && segs[0] == "api" && segs[1] == "graphql":
		return method == http.MethodPost
	default:
		return p.allowsGit(segs)
	}
}

// githubActionsReads are the parts of a repository's Actions API a
// GitHub PAT is used on, read only. Secrets, variables, runners,
// permissions, OIDC and caches are refused by being absent.
//
// @decision(DL-107)
var githubActionsReads = map[string]bool{
	"runs":      true,
	"jobs":      true,
	"workflows": true,
	"artifacts": true,
}

// allowsGitHubAPI is allowsAPI plus the GitHub-only paths: GraphQL and
// search, which name no repository, and a repository's Actions reads and
// releases.
//
// @decision(DL-034) @decision(DL-107)
func (p Platform) allowsGitHubAPI(segs []string, read bool, method string) bool {
	switch {
	case len(segs) == 1 && segs[0] == "graphql":
		return method == http.MethodPost
	case len(segs) == 2 && segs[0] == "search":
		return read
	case len(segs) > 3 && segs[0] == "repos" && p.hasRepo(segs[1], segs[2]):
		switch segs[3] {
		case "releases":
			return read
		case "actions":
			return read && len(segs) > 4 && githubActionsReads[segs[4]]
		}
	}
	return p.allowsAPI(segs, read)
}

// allowsAPI checks a REST path below the API root.
func (p Platform) allowsAPI(segs []string, read bool) bool {
	switch {
	case len(segs) == 1 && (segs[0] == "user" || segs[0] == "version" || segs[0] == "rate_limit"):
		// The reads a CLI makes before it names a repository.
		return read
	case len(segs) < 3 || segs[0] != "repos" || !p.hasRepo(segs[1], segs[2]):
		return false
	case len(segs) == 3:
		// The repository itself: reading it, never changing or deleting it.
		return read
	}
	anyMethod, listed := repoSections[segs[3]]
	if segs[3] == "pulls" && len(segs) > 5 && segs[5] == "merge" {
		anyMethod = false
	}
	return listed && (anyMethod || read)
}

// allowsGit checks a git smart HTTP path: <owner>/<repo>[.git]/<service>.
func (p Platform) allowsGit(segs []string) bool {
	if len(segs) < 3 || !p.hasRepo(segs[0], segs[1]) {
		return false
	}
	for _, s := range gitServices {
		if slices.Equal(segs[2:], s) {
			return true
		}
	}
	// An LFS object transfer, where the batch response points a
	// same-host transfer (Forgejo's basic adapter).
	rest := segs[2:]
	return len(rest) >= 4 && len(rest) <= 5 && rest[0] == "info" && rest[1] == "lfs" && rest[2] == "objects" &&
		rest[3] != "batch" && (len(rest) == 4 || rest[4] == "verify")
}

// hasRepo compares case-insensitively, with any .git removed. With
// AllRepos every repository is the session's.
//
// @decision(DL-034) @decision(DL-043)
func (p Platform) hasRepo(owner, name string) bool {
	if p.AllRepos {
		return true
	}
	want := strings.ToLower(owner + "/" + strings.TrimSuffix(name, ".git"))
	for _, r := range p.Repos {
		if strings.ToLower(strings.TrimSuffix(r, ".git")) == want {
			return true
		}
	}
	return false
}

// cleanSegments splits the request's path, refusing one with
// percent-encoding, a dot segment or an empty segment, with or without
// path parameters.
func cleanSegments(r *http.Request) ([]string, bool) {
	// An absolute-form request line names its own target.
	if r.RequestURI != "" && !strings.HasPrefix(r.RequestURI, "/") {
		return nil, false
	}
	raw := r.URL.EscapedPath()
	if !strings.HasPrefix(raw, "/") || strings.Contains(raw, "%") {
		return nil, false
	}
	segs := strings.Split(raw[1:], "/")
	for _, s := range segs {
		// A servlet container drops a segment's `;parameters` before it
		// resolves dot segments, so `..;x` is `..` there.
		name, _, _ := strings.Cut(s, ";")
		if name == "" || name == "." || name == ".." {
			return nil, false
		}
	}
	return segs, true
}

// cleanBase splits a Forgejo base path ("" or "/forgejo").
func cleanBase(base string) ([]string, bool) {
	base = strings.Trim(base, "/")
	if base == "" {
		return nil, true
	}
	segs := strings.Split(base, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return nil, false
		}
	}
	return segs, true
}

func hasPrefix(segs, prefix []string) bool {
	return len(segs) >= len(prefix) && slices.Equal(segs[:len(prefix)], prefix)
}
