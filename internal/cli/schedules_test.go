package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// scheduleWorld is the fixture's real server made a hosted one with a
// supervisor, which schedules need, recording each request the CLI
// sends to /api/v1/schedules; intercept answers a route in its place.
type scheduleWorld struct {
	f         *apiFixture
	env       Env
	mu        sync.Mutex
	sent      []sentRequest
	intercept map[string]http.HandlerFunc
}

type sentRequest struct {
	method, path string
	body         []byte
}

func newScheduleWorld(t *testing.T) *scheduleWorld {
	t.Helper()
	w := &scheduleWorld{f: newAPIFixture(t), intercept: map[string]http.HandlerFunc{}}
	f := w.f
	f.deps.Local = false
	f.deps.Jobs = &jobs.Supervisor{
		Store: f.deps.Store, Provider: f.provider,
		Queue: func(context.Context, jobs.JobArgs) error { return nil },
		Warn:  func(string) {},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(f.deps, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	inner := srv.Handler()
	hs := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/schedules") {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			w.mu.Lock()
			w.sent = append(w.sent, sentRequest{r.Method, r.URL.Path, body})
			h := w.intercept[r.Method+" "+r.URL.Path]
			w.mu.Unlock()
			if h != nil {
				h(rw, r)
				return
			}
		}
		inner.ServeHTTP(rw, r)
	}))
	hs.Listener = ln
	hs.Start()
	t.Cleanup(hs.Close)
	w.env = f.env
	w.env.Client = client.New(hs.URL, f.token)
	w.env.Environ = config.MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": f.env.Environ.Getenv("XDG_CONFIG_HOME"), "TZ": "Europe/Stockholm"}}
	return w
}

// last is the newest request sent with method to a path ending in suffix.
func (w *scheduleWorld) last(t *testing.T, method, suffix string) sentRequest {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.sent) - 1; i >= 0; i-- {
		if w.sent[i].method == method && strings.HasSuffix(w.sent[i].path, suffix) {
			return w.sent[i]
		}
	}
	t.Fatalf("no %s …%s was sent", method, suffix)
	return sentRequest{}
}

func (w *scheduleWorld) run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return runWith(t, w.env, append([]string{"jobs", "schedule"}, args...)...)
}

func (w *scheduleWorld) stored(t *testing.T, name string) store.Schedule {
	t.Helper()
	sc, err := w.f.deps.Store.Schedules().GetByName(context.Background(), auth.LocalID, auth.LocalID, name)
	if err != nil {
		t.Fatalf("schedule %s: %v", name, err)
	}
	return sc
}

