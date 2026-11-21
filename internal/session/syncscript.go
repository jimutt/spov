package session

import (
	"strconv"
	"strings"

	"github.com/jimutt/spov/internal/git"
)

// syncStateScript reads the repository at $1 against the base $2, in
// the mode $3:
//
//   - "export" prints the state ExportSync decodes, as JSON, and fails
//     over the JSON route's packed bound;
//   - "archive" measures, refuses over the landing's bounds with
//     nothing on stdout, and otherwise writes DL-176's tar to stdout,
//     under `timeout` set to $4 seconds;
//   - "inspect" prints the counts and the measures, whatever they are.
//
// The bounds are filled in from the git package's constants.
//
// @decision(DL-027) @decision(DL-101) @decision(DL-176) @decision(DL-177)
var syncStateScript = buildSyncScript(syncBounds{
	JSON: git.MaxSyncJSONExportBytes, Export: git.MaxSyncExportBytes, Packed: git.MaxSyncPackedBytes,
	Blob: git.MaxSyncIncomingBlobBytes, Meta: git.MaxSyncMetaBytes, Entries: git.MaxSyncEntries,
	WorkDir: archiveWorkDir,
})

// syncBounds are the limits the export script checks, and where its
// archive mode packs.
type syncBounds struct {
	JSON, Export, Packed, Blob, Meta, Entries int
	WorkDir                                   string
}

func buildSyncScript(b syncBounds) string {
	return strings.NewReplacer(
		"@JSON_MAX@", strconv.Itoa(b.JSON),
		"@EXPORT_MAX@", strconv.Itoa(b.Export),
		"@PACKED_MAX@", strconv.Itoa(b.Packed),
		"@BLOB_MAX@", strconv.Itoa(b.Blob),
		"@META_MAX@", strconv.Itoa(b.Meta),
		"@ENTRIES_MAX@", strconv.Itoa(b.Entries),
		"@WORK_DIR@", b.WorkDir,
	).Replace(syncStateScriptTemplate)
}

// archiveWorkDir is where an archive run packs; a new run stops the
// one a previous run left and clears it.
//
// @decision(DL-176)
const archiveWorkDir = "/tmp/spov-sync-archive"

