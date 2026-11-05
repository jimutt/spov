package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// attachPoll is how often attach re-reads the session it waits on. Both
// reads answer from the server's store.
//
// @decision(DL-025) @decision(DL-018)
const attachPoll = time.Second

// awaitJoinable returns the session once it can be joined: running, and
// for a job's session, the job running and its conversation recorded. For
// a job it also returns that conversation, so attach need not read the
// transcript again. It prints a line each time what it waits on changes,
// and ends early when the session or its job ends, a read fails, the
// wait times out or the context is cancelled. Ending the wait never stops
// the session or the job.
//
// @decision(DL-025)
func awaitJoinable(ctx context.Context, env *Env, c *client.Client, view server.SessionView, opts attachOptions) (server.SessionView, string, error) {
	wctx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	poll := env.AttachPoll
	if poll <= 0 {
		poll = attachPoll
	}
	last := ""
	for {
		wait, err := joinable(view)
		if err != nil {
			return view, "", err
		}
		conv := ""
		if wait == "" && view.Job != nil {
			convs, err := c.Conversations(wctx, view.ID)
			if err != nil {
				return view, "", readFailed(ctx, wctx, view, "waiting for the job's conversation to be recorded", opts, err)
			}
			if conv = firstConversation(convs); conv == "" {
				wait = "waiting for the job's conversation to be recorded"
			}
		}
		// A session that became ready after Ctrl-C is not joined.
		if wait == "" && ctx.Err() == nil {
			return view, conv, nil
		}
		if wait == "" {
			wait = last
		}
		if wctx.Err() != nil {
			return view, "", leftWaiting(ctx, view, wait, opts)
		}
		if opts.NoWait {
			return view, "", fmt.Errorf("%s cannot be attached to yet: %s\nleave out --no-wait to wait until it can", viewName(view), wait)
		}
		if wait != last {
			if last == "" {
				_, _ = fmt.Fprintf(env.Err, "Waiting for %s (Ctrl-C stops waiting; it keeps going).\n", viewName(view))
			}
			_, _ = fmt.Fprintf(env.Err, "  %s\n", wait)
			last = wait
		}
		select {
		case <-wctx.Done():
			return view, "", leftWaiting(ctx, view, last, opts)
		case <-time.After(poll):
		}
		next, err := c.GetSession(wctx, view.ID)
		if err != nil {
			if client.IsNotFound(err) {
				return view, "", sessionError(opts.Ref, err)
			}
			return view, "", readFailed(ctx, wctx, view, last, opts, err)
		}
		view = next
	}
}

// readFailed ends the wait on a failed read: as a given-up wait when the
// context ended it, otherwise with the error and the same hints.
//
// @decision(DL-025)
func readFailed(ctx, wctx context.Context, v server.SessionView, waiting string, opts attachOptions, err error) error {
	if wctx.Err() != nil {
		return leftWaiting(ctx, v, waiting, opts)
	}
	return fmt.Errorf("reading %s failed: %w\n%s", viewName(v), err, comeBack(v, opts))
}

// joinable says what a session is still waiting on, or "" when it can be
// joined, or an error when it never will be.
//
// @decision(DL-025)
func joinable(v server.SessionView) (string, error) {
	name := viewName(v)
	if j := v.Job; j != nil {
		if j.State.Terminal() {
			return "", fmt.Errorf("job %s has %s, so its session %s cannot be attached to\n`spov jobs show %s` shows how it ended", j.ID, j.State, name, j.ID)
		}
		if v.State == store.StateFailed || v.State == store.StateStopped {
			why := ""
			if v.Error != "" {
				why = ": " + v.Error
			}
			return "", fmt.Errorf("the session %s of job %s is %s%s\n`spov jobs show %s` shows how the job ends", name, j.ID, v.State, why, j.ID)
		}
		switch j.State {
		case store.JobQueued:
			return "the job is queued", nil
		case store.JobProvisioning:
			return "starting the job's sandbox", nil
		case store.JobDelivering:
			return "the job is finishing", nil
		case store.JobRunning, store.JobBlocked, store.JobSteered:
			if v.State != store.StateRunning {
				return "starting the job's sandbox", nil
			}
			return "", nil
		default:
			return "the job is " + string(j.State), nil
		}
	}
	switch v.State {
	case store.StateRunning:
		return "", nil
	case store.StateStarting:
		return "starting the sandbox", nil
	default:
		return "", fmt.Errorf("session %s is %s\nit cannot be attached to; run `spov fly` to start a new session", name, v.State)
	}
}

// leftWaiting is the error for a wait given up by the user or by
// --timeout.
//
// @decision(DL-025)
func leftWaiting(ctx context.Context, v server.SessionView, waiting string, opts attachOptions) error {
	why := "stopped waiting"
	if ctx.Err() == nil {
		why = fmt.Sprintf("gave up after %s", opts.Timeout)
	}
	if waiting == "" {
		waiting = "not ready"
	}
	return fmt.Errorf("%s for %s (%s)\n%s", why, viewName(v), waiting, comeBack(v, opts))
}

// comeBack says that nothing was stopped, and how to wait again or stop
// the session or its job.
//
// @decision(DL-025)
func comeBack(v server.SessionView, opts attachOptions) string {
	stop := fmt.Sprintf("spov stop %s", opts.Ref)
	if v.Job != nil {
		stop = fmt.Sprintf("spov jobs cancel %s", v.Job.ID)
	}
	return fmt.Sprintf("it keeps going, nothing was stopped\n  spov attach %s   # wait again\n  %s", opts.Ref, stop)
}
