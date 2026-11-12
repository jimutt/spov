package git

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// MaxSyncBundleBytes bounds a git bundle carried between the CLI and a
// sandbox. A larger unpushed history should be pushed to the forge
// first.
//
// @decision(DL-027)
const MaxSyncBundleBytes = 16 << 20

// MaxSyncExportBytes bounds the total decoded sync-export payload
// (bundle + working-tree archive).
//
// @decision(DL-027)
const MaxSyncExportBytes = 32 << 20

// MaxSyncIncomingBytes bounds the total size of the git objects a sync
// brings in, and MaxSyncIncomingBlobBytes any single blob. The bundle's
// own bytes are small (MaxSyncExportBytes), but its deltas can expand to
// much more at fetch or checkout; these bound what reaches the disk.
//
// @decision(DL-126)
const (
	MaxSyncIncomingBytes     = 1 << 30   // 1 GiB
	MaxSyncIncomingBlobBytes = 256 << 20 // 256 MiB
)

// Overridable for tests; the production values are the constants above.
var (
	maxSyncIncomingBytes     int64 = MaxSyncIncomingBytes
	maxSyncIncomingBlobBytes int64 = MaxSyncIncomingBlobBytes
)

// LocalCheckout describes a clean local git checkout ready to seed a
// synced session.
//
// @decision(DL-027)
type LocalCheckout struct {
	// Root is the top-level working tree directory (`git rev-parse --show-toplevel`).
	Root string
	// Remote is the remote name (`branch.<name>.remote` or `origin`).
	Remote string
	// RemoteURL is the raw remote URL configured in git.
	RemoteURL string
	// Host is the normalized forge host (lowercased, `:443` stripped).
	Host string
	// BasePath is any leading path before owner/name on a subpath forge.
	BasePath string
	// Owner and Name identify the repository on the forge.
	Owner string
	Name  string
	// Branch is the attached local branch name (`refs/heads/<Branch>`).
	Branch string
	// HeadCommit is the 40-character lowercase hex SHA of HEAD.
	HeadCommit string
	// Bundle holds the git bundle of commits ahead of the remote when
	// HEAD is not already reachable from a remote-tracking ref on Remote.
	Bundle []byte
}

// ExportedState is the repository state exported from a running sandbox
// for sync-back to a local checkout.
//
// @decision(DL-027)
type ExportedState struct {
	// Branch is the current branch in the sandbox (empty when detached).
	Branch string
	// BaseCommit is the commit the session started from.
	BaseCommit string
	// HeadCommit is the current HEAD commit in the sandbox.
	HeadCommit string
	// Bundle is a git bundle of commits between BaseCommit and HeadCommit
	// (empty when HeadCommit == BaseCommit).
	Bundle []byte
	// WorktreeTar is a gzipped tar archive of modified, staged and
	// untracked non-ignored files relative to the repository root.
	WorktreeTar []byte
	// Deleted lists tracked repository-relative paths deleted in the
	// working tree or index.
	Deleted []string
}

// SyncResult describes how ApplySync landed the sandbox's state in the
// local repository.
//
// @decision(DL-027)
type SyncResult struct {
	// NoChanges is true when the sandbox had neither new commits nor
	// working-tree edits.
	NoChanges bool
	// InPlace is true when the checked-out local branch and working tree
	// were updated directly; false when FallbackBranch was written instead.
	InPlace bool
	// Branch is the checked-out branch updated when InPlace is true.
	Branch string
	// FallbackBranch is the `spov/<session>` branch written when the
	// local checkout moved or became dirty while the session ran.
	FallbackBranch string
	// Reason explains why FallbackBranch was used instead of an in-place
	// update.
	Reason string
	// HeadCommit is the commit the updated branch or FallbackBranch now
	// points at.
	HeadCommit string
	// Commits counts the new commits brought back from the sandbox (not
	// counting a synthetic WIP commit on FallbackBranch).
	Commits int
	// FilesChanged counts the uncommitted working-tree files unpacked or
	// deleted.
	FilesChanged int
	// DirtyCommitted is true when uncommitted sandbox edits were captured
	// as a WIP commit on FallbackBranch.
	DirtyCommitted bool
	// RangeFrom and RangeTo are the short commit names bounding the
	// commits brought back; empty when there are none.
	RangeFrom, RangeTo string
	// Changes lists every path the sync changed, added or deleted
	// relative to what the checkout held before, sorted by path.
	//
	// @decision(DL-136)
	Changes []PathChange
}

// PathChange is one path a sync changed: Status is 'A' (added), 'M'
// (modified) or 'D' (deleted).
//
// @decision(DL-136)
type PathChange struct {
	Status byte
	Path   string
}

var hexCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidCommitSHA reports whether s is a 40-character lowercase hex SHA-1.
//
// @decision(DL-027)
func ValidCommitSHA(s string) bool {
	return hexCommitRE.MatchString(strings.ToLower(strings.TrimSpace(s)))
}

