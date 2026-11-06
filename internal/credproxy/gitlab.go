package credproxy

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// The GitLab policy. A GitLab personal access token reaches every project
// its account can, so, as for a Forgejo token, the proxy adds it only to
// allowed endpoints of the session's projects. GitLab names a project in
// its API by its whole path encoded as one segment, or by a numeric ID
// the launch recorded.
//
// @decision(DL-034)

// gitlabSections are the parts of a project's REST API the token is used
// on, by the segment after projects/<id> (with repository/<x> as one key),
// and whether writes are allowed there or only reads. Merge requests and
// issues allow writes only where gitlabItemWrites says. Everything absent
// is refused: hooks, members, variables, deploy keys and tokens, access
// tokens, protected branches and tags, mirrors, transfer, export,
// runners, releases, packages and the rest.
//
// @decision(DL-034)
var gitlabSections = map[string]bool{
	"repository/files":    true,
	"repository/commits":  true,
	"merge_requests":      true,
	"issues":              true,
	"repository/branches": false,
	"repository/tags":     false,
	"repository/tree":     false,
	"repository/blobs":    false,
	"repository/compare":  false,
	"repository/archive":  false,
	"labels":              false,
	"milestones":          false,
	"pipelines":           false,
	"jobs":                false,
	"statuses":            false,
}

// gitlabNamed are the sections whose next segment is a name GitLab takes
// encoded, since it may hold a slash: a file path, a branch, a tag.
var gitlabNamed = map[string]bool{
	"repository/files":    true,
	"repository/branches": true,
	"repository/tags":     true,
}

// gitlabItemWrites are the writes allowed on one merge request or issue,
// by what follows its number: the item itself (edit, not delete), and its
// notes and discussions. Anything else under it is read only, so merging,
// approving, rebasing, starting a pipeline, changing approval rules and
// moving or cloning an issue into another project are all refused.
//
// @decision(DL-034)
var gitlabItemWrites = map[string]bool{"notes": true, "discussions": true}

// gitlabTopLevel are the reads a CLI makes before it names a project.
var gitlabTopLevel = map[string]bool{
	"user": true, "version": true, "metadata": true,
}

// gitlabCredentialQuery are query parameters GitLab reads a credential or
// an identity from ahead of the header the proxy sets; a request carrying
// one gets none, so a placeholder never decides who GitLab acts as.
var gitlabCredentialQuery = []string{"private_token", "access_token", "job_token", "sudo"}

// allowsGitLab is the policy for GitLab: the REST API under api/v4,
// GraphQL, and git below the base path.
//
// @decision(DL-034)
func (p Platform) allowsGitLab(r *http.Request, read bool) bool {
	if r.RequestURI != "" && !strings.HasPrefix(r.RequestURI, "/") {
		return false
	}
	if carriesQueryCredential(r.URL.RawQuery) {
		return false
	}
	raw := r.URL.EscapedPath()
	if !strings.HasPrefix(raw, "/") {
		return false
	}
	segs := strings.Split(raw[1:], "/")
	base, ok := cleanBase(p.BasePath)
	if !ok || !hasPrefix(segs, base) {
		return false
	}
	segs = segs[len(base):]
	switch {
	case len(segs) == 2 && segs[0] == "api" && segs[1] == "graphql":
		// GraphQL names no project: there the token reaches what its
		// owner gave it, as a GitHub PAT does.
		return r.Method == http.MethodPost
	case len(segs) > 2 && segs[0] == "api" && segs[1] == "v4":
		return p.allowsGitLabAPI(segs[2:], r.Method)
	}
	if !plainSegments(segs) {
		return false
	}
	return p.allowsGitLabGit(segs, r.Method)
}

// allowsGitLabAPI checks a REST path below api/v4.
func (p Platform) allowsGitLabAPI(segs []string, method string) bool {
	read := method == http.MethodGet || method == http.MethodHead
	if len(segs) == 1 && gitlabTopLevel[segs[0]] {
		return read
	}
	if len(segs) == 2 && segs[0] == "personal_access_tokens" && segs[1] == "self" {
		return read
	}
	// @decision(DL-078)
	if handled, allowed := p.allowsGitLabPackages(segs, method); handled {
		return allowed
	}
	if len(segs) < 2 || segs[0] != "projects" || !p.hasProject(segs[1]) {
		return false
	}
	rest := segs[2:]
	if len(rest) == 0 {
		// The project itself: reading it, never changing or deleting it.
		return read
	}
	key, n := rest[0], 1
	if key == "repository" && len(rest) > 1 {
		key, n = "repository/"+rest[1], 2
		// An archive's format is its suffix; the section is read only.
		if strings.HasPrefix(rest[1], "archive.") && len(rest) == 2 {
			return read
		}
	}
	writable, listed := gitlabSections[key]
	if !listed {
		return false
	}
	tail := rest[n:]
	// The one segment after a named section may be encoded and hold a
	// dot; nothing else past the project may do either. GitLab routes
	// "merge.json" as "merge", so a dot is refused where a name is not
	// expected rather than matched.
	if gitlabNamed[key] && len(tail) > 0 {
		if !encodedName(tail[0]) || !plainNames(tail[1:]) {
			return false
		}
	} else if !plainNames(tail) {
		return false
	}
	if read || !writable {
		return read
	}
	if key == "merge_requests" || key == "issues" {
		return itemWrite(tail, method)
	}
	return true
}

