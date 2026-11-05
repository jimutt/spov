package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	gopath "path"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/opencode/contract"
	"github.com/jimutt/spov/internal/store"
)

// openCapture records what the supervisor sent.
type openCapture struct {
	path   string
	body   map[string]any
	cookie string
	user   string
	pass   string
	status int
}

func newOpenServer(t *testing.T, c *openCapture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.path = r.URL.Path
		if ck, err := r.Cookie("_modal_connect_token"); err == nil {
			c.cookie = ck.Value
		}
		c.user, c.pass, _ = r.BasicAuth()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &c.body)
		if c.status == 0 {
			c.status = http.StatusOK
		}
		w.WriteHeader(c.status)
	})))
}

func testConn(url string) Connection {
	return Connection{URL: url, Token: "tok", User: "u", Password: "p"}
}

// The opening turn must be a synthetic. A prompt arrives as a `user`
// inbox item, indistinguishable from a human typing, and every job
// would enter steered on its own first turn.
func TestTheOpeningTurnIsASyntheticAndNotAPrompt(t *testing.T) {
	var c openCapture
	srv := newOpenServer(t, &c)
	defer srv.Close()

	job := store.Job{ID: "job_1", Goal: "fix the flaky test"}
	if err := openJob(context.Background(), testConn(srv.URL), "conv_1", job); err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(c.path, "/synthetic") {
		t.Errorf("the opening turn went to %q, which is not the synthetic endpoint", c.path)
	}
	if strings.Contains(c.path, "/prompt") {
		t.Errorf("the opening turn was sent as a prompt: %q", c.path)
	}
	if c.path != "/api/session/conv_1/synthetic" {
		t.Errorf("path = %q, want /api/session/conv_1/synthetic", c.path)
	}
}

// resume is what schedules execution. Without it the item is enqueued
// and the job never starts.
func TestTheOpeningTurnResumes(t *testing.T) {
	var c openCapture
	srv := newOpenServer(t, &c)
	defer srv.Close()

	job := store.Job{ID: "job_1", Goal: "do the thing"}
	if err := openJob(context.Background(), testConn(srv.URL), "conv_1", job); err != nil {
		t.Fatal(err)
	}
	if resume, _ := c.body["resume"].(bool); !resume {
		t.Errorf("resume = %v, want true; without it nothing runs", c.body["resume"])
	}
	if got, _ := c.body["text"].(string); got != "do the thing" {
		t.Errorf("text = %q, want the goal", got)
	}
}

// The opening text carries the goal. DL-047 changes this one function
// and nothing else.
func TestTheOpeningTextIsTheGoal(t *testing.T) {
	job := store.Job{Goal: "investigate the flake"}
	if got := openingText(job); !strings.Contains(got, "investigate the flake") {
		t.Errorf("openingText = %q, want it to carry the goal", got)
	}
}

// The connect credentials must travel, or the sandbox refuses the write
// and the job never opens.
func TestTheOpeningTurnCarriesItsCredentials(t *testing.T) {
	var c openCapture
	srv := newOpenServer(t, &c)
	defer srv.Close()

	if err := openJob(context.Background(), testConn(srv.URL), "conv_1", store.Job{Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	if c.cookie != "tok" {
		t.Errorf("connect token = %q, want tok", c.cookie)
	}
	if c.user != "u" || c.pass != "p" {
		t.Errorf("basic auth = %q/%q, want u/p", c.user, c.pass)
	}
}

// A sandbox that refuses the opening turn must surface as an error
// rather than a job that looks started and never runs.
func TestARefusedOpeningIsAnError(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusNotFound,
		http.StatusInternalServerError, http.StatusBadRequest,
	} {
		c := openCapture{status: status}
		srv := newOpenServer(t, &c)
		err := openJob(context.Background(), testConn(srv.URL), "conv_1", store.Job{Goal: "g"})
		srv.Close()
		if err == nil {
			t.Errorf("status %d returned no error", status)
		}
	}
}

// Both are "accepted": S5 measured 200 from the synthetic endpoint, and
// 204 is what the command endpoint answers.
func TestAnAcceptedOpeningIsNotAnError(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		c := openCapture{status: status}
		srv := newOpenServer(t, &c)
		err := openJob(context.Background(), testConn(srv.URL), "conv_1", store.Job{Goal: "g"})
		srv.Close()
		if err != nil {
			t.Errorf("status %d returned %v", status, err)
		}
	}
}

