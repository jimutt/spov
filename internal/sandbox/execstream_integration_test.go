//go:build integration

package sandbox

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// countingWriter counts what reaches it, and fails once it has seen
// stopAfter bytes when that is set.
type countingWriter struct {
	n         atomic.Int64
	stopAfter int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.stopAfter > 0 && c.n.Load() >= c.stopAfter {
		return 0, errors.New("the reader went away")
	}
	c.n.Add(int64(len(p)))
	return len(p), nil
}

// TestIntegrationExecStream measures DL-175's streaming exec on Modal:
// a gigabyte through with the heap flat, a reader that stops partway,
// and a stream that outlasts the sandbox's idle timeout. Re-run it on a
// Modal SDK bump and record the figures in DL-175.
//
//	go test -tags integration ./internal/sandbox/ -run ExecStream -v -timeout 30m
//
// @decision(DL-175)
func TestIntegrationExecStream(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	create := func(idle time.Duration) Handle {
		sb, err := p.Create(ctx, Spec{
			Image:       testImage(),
			Command:     []string{"sleep", "infinity"},
			Ports:       []int{testPort},
			CPU:         1,
			MemoryMiB:   2048,
			Timeout:     30 * time.Minute,
			IdleTimeout: idle,
			Tags:        BuildTags(TagSet{User: "integration-test", CreatedAt: time.Now()}),
			Egress:      EgressOpen(),
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = sb.Terminate(context.WithoutCancel(ctx)) })
		return sb
	}
	sb := create(10 * time.Minute)

	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}

	t.Run("a gigabyte reaches the writer with the heap flat", func(t *testing.T) {
		execCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		const size = 1 << 30
		w := &countingWriter{}
		before := heap()
		start := time.Now()
		res, err := sb.ExecStream(execCtx, []string{"bash", "-c", "head -c 1073741824 /dev/urandom"}, w, size)
		took := time.Since(start)
		grew := int64(heap()) - int64(before)
		if err != nil {
			t.Fatalf("ExecStream after %d bytes in %s: %v", w.n.Load(), took.Round(time.Second), err)
		}
		t.Logf("1 GiB in %s (%.1f MiB/s), exit %d, heap %+d KiB",
			took.Round(100*time.Millisecond), float64(size)/(1<<20)/took.Seconds(), res.ExitCode, grew/1024)
		if w.n.Load() != size || res.ExitCode != 0 {
			t.Errorf("wrote %d bytes, exit %d", w.n.Load(), res.ExitCode)
		}
		if grew > 16<<20 {
			t.Errorf("the heap grew %d bytes over a streamed exec", grew)
		}
	})

	t.Run("over the bound is refused", func(t *testing.T) {
		execCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		w := &countingWriter{}
		_, err := sb.ExecStream(execCtx, []string{"bash", "-c", "head -c 2000000 /dev/zero"}, w, 1_000_000)
		if !errors.Is(err, ErrOutputTooLarge) || w.n.Load() != 1_000_000 {
			t.Errorf("err = %v after %d bytes, want ErrOutputTooLarge after 1000000", err, w.n.Load())
		}
	})

	t.Run("a reader that stops leaves the program to its deadline", func(t *testing.T) {
		execCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		w := &countingWriter{stopAfter: 1 << 20}
		script := "rm -f /tmp/stream-done; head -c 268435456 /dev/zero; touch /tmp/stream-done; sleep 120; touch /tmp/stream-late"
		_, err := sb.ExecStream(execCtx, []string{"bash", "-c", script}, w, 1<<30)
		if err == nil {
			t.Fatal("a failed writer answered no error")
		}
		time.Sleep(70 * time.Second)
		check, cerr := sb.Exec(ctx, []string{"bash", "-c", "ls /tmp/stream-done /tmp/stream-late 2>&1; pgrep -f 'sleep 120' || echo no-sleeper"})
		t.Logf("after the reader stopped (%v): %q, %v", err, check.Stdout, cerr)
	})

	t.Run("a fast stream under the shortest idle timeout lives with a short exec every 30 seconds", func(t *testing.T) {
		sb := create(MinIdleTimeout)
		execCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		awake, stop := context.WithCancel(execCtx)
		go func() {
			tick := time.NewTicker(30 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-awake.Done():
					return
				case <-tick.C:
					c, cc := context.WithTimeout(awake, 10*time.Second)
					_, _ = sb.Exec(c, []string{"true"})
					cc()
				}
			}
		}()
		w := &countingWriter{}
		start := time.Now()
		_, err := sb.ExecStream(execCtx, []string{"bash", "-c", "head -c 1073741824 /dev/urandom"}, w, 1<<30)
		stop()
		t.Logf("1 GiB under a %s idle timeout with keep-awake execs: %d bytes in %s, err %v",
			MinIdleTimeout, w.n.Load(), time.Since(start).Round(time.Second), err)
		if err != nil || w.n.Load() != 1<<30 {
			t.Errorf("the stream ended early: %v", err)
		}
	})

	t.Run("a stream outlasting the idle timeout keeps the sandbox", func(t *testing.T) {
		sb := create(MinIdleTimeout)
		execCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		defer cancel()
		w := &countingWriter{}
		start := time.Now()
		res, err := sb.ExecStream(execCtx, []string{"bash", "-c", "for i in $(seq 36); do echo tick; sleep 5; done"}, w, 1<<20)
		t.Logf("a %s stream with a %s idle timeout: exit %d, %d bytes, err %v",
			time.Since(start).Round(time.Second), MinIdleTimeout, res.ExitCode, w.n.Load(), err)
		if err != nil {
			t.Fatalf("the stream ended: %v", err)
		}
		if _, err := sb.Exec(ctx, []string{"true"}); err != nil {
			t.Errorf("the sandbox is gone after the stream: %v", err)
		}
	})
}