// itemWrite reports whether a write to a merge request or issue path is
// one gitlabItemWrites allows: creating one on the collection, editing
// one (never deleting it), or anything on its notes and discussions.
//
// @decision(DL-034)
func itemWrite(tail []string, method string) bool {
	switch {
	case len(tail) == 0:
		return method == http.MethodPost
	case len(tail) == 1:
		return method == http.MethodPut
	default:
		return gitlabItemWrites[tail[1]]
	}
}

// plainNames are plain segments with no dot, which GitLab would read as
// a format suffix.
func plainNames(segs []string) bool {
	return plainSegments(segs) && !slices.ContainsFunc(segs, func(s string) bool { return strings.Contains(s, ".") })
}

// carriesQueryCredential reports whether a raw query names one of
// gitlabCredentialQuery, in any case and split on '&' or ';' as GitLab's
// Rack splits it.
//
// @decision(DL-034)
func carriesQueryCredential(raw string) bool {
	for _, pair := range strings.FieldsFunc(raw, func(r rune) bool { return r == '&' || r == ';' }) {
		k, _, _ := strings.Cut(pair, "=")
		if d, err := url.QueryUnescape(k); err == nil {
			k = d
		}
		k = strings.ToLower(strings.TrimSpace(k))
		for _, c := range gitlabCredentialQuery {
			if k == c || strings.HasPrefix(k, c+"[") {
				return true
			}
		}
	}
	return false
}

// allowsGitLabGit checks git smart HTTP, LFS and GitLab's own LFS
// transfer path, below a repository path of any depth.
func (p Platform) allowsGitLabGit(segs []string, method string) bool {
	for _, s := range gitServices {
		if len(segs) > len(s) && slices.Equal(segs[len(segs)-len(s):], s) {
			return p.hasRepoPath(segs[:len(segs)-len(s)])
		}
	}
	// <project>.git/gitlab-lfs/objects/<oid>[/<size>], which the LFS
	// batch answer points a same-host transfer at.
	for i := len(segs) - 3; i >= 1 && i >= len(segs)-4; i-- {
		if segs[i] == "gitlab-lfs" && segs[i+1] == "objects" {
			if method != http.MethodGet && method != http.MethodHead && method != http.MethodPut {
				return false
			}
			return p.hasRepoPath(segs[:i])
		}
	}
	return false
}

// hasProject reports whether the API's project segment names one of the
// session's projects: its whole path with every slash encoded as %2F, or
// a numeric ID the launch recorded. No other escape is accepted, and the
// decoded path is checked for empty and dot segments.
//
// @decision(DL-034)
func (p Platform) hasProject(seg string) bool {
	if seg == "" {
		return false
	}
	if isDigits(seg) {
		// With git access all every project is the session's, so any
		// ID is; otherwise only one the launch recorded.
		return p.AllRepos || slices.Contains(p.ProjectIDs, seg)
	}
	decoded := strings.ReplaceAll(strings.ReplaceAll(seg, "%2F", "/"), "%2f", "/")
	if strings.Contains(decoded, "%") {
		return false
	}
	parts := strings.Split(decoded, "/")
	// The API names a project without git's .git suffix.
	if !plainSegments(parts) || len(parts) < 2 || strings.HasSuffix(parts[len(parts)-1], ".git") {
		return false
	}
	return p.hasRepoPath(parts)
}

// hasRepoPath reports whether segs, a repository's whole path with any
// .git on its last segment, is one of the session's. With AllRepos any
// path of an owner and a name is.
//
// @decision(DL-034) @decision(DL-041)
func (p Platform) hasRepoPath(segs []string) bool {
	if len(segs) < 2 {
		return false
	}
	if p.AllRepos {
		return true
	}
	last := len(segs) - 1
	return p.hasRepo(strings.Join(segs[:last], "/"), segs[last])
}

// plainSegments reports whether every segment is a plain name: not empty,
// not a dot segment, and without percent-encoding.
func plainSegments(segs []string) bool {
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || strings.Contains(s, "%") {
			return false
		}
	}
	return true
}

// encodedName reports whether a named section's segment is a well-formed
// encoding of a name whose parts are not empty or dot segments. An
// encoded percent sign is refused, so a name cannot be decoded twice into
// something else.
func encodedName(seg string) bool {
	if seg == "" || strings.Contains(strings.ToUpper(seg), "%25") {
		return false
	}
	decoded, err := url.PathUnescape(seg)
	if err != nil {
		return false
	}
	for _, part := range strings.Split(decoded, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