// ValidBranchName checks whether name is a valid git branch name under
// `git check-ref-format --branch` rules.
//
// @decision(DL-027)
func ValidBranchName(name string) error {
	if name == "" {
		return errors.New("branch name is empty")
	}
	if name == "@" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("invalid branch name %q", name)
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") || strings.Contains(name, `\`) {
		return fmt.Errorf("invalid branch name %q", name)
	}
	for _, r := range name {
		if r <= 0x20 || r == 0x7f || strings.ContainsRune("~^:?*[", r) {
			return fmt.Errorf("invalid branch name %q", name)
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return fmt.Errorf("invalid branch name %q", name)
		}
	}
	return nil
}

// InCheckout reports whether dir is inside a git working tree. Only
// git's own "not a git repository", or no git at all, is outside one: a
// checkout git refuses to read (an unsafe owner, a corrupt .git) is
// inside, so syncing it fails rather than being skipped.
//
// @decision(DL-095)
func InCheckout(ctx context.Context, dir string) bool {
	if _, err := exec.LookPath("git"); err != nil {
		return false
	}
	_, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
	return err == nil || !notARepository(err)
}

// notARepository is git's answer for a directory outside any checkout.
func notARepository(err error) bool {
	return strings.Contains(err.Error(), "not a git repository")
}

// InspectLocalCheckout verifies that dir is inside a clean git working
// tree on an attached branch, resolves its remote URL, and packs any
// commits ahead of the remote into a bundle when bundleAhead is true.
//
// @decision(DL-027)
func InspectLocalCheckout(ctx context.Context, dir string, kindsByHost map[string]string, bundleAhead bool) (LocalCheckout, error) {
	root, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if notARepository(err) {
			return LocalCheckout{}, errors.New("not in a git repository; cd into a checkout or pass --no-sync")
		}
		return LocalCheckout{}, fmt.Errorf("git cannot read the checkout in %s: %w; fix it or pass --no-sync", dir, err)
	}
	gitDir, err := gitOutput(ctx, root, "rev-parse", "--git-dir")
	if err != nil {
		return LocalCheckout{}, fmt.Errorf("git: locate .git in %s: %w", root, err)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	for _, marker := range []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDir, marker)); err == nil {
			return LocalCheckout{}, fmt.Errorf("git checkout in %s has an in-progress operation (%s); finish or abort it before syncing", root, marker)
		}
	}

	status, err := gitOutputRaw(ctx, root, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return LocalCheckout{}, fmt.Errorf("git status in %s: %w", root, err)
	}
	if strings.TrimSpace(status) != "" {
		return LocalCheckout{}, fmt.Errorf("local checkout in %s has uncommitted or untracked changes; commit or stash them before syncing, or pass --no-sync", root)
	}

	branch, err := gitOutput(ctx, root, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil || branch == "" {
		return LocalCheckout{}, fmt.Errorf("local checkout in %s is in detached HEAD state; check out a branch before syncing, or pass --no-sync", root)
	}
	if err := ValidBranchName(branch); err != nil {
		return LocalCheckout{}, err
	}

	head, err := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil || !ValidCommitSHA(head) {
		return LocalCheckout{}, fmt.Errorf("local branch %q in %s has no commits yet", branch, root)
	}
	head = strings.ToLower(head)

	remote, _ := gitConfigValue(ctx, root, "branch."+branch+".remote")
	if remote == "" || remote == "." {
		remote = "origin"
	}
	remoteURL, _ := gitOutput(ctx, root, "remote", "get-url", remote)
	var host, basePath, owner, name string
	if strings.TrimSpace(remoteURL) != "" {
		host, basePath, owner, name, err = ParseRemoteURL(remoteURL, kindsByHost)
		if err != nil {
			return LocalCheckout{}, fmt.Errorf("remote %q (%s): %w", remote, RedactURL(remoteURL), err)
		}
	} else {
		remote = ""
		owner = "local"
		name = filepath.Base(root)
		if err := CheckRepoPath(KindGitHub, owner, name); err != nil {
			return LocalCheckout{}, fmt.Errorf("local checkout directory name %q: %w", name, err)
		}
	}

	out := LocalCheckout{
		Root:       root,
		Remote:     remote,
		RemoteURL:  remoteURL,
		Host:       host,
		BasePath:   basePath,
		Owner:      owner,
		Name:       name,
		Branch:     branch,
		HeadCommit: head,
	}
	if !bundleAhead {
		return out, nil
	}

	var bundle []byte
	if remote != "" {
		bundle, err = bundleUnpushedCommits(ctx, root, remote, branch, head)
	} else {
		bundle, err = BundleHEAD(ctx, root)
	}
	if err != nil {
		return LocalCheckout{}, err
	}
	out.Bundle = bundle
	return out, nil
}

// BundleHEAD creates a full git bundle of HEAD at root, bounded to
// MaxSyncBundleBytes, for seeding a sandbox without cloning from a
// forge.
//
// @decision(DL-027)
func BundleHEAD(ctx context.Context, root string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "spov-head-*.bundle")
	if err != nil {
		return nil, err
	}
	bundlePath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(bundlePath) }()

	if _, err := gitOutput(ctx, root, "bundle", "create", bundlePath, "HEAD"); err != nil {
		return nil, fmt.Errorf("git: bundle HEAD: %w", err)
	}
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSyncBundleBytes {
		return nil, fmt.Errorf("repository history in %s is %d MiB (limit %d MiB for local bundle seeding)",
			root, len(data)>>20, MaxSyncBundleBytes>>20)
	}
	return data, nil
}

