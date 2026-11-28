package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// `spov jobs schedule` manages the caller's schedules through
// /api/v1/schedules. A schedule's job is described by the flags `spov
// jobs create` takes (addJobFlags), and its cadence by --cron or one of
// the shorthands, built into a cron expression here.
//
// @decision(DL-196) @decision(DL-023)

// newJobsScheduleCmd is the schedule group. Called bare it lists, as
// `jobs schedule list` does.
//
// @decision(DL-196)
func newJobsScheduleCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Run a job on a cadence",
		Long: "A schedule creates the same job on a cadence: five cron fields or a shorthand, read in a time zone," +
			" at most once an hour. Each run is an ordinary job; a run is skipped while the schedule's previous job" +
			" is unfinished.\n\nSchedules need a hosted server; a local server refuses them.",
		Args: cobra.NoArgs,
	}
	listSchedules(env, cmd)
	list := &cobra.Command{Use: "list", Short: "List your schedules", Args: cobra.NoArgs}
	listSchedules(env, list)
	cmd.AddCommand(newScheduleCreateCmd(env), list, newScheduleShowCmd(env), newScheduleEditCmd(env),
		newSchedulePauseCmd(env, true), newSchedulePauseCmd(env, false), newScheduleDeleteCmd(env), newScheduleRunCmd(env))
	return cmd
}

// cadenceFlags are --cron and its shorthands, and --tz.
//
// @decision(DL-196)
type cadenceFlags struct {
	Cron    string
	Hourly  bool
	Daily   string
	Weekly  string
	Monthly string
	Zone    string
}

// addCadenceFlags registers the cadence flags; zoneDefault ends --tz's
// help with what an unset zone means.
//
// @decision(DL-196)
func addCadenceFlags(cmd *cobra.Command, c *cadenceFlags, zoneDefault string) {
	cmd.Flags().StringVar(&c.Cron, "cron", "",
		"five cron fields (minute hour day-of-month month day-of-week), or @hourly, @daily, @weekly or @monthly; at most hourly")
	cmd.Flags().BoolVar(&c.Hourly, "hourly", false, "run at the start of every hour")
	cmd.Flags().StringVar(&c.Daily, "daily", "", "run every day at HH:MM, e.g. 09:30")
	cmd.Flags().StringVar(&c.Weekly, "weekly", "", `run every week on DAY at HH:MM, e.g. "mon 09:00"`)
	cmd.Flags().StringVar(&c.Monthly, "monthly", "", `run every month on day DAY (1 to 28) at HH:MM, e.g. "1 06:00"`)
	cmd.MarkFlagsMutuallyExclusive("cron", "hourly", "daily", "weekly", "monthly")
	cmd.Flags().StringVar(&c.Zone, "tz", "", "IANA time zone the cadence is read in, e.g. Europe/Stockholm; "+zoneDefault)
}

// expression is the cron expression the flags name, and whether any
// named one.
//
// @decision(DL-196)
func (c cadenceFlags) expression() (string, bool, error) {
	switch {
	case c.Cron != "":
		return strings.TrimSpace(c.Cron), true, nil
	case c.Hourly:
		return "@hourly", true, nil
	case c.Daily != "":
		h, m, err := clockTime(c.Daily)
		if err != nil {
			return "", true, fmt.Errorf("--daily: %w", err)
		}
		return fmt.Sprintf("%d %d * * *", m, h), true, nil
	case c.Weekly != "":
		day, at, ok := strings.Cut(strings.TrimSpace(c.Weekly), " ")
		dow, known := weekdays[strings.ToLower(day)]
		if !ok || !known {
			return "", true, fmt.Errorf(`--weekly takes a day and a time, e.g. "mon 09:00", not %q`, c.Weekly)
		}
		h, m, err := clockTime(at)
		if err != nil {
			return "", true, fmt.Errorf("--weekly: %w", err)
		}
		return fmt.Sprintf("%d %d * * %d", m, h, dow), true, nil
	case c.Monthly != "":
		day, at, ok := strings.Cut(strings.TrimSpace(c.Monthly), " ")
		dom, err := strconv.Atoi(day)
		// A later day skips the months without it; --cron writes one.
		if !ok || err != nil || dom < 1 || dom > 28 {
			return "", true, fmt.Errorf(`--monthly takes a day of the month (1 to 28) and a time, e.g. "1 06:00", not %q; --cron writes a later day`, c.Monthly)
		}
		h, m, err := clockTime(at)
		if err != nil {
			return "", true, fmt.Errorf("--monthly: %w", err)
		}
		return fmt.Sprintf("%d %d %d * *", m, h, dom), true, nil
	}
	return "", false, nil
}

