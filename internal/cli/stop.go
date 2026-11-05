package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// newStopCmd ends sessions and terminates their sandboxes. A job's
// session is stopped like any other, which cancels the job.
//
// @decision(DL-023) @decision(DL-029) @decision(DL-002)
func newStopCmd(env *Env) *cobra.Command {
	var (
		all  bool
		sync bool
	)
	cmd := &cobra.Command{
		Use:   "stop [name|id]",
		Short: "Stop a spov session and terminate its sandbox",
		Long: "Stop a spov session: its sandbox is terminated and stops billing. A stopped session cannot be\n" +
			"resumed; on a hosted server its transcript stays readable on the web.\n\n" +
			"Stopping a job's session cancels the job, as `spov jobs cancel` does.",
		Args: cobra.MaximumNArgs(1),
		// @decision(DL-023)
		ValidArgsFunction: completeSessions(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return fmt.Errorf("give either a session or --all, not both")
			}
			if !all && len(args) == 0 {
				return fmt.Errorf("name a session to stop, or pass --all")
			}
			if all && sync {
				return fmt.Errorf("--sync cannot be used with --all")
			}

			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			if all {
				return stopAll(cmd.Context(), env, c)
			}
			return stopOne(cmd.Context(), env, c, args[0], sync)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop every running spov session, cancelling the jobs among them")
	// @decision(DL-027)
	cmd.Flags().BoolVar(&sync, "sync", false,
		"sync the sandbox repository's commits and working-tree changes back to the local checkout before stopping")
	return cmd
}

func stopOne(ctx context.Context, env *Env, c *client.Client, ref string, sync bool) error {
	// Read first only to say whether a job was cancelled (and to resolve
	// the session when --sync is given); without --sync, a failed read
	// leaves that out and the delete decides.
	view, getErr := c.GetSession(ctx, ref)
	// @decision(DL-027)
	if sync {
		if getErr != nil {
			return sessionError(ref, getErr)
		}
		if view.Job != nil {
			return fmt.Errorf("--sync is for interactive sessions, not autonomous jobs")
		}
		if _, ok := singleViewRepo(view); !ok {
			n := len(view.Repos)
			if n == 0 {
				n = len(view.Clones)
			}
			return fmt.Errorf("session %s has %d repositories; --sync supports single-repository sessions only",
				displayName(view), n)
		}
		if view.State != store.StateRunning {
			return fmt.Errorf("session %s is %s and has no running sandbox to sync from", displayName(view), view.State)
		}
		res, err := env.config()
		if err != nil {
			return err
		}
		if err := env.syncSessionBack(ctx, c, res.Config, view); err != nil {
			printSyncFailureHint(env, viewName(view))
			return err
		}
	}
	if err := c.DeleteSession(ctx, ref); err != nil {
		return sessionError(ref, err)
	}
	_, err := fmt.Fprintln(env.Out, stoppedLine(ref, view.Job))
	return err
}

// stoppedLine reports a stop, naming the job it cancelled when the
// session belonged to an unfinished one.
//
// @decision(DL-023)
func stoppedLine(name string, job *server.SessionJobView) string {
	if job == nil || job.State.Terminal() {
		return fmt.Sprintf("Stopped %s.", name)
	}
	return fmt.Sprintf("Stopped %s and cancelled its job %s.", name, job.ID)
}

// stopAll terminates every live session, continuing past failures: one
// sandbox that refuses to stop must not leave the rest billing.
//
// @decision(DL-029)
func stopAll(ctx context.Context, env *Env, c *client.Client) error {
	// Every live session, through the cursor. @decision(DL-012)
	var sessions []server.SessionView
	for before := ""; ; {
		page, err := c.ListSessions(ctx, client.Page{States: liveStates(), Limit: server.MaxPageSize, Before: before})
		if err != nil {
			return err
		}
		sessions = append(sessions, page.Sessions...)
		if before = page.Next; before == "" {
			break
		}
	}
	sessions = liveOnly(sessions)
	if len(sessions) == 0 {
		_, err := fmt.Fprintln(env.Out, "No running spov sessions.")
		return err
	}

	var failed int
	for _, s := range sessions {
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted; some sessions may still be running, run `spov ls`")
		}
		if err := c.DeleteSession(ctx, s.ID); err != nil {
			failed++
			_, _ = fmt.Fprintf(env.Err, "spov: %s: %v\n", displayName(s), err)
			continue
		}
		_, _ = fmt.Fprintln(env.Out, stoppedLine(displayName(s), s.Job))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d sessions could not be terminated; they are still billing", failed, len(sessions))
	}
	return nil
}
