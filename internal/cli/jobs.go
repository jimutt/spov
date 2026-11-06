package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// `spov jobs` creates autonomous jobs, reports on them and acts on them.
// Every subcommand is a client of the server API and gains no privileged
// path: a job is created by POST /api/v1/jobs whoever asks for it.
//
// @decision(DL-045) @decision(DL-002) @decision(DL-023)

// newJobsCreateCmd starts a job and exits. A job outlives the terminal
// that started it, so blocking a shell on something that may run for
// hours is the wrong default; the command prints how to follow it.
//
// @decision(DL-045) @decision(DL-023)
func newJobsCreateCmd(env *Env) *cobra.Command {
	var (
		kind     string
		repos    []string
		landing  string
		goalFile string
		check    string
		model    string
		clock    string
		access   string
		asJSON   bool
		routed   routedFlags
		envVars  []string
	)
	cmd := &cobra.Command{
		Use:   "create [goal]",
		Short: "Start an autonomous job in a new sandbox",
		Long: "Start an autonomous job: spov launches a sandbox, gives OpenCode the goal, and supervises it.\n\n" +
			"The job outlives this terminal. `spov jobs` reports on it and `spov jobs cancel` ends it.\n\n" +
			"Autonomous jobs need a hosted server; a local server refuses them.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			goal, err := readGoal(args, goalFile)
			if err != nil {
				return err
			}
			return createJob(cmd.Context(), env, createOptions{
				Goal: goal, Kind: kind, Repos: repos, Landing: landing, Check: check,
				Model: model, MaxDuration: clock, GitAccess: access, JSON: asJSON, Routed: routed,
				Env: envVars,
			})
		},
	}
	cmd.Flags().StringVar(&kind, "kind", string(store.JobKindPR),
		"what the job delivers: pr or report")
	cmd.Flags().StringArrayVar(&repos, "repo", nil,
		"repository to work in as owner/name, or platform:owner/name (repeatable); overrides the configured list")
	cmd.Flags().StringVar(&landing, "landing", "", "workspace directory to start in (defaults to the workspace root)")
	cmd.Flags().StringVar(&goalFile, "goal-file", "", "read the goal from a file")
	// Gate 1 of the job's verdict. It is workspace-relative, so the help's
	// example names the repository directory.
	//
	// @decision(DL-050) @decision(DL-037)
	cmd.Flags().StringVar(&check, "check", "",
		"command that verifies the work, run at the workspace root, e.g. ./repo/scripts/check.sh;"+
			" its exit status is one of the job's gates")
	// @decision(DL-014)
	cmd.Flags().StringVar(&model, "model", "",
		"model to run on as provider/model, e.g. anthropic/claude-sonnet-5; your account default when unset")
	// @decision(DL-049)
	cmd.Flags().StringVar(&clock, "max-duration", "",
		"how long the job may run unattended, e.g. 45m; sandbox.job_max_duration, then your account's default,"+
			" then 1h, when unset, and never more than your limit")
	// @decision(DL-043)
	cmd.Flags().StringVar(&access, "git-access", "",
		"how far the sandbox's git credentials reach: repos (the job's) or all (every repository they can);"+
			" sandbox.job_git_access, then your account's job default, then repos, when unset")
	addRoutedFlags(cmd, &routed, "sandbox.job_credentials")
	addEnvFlag(cmd, &envVars)
	jsonFlag(cmd, &asJSON)
	return cmd
}

type createOptions struct {
	// @decision(DL-075)
	Routed routedFlags
	// Env are --env KEY=VALUE.
	//
	// @decision(DL-096)
	Env     []string
	Goal    string
	Kind    string
	Model   string
	Repos   []string
	Landing string
	// Check is the repo's own check command; gate 1 of the verdict.
	//
	// @decision(DL-050)
	Check string
	// MaxDuration is the job's wall clock, as a duration.
	//
	// @decision(DL-049)
	MaxDuration string
	// GitAccess is --git-access; empty falls back to sandbox.job_git_access.
	//
	// @decision(DL-043)
	GitAccess string
	// JSON prints the created job's view instead of the queued line.
	//
	// @decision(DL-023)
	JSON bool
}

