package sandbox

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestFakeConcurrentUse guards the fake against deadlock and data races.
// It is shared test infrastructure for later slices, so tests that use it
// must not fail for reasons of its own.
func TestFakeConcurrentUse(t *testing.T) {
	f := NewFake()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			spec := validSpec()
			spec.Tags = BuildTags(TagSet{User: "u", CreatedAt: time.Now()})
			h, err := f.Create(context.Background(), spec)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := h.Info(context.Background()); err != nil {
				t.Error(err)
			}
			if _, err := h.Tunnels(context.Background()); err != nil {
				t.Error(err)
			}
			if _, err := f.List(context.Background(), OwnedTags()); err != nil {
				t.Error(err)
			}
			if _, err := h.Exec(context.Background(), []string{"echo", "x"}); err != nil {
				t.Error(err)
			}
			_ = f.Running()
			if i%2 == 0 {
				if err := h.Terminate(context.Background()); err != nil {
					t.Error(err)
				}
			}
			// Recorded state must be readable while other goroutines are
			// still using the fake.
			fh := h.(*FakeHandle)
			_ = fh.WasTerminated()
			_ = fh.WasDetached()
			_ = fh.ExecLog()
		}(i)
	}
	wg.Wait()
}