// ParseRemoteURL extracts the normalized host, optional forge base path,
// owner and repository name from an HTTPS, ssh:// or SCP-style git
// remote URL. kindsByHost maps a configured platform's host (or
// host+basePath) to its forge kind so GitLab nested group paths and
// subpath instances split accurately.
//
// @decision(DL-027) @decision(DL-041)
func ParseRemoteURL(raw string, kindsByHost map[string]string) (host, basePath, owner, name string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", "", "", errors.New("remote URL is empty")
	}
	var rawHost, rawPath string
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return "", "", "", "", err
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git":
		default:
			return "", "", "", "", fmt.Errorf("unsupported remote scheme %q", u.Scheme)
		}
		rawHost = u.Host
		if u.Scheme == "ssh" || u.Scheme == "git" {
			if h, _, splitErr := net.SplitHostPort(rawHost); splitErr == nil {
				rawHost = h
			}
		} else {
			rawHost = strings.TrimSuffix(strings.TrimSuffix(rawHost, ":443"), ":80")
		}
		rawPath = u.Path
	case strings.Contains(s, ":"):
		// SCP-style: [user@]host:path/to/repo.git
		left, right, _ := strings.Cut(s, ":")
		if strings.Contains(left, "/") || right == "" {
			return "", "", "", "", fmt.Errorf("cannot parse remote URL %q", RedactURL(raw))
		}
		if _, h, ok := strings.Cut(left, "@"); ok {
			rawHost = h
		} else {
			rawHost = left
		}
		rawPath = right
	default:
		return "", "", "", "", fmt.Errorf("cannot parse remote URL %q", RedactURL(raw))
	}

	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(rawHost)), ".")
	if host == "" {
		return "", "", "", "", fmt.Errorf("remote URL %q has no host", RedactURL(raw))
	}
	trimmed := strings.Trim(strings.TrimSuffix(strings.TrimSpace(rawPath), ".git"), "/")
	if trimmed == "" {
		return "", "", "", "", fmt.Errorf("remote URL %q has no repository path", RedactURL(raw))
	}

	kind := kindsByHost[host]
	// Check if a configured platform on this host has a base path prefix
	// (stored in kindsByHost as "host/base").
	for k, v := range kindsByHost {
		h, bp, ok := strings.Cut(k, "/")
		if !ok || h != host || bp == "" {
			continue
		}
		if strings.HasPrefix(trimmed, bp+"/") {
			basePath = "/" + bp
			trimmed = strings.TrimPrefix(trimmed, bp+"/")
			kind = v
			break
		}
	}

	owner, name, err = SplitPath(trimmed)
	if err != nil {
		return "", "", "", "", err
	}
	if kind != "" {
		if err := CheckRepoPath(kind, owner, name); err != nil {
			return "", "", "", "", err
		}
	} else {
		// Validate segments conservatively when no kind was matched yet.
		for _, seg := range strings.Split(owner, "/") {
			if err := CheckRepoPath(KindGitHub, seg, name); err != nil {
				return "", "", "", "", err
			}
		}
	}
	return host, basePath, owner, name, nil
}

// bundleUnpushedCommits builds a git bundle when head is not already at
// a remote-tracking ref of remote. When a remote-tracking ref is an
// ancestor of head, only `<base>..HEAD` is bundled; otherwise all of
// HEAD is bundled.
//
// @decision(DL-027)
func bundleUnpushedCommits(ctx context.Context, root, remote, branch, head string) ([]byte, error) {
	return bundleAhead(ctx, root, remote, branch, head, "HEAD")
}

// BundleCommit packs commit from a local checkout for a resume's fresh
// clone, as a launch packs HEAD: from the newest remote-tracking
// ancestor, else the commit's whole history. It is nil when the commit
// is a remote branch's tip, which the clone fetches itself.
//
// @decision(DL-098)
func BundleCommit(ctx context.Context, root, remote, commit string) ([]byte, error) {
	commit = strings.ToLower(strings.TrimSpace(commit))
	if !ValidCommitSHA(commit) {
		return nil, fmt.Errorf("git: %q is not a commit", commit)
	}
	if !commitExists(ctx, root, commit) {
		return nil, fmt.Errorf("the checkout in %s does not have commit %s", root, commit[:12])
	}
	// A bundle names its tips by ref, so the commit gets one of its own
	// for as long as the bundle takes.
	ref := fmt.Sprintf("refs/spov/resume-seed-%d-%d", os.Getpid(), time.Now().UnixNano())
	if _, err := gitOutput(ctx, root, "update-ref", ref, commit); err != nil {
		return nil, fmt.Errorf("git: mark %s for bundling: %w", commit[:12], err)
	}
	defer func() { _, _ = gitOutput(context.WithoutCancel(ctx), root, "update-ref", "-d", ref, commit) }()
	return bundleAhead(ctx, root, remote, "", commit, ref)
}

