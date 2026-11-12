package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// fastForward moves the checked-out branch and the work tree from cur
// to head with a fast-forward merge that refuses to overwrite an
// untracked or ignored file (`--no-overwrite-ignore`); `reset --hard`
// and `read-tree -m -u` both overwrite ignored files. When git refuses
// and the checkout is still at cur and clean, refused is git's message
// and nothing was written; any other failure is an error.
//
// @decision(DL-126)
func fastForward(ctx context.Context, root, cur, head string) (refused string, err error) {
	_, mergeErr := gitOutput(ctx, root, "merge", "--ff-only", "--no-overwrite-ignore", "--no-stat", "-q", head)
	if mergeErr == nil {
		return "", nil
	}
	now, herr := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	status, serr := gitOutputRaw(ctx, root, "status", "--porcelain", "--untracked-files=normal")
	if herr != nil || serr != nil || !strings.EqualFold(now, cur) || strings.TrimSpace(status) != "" {
		return "", fmt.Errorf("%w; the checkout may be partly updated", mergeErr)
	}
	return "git refused to update the checkout: " + strings.Join(strings.Fields(mergeErr.Error()), " "), nil
}

// boundIncomingObjects refuses a sync whose new objects (those reachable
// from head but not from cur) total more than maxSyncIncomingBytes, or
// whose largest blob is over maxSyncIncomingBlobBytes. The sizes come
// from the object headers (`cat-file --batch-check`), which does not
// inflate the content, so a delta that would expand to gigabytes is
// caught before the checkout pays for it. No ref has been written when
// this runs, so a refusal leaves the checkout and its refs as they were.
//
// @decision(DL-126)
func boundIncomingObjects(ctx context.Context, root, cur, head string) error {
	rangeArg := head
	if cur != "" && cur != head && commitExists(ctx, root, cur) {
		rangeArg = cur + ".." + head
	}
	listed, err := gitOutputRaw(ctx, root, "rev-list", "--objects", rangeArg)
	if err != nil {
		return fmt.Errorf("git: list the objects the sync brings in: %w", err)
	}
	var shas strings.Builder
	for _, line := range strings.Split(listed, "\n") {
		if sha, _, _ := strings.Cut(strings.TrimSpace(line), " "); sha != "" {
			shas.WriteString(sha)
			shas.WriteByte('\n')
		}
	}
	if shas.Len() == 0 {
		return nil
	}
	cmd := Command(ctx, root, nil, "cat-file", "--batch-check=%(objecttype) %(objectsize)")
	cmd.Stdin = strings.NewReader(shas.String())
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git: measure the objects the sync brings in: %w", err)
	}
	var total, biggest int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		typ, sizeStr, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		var size int64
		if _, err := fmt.Sscan(sizeStr, &size); err != nil {
			continue
		}
		total += size
		if typ == "blob" && size > biggest {
			biggest = size
		}
	}
	if biggest > maxSyncIncomingBlobBytes {
		return fmt.Errorf("git: the sandbox's export holds a %d MiB file (limit %d MiB); nothing was applied",
			biggest>>20, maxSyncIncomingBlobBytes>>20)
	}
	if total > maxSyncIncomingBytes {
		return fmt.Errorf("git: the sandbox's export expands to %d MiB of objects (limit %d MiB); nothing was applied",
			total>>20, maxSyncIncomingBytes>>20)
	}
	return nil
}

// An in-place sync-back lands the sandbox's commits and uncommitted
// files in the user's work tree. inPlaceVeto decides, before anything
// is written, whether it may: it returns a reason, naming the paths,
// when it may not, and an error only for an export spov's own script
// could not have built.

// Kinds of entry a commit's tree holds at a path.
const (
	kindFile   = 'f'
	kindLink   = 'l'
	kindTree   = 'd'
	kindGitlnk = 'g'
)

// treeKinds is every path of a commit, its trees included, with its kind.
type treeKinds map[string]byte

// kindsAt lists commit's tree recursively, trees and gitlinks included.
func kindsAt(ctx context.Context, root, commit string) (treeKinds, error) {
	out, err := gitOutputRaw(ctx, root, "ls-tree", "-r", "-t", "-z", "--full-tree", commit)
	if err != nil {
		return nil, fmt.Errorf("git: list %s: %w", commit[:min(12, len(commit))], err)
	}
	kinds := treeKinds{}
	for _, rec := range strings.Split(out, "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		mode, _, _ := strings.Cut(meta, " ")
		switch mode {
		case "040000":
			kinds[p] = kindTree
		case "120000":
			kinds[p] = kindLink
		case "160000":
			kinds[p] = kindGitlnk
		default:
			kinds[p] = kindFile
		}
	}
	return kinds, nil
}