// readGoal takes the goal from the argument or from a file. A goal is
// often a paragraph, and shells make that awkward as an argument, so
// --goal-file exists.
//
// @decision(DL-045)
func readGoal(args []string, file string) (string, error) {
	switch {
	case file != "" && len(args) > 0:
		return "", errors.New("give the goal as an argument or with --goal-file, not both")
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading the goal: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	case len(args) == 1:
		return strings.TrimSpace(args[0]), nil
	}
	return "", errors.New("a job needs a goal: `spov jobs create \"fix the failing test\"` or --goal-file")
}

// createJob creates the job, kept out of cobra so it can be driven end
// to end against an in-process server.
//
// @decision(DL-045)
func createJob(ctx context.Context, env *Env, opts createOptions) error {
	if opts.Goal == "" {
		return errors.New("a job needs a goal")
	}
	res, err := env.config()
	if err != nil {
		return err
	}
	cfg := res.Config
	// @decision(DL-096)
	vars, err := launchEnv(cfg.Sandbox.Env, opts.Env, cfg.Sandbox.Addons)
	if err != nil {
		return err
	}
	c, err := env.client(ctx)
	if err != nil {
		return err
	}
	refs, err := env.resolveRepoRefs(ctx, c, cfg, opts.Repos)
	if err != nil {
		return err
	}
	// The same sandbox shape, image, egress and git identity a `spov fly`
	// would send, so a `pr` job's commits are attributed to the caller.
	//
	// @decision(DL-044) @decision(DL-002)
	identity := env.gitIdentity(ctx, cfg, git.Identity{})
	if warning := identityWarning(identity); warning != "" {
		env.deferWarning(warning)
	}
	sb := cfg.Sandbox
	job, err := c.CreateJob(ctx, server.JobRequest{
		Goal:    opts.Goal,
		Kind:    store.JobKind(opts.Kind),
		Repos:   refs,
		Landing: opts.Landing,
		Check:   opts.Check,
		Model:   opts.Model,
		// @decision(DL-049)
		MaxDuration:          firstNonEmpty(opts.MaxDuration, configuredDuration(sb.JobMaxDuration)),
		MaxDurationPreferred: opts.MaxDuration == "" && sb.JobMaxDuration > 0,
		Image:                sb.Image,
		// @decision(DL-033)
		Addons: sb.Addons,
		Apt:    sb.Apt,
		// @decision(DL-096)
		Env:    vars,
		Egress: sb.Egress,
		// @decision(DL-068)
		EgressExtraDomains: sb.EgressExtraDomains,
		// No timeout and no idle timeout: the server derives both from
		// the job's wall clock and ignores the request's.
		//
		// @decision(DL-049)
		Sandbox: server.SandboxConfigView{
			CPU: sb.CPU, CPULimit: sb.CPULimit, MemoryMiB: sb.MemoryMiB, MemoryLimitMiB: sb.MemoryLimitMiB,
			Regions: sb.Regions, Cloud: sb.Cloud, Workdir: sb.Workdir, InboundCIDRs: sb.InboundCIDRs,
		},
		Git: server.GitIdentityView{Name: identity.Name, Email: identity.Email},
		// @decision(DL-043)
		GitAccess:          store.GitAccess(firstNonEmpty(opts.GitAccess, sb.JobGitAccess)),
		GitAccessPreferred: opts.GitAccess == "" && sb.JobGitAccess != "",
		// @decision(DL-042)
		CredentialPreference: credentialPreference(cfg),
		// A job never takes sandbox.credentials.
		//
		// @decision(DL-075)
		Credentials: opts.Routed.Names, NoCredentials: opts.Routed.None,
		CredentialsPreferred: sb.JobCredentials, Needs: sb.Needs,
	})
	if err != nil {
		return jobError(err)
	}
	// @decision(DL-023)
	if opts.JSON {
		err = writeJSON(env.Out, job)
	} else {
		_, err = fmt.Fprintf(env.Out, "job %s queued\n", job.ID)
	}
	if err != nil {
		return err
	}
	// @decision(DL-025)
	_, _ = fmt.Fprintf(env.Err, "\n  spov jobs               # where it has got to\n"+
		"  spov attach %s   # joins it once it has started\n"+
		"  spov jobs cancel %s\n\n", job.SessionID, job.ID)
	return nil
}