// weekdays are the days --weekly takes, as cron's day-of-week numbers.
var weekdays = map[string]int{
	"sun": 0, "sunday": 0, "mon": 1, "monday": 1, "tue": 2, "tuesday": 2, "wed": 3, "wednesday": 3,
	"thu": 4, "thursday": 4, "fri": 5, "friday": 5, "sat": 6, "saturday": 6,
}

// clockTime reads HH:MM on a 24-hour clock.
func clockTime(s string) (hour, minute int, err error) {
	hs, ms, ok := strings.Cut(strings.TrimSpace(s), ":")
	hour, herr := strconv.Atoi(hs)
	minute, merr := strconv.Atoi(ms)
	if !ok || herr != nil || merr != nil || !digits(hs, 1, 2) || !digits(ms, 2, 2) || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("a time is HH:MM on a 24-hour clock, e.g. 09:30, not %q", s)
	}
	return hour, minute, nil
}

// digits reports whether s is lo to hi ASCII digits.
func digits(s string, lo, hi int) bool {
	if len(s) < lo || len(s) > hi {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// timezoneFile names the zone where /etc/localtime is a copy, not a
// link (Debian's containers); a test points it elsewhere.
var timezoneFile = "/etc/timezone"

// localtimeFile is the link whose target names this machine's zone; a
// test points it elsewhere.
var localtimeFile = "/etc/localtime"

// localZone is this machine's IANA zone name: TZ's, else the zone
// /etc/localtime links to, else /etc/timezone's, else UTC. It never
// fails.
//
// @decision(DL-196)
func (e *Env) localZone() string {
	if e.Environ != nil {
		if z := zoneName(strings.TrimPrefix(strings.TrimSpace(e.Environ.Getenv("TZ")), ":")); z != "" {
			return z
		}
	}
	if target, err := os.Readlink(localtimeFile); err == nil {
		if z := zoneName(target); z != "" {
			return z
		}
	}
	if b, err := os.ReadFile(timezoneFile); err == nil {
		if z := zoneName(strings.TrimSpace(string(b))); z != "" {
			return z
		}
	}
	return "UTC"
}

// zoneName is the IANA name s holds, as a name or a path under a
// zoneinfo directory, or "" when it holds none Go can load.
func zoneName(s string) string {
	if i := strings.LastIndex(s, "zoneinfo/"); i >= 0 {
		s = s[i+len("zoneinfo/"):]
		s = strings.TrimPrefix(strings.TrimPrefix(s, "posix/"), "right/")
	}
	if s == "" || s == "Local" || strings.HasPrefix(s, "/") {
		return ""
	}
	if _, err := time.LoadLocation(s); err != nil {
		return ""
	}
	return s
}

// scheduleError words a server that keeps no schedules.
//
// @decision(DL-196)
func scheduleError(err error) error {
	if client.StatusOf(err) != http.StatusNotImplemented {
		return err
	}
	if strings.Contains(err.Error(), "local") {
		return fmt.Errorf("%w\n\nSchedules need a hosted spov server; `spov login --server <url>` points this CLI at one", err)
	}
	return fmt.Errorf("%w\n\nSchedules need a hosted spov server that runs the job supervisor", err)
}

// readScheduleGoal is a schedule's goal from its argument or --goal-file.
func readScheduleGoal(args []string, file string) (string, error) {
	if len(args) == 0 && file == "" {
		return "", errors.New("a schedule needs a goal: `spov jobs schedule create nightly \"update the dependencies\" --daily 02:00`, or --goal-file")
	}
	return readGoal(args, file)
}

// refuseSyncKind refuses the kind a schedule cannot run.
func refuseSyncKind(kind string) error {
	if store.JobKind(kind) == store.JobKindSync {
		return errors.New("a schedule runs pr or report jobs; a sync job starts from a checkout, with `spov jobs create --kind sync`")
	}
	return nil
}

// newScheduleCreateCmd stores a schedule. Its job is built as `spov jobs
// create` builds one, from the same flags and configuration.
//
// @decision(DL-196)
func newScheduleCreateCmd(env *Env) *cobra.Command {
	var (
		opts   createOptions
		cad    cadenceFlags
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "create <name> [goal]",
		Short: "Create a schedule that starts a job on a cadence",
		Long: "Create a schedule: the job is described by the flags `spov jobs create` takes, and each run creates it" +
			" as that command would. The cadence is --cron, or one of --hourly, --daily, --weekly and --monthly.\n\n" +
			"A name is lower-case letters, digits, dots, dashes and underscores.",
		Example: `  spov jobs schedule create deps "update the dependencies" --repo alice/spov --kind pr --daily 02:00
  spov jobs schedule create audit --goal-file audit.md --kind report --weekly "mon 09:00" --tz Europe/Stockholm`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: noCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			goal, err := readScheduleGoal(args[1:], opts.GoalFile)
			if err != nil {
				return err
			}
			opts.Goal = goal
			if err := refuseSyncKind(opts.Kind); err != nil {
				return err
			}
			expr, ok, err := cad.expression()
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("a schedule needs a cadence: --cron, --hourly, --daily, --weekly or --monthly")
			}
			zone := strings.TrimSpace(cad.Zone)
			if zone == "" {
				zone = env.localZone()
			}
			ctx := cmd.Context()
			// A server that keeps no schedules says so before the job is resolved.
			if probe, err := env.client(ctx); err == nil {
				if _, err := probe.ListSchedules(ctx, client.Page{Limit: 1}); client.StatusOf(err) == http.StatusNotImplemented {
					return scheduleError(err)
				}
			}
			req, c, err := jobRequest(ctx, env, opts)
			if err != nil {
				return scheduleError(err)
			}
			v, err := c.CreateSchedule(ctx, server.ScheduleRequest{Name: args[0], Cron: expr, Zone: zone, Job: req})
			if err != nil {
				return scheduleError(err)
			}
			if asJSON {
				return writeJSON(env.Out, v)
			}
			if _, err := fmt.Fprintf(env.Out, "schedule %s created: %s\n", v.Name, nextRunWords(v)); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(env.Err, "\n  spov jobs schedule show %s\n  spov jobs schedule run %s    # runs it now\n\n", v.Name, v.Name)
			return nil
		},
	}
	addJobFlags(env, cmd, &opts, true)
	addCadenceFlags(cmd, &cad, "this machine's zone, else UTC, when unset")
	jsonFlag(cmd, &asJSON)
	return cmd
}