// committedPath is one path the commits change, with git's status
// letter (A, M, T or D; renames are off).
type committedPath struct {
	status byte
	path   string
}

// committedPaths is `git diff --raw --no-renames` from the checkout's
// head to the sandbox's.
func committedPaths(ctx context.Context, root, from, to string) ([]committedPath, error) {
	out, err := gitOutputRaw(ctx, root, "diff", "--raw", "-z", "--no-renames", "--no-abbrev", "--no-ext-diff", from, to)
	if err != nil {
		return nil, fmt.Errorf("git: compare %s with %s: %w", from[:min(12, len(from))], to[:min(12, len(to))], err)
	}
	fields := strings.Split(out, "\x00")
	var paths []committedPath
	for i := 0; i+1 < len(fields); i += 2 {
		meta := fields[i]
		if meta == "" {
			break
		}
		parts := strings.Fields(meta)
		if len(parts) < 5 || parts[4] == "" {
			return nil, fmt.Errorf("git: unexpected diff record %q", meta)
		}
		paths = append(paths, committedPath{status: parts[4][0], path: fields[i+1]})
	}
	return paths, nil
}

// inPlaceVeto says why the export must not land in place in root, whose
// head is curHead, or "" when it may. packed and deleted are the
// export's uncommitted paths, already cleaned.
//
// @decision(DL-126) @decision(DL-136)
func inPlaceVeto(ctx context.Context, root, curHead, headCommit string, packed, deleted []string) (string, error) {
	cur, err := kindsAt(ctx, root, curHead)
	if err != nil {
		return "", err
	}
	head := cur
	if headCommit != curHead {
		if head, err = kindsAt(ctx, root, headCommit); err != nil {
			return "", err
		}
	}
	for _, d := range deleted {
		if k := head[d]; k != kindFile && k != kindLink {
			return "", fmt.Errorf("git: the sandbox's export deletes %q, which its commit does not track; nothing was applied", d)
		}
	}
	var committed []committedPath
	if headCommit != curHead {
		if committed, err = committedPaths(ctx, root, curHead, headCommit); err != nil {
			return "", err
		}
	}
	var touched, written []string
	for _, c := range committed {
		touched = append(touched, c.path)
		if c.status != 'D' {
			written = append(written, c.path)
		}
	}
	uncommitted := append(slices.Clone(packed), deleted...)
	touched = append(touched, uncommitted...)

	for _, check := range []func() (string, error){
		func() (string, error) { return gitDirVeto(touched), nil },
		func() (string, error) { return executedVeto(ctx, root, touched), nil },
		func() (string, error) { return gitReadFileVeto(touched), nil },
		func() (string, error) { return submoduleVeto(cur, head, touched), nil },
		func() (string, error) { return aliasVeto(root, touched, osProbe()), nil },
		func() (string, error) { return inTheWayVeto(ctx, root, cur, head, written, uncommitted) },
		func() (string, error) { return archiveShapeVeto(head, packed, deleted), nil },
		func() (string, error) { return ignoredVeto(ctx, root, head, packed) },
	} {
		if reason, err := check(); reason != "" || err != nil {
			return reason, err
		}
	}
	return "", nil
}

// unpredictableWorkTreeVeto falls back when the checkout's own config
// makes an in-place landing write somewhere other than where the veto
// looked, or hide what it must see: a sparse checkout (the fast-forward
// skips paths outside the sparse set, and status would not show a file
// hidden by it) or a work tree moved off root (`core.worktree`, a git
// file worktree). The veto reads and writes root; git would not. These
// cannot be overridden per command without changing where the sync
// lands, so the work goes to spov/<session> instead.
//
// @decision(DL-126)
func unpredictableWorkTreeVeto(ctx context.Context, root string) string {
	if v, _ := gitConfigValue(ctx, root, "core.sparsecheckout"); strings.EqualFold(strings.TrimSpace(v), "true") {
		return "this checkout is a sparse checkout (core.sparseCheckout), where an in-place sync could miss paths"
	}
	if v, _ := gitConfigValue(ctx, root, "core.worktree"); strings.TrimSpace(v) != "" {
		return "this checkout's work tree is set elsewhere (core.worktree)"
	}
	top, err := gitOutput(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	realTop, rerr := filepath.EvalSymlinks(top)
	realRoot, qerr := filepath.EvalSymlinks(root)
	if rerr == nil && qerr == nil && realTop != realRoot {
		return "this checkout's work tree is not where the sync was told to land"
	}
	return ""
}

// namePaths names up to three paths, then how many more there are.
func namePaths(paths []string) string {
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if len(paths) <= 3 {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:3], ", "), len(paths)-3)
}

