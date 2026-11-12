// Package safego runs the server's background goroutines so a panic in
// one is logged and survived instead of ending the process for every
// user. net/http recovers a handler's panic and River its workers'; the
// goroutines spov starts itself have no one else to.
//
// @decision(DL-135)
package safego

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"
)

// Do runs fn and recovers a panic, reporting its value and stack through
// warn. It reports whether fn panicked. A nil warn discards the report.
//
// @decision(DL-135)
func Do(name string, warn func(string), fn func()) (panicked bool) {
	defer func() {
		if v := recover(); v != nil {
			panicked = true
			if warn != nil {
				warn(fmt.Sprintf("%s: recovered from a panic: %v\n%s", name, v, debug.Stack()))
			}
		}
	}()
	fn()
	return false
}

// Restart backoff bounds. A loop that keeps panicking is run again after
// a wait that doubles to the maximum, so it never spins.
//
// @decision(DL-135)
var (
	backoffStart = time.Second
	backoffMax   = time.Minute
)

// Loop runs fn, a loop that returns when its context ends, until it
// returns without panicking. After a panic it waits and runs fn again,
// for as long as ctx lives, since each of the server's loops starts from
// the store's state and not from anything the panic could have left half
// done.
//
// @decision(DL-135)
func Loop(ctx context.Context, name string, warn func(string), fn func()) {
	wait := backoffStart
	for {
		started := time.Now()
		if !Do(name, warn, fn) {
			return
		}
		if time.Since(started) > 2*backoffMax {
			wait = backoffStart
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, backoffMax)
	}
}
