package postgres

import (
	"testing"
	"time"
)

// TestLeaderDefaults: an unconfigured Leader must still use the key and
// interval the decision names, since that is what makes two replicas
// contend for the same lock.
//
// @decision(DL-004)
func TestLeaderDefaults(t *testing.T) {
	var l Leader
	if l.key() != ReconcilerLockKey {
		t.Errorf("key = %q, want %q", l.key(), ReconcilerLockKey)
	}
	if l.retry() != LeaderRetryInterval {
		t.Errorf("retry = %v, want %v", l.retry(), LeaderRetryInterval)
	}
	custom := Leader{Key: "other", Retry: time.Second}
	if custom.key() != "other" || custom.retry() != time.Second {
		t.Errorf("overrides ignored: %q %v", custom.key(), custom.retry())
	}
	// A negative interval would busy-loop, so it falls back too.
	bad := Leader{Retry: -time.Second}
	if bad.retry() != LeaderRetryInterval {
		t.Errorf("a negative retry gave %v", bad.retry())
	}
}

// TestLeaderHeldIsFalseUntilTaken.
//
// @decision(DL-004)
func TestLeaderHeldIsFalseUntilTaken(t *testing.T) {
	var l Leader
	if l.Held() {
		t.Error("a leader that never connected reports the lock held")
	}
	l.setHeld(true)
	if !l.Held() {
		t.Error("setHeld(true) did not take")
	}
	l.setHeld(false)
	if l.Held() {
		t.Error("setHeld(false) did not take")
	}
}

// TestLeaderWarnIsOptional: a nil Warn must not panic, since the server
// wires one and tests do not.
//
// @decision(DL-004)
func TestLeaderWarnIsOptional(t *testing.T) {
	var l Leader
	l.warn("no sink for %s", "this")

	var got string
	withSink := Leader{Warn: func(s string) { got = s }}
	withSink.warn("lost the lock: %v", "boom")
	if got != "lost the lock: boom" {
		t.Errorf("warn = %q", got)
	}
}

// TestLeaderCheckIntervalStaysBelowTheRetry: re-asserting ownership must
// not be gated on the far slower failover schedule, or a lost lock goes
// unnoticed for a whole retry period.
//
// @decision(DL-004)
func TestLeaderCheckIntervalStaysBelowTheRetry(t *testing.T) {
	var def Leader
	if def.checkInterval() != LeaderCheckInterval {
		t.Errorf("check interval = %v, want %v", def.checkInterval(), LeaderCheckInterval)
	}
	if LeaderCheckInterval >= LeaderRetryInterval {
		t.Errorf("the check interval %v is not below the retry interval %v",
			LeaderCheckInterval, LeaderRetryInterval)
	}
	// A test that sets a very short retry must not get a slower check.
	fast := Leader{Retry: 200 * time.Millisecond}
	if fast.checkInterval() != 200*time.Millisecond {
		t.Errorf("a %v retry gave a %v check", fast.Retry, fast.checkInterval())
	}
}
