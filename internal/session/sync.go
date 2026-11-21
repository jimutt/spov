package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
)

// ErrSeedBaseMissing is a seed allowed to miss its base that missed it:
// the clone stays on its default branch.
//
// @decision(DL-098)
var ErrSeedBaseMissing = errors.New("session: the sync base commit is not reachable")

// seedBaseMissingExit is the seed script's exit for ErrSeedBaseMissing.
const seedBaseMissingExit = 95

// applySyncSeed checks out seed.Branch at seed.BaseCommit inside the
// sandbox's cloned repository, fetching or unbundling commits as needed.
// Any bundle bytes travel on stdin rather than argv.
//
// @decision(DL-027)
func applySyncSeed(ctx context.Context, h sandbox.Handle, dir string, seed SyncSeed) error {
	if err := git.ValidBranchName(seed.Branch); err != nil {
		return fmt.Errorf("session: sync seed: %w", err)
	}
	commit := strings.ToLower(strings.TrimSpace(seed.BaseCommit))
	if !git.ValidCommitSHA(commit) {
		return fmt.Errorf("session: sync seed: invalid base commit %q", seed.BaseCommit)
	}
	repoPath := path.Join(opencode.WorkspaceRoot, dir)
	script := `set -e
repo_path="$1"
branch="$2"
base="$3"
has_bundle="$4"
allow_missing="$5"
if [ "$has_bundle" = "1" ]; then
  cat > /tmp/spov-seed.bundle
  trap 'rm -f /tmp/spov-seed.bundle' EXIT
fi
if [ ! -d "$repo_path/.git" ]; then
  if [ "$has_bundle" != "1" ]; then
    echo "spov: cannot initialize $repo_path without a remote clone or sync bundle" >&2
    exit 1
  fi
  mkdir -p "$(dirname "$repo_path")"
  git clone --no-checkout /tmp/spov-seed.bundle "$repo_path"
  cd "$repo_path"
  git remote remove origin >/dev/null 2>&1 || true
else
  cd "$repo_path"
fi
if ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
  git fetch --no-tags origin "+refs/heads/${branch}:refs/remotes/origin/${branch}" >/dev/null 2>&1 || true
fi
if [ "$has_bundle" = "1" ] && ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
  for prereq in $(awk '/^$/ { exit } /^-[0-9a-f]{40}/ { print substr($1, 2) }' /tmp/spov-seed.bundle); do
    if ! git cat-file -e "${prereq}^{commit}" 2>/dev/null; then
      git fetch --no-tags origin "$prereq" >/dev/null 2>&1 || true
    fi
  done
  if ! git bundle unbundle /tmp/spov-seed.bundle >/dev/null 2>&1 && ! git fetch --no-tags /tmp/spov-seed.bundle "+refs/*:refs/spov-seed/*" >/dev/null 2>&1; then
    git fetch --unshallow --no-tags origin "+refs/heads/*:refs/remotes/origin/*" >/dev/null 2>&1 || \
      git fetch --depth=2147483647 --no-tags origin "+refs/heads/*:refs/remotes/origin/*" >/dev/null 2>&1 || true
    git bundle unbundle /tmp/spov-seed.bundle >/dev/null 2>&1 || git fetch --no-tags /tmp/spov-seed.bundle "+refs/*:refs/spov-seed/*"
  fi
fi
if ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
  git fetch --no-tags origin "$base" >/dev/null 2>&1 || \
    git fetch --unshallow --no-tags origin "+refs/heads/*:refs/remotes/origin/*" >/dev/null 2>&1 || \
    git fetch --depth=2147483647 --no-tags origin "+refs/heads/*:refs/remotes/origin/*" >/dev/null 2>&1 || true
fi
if [ "$allow_missing" = "1" ] && ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
  exit 95
fi
git checkout -B "$branch" "$base"
`
	hasBundle := "0"
	if len(seed.Bundle) > 0 {
		hasBundle = "1"
	}
	allowMissing := "0"
	if seed.AllowMissingBase {
		allowMissing = "1"
	}
	ctx, cancel := context.WithTimeout(ctx, SeedTimeout)
	defer cancel()
	res, err := h.ExecInput(ctx, []string{"bash", "-c", script, "spov-seed", repoPath, seed.Branch, commit, hasBundle, allowMissing}, seed.Bundle)
	if err != nil {
		return fmt.Errorf("session: seed local branch %s: %w", seed.Branch, err)
	}
	// @decision(DL-098)
	if res.ExitCode == seedBaseMissingExit && seed.AllowMissingBase {
		return ErrSeedBaseMissing
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		if detail == "" {
			detail = fmt.Sprintf("exit %d", res.ExitCode)
		}
		return fmt.Errorf("session: seed local branch %s at %s: %s", seed.Branch, commit[:12], git.RedactAll(detail))
	}
	return nil
}