// noCompletion offers nothing for a name that is new.
func noCompletion(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// newScheduleEditCmd changes a schedule, sending only what was given.
//
// @decision(DL-196)
func newScheduleEditCmd(env *Env) *cobra.Command {
	var (
		opts createOptions
		cad  cadenceFlags
	)
	cmd := &cobra.Command{
		Use:   "edit <name> [goal]",
		Short: "Change a schedule's cadence, zone or job",
		Long: "Change a schedule. Only what is given changes: a cadence flag or --tz changes when it runs, and a goal" +
			" or a job flag changes the job. The server takes a job whole, so with a goal or any job flag the stored job" +
			" is read, the flags given are applied over it, and the whole job is sent back; nothing is read again from" +
			" your configuration. --env sets variables over the stored ones by name. A template named here applies" +
			" under what the stored job sets.",
		Example:           `  spov jobs schedule edit deps --daily 03:00 --model anthropic/claude-sonnet-5`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeSchedules(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var patch server.SchedulePatch
			expr, ok, err := cad.expression()
			if err != nil {
				return err
			}
			if ok {
				patch.Cron = &expr
			}
			if cmd.Flags().Changed("tz") {
				zone := strings.TrimSpace(cad.Zone)
				if zone == "" {
					return errors.New("--tz needs a zone, e.g. Europe/Stockholm or UTC")
				}
				patch.Zone = &zone
			}
			jobChanged := len(args) > 1 || len(changedJobFlags(cmd)) > 0
			if patch.Cron == nil && patch.Zone == nil && !jobChanged {
				return errors.New("nothing to change: give a cadence, --tz, a goal or a job flag")
			}
			c, err := env.client(ctx)
			if err != nil {
				return err
			}
			ref := args[0]
			if jobChanged {
				cur, err := c.GetSchedule(ctx, args[0])
				if err != nil {
					return scheduleError(err)
				}
				job, err := applyJobFlags(ctx, env, c, cmd, cur.Job, opts, args[1:])
				if err != nil {
					return err
				}
				patch.Job = &job
				// Written to the schedule that was read, whatever its name is by now.
				ref = cur.ID
			}
			v, err := c.UpdateSchedule(ctx, ref, patch)
			if err != nil {
				return scheduleError(err)
			}
			_, err = fmt.Fprintf(env.Out, "schedule %s changed: %s\n", v.Name, nextRunWords(v))
			return err
		},
	}
	addJobFlags(env, cmd, &opts, true)
	addCadenceFlags(cmd, &cad, "the stored zone when unset")
	return cmd
}

// changedJobFlags are the job flags cmd was given.
//
// @decision(DL-196)
func changedJobFlags(cmd *cobra.Command) []*pflag.Flag {
	var out []*pflag.Flag
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if _, ok := f.Annotations[jobFlagAnnotation]; ok {
			out = append(out, f)
		}
	})
	return out
}