// A redirect is the one way this credential could be carried to a host
// it was not minted for.
func TestTheOpeningTurnRefusesARedirect(t *testing.T) {
	var reached bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/session/x/synthetic", http.StatusTemporaryRedirect)
	})))
	defer srv.Close()

	err := openJob(context.Background(), testConn(srv.URL), "conv_1", store.Job{Goal: "g"})
	if reached {
		t.Error("the credential was carried to the redirect target")
	}
	if err == nil {
		t.Error("a redirect was treated as success")
	}
}

// A report read is one GET against a sandbox about to be terminated,
// relative to the out directory, carrying the same credentials the write
// does.
//
// @decision(DL-045) @decision(DL-018) @decision(DL-051)
func TestReadOutFileFetchesTheAgreedPath(t *testing.T) {
	var gotPath, gotDir string
	var gotAuth bool
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotDir = r.URL.Query().Get("location[directory]")
		_, _, gotAuth = r.BasicAuth()
		_, _ = w.Write([]byte("# the report"))
	})))
	defer srv.Close()

	body, err := readOutFile(context.Background(),
		Connection{URL: srv.URL, Token: "t", User: "u", Password: "p"}, "images/a #1.png", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "# the report" {
		t.Errorf("body = %q", body)
	}
	if gotPath != "/api/fs/read/images/a%20%231.png" {
		t.Errorf("path = %q, want each segment escaped", gotPath)
	}
	if gotDir != OutDir {
		t.Errorf("location[directory] = %q, want %q", gotDir, OutDir)
	}
	if !gotAuth {
		t.Error("the read carried no credentials")
	}
}

// A listing that answers anything but 200 is a read that broke, since
// the directory exists from the sandbox's start.
//
// @decision(DL-051)
func TestAListingThatFailsIsNotAnEmptyDirectory(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusNotFound} {
		srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})))
		got, err := listOutDir(context.Background(), Connection{URL: srv.URL}, "")
		srv.Close()
		if err == nil || errors.Is(err, jobs.ErrNoFile) {
			t.Errorf("status %d: %v, %v; want a broken read", status, got, err)
		}
	}
}

// The cap is enforced by the reader, not trusted from the sandbox: the
// bytes read are kept and the truncation is reported.
//
// @decision(DL-045)
func TestReadingASandboxFileEnforcesItsCap(t *testing.T) {
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 100))
	})))
	defer srv.Close()

	body, err := readSandboxFile(context.Background(), Connection{URL: srv.URL}, "f", "", 10)
	if !errors.Is(err, jobs.ErrFileTruncated) {
		t.Fatalf("error = %v, want jobs.ErrFileTruncated", err)
	}
	if len(body) != 10 {
		t.Errorf("kept %d bytes, want the cap of 10", len(body))
	}
	// A file exactly at the cap is not truncated.
	if _, err := readSandboxFile(context.Background(), Connection{URL: srv.URL}, "f", "", 100); err != nil {
		t.Errorf("a file exactly at the cap reported %v", err)
	}
}

// A sandbox that answers something other than 200 or a missing-file
// status is reported with its status. 500 is not tested here, since it
// means "no such file" and is covered by its own test.
//
// @decision(DL-045)
func TestReadingASandboxFileReportsAnUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})))
	defer srv.Close()

	_, err := readSandboxFile(context.Background(), Connection{URL: srv.URL}, "f", "", 10)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %v, want it to name the status", err)
	}
}

// The evidence read names the plugin's own file, and reaches it the way
// the sandbox accepts: the directory as the location, the name in the
// path.
//
// @decision(DL-045)
func TestReadEvidenceUsesThePluginsPath(t *testing.T) {
	var gotPath, gotDir string
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotDir = r.URL.Query().Get("location[directory]")
		_, _ = w.Write([]byte(`{"event":"loop.stopped"}`))
	})))
	defer srv.Close()

	if _, err := readEvidence(context.Background(), Connection{URL: srv.URL}, EvidencePath); err != nil {
		t.Fatal(err)
	}
	dir, name := gopath.Split(EvidencePath)
	if gotDir != strings.TrimSuffix(dir, "/") {
		t.Errorf("location[directory] = %q, want %q", gotDir, strings.TrimSuffix(dir, "/"))
	}
	if !strings.HasSuffix(gotPath, name) {
		t.Errorf("path = %q, want it to name %q", gotPath, name)
	}
}

