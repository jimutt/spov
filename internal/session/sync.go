package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

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

// maxExportOutput is how much an export may print: the state at its
// limit in base64, and room for the JSON around it and the deleted
// paths.
//
// @decision(DL-130)
const maxExportOutput = git.MaxSyncExportBytes/3*4 + 1<<20

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
	if len(bundleBytes)+len(tarBytes) > git.MaxSyncExportBytes {
		return git.ExportedState{}, fmt.Errorf("session: exported sandbox state exceeds %d MiB limit", git.MaxSyncExportBytes>>20)
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
	// Bytes is the size ExportSync's bundle and archive would have.
	Bytes int64
	// Rewritten says the base is no longer an ancestor of HEAD.
	Rewritten bool
}

// InspectSync measures a sandbox repository's state as ExportSync would
// package it, in one exec, building the bundle and archive only to take
// their sizes.
//
// @decision(DL-101)
func InspectSync(ctx context.Context, h sandbox.Handle, dir, baseCommit string) (SyncSummary, error) {
	base := strings.ToLower(strings.TrimSpace(baseCommit))
	if base != "" && !git.ValidCommitSHA(base) {
		return SyncSummary{}, fmt.Errorf("session: invalid base commit %q", baseCommit)
	}
	res, err := h.Exec(ctx, []string{"bash", "-c", syncStateScript, "spov-inspect",
		path.Join(opencode.WorkspaceRoot, dir), base, "inspect"})
	if err != nil {
		return SyncSummary{}, fmt.Errorf("session: inspect sandbox repository state: %w", err)
	}
	if res.ExitCode != 0 {
		return SyncSummary{}, fmt.Errorf("session: inspect sandbox repository state: %s", execDetail(res))
	}
	var wire struct {
		Branch     string `json:"branch"`
		HeadCommit string `json:"head_commit"`
		Commits    int    `json:"commits"`
		Files      int    `json:"files"`
		Bytes      int64  `json:"bytes"`
		Rewritten  bool   `json:"rewritten"`
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

// syncStateScript reads the repository at $1 against the base $2. In
// the mode "export" ($3) it prints the state ExportSync decodes and
// fails over the bound; in "inspect" it prints only the counts and the
// sizes, whatever they are.
//
// @decision(DL-027) @decision(DL-101)
const syncStateScript = `set -e
cd "$1"
python3 - "$2" "$3" <<'PY'
import base64, json, os, subprocess, sys, tempfile

MAX_BYTES = 32 * 1024 * 1024
base = sys.argv[1].strip().lower()
inspect = sys.argv[2] == "inspect"

def run(*args, check=True):
    r = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if check and r.returncode != 0:
        msg = r.stderr.decode("utf-8", errors="replace").strip() or f"{args[0]} exited with {r.returncode}"
        sys.stderr.write(msg + "\n")
        sys.exit(r.returncode)
    return r

head = run("git", "rev-parse", "--verify", "HEAD").stdout.decode().strip().lower()
branch_res = run("git", "symbolic-ref", "-q", "--short", "HEAD", check=False)
branch = branch_res.stdout.decode().strip() if branch_res.returncode == 0 else ""

given_base = base
if not base:
    origin_head = run("git", "rev-parse", "--verify", "-q", "refs/remotes/origin/HEAD", check=False)
    if origin_head.returncode == 0:
        cand = origin_head.stdout.decode().strip().lower()
        if cand and run("git", "merge-base", "--is-ancestor", cand, head, check=False).returncode == 0:
            base = cand

bundle_b64 = ""
total_bytes = 0
if head and head != base:
    with tempfile.NamedTemporaryFile(suffix=".bundle", delete=False) as tf:
        bpath = tf.name
    try:
        # From the base when the sandbox's history holds it, else from the
        # remote's default branch, which the local checkout has too.
        range_base = ""
        if base and run("git", "merge-base", "--is-ancestor", base, head, check=False).returncode == 0:
            range_base = base
        else:
            oh = run("git", "rev-parse", "--verify", "-q", "refs/remotes/origin/HEAD", check=False)
            cand = oh.stdout.decode().strip().lower() if oh.returncode == 0 else ""
            if cand and run("git", "merge-base", "--is-ancestor", cand, head, check=False).returncode == 0:
                range_base = cand
        rev_arg = f"{range_base}..HEAD" if range_base else "HEAD"
        run("git", "bundle", "create", bpath, rev_arg)
        bsize = os.path.getsize(bpath)
        total_bytes += bsize
        if total_bytes > MAX_BYTES and not inspect:
            sys.stderr.write(f"exported sandbox state exceeds {MAX_BYTES // (1024 * 1024)} MiB limit\n")
            sys.exit(1)
        if not inspect:
            with open(bpath, "rb") as f:
                bundle_b64 = base64.b64encode(f.read()).decode("ascii")
    finally:
        if os.path.exists(bpath):
            os.remove(bpath)

status = run("git", "status", "--porcelain=v1", "-z", "--untracked-files=all").stdout
entries = status.split(b"\x00")
to_pack = []
deleted = []
seen_pack = set()
seen_del = set()
i = 0
while i < len(entries):
    item = entries[i]
    i += 1
    if not item or len(item) < 4:
        continue
    xy = item[:2].decode("latin1", errors="replace")
    path_bytes = item[3:]
    if "R" in xy or "C" in xy:
        if i < len(entries):
            orig = entries[i].decode("utf-8", errors="surrogateescape")
            i += 1
            if "R" in xy and orig and not os.path.lexists(orig) and orig not in seen_del:
                seen_del.add(orig)
                deleted.append(orig)
    rel = path_bytes.decode("utf-8", errors="surrogateescape")
    if not rel:
        continue
    if os.path.lexists(rel):
        if os.path.isdir(rel) and not os.path.islink(rel):
            continue
        if rel not in seen_pack:
            seen_pack.add(rel)
            to_pack.append(rel)
    else:
        if rel not in seen_del:
            seen_del.add(rel)
            deleted.append(rel)

deleted = [p for p in deleted if p not in seen_pack]

tar_b64 = ""
if to_pack:
    with tempfile.NamedTemporaryFile(suffix=".list", delete=False) as lf, \
         tempfile.NamedTemporaryFile(suffix=".tar.gz", delete=False) as tf:
        lpath, tpath = lf.name, tf.name
        for p in to_pack:
            lf.write(p.encode("utf-8") + b"\x00")
    try:
        run("tar", "-czf", tpath, "--null", "--no-recursion", "-T", lpath)
        tsize = os.path.getsize(tpath)
        total_bytes += tsize
        if total_bytes > MAX_BYTES and not inspect:
            sys.stderr.write(f"exported sandbox state exceeds {MAX_BYTES // (1024 * 1024)} MiB limit\n")
            sys.exit(1)
        if not inspect:
            with open(tpath, "rb") as f:
                tar_b64 = base64.b64encode(f.read()).decode("ascii")
    finally:
        for p in (lpath, tpath):
            if os.path.exists(p):
                os.remove(p)

if inspect:
    commits, rewritten = 0, False
    if given_base and head != given_base:
        if run("git", "merge-base", "--is-ancestor", given_base, head, check=False).returncode == 0:
            commits = int(run("git", "rev-list", "--count", f"{given_base}..HEAD").stdout.decode().strip() or "0")
        else:
            rewritten = True
    sys.stdout.write(json.dumps({
        "branch": branch,
        "head_commit": head,
        "commits": commits,
        "files": len(to_pack) + len(deleted),
        "bytes": total_bytes,
        "rewritten": rewritten,
    }))
    sys.exit(0)

out = {
    "branch": branch,
    "base_commit": base,
    "head_commit": head,
    "bundle": bundle_b64,
    "worktree_tar": tar_b64,
    "deleted": deleted,
}
sys.stdout.write(json.dumps(out))
PY
`

// shortCommit is a commit as a person reads it.
func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
