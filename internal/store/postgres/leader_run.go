package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Run takes the lock and calls work with a context that ends when the
// lock is lost or ctx does. It keeps retrying until ctx ends, so a
// replica that starts as a follower becomes leader when the holder dies.
//
// @decision(DL-004)
func (l *Leader) Run(ctx context.Context, work func(context.Context)) {
	for ctx.Err() == nil {
		got, err := l.runOnce(ctx, work)
		if err != nil && ctx.Err() == nil {
			l.warn("leader: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
		// Held-and-lost is retried on the same schedule as never-held;
		// there is no benefit to reconnecting faster than a follower.
		_ = got
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.retry()):
		}
	}
}

// runOnce tries to take the lock once. When it succeeds it runs work
// until the connection dies or ctx ends, then releases.
func (l *Leader) runOnce(ctx context.Context, work func(context.Context)) (bool, error) {
	conn, err := pgx.Connect(ctx, l.URL)
	if err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	defer func() {
		// Closing the session releases the lock, so this is also the
		// release path when work returns.
		_ = conn.Close(context.WithoutCancel(ctx))
		l.setHeld(false)
	}()

	var got bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock(hashtext($1))", l.key()).Scan(&got); err != nil {
		return false, fmt.Errorf("take lock: %w", err)
	}
	if !got {
		return false, nil
	}
	l.setHeld(true)

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		work(workCtx)
	}()

	// A dropped connection releases the lock server-side without telling
	// us, and the lock can also be released out from under us
	// (pg_advisory_unlock_all, an admin), so ownership is re-asserted
	// rather than assumed. Checking is cheap, so it runs on its own
	// schedule rather than the far slower retry interval.
	ticker := time.NewTicker(l.checkInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return true, nil
		case <-done:
			return true, nil
		case <-ticker.C:
			held, err := l.stillHeld(ctx, conn)
			if err == nil && held {
				continue
			}
			// Held goes false before the work is torn down, so nothing
			// observes a leader that is still claiming a lock it lost.
			l.setHeld(false)
			cancel()
			<-done
			if err != nil {
				return true, fmt.Errorf("lost the lock: %w", err)
			}
			return true, fmt.Errorf("the advisory lock is no longer held")
		}
	}
}

// stillHeld asks whether this connection still owns the lock, rather
// than merely whether the socket is alive: a Ping succeeds on a
// connection whose lock someone else released.
//
// @decision(DL-004)
func (l *Leader) stillHeld(ctx context.Context, conn *pgx.Conn) (bool, error) {
	var held bool
	err := conn.QueryRow(ctx, `select exists (
		select 1 from pg_locks
		where locktype = 'advisory'
		  and pid = pg_backend_pid()
		  and objid = hashtext($1)::bigint & 4294967295
		  and granted)`, l.key()).Scan(&held)
	if err != nil {
		return false, err
	}
	return held, nil
}
