package git

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SyncMeta is a sync archive's first entry, spov-sync.json: what the
// sandbox measured before it packed anything.
//
// @decision(DL-176)
type SyncMeta struct {
	Format        int      `json:"format"`
	Branch        string   `json:"branch"`
	BaseCommit    string   `json:"base_commit"`
	HeadCommit    string   `json:"head_commit"`
	Deleted       []string `json:"deleted"`
	Commits       int      `json:"commits"`
	Files         int      `json:"files"`
	ObjectBytes   int64    `json:"object_bytes"`
	WorktreeBytes int64    `json:"worktree_bytes"`
	Bundle        bool     `json:"bundle"`
	Worktree      bool     `json:"worktree"`
}

// syncEnd is a sync archive's last entry: the packed sizes that came.
type syncEnd struct {
	BundleBytes   int64 `json:"bundle_bytes"`
	WorktreeBytes int64 `json:"worktree_bytes"`
}

// The names of a sync archive's entries.
const (
	syncMetaEntry      = "spov-sync.json"
	syncKeepaliveEntry = "spov-sync-keepalive"
	syncBundleEntry    = "state.bundle"
	syncWorktreeEntry  = "worktree.tar.gz"
	syncEndEntry       = "spov-sync-end.json"
)

// ErrSyncCutShort is a sync archive that did not arrive whole.
//
// @decision(DL-178)
var ErrSyncCutShort = errors.New("the sync-back was cut short before it arrived whole; nothing was applied, and syncing again is safe")

// SpooledSync is a sync-back's state on disk, ready for ApplySync. The
// bundle and the worktree archive are files in Dir, which Remove
// deletes.
//
// @decision(DL-178)
type SpooledSync struct {
	Dir        string
	Branch     string
	BaseCommit string
	HeadCommit string
	Deleted    []string
	// BundlePath and WorktreeTarPath are empty when there is none.
	BundlePath      string
	WorktreeTarPath string
}

// Remove deletes the spool's directory.
func (s SpooledSync) Remove() {
	if s.Dir != "" {
		_ = os.RemoveAll(s.Dir)
	}
}

// staleSpool is how old a spool directory a sync-back left must be
// before the next one removes it.
const staleSpool = 24 * time.Hour