// bundleAhead packs tip, which is head, from the newest remote-tracking
// ancestor of head, or whole when there is none.
func bundleAhead(ctx context.Context, root, remote, branch, head, tip string) ([]byte, error) {
	refsOut, err := gitOutputRaw(ctx, root, "for-each-ref", "--format=%(objectname) %(refname)", "refs/remotes/"+remote+"/")
	if err != nil {
		return nil, fmt.Errorf("git: inspect remote refs in %s: %w", root, err)
	}
	var bases []string
	var preferredBase string
	wantTracking := "refs/remotes/" + remote + "/" + branch
	wantDefault := "refs/remotes/" + remote + "/HEAD"
	for _, line := range strings.Split(strings.TrimSpace(refsOut), "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !ValidCommitSHA(sha) {
			continue
		}
		sha = strings.ToLower(sha)
		if sha == head {
			// Already a remote branch's tip, which the sandbox fetches.
			//
			// @decision(DL-098)
			return nil, nil
		}
		if isAncestor(ctx, root, sha, head) {
			if ref == wantDefault || (ref == wantTracking && preferredBase == "") {
				preferredBase = sha
			}
			if !slices.Contains(bases, sha) {
				bases = append(bases, sha)
			}
		}
	}

	tmp, err := os.CreateTemp("", "spov-sync-*.bundle")
	if err != nil {
		return nil, err
	}
	bundlePath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(bundlePath) }()

	args := []string{"bundle", "create", bundlePath, tip}
	if preferredBase != "" {
		args = []string{"bundle", "create", bundlePath, preferredBase + ".." + tip}
	} else if len(bases) > 0 {
		args = []string{"bundle", "create", bundlePath, bases[0] + ".." + tip}
	}
	if _, err := gitOutput(ctx, root, args...); err != nil {
		return nil, fmt.Errorf("git: bundle unpushed commits: %w", err)
	}
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSyncBundleBytes {
		return nil, fmt.Errorf("unpushed commits in %s are %d MiB (limit %d MiB); push the branch to %s first",
			root, len(data)>>20, MaxSyncBundleBytes>>20, remote)
	}
	return data, nil
}

