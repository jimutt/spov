package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// `spov jobs wait` blocks until a job is terminal and exits by how it
// ended, so a script can branch on `$?` without parsing anything. The
// codes are the table in DL-103; 1 stays the code for a failure of the command itself and 2 is
// left to cobra and shells.
//
// @decision(DL-103)
const (
	exitPartial     = 3
	exitFailed      = 4
	exitCancelled   = 5
	exitWaitTimeout = 6
	exitInterrupted = 130
)

// ExitError is an error that sets the process's exit code. `cmd/spov`
// exits with Code, and prints Err when it is not nil; a nil Err means the
// command has already said everything.
//
// @decision(DL-103)
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// jobPoll is how often a wait re-reads the job. The read answers from the
// server's store and never touches the sandbox, so a long wait keeps
// nothing billing.
//
// @decision(DL-103) @decision(DL-018)
const jobPoll = 5 * time.Second

// errWaitTimedOut is waitForJob's answer to --timeout running out.
var errWaitTimedOut = errors.New("the wait timed out")

// newJobsWaitCmd is `spov jobs wait`.
//
// @decision(DL-103)
func newJobsWaitCmd(env *Env) *cobra.Command {
	var (
		asJSON  bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "wait <job>",
		Short: "Wait for a job to finish and exit by how it ended",
		Long: "Block until the job succeeds, fails or is cancelled, then print it as `spov jobs show` does.\n\n" +
			"Exit status: 0 succeeded, 3 succeeded partly, 4 failed, 5 cancelled, 6 the wait timed out " +
			"(--timeout), 130 interrupted, and 1 when the command could not finish (a bad flag, an unknown job, no server). A timeout or Ctrl-C ends only the " +
			"wait; the job goes on.\n\n" +
			"A job that is blocked or steered is not finished, so the wait goes on through it.",
		Args: cobra.ExactArgs(1),
		// @decision(DL-023)
		ValidArgsFunction: completeJobs(env, true),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkTimeout(timeout); err != nil {
				return err
			}
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			return waitAndReport(cmd.Context(), env, c, args[0], timeout, asJSON, false)
		},
	}
	timeoutFlag(cmd, &timeout)
	jsonFlag(cmd, &asJSON)
	return cmd
}

// timeoutFlag registers --timeout, shared by `jobs wait` and
// `jobs create --wait`.
func timeoutFlag(cmd *cobra.Command, d *time.Duration) {
	cmd.Flags().DurationVar(d, "timeout", 0,
		"stop waiting after this long, e.g. 30m (exit 6, the job goes on); unset waits as long as the job runs")
}

func checkTimeout(d time.Duration) error {
	if d < 0 {
		return errors.New("--timeout cannot be negative")
	}
	return nil
}

// waitAndReport waits for the job, prints it and returns the exit code's
// error. compact prints the JSON on one line, for `create --wait --json`,
// whose output is one document per line.
//
// @decision(DL-103)
func waitAndReport(ctx context.Context, env *Env, c *client.Client, id string, timeout time.Duration, asJSON, compact bool) error {
	progress := env.stderrIsTerminal()
	view, err := waitForJob(ctx, env, c, id, timeout, func(v server.JobView) {
		if progress {
			_, _ = fmt.Fprintf(env.Err, "  job %s: %s\n", v.ID, v.State)
		}
	})
	// The final view comes before a non-zero outcome code. A wait that
	// timed out prints the last view it read, if it read one.
	if (err == nil || errors.Is(err, errWaitTimedOut)) && view.ID != "" {
		if perr := printJob(env, view, asJSON, compact); perr != nil {
			return perr
		}
	}
	switch {
	case err == nil:
		return outcomeCode(view)
	case errors.Is(err, errWaitTimedOut):
		return &ExitError{Code: exitWaitTimeout, Err: fmt.Errorf(
			"gave up waiting for job %s after %s (%s); it keeps going, nothing was stopped\n"+
				"  spov jobs wait %s   # wait again\n  spov jobs cancel %s",
			id, timeout, view.State, id, id)}
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		return &ExitError{Code: exitInterrupted, Err: fmt.Errorf(
			"stopped waiting for job %s; it keeps going, nothing was stopped\n"+
				"  spov jobs wait %s   # wait again\n  spov jobs cancel %s",
			id, id, id)}
	}
	return jobError(err)
}

func printJob(env *Env, v server.JobView, asJSON, compact bool) error {
	switch {
	case asJSON && compact:
		return writeJSONLine(env.Out, v)
	case asJSON:
		return writeJSON(env.Out, v)
	}
	return writeJobDetail(env, v)
}

// writeJSONLine prints v as one line.
func writeJSONLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// outcomeCode is the exit status for a terminal job: nil for a success.
//
// @decision(DL-103) @decision(DL-050)
func outcomeCode(v server.JobView) error {
	switch v.State {
	case store.JobSucceeded:
		if v.Partial {
			return &ExitError{Code: exitPartial}
		}
		return nil
	case store.JobCancelled:
		return &ExitError{Code: exitCancelled}
	}
	// Failed, and any terminal state added later that is not a success.
	return &ExitError{Code: exitFailed}
}

// waitForJob polls the job until it is terminal and returns it. It calls
// onChange with the first view and each time the job's state changes. It
// returns the latest view read alongside errWaitTimedOut when timeout (if
// positive) runs out, and alongside ctx's error when ctx ends; either
// ends only the wait, never the job. Every read is the job's store-backed
// view.
//
// @decision(DL-103) @decision(DL-018)
func waitForJob(ctx context.Context, env *Env, c *client.Client, id string, timeout time.Duration, onChange func(server.JobView)) (server.JobView, error) {
	wctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	poll := env.JobPoll
	if poll <= 0 {
		poll = jobPoll
	}
	var view server.JobView
	seen := false
	// ended says why a done context stopped the wait.
	ended := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errWaitTimedOut
	}
	for {
		next, err := c.GetJob(wctx, id)
		if err != nil {
			if wctx.Err() != nil {
				return view, ended()
			}
			return view, err
		}
		if !seen || next.State != view.State {
			onChange(next)
		}
		view, seen = next, true
		if view.State.Terminal() {
			return view, nil
		}
		select {
		case <-wctx.Done():
			return view, ended()
		case <-time.After(poll):
		}
	}
}

// stderrIsTerminal says whether progress lines are wanted: only when a
// person is looking.
//
// @decision(DL-103)
func (e *Env) stderrIsTerminal() bool {
	if e.StderrIsTerminal != nil {
		return e.StderrIsTerminal()
	}
	f, ok := e.Err.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(f.Fd()))
}
