package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// `spov jobs` and its subcommands.
//
// @decision(DL-045)

// The local server has no supervisor, so a job is refused, and the
// message names the kind of server that is needed.
//
// @decision(DL-046)
func TestJobsCreateAgainstALocalServerExplainsWhyItRefuses(t *testing.T) {
	f := newAPIFixture(t)
	_, _, err := runWith(t, f.env, "jobs", "create", "fix the failing test")
	if err == nil {
		t.Fatal("a local server accepted a job")
	}
	msg := err.Error()
	for _, want := range []string{"hosted", "spov login"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
}

// A goal is required and the two ways of giving one are exclusive. All
// three are refused before any request is made, so a typo does not cost
// a round trip.
func TestReadGoalTakesOneSourceAndInsistsOnOne(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/goal.txt"
	if err := os.WriteFile(file, []byte("  update the changelog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("from an argument", func(t *testing.T) {
		got, err := readGoal([]string{"  do the thing  "}, "")
		if err != nil || got != "do the thing" {
			t.Errorf("= %q, %v", got, err)
		}
	})
	t.Run("from a file", func(t *testing.T) {
		got, err := readGoal(nil, file)
		if err != nil || got != "update the changelog" {
			t.Errorf("= %q, %v", got, err)
		}
	})
	t.Run("both is a mistake", func(t *testing.T) {
		if _, err := readGoal([]string{"a"}, file); err == nil {
			t.Error("an argument and a file were both accepted")
		}
	})
	t.Run("neither is a mistake", func(t *testing.T) {
		_, err := readGoal(nil, "")
		if err == nil {
			t.Fatal("a job with no goal was accepted")
		}
		// The message has to say how to give one; "a goal is required"
		// on its own leaves the user guessing.
		if !strings.Contains(err.Error(), "--goal-file") {
			t.Errorf("the message does not say how to give a goal: %v", err)
		}
	})
	t.Run("a missing file is reported", func(t *testing.T) {
		if _, err := readGoal(nil, dir+"/nope.txt"); err == nil {
			t.Error("a missing goal file was accepted")
		}
	})
}

// The table is what a person scans to see whether a job is stuck, so
// the state and the age have to be in it, and a multi-line goal must
// not break the layout.
func TestJobTableIsOneRowPerJob(t *testing.T) {
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}}
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	jobs := []server.JobView{{
		ID: "01JOB", Kind: store.JobKindPR, State: store.JobRunning,
		CreatedAt: now.Add(-90 * time.Minute),
		Goal:      "rewrite the parser\nand then the printer",
	}}
	if err := writeJobTable(env, jobs, now); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("a one-job table has %d lines; a goal's newline reached the output:\n%s", len(lines), out.String())
	}
	row := lines[1]
	for _, want := range []string{"01JOB", "running", "1h30m", "rewrite the parser"} {
		if !strings.Contains(row, want) {
			t.Errorf("the row does not carry %q: %s", want, row)
		}
	}
	if strings.Contains(row, "printer") {
		t.Errorf("the second line of the goal reached the table: %s", row)
	}
}

// An empty list says so rather than printing a bare header, which reads
// as a broken command.
func TestJobTableSaysWhenThereAreNoJobs(t *testing.T) {
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}}
	if err := writeJobTable(env, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No jobs") {
		t.Errorf("an empty listing printed %q", out.String())
	}
}

// firstLine is what keeps a paragraph out of a table cell.
func TestFirstLineTrimsToOneLineAndWidth(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"short":        {"do the thing", "do the thing"},
		"newline":      {"first\nsecond", "first"},
		"carriage":     {"first\r\nsecond", "first"},
		"trailing ws":  {"  padded  ", "padded"},
		"over the max": {strings.Repeat("x", 60), strings.Repeat("x", 47) + "…"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := firstLine(tc.in, 48); got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}

// The detail view is where the goal and the result live, since the
// table has no room for either.
func TestJobDetailShowsTheGoalAndTheSession(t *testing.T) {
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}}
	started := time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC)
	j := server.JobView{
		ID: "01JOB", Kind: store.JobKindReport, State: store.JobBlocked,
		SessionID: "01SESS", Goal: "summarise the release notes",
		Error: "waiting on a permission", StartedAt: &started,
	}
	if err := writeJobDetail(env, j); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"01JOB", "01SESS", "blocked", "summarise the release notes", "waiting on a permission"} {
		if !strings.Contains(got, want) {
			t.Errorf("the detail does not show %q:\n%s", want, got)
		}
	}
}

