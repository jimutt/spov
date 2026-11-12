package safego

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestAPanicInABackgroundGoroutineIsLoggedAndSurvived: the report names
// the goroutine, the value and the stack.
//
// @decision(DL-135)
func TestAPanicInABackgroundGoroutineIsLoggedAndSurvived(t *testing.T) {
	var got []string
	panicked := Do("the recorder", func(m string) { got = append(got, m) }, func() { panic("boom") })
	if !panicked {
		t.Fatal("a panic was not reported")
	}
	if len(got) != 1 || !strings.Contains(got[0], "the recorder") || !strings.Contains(got[0], "boom") ||
		!strings.Contains(got[0], "safego_test.go") {
		t.Errorf("the report = %q, want the name, the value and the stack", got)
	}
	if Do("quiet", nil, func() {}) {
		t.Error("a function that returned was reported as a panic")
	}
	if !Do("no warn", nil, func() { panic("x") }) {
		t.Error("a nil warn lost the panic")
	}
}

// TestALoopThatPanicsRunsAgainUntilItsContextEnds: the server's loops
// keep running after a panic, and a loop that returns is not restarted.
//
// @decision(DL-135)
func TestALoopThatPanicsRunsAgainUntilItsContextEnds(t *testing.T) {
	was := backoffStart
	backoffStart = time.Millisecond
	t.Cleanup(func() { backoffStart = was })

	runs, warned := 0, 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		Loop(context.Background(), "renewals", func(string) { warned++ }, func() {
			runs++
			if runs < 3 {
				panic("again")
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not finish after it stopped panicking")
	}
	if runs != 3 || warned != 2 {
		t.Errorf("runs = %d and warnings = %d, want 3 and 2", runs, warned)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runs = 0
	Loop(ctx, "ended", nil, func() { runs++; panic("x") })
	if runs != 1 {
		t.Errorf("a loop under an ended context ran %d times, want once", runs)
	}
}