const syncStateScriptTemplate = `set -e
cd "$1"
exec ${4:+env SPOV_ARCHIVE_TIMED=1 timeout -s TERM "$4"} python3 - "$2" "$3" <<'PY'
import base64, json, os, shutil, signal, subprocess, sys, tarfile, tempfile, threading, time

JSON_MAX = @JSON_MAX@
EXPORT_MAX = @EXPORT_MAX@
PACKED_MAX = @PACKED_MAX@
BLOB_MAX = @BLOB_MAX@
META_MAX = @META_MAX@
ENTRIES_MAX = @ENTRIES_MAX@
WORK_DIR = "@WORK_DIR@"
MIB = 1024 * 1024

base = sys.argv[1].strip().lower()
mode = sys.argv[2]
inspect = mode == "inspect"
archive = mode == "archive"


def run(*args, check=True, input=None):
    r = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, input=input)
    if check and r.returncode != 0:
        msg = r.stderr.decode("utf-8", errors="replace").strip() or f"{args[0]} exited with {r.returncode}"
        sys.stderr.write(msg + "\n")
        sys.exit(r.returncode)
    return r


def mib(n):
    return f"{n / MIB:.0f} MiB" if n >= MIB else f"{n} bytes"


def ours(pgid):
    """Whether pgid leads a previous archive run: timeout running python3."""
    try:
        with open(f"/proc/{pgid}/cmdline", "rb") as f:
            argv = f.read().split(b"\0")
    except OSError:
        return False
    return len(argv) > 1 and os.path.basename(argv[0]) == b"timeout" and b"python3" in argv


def gone(pgid):
    try:
        os.killpg(pgid, 0)
        return False
    except OSError:
        return True


RUN_DIR = ""
if archive:
    if not os.environ.get("SPOV_ARCHIVE_TIMED"):
        sys.stderr.write("the archive mode runs only under its timeout\n")
        sys.exit(2)
    # One archive run at a time per sandbox: stop the group a previous
    # run left (its reader went away), wait for it to go, and pack in a
    # directory of this run's own. The group is timeout's, which holds
    # this run and its git and tar.
    me = os.getpgrp()
    os.makedirs(WORK_DIR, mode=0o700, exist_ok=True)
    pid_file = os.path.join(WORK_DIR, "pid")
    try:
        with open(pid_file) as f:
            old = int(f.read().strip())
    except (OSError, ValueError):
        old = 0
    if old > 1 and old != me and ours(old):
        try:
            os.killpg(old, signal.SIGTERM)
        except OSError:
            pass
        for _ in range(50):
            if gone(old):
                break
            time.sleep(0.1)
        else:
            try:
                os.killpg(old, signal.SIGKILL)
            except OSError:
                pass
    for entry in os.listdir(WORK_DIR):
        if entry != "pid":
            shutil.rmtree(os.path.join(WORK_DIR, entry), ignore_errors=True)
    RUN_DIR = os.path.join(WORK_DIR, str(me))
    os.makedirs(RUN_DIR, mode=0o700)
    with open(pid_file, "w") as f:
        f.write(str(me))

    def on_term(signum, frame):
        raise SystemExit(143)

    signal.signal(signal.SIGTERM, on_term)

try:
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

    # The range the bundle carries: from the base when the sandbox's
    # history holds it, else from the remote's default branch, which the
    # local checkout has too, else the whole history.
    has_commits = bool(head) and head != base
    rev_arg = ""
    commits = 0
    if has_commits:
        range_base = ""
        if base and run("git", "merge-base", "--is-ancestor", base, head, check=False).returncode == 0:
            range_base = base
        else:
            oh = run("git", "rev-parse", "--verify", "-q", "refs/remotes/origin/HEAD", check=False)
            cand = oh.stdout.decode().strip().lower() if oh.returncode == 0 else ""
            if cand and run("git", "merge-base", "--is-ancestor", cand, head, check=False).returncode == 0:
                range_base = cand
        rev_arg = f"{range_base}..HEAD" if range_base else "HEAD"
        # A HEAD reset onto the default branch leaves an empty range,
        # which git will not bundle.
        commits = int(run("git", "rev-list", "--count", rev_arg).stdout.decode().strip() or "0")
        has_commits = commits > 0

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

    # The landing's measures (DL-177): the objects the commits add, by
    # their inflated sizes, and the files to pack, by their own.
    object_bytes = 0
    blobs = []
    if has_commits and not (mode == "export"):
        objs = run("git", "rev-list", "--objects", rev_arg).stdout
        sizes = run("git", "cat-file", "--batch-check=%(objecttype) %(objectsize) %(rest)", input=objs).stdout
        for line in sizes.decode("utf-8", errors="surrogateescape").splitlines():
            parts = line.split(" ", 2)
            if len(parts) < 2 or not parts[1].isdigit():
                continue
            size = int(parts[1])
            object_bytes += size
            if parts[0] == "blob":
                blobs.append((size, parts[2] if len(parts) > 2 else ""))
    files = []
    worktree_bytes = 0
    for rel in to_pack:
        try:
            size = os.lstat(rel).st_size
        except OSError:
            size = 0
        worktree_bytes += size
        files.append((size, rel))

    def over_reasons():
        reasons = []
        total = object_bytes + worktree_bytes
        if total > EXPORT_MAX:
            reasons.append(f"the commits add {mib(object_bytes)} and the changed files are {mib(worktree_bytes)}, "
                           f"{mib(total)} together, over the {mib(EXPORT_MAX)} a sync can take")
        if len(to_pack) > ENTRIES_MAX:
            reasons.append(f"{len(to_pack)} changed files, over the {ENTRIES_MAX} a sync can take")
        big = [(size, name) for size, name in sorted(blobs + files, reverse=True) if size > BLOB_MAX]
        for size, name in big[:5]:
            reasons.append(f"{name[:200]} is {mib(size)}, over the {mib(BLOB_MAX)} one file may be")
        if len(big) > 5:
            reasons.append(f"and {len(big) - 5} more files over {mib(BLOB_MAX)}")
        return reasons

    def largest():
        top = sorted(blobs + files, reverse=True)[:5]
        return "".join(f"\n  {mib(size):>9}  {name[:200]}" for size, name in top)

    if inspect:
        commits, rewritten = 0, False
        if given_base and head != given_base:
            if run("git", "merge-base", "--is-ancestor", given_base, head, check=False).returncode == 0:
                commits = int(run("git", "rev-list", "--count", f"{given_base}..HEAD").stdout.decode().strip() or "0")
            else:
                rewritten = True
        reasons = over_reasons()
        sys.stdout.write(json.dumps({
            "branch": branch,
            "head_commit": head,
            "commits": commits,
            "files": len(to_pack) + len(deleted),
            "bytes": object_bytes + worktree_bytes,
            "object_bytes": object_bytes,
            "worktree_bytes": worktree_bytes,
            "over": "; ".join(reasons),
            "rewritten": rewritten,
        }))
        sys.exit(0)

    if archive:
        reasons = over_reasons()
        if reasons:
            sys.stderr.write("This sync-back is too large: " + "; ".join(reasons) + ".\n"
                             + "The largest files:" + largest() + "\n"
                             + "Add generated output to .gitignore, or remove the files in the sandbox, and sync again.\n")
            sys.exit(3)
        meta = json.dumps({
            "format": 1,
            "branch": branch,
            "base_commit": base,
            "head_commit": head,
            "deleted": deleted,
            "commits": commits,
            "files": len(to_pack) + len(deleted),
            "object_bytes": object_bytes,
            "worktree_bytes": worktree_bytes,
            "bundle": has_commits,
            "worktree": bool(to_pack),
        }).encode("utf-8")
        if len(meta) > META_MAX:
            sys.stderr.write(f"This sync-back lists too many deleted paths ({len(deleted)}) to send: "
                             "commit the deletions in the sandbox and sync again.\n")
            sys.exit(3)

        out = sys.stdout.buffer
        lock = threading.Lock()

        def header(name, size):
            info = tarfile.TarInfo(name)
            info.size = size
            info.mode = 0o600
            info.mtime = int(time.time())
            return info.tobuf(format=tarfile.PAX_FORMAT)

        def pad(size):
            return b"\0" * ((512 - size % 512) % 512)

        def add_bytes(name, data):
            with lock:
                out.write(header(name, len(data)) + data + pad(len(data)))
                out.flush()

        def add_file(name, fpath):
            size = os.path.getsize(fpath)
            with lock:
                out.write(header(name, size))
                with open(fpath, "rb") as f:
                    while True:
                        chunk = f.read(MIB)
                        if not chunk:
                            break
                        out.write(chunk)
                out.write(pad(size))
                out.flush()
            return size

        packing = threading.Event()

        def keepalive():
            while not packing.wait(10):
                add_bytes("spov-sync-keepalive", b"")

        add_bytes("spov-sync.json", meta)
        ticker = threading.Thread(target=keepalive, daemon=True)
        ticker.start()
        bundle_path = os.path.join(RUN_DIR, "state.bundle")
        tar_path = os.path.join(RUN_DIR, "worktree.tar.gz")
        list_path = os.path.join(RUN_DIR, "worktree.list")
        if has_commits:
            run("git", "bundle", "create", bundle_path, rev_arg)
        if to_pack:
            with open(list_path, "wb") as lf:
                for p in to_pack:
                    lf.write(p.encode("utf-8", errors="surrogateescape") + b"\x00")
            run("tar", "-czf", tar_path, "--null", "--no-recursion", "-T", list_path)
        packing.set()
        ticker.join()
        sent = {"bundle_bytes": 0, "worktree_bytes": 0}
        packed = (os.path.getsize(bundle_path) if has_commits else 0) + (os.path.getsize(tar_path) if to_pack else 0)
        if packed > PACKED_MAX:
            sys.stderr.write(f"This sync-back packs to {mib(packed)}, over the {mib(PACKED_MAX)} a sync can send.\n")
            sys.exit(3)
        if has_commits:
            sent["bundle_bytes"] = add_file("state.bundle", bundle_path)
        if to_pack:
            sent["worktree_bytes"] = add_file("worktree.tar.gz", tar_path)
        add_bytes("spov-sync-end.json", json.dumps(sent).encode("utf-8"))
        with lock:
            out.write(b"\0" * 1024)
            out.flush()
        sys.exit(0)

    # mode "export": the JSON route's answer, bounded by its own limit.
    over_json = (f"exported sandbox state exceeds {JSON_MAX // MIB} MiB limit; "
                 f"update spov, whose newer builds sync up to {EXPORT_MAX // MIB // 1024} GiB\n")
    total_bytes = 0
    bundle_b64 = ""
    if has_commits:
        with tempfile.NamedTemporaryFile(suffix=".bundle", delete=False) as tf:
            bpath = tf.name
        try:
            run("git", "bundle", "create", bpath, rev_arg)
            total_bytes += os.path.getsize(bpath)
            if total_bytes > JSON_MAX:
                sys.stderr.write(over_json)
                sys.exit(1)
            with open(bpath, "rb") as f:
                bundle_b64 = base64.b64encode(f.read()).decode("ascii")
        finally:
            if os.path.exists(bpath):
                os.remove(bpath)

    tar_b64 = ""
    if to_pack:
        with tempfile.NamedTemporaryFile(suffix=".list", delete=False) as lf, \
             tempfile.NamedTemporaryFile(suffix=".tar.gz", delete=False) as tf:
            lpath, tpath = lf.name, tf.name
            for p in to_pack:
                lf.write(p.encode("utf-8", errors="surrogateescape") + b"\x00")
        try:
            run("tar", "-czf", tpath, "--null", "--no-recursion", "-T", lpath)
            total_bytes += os.path.getsize(tpath)
            if total_bytes > JSON_MAX:
                sys.stderr.write(over_json)
                sys.exit(1)
            with open(tpath, "rb") as f:
                tar_b64 = base64.b64encode(f.read()).decode("ascii")
        finally:
            for p in (lpath, tpath):
                if os.path.exists(p):
                    os.remove(p)

    sys.stdout.write(json.dumps({
        "branch": branch,
        "base_commit": base,
        "head_commit": head,
        "bundle": bundle_b64,
        "worktree_tar": tar_b64,
        "deleted": deleted,
    }))
finally:
    # Only this run's own directory, and the pid file only while it
    # still names this run: a newer run may have taken over.
    if RUN_DIR:
        shutil.rmtree(RUN_DIR, ignore_errors=True)
        try:
            with open(os.path.join(WORK_DIR, "pid")) as f:
                if f.read().strip() == str(os.getpgrp()):
                    os.remove(os.path.join(WORK_DIR, "pid"))
        except OSError:
            pass
PY
`
