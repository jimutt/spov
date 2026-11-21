package git

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type archiveEntry struct {
	name string
	body []byte
}

// syncArchive builds a sync archive from entries, as the export script
// writes one, with the end-of-archive record unless cut is set.
func syncArchive(t *testing.T, entries []archiveEntry, cut bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Size: int64(len(e.body)), Mode: 0o600, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	if !cut {
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func jsonOf(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wholeArchive is a well-formed archive of a bundle and a worktree
// archive, with a keep-alive between the metadata and the parts.
func wholeArchive(t *testing.T, bundle, worktree []byte) []archiveEntry {
	meta := SyncMeta{Format: 1, Branch: "main", BaseCommit: strings.Repeat("a", 40), HeadCommit: strings.Repeat("b", 40),
		Deleted: []string{"gone.txt"}, Bundle: true, Worktree: true}
	return []archiveEntry{
		{syncMetaEntry, jsonOf(t, meta)},
		{syncKeepaliveEntry, nil},
		{syncBundleEntry, bundle},
		{syncKeepaliveEntry, nil},
		{syncWorktreeEntry, worktree},
		{syncEndEntry, jsonOf(t, syncEnd{BundleBytes: int64(len(bundle)), WorktreeBytes: int64(len(worktree))})},
	}
}

// TestASyncArchiveIsSpooledToFiles: the parts land as files in the
// spool, the metadata is reported before them, and keep-alives are
// skipped.
//
// @decision(DL-178)
func TestASyncArchiveIsSpooledToFiles(t *testing.T) {
	dir := t.TempDir()
	var told SyncMeta
	got, err := SpoolSyncArchive(bytes.NewReader(syncArchive(t, wholeArchive(t, []byte("BUNDLE"), []byte("TREE")), false)), dir,
		func(m SyncMeta) { told = m })
	if err != nil {
		t.Fatal(err)
	}
	if told.HeadCommit != strings.Repeat("b", 40) || got.HeadCommit != told.HeadCommit || got.Branch != "main" ||
		len(got.Deleted) != 1 || got.Dir != dir {
		t.Errorf("spooled %+v after meta %+v", got, told)
	}
	for path, want := range map[string]string{got.BundlePath: "BUNDLE", got.WorktreeTarPath: "TREE"} {
		if b, err := os.ReadFile(path); err != nil || string(b) != want || filepath.Dir(path) != dir {
			t.Errorf("%s = %q, %v", path, b, err)
		}
	}
	got.Remove()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the spool is still there: %v", err)
	}
}

// TestASyncArchiveThatIsNotWholeIsRefused: every way the stream can end
// early or hold something else is refused before anything lands.
//
// @decision(DL-178) @decision(DL-176)
func TestASyncArchiveThatIsNotWholeIsRefused(t *testing.T) {
	whole := wholeArchive(t, []byte("BUNDLE"), []byte("TREE"))
	archive := syncArchive(t, whole, false)
	for _, c := range []struct {
		name    string
		data    []byte
		cutWord bool
		says    string
	}{
		{name: "nothing", data: nil, cutWord: true},
		{name: "cut inside the bundle", data: archive[:512*3+3], cutWord: true},
		{name: "cut before the end entry", data: syncArchive(t, whole[:5], true), cutWord: true},
		{name: "an end that disagrees", data: syncArchive(t, append(slicesClone(whole[:5]),
			archiveEntry{syncEndEntry, jsonOf(t, syncEnd{BundleBytes: 99, WorktreeBytes: 4})}), false), cutWord: true},
		{name: "another entry first", data: syncArchive(t, append([]archiveEntry{{"evil", []byte("x")}}, whole...), false), says: "starts with"},
		{name: "an extra entry", data: syncArchive(t, append(slicesClone(whole[:3]), append([]archiveEntry{{"../escape", []byte("x")}}, whole[3:]...)...), false), says: "should hold"},
		{name: "another format", data: syncArchive(t, append([]archiveEntry{{syncMetaEntry, []byte(`{"format":2}`)}}, whole[1:]...), false), says: "format 2"},
		{name: "metadata over its bound", data: syncArchive(t, []archiveEntry{{syncMetaEntry, bytes.Repeat([]byte(" "), MaxSyncMetaBytes+1)}}, false), says: "over its"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := SpoolSyncArchive(bytes.NewReader(c.data), dir, nil)
			if err == nil {
				t.Fatal("accepted")
			}
			if c.cutWord && !errors.Is(err, ErrSyncCutShort) {
				t.Errorf("err = %v, want ErrSyncCutShort", err)
			}
			if c.says != "" && !strings.Contains(err.Error(), c.says) {
				t.Errorf("err = %v, want it to say %q", err, c.says)
			}
		})
	}
}

// TestALinkInASyncArchiveIsRefused: only regular files are read.
//
// @decision(DL-178)
func TestALinkInASyncArchiveIsRefused(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: syncMetaEntry, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = tw.Close()
	if _, err := SpoolSyncArchive(&buf, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "not a file") {
		t.Errorf("err = %v", err)
	}
}

// TestABrokenStreamIsCutShort: a reader that fails partway, as a
// truncated chunked body does, reads as a transfer cut short.
//
// @decision(DL-178)
func TestABrokenStreamIsCutShort(t *testing.T) {
	archive := syncArchive(t, wholeArchive(t, bytes.Repeat([]byte("b"), 4000), []byte("TREE")), false)
	r := io.MultiReader(bytes.NewReader(archive[:2000]), errReader{io.ErrUnexpectedEOF})
	if _, err := SpoolSyncArchive(r, t.TempDir(), nil); !errors.Is(err, ErrSyncCutShort) {
		t.Errorf("err = %v, want ErrSyncCutShort", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func slicesClone(e []archiveEntry) []archiveEntry { return append([]archiveEntry(nil), e...) }

// TestASpoolLivesInTheGitDirectoryAndOldOnesAreCleared: the spool is on
// the checkout's own filesystem, and a day-old one is removed.
//
// @decision(DL-178)
func TestASpoolLivesInTheGitDirectoryAndOldOnesAreCleared(t *testing.T) {
	dir, _ := initTestRepo(t, "")
	stale := filepath.Join(dir, ".git", "spov-sync-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	spool, err := NewSyncSpool(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(spool) != filepath.Join(dir, ".git") {
		t.Errorf("spool at %s, want it under .git", spool)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale spool is still there: %v", err)
	}
	if info, err := os.Stat(spool); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("spool mode = %v, %v", info, err)
	}
}

// TestTheWorktreeBoundsAreReadBeforeAnythingLands: archivePaths refuses
// a file or a total over the landing's bounds from the headers alone.
//
// @decision(DL-177)
func TestTheWorktreeBoundsAreReadBeforeAnythingLands(t *testing.T) {
	defer func(total, blob int64) { maxSyncIncomingBytes, maxSyncIncomingBlobBytes = total, blob }(maxSyncIncomingBytes, maxSyncIncomingBlobBytes)
	maxSyncIncomingBytes, maxSyncIncomingBlobBytes = 10, 6
	write := func(files map[string]string) string {
		p := filepath.Join(t.TempDir(), "w.tar.gz")
		if err := os.WriteFile(p, makeWorktreeTar(t, files), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := archivePaths(write(map[string]string{"a": "12345", "b": "12345"})); err != nil {
		t.Errorf("within the bounds: %v", err)
	}
	if _, err := archivePaths(write(map[string]string{"big": "1234567"})); err == nil || !strings.Contains(err.Error(), "big") {
		t.Errorf("one file over: %v", err)
	}
	if _, err := archivePaths(write(map[string]string{"a": "123456", "b": "123456"})); err == nil || !strings.Contains(err.Error(), "changed files") {
		t.Errorf("the total over: %v", err)
	}
}

// TestAWorktreeArchiveOverItsStreamOrFileCountIsRefused: the bound on
// the decompressed stream is an error, never an early end, and too many
// entries are refused before anything lands.
//
// @decision(DL-177)
func TestAWorktreeArchiveOverItsStreamOrFileCountIsRefused(t *testing.T) {
	defer func(n int) { maxSyncEntries = n }(maxSyncEntries)
	maxSyncEntries = 2
	p := filepath.Join(t.TempDir(), "w.tar.gz")
	if err := os.WriteFile(p, makeWorktreeTar(t, map[string]string{"a": "1", "b": "2", "c": "3"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := archivePaths(p); err == nil || !strings.Contains(err.Error(), "over 2 files") {
		t.Errorf("three files under a cap of two: %v", err)
	}

	r := &errorAfter{r: strings.NewReader(strings.Repeat("x", 100)), left: 10}
	if _, err := io.ReadAll(r); !errors.Is(err, errTreeTooLarge) {
		t.Errorf("a stream past its bound = %v, want errTreeTooLarge", err)
	}
	r = &errorAfter{r: strings.NewReader("short"), left: 10}
	if b, err := io.ReadAll(r); err != nil || string(b) != "short" {
		t.Errorf("a stream within its bound = %q, %v", b, err)
	}
}

// TestAStreamThatBreaksIsCutShortWhateverTheCause: a reset stream reads
// as a transfer cut short, with its cause kept.
//
// @decision(DL-178)
func TestAStreamThatBreaksIsCutShortWhateverTheCause(t *testing.T) {
	archive := syncArchive(t, wholeArchive(t, bytes.Repeat([]byte("b"), 4000), []byte("TREE")), false)
	r := io.MultiReader(bytes.NewReader(archive[:2000]), errReader{errors.New("stream error: stream ID 3; INTERNAL_ERROR")})
	_, err := SpoolSyncArchive(r, t.TempDir(), nil)
	if !errors.Is(err, ErrSyncCutShort) || !strings.Contains(err.Error(), "INTERNAL_ERROR") {
		t.Errorf("err = %v", err)
	}
}