func isAncestor(ctx context.Context, dir, ancestor, descendant string) bool {
	cmd := Command(ctx, dir, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	return cmd.Run() == nil
}

// ApplySync lands an exported sandbox state in the local repository at
// root. If the local checkout is still clean on startBranch at
// BaseCommit (or already at an ancestor of HeadCommit), it updates the
// branch and working tree in place; otherwise it writes everything to
// `refs/heads/spov/<sessionName>` without touching the working tree.
//
// @decision(DL-027)
func ApplySync(ctx context.Context, root, sessionName, startBranch string, state ExportedState) (SyncResult, error) {
	if !ValidCommitSHA(state.HeadCommit) {
		return SyncResult{}, fmt.Errorf("git: invalid sandbox HEAD commit %q", state.HeadCommit)
	}
	state.HeadCommit = strings.ToLower(state.HeadCommit)
	state.BaseCommit = strings.ToLower(state.BaseCommit)
	if len(state.Bundle)+len(state.WorktreeTar) > MaxSyncExportBytes {
		return SyncResult{}, fmt.Errorf("git: exported sandbox state exceeds %d MiB limit", MaxSyncExportBytes>>20)
	}
	for _, d := range state.Deleted {
		if _, err := cleanRepoRelPath(d); err != nil {
			return SyncResult{}, fmt.Errorf("git: deleted path %q: %w", d, err)
		}
	}

	if state.HeadCommit == state.BaseCommit && len(state.Bundle) == 0 && len(state.WorktreeTar) == 0 && len(state.Deleted) == 0 {
		return SyncResult{NoChanges: true, Branch: startBranch, HeadCommit: state.HeadCommit}, nil
	}

	// Bring any new commits from the bundle into the local object store
	// first, without touching the working tree or branch refs.
	if len(state.Bundle) > 0 {
		if err := unbundleCommits(ctx, root, state.Bundle, state.HeadCommit); err != nil {
			return SyncResult{}, err
		}
	}
	if !commitExists(ctx, root, state.HeadCommit) {
		return SyncResult{}, fmt.Errorf("git: commit %s is not present in %s after unbundling", state.HeadCommit[:12], root)
	}

	// Measure what the sync brings in before the fast-forward or the
	// fallback landing materialises it, and refuse an expansion that
	// would fill the disk. No ref is written yet, so refusing here
	// leaves the checkout exactly as it was.
	//
	// @decision(DL-126)
	cur, _ := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	if err := boundIncomingObjects(ctx, root, strings.ToLower(cur), state.HeadCommit); err != nil {
		return SyncResult{}, err
	}

	packed, err := archivePaths(state.WorktreeTar)
	if err != nil {
		return SyncResult{}, err
	}
	cleanDeleted := make([]string, 0, len(state.Deleted))
	for _, d := range state.Deleted {
		clean, _ := cleanRepoRelPath(d)
		cleanDeleted = append(cleanDeleted, clean)
	}
	if err := refuseInsideSubmodule(ctx, root, state.HeadCommit, packed, cleanDeleted); err != nil {
		return SyncResult{}, err
	}

	commits := countCommitsBetween(ctx, root, state.BaseCommit, state.HeadCommit)
	inPlace, reason := canApplyInPlace(ctx, root, startBranch, state.BaseCommit, state.HeadCommit)
	// A checkout whose own config would make an in-place landing write
	// somewhere other than where the veto looked, or hide what it must
	// see, goes to the fallback with a clear reason, whatever
	// canApplyInPlace concluded from the relocated tree.
	//
	// @decision(DL-126)
	if u := unpredictableWorkTreeVeto(ctx, root); u != "" {
		inPlace, reason = false, u
	}
	var curHead string
	if inPlace {
		curHead, _ = gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
		curHead = strings.ToLower(curHead)
		// Checked while the checkout is still the user's own: the
		// ignore rules, the hooks directory and what is on disk are
		// read before anything below can change them.
		//
		// @decision(DL-126)
		veto, err := inPlaceVeto(ctx, root, curHead, state.HeadCommit, packed, cleanDeleted)
		if err != nil {
			return SyncResult{}, err
		}
		if veto != "" {
			inPlace, reason = false, veto
		}
	}
	if inPlace && curHead != state.HeadCommit {
		refused, err := fastForward(ctx, root, curHead, state.HeadCommit)
		if err != nil {
			return SyncResult{}, fmt.Errorf("git: advance %s to %s: %w", startBranch, state.HeadCommit[:12], err)
		}
		if refused != "" {
			inPlace, reason = false, refused
		}
	}
	if inPlace {
		before := trackedAt(ctx, root, curHead)
		changed, err := applyWorktreeInPlace(root, state.WorktreeTar, state.Deleted)
		if err != nil {
			return SyncResult{}, err
		}
		from := state.BaseCommit
		if !commitExists(ctx, root, from) {
			from = curHead
		}
		return SyncResult{
			InPlace:      true,
			Branch:       startBranch,
			HeadCommit:   state.HeadCommit,
			Commits:      commits,
			FilesChanged: changed,
			RangeFrom:    shortRange(commits, from),
			RangeTo:      shortRange(commits, state.HeadCommit),
			Changes:      inPlaceChanges(ctx, root, curHead, state.HeadCommit, before, state.WorktreeTar, state.Deleted),
		}, nil
	}

	fallbackBranch := fallbackBranchName(sessionName)
	targetCommit := state.HeadCommit
	dirtyCommitted := false
	filesChanged := 0
	if len(state.WorktreeTar) > 0 || len(state.Deleted) > 0 {
		var err error
		targetCommit, filesChanged, err = commitWorktreeOnFallback(ctx, root, sessionName, state.HeadCommit, state.WorktreeTar, state.Deleted)
		if err != nil {
			return SyncResult{}, err
		}
		dirtyCommitted = filesChanged > 0
	}
	if _, err := gitOutput(ctx, root, "update-ref", "refs/heads/"+fallbackBranch, targetCommit); err != nil {
		return SyncResult{}, fmt.Errorf("git: write branch %s: %w", fallbackBranch, err)
	}
	return SyncResult{
		InPlace:        false,
		Branch:         startBranch,
		FallbackBranch: fallbackBranch,
		Reason:         reason,
		HeadCommit:     targetCommit,
		Commits:        commits,
		FilesChanged:   filesChanged,
		DirtyCommitted: dirtyCommitted,
		RangeFrom:      shortRange(commits, state.BaseCommit),
		RangeTo:        shortRange(commits, state.HeadCommit),
		Changes:        commitChanges(ctx, root, state.BaseCommit, targetCommit),
	}, nil
}

func fallbackBranchName(sessionName string) string {
	name := strings.TrimSpace(sessionName)
	if name == "" || ValidBranchName("spov/"+name) != nil {
		name = "sync"
	}
	return "spov/" + name
}

func unbundleCommits(ctx context.Context, root string, bundle []byte, wantCommit string) error {
	tmp, err := os.CreateTemp("", "spov-recv-*.bundle")
	if err != nil {
		return err
	}
	bundlePath := tmp.Name()
	if _, err := tmp.Write(bundle); err != nil {
		_ = tmp.Close()
		_ = os.Remove(bundlePath)
		return err
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(bundlePath) }()

	tempRef := "refs/spov-sync/head"
	defer func() { _, _ = gitOutput(ctx, root, "update-ref", "-d", tempRef) }()

	// fsck the incoming objects: a malformed or malicious pack is
	// refused as it is received, not after it is in the store.
	//
	// @decision(DL-126)
	if _, err := gitOutput(ctx, root, "-c", "fetch.fsckObjects=true", "fetch", "--no-tags", "--", bundlePath, "HEAD:"+tempRef); err != nil {
		// Fall back to unbundling every ref in the bundle.
		if _, uerr := gitOutput(ctx, root, "-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true", "bundle", "unbundle", bundlePath); uerr != nil {
			return fmt.Errorf("git: unbundle sandbox commits: %w", err)
		}
	}
	if !commitExists(ctx, root, wantCommit) {
		return fmt.Errorf("git: bundle did not contain expected commit %s", wantCommit[:12])
	}
	return nil
}

func commitExists(ctx context.Context, dir, sha string) bool {
	out, err := gitOutput(ctx, dir, "cat-file", "-t", sha)
	return err == nil && out == "commit"
}

func countCommitsBetween(ctx context.Context, dir, base, head string) int {
	if base == "" || base == head || !commitExists(ctx, dir, base) {
		return 0
	}
	out, err := gitOutput(ctx, dir, "rev-list", "--count", base+".."+head)
	if err != nil {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(out, "%d", &n)
	return n
}

func canApplyInPlace(ctx context.Context, root, startBranch, baseCommit, headCommit string) (bool, string) {
	status, err := gitOutputRaw(ctx, root, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, "could not read local git status"
	}
	if strings.TrimSpace(status) != "" {
		return false, "local checkout has uncommitted changes"
	}
	curBranch, err := gitOutput(ctx, root, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil || curBranch == "" {
		return false, "local checkout is in detached HEAD state"
	}
	if startBranch != "" && curBranch != startBranch {
		return false, fmt.Sprintf("local checkout is on branch %s (session started on %s)", curBranch, startBranch)
	}
	curHead, err := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return false, "could not read local HEAD"
	}
	curHead = strings.ToLower(curHead)
	// In place only onto a descendant: a sandbox whose history does not
	// hold the local head (rewritten, or a resume that could not reach
	// its base) would otherwise drop the local commits in the reset.
	//
	// @decision(DL-098)
	if curHead != headCommit && !isAncestor(ctx, root, curHead, headCommit) {
		return false, fmt.Sprintf("local branch %s moved to %s", curBranch, curHead[:min(12, len(curHead))])
	}
	return true, ""
}

// SingleRepoDir is the workspace directory name a single-repository
// session clones into, accounting for ReservedDirs.
//
// @decision(DL-041) @decision(DL-027)
func SingleRepoDir(owner, name string) string {
	if slices.Contains(ReservedDirs, name) {
		return strings.ReplaceAll(owner, "/", "-") + "-" + name
	}
	return name
}

// applyWorktreeInPlace unpacks worktreeTar and removes deleted files
// inside root, validating every path so nothing can escape root, follow
// a symlinked parent directory, or write into .git.
//
// @decision(DL-027)
func applyWorktreeInPlace(root string, worktreeTar []byte, deleted []string) (int, error) {
	changed := map[string]bool{}
	for _, rel := range deleted {
		clean, err := cleanRepoRelPath(rel)
		if err != nil {
			return 0, err
		}
		if err := ensureCanonicalRelParent(root, clean, false); err != nil {
			return 0, err
		}
		target := filepath.Join(root, filepath.FromSlash(clean))
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("remove %s: %w", clean, err)
		}
		changed[clean] = true
	}
	if len(worktreeTar) > 0 {
		if _, err := unpackWorktreeTar(root, worktreeTar, changed); err != nil {
			return 0, err
		}
	}
	return len(changed), nil
}

func unpackWorktreeTar(root string, archive []byte, changed map[string]bool) (int, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return 0, fmt.Errorf("git: read worktree archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(io.LimitReader(gz, MaxSyncExportBytes+(4<<20)))
	var totalBytes int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("git: unpack worktree archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader || hdr.Typeflag == tar.TypeDir {
			continue
		}
		clean, err := cleanRepoRelPath(hdr.Name)
		if err != nil {
			return 0, fmt.Errorf("git: archive entry %q: %w", hdr.Name, err)
		}
		if err := ensureCanonicalRelParent(root, clean, true); err != nil {
			return 0, err
		}
		target := filepath.Join(root, filepath.FromSlash(clean))
		switch hdr.Typeflag {
		case tar.TypeReg, '\x00':
			totalBytes += hdr.Size
			if totalBytes > MaxSyncExportBytes {
				return 0, fmt.Errorf("git: unpacked worktree exceeds %d MiB limit", MaxSyncExportBytes>>20)
			}
			_ = os.Remove(target)
			mode := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return 0, err
			}
			_, copyErr := io.Copy(f, io.LimitReader(tr, MaxSyncExportBytes+1))
			closeErr := f.Close()
			if copyErr != nil {
				return 0, copyErr
			}
			if closeErr != nil {
				return 0, closeErr
			}
			changed[clean] = true
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return 0, fmt.Errorf("git: symlink %q points to absolute path %q", clean, hdr.Linkname)
			}
			resolved := path.Clean(path.Join(path.Dir(clean), filepath.ToSlash(hdr.Linkname)))
			if _, err := cleanRepoRelPath(resolved); err != nil {
				return 0, fmt.Errorf("git: symlink %q escapes the repository (%q)", clean, hdr.Linkname)
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return 0, err
			}
			changed[clean] = true
		default:
			return 0, fmt.Errorf("git: unsupported tar entry type %d for %q", hdr.Typeflag, clean)
		}
	}
	return len(changed), nil
}

