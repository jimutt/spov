//go:build integration

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// Spike S-SR1 answers what snapshot resume would rest on, before any
// decision: how long a filesystem snapshot of a working sandbox takes,
// and a boot from it; what survives (files in the workspace, /tmp, a
// large file, a running process); whether taking snapshots keeps an
// idle sandbox alive, which would make a snapshot schedule a billing
// leak (DL-018); and whether an exit snapshot can be had without asking
// for one at create. It creates a few small billable sandboxes and
// terminates them, and deletes the images it makes.
//
//	SPOV_SNAPSHOT_SPIKE=1 go test -tags integration ./internal/sandbox/ -run SSR1 -v -timeout 30m
func TestIntegrationSSR1SnapshotResume(t *testing.T) {
	if testing.Short() || os.Getenv("SPOV_SNAPSHOT_SPIKE") == "" {
		t.Skip("set SPOV_SNAPSHOT_SPIKE=1 to create billable sandboxes")
	}
	p := newIntegrationProvider(t)
	base, err := p.image(context.Background(), testImage())
	if err != nil {
		t.Fatal(err)
	}
	bootWith := func(t *testing.T, ctx context.Context, name string, image *modal.Image, idle time.Duration, options map[string]any) *modal.Sandbox {
		t.Helper()
		params, err := p.createParams(ctx, Spec{
			Name: name, Image: testImage(), Command: []string{"sleep", "infinity"},
			CPU: 0.25, MemoryMiB: 1024, Timeout: 20 * time.Minute, IdleTimeout: idle,
			Tags:   BuildTags(TagSet{Name: name, User: "integration-test", CreatedAt: time.Now()}),
			Egress: EgressOpen(),
		})
		if err != nil {
			t.Fatal(err)
		}
		params.ExperimentalOptions = options
		sb, err := p.client.Sandboxes.ExperimentalCreate(ctx, p.app, image, params)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			if _, err := sb.Terminate(context.WithoutCancel(ctx), nil); err != nil {
				t.Logf("cleanup terminate %s: %v", name, err)
			}
		})
		return sb
	}
	boot := func(t *testing.T, ctx context.Context, name string, image *modal.Image, idle time.Duration) *modal.Sandbox {
		t.Helper()
		return bootWith(t, ctx, name, image, idle, nil)
	}
	run := func(t *testing.T, ctx context.Context, sb *modal.Sandbox, script string) string {
		t.Helper()
		res, err := (&modalHandle{sb: sb}).Exec(ctx, []string{"sh", "-c", script})
		if err != nil {
			t.Fatalf("exec %q: %v", script, err)
		}
		return strings.TrimSpace(res.Stdout + res.Stderr)
	}
	snapshot := func(t *testing.T, ctx context.Context, sb *modal.Sandbox) (*modal.Image, time.Duration) {
		t.Helper()
		start := time.Now()
		img, err := sb.SnapshotFilesystem(ctx, &modal.SandboxSnapshotFilesystemParams{TTL: time.Hour, Timeout: 5 * time.Minute})
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		took := time.Since(start).Round(100 * time.Millisecond)
		t.Cleanup(func() {
			if err := p.client.Images.Delete(context.WithoutCancel(ctx), img.ImageID, nil); err != nil {
				t.Logf("cleanup delete image %s: %v", img.ImageID, err)
			}
		})
		return img, took
	}
	stamp := time.Now().Unix()

	t.Run("what survives, and how long it takes", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		src := boot(t, ctx, fmt.Sprintf("spov-ssr1-src-%d", stamp), base, 10*time.Minute)
		run(t, ctx, src, "mkdir -p /workspace/repo && echo marker > /workspace/repo/dirty.txt && echo tmp > /tmp/scratch && "+
			"(nohup sleep 3600 >/dev/null 2>&1 &) && echo ok")
		small, tookSmall := snapshot(t, ctx, src)
		t.Logf("snapshot with a few files: %s (%s)", tookSmall, small.ImageID)
		run(t, ctx, src, "dd if=/dev/urandom of=/workspace/big bs=1M count=300 status=none && sync && echo ok")
		big, tookBig := snapshot(t, ctx, src)
		t.Logf("snapshot after writing 300 MiB: %s (%s)", tookBig, big.ImageID)
		again, tookAgain := snapshot(t, ctx, src)
		t.Logf("a second snapshot with nothing changed: %s (%s)", tookAgain, again.ImageID)
		if out := run(t, ctx, src, "cat /workspace/repo/dirty.txt"); out != "marker" {
			t.Errorf("the source lost its file after being snapshotted: %q", out)
		}
		if _, err := src.Terminate(ctx, nil); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		restored := boot(t, ctx, fmt.Sprintf("spov-ssr1-restored-%d", stamp), big, 10*time.Minute)
		out := run(t, ctx, restored, "cat /workspace/repo/dirty.txt; cat /tmp/scratch 2>&1; stat -c %s /workspace/big; "+
			"pgrep -f 'sleep 3600' >/dev/null && echo process-survived || echo process-gone")
		t.Logf("boot from the snapshot to a first exec: %s; it sees:\n%s", time.Since(start).Round(100*time.Millisecond), out)
		if !strings.Contains(out, "marker") || !strings.Contains(out, "314572800") {
			t.Errorf("the restored sandbox lost the workspace: %s", out)
		}
	})

	t.Run("do snapshots keep an idle sandbox alive", func(t *testing.T) {
		t.Parallel()
		const idle = 2 * time.Minute
		observe := idle*2 + 30*time.Second
		ctx, cancel := context.WithTimeout(context.Background(), observe+5*time.Minute)
		defer cancel()
		sb := boot(t, ctx, fmt.Sprintf("spov-ssr1-idle-%d", stamp), base, idle)
		start := time.Now()
		next := 50 * time.Second
		for time.Since(start) < observe {
			time.Sleep(10 * time.Second)
			code, err := sb.Poll(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if code != nil {
				t.Logf("exited after %s, taking a snapshot every 50s: snapshots do not count as activity", time.Since(start).Round(time.Second))
				return
			}
			if time.Since(start) >= next {
				next += 50 * time.Second
				if _, took := snapshot(t, ctx, sb); took > 0 {
					t.Logf("snapshot at %s took %s", time.Since(start).Round(time.Second), took)
				}
			}
		}
		t.Logf("alive after %s with IdleTimeout %s and a snapshot every 50s: snapshots count as activity, so a schedule keeps a sandbox billing", observe, idle)
	})

	// An exit snapshot is the one that can outlive a sandbox nobody was
	// watching: does Modal take it when the idle timeout ends the
	// sandbox, and when spov terminates it, and does a boot from it see
	// the work?
	for _, end := range []string{"idle timeout", "terminate"} {
		t.Run("an exit snapshot after "+end, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			sb := bootWith(t, ctx, fmt.Sprintf("spov-ssr1-exit-%s-%d", strings.ReplaceAll(end, " ", ""), stamp), base, time.Minute,
				map[string]any{"enable_exit_snapshot": true})
			run(t, ctx, sb, "mkdir -p /workspace/repo && echo exit-marker > /workspace/repo/dirty.txt && echo ok")
			start := time.Now()
			if end == "terminate" {
				if _, err := sb.Terminate(ctx, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				for {
					time.Sleep(10 * time.Second)
					code, err := sb.Poll(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					if code != nil {
						t.Logf("idled out after %s", time.Since(start).Round(time.Second))
						start = time.Now()
						break
					}
				}
			}
			wait := 5 * time.Minute
			img, err := sb.ExperimentalGetExitSnapshot(ctx, &modal.SandboxExperimentalGetExitSnapshotParams{Timeout: &wait})
			if err != nil {
				t.Fatalf("no exit snapshot after %s: %v", end, err)
			}
			t.Logf("exit snapshot after %s: %s, ready %s after the end", end, img.ImageID, time.Since(start).Round(100*time.Millisecond))
			t.Cleanup(func() {
				if err := p.client.Images.Delete(context.WithoutCancel(ctx), img.ImageID, nil); err != nil {
					t.Logf("cleanup delete image %s: %v", img.ImageID, err)
				}
			})
			restored := boot(t, ctx, fmt.Sprintf("spov-ssr1-exit-restored-%s-%d", strings.ReplaceAll(end, " ", ""), stamp), img, 5*time.Minute)
			if out := run(t, ctx, restored, "cat /workspace/repo/dirty.txt"); out != "exit-marker" {
				t.Errorf("the exit snapshot lost the workspace: %q", out)
			}
		})
	}

	t.Run("an exit snapshot without asking for one", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		sb := boot(t, ctx, fmt.Sprintf("spov-ssr1-exit-%d", stamp), base, 5*time.Minute)
		zero := time.Duration(0)
		_, err := sb.ExperimentalGetExitSnapshot(ctx, &modal.SandboxExperimentalGetExitSnapshotParams{Timeout: &zero})
		var invalid modal.InvalidError
		t.Logf("exit snapshot on a sandbox created without one: %v (invalid: %v)", err, errors.As(err, &invalid))
	})
}
