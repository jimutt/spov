package git

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeInfo is a directory or file entry for a fake probe.
type fakeInfo struct{ dir bool }

func (f fakeInfo) Name() string       { return "" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return nil }
func (f fakeInfo) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

// TestEveryExistingComponentMustBeListedUnderItsExactName: a component
// the filesystem resolves but its directory does not list by that name
// is an alias, and a new component shaped like one is refused too.
//
// @decision(DL-126)
func TestEveryExistingComponentMustBeListedUnderItsExactName(t *testing.T) {
	root := "/r"
	// The filesystem resolves GITHOO~1 and .GITHOOKS to .githooks, as
	// NTFS does, but lists only the long name.
	exists := map[string]fakeInfo{
		"/r/.githooks": {dir: true}, "/r/GITHOO~1": {dir: true}, "/r/.GITHOOKS": {dir: true},
		"/r/src": {dir: true}, "/r/src/main.go": {},
	}
	listings := map[string][]string{"/r": {".githooks", "src"}, "/r/src": {"main.go"}}
	probe := pathProbe{
		lstat: func(p string) (fs.FileInfo, error) {
			if info, ok := exists[filepath.ToSlash(p)]; ok {
				return info, nil
			}
			return nil, fs.ErrNotExist
		},
		readDir: func(d string) ([]string, error) { return listings[filepath.ToSlash(d)], nil },
	}
	for _, p := range []string{"GITHOO~1/post-index-change", ".GITHOOKS/pre-push", "src/new~1.txt", "src/a.", "src/b ", "src/c:d", "new/PROGRA~2/x"} {
		if reason := aliasVeto(root, []string{p}, probe); !strings.Contains(reason, p) {
			t.Errorf("aliasVeto(%q) = %q, want a reason naming it", p, reason)
		}
	}
	for _, p := range []string{".githooks/pre-push", "src/main.go", "src/new.go", "new/dir/file.txt", "a~b.txt"} {
		if reason := aliasVeto(root, []string{p}, probe); reason != "" {
			t.Errorf("aliasVeto(%q) = %q, want none", p, reason)
		}
	}
}

// windowsProbeRoot is where the NTFS test works: a Windows drive
// mounted under WSL. It is removed only when empty.
const windowsProbeRoot = "/mnt/c/Users/Public/spov-fix-probe"

// TestSyncBackOnAWindowsDriveNeverWritesThroughAShortName runs a real
// sync-back on NTFS, where `GITHOO~1` is `.githooks` and `NODE_M~1` is
// `node_modules`. It skips where no such drive is mounted or 8.3 names
// do not resolve.
//
// @decision(DL-126)
func TestSyncBackOnAWindowsDriveNeverWritesThroughAShortName(t *testing.T) {
	if err := os.MkdirAll(windowsProbeRoot, 0o755); err != nil {
		t.Skipf("no Windows drive here: %v", err)
	}
	dir, err := os.MkdirTemp(windowsProbeRoot, "sync-")
	if err != nil {
		t.Skipf("cannot create a directory on the Windows drive: %v", err)
	}
	t.Cleanup(func() {
		if strings.HasPrefix(dir, windowsProbeRoot+"/sync-") {
			_ = os.RemoveAll(dir)
		}
		_ = os.Remove(windowsProbeRoot)
	})
	writeFile(t, dir, ".githooks/pre-push", "#!/bin/sh\n", 0o755)
	if _, err := os.Lstat(filepath.Join(dir, "GITHOO~1")); err != nil {
		t.Skipf("8.3 short names do not resolve here: %v", err)
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	writeFile(t, dir, ".gitignore", "node_modules/\n", 0o644)
	runGit(t, dir, "add", ".")
	head := commitHead(t, dir, "hooks")
	runGit(t, dir, "config", "core.hooksPath", ".githooks")
	writeFile(t, dir, "node_modules/.bin/tsc", "real\n", 0o755)

	for _, entries := range [][]tarEntry{
		{{name: "GITHOO~1/post-index-change", body: "#!/bin/sh\necho ran > hook-ran\n"}},
		{{name: "NODE_M~1/.bin/tsc", body: "planted\n"}},
		{{name: ".GITHOOKS/post-index-change", body: "#!/bin/sh\necho ran > hook-ran\n"}},
	} {
		res, err := ApplySync(context.Background(), dir, "s", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: head, WorktreeTar: makeTar(t, entries...),
		})
		wantFallback(t, dir, head, res, err, entries[0].name)
	}
	wantAbsent(t, dir, ".githooks/post-index-change")
	wantContent(t, dir, "node_modules/.bin/tsc", "real\n")

	state := sandboxCommits(t, dir, head, func(s string) {
		writeFile(t, s, "GITHOO~1/post-index-change", "#!/bin/sh\necho ran > hook-ran\n", 0o755)
		runGit(t, s, "add", "-A")
	})
	res, err := ApplySync(context.Background(), dir, "s2", "main", state)
	wantFallback(t, dir, head, res, err, "GITHOO~1/post-index-change")
	wantAbsent(t, dir, ".githooks/post-index-change")
	userCommits(t, dir)
	wantAbsent(t, dir, "hook-ran")
}
