package credproxy

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// The forges' own package registries: a forge token is also set on reads
// of its forge's registry for the session's own namespaces, never on a
// write, so it can install a package and never publish one.
//
// @decision(DL-078)

// GitHubPackageHosts are GitHub Packages' hosts for npm, Maven and NuGet.
// Only a classic PAT reaches them; GitHub refuses an App installation
// token or a fine-grained PAT there.
//
// @decision(DL-078)
var GitHubPackageHosts = []string{"npm.pkg.github.com", "maven.pkg.github.com", "nuget.pkg.github.com"}

// classicPAT is a classic personal access token's prefix.
const classicPAT = "ghp_"

// readsPackages reports whether a GitHub platform's token is one GitHub
// Packages accepts.
func (p Platform) readsPackages() bool {
	return p.Kind == KindGitHub && RequestKey(p.Host) == "github.com" && strings.HasPrefix(p.Token, classicPAT)
}

// PackageHosts are the package hosts a platform's token is set on beyond
// its own, which the session's egress and CA must cover.
//
// @decision(DL-078)
func (p Platform) PackageHosts() []string {
	if p.readsPackages() {
		return slices.Clone(GitHubPackageHosts)
	}
	return nil
}

// rawSegments splits a request's escaped path, for a policy that reads
// escapes itself.
func rawSegments(r *http.Request) ([]string, bool) {
	if r.RequestURI != "" && !strings.HasPrefix(r.RequestURI, "/") {
		return nil, false
	}
	raw := r.URL.EscapedPath()
	if !strings.HasPrefix(raw, "/") {
		return nil, false
	}
	return strings.Split(raw[1:], "/"), true
}

// packageSegment accepts one segment of a package path: never empty or a
// dot segment, and escaped only as %2F or %40, which npm's scoped names
// use. Decoded, it is no dot segment either, so a read cannot climb out
// of the namespace the path names.
func packageSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	rest := strings.NewReplacer("%2F", "", "%2f", "", "%40", "").Replace(s)
	if strings.Contains(rest, "%") {
		return false
	}
	decoded, err := url.PathUnescape(s)
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

func packagePath(segs []string) bool {
	return len(segs) > 0 && !slices.ContainsFunc(segs, func(s string) bool { return !packageSegment(s) })
}

// hasOwner reports whether owner owns one of the session's repositories.
// With AllRepos every owner does.
func (p Platform) hasOwner(owner string) bool {
	if p.AllRepos {
		return true
	}
	for _, r := range p.Repos {
		if i := strings.LastIndex(r, "/"); i > 0 && strings.EqualFold(r[:i], owner) {
			return true
		}
	}
	return false
}

// hasGroup reports whether a GitLab group, by its whole path encoded as
// one segment, is a session project's namespace or one of its parents.
// A numeric group ID is the session's only with AllRepos, since a launch
// records no group's.
func (p Platform) hasGroup(seg string) bool {
	if p.AllRepos {
		return seg != ""
	}
	if isDigits(seg) {
		return false
	}
	decoded := strings.ReplaceAll(strings.ReplaceAll(seg, "%2F", "/"), "%2f", "/")
	parts := strings.Split(decoded, "/")
	if strings.Contains(decoded, "%") || !plainSegments(parts) {
		return false
	}
	want := strings.ToLower(decoded)
	for _, r := range p.Repos {
		ns := strings.ToLower(r[:max(strings.LastIndex(r, "/"), 0)])
		if ns == want || strings.HasPrefix(ns, want+"/") {
			return true
		}
	}
	return false
}

// allowsForgePackages is the package-registry policy, for a request whose
// path the forge's own policy has not decided: handled says whether the
// path is a package registry's, and allowed whether it gets the token.
//
// @decision(DL-078)
func (p Platform) allowsForgePackages(host string, r *http.Request) (handled, allowed bool) {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	segs, ok := rawSegments(r)
	if !ok {
		return false, false
	}
	switch p.Kind {
	case KindGitHub:
		if !slices.Contains(GitHubPackageHosts, host) {
			return false, false
		}
		if !read || !p.readsPackages() || !packagePath(segs) {
			return true, false
		}
		owner := segs[0]
		if host == "npm.pkg.github.com" {
			// npm names a scoped package @owner/name, its slash escaped
			// or not.
			name, _ := url.PathUnescape(owner)
			scope, _, _ := strings.Cut(strings.TrimPrefix(name, "@"), "/")
			if !strings.HasPrefix(name, "@") {
				return true, false
			}
			owner = scope
		}
		return true, p.hasOwner(owner)
	case KindForgejo, KindGitea:
		base, ok := cleanBase(p.BasePath)
		if !ok || !hasPrefix(segs, base) {
			return false, false
		}
		segs = segs[len(base):]
		if len(segs) < 2 || segs[0] != "api" || segs[1] != "packages" {
			return false, false
		}
		return true, read && len(segs) > 3 && plainSegments(segs[2:3]) && p.hasOwner(segs[2]) && packagePath(segs[3:])
	}
	return false, false
}

// allowsGitLabPackages is GitLab's package-registry policy below api/v4:
// a session project's packages, and a group's that is a session
// project's namespace or a parent of it. The instance-level endpoints
// name a package rather than a project, and get nothing.
//
// @decision(DL-078)
func (p Platform) allowsGitLabPackages(segs []string, method string) (handled, allowed bool) {
	read := method == http.MethodGet || method == http.MethodHead
	switch {
	case len(segs) >= 3 && segs[0] == "projects" && segs[2] == "packages":
		return true, read && p.hasProject(segs[1]) && packagePath(segs[3:])
	case len(segs) >= 4 && segs[0] == "groups" && segs[2] == "-" && segs[3] == "packages":
		return true, read && p.hasGroup(segs[1]) && packagePath(segs[4:])
	}
	return false, false
}