// applyJobFlags is the stored job with the goal and every job flag cmd
// was given set over it, checked as `spov jobs create` checks them.
//
// @decision(DL-196)
func applyJobFlags(ctx context.Context, env *Env, c *client.Client, cmd *cobra.Command, job server.JobRequest,
	opts createOptions, goalArgs []string) (server.JobRequest, error) {
	if len(goalArgs) > 0 || opts.GoalFile != "" {
		goal, err := readGoal(goalArgs, opts.GoalFile)
		if err != nil {
			return job, err
		}
		job.Goal = goal
	}
	for _, f := range changedJobFlags(cmd) {
		switch f.Name {
		case "goal-file":
		case "kind":
			if err := refuseSyncKind(opts.Kind); err != nil {
				return job, err
			}
			job.Kind = store.JobKind(opts.Kind)
			// A policy only a pull request job takes goes with that kind.
			if job.Kind != store.JobKindPR && !cmd.Flags().Changed("pr-policy") {
				job.PRPolicy = ""
			}
		case "repo":
			res, err := env.config()
			if err != nil {
				return job, err
			}
			if job.Repos, err = env.resolveRepoRefs(ctx, c, res.Config, opts.Repos); err != nil {
				return job, err
			}
		case "landing":
			job.Landing = opts.Landing
		case "check":
			job.Check = opts.Check
		case "pr-policy":
			job.PRPolicy = store.PRPolicy(opts.PRPolicy)
		case "no-prep":
			job.SkipPrep = opts.NoPrep
		case "model":
			job.Model = opts.Model
			if strings.Contains(opts.Model, "#") && !cmd.Flags().Changed("reasoning") {
				job.Reasoning = ""
			}
		case "reasoning":
			job.Reasoning = strings.TrimSpace(opts.Reasoning)
			// The stored model's own mode gives way to the new one.
			if !cmd.Flags().Changed("model") {
				job.Model, _, _ = strings.Cut(job.Model, "#")
			}
		case "max-duration":
			job.MaxDuration, job.MaxDurationPreferred = opts.MaxDuration, false
		case "git-access":
			job.GitAccess, job.GitAccessPreferred = store.GitAccess(opts.GitAccess), false
		case "credentials":
			job.Credentials, job.NoCredentials = opts.Routed.Names, false
		case "no-credentials":
			job.Credentials, job.NoCredentials = nil, opts.Routed.None
		case "template":
			job.Template = opts.Template.Name
		case "no-template":
			if opts.Template.None {
				job.Template = ""
			}
		case "env":
			vars, err := launchEnv(job.Env, opts.Env, job.Addons)
			if err != nil {
				return job, err
			}
			job.Env = vars
		default:
			return job, fmt.Errorf("--%s cannot be changed on a schedule yet; delete it and create it again", f.Name)
		}
	}
	if err := checkReasoning(job.Model, job.Reasoning); err != nil {
		return job, err
	}
	if err := checkPRPolicy(string(job.PRPolicy), job.Kind); err != nil {
		return job, err
	}
	return job, nil
}

