package git

import (
	"errors"
	"fmt"
	"strings"
)

// The forge kinds spov speaks. A kind names the product a platform runs,
// and the same string is used by the config, the proxy, the sandbox's
// CLI choice and the API.
//
// @decision(DL-041)
const (
	KindGitHub  = "github"
	KindForgejo = "forgejo"
	KindGitea   = "gitea"
	KindGitLab  = "gitlab"
)

// Kinds is every kind, in the order help text lists them.
//
// @decision(DL-041)
var Kinds = []string{KindGitHub, KindForgejo, KindGitea, KindGitLab}

// KnownKind reports whether kind is one of Kinds.
func KnownKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// KindName is a kind as a person reads it.
//
// @decision(DL-041)
func KindName(kind string) string {
	switch kind {
	case KindGitHub:
		return "GitHub"
	case KindForgejo:
		return "Forgejo"
	case KindGitea:
		return "Gitea"
	case KindGitLab:
		return "GitLab"
	}
	return kind
}

// ChangeNoun is what a kind calls a change request: a merge request on
// GitLab, a pull request elsewhere, and on an unknown kind too.
//
// @decision(DL-051)
func ChangeNoun(kind string) string {
	if kind == KindGitLab {
		return "merge request"
	}
	return "pull request"
}

// ChangeRef is a change request's number as its forge writes it: !12 on
// GitLab, #12 elsewhere.
//
// @decision(DL-051)
func ChangeRef(kind string, number int) string {
	if kind == KindGitLab {
		return fmt.Sprintf("!%d", number)
	}
	return fmt.Sprintf("#%d", number)
}

// NestedNamespaces reports whether a kind's repositories may live under
// a namespace of several segments, as a GitLab project in a subgroup
// does.
//
// @decision(DL-041)
func NestedNamespaces(kind string) bool { return kind == KindGitLab }

// SplitPath splits a repository's full path at its last slash into the
// namespace and the name. It checks only that both halves are there;
// CheckRepoPath checks what they contain.
//
// @decision(DL-041)
func SplitPath(path string) (owner, name string, err error) {
	i := strings.LastIndex(path, "/")
	if i <= 0 || i == len(path)-1 {
		return "", "", fmt.Errorf("%q is not owner/name", path)
	}
	return path[:i], path[i+1:], nil
}

// CheckRepoPath validates a repository's namespace and name for a kind.
// Every segment becomes a URL path segment and part of a workspace
// directory name, so the alphabet is the one every forge allows for
// names: letters, digits, '.', '_' and '-'. A namespace of more than
// one segment is accepted only for a kind with nested namespaces.
//
// @decision(DL-041)
func CheckRepoPath(kind, owner, name string) error {
	if err := checkSegment(name); err != nil {
		return fmt.Errorf("name %w", err)
	}
	if owner == "" {
		return errors.New("owner is empty")
	}
	segs := strings.Split(owner, "/")
	if len(segs) > 1 && !NestedNamespaces(kind) {
		return fmt.Errorf("owner %q has more than one segment, which only a GitLab project's group may", owner)
	}
	for _, s := range segs {
		if err := checkSegment(s); err != nil {
			return fmt.Errorf("owner %w", err)
		}
	}
	return nil
}

// checkSegment validates one segment of a repository path.
func checkSegment(s string) error {
	switch {
	case s == "":
		return errors.New("has an empty segment")
	case s == "." || s == "..":
		return fmt.Errorf("%q is a path traversal segment", s)
	case strings.HasPrefix(s, "-"):
		return fmt.Errorf("%q starts with '-'", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("%q contains %q; a repository path has only letters, digits, '.', '_' and '-'", s, r)
		}
	}
	return nil
}
