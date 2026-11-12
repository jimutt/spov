package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func changeSet(res SyncResult) map[string]byte {
	out := map[string]byte{}
	for _, c := range res.Changes {
		out[c.Path] = c.Status
	}
	return out
}

// @decision(DL-136)
func TestASyncResultNamesEveryPathItChanged(t *testing.T) {
	ctx := context.Background()
	t.Run("in place", func(t *testing.T) {
		dir, _ := initTestRepo(t, "https://codefloe.com/alice/spov.git")
		if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "old.txt")
		runGit(t, dir, "commit", "-q", "-m", "old")
		head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
		_, sandboxHead, bundle := sandboxCommit(t, dir, head, "committed.txt")
		res, err := ApplySync(ctx, dir, "s", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: sandboxHead, Bundle: bundle,
			WorktreeTar: makeWorktreeTar(t, map[string]string{"README.md": "edited\n", "fresh.txt": "new\n"}),
			Deleted:     []string{"old.txt"},
		})
		if err != nil || !res.InPlace {
			t.Fatalf("result = %+v, %v", res, err)
		}
		want := map[string]byte{"committed.txt": 'A', "README.md": 'M', "fresh.txt": 'A', "old.txt": 'D'}
		got := changeSet(res)
		if len(got) != len(want) {
			t.Fatalf("changes = %v, want %v", got, want)
		}
		for p, st := range want {
			if got[p] != st {
				t.Errorf("%s = %c, want %c (all: %v)", p, got[p], st, got)
			}
		}
		if res.Commits != 1 || res.RangeFrom != head[:12] || res.RangeTo != sandboxHead[:12] {
			t.Errorf("range = %s..%s (%d), want %s..%s", res.RangeFrom, res.RangeTo, res.Commits, head[:12], sandboxHead[:12])
		}
		if res.Changes[0].Path != "README.md" {
			t.Errorf("changes are not sorted by path: %v", res.Changes)
		}
	})
	t.Run("on a branch", func(t *testing.T) {
		dir, head := initTestRepo(t, "https://codefloe.com/alice/spov.git")
		_, sandboxHead, bundle := sandboxCommit(t, dir, head, "committed.txt")
		if err := os.WriteFile(filepath.Join(dir, "local.txt"), []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := ApplySync(ctx, dir, "s", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: sandboxHead, Bundle: bundle,
			WorktreeTar: makeWorktreeTar(t, map[string]string{"fresh.txt": "new\n"}),
		})
		if err != nil || res.InPlace {
			t.Fatalf("result = %+v, %v", res, err)
		}
		got := changeSet(res)
		if got["committed.txt"] != 'A' || got["fresh.txt"] != 'A' || len(got) != 2 {
			t.Errorf("changes = %v", got)
		}
		if res.RangeFrom != head[:12] || res.RangeTo != sandboxHead[:12] {
			t.Errorf("range = %s..%s", res.RangeFrom, res.RangeTo)
		}
	})
	t.Run("nothing committed has no range", func(t *testing.T) {
		dir, head := initTestRepo(t, "https://codefloe.com/alice/spov.git")
		res, err := ApplySync(ctx, dir, "s", "main", ExportedState{
			Branch: "main", BaseCommit: head, HeadCommit: head,
			WorktreeTar: makeWorktreeTar(t, map[string]string{"fresh.txt": "new\n"}),
		})
		if err != nil || !res.InPlace {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if res.RangeFrom != "" || res.RangeTo != "" || changeSet(res)["fresh.txt"] != 'A' {
			t.Errorf("result = %+v", res)
		}
	})
}