// leadingDirs are the directories above p, outermost first.
func leadingDirs(p string) []string {
	var dirs []string
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			dirs = append(dirs, p[:i])
		}
	}
	return dirs
}

// within reports whether p is q or lies under it.
func within(p, q string) bool {
	return q == "." || p == q || strings.HasPrefix(p, q+"/")
}

// gitDirVeto: a committed path that names the git directory on some
// filesystem. git's own checks cover `.git` itself; these cover the
// other names namesGitDir knows.
func gitDirVeto(touched []string) string {
	var hit []string
	for _, p := range touched {
		for _, seg := range strings.Split(p, "/") {
			if namesGitDir(seg) {
				hit = append(hit, p)
				break
			}
		}
	}
	if len(hit) == 0 {
		return ""
	}
	return fmt.Sprintf("the session's changes name the git directory (%s)", namePaths(hit))
}

// executedVeto: a touched path that is, lies under, or lies above a
// directory or file this checkout makes git run or read as its own
// configuration: the hooks directory and any config file in the work
// tree.
func executedVeto(ctx context.Context, root string, touched []string) string {
	protected := executedPaths(ctx, root)
	var hit, what []string
	for _, p := range touched {
		lp := strings.ToLower(p)
		for _, q := range protected {
			if within(lp, q.path) || within(q.path, lp) {
				hit = append(hit, p)
				what = append(what, q.what)
				break
			}
		}
	}
	if len(hit) == 0 {
		return ""
	}
	return fmt.Sprintf("the session changed %s, which is or holds %s in this checkout; read it before you check the branch out",
		namePaths(hit), namePaths(what))
}

// executedPath is a work-tree path, lowercased, and what git takes it for.
type executedPath struct{ path, what string }

// executedPaths are the work-tree paths, relative to root and
// lowercased, of the hooks directory and of every config file git reads
// for this checkout or would read through an include, each both as
// configured and as resolved on disk. Paths under the git directory are
// left out, since nothing in an export can reach them.
func executedPaths(ctx context.Context, root string) []executedPath {
	var out []executedPath
	add := func(p, what string) {
		for _, rel := range workTreeRels(root, p) {
			out = append(out, executedPath{path: strings.ToLower(rel), what: what + " " + rel})
		}
	}
	if hooks, err := outputOf(commandReadingHooksPath(ctx, root, "rev-parse", "--git-path", "hooks")); err == nil {
		if hooks = strings.TrimSpace(hooks); hooks != "" {
			add(hooks, "the hooks directory")
		}
	}
	listing, err := gitOutputRaw(ctx, root, "config", "--list", "--show-origin", "-z")
	if err != nil {
		return out
	}
	fields := strings.Split(listing, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		origin, entry := fields[i], fields[i+1]
		file, ok := strings.CutPrefix(origin, "file:")
		if !ok {
			continue
		}
		add(file, "the git config file")
		key, value, _ := strings.Cut(entry, "\n")
		key = strings.ToLower(key)
		if key == "include.path" || (strings.HasPrefix(key, "includeif.") && strings.HasSuffix(key, ".path")) {
			base := file
			if !filepath.IsAbs(base) {
				base = filepath.Join(root, base)
			}
			add(expandHome(value, filepath.Dir(base)), "the git config file")
			continue
		}
		// Any other value may name a program or a file git runs or
		// reads (core.fsmonitor, a filter driver, core.attributesFile),
		// relative to the work tree.
		for _, tok := range strings.Fields(value) {
			tok = strings.Trim(tok, `"'!`)
			if tok == "" || strings.Contains(tok, "://") {
				continue
			}
			if !strings.Contains(tok, "/") {
				if _, err := os.Lstat(filepath.Join(root, tok)); err != nil {
					continue
				}
			}
			add(expandHome(tok, root), "a path git config names ("+key+")")
		}
	}
	return out
}