func (w *scheduleWorld) create(t *testing.T, name string, flags ...string) {
	t.Helper()
	args := append([]string{"create", name, "audit the dependencies", "--repo", "alice/spov", "--kind", "report", "--daily", "02:00"}, flags...)
	if _, _, err := w.run(t, args...); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

// The cadence shorthands build the cron expression the server reads,
// and a malformed one is refused before anything is sent.
//
// @decision(DL-196)
func TestTheCadenceFlagsBuildACronExpression(t *testing.T) {
	for _, tt := range []struct {
		flags cadenceFlags
		want  string
	}{
		{cadenceFlags{Cron: " 15 */2 * * 1-5 "}, "15 */2 * * 1-5"},
		{cadenceFlags{Cron: "@weekly"}, "@weekly"},
		{cadenceFlags{Hourly: true}, "@hourly"},
		{cadenceFlags{Daily: "09:30"}, "30 9 * * *"},
		{cadenceFlags{Daily: "0:05"}, "5 0 * * *"},
		{cadenceFlags{Weekly: "mon 09:00"}, "0 9 * * 1"},
		{cadenceFlags{Weekly: "Sunday 23:59"}, "59 23 * * 0"},
		{cadenceFlags{Monthly: "1 06:00"}, "0 6 1 * *"},
		{cadenceFlags{Monthly: "28 18:45"}, "45 18 28 * *"},
	} {
		got, ok, err := tt.flags.expression()
		if err != nil || !ok || got != tt.want {
			t.Errorf("%+v = %q, %v, %v; want %q", tt.flags, got, ok, err, tt.want)
		}
	}
	for _, bad := range []cadenceFlags{
		{Daily: "9"}, {Daily: "24:00"}, {Daily: "09:60"}, {Daily: "9:5"},
		{Weekly: "09:00"}, {Weekly: "someday 09:00"}, {Weekly: "mon"},
		{Monthly: "0 06:00"}, {Monthly: "29 06:00"}, {Monthly: "31 06:00"}, {Monthly: "first 06:00"},
		{Daily: "+9:30"}, {Daily: "009:30"},
	} {
		if _, _, err := bad.expression(); err == nil {
			t.Errorf("%+v was taken", bad)
		}
	}
	if _, ok, _ := (cadenceFlags{Zone: "UTC"}).expression(); ok {
		t.Error("no cadence flag, yet a cadence")
	}
	// The flags exclude each other.
	if _, _, err := runWith(t, Env{}, "jobs", "schedule", "create", "x", "g", "--daily", "02:00", "--hourly"); err == nil ||
		!strings.Contains(err.Error(), "none of the others") {
		t.Errorf("two cadences: %v", err)
	}
}

// --tz defaults to TZ's zone, else the zone /etc/localtime links to,
// else UTC, and a zone Go cannot load is passed over, never an error.
//
// @decision(DL-196)
func TestTheTimeZoneDefaultsToTheMachinesZone(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "localtime")
	if err := os.Symlink("/usr/share/zoneinfo/America/New_York", link); err != nil {
		t.Fatal(err)
	}
	old, oldTZ := localtimeFile, timezoneFile
	t.Cleanup(func() { localtimeFile, timezoneFile = old, oldTZ })
	timezoneFile = filepath.Join(dir, "missing-timezone")
	zone := func(tz string, set bool) string {
		vars := map[string]string{}
		if set {
			vars["TZ"] = tz
		}
		return (&Env{Environ: config.MapEnv{Vars: vars}}).localZone()
	}
	localtimeFile = link
	for _, tt := range []struct {
		tz   string
		set  bool
		want string
	}{
		{"Europe/Stockholm", true, "Europe/Stockholm"},
		{":Asia/Tokyo", true, "Asia/Tokyo"},
		{"/usr/share/zoneinfo/Europe/Oslo", true, "Europe/Oslo"},
		{"CET-1CEST", true, "America/New_York"},
		{"Local", true, "America/New_York"},
		{"", false, "America/New_York"},
	} {
		if got := zone(tt.tz, tt.set); got != tt.want {
			t.Errorf("TZ=%q: %q, want %q", tt.tz, got, tt.want)
		}
	}
	localtimeFile = filepath.Join(dir, "missing")
	// A copied /etc/localtime leaves /etc/timezone to name the zone.
	named := filepath.Join(dir, "timezone")
	if err := os.WriteFile(named, []byte("Europe/Helsinki\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	timezoneFile = named
	if got := zone("", false); got != "Europe/Helsinki" {
		t.Errorf("with /etc/timezone: %q, want Europe/Helsinki", got)
	}
	timezoneFile = filepath.Join(dir, "missing-timezone")
	if got := zone("", false); got != "UTC" {
		t.Errorf("no zone anywhere: %q, want UTC", got)
	}
	if got := (&Env{}).localZone(); got != "UTC" {
		t.Errorf("no environment: %q, want UTC", got)
	}
}

// create sends the job flags `spov jobs create` takes, built into the
// same request, with the cadence and the zone; with no --tz the zone is
// this machine's.
//
// @decision(DL-196)
func TestAScheduleIsCreatedWithTheJobFlagsAndItsCadence(t *testing.T) {
	w := newScheduleWorld(t)
	out, stderr, err := w.run(t, "create", "nightly", "audit the dependencies", "--repo", "alice/spov", "--kind", "report",
		"--check", "./spov/scripts/check.sh", "--max-duration", "45m", "--no-prep", "--env", "STAGE=nightly",
		"--landing", "spov", "--git-access", "repos", "--weekly", "mon 09:30")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(out, "schedule nightly created: next run Mon ") || !strings.Contains(stderr, "spov jobs schedule run nightly") {
		t.Errorf("printed %q, %q", out, stderr)
	}
	sc := w.stored(t, "nightly")
	if sc.Cron != "30 9 * * 1" || sc.Zone != "Europe/Stockholm" || sc.State != store.ScheduleActive {
		t.Errorf("stored cron %q zone %q state %q", sc.Cron, sc.Zone, sc.State)
	}
	var job server.JobRequest
	if err := json.Unmarshal(sc.Request, &job); err != nil {
		t.Fatal(err)
	}
	want := server.RepoRef{Platform: "codefloe", Owner: "alice", Name: "spov"}
	if job.Goal != "audit the dependencies" || job.Kind != store.JobKindReport || len(job.Repos) != 1 || job.Repos[0] != want ||
		job.Check != "./spov/scripts/check.sh" || job.MaxDuration != "45m" || !job.SkipPrep || job.Env["STAGE"] != "nightly" ||
		job.Landing != "spov" || job.GitAccess != store.GitAccessRepos || job.Git.Email != "test@example.com" {
		t.Errorf("stored job %+v", job)
	}

	// With --tz the zone is the flag's.
	w.create(t, "early", "--tz", "Asia/Tokyo")
	if got := w.stored(t, "early").Zone; got != "Asia/Tokyo" {
		t.Errorf("--tz Asia/Tokyo stored %q", got)
	}
	// A sync job, a missing cadence and a missing goal are refused before
	// the server is asked.
	before := len(w.sent)
	for _, args := range [][]string{
		{"create", "s", "g", "--kind", "sync", "--daily", "02:00"},
		{"create", "s", "g", "--repo", "alice/spov"},
		{"create", "s", "--daily", "02:00"},
	} {
		if _, _, err := w.run(t, args...); err == nil {
			t.Errorf("%v was taken", args)
		}
	}
	if len(w.sent) != before {
		t.Errorf("a refused create reached the server: %v", w.sent[before:])
	}
}

// Every job-describing flag of `spov jobs create` is a flag of `jobs
// schedule create` and of `edit`, marked as a job flag, and edit knows
// what each one changes.
//
// @decision(DL-196)
func TestAScheduleTakesEveryJobFlagOfJobsCreate(t *testing.T) {
	root := NewRoot(Env{})
	find := func(path ...string) *cobra.Command {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	create := find("jobs", "create")
	notForSchedules := []string{"wait", "timeout", "hold", "json", "help"}
	var names []string
	create.Flags().VisitAll(func(f *pflag.Flag) {
		if !slices.Contains(notForSchedules, f.Name) {
			names = append(names, f.Name)
		}
	})
	slices.Sort(names)
	if want := slices.Sorted(slices.Values(jobFlagNames)); !slices.Equal(names, want) {
		t.Errorf("jobs create's job flags are %v, jobFlagNames %v", names, want)
	}
	for _, sub := range []string{"create", "edit"} {
		cmd := find("jobs", "schedule", sub)
		for _, name := range jobFlagNames {
			f := cmd.Flags().Lookup(name)
			if f == nil {
				t.Errorf("jobs schedule %s has no --%s", sub, name)
				continue
			}
			if _, ok := f.Annotations[jobFlagAnnotation]; !ok {
				t.Errorf("jobs schedule %s's --%s is not marked as a job flag", sub, name)
			}
		}
	}
	// edit applies every job flag: none falls to its refusal.
	edit := find("jobs", "schedule", "edit")
	values := map[string]string{"kind": "report", "repo": "alice/spov", "credentials": "npm", "env": "A=b",
		"no-prep": "true", "no-credentials": "true", "no-template": "true", "pr-policy": "", "goal-file": "",
		"max-duration": "1h", "git-access": "repos", "model": "a/b"}
	for _, name := range jobFlagNames {
		v, ok := values[name]
		if !ok {
			v = "x"
		}
		if name == "goal-file" {
			continue
		}
		if err := edit.Flags().Set(name, v); err != nil {
			t.Fatalf("--%s %s: %v", name, v, err)
		}
	}
	w := newScheduleWorld(t)
	_, err := applyJobFlags(context.Background(), &w.env, w.env.Client, edit, server.JobRequest{Kind: store.JobKindReport},
		createOptions{Kind: "report", Repos: []string{"alice/spov"}}, nil)
	if err != nil {
		t.Errorf("edit does not apply every job flag: %v", err)
	}
}

// list and show print tables, and with --json the view they decoded and
// nothing else.
//
// @decision(DL-196) @decision(DL-023)
func TestScheduleListAndShowPrintTablesAndTheirJSON(t *testing.T) {
	w := newScheduleWorld(t)
	if out, _, err := w.run(t, "list"); err != nil || strings.TrimSpace(out) != "No schedules." {
		t.Errorf("an empty list printed %q, %v", out, err)
	}
	w.create(t, "nightly")
	w.create(t, "weekly")
	out, _, err := w.run(t, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var body schedulesBody
	decodeOnly(t, out, &body)
	if len(body.Schedules) != 2 || body.Total != 2 || body.Schedules[0].Name != "weekly" || body.Schedules[1].Job.Goal != "audit the dependencies" {
		t.Errorf("list --json printed %+v", body)
	}
	out, _, err = w.run(t)
	if err != nil || !strings.Contains(out, "NAME") || !strings.Contains(out, "nightly") || !strings.Contains(out, "0 2 * * * Europe/Stockholm") {
		t.Errorf("the bare group listed %q, %v", out, err)
	}
	out, _, err = w.run(t, "show", "nightly", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v server.ScheduleView
	decodeOnly(t, out, &v)
	if v.Name != "nightly" || len(v.NextRuns) != 3 || v.Cron != "0 2 * * *" {
		t.Errorf("show --json printed %+v", v)
	}
	out, _, err = w.run(t, "show", "nightly")
	if err != nil || !strings.Contains(out, "Next runs") || !strings.Contains(out, "No runs yet.") || !strings.Contains(out, "audit the dependencies") {
		t.Errorf("show printed %q, %v", out, err)
	}
}

// show prints the history, newest first, with what a server stored made
// safe for a terminal.
//
// @decision(DL-196) @decision(DL-136)
func TestAScheduleShowsItsHistorySafely(t *testing.T) {
	slot := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	v := server.ScheduleView{Name: "nightly", Cron: "0 2 * * *", Zone: "Europe/Stockholm", State: "paused",
		PausedReason: "paused after 3 failed runs in a row: \x1b[31mno", Failures: 3,
		Job: server.JobRequest{Goal: "audit\x1b]0;x\x07", Kind: store.JobKindReport},
		History: []server.ScheduleFireView{
			{Slot: slot, Outcome: "failed", Reason: "refused\x1b[2J"},
			{Slot: slot.Add(-24 * time.Hour), Outcome: "started", JobID: "01JOB"},
			{Slot: slot.Add(-72 * time.Hour), Outcome: "missed", Missed: 2},
		}}
	var out bytes.Buffer
	if err := writeScheduleDetail(&Env{Out: &out, Err: &bytes.Buffer{}}, v); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) {
		t.Errorf("a control character reached the terminal:\n%q", got)
	}
	for _, want := range []string{"History:", "Fri 2026-11-27 02:00 CET", "01JOB", "missed (2)", "3 in a row", "paused: paused after 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("show lacks %q:\n%s", want, got)
		}
	}
}

// run prints the job it started with jobs create's hints, and a skip's
// reason with exit status 1.
//
// @decision(DL-196)
func TestRunPrintsTheJobOrTheSkipsReason(t *testing.T) {
	w := newScheduleWorld(t)
	w.create(t, "nightly")
	created, _, err := runWith(t, w.env, "jobs", "create", "a job to point at", "--repo", "alice/spov", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var job server.JobView
	if err := json.Unmarshal([]byte(created), &job); err != nil {
		t.Fatal(err)
	}
	w.intercept["POST /api/v1/schedules/nightly/run"] = func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(rw).Encode(map[string]string{"job_id": job.ID})
	}
	out, stderr, err := w.run(t, "run", "nightly")
	if err != nil || out != "job "+job.ID+" queued\n" || !strings.Contains(stderr, "spov attach "+job.SessionID) ||
		!strings.Contains(stderr, "spov jobs cancel "+job.ID) {
		t.Errorf("run printed %q, %q, %v", out, stderr, err)
	}

	w.intercept["POST /api/v1/schedules/nightly/run"] = func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/problem+json")
		rw.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(rw).Encode(map[string]any{"status": 409, "title": "Conflict", "detail": "not run: previous run still running"})
	}
	out, _, err = w.run(t, "run", "nightly")
	if err == nil || err.Error() != "schedule nightly was not run: previous run still running" || out != "" {
		t.Errorf("a skip printed %q, %v", out, err)
	}
	var exit *ExitError
	if errors.As(err, &exit) {
		t.Errorf("a skip exits %d, want the plain error's 1", exit.Code)
	}
}

// edit sends only the fields given; a job flag sends the stored job
// whole, with the flag applied over it.
//
// @decision(DL-196)
func TestEditSendsOnlyWhatWasGiven(t *testing.T) {
	w := newScheduleWorld(t)
	w.create(t, "nightly", "--check", "./spov/check.sh", "--max-duration", "45m")
	keys := func(body []byte) []string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		return slices.Sorted(func(yield func(string) bool) {
			for k := range m {
				if !yield(k) {
					return
				}
			}
		})
	}
	if _, _, err := w.run(t, "edit", "nightly", "--weekly", "fri 17:00"); err != nil {
		t.Fatal(err)
	}
	if got := keys(w.last(t, http.MethodPatch, "/nightly").body); !slices.Equal(got, []string{"cron"}) {
		t.Errorf("a cadence edit sent %v", got)
	}
	if sc := w.stored(t, "nightly"); sc.Cron != "0 17 * * 5" || sc.Zone != "Europe/Stockholm" {
		t.Errorf("stored %q in %q", sc.Cron, sc.Zone)
	}
	if _, _, err := w.run(t, "edit", "nightly", "--tz", "UTC"); err != nil {
		t.Fatal(err)
	}
	if got := keys(w.last(t, http.MethodPatch, "/nightly").body); !slices.Equal(got, []string{"zone"}) {
		t.Errorf("a zone edit sent %v", got)
	}

	if _, _, err := w.run(t, "edit", "nightly", "--check", "./spov/scripts/check.sh", "--env", "A=1"); err != nil {
		t.Fatal(err)
	}
	// A job edit writes to the schedule it read, by its id.
	id := w.stored(t, "nightly").ID
	if got := keys(w.last(t, http.MethodPatch, "/"+id).body); !slices.Equal(got, []string{"job"}) {
		t.Errorf("a job edit sent %v", got)
	}
	w.last(t, http.MethodGet, "/nightly")
	var job server.JobRequest
	if err := json.Unmarshal(w.stored(t, "nightly").Request, &job); err != nil {
		t.Fatal(err)
	}
	if job.Check != "./spov/scripts/check.sh" || job.Goal != "audit the dependencies" || job.MaxDuration != "45m" ||
		job.Kind != store.JobKindReport || len(job.Repos) != 1 || job.Env["A"] != "1" {
		t.Errorf("the edited job is %+v", job)
	}
	if _, _, err := w.run(t, "edit", "nightly", "a new goal"); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(w.stored(t, "nightly").Request, &job); err != nil {
		t.Fatal(err)
	}
	if job.Goal != "a new goal" || job.Check != "./spov/scripts/check.sh" {
		t.Errorf("a goal edit stored %+v", job)
	}
	if _, _, err := w.run(t, "edit", "nightly"); err == nil || !strings.Contains(err.Error(), "nothing to change") {
		t.Errorf("an empty edit: %v", err)
	}
	if _, _, err := w.run(t, "edit", "nightly", "--kind", "sync"); err == nil {
		t.Error("an edit to a sync job was taken")
	}
	if _, _, err := w.run(t, "edit", "nightly", "--tz", " "); err == nil || !strings.Contains(err.Error(), "--tz needs a zone") {
		t.Errorf("an empty --tz: %v", err)
	}
}

// An edit's flags give way to each other as a person means them: a new
// reasoning mode replaces the stored model's own, and a kind that is not
// pr drops the stored pull-request policy unless --pr-policy is given.
//
// @decision(DL-196)
func TestAnEditsFlagsReplaceWhatTheyContradict(t *testing.T) {
	apply := func(job server.JobRequest, args ...string) (server.JobRequest, error) {
		t.Helper()
		env := &Env{}
		cmd := &cobra.Command{Use: "edit"}
		var opts createOptions
		addJobFlags(env, cmd, &opts, true)
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		return applyJobFlags(context.Background(), env, nil, cmd, job, opts, nil)
	}
	job, err := apply(server.JobRequest{Kind: store.JobKindReport, Model: "anthropic/m#high"}, "--reasoning", "low")
	if err != nil || job.Model != "anthropic/m" || job.Reasoning != "low" {
		t.Errorf("--reasoning over a stored mode = %+v, %v", job, err)
	}
	job, err = apply(server.JobRequest{Kind: store.JobKindPR, PRPolicy: store.PRPolicyGoal}, "--kind", "report")
	if err != nil || job.PRPolicy != "" || job.Kind != store.JobKindReport {
		t.Errorf("--kind report over a goal policy = %+v, %v", job, err)
	}
	if _, err := apply(server.JobRequest{Kind: store.JobKindPR}, "--kind", "report", "--pr-policy", "goal"); err == nil {
		t.Error("a pr policy given beside --kind report was taken")
	}
}

// delete asks on a terminal and deletes only on a yes; off a terminal
// it needs --yes. The other acts ask nothing.
//
// @decision(DL-196)
func TestDeleteAsksUnlessYes(t *testing.T) {
	w := newScheduleWorld(t)
	w.create(t, "nightly")
	var asked int
	w.env.Confirm = func(string) (bool, error) { asked++; return false, nil }
	if _, _, err := w.run(t, "delete", "nightly"); err == nil || err.Error() != "not deleted" {
		t.Errorf("a no: %v", err)
	}
	w.stored(t, "nightly")
	// Whatever their outcome, the other acts never ask.
	for _, args := range [][]string{{"pause", "nightly"}, {"edit", "nightly", "--hourly"}, {"run", "nightly"}} {
		_, _, _ = w.run(t, args...)
	}
	if asked != 1 {
		t.Errorf("asked %d times, want once", asked)
	}
	w.env.Confirm = nil
	if _, _, err := w.run(t, "delete", "nightly"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("off a terminal: %v", err)
	}
	w.stored(t, "nightly")
	out, _, err := w.run(t, "delete", "nightly", "--yes")
	if err != nil || !strings.Contains(out, "schedule nightly deleted") {
		t.Fatalf("--yes: %q, %v", out, err)
	}
	if _, err := w.f.deps.Store.Schedules().GetByName(context.Background(), auth.LocalID, auth.LocalID, "nightly"); err == nil {
		t.Error("the schedule is still stored")
	}
	w.env.Confirm = func(string) (bool, error) { return true, nil }
	w.create(t, "again")
	if _, _, err := w.run(t, "delete", "again"); err != nil {
		t.Errorf("a yes: %v", err)
	}
}

// pause and resume say what they did.
//
// @decision(DL-196)
func TestPauseAndResumeSayWhatTheyDid(t *testing.T) {
	w := newScheduleWorld(t)
	w.create(t, "nightly")
	if out, _, err := w.run(t, "pause", "nightly"); err != nil || out != "schedule nightly paused\n" {
		t.Errorf("pause: %q, %v", out, err)
	}
	if got := w.stored(t, "nightly").State; got != store.SchedulePaused {
		t.Errorf("stored %q", got)
	}
	// A resume needs an owner who may sign in, which the fixture's local
	// principal is not; the server's answer stands in.
	next := time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC)
	w.intercept["POST /api/v1/schedules/nightly/resume"] = func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(server.ScheduleView{Name: "nightly", Zone: "Europe/Stockholm", State: "active", NextRunAt: &next})
	}
	if out, _, err := w.run(t, "resume", "nightly"); err != nil || out != "schedule nightly resumed: next run Sat 2026-11-28 02:00 CET\n" {
		t.Errorf("resume: %q, %v", out, err)
	}
}