// The evidence is read relative to its directory, never as an absolute
// path, which answers 500.
//
// @decision(DL-045)
func TestTheEvidenceIsReadRelativeToItsDirectory(t *testing.T) {
	var gotPath, gotDir string
	srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotDir = r.URL.Query().Get("location[directory]")
		_, _ = w.Write([]byte(`{"event":"loop.stopped"}`))
	})))
	defer srv.Close()

	if _, err := readEvidence(context.Background(), Connection{URL: srv.URL}, "/tmp/spov-goal.jsonl"); err != nil {
		t.Fatal(err)
	}
	if gotDir != "/tmp" {
		t.Errorf("location[directory] = %q, want the file's own directory", gotDir)
	}
	if strings.Contains(gotPath, "/tmp/") {
		t.Errorf("path = %q, want just the file name;"+
			" an absolute path answers 500 and collects nothing", gotPath)
	}
	if !strings.HasSuffix(gotPath, "spov-goal.jsonl") {
		t.Errorf("path = %q, want it to name the evidence file", gotPath)
	}
}

// A missing file answers 500, not 404. Both are read as "no such file".
//
// @decision(DL-045) @decision(DL-050)
func TestAMissingFileIsNoFileWhicheverStatusItUses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(contract.Checked(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			})))
			defer srv.Close()
			if _, err := readOutFile(context.Background(),
				Connection{URL: srv.URL}, "report.md", 1<<20); !errors.Is(err, jobs.ErrNoFile) {
				t.Fatalf("status %d gave %v, want jobs.ErrNoFile", status, err)
			}
		})
	}
}

// The loop command reaches the plugin's own command endpoint, with the
// command name and the argument the JavaScript's switch matches. A
// wrong path, command name or argument all fail the same silent way: a
// pause that paused nothing looks exactly like one that worked.
//
// @decision(DL-054) @decision(DL-039)
func TestTheLoopCommandGoesToTheCommandEndpoint(t *testing.T) {
	for _, arg := range []string{LoopPause, LoopResume} {
		c := openCapture{status: http.StatusNoContent}
		srv := newOpenServer(t, &c)

		if err := sendLoopCommand(context.Background(), testConn(srv.URL), "conv_1", arg); err != nil {
			srv.Close()
			t.Fatalf("%s: %v", arg, err)
		}
		srv.Close()

		if c.path != "/api/session/conv_1/command" {
			t.Errorf("%s went to %q, want /api/session/conv_1/command", arg, c.path)
		}
		if got := c.body["name"]; got != opencode.PluginCommand {
			t.Errorf("name = %v, want %q", got, opencode.PluginCommand)
		}
		if got := c.body["text"]; got != arg {
			t.Errorf("text = %v, want %q", got, arg)
		}
		// The credential travels the same way every other sandbox
		// write's does, and never in the URL.
		if c.cookie != "tok" || c.pass != "p" {
			t.Errorf("%s: cookie = %q, password set = %v", arg, c.cookie, c.pass != "")
		}
		if strings.Contains(c.path, "tok") {
			t.Errorf("%s: the connect token is in the path %q", arg, c.path)
		}
	}
}

// A sandbox that refuses the command is an error rather than a silent
// success, so a pause that did not land is at least warned about and a
// resume that did not land is reported to the person who asked for it.
//
// @decision(DL-054)
func TestALoopCommandTheSandboxRefusesIsAnError(t *testing.T) {
	c := openCapture{status: http.StatusInternalServerError}
	srv := newOpenServer(t, &c)
	defer srv.Close()

	if err := sendLoopCommand(context.Background(), testConn(srv.URL), "conv_1", LoopResume); err == nil {
		t.Error("a 500 from the sandbox was reported as a successful resume")
	}
}
