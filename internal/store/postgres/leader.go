package postgres

import (
	"fmt"
	"sync"
	"time"
)

// ReconcilerLockKey names the advisory lock the reconciler leader holds.
// One lock, one leader, for the whole deployment.
//
// @decision(DL-004)
const ReconcilerLockKey = "spov.reconciler"

// LeaderRetryInterval is how often a follower tries to take over.
//
// @decision(DL-004)
const LeaderRetryInterval = 30 * time.Second

// LeaderCheckInterval is how often the holder re-asserts its ownership.
//
// @decision(DL-004)
const LeaderCheckInterval = 5 * time.Second

// Leader holds a Postgres advisory lock for as long as its connection
// lives, so exactly one replica runs background work. The lock is
// session-scoped, which is why it needs its own connection rather than a
// pooled one: returning a pooled connection would release it.
//
// @decision(DL-004)
type Leader struct {
	// URL is the database to connect to.
	URL string
	// Key names the lock; empty means ReconcilerLockKey.
	Key string
	// Retry is how often to try again after losing or failing to take
	// the lock; zero means LeaderRetryInterval.
	Retry time.Duration
	// Warn receives one line per problem; nil discards.
	Warn func(string)

	mu   sync.Mutex
	held bool
}

func (l *Leader) key() string {
	if l.Key != "" {
		return l.Key
	}
	return ReconcilerLockKey
}

func (l *Leader) retry() time.Duration {
	if l.Retry > 0 {
		return l.Retry
	}
	return LeaderRetryInterval
}

// checkInterval is how often the holder re-asserts ownership. It is
// bounded well below the retry interval so a lost lock is noticed
// promptly rather than after a full failover period.
//
// @decision(DL-004)
func (l *Leader) checkInterval() time.Duration {
	if d := l.retry(); d < LeaderCheckInterval {
		return d
	}
	return LeaderCheckInterval
}

func (l *Leader) warn(format string, args ...any) {
	if l.Warn != nil {
		l.Warn(fmt.Sprintf(format, args...))
	}
}

// Held reports whether this replica currently holds the lock.
func (l *Leader) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

func (l *Leader) setHeld(v bool) {
	l.mu.Lock()
	l.held = v
	l.mu.Unlock()
}
