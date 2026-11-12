//go:build integration

package sandbox

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// TestIntegrationExecOutputBound measures what a program that writes far
// more than its bound costs the caller: how long the exec takes, how much
// memory the process gains and whether a goroutine is left reading.
// Re-run it on a Modal SDK bump.
//
//	go test -tags integration ./internal/sandbox/ -run ExecOutputBound -v
//
// @decision(DL-130)
func TestIntegrationExecOutputBound(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	sb, err := p.Create(ctx, Spec{
		Image:       testImage(),
		Command:     []string{"sleep", "infinity"},
		Ports:       []int{testPort},
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     10 * time.Minute,
		IdleTimeout: 2 * time.Minute,
		Tags:        BuildTags(TagSet{User: "integration-test", CreatedAt: time.Now()}),
		Egress:      EgressOpen(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = sb.Terminate(context.WithoutCancel(ctx)) })

	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}

	t.Run("output within the bound is returned", func(t *testing.T) {
		res, err := sb.Exec(ctx, []string{"bash", "-c", "head -c 60000 /dev/zero | tr '\\0' a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Stdout) != 60000 {
			t.Errorf("read %d bytes, want 60000", len(res.Stdout))
		}
	})

	for _, c := range []struct {
		name    string
		command string
	}{
		{"512 MiB on stdout", "head -c 536870912 /dev/zero"},
		{"512 MiB on stderr", "head -c 536870912 /dev/zero >&2"},
		{"a program that never stops writing", "yes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			execCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			beforeHeap, beforeG := heap(), runtime.NumGoroutine()
			start := time.Now()
			_, err := sb.Exec(execCtx, []string{"bash", "-c", c.command})
			took := time.Since(start).Round(100 * time.Millisecond)
			time.Sleep(2 * time.Second)
			afterHeap, afterG := heap(), runtime.NumGoroutine()
			t.Logf("took %s, err = %v, heap %+d KiB, goroutines %d -> %d",
				took, err, (int64(afterHeap)-int64(beforeHeap))/1024, beforeG, afterG)
			if err == nil {
				t.Fatal("the exec answered with no error")
			}
			if !errors.Is(err, ErrOutputTooLarge) {
				t.Logf("not ErrOutputTooLarge: the exec ended another way")
			}
			if grew := int64(afterHeap) - int64(beforeHeap); grew > 8<<20 {
				t.Errorf("the heap grew %d bytes over an exec bounded at %d", grew, MaxExecOutput)
			}
			if afterG > beforeG+2 {
				t.Errorf("goroutines %d -> %d: something is still reading", beforeG, afterG)
			}
		})
	}

	t.Run("the sandbox still answers", func(t *testing.T) {
		res, err := sb.Exec(ctx, []string{"echo", "ok"})
		if err != nil || res.Stdout != "ok\n" {
			t.Fatalf("exec after the floods: %q, %v", res.Stdout, err)
		}
	})
}
