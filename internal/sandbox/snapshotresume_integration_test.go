//go:build integration

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
// leak (DL-018); whether an exit snapshot can be had without asking
// for one at create; and for an exit snapshot: after terminate, the idle
// timeout and the hard timeout, with a sidecar (main container only),
// whether OpenCode's own store comes back, finding it later by sandbox
// ID, and booting it once its base image and an earlier generation are
// deleted. It creates a few small billable sandboxes and
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
	bootFor := func(t *testing.T, ctx context.Context, name string, image *modal.Image, idle, timeout time.Duration, options map[string]any) *modal.Sandbox {
		t.Helper()
		params, err := p.createParams(ctx, Spec{
			Name: name, Image: testImage(), Command: []string{"sleep", "infinity"},
			CPU: 0.25, MemoryMiB: 1024, Timeout: timeout, IdleTimeout: idle,
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
	bootWith := func(t *testing.T, ctx context.Context, name string, image *modal.Image, idle time.Duration, options map[string]any) *modal.Sandbox {
		t.Helper()
		return bootFor(t, ctx, name, image, idle, 20*time.Minute, options)
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
	exitSnapshot := func(t *testing.T, ctx context.Context, sb *modal.Sandbox, since time.Time) *modal.Image {
		t.Helper()
		wait := 5 * time.Minute
		img, err := sb.ExperimentalGetExitSnapshot(ctx, &modal.SandboxExperimentalGetExitSnapshotParams{Timeout: &wait})
		if err != nil {
			t.Fatalf("no exit snapshot: %v", err)
		}
		t.Logf("exit snapshot %s ready %s after the end", img.ImageID, time.Since(since).Round(100*time.Millisecond))
		t.Cleanup(func() {
			if err := p.client.Images.Delete(context.WithoutCancel(ctx), img.ImageID, nil); err != nil {
				t.Logf("cleanup delete image %s: %v", img.ImageID, err)
			}
		})
		return img
	}
	waitForExit := func(t *testing.T, ctx context.Context, sb *modal.Sandbox) time.Duration {
		t.Helper()
		start := time.Now()
		for {
			time.Sleep(5 * time.Second)
			code, err := sb.Poll(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if code != nil {
				return time.Since(start).Round(time.Second)
			}
		}
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
	// Resume reopens a session in a new sandbox from its exit snapshot.
	// Does OpenCode find its own conversation there, so no import is
	// needed? And can the snapshot be found later from the sandbox ID
	// alone, as a restarted server would?
	t.Run("OpenCode's own store after an exit snapshot", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		on := map[string]any{"enable_exit_snapshot": true}
		sb, creds, password := startOpenCodeSandboxOn(t, ctx, p, nil, "spov-ssr1-oc", 10*time.Minute, nil, nil, on)
		client := &http.Client{Timeout: 60 * time.Second}
		get := func(base, token, password, path string) string {
			t.Helper()
			resp, err := client.Do(sandboxHTTP(ctx, t, http.MethodGet, base+path, token, password))
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return resp.Status + " " + string(body)
		}
		id := createSession(ctx, t, client, creds.URL, creds.Token, password)
		prompt(ctx, t, client, creds.URL, creds.Token, password, id, "A message written before the exit snapshot.")
		time.Sleep(10 * time.Second)
		before := get(creds.URL, creds.Token, password, "/api/session/"+id+"/message")
		t.Logf("messages before: %d bytes, %d messages: %s", len(before), strings.Count(before, `"id":"msg_`), truncate(before, 400))
		t.Logf("what OpenCode keeps on disk:\n%s", run(t, ctx, sb, "du -sh /root/.local/share/opencode /root/.local/state/opencode /root/.cache/opencode 2>/dev/null; ls -la /root/.local/share/opencode 2>&1 | head -20"))
		t.Logf("what the sandbox wrote over the image (sizes):\n%s", run(t, ctx, sb, "du -xsh /root /tmp /workspace 2>/dev/null"))

		ended := time.Now()
		if _, err := sb.Terminate(ctx, nil); err != nil {
			t.Fatal(err)
		}
		later, err := p.client.Sandboxes.FromID(ctx, sb.SandboxID, nil)
		if err != nil {
			t.Fatalf("find the ended sandbox by ID: %v", err)
		}
		img := exitSnapshot(t, ctx, later, ended)
		restored, rcreds, rpassword := startOpenCodeSandboxOn(t, ctx, p, img, "spov-ssr1-oc-restored", 5*time.Minute, nil, nil, nil)
		listing := get(rcreds.URL, rcreds.Token, rpassword, "/api/session")
		after := get(rcreds.URL, rcreds.Token, rpassword, "/api/session/"+id+"/message")
		t.Logf("after restore: the listing has the conversation: %v; messages: %d bytes, %d messages", strings.Contains(listing, id), len(after), strings.Count(after, `"id":"msg_`))
		if !strings.Contains(listing, id) || after != before {
			t.Errorf("the restored OpenCode does not have the conversation as it was:\nlisting %s\nmessages %s", truncate(listing, 600), truncate(after, 600))
		}
		_ = restored
	})

	// The proxy sidecar is a second container: which filesystem does an
	// exit snapshot hold, and does the sidecar delay it?
	t.Run("an exit snapshot with a sidecar", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		sb := bootWith(t, ctx, fmt.Sprintf("spov-ssr1-sidecar-%d", stamp), base, 5*time.Minute, map[string]any{"enable_exit_snapshot": true})
		side, err := sb.ExperimentalSidecars.Create(ctx, "proxy", base, &modal.SidecarCreateParams{Command: []string{"sleep", "infinity"}})
		if err != nil {
			t.Fatalf("sidecar: %v", err)
		}
		if code, out, errOut := runIn(ctx, t, side, "mkdir -p /workspace && echo side > /workspace/sidecar.txt && echo side > /tmp/sidecar.txt", nil); code != 0 {
			t.Fatalf("sidecar write: %d %s%s", code, out, errOut)
		}
		run(t, ctx, sb, "mkdir -p /workspace/repo && echo main > /workspace/repo/main.txt && echo ok")
		ended := time.Now()
		if _, err := sb.Terminate(ctx, nil); err != nil {
			t.Fatal(err)
		}
		img := exitSnapshot(t, ctx, sb, ended)
		restored := boot(t, ctx, fmt.Sprintf("spov-ssr1-sidecar-restored-%d", stamp), img, 5*time.Minute)
		out := run(t, ctx, restored, "cat /workspace/repo/main.txt; cat /workspace/sidecar.txt /tmp/sidecar.txt 2>&1")
		t.Logf("a boot from a sidecar sandbox's exit snapshot sees:\n%s", out)
		if !strings.Contains(out, "main") {
			t.Errorf("the main container's work is not in the snapshot: %s", out)
		}
	})

	// A session's sandbox also ends at its hard timeout. Is an exit
	// snapshot taken then, and how long does a larger one take?
	t.Run("an exit snapshot at the hard timeout", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cancel()
		sb := bootFor(t, ctx, fmt.Sprintf("spov-ssr1-hard-%d", stamp), base, 10*time.Minute, 2*time.Minute, map[string]any{"enable_exit_snapshot": true})
		run(t, ctx, sb, "mkdir -p /workspace/repo && echo hard > /workspace/repo/dirty.txt && "+
			"dd if=/dev/urandom of=/workspace/big bs=1M count=300 status=none && sync && echo ok")
		t.Logf("ended by its timeout after %s more", waitForExit(t, ctx, sb))
		img := exitSnapshot(t, ctx, sb, time.Now())
		restored := boot(t, ctx, fmt.Sprintf("spov-ssr1-hard-restored-%d", stamp), img, 5*time.Minute)
		if out := run(t, ctx, restored, "cat /workspace/repo/dirty.txt; stat -c %s /workspace/big"); !strings.Contains(out, "hard") || !strings.Contains(out, "314572800") {
			t.Errorf("the hard-timeout snapshot lost the workspace: %s", out)
		}
	})
	// DL-016's pruning deletes variant images, and a resumed session is
	// snapshotted again at its end. Does a snapshot still boot once its
	// base image is deleted, and does a snapshot of a restored sandbox
	// keep both generations' work?
	t.Run("a snapshot outlives its base, and chains", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		own, err := base.DockerfileCommands([]string{fmt.Sprintf("RUN echo %d > /spov-ssr1-base", stamp)}, nil).Build(ctx, p.app, nil)
		if err != nil {
			t.Fatalf("build a throwaway base: %v", err)
		}
		on := map[string]any{"enable_exit_snapshot": true}
		first := bootWith(t, ctx, fmt.Sprintf("spov-ssr1-chain1-%d", stamp), own, 5*time.Minute, on)
		run(t, ctx, first, "mkdir -p /workspace/repo && echo one > /workspace/repo/one.txt && echo ok")
		ended := time.Now()
		if _, err := first.Terminate(ctx, nil); err != nil {
			t.Fatal(err)
		}
		snap1 := exitSnapshot(t, ctx, first, ended)
		if err := p.client.Images.Delete(ctx, own.ImageID, nil); err != nil {
			t.Fatalf("delete the base: %v", err)
		}
		t.Logf("deleted the base %s", own.ImageID)
		second := bootWith(t, ctx, fmt.Sprintf("spov-ssr1-chain2-%d", stamp), snap1, 5*time.Minute, on)
		out := run(t, ctx, second, "cat /spov-ssr1-base /workspace/repo/one.txt; echo two > /workspace/repo/two.txt")
		t.Logf("a boot from the snapshot of a deleted base sees: %s", out)
		ended = time.Now()
		if _, err := second.Terminate(ctx, nil); err != nil {
			t.Fatal(err)
		}
		snap2 := exitSnapshot(t, ctx, second, ended)
		if err := p.client.Images.Delete(ctx, snap1.ImageID, nil); err != nil {
			t.Logf("delete the first snapshot: %v", err)
		}
		start := time.Now()
		third := boot(t, ctx, fmt.Sprintf("spov-ssr1-chain3-%d", stamp), snap2, 5*time.Minute)
		out = run(t, ctx, third, "cat /spov-ssr1-base /workspace/repo/one.txt /workspace/repo/two.txt")
		t.Logf("a boot from the second generation, with the first deleted, in %s, sees:\n%s", time.Since(start).Round(100*time.Millisecond), out)
		if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
			t.Errorf("the chain lost work: %s", out)
		}
	})
}