// commitWorktreeOnFallback builds a synthetic commit on top of
// headCommit containing the sandbox's uncommitted working-tree changes,
// using a temporary working directory and GIT_INDEX_FILE so the
// developer's local index and working tree are untouched.
//
// @decision(DL-027)
func commitWorktreeOnFallback(ctx context.Context, root, sessionName, headCommit string, worktreeTar []byte, deleted []string) (string, int, error) {
	tmpDir, err := os.MkdirTemp("", "spov-sync-tree-*")
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	indexFile := filepath.Join(tmpDir, "index")
	workDir := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return "", 0, err
	}
	gitDir, err := gitOutput(ctx, root, "rev-parse", "--git-dir")
	if err != nil {
		return "", 0, err
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}

	env := append(os.Environ(), "GIT_INDEX_FILE="+indexFile, "GIT_DIR="+gitDir)
	if _, err := gitOutputEnv(ctx, root, env, "read-tree", headCommit); err != nil {
		return "", 0, fmt.Errorf("git: read-tree %s: %w", headCommit[:12], err)
	}

	changed := map[string]bool{}
	for _, rel := range deleted {
		clean, err := cleanRepoRelPath(rel)
		if err != nil {
			return "", 0, err
		}
		if _, err := gitOutputEnv(ctx, root, env, "update-index", "--force-remove", "--", clean); err != nil {
			return "", 0, fmt.Errorf("git: remove %s from fallback index: %w", clean, err)
		}
		changed[clean] = true
	}

	if len(worktreeTar) > 0 {
		unpacked := map[string]bool{}
		if _, err := unpackWorktreeTar(workDir, worktreeTar, unpacked); err != nil {
			return "", 0, err
		}
		var paths []string
		for p := range unpacked {
			paths = append(paths, p)
			changed[p] = true
		}
		slices.Sort(paths)
		workEnv := append(slices.Clone(env), "GIT_WORK_TREE="+workDir)
		for _, p := range paths {
			full := filepath.Join(workDir, filepath.FromSlash(p))
			info, err := os.Lstat(full)
			if err != nil {
				continue
			}
			// If update-index --add lacks --work-tree or object writing fails,
			// hash-object -w writes the blob directly into root's object store.
			if info.Mode()&os.ModeSymlink != 0 {
				if _, err := gitOutputEnv(ctx, workDir, workEnv, "update-index", "--add", "--", p); err != nil {
					return "", 0, fmt.Errorf("git: stage %s in fallback index: %w", p, err)
				}
				continue
			}
			oid, err := gitOutputEnv(ctx, root, env, "hash-object", "-w", "--", full)
			if err != nil {
				return "", 0, fmt.Errorf("git: hash %s for fallback index: %w", p, err)
			}
			oid = strings.TrimSpace(oid)
			mode := "100644"
			if info.Mode()&0o111 != 0 {
				mode = "100755"
			}
			if _, err := gitOutputEnv(ctx, root, env, "update-index", "--add", "--cacheinfo", mode+","+oid+","+p); err != nil {
				return "", 0, fmt.Errorf("git: stage %s in fallback index: %w", p, err)
			}
		}
	}
	if len(changed) == 0 {
		return headCommit, 0, nil
	}

	tree, err := gitOutputEnv(ctx, root, env, "write-tree")
	if err != nil {
		return "", 0, fmt.Errorf("git: write fallback tree: %w", err)
	}
	tree = strings.TrimSpace(tree)
	msg := fmt.Sprintf("spov: uncommitted changes from %s", sessionName)
	commitEnv := append(slices.Clone(env),
		"GIT_AUTHOR_NAME=spov",
		"GIT_AUTHOR_EMAIL=spov@localhost",
		"GIT_COMMITTER_NAME=spov",
		"GIT_COMMITTER_EMAIL=spov@localhost",
	)
	commit, err := gitOutputEnv(ctx, root, commitEnv, "commit-tree", tree, "-p", headCommit, "-m", msg)
	if err != nil {
		return "", 0, fmt.Errorf("git: commit fallback tree: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(commit)), len(changed), nil
}

func cleanRepoRelPath(p string) (string, error) {
	s := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(p)), "./")
	if s == "" || s == "." {
		return "", errors.New("empty path")
	}
	if strings.HasPrefix(s, "/") || strings.ContainsAny(s, "\x00\\") {
		return "", errors.New("path must be relative to the repository root")
	}
	clean := path.Clean(s)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes the repository root")
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." || namesGitDir(seg) {
			return "", fmt.Errorf("disallowed path segment %q", seg)
		}
	}
	return clean, nil
}

