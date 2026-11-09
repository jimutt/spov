package cli

import (
	"context"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// `spov jobs apply` and `spov jobs release` take a sync job's work out of
// its held sandbox, or discard it. `apply` lands a job's work, `release`
// ends a hold, and neither word means anything else.
//
// @decision(DL-102) @decision(DL-023)

// newJobsApplyCmd lands a sync job's state in the checkout it runs in.
//
// @decision(DL-102)
func newJobsApplyCmd(env *Env) *cobra.Command {
	var keep, asJSON bool
	cmd := &cobra.Command{
		Use:   "apply <job>",
		Short: "Land a sync job's work in this checkout",
		Long: "Land a sync job's commits and uncommitted changes in the checkout this runs in: in place when\n" +
			"it is still on the branch and commit the job started from, else on a branch named for the job.\n\n" +
			"On a finished job the held sandbox is then released, unless --keep. On a job still running the\n" +
			"work so far is landed and the job carries on.",
		Args: cobra.ExactArgs(1),
		// @decision(DL-023)
		ValidArgsFunction: completeSyncJobs(env, true),
		RunE: func(cmd *cobra.Command, args []string) error {
			return applyJob(cmd.Context(), env, args[0], keep, asJSON)
		},
	}
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the held sandbox after applying, to apply again elsewhere")
	jsonFlag(cmd, &asJSON)
	return cmd
}

// newJobsReleaseCmd ends a held sync job's sandbox without applying.
//
// @decision(DL-102)
func newJobsReleaseCmd(env *Env) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "release <job>",
		Short: "Discard a sync job's held work and end its sandbox",
		Args:  cobra.ExactArgs(1),
		// @decision(DL-023)
		ValidArgsFunction: completeSyncJobs(env, false),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			before, err := c.GetJob(cmd.Context(), args[0])
			if err != nil {
				return jobError(err)
			}
			job, err := c.ReleaseJob(cmd.Context(), args[0], false)
			if err != nil {
				return jobError(err)
			}
			if asJSON {
				return writeJSON(env.Out, job)
			}
			if before.Sync == nil || !before.Sync.Held {
				_, err = fmt.Fprintf(env.Out, "job %s holds no sandbox; nothing was released\n", job.ID)
				return err
			}
			_, err = fmt.Fprintf(env.Out, "job %s released; its work was discarded\n", job.ID)
			return err
		},
	}
	jsonFlag(cmd, &asJSON)
	return cmd
}

// applyJob exports a sync job's state, lands it in the checkout and,
// for a held job, releases the sandbox unless keep.
//
// @decision(DL-102)
func applyJob(ctx context.Context, env *Env, id string, keep, asJSON bool) error {
	res, err := env.config()
	if err != nil {
		return err
	}
	c, err := env.client(ctx)
	if err != nil {
		return err
	}
	job, err := c.GetJob(ctx, id)
	if err != nil {
		return jobError(err)
	}
	if job.Kind != store.JobKindSync {
		return fmt.Errorf("job %s is a %s job; only a sync job's work is applied", job.ID, job.Kind)
	}
	view, err := c.GetSession(ctx, job.SessionID)
	if err != nil {
		return sessionError(job.SessionID, err)
	}
	syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
	defer cancel()
	checkout, err := env.verifyCheckoutOf(syncCtx, res.Config, view)
	if err != nil {
		return err
	}
	keptHint := func() {
		_, _ = fmt.Fprintf(env.Err, "\nNothing was released; the job's sandbox still holds its work.\n"+
			"  retry:   spov jobs apply %s\n  discard: spov jobs release %s\n", job.ID, job.ID)
	}
	exported, err := c.ExportJobSync(syncCtx, job.ID)
	if err != nil {
		// A 409 or 404 is a job with nothing to take, not a hold kept.
		if st := client.StatusOf(err); st != http.StatusConflict && st != http.StatusNotFound {
			keptHint()
		}
		return fmt.Errorf("exporting job %s's state: %w", job.ID, jobError(err))
	}
	landed, err := landExport(syncCtx, checkout, view, exported)
	if err != nil {
		keptHint()
		return err
	}
	printSyncResult(env, landed)
	held := job.Sync != nil && job.Sync.Held
	switch {
	case !job.State.Terminal():
		_, _ = fmt.Fprintf(env.Err, "Job %s is still %s; this is its work so far.\n", job.ID, job.State)
	case held && keep:
		_, _ = fmt.Fprintf(env.Err, "The sandbox is kept: `spov jobs release %s` ends it.\n", job.ID)
	case held:
		if job, err = c.ReleaseJob(syncCtx, job.ID, true); err != nil {
			return fmt.Errorf("the work is applied, but the sandbox was not released: %w", jobError(err))
		}
		if job.Sync != nil && job.Sync.AppliedAt != nil {
			_, _ = fmt.Fprintf(env.Err, "Released job %s's sandbox.\n", job.ID)
		} else {
			_, _ = fmt.Fprintf(env.Err, "Job %s's hold had already ended; nothing was released.\n", job.ID)
		}
	}
	if asJSON {
		if job, err = c.GetJob(syncCtx, job.ID); err != nil {
			return jobError(err)
		}
		return writeJSON(env.Out, job)
	}
	return nil
}

// heldCancelHint is the hint a refused cancel of a held job carries.
//
// @decision(DL-102)
func heldCancelHint(ctx context.Context, c *client.Client, id string, err error) error {
	if client.StatusOf(err) != http.StatusConflict {
		return err
	}
	job, gerr := c.GetJob(ctx, id)
	if gerr != nil || job.Sync == nil || !job.Sync.Held {
		return err
	}
	return fmt.Errorf("%w\n\nIts sandbox is held: `spov jobs apply %s` takes its work, `spov jobs release %s` discards it",
		err, id, id)
}

// heldMark is the `held` beside a finished sync job's state in a table.
//
// @decision(DL-101)
func heldMark(sync *server.JobSyncView) string {
	if sync != nil && sync.Held {
		return ", held"
	}
	return ""
}

// shortSHA is a commit as a person reads it.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