// schedulesBody is `jobs schedule list --json`: the listing's page.
//
// @decision(DL-196) @decision(DL-023)
type schedulesBody struct {
	Schedules []server.ScheduleView `json:"schedules"`
	Next      string                `json:"next,omitempty"`
	Total     int                   `json:"total"`
}

// listSchedules makes cmd list the caller's schedules: the server's
// first page, newest first, with a line saying when there are more.
//
// @decision(DL-196) @decision(DL-012)
func listSchedules(env *Env, cmd *cobra.Command) {
	var asJSON bool
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.ListSchedules(cmd.Context(), client.Page{})
		if err != nil {
			return scheduleError(err)
		}
		if asJSON {
			return writeJSON(env.Out, schedulesBody{Schedules: orEmpty(page.Schedules), Next: page.Next, Total: page.Total})
		}
		if len(page.Schedules) == 0 {
			_, err := fmt.Fprintln(env.Out, "No schedules.")
			return err
		}
		w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tSTATE\tCADENCE\tNEXT RUN\tLAST RUN\tGOAL")
		for _, v := range page.Schedules {
			next := "-"
			if v.NextRunAt != nil {
				next = inZone(*v.NextRunAt, v.Zone)
			}
			last := "-"
			if v.LastRun != nil {
				last = v.LastRun.Outcome + " " + formatAge(env.now().Sub(v.LastRun.FiredAt)) + " ago"
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", printableLine(v.Name), v.State,
				printableLine(v.Cron+" "+v.Zone), next, last, printableLine(firstLine(v.Job.Goal, 40)))
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if page.Next != "" {
			return writeFooter(env, fmt.Sprintf("the newest %d of %d", len(page.Schedules), page.Total))
		}
		return nil
	}
	jsonFlag(cmd, &asJSON)
}

// newScheduleShowCmd prints one schedule with its next runs and its
// history.
//
// @decision(DL-196)
func newScheduleShowCmd(env *Env) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "show <name>",
		Short:             "Show a schedule, its next runs and its history",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSchedules(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			v, err := c.GetSchedule(cmd.Context(), args[0])
			if err != nil {
				return scheduleError(err)
			}
			if asJSON {
				return writeJSON(env.Out, v)
			}
			return writeScheduleDetail(env, v)
		},
	}
	jsonFlag(cmd, &asJSON)
	return cmd
}