// expandHome is p with a leading `~/` made the home directory, and a
// relative p taken against base.
func expandHome(p, base string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// workTreeRels is p relative to the work tree at root, as written and
// as resolved through any symlinks on disk, leaving out a form outside
// the work tree or under its .git.
func workTreeRels(root, p string) []string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	roots := []string{filepath.Clean(root)}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != roots[0] {
		roots = append(roots, real)
	}
	forms := []string{filepath.Clean(p)}
	if real := resolveExisting(p); real != forms[0] {
		forms = append(forms, real)
	}
	var rels []string
	for _, r := range roots {
		for _, f := range forms {
			rel, err := filepath.Rel(r, f)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			rel = filepath.ToSlash(rel)
			if within(strings.ToLower(rel), ".git") {
				continue
			}
			if !slices.Contains(rels, rel) {
				rels = append(rels, rel)
			}
		}
	}
	return rels
}

// resolveExisting resolves the symlinks in the longest leading part of p
// that exists, keeping the rest as written.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// gitReadFiles are the files at any depth git reads from the work tree
// and that can make it run a program (a filter driver, a submodule's
// URL, git-lfs's endpoint).
var gitReadFiles = []string{".gitattributes", ".lfsconfig", ".gitmodules"}

// namesGitReadFile reports whether a path segment is one of gitReadFiles
// under the same rules namesGitDir applies to `.git`.
func namesGitReadFile(seg string) bool {
	seg = strings.TrimRight(cutStream(stripIgnorable(seg)), ". ")
	for _, name := range gitReadFiles {
		if strings.EqualFold(seg, name) {
			return true
		}
	}
	return false
}

// gitReadFileVeto: a touched path whose last segment is one of
// gitReadFiles.
func gitReadFileVeto(touched []string) string {
	var hit []string
	for _, p := range touched {
		if namesGitReadFile(path.Base(p)) {
			hit = append(hit, p)
		}
	}
	if len(hit) == 0 {
		return ""
	}
	return fmt.Sprintf("the session changed %s, which git reads to decide what programs to run; read it before you check the branch out", namePaths(hit))
}

// submoduleVeto: a touched path inside a submodule of the checkout's
// commit or the sandbox's. Only the outer repository's rules are
// checked, and an honest export never writes there.
func submoduleVeto(cur, head treeKinds, touched []string) string {
	var hit []string
	for _, p := range touched {
		for _, d := range leadingDirs(p) {
			if cur[d] == kindGitlnk || head[d] == kindGitlnk {
				hit = append(hit, p)
				break
			}
		}
	}
	if len(hit) == 0 {
		return ""
	}
	return fmt.Sprintf("the session's changes reach inside a submodule (%s)", namePaths(hit))
}

// refuseInsideSubmodule refuses an export whose uncommitted state
// writes or deletes a path inside a submodule of the sandbox's commit:
// git status could not have listed it, and neither landing can hold it.
//
// @decision(DL-126)
func refuseInsideSubmodule(ctx context.Context, root, headCommit string, packed, deleted []string) error {
	if len(packed) == 0 && len(deleted) == 0 {
		return nil
	}
	head, err := kindsAt(ctx, root, headCommit)
	if err != nil {
		return err
	}
	if reason := submoduleVeto(head, head, append(slices.Clone(packed), deleted...)); reason != "" {
		return fmt.Errorf("git: %s; nothing was applied", reason)
	}
	return nil
}

// pathProbe reads the filesystem for aliasVeto; a test gives it a fake.
type pathProbe struct {
	lstat   func(string) (fs.FileInfo, error)
	readDir func(string) ([]string, error)
}

func osProbe() pathProbe {
	return pathProbe{
		lstat: os.Lstat,
		readDir: func(dir string) ([]string, error) {
			entries, err := os.ReadDir(dir)
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			return names, err
		},
	}
}

// aliasSegment is a segment some filesystem may resolve to another name:
// an NTFS short name, a trailing dot or space, a stream suffix.
var aliasSegment = regexp.MustCompile(`~[0-9]|[. ]$|:`)

// aliasVeto: a touched path one of whose components exists on disk but
// is not listed in its directory under that exact name (an NTFS short
// name, a case or Unicode alias, a name with a trailing dot or space),
// or one of whose new components looks like such a name. Either way the
// path a check reads is not the file the write reaches.
func aliasVeto(root string, touched []string, probe pathProbe) string {
	listed := map[string]map[string]bool{}
	names := func(dir string) map[string]bool {
		if set, ok := listed[dir]; ok {
			return set
		}
		set := map[string]bool{}
		if all, err := probe.readDir(dir); err == nil {
			for _, n := range all {
				set[n] = true
			}
		}
		listed[dir] = set
		return set
	}
	var hit []string
	for _, p := range touched {
		if aliasedPath(root, p, probe, names) {
			hit = append(hit, p)
		}
	}
	if len(hit) == 0 {
		return ""
	}
	return fmt.Sprintf("the session's changes name %s, which this filesystem may resolve to another file", namePaths(hit))
}