// NewSyncSpool makes a private directory for one sync-back under the
// checkout's git common directory, on the checkout's own filesystem,
// and removes ones a day old that earlier sync-backs left.
//
// @decision(DL-178)
func NewSyncSpool(ctx context.Context, root string) (string, error) {
	common, err := gitOutput(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("git: find the git directory of %s: %w", root, err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	if old, _ := filepath.Glob(filepath.Join(common, "spov-sync-*")); len(old) > 0 {
		for _, d := range old {
			if info, err := os.Lstat(d); err == nil && info.IsDir() && time.Since(info.ModTime()) > staleSpool {
				_ = os.RemoveAll(d)
			}
		}
	}
	dir, err := os.MkdirTemp(common, "spov-sync-")
	if err != nil {
		return "", fmt.Errorf("git: make a directory for the sync-back: %w", err)
	}
	return dir, nil
}

// SpoolExported writes a JSON export's parts into dir, so that
// ApplySync has one input shape.
//
// @decision(DL-178)
func SpoolExported(dir string, st ExportedState) (SpooledSync, error) {
	out := SpooledSync{Dir: dir, Branch: st.Branch, BaseCommit: st.BaseCommit, HeadCommit: st.HeadCommit, Deleted: st.Deleted}
	if len(st.Bundle) > 0 {
		out.BundlePath = filepath.Join(dir, syncBundleEntry)
		if err := os.WriteFile(out.BundlePath, st.Bundle, 0o600); err != nil {
			return SpooledSync{}, err
		}
	}
	if len(st.WorktreeTar) > 0 {
		out.WorktreeTarPath = filepath.Join(dir, syncWorktreeEntry)
		if err := os.WriteFile(out.WorktreeTarPath, st.WorktreeTar, 0o600); err != nil {
			return SpooledSync{}, err
		}
	}
	return out, nil
}

// SpoolSyncArchive reads DL-176's archive from r into dir. onMeta, when
// set, is told what the archive holds once its first entry is read.
// Anything but the whole archive in its order (a stream that ends early,
// an entry it should not hold, a part of another size than it says) is
// refused, with nothing landed; a stream that stops is ErrSyncCutShort.
//
// @decision(DL-178) @decision(DL-176)
func SpoolSyncArchive(r io.Reader, dir string, onMeta func(SyncMeta)) (SpooledSync, error) {
	tr := tar.NewReader(r)
	next := func() (*tar.Header, error) {
		for {
			hdr, err := tr.Next()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return nil, ErrSyncCutShort
				}
				return nil, fmt.Errorf("git: read the sync-back: %w", err)
			}
			if hdr.Typeflag != tar.TypeReg {
				return nil, fmt.Errorf("git: the sync-back holds %q, which is not a file", hdr.Name)
			}
			if hdr.Name == syncKeepaliveEntry && hdr.Size == 0 {
				continue
			}
			return hdr, nil
		}
	}
	readJSON := func(hdr *tar.Header, v any) error {
		if hdr.Size > MaxSyncMetaBytes {
			return fmt.Errorf("git: the sync-back's %s is %d bytes, over its %d", hdr.Name, hdr.Size, MaxSyncMetaBytes)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return cutShort(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			return fmt.Errorf("git: read the sync-back's %s: %w", hdr.Name, err)
		}
		return nil
	}

	hdr, err := next()
	if err != nil {
		return SpooledSync{}, err
	}
	if hdr.Name != syncMetaEntry {
		return SpooledSync{}, fmt.Errorf("git: the sync-back starts with %q, not %s", hdr.Name, syncMetaEntry)
	}
	var meta SyncMeta
	if err := readJSON(hdr, &meta); err != nil {
		return SpooledSync{}, err
	}
	if meta.Format != 1 {
		return SpooledSync{}, fmt.Errorf("git: the sync-back is in format %d, which this build cannot read; update spov", meta.Format)
	}
	if onMeta != nil {
		onMeta(meta)
	}
	out := SpooledSync{Dir: dir, Branch: meta.Branch, BaseCommit: meta.BaseCommit, HeadCommit: meta.HeadCommit, Deleted: meta.Deleted}
	var total int64
	part := func(name string, dst *string) (int64, error) {
		hdr, err := next()
		if err != nil {
			return 0, err
		}
		if hdr.Name != name {
			return 0, fmt.Errorf("git: the sync-back holds %q where it should hold %s", hdr.Name, name)
		}
		if total += hdr.Size; total > MaxSyncExportBytes {
			return 0, fmt.Errorf("git: the sync-back is over the %d MiB a sync can send", MaxSyncExportBytes>>20)
		}
		*dst = filepath.Join(dir, name)
		f, err := os.OpenFile(*dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		n, copyErr := io.Copy(f, tr)
		closeErr := f.Close()
		if copyErr != nil {
			return 0, cutShort(copyErr)
		}
		if closeErr != nil {
			return 0, closeErr
		}
		return n, nil
	}
	var got syncEnd
	if meta.Bundle {
		if got.BundleBytes, err = part(syncBundleEntry, &out.BundlePath); err != nil {
			return SpooledSync{}, err
		}
	}
	if meta.Worktree {
		if got.WorktreeBytes, err = part(syncWorktreeEntry, &out.WorktreeTarPath); err != nil {
			return SpooledSync{}, err
		}
	}
	if hdr, err = next(); err != nil {
		return SpooledSync{}, err
	}
	if hdr.Name != syncEndEntry {
		return SpooledSync{}, fmt.Errorf("git: the sync-back holds %q where it should end", hdr.Name)
	}
	var end syncEnd
	if err := readJSON(hdr, &end); err != nil {
		return SpooledSync{}, err
	}
	if end != got {
		return SpooledSync{}, ErrSyncCutShort
	}
	return out, nil
}

// cutShort reads a stream that broke inside an entry as one cut short.
func cutShort(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || strings.Contains(err.Error(), "unexpected EOF") {
		return ErrSyncCutShort
	}
	return fmt.Errorf("git: read the sync-back: %w", err)
}