// The deadlines of the commands a sync runs in the sandbox, which a
// program that never exits would otherwise hold for ever: seeding a
// clone, packaging its state, and measuring it.
//
// @decision(DL-135)
const (
	SeedTimeout    = 10 * time.Minute
	ExportTimeout  = 10 * time.Minute
	InspectTimeout = 5 * time.Minute
)

// ArchiveTimeout bounds a streamed sync-back from the first measure to
// the last byte.
//
// @decision(DL-176)
const ArchiveTimeout = 45 * time.Minute

// maxArchiveOutput is what a streamed sync-back may write: the packed
// parts at their bound, the metadata at its, and the tar's framing and
// keep-alive entries.
//
// @decision(DL-176) @decision(DL-177)
const maxArchiveOutput = int64(git.MaxSyncPackedBytes) + git.MaxSyncMetaBytes + 1<<20

// StreamSync writes the repository's state as DL-176's archive to w as
// the sandbox produces it. A refusal (over the bounds, no repository)
// ends the exec with a non-zero exit before anything is written; the
// caller tells the two apart by whether w saw a byte.
//
// @decision(DL-176)
func StreamSync(ctx context.Context, h sandbox.Handle, dir, baseCommit string, w io.Writer) (sandbox.ExecResult, error) {
	base := strings.ToLower(strings.TrimSpace(baseCommit))
	if base != "" && !git.ValidCommitSHA(base) {
		return sandbox.ExecResult{}, fmt.Errorf("session: invalid base commit %q", baseCommit)
	}
	ctx, cancel := context.WithTimeout(ctx, ArchiveTimeout)
	defer cancel()
	secs := int(ArchiveTimeout / time.Second)
	if dl, ok := ctx.Deadline(); ok {
		secs = max(int(time.Until(dl)/time.Second)-5, 1)
	}
	stop := keepAwake(ctx, h)
	res, err := h.ExecStream(ctx, []string{"bash", "-c", syncStateScript, "spov-archive",
		path.Join(opencode.WorkspaceRoot, dir), base, "archive", strconv.Itoa(secs)}, w, maxArchiveOutput)
	stop()
	if err != nil {
		return sandbox.ExecResult{}, fmt.Errorf("session: stream sandbox repository state: %w", err)
	}
	return res, nil
}

// keepAwakeEvery is how often a streaming export runs a short exec,
// which resets the sandbox's idle clock (S-R1), while it streams.
//
// @decision(DL-175)
var keepAwakeEvery = 30 * time.Second