// jobError turns the server's refusals into something a terminal can
// act on. A 501 is the one worth rewriting: "not implemented" reads as
// a missing feature rather than as the wrong kind of server.
//
// @decision(DL-045) @decision(DL-046)
func jobError(err error) error {
	if client.StatusOf(err) == http.StatusNotImplemented {
		return fmt.Errorf("%w\n\n"+
			"Autonomous jobs need a hosted spov server with a supervisor;"+
			" a local server has neither. `spov login --server <url>` points this CLI at one", err)
	}
	return err
}

// newJobsCmd is the job group. Called bare it lists, as `jobs list`
// does. Following a job is this listing and the web page, not a flag on
// create.
//
// @decision(DL-045) @decision(DL-023)
func newJobsCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Create, list and act on autonomous jobs",
		Args:  cobra.NoArgs,
	}
	listJobs(env, cmd)
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List autonomous jobs",
		Args:    cobra.NoArgs,
	}
	listJobs(env, list)
	cmd.AddCommand(newJobsCreateCmd(env), list, newJobsShowCmd(env), newJobsReportCmd(env),
		newJobsCancelCmd(env), newJobsResumeCmd(env), newJobsInterruptCmd(env))
	return cmd
}

// listJobs makes cmd list the caller's jobs, newest first, a page at a
// time, with every unfinished job on the first page however old.
//
// @decision(DL-045) @decision(DL-023) @decision(DL-012)
func listJobs(env *Env, cmd *cobra.Command) {
	var asJSON bool
	var pf pageFlags
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := pf.check(); err != nil {
			return err
		}
		c, err := env.client(cmd.Context())
		if err != nil {
			return err
		}
		var unfinished []string
		for _, st := range store.JobCounts {
			unfinished = append(unfinished, string(st))
		}
		l, err := pageThrough(cmd.Context(), func(ctx context.Context, p client.Page) ([]server.JobView, string, int, error) {
			page, err := c.ListJobs(ctx, p)
			return page.Jobs, page.Next, page.Total, err
		}, pf, unfinished)
		if err != nil {
			return jobError(err)
		}
		if asJSON {
			return writeJSON(env.Out, jobsBody{Jobs: orEmpty(l.rows), Next: l.next, Total: l.total})
		}
		if err := writeJobTable(env, l.rows, env.now()); err != nil {
			return err
		}
		return writeFooter(env, l.footer("spov jobs", "unfinished", pf))
	}
	pf.add(cmd)
	jsonFlag(cmd, &asJSON)
}

// newJobsCancelCmd ends a job for good; `spov jobs resume` is the other
// way out of blocked or steered.
//
// @decision(DL-045) @decision(DL-054)
func newJobsCancelCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <job>",
		Short: "Cancel a job and terminate its sandbox",
		Args:  cobra.ExactArgs(1),
		// @decision(DL-023)
		ValidArgsFunction: completeJobs(env, true),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.CancelJob(cmd.Context(), args[0]); err != nil {
				return jobError(err)
			}
			_, err = fmt.Fprintf(env.Out, "job %s cancelled\n", args[0])
			return err
		},
	}
}