// The detail view shows every gate of the verdict, including for a
// succeeded job, which has no Error.
//
// @decision(DL-050)
func TestJobDetailShowsTheVerdict(t *testing.T) {
	const verdict = "- check: passed (exit 0)\n" +
		"- deliverable: passed (the report is at /tmp/report.md)\n" +
		"- conduct: passed (nothing was refused)"

	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}}
	j := server.JobView{
		ID: "01JOB", Kind: store.JobKindReport, State: store.JobSucceeded,
		SessionID: "01SESS", Goal: "summarise the release notes",
		Verdict: verdict,
	}
	if err := writeJobDetail(env, j); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	// Every gate, not merely the word "verdict": a summary line alone
	// would hide which gate decided the outcome.
	for _, want := range []string{"check: passed", "deliverable: passed", "conduct: passed"} {
		if !strings.Contains(got, want) {
			t.Errorf("the detail does not show %q:\n%s", want, got)
		}
	}
	// A succeeded job has no Error, so the verdict cannot be riding on
	// that field.
	if j.Error != "" {
		t.Fatal("this fixture is meant to have no error")
	}
}

// `spov jobs create` creates a job over the API, `spov jobs` lists it, and
// `spov jobs resume` and `cancel` act on it, against an in-process server
// whose supervisor queue only counts, so nothing provisions a sandbox.
//
// @decision(DL-045)
func TestJobsCreateMakesAJobThatJobsListsAndCancels(t *testing.T) {
	f := newAPIFixture(t)
	var queued int
	// The same server, rebuilt with a supervisor: the fixture's own is
	// local mode, which refuses jobs by design.
	f.deps.Jobs = &jobs.Supervisor{
		Store: f.deps.Store, Provider: f.provider,
		Queue: func(context.Context, jobs.JobArgs) error { queued++; return nil },
		Warn:  func(string) {},
	}
	// The guard checks Host against the address the server was built
	// with, so the listener has to exist first.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(f.deps, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.Listener = ln
	hs.Start()
	t.Cleanup(hs.Close)
	env := f.env
	env.Client = client.New(hs.URL, f.token)

	out, _, err := runWith(t, env, "jobs", "create", "fix the failing test", "--repo", "alice/spov")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "queued") {
		t.Errorf("run printed %q", out)
	}
	if queued != 1 {
		t.Errorf("the queue was handed %d jobs, want 1", queued)
	}

	// The listing finds it, with its goal.
	out, _, err = runWith(t, env, "jobs")
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	if !strings.Contains(out, "queued") || !strings.Contains(out, "fix the failing test") {
		t.Fatalf("the listing does not show the new job:\n%s", out)
	}
	id := jobIDFromTable(t, out)

	// A second job, moved past queued. The default listing sends every
	// unfinished state, so this is what proves the filter carries more
	// than its first value — the server keeps only the first of a
	// repeated query parameter, and with one queued job the truncated
	// filter happens to give the right answer.
	out2, _, err := runWith(t, env, "jobs", "create", "second goal", "--repo", "alice/spov")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	secondID := strings.Fields(out2)[1]
	ctx := context.Background()
	moved, err := f.deps.Store.Jobs().Get(ctx, auth.LocalID, secondID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []store.JobState{store.JobProvisioning, store.JobRunning} {
		moved.State = st
		if err := f.deps.Store.Jobs().Update(ctx, moved); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err = runWith(t, env, "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, secondID) {
		t.Errorf("the running job is missing from the default listing;"+
			" the state filter is losing all but its first value:\n%s", out)
	}
	if !strings.Contains(out, id) {
		t.Errorf("the queued job vanished from the default listing:\n%s", out)
	}
	// The bare group and its list subcommand are the same listing.
	//
	// @decision(DL-023)
	for _, args := range [][]string{{"jobs", "list"}, {"jobs", "ls"}} {
		got, _, err := runWith(t, env, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if got != out {
			t.Errorf("%v printed\n%s\nwhere `spov jobs` printed\n%s", args, got, out)
		}
	}

	// `show` carries what the table cannot.
	out, _, err = runWith(t, env, "jobs", "show", id)
	if err != nil {
		t.Fatalf("jobs show: %v", err)
	}
	if !strings.Contains(out, "fix the failing test") {
		t.Errorf("show does not print the goal:\n%s", out)
	}

	// A blocked job resumes to running. It was never paused, so this is a
	// pure row move and needs no sandbox.
	//
	// @decision(DL-054)
	job, err := f.deps.Store.Jobs().Get(t.Context(), auth.LocalID, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []store.JobState{store.JobProvisioning, store.JobRunning, store.JobBlocked} {
		job.State = st
		if err := f.deps.Store.Jobs().Update(t.Context(), job); err != nil {
			t.Fatalf("moving to %s: %v", st, err)
		}
	}
	out, _, err = runWith(t, env, "jobs", "resume", id)
	if err != nil {
		t.Fatalf("jobs resume: %v", err)
	}
	if !strings.Contains(out, "resumed") {
		t.Errorf("resume printed %q", out)
	}
	job, err = f.deps.Store.Jobs().Get(t.Context(), auth.LocalID, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.JobRunning {
		t.Errorf("a resumed job is %s, want running", job.State)
	}

	// A cancelled job stays in the default listing, which is the newest
	// jobs of every state, on the group and on list.
	//
	// @decision(DL-012)
	if _, _, err := runWith(t, env, "jobs", "cancel", id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	for _, args := range [][]string{{"jobs"}, {"jobs", "list"}} {
		out, _, err = runWith(t, env, args...)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, id) || !strings.Contains(out, "cancelled") {
			t.Errorf("%v does not show the cancelled job:\n%s", args, out)
		}
	}

	// --json is the API's view, with the hints kept off stdout. The
	// cancel above made room under the job quota for a third.
	//
	// @decision(DL-023)
	out, stderr, err := runWith(t, env, "jobs", "create", "third goal", "--repo", "alice/spov", "--json")
	if err != nil {
		t.Fatalf("create --json: %v", err)
	}
	var created server.JobView
	decodeOnly(t, out, &created)
	if created.ID == "" || created.State != store.JobQueued || created.Goal != "third goal" {
		t.Errorf("create --json printed %+v", created)
	}
	if !strings.Contains(stderr, "spov jobs cancel "+created.ID) {
		t.Errorf("the hints left stderr:\n%s", stderr)
	}
	var shown server.JobView
	out, _, err = runWith(t, env, "jobs", "show", id, "--json")
	if err != nil {
		t.Fatalf("show --json: %v", err)
	}
	decodeOnly(t, out, &shown)
	if shown.ID != id || shown.Goal != "fix the failing test" {
		t.Errorf("show --json printed %+v", shown)
	}
	var listed jobsBody
	out, _, err = runWith(t, env, "jobs", "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	decodeOnly(t, out, &listed)
	// The cancelled one too: the listing is every state (DL-012).
	if len(listed.Jobs) != 3 || listed.Total != 3 {
		t.Errorf("list --json has %d jobs of %d, want 3:\n%s", len(listed.Jobs), listed.Total, out)
	}
}

// `spov jobs interrupt` reads the job for its session and interrupts that
// session, saying what the server answered; a refusal reaches the
// terminal in the server's words.
//
// @decision(DL-023)
func TestJobsInterruptStopsTheJobsSessionTurn(t *testing.T) {
	var interrupted []string
	answer := `{"interrupted":true,"conversation":"ses_1"}`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/jobs/job_1":
			_, _ = io.WriteString(w, `{"id":"job_1","session_id":"sess_9","kind":"pr","state":"running","goal":"g"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/sess_9/interrupt":
			interrupted = append(interrupted, r.URL.Path)
			_, _ = io.WriteString(w, answer)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/jobs/job_2":
			_, _ = io.WriteString(w, `{"id":"job_2","session_id":"sess_8","kind":"pr","state":"blocked","goal":"g"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/sess_8/interrupt":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"status":409,"title":"Conflict","detail":"its agent is waiting on a question"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	env := Env{Client: client.New(api.URL, "tok")}

	out, stderr, err := runWith(t, env, "jobs", "interrupt", "job_1")
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if len(interrupted) != 1 || !strings.Contains(out, "job_1 interrupted") {
		t.Errorf("interrupt calls %v, printed %q", interrupted, out)
	}
	for _, want := range []string{"spov attach sess_9", "spov jobs resume job_1"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the next steps do not include %q:\n%s", want, stderr)
		}
	}

	// No turn was running: still a success, with both readings.
	answer = `{"interrupted":false,"conversation":"ses_1"}`
	out, _, err = runWith(t, env, "jobs", "interrupt", "job_1")
	if err != nil || !strings.Contains(out, "taken over; either no turn was running") {
		t.Errorf("an idle interrupt: err %v, printed %q", err, out)
	}

	_, _, err = runWith(t, env, "jobs", "interrupt", "job_2")
	if err == nil || !strings.Contains(err.Error(), "waiting on a question") {
		t.Errorf("a refused interrupt: %v", err)
	}
}

// jobIDFromTable reads the id out of the first row of `spov jobs`.
func jobIDFromTable(t *testing.T, table string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if len(lines) < 2 {
		t.Fatalf("no job rows in:\n%s", table)
	}
	fields := strings.Fields(lines[1])
	if len(fields) == 0 {
		t.Fatalf("empty job row in:\n%s", table)
	}
	return fields[0]
}

// firstLine slices runes: a goal is user text, and cutting bytes puts
// invalid UTF-8 in the table for anything non-ASCII.
func TestFirstLineDoesNotSplitARune(t *testing.T) {
	goal := strings.Repeat("ä", 60)
	got := firstLine(goal, 48)
	if !utf8.ValidString(got) {
		t.Errorf("firstLine produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 48 {
		t.Errorf("= %d runes, want 48 (47 plus the ellipsis)", n)
	}
}

// `spov jobs create --check` reaches the job row, where it is gate 1 of the
// verdict. Driven through the real command and server, flag to stored row.
//
// @decision(DL-050)
func TestRunSendsTheCheckCommand(t *testing.T) {
	testRunFlagReachesTheRow(t, []string{"--check", "./scripts/check.sh"},
		func(t *testing.T, j store.Job) {
			if j.CheckCommand != "./scripts/check.sh" {
				t.Errorf("the row carries check %q, want ./scripts/check.sh: gate 1 would be unverified on every job",
					j.CheckCommand)
			}
		})
}

// TestRunSendsTheModel is the same path for --model: a flag that never
// leaves the CLI would leave every job on OpenCode's default while the
// person who typed it believed otherwise.
//
// @decision(DL-014)
func TestRunSendsTheModel(t *testing.T) {
	testRunFlagReachesTheRow(t, []string{"--model", "anthropic/claude-sonnet-5"},
		func(t *testing.T, j store.Job) {
			if j.Model != "anthropic/claude-sonnet-5" {
				t.Errorf("the row carries model %q, want anthropic/claude-sonnet-5", j.Model)
			}
		})
}

// TestRunSendsTheWallClock: --max-duration reaches the row.
//
// @decision(DL-049)
func TestRunSendsTheWallClock(t *testing.T) {
	testRunFlagReachesTheRow(t, []string{"--max-duration", "45m"},
		func(t *testing.T, j store.Job) {
			if j.MaxDuration != 45*time.Minute {
				t.Errorf("the row carries a %s clock, want 45m", j.MaxDuration)
			}
		})
}

// sandbox.job_max_duration is sent as a preference: a flag wins over it,
// and the server clamps it to the caller's limit rather than refusing.
//
// @decision(DL-049)
func TestRunSendsTheConfiguredWallClockAsAPreference(t *testing.T) {
	_, j := runFlagRow(t, nil, "sandbox:\n  job_max_duration: 2h\n")
	if j.MaxDuration != 2*time.Hour {
		t.Errorf("the configured clock reached the row as %s, want 2h", j.MaxDuration)
	}
	_, j = runFlagRow(t, []string{"--max-duration", "45m"}, "sandbox:\n  job_max_duration: 2h\n")
	if j.MaxDuration != 45*time.Minute {
		t.Errorf("with a flag the row carries %s, want the flag's 45m", j.MaxDuration)
	}
	_, j = runFlagRow(t, nil, "sandbox:\n  job_max_duration: 9h\n")
	if j.MaxDuration != server.DefaultQuotas.MaxJobDuration {
		t.Errorf("a configured clock over the limit reached the row as %s, want it clamped to %s",
			j.MaxDuration, server.DefaultQuotas.MaxJobDuration)
	}
}

// The detail shows the clock: when it runs out while it counts, taking
// the time already used into account, and what is left while a person
// steers.
//
// @decision(DL-049)
func TestJobDetailShowsTheWallClock(t *testing.T) {
	started := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		state   store.JobState
		started *time.Time
		want    string
		absent  string
	}{
		{store.JobRunning, &started, "2026-09-26T10:40:00Z", "Left"},
		{store.JobSteered, nil, "40m0s, paused while steered", "Ends by"},
		{store.JobSucceeded, nil, "1h0m0s", "Left"},
	} {
		var out bytes.Buffer
		env := &Env{Out: &out, Err: &bytes.Buffer{}}
		j := server.JobView{ID: "01JOB", Kind: store.JobKindReport, State: tc.state,
			SessionID: "01SESS", Goal: "g", MaxDuration: "1h0m0s", ClockUsed: "20m0s", ClockStartedAt: tc.started}
		if err := writeJobDetail(env, j); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if !strings.Contains(got, "1h0m0s") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: the detail lacks %q:\n%s", tc.state, tc.want, got)
		}
		if strings.Contains(got, tc.absent) {
			t.Errorf("%s: the detail shows %q:\n%s", tc.state, tc.absent, got)
		}
	}
}

// TestRunSendsTheGitAccess: --git-access reaches the job's session, and
// sandbox.job_git_access goes as a preference, never the interactive key.
//
// @decision(DL-043)
func TestRunSendsTheGitAccess(t *testing.T) {
	st, j := runFlagRow(t, []string{"--git-access", "all"}, "")
	sess, err := st.Sessions().Get(context.Background(), auth.LocalID, j.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Request.GitAccess != store.GitAccessAll || sess.Request.GitAccessPreferred {
		t.Errorf("the flag reached the session as %q (preferred %v)", sess.Request.GitAccess, sess.Request.GitAccessPreferred)
	}
	st, j = runFlagRow(t, nil, "sandbox:\n  git_access: all\n  job_git_access: repos\n")
	sess, err = st.Sessions().Get(context.Background(), auth.LocalID, j.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Request.GitAccess != store.GitAccessRepos || !sess.Request.GitAccessPreferred {
		t.Errorf("config reached the session as %q (preferred %v)", sess.Request.GitAccess, sess.Request.GitAccessPreferred)
	}
}

func testRunFlagReachesTheRow(t *testing.T, flags []string, check func(*testing.T, store.Job)) {
	t.Helper()
	_, j := runFlagRow(t, flags, "")
	check(t, j)
}

// runFlagRow runs `spov jobs create` with flags, and extra appended to the
// project file, and returns the store and the one job it recorded.
func runFlagRow(t *testing.T, flags []string, extra string) (store.Store, store.Job) {
	t.Helper()
	f := newAPIFixtureWithConfig(t, defaultConfig+extra)
	f.deps.Jobs = &jobs.Supervisor{
		Store: f.deps.Store, Provider: f.provider,
		Queue: func(context.Context, jobs.JobArgs) error { return nil },
		Warn:  func(string) {},
	}
	// DL-013 refuses a model the deployment holds no key for, and
	// --model is one of the flags driven through here.
	f.deps.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "sk-fixture"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(f.deps, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.Listener = ln
	hs.Start()
	t.Cleanup(hs.Close)
	env := f.env
	env.Client = client.New(hs.URL, f.token)

	args := append([]string{"jobs", "create", "fix the failing test", "--repo", "alice/spov"}, flags...)
	if _, _, err := runWith(t, env, args...); err != nil {
		t.Fatalf("run: %v", err)
	}

	jobs, err := f.deps.Store.Jobs().List(context.Background(), auth.LocalID, store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("stored %d jobs, want 1", len(jobs))
	}
	return f.deps.Store, jobs[0]
}

// A GitLab member reads as a merge request with GitLab's ! number.
//
// @decision(DL-051)
func TestAGitLabDeliverableIsAMergeRequest(t *testing.T) {
	d := server.DeliverableView{Kind: store.DeliverablePR, Forge: "gitlab", Repository: "g/s/r", Platform: "gl", Number: 4, URL: "https://x/4"}
	if got := deliverableLine(d); !strings.HasPrefix(got, "merge request !4 in g/s/r") {
		t.Errorf("line = %q", got)
	}
	d.Forge = "github"
	if got := deliverableLine(d); !strings.HasPrefix(got, "pull request #4") {
		t.Errorf("line = %q", got)
	}
}

// The inventory lists each member under its status and then what the
// agent said it left unfinished; a job with neither prints nothing.
//
// @decision(DL-051)
func TestTheInventoryListsMembersAndWhatWasLeftUnfinished(t *testing.T) {
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}}
	if err := writeDeliverables(env, server.JobView{}); err != nil || out.Len() != 0 {
		t.Fatalf("empty job printed %q, err %v", out.String(), err)
	}
	j := server.JobView{
		Deliverables: []server.DeliverableView{
			{Kind: store.DeliverableReport, Status: "verified", Path: "findings.md", Title: "Findings", Size: 2048},
			{Kind: store.DeliverablePR, Status: "missing", Repository: "o/r", Branch: "fix", Platform: "gh"},
		},
		Unfinished: "the migration",
	}
	if err := writeDeliverables(env, j); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Deliverables:", "verified", "findings.md", "2.0 KiB", "pull request from fix in o/r (gh)", "Left unfinished:\nthe migration"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestSizesReadInTheirLargestUnit(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 3 << 20: "3.0 MiB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