// ntfsShortGit is the 8.3 name NTFS gives `.git`.
var ntfsShortGit = regexp.MustCompile(`^(?i)git~[0-9]+$`)

// namesGitDir reports whether a path segment is `.git` on some
// filesystem a checkout may be on: in any case, with the trailing dots
// and spaces or the stream suffix NTFS drops, as NTFS's short name, or
// with the code points HFS+ ignores. git refuses the same names
// (core.protectNTFS, core.protectHFS).
//
// @decision(DL-126)
func namesGitDir(seg string) bool {
	seg = strings.TrimRight(cutStream(stripIgnorable(seg)), ". ")
	return strings.EqualFold(seg, ".git") || ntfsShortGit.MatchString(seg)
}

// stripIgnorable drops the code points HFS+ ignores in a name.
func stripIgnorable(seg string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
			return -1
		}
		return r
	}, seg)
}

// cutStream drops an NTFS alternate stream suffix (`name:stream`).
func cutStream(seg string) string {
	if i := strings.IndexByte(seg, ':'); i >= 0 {
		return seg[:i]
	}
	return seg
}

// archivePaths lists the cleaned paths a worktree archive would write.
func archivePaths(archive []byte) ([]string, error) {
	if len(archive) == 0 {
		return nil, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("git: read worktree archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(io.LimitReader(gz, MaxSyncExportBytes+(4<<20)))
	var paths []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return paths, nil
		}
		if err != nil {
			return nil, fmt.Errorf("git: unpack worktree archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader || hdr.Typeflag == tar.TypeDir {
			continue
		}
		clean, err := cleanRepoRelPath(hdr.Name)
		if err != nil {
			return nil, fmt.Errorf("git: archive entry %q: %w", hdr.Name, err)
		}
		// Refused here, before anything is written, as the unpacker
		// would refuse it after the commits had landed.
		//
		// @decision(DL-126)
		switch hdr.Typeflag {
		case tar.TypeReg, '\x00':
		case tar.TypeSymlink:
			resolved := path.Clean(path.Join(path.Dir(clean), filepath.ToSlash(hdr.Linkname)))
			if _, err := cleanRepoRelPath(resolved); err != nil || filepath.IsAbs(hdr.Linkname) {
				return nil, fmt.Errorf("git: symlink %q escapes the repository (%q)", clean, hdr.Linkname)
			}
		default:
			return nil, fmt.Errorf("git: unsupported tar entry type %d for %q", hdr.Typeflag, clean)
		}
		paths = append(paths, clean)
	}
}

// ensureCanonicalRelParent checks that no directory component along
// clean's parent path is a symlink (creating missing directories when
// mkdir is true) and that the resolved parent path matches its lexical
// path under root.
func ensureCanonicalRelParent(root, clean string, mkdir bool) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	dirRel := path.Dir(clean)
	if dirRel == "." || dirRel == "" {
		return nil
	}
	cur := realRoot
	for _, seg := range strings.Split(dirRel, "/") {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if !mkdir {
				return nil
			}
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("git: parent component %q of %q is not a regular directory", seg, clean)
		}
	}
	realParent, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return err
	}
	wantParent := filepath.Join(realRoot, filepath.FromSlash(dirRel))
	if realParent != wantParent {
		return fmt.Errorf("git: parent of %q resolves to %q, want %q", clean, realParent, wantParent)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitOutputRaw(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// InspectCommandOutput runs `git args...` in dir and returns its
// trimmed stdout.
//
// @decision(DL-027)
func InspectCommandOutput(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutput(ctx, dir, args...)
}

func gitOutputRaw(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutputEnv(ctx, dir, nil, args...)
}

func gitOutputEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return outputOf(Command(ctx, dir, env, args...))
}

// outputOf runs cmd and returns its stdout, or its stderr as the error.
func outputOf(cmd *exec.Cmd) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", RedactAll(msg))
	}
	return string(out), nil
}