// keepAwake runs `true` in the sandbox every keepAwakeEvery until the
// returned func is called.
//
// @decision(DL-175)
func keepAwake(ctx context.Context, h sandbox.Handle) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(keepAwakeEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				execCtx, execCancel := context.WithTimeout(ctx, 10*time.Second)
				_, _ = h.Exec(execCtx, []string{"true"})
				execCancel()
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// ExecFailure is a failed exec's message as ExportSync words it.
//
// @decision(DL-176)
func ExecFailure(res sandbox.ExecResult) error {
	return fmt.Errorf("session: export sandbox repository state: %s", execDetail(res))
}

// maxExportOutput is how much an export may print: the state at its
// limit in base64, and room for the JSON around it and the deleted
// paths.
//
// @decision(DL-130)
const maxExportOutput = git.MaxSyncJSONExportBytes/3*4 + 1<<20

// ExportSync packages the current git commits and uncommitted
// working-tree changes from a running sandbox's repository at
// `/workspace/<dir>`.
//
// @decision(DL-027)
func ExportSync(ctx context.Context, h sandbox.Handle, dir, baseCommit string) (git.ExportedState, error) {
	base := strings.ToLower(strings.TrimSpace(baseCommit))
	if base != "" && !git.ValidCommitSHA(base) {
		return git.ExportedState{}, fmt.Errorf("session: invalid base commit %q", baseCommit)
	}
	repoPath := path.Join(opencode.WorkspaceRoot, dir)
	ctx, cancel := context.WithTimeout(ctx, ExportTimeout)
	defer cancel()
	res, err := h.ExecBounded(ctx, []string{"bash", "-c", syncStateScript, "spov-export", repoPath, base, "export"}, nil, maxExportOutput)
	if err != nil {
		return git.ExportedState{}, fmt.Errorf("session: export sandbox repository state: %w", err)
	}
	if res.ExitCode != 0 {
		return git.ExportedState{}, fmt.Errorf("session: export sandbox repository state: %s", execDetail(res))
	}
	var wire struct {
		Branch      string   `json:"branch"`
		BaseCommit  string   `json:"base_commit"`
		HeadCommit  string   `json:"head_commit"`
		Bundle      string   `json:"bundle"`
		WorktreeTar string   `json:"worktree_tar"`
		Deleted     []string `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &wire); err != nil {
		return git.ExportedState{}, fmt.Errorf("session: decode exported sandbox state: %w", err)
	}
	var bundleBytes, tarBytes []byte
	if wire.Bundle != "" {
		bundleBytes, err = base64.StdEncoding.DecodeString(wire.Bundle)
		if err != nil {
			return git.ExportedState{}, fmt.Errorf("session: decode exported bundle: %w", err)
		}
	}
	if wire.WorktreeTar != "" {
		tarBytes, err = base64.StdEncoding.DecodeString(wire.WorktreeTar)
		if err != nil {
			return git.ExportedState{}, fmt.Errorf("session: decode exported worktree archive: %w", err)
		}
	}
	if len(bundleBytes)+len(tarBytes) > git.MaxSyncJSONExportBytes {
		return git.ExportedState{}, fmt.Errorf("session: exported sandbox state exceeds %d MiB limit", git.MaxSyncJSONExportBytes>>20)
	}
	return git.ExportedState{
		Branch:      wire.Branch,
		BaseCommit:  wire.BaseCommit,
		HeadCommit:  wire.HeadCommit,
		Bundle:      bundleBytes,
		WorktreeTar: tarBytes,
		Deleted:     wire.Deleted,
	}, nil
}

// SyncSummary is what InspectSync measures of a sandbox's repository:
// where it is, how much changed since the base, and the size its export
// would have. It carries no contents.
//
// @decision(DL-101)
type SyncSummary struct {
	Branch     string
	HeadCommit string
	// Commits counts base..HEAD, and is zero when Rewritten.
	Commits int
	// Files counts the changed, added and deleted paths.
	Files int
	// Bytes is ObjectBytes and WorktreeBytes together.
	Bytes int64
	// ObjectBytes is what the commits add, inflated; WorktreeBytes the
	// changed files' own sizes.
	ObjectBytes   int64
	WorktreeBytes int64
	// Over is why the state is over DL-177's bounds, or empty.
	Over string
	// Rewritten says the base is no longer an ancestor of HEAD.
	Rewritten bool
}

// InspectSync measures a sandbox repository's state with the landing's
// measures, in one exec, packing nothing.
//
// @decision(DL-101) @decision(DL-177)
func InspectSync(ctx context.Context, h sandbox.Handle, dir, baseCommit string) (SyncSummary, error) {
	base := strings.ToLower(strings.TrimSpace(baseCommit))
	if base != "" && !git.ValidCommitSHA(base) {
		return SyncSummary{}, fmt.Errorf("session: invalid base commit %q", baseCommit)
	}
	ctx, cancel := context.WithTimeout(ctx, InspectTimeout)
	defer cancel()
	res, err := h.Exec(ctx, []string{"bash", "-c", syncStateScript, "spov-inspect",
		path.Join(opencode.WorkspaceRoot, dir), base, "inspect"})
	if err != nil {
		return SyncSummary{}, fmt.Errorf("session: inspect sandbox repository state: %w", err)
	}
	if res.ExitCode != 0 {
		return SyncSummary{}, fmt.Errorf("session: inspect sandbox repository state: %s", execDetail(res))
	}
	var wire struct {
		Branch        string `json:"branch"`
		HeadCommit    string `json:"head_commit"`
		Commits       int    `json:"commits"`
		Files         int    `json:"files"`
		Bytes         int64  `json:"bytes"`
		ObjectBytes   int64  `json:"object_bytes"`
		WorktreeBytes int64  `json:"worktree_bytes"`
		Over          string `json:"over"`
		Rewritten     bool   `json:"rewritten"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &wire); err != nil {
		return SyncSummary{}, fmt.Errorf("session: decode inspected sandbox state: %w", err)
	}
	return SyncSummary(wire), nil
}

// execDetail is a failed exec's message, redacted.
func execDetail(res sandbox.ExecResult) string {
	detail := strings.TrimSpace(res.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(res.Stdout)
	}
	if detail == "" {
		detail = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return git.RedactAll(detail)
}

// shortCommit is a commit as a person reads it.
func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