// writeScheduleDetail prints a schedule. What a server or an agent
// stored goes through Printable.
//
// @decision(DL-196) @decision(DL-136)
func writeScheduleDetail(env *Env, v server.ScheduleView) error {
	w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	row := func(k, val string) {
		if val != "" {
			_, _ = fmt.Fprintf(w, "%s\t%s\n", k, printableLine(val))
		}
	}
	row("Name", v.Name)
	row("ID", v.ID)
	state := v.State
	if v.PausedReason != "" {
		state += ": " + v.PausedReason
	}
	row("State", state)
	row("Cadence", v.Cron+" in "+v.Zone)
	for i, t := range v.NextRuns {
		label := ""
		if i == 0 {
			label = "Next runs"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\n", label, inZone(t, v.Zone))
	}
	if v.Failures > 0 {
		row("Failures", fmt.Sprintf("%d in a row; three pause it", v.Failures))
	}
	row("Kind", string(v.Job.Kind))
	repos := make([]string, 0, len(v.Job.Repos))
	for _, r := range v.Job.Repos {
		ref := r.Owner + "/" + r.Name
		if r.Platform != "" {
			ref = r.Platform + ":" + ref
		}
		repos = append(repos, ref)
	}
	row("Repositories", strings.Join(repos, ", "))
	row("Landing", v.Job.Landing)
	row("Check", v.Job.Check)
	model := v.Job.Model
	if v.Job.Reasoning != "" {
		model += " (" + v.Job.Reasoning + ")"
	}
	row("Model", strings.TrimSpace(model))
	row("Wall clock", v.Job.MaxDuration)
	row("Git access", string(v.Job.GitAccess))
	row("Template", v.Job.Template)
	switch {
	case v.Job.NoCredentials:
		row("Credentials", "none")
	case len(v.Job.Credentials) > 0:
		row("Credentials", strings.Join(v.Job.Credentials, ", "))
	}
	for i, name := range slices.Sorted(maps.Keys(v.Job.Env)) {
		label := ""
		if i == 0 {
			label = "Variables"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\n", label, printableLine(name+"="+v.Job.Env[name]))
	}
	if v.Job.PRPolicy == store.PRPolicyGoal {
		row("Pull requests", "as the goal says")
	}
	if v.Job.SkipPrep {
		row("Preparation", "skipped")
	}
	row("Created", v.CreatedAt.Format(time.RFC3339))
	if err := w.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(env.Out, "\nGoal:\n%s\n", Printable(v.Job.Goal)); err != nil {
		return err
	}
	if len(v.History) == 0 {
		_, err := fmt.Fprintln(env.Out, "\nNo runs yet.")
		return err
	}
	if _, err := fmt.Fprintln(env.Out, "\nHistory:"); err != nil {
		return err
	}
	w = tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  SLOT\tOUTCOME\tJOB\tREASON")
	for _, f := range v.History {
		outcome := f.Outcome
		if f.Missed > 1 {
			outcome += fmt.Sprintf(" (%d)", f.Missed)
		}
		job := f.JobID
		if job == "" {
			job = "-"
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", inZone(f.Slot, v.Zone), outcome, printableLine(job), printableLine(f.Reason))
	}
	return w.Flush()
}

// inZone is t as a person in zone reads it, in UTC when zone does not
// load.
func inZone(t time.Time, zone string) string {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("Mon 2006-01-02 15:04 MST")
}

// nextRunWords says when a schedule runs next.
func nextRunWords(v server.ScheduleView) string {
	switch {
	case v.State != "active":
		return "paused, so it does not run"
	case v.NextRunAt == nil:
		return "it runs no more"
	}
	return "next run " + inZone(*v.NextRunAt, v.Zone)
}

// newSchedulePauseCmd is `pause`, or with pause false `resume`, which
// starts again from the next slot after now.
//
// @decision(DL-196)
func newSchedulePauseCmd(env *Env, pause bool) *cobra.Command {
	use, short, word := "resume <name>", "Resume a paused schedule from its next slot", "resumed"
	if pause {
		use, short, word = "pause <name>", "Pause a schedule; it runs nothing until it is resumed", "paused"
	}
	return &cobra.Command{
		Use:               use,
		Short:             short,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSchedules(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			change := c.ResumeSchedule
			if pause {
				change = c.PauseSchedule
			}
			v, err := change(cmd.Context(), args[0])
			if err != nil {
				return scheduleError(err)
			}
			line := fmt.Sprintf("schedule %s %s", v.Name, word)
			if !pause {
				line += ": " + nextRunWords(v)
			}
			_, err = fmt.Fprintln(env.Out, line)
			return err
		},
	}
}

// newScheduleDeleteCmd removes a schedule and its history, asking first
// unless --yes; the jobs it started are kept.
//
// @decision(DL-196)
func newScheduleDeleteCmd(env *Env) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:               "delete <name>",
		Short:             "Delete a schedule and its history; the jobs it started are kept",
		Long:              "Delete a schedule and its history. The jobs it started are kept. It asks first; --yes does not ask.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSchedules(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := env.client(ctx)
			if err != nil {
				return err
			}
			v, err := c.GetSchedule(ctx, args[0])
			if err != nil {
				return scheduleError(err)
			}
			if !yes {
				ok, err := env.confirm(fmt.Sprintf("delete the schedule %s and its history? [y/N] ", v.Name))
				if err != nil {
					return fmt.Errorf("%w; pass --yes to delete the schedule", err)
				}
				if !ok {
					return errors.New("not deleted")
				}
			}
			if err := c.DeleteSchedule(ctx, v.ID); err != nil {
				return scheduleError(err)
			}
			_, err = fmt.Fprintf(env.Out, "schedule %s deleted; the jobs it started are kept\n", v.Name)
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "delete without asking")
	return cmd
}

// newScheduleRunCmd fires a schedule now, under the rules of a slot.
//
// @decision(DL-196)
func newScheduleRunCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "run <name>",
		Short: "Run a schedule now",
		Long: "Start the schedule's job now, as its next slot would. It is skipped, with exit status 1, while the" +
			" schedule's previous job is unfinished or you are at the job limit. The schedule's own slots are unchanged.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSchedules(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := env.client(ctx)
			if err != nil {
				return err
			}
			id, err := c.RunSchedule(ctx, args[0])
			if client.StatusOf(err) == http.StatusConflict {
				var apiErr *client.Error
				reason := err.Error()
				if errors.As(err, &apiErr) {
					reason = strings.TrimPrefix(apiErr.Detail, "not run: ")
				}
				return fmt.Errorf("schedule %s was not run: %s", args[0], reason)
			}
			if err != nil {
				return scheduleError(err)
			}
			if _, err := fmt.Fprintf(env.Out, "job %s queued\n", id); err != nil {
				return err
			}
			job, err := c.GetJob(ctx, id)
			if err != nil {
				_, _ = fmt.Fprintf(env.Err, "\n  spov jobs show %s\n\n", id)
				return nil
			}
			printJobHints(env, job)
			return nil
		},
	}
}

// completeSchedules offers the caller's schedule names, from a server
// that is already up.
//
// @decision(DL-196) @decision(DL-023)
func completeSchedules(env *Env) cobra.CompletionFunc {
	return completeWithin(env, func(ctx context.Context, prefix string) []cobra.Completion {
		c, ok := env.existingClient(ctx)
		if !ok {
			return nil
		}
		page, err := c.ListSchedules(ctx, client.Page{Limit: server.MaxPageSize})
		if err != nil {
			return nil
		}
		var out []cobra.Completion
		for _, v := range page.Schedules {
			if strings.HasPrefix(v.Name, prefix) {
				out = append(out, cobra.CompletionWithDesc(v.Name, printableLine(v.State+", "+v.Cron+" "+v.Zone)))
			}
		}
		return out
	})
}