func aliasedPath(root, p string, probe pathProbe, names func(string) map[string]bool) bool {
	dir := root
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		full := filepath.Join(dir, seg)
		info, err := probe.lstat(full)
		if err != nil {
			// The rest is new: refuse what a filesystem may alias.
			return slices.ContainsFunc(segs[i:], aliasSegment.MatchString)
		}
		if !names(dir)[seg] {
			return true
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return false // the shape checks decide what lies below
		}
		dir = full
	}
	return false
}

// inTheWayVeto: a path the commits write, or the export's uncommitted
// state writes or deletes, where the checkout holds something it does
// not track: an untracked or ignored file, a directory with such files
// in it, or such a directory above the path. git status --ignored names
// these; an entry on disk that no tracked path accounts for counts too.
func inTheWayVeto(ctx context.Context, root string, cur, head treeKinds, written, uncommitted []string) (string, error) {
	if len(written) == 0 && len(uncommitted) == 0 {
		return "", nil
	}
	out, err := gitOutputRaw(ctx, root, "status", "--porcelain=v1", "-z", "--ignored", "--untracked-files=normal")
	if err != nil {
		return "", fmt.Errorf("git: list what this checkout does not track: %w", err)
	}
	unseen := map[string]bool{}
	above := map[string]bool{}
	for _, rec := range strings.Split(out, "\x00") {
		if len(rec) < 4 || (rec[:3] != "?? " && rec[:3] != "!! ") {
			continue
		}
		p := strings.TrimSuffix(rec[3:], "/")
		unseen[p] = true
		for _, d := range leadingDirs(p) {
			above[d] = true
		}
	}
	blocked := func(p string, tracked func(string) bool) bool {
		if unseen[p] || above[p] {
			return true
		}
		for _, d := range leadingDirs(p) {
			if unseen[d] {
				return true
			}
		}
		for _, x := range append(leadingDirs(p), p) {
			info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(x)))
			switch {
			case err != nil:
				return false // nothing on disk from here down
			case !tracked(x):
				return true
			case info.Mode()&fs.ModeSymlink != 0 || !info.IsDir():
				return false // a tracked entry the change replaces
			}
		}
		return false
	}
	var hit []string
	for _, p := range written {
		if blocked(p, func(x string) bool { return cur[x] != 0 }) {
			hit = append(hit, p)
		}
	}
	for _, p := range uncommitted {
		if blocked(p, func(x string) bool { return cur[x] != 0 || head[x] != 0 }) {
			hit = append(hit, p)
		}
	}
	if len(hit) == 0 {
		return "", nil
	}
	return fmt.Sprintf("the session's changes would replace files this checkout does not track (%s)", namePaths(hit)), nil
}

// archiveShapeVeto: an uncommitted path under one the sandbox's commit
// holds as a file or symlink, or under another archive entry. git
// status could not have listed it.
func archiveShapeVeto(head treeKinds, packed, deleted []string) string {
	entries := map[string]bool{}
	for _, p := range packed {
		entries[p] = true
	}
	var hit []string
	for _, p := range append(slices.Clone(packed), deleted...) {
		for _, d := range leadingDirs(p) {
			if k := head[d]; k == kindFile || k == kindLink || entries[d] {
				hit = append(hit, p)
				break
			}
		}
	}
	if len(hit) == 0 {
		return ""
	}
	return fmt.Sprintf("the session's uncommitted files lie under a file or symlink (%s)", namePaths(hit))
}

// ignoredVeto: an archive entry the sandbox's commit does not track and
// this checkout's ignore rules match. The rules are the user's, read
// before anything is written; an honest export can hold such a path
// when the user ignores more than the sandbox did (a global *.log).
func ignoredVeto(ctx context.Context, root string, head treeKinds, packed []string) (string, error) {
	var untracked []string
	for _, p := range packed {
		if head[p] == 0 {
			untracked = append(untracked, p)
		}
	}
	if len(untracked) == 0 {
		return "", nil
	}
	cmd := Command(ctx, root, nil, "check-ignore", "--no-index", "-z", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(untracked, "\x00") + "\x00")
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return "", nil // nothing is ignored
	case err != nil:
		return "", fmt.Errorf("git: check the export against this checkout's ignore rules: %w", err)
	}
	ignored := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	return fmt.Sprintf("the session left files this checkout ignores (%s), which git status would not show", namePaths(ignored)), nil
}