// newJobsResumeCmd hands a steered or blocked job back to its loop. A
// message typed into the session never resumes a job; this command is
// the act that does.
//
// @decision(DL-054) @decision(DL-045) @decision(DL-049)
func newJobsResumeCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <job>",
		Short: "Hand a suspended job back to its loop",
		Long: "Resume a job that is steered or blocked.\n\n" +
			"A job is steered once someone takes it over: by a message, by attaching and typing, or by " +
			"`spov jobs interrupt`. Resuming starts its agent working towards its goal again, and the job " +
			"finishes on its own as it would have if nobody had taken it over. " +
			"A resumed job keeps the time it has left on its wall clock; resuming never adds time.",
		Args: cobra.ExactArgs(1),
		// @decision(DL-023)
		ValidArgsFunction: completeJobs(env, true),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.ResumeJob(cmd.Context(), args[0]); err != nil {
				return jobError(err)
			}
			_, err = fmt.Fprintf(env.Out, "job %s resumed\n", args[0])
			return err
		},
	}
}

// newJobsShowCmd prints one job in full, including the goal and the
// result, which the table has no room for.
//
// @decision(DL-045) @decision(DL-023)
func newJobsShowCmd(env *Env) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <job>",
		Short: "Show one job in full",
		Args:  cobra.ExactArgs(1),
		// @decision(DL-023)
		ValidArgsFunction: completeJobs(env, false),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			job, err := c.GetJob(cmd.Context(), args[0])
			if err != nil {
				return jobError(err)
			}
			if asJSON {
				return writeJSON(env.Out, job)
			}
			return writeJobDetail(env, job)
		},
	}
	jsonFlag(cmd, &asJSON)
	return cmd
}

// newJobsInterruptCmd stops a job's current turn through its session's
// interrupt endpoint, the same call as the web's Interrupt. The server
// moves the job to steered; `spov jobs resume` hands it back.
//
// @decision(DL-023) @decision(DL-053)
func newJobsInterruptCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "interrupt <job>",
		Short: "Interrupt a job's current turn and take the job over",
		Long: "Interrupt the turn a job's agent is running. The job is then steered: it waits for you instead " +
			"of continuing on its own, and its sandbox keeps running.\n\n" +
			"`spov attach <session>` steers it from the terminal, and `spov jobs resume` hands it back to its loop.",
		Args: cobra.ExactArgs(1),
		// @decision(DL-023)
		ValidArgsFunction: completeJobs(env, true),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			job, err := c.GetJob(cmd.Context(), args[0])
			if err != nil {
				return jobError(err)
			}
			got, err := c.Interrupt(cmd.Context(), job.SessionID, server.InterruptRequest{})
			if err != nil {
				return jobError(err)
			}
			// interrupted=false is a success too, with two readings, as on
			// the web's Interrupt; the job is taken over either way.
			//
			// @decision(DL-053)
			line := fmt.Sprintf("job %s interrupted and taken over", job.ID)
			if !got.Interrupted {
				line = fmt.Sprintf("job %s taken over; either no turn was running,"+
					" or a message that was waiting has just started one", job.ID)
			}
			if _, err := fmt.Fprintln(env.Out, line); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(env.Err, "\n  spov attach %s\n  spov jobs resume %s\n\n", job.SessionID, job.ID)
			return nil
		},
	}
}

// writeJobTable renders the job list. Age is shown for the same reason
// the session table shows it: a job holds a sandbox for its whole life.
//
// @decision(DL-045) @decision(DL-029)
func writeJobTable(env *Env, jobs []server.JobView, now time.Time) error {
	if len(jobs) == 0 {
		_, err := fmt.Fprintln(env.Out, "No jobs.")
		return err
	}
	// The tabwriter buffers every row and writes on Flush, so a write
	// here cannot fail against the underlying stream; Flush is what
	// reports a broken pipe.
	w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tKIND\tSTATE\tAGE\tGOAL"); err != nil {
		return err
	}
	for _, j := range jobs {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			j.ID, j.Kind, j.State, formatAge(now.Sub(j.CreatedAt)), firstLine(j.Goal, 48))
	}
	return w.Flush()
}