// Names complete from a running server's listing, and with none
// running nothing is offered and no server is started.
//
// @decision(DL-196) @decision(DL-023)
func TestScheduleNamesCompleteWithoutStartingAServer(t *testing.T) {
	w := newScheduleWorld(t)
	w.create(t, "nightly")
	w.create(t, "noon")
	w.create(t, "weekly")
	for _, sub := range []string{"show", "edit", "pause", "resume", "delete", "run"} {
		if got := completions(t, w.env, "jobs", "schedule", sub, "n"); !slices.Equal(got, []string{"noon", "nightly"}) {
			t.Errorf("%s offers %v", sub, got)
		}
	}
	if got := completions(t, w.env, "jobs", "schedule", "create", ""); len(got) != 0 {
		t.Errorf("create's new name offers %v", got)
	}
	env := Env{StateDir: t.TempDir(), Spawn: func(context.Context) error { t.Error("a completion spawned a server"); return nil }}
	if got := completions(t, env, "jobs", "schedule", "show", ""); len(got) != 0 {
		t.Errorf("with no server running, show offers %v", got)
	}
}

// Against a local server every subcommand says schedules need a hosted
// one, from its 501.
//
// @decision(DL-196)
func TestALocalServerSaysSchedulesNeedAHostedOne(t *testing.T) {
	f := newAPIFixture(t)
	for _, args := range [][]string{
		{"list"}, {"show", "x"}, {"pause", "x"}, {"resume", "x"}, {"delete", "x", "--yes"}, {"run", "x"},
		{"edit", "x", "--daily", "02:00"}, {"edit", "x", "--model", "a/b"},
		{"create", "x", "g", "--repo", "alice/spov", "--daily", "02:00"},
	} {
		_, _, err := runWith(t, f.env, append([]string{"jobs", "schedule"}, args...)...)
		if client.StatusOf(err) != http.StatusNotImplemented || !strings.Contains(err.Error(), "Schedules need a hosted spov server") {
			t.Errorf("%v: %v", args, err)
		}
	}
}