// shortRange is a commit's short name, or empty when no commits were
// brought back.
func shortRange(commits int, sha string) string {
	if commits <= 0 || sha == "" {
		return ""
	}
	return sha[:min(12, len(sha))]
}

// trackedAt is the set of paths commit tracks; nil when it cannot be
// read.
//
// @decision(DL-136)
func trackedAt(ctx context.Context, root, commit string) map[string]bool {
	listing, err := gitOutputRaw(ctx, root, "ls-tree", "-r", "-z", "--name-only", commit)
	if err != nil {
		return nil
	}
	tracked := map[string]bool{}
	for _, p := range strings.Split(listing, "\x00") {
		if p != "" {
			tracked[p] = true
		}
	}
	return tracked
}

// commitChanges is `git diff --name-status` between two commits, as
// path changes. Renames are reported as a delete and an add. An
// unreadable range lists nothing.
//
// @decision(DL-136)
func commitChanges(ctx context.Context, root, from, to string) []PathChange {
	if from == "" || !commitExists(ctx, root, from) {
		return nil
	}
	out, err := gitOutputRaw(ctx, root, "diff", "--name-status", "-z", "--no-renames", from, to)
	if err != nil {
		return nil
	}
	fields := strings.Split(out, "\x00")
	var changes []PathChange
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "" {
			break
		}
		switch st := fields[i][0]; st {
		case 'A', 'M', 'D':
			changes = append(changes, PathChange{Status: st, Path: fields[i+1]})
		default:
			changes = append(changes, PathChange{Status: 'M', Path: fields[i+1]})
		}
	}
	return changes
}

// inPlaceChanges lists what an in-place apply changed: the committed
// difference from the checkout's old head, then the sandbox's
// uncommitted files and deletions on top. A path the old head did not
// track and no commit added is an added file.
//
// @decision(DL-136)
func inPlaceChanges(ctx context.Context, root, oldHead, newHead string, before map[string]bool, worktreeTar []byte, deleted []string) []PathChange {
	byPath := map[string]byte{}
	if oldHead != newHead {
		for _, c := range commitChanges(ctx, root, oldHead, newHead) {
			byPath[c.Path] = c.Status
		}
	}
	packed, _ := archivePaths(worktreeTar)
	for _, p := range packed {
		switch st, seen := byPath[p]; {
		case seen && st == 'D':
			byPath[p] = 'M'
		case seen:
		case before[p]:
			byPath[p] = 'M'
		default:
			byPath[p] = 'A'
		}
	}
	for _, d := range deleted {
		if clean, err := cleanRepoRelPath(d); err == nil {
			byPath[clean] = 'D'
		}
	}
	changes := make([]PathChange, 0, len(byPath))
	for p, st := range byPath {
		changes = append(changes, PathChange{Status: st, Path: p})
	}
	slices.SortFunc(changes, func(a, b PathChange) int { return strings.Compare(a.Path, b.Path) })
	return changes
}