// writeJobDetail prints one job. The session id is shown because it is
// how a person reaches the transcript and `spov attach`.
//
// @decision(DL-045)
func writeJobDetail(env *Env, j server.JobView) error {
	// Buffered until Flush; see writeJobTable.
	w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "ID\t%s\n", j.ID)
	_, _ = fmt.Fprintf(w, "Kind\t%s\n", j.Kind)
	state := string(j.State)
	// @decision(DL-050)
	if j.Partial {
		state += ", partly"
	}
	_, _ = fmt.Fprintf(w, "State\t%s\n", state)
	_, _ = fmt.Fprintf(w, "Session\t%s\n", j.SessionID)
	_, _ = fmt.Fprintf(w, "Created\t%s\n", j.CreatedAt.Format(time.RFC3339))
	if j.StartedAt != nil {
		_, _ = fmt.Fprintf(w, "Started\t%s\n", j.StartedAt.Format(time.RFC3339))
	}
	if j.StoppedAt != nil {
		_, _ = fmt.Fprintf(w, "Stopped\t%s\n", j.StoppedAt.Format(time.RFC3339))
	}
	// When the clock runs out while it counts, and what is left while a
	// person steers.
	//
	// @decision(DL-049)
	if j.MaxDuration != "" {
		_, _ = fmt.Fprintf(w, "Wall clock\t%s\n", j.MaxDuration)
		d, err := time.ParseDuration(j.MaxDuration)
		used, _ := time.ParseDuration(j.ClockUsed)
		switch {
		case err != nil || j.State.Terminal():
		case j.ClockStartedAt != nil && (j.State == store.JobRunning || j.State == store.JobBlocked):
			_, _ = fmt.Fprintf(w, "Ends by\t%s\n", j.ClockStartedAt.Add(d-used).Format(time.RFC3339))
		case j.State == store.JobSteered:
			_, _ = fmt.Fprintf(w, "Left\t%s, paused while steered\n", max(d-used, 0).Round(time.Second))
		}
	}
	if j.Attempt > 0 {
		_, _ = fmt.Fprintf(w, "Attempt\t%d\n", j.Attempt)
	}
	if j.Error != "" {
		_, _ = fmt.Fprintf(w, "Note\t%s\n", j.Error)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := writeDeliverables(env, j); err != nil {
		return err
	}
	// The verdict is its own block rather than a tabwriter row, since it
	// is one line per gate. It is separate from Error: a succeeded job
	// has a verdict too.
	//
	// @decision(DL-050)
	if strings.TrimSpace(j.Verdict) != "" {
		if _, err := fmt.Fprintf(env.Out, "\nVerdict:\n%s\n", strings.TrimRight(j.Verdict, "\n")); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(env.Out, "\nGoal:\n%s\n", j.Goal)
	return err
}

// firstLine is the goal's opening line, truncated for a table cell. A
// goal is a paragraph; a table row is not.
func firstLine(s string, max int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	// Runes, not bytes: a goal is user text and slicing bytes cuts a
	// multi-byte character in half, putting invalid UTF-8 in the table.
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// configuredDuration is the config's job duration as the API takes it,
// or empty when it is unset.
//
// @decision(DL-049)
func configuredDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

// writeDeliverables prints a job's inventory, one line per member, then
// what the agent left unfinished.
//
// @decision(DL-051)
func writeDeliverables(env *Env, j server.JobView) error {
	if len(j.Deliverables) > 0 {
		if _, err := fmt.Fprintln(env.Out, "\nDeliverables:"); err != nil {
			return err
		}
		w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		for _, d := range j.Deliverables {
			_, _ = fmt.Fprintf(w, "  %s\t%s\n", d.Status, deliverableLine(d))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if j.Unfinished != "" {
		if _, err := fmt.Fprintf(env.Out, "\nLeft unfinished:\n%s\n", j.Unfinished); err != nil {
			return err
		}
	}
	return nil
}

// deliverableLine describes one member after its status.
//
// @decision(DL-051)
func deliverableLine(d server.DeliverableView) string {
	var line string
	switch d.Kind {
	// @decision(DL-051)
	case store.DeliverablePR:
		noun := git.ChangeNoun(d.Forge)
		line = fmt.Sprintf("%s from %s in %s (%s)", noun, d.Branch, d.Repository, d.Platform)
		if d.Number > 0 {
			line = fmt.Sprintf("%s %s in %s (%s)  %s", noun, git.ChangeRef(d.Forge, d.Number), d.Repository, d.Platform, d.URL)
		}
	default:
		line = fmt.Sprintf("report %s  %q", d.Path, d.Title)
		if d.Size > 0 {
			line += "  " + formatBytes(d.Size)
		}
	}
	if d.Detail != "" {
		line += "  (" + d.Detail + ")"
	}
	return line
}

// formatBytes is a size for a person.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// newJobsReportCmd prints one of a report job's reports, or saves it with
// the images it links.
//
// @decision(DL-051) @decision(DL-063) @decision(DL-023)
func newJobsReportCmd(env *Env) *cobra.Command {
	var saveDir string
	cmd := &cobra.Command{
		Use:   "report <job> [<path>]",
		Short: "Print a report a job delivered",
		Long: "Print one of a report job's reports as Markdown. With no path, the job's only report is " +
			"printed, or its reports are listed when it has several.\n\n" +
			"--save writes the report and the images it links into a directory instead.",
		Args: cobra.RangeArgs(1, 2),
		// @decision(DL-023)
		ValidArgsFunction: completeJobs(env, false),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := env.client(ctx)
			if err != nil {
				return err
			}
			job, err := c.GetJob(ctx, args[0])
			if err != nil {
				return jobError(err)
			}
			var reports []server.DeliverableView
			for _, d := range job.Deliverables {
				if d.Kind == store.DeliverableReport && d.Status == store.DeliverableVerified {
					reports = append(reports, d)
				}
			}
			var path string
			switch {
			case len(args) == 2:
				path = args[1]
			case len(reports) == 1:
				path = reports[0].Path
			case len(reports) == 0:
				return fmt.Errorf("job %s has no report", job.ID)
			default:
				w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
				for _, r := range reports {
					_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", r.Path, r.Title, formatBytes(r.Size))
				}
				if err := w.Flush(); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(env.Err, "\n  spov jobs report %s <path>\n\n", job.ID)
				return nil
			}
			body, _, err := c.GetJobFile(ctx, job.ID, path)
			if err != nil {
				return jobError(err)
			}
			if saveDir == "" {
				_, err = env.Out.Write(body)
				return err
			}
			return saveReport(ctx, env, c, job.ID, saveDir, path, body)
		},
	}
	cmd.Flags().StringVar(&saveDir, "save", "", "Write the report and its images into this directory")
	return cmd
}

// saveReport writes a report and every collected image it links under
// dir, at their paths in the out directory. A link to a file that was not
// collected is skipped.
//
// @decision(DL-051)
func saveReport(ctx context.Context, env *Env, c *client.Client, jobID, dir, path string, body []byte) error {
	write := func(rel string, b []byte) error {
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	}
	rel, ok := jobs.OutRelative(path)
	if !ok {
		return fmt.Errorf("%q is not a path under the out directory", path)
	}
	path = rel
	if err := write(path, body); err != nil {
		return err
	}
	saved := 1
	for _, img := range jobs.ImageLinks(body) {
		b, _, err := c.GetJobFile(ctx, jobID, img)
		if err != nil {
			continue
		}
		if err := write(img, b); err != nil {
			return err
		}
		saved++
	}
	_, err := fmt.Fprintf(env.Err, "saved %d files to %s\n", saved, dir)
	return err
}
