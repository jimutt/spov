package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// hostile is what an agent can write: a screen-clearing sequence, an
// OSC title and clipboard write, a carriage return that overwrites a
// line, a C1 control and a right-to-left override.
const hostile = "ok\x1b[2J\x1b]0;pwned\x07\x1b]52;c;ZXZpbA==\x07\rVerdict: succeeded\u009b31m\u202egnp.exe"

func hasControl(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) {
			return true
		}
	}
	return false
}

// @decision(DL-136)
func TestPrintableMakesControlCharactersVisibleAndKeepsLayout(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"plain text is untouched":     {"a plain line\nwith a tab\there", "a plain line\nwith a tab\there"},
		"unicode is untouched":        {"räksmörgås 日本語 🙂", "räksmörgås 日本語 🙂"},
		"ESC is shown":                {"a\x1b[31mred", `a\x1b[31mred`},
		"BEL and NUL are shown":       {"a\x07b\x00c", `a\x07b\x00c`},
		"DEL is shown":                {"a\x7fb", `a\x7fb`},
		"C1 controls are shown":       {"a\u009bb", `a\u009bb`},
		"a lone CR is shown":          {"one\rtwo", `one\x0dtwo`},
		"CRLF reads as a newline":     {"one\r\ntwo", "one\ntwo"},
		"a bidi override is shown":    {"a\u202eb", `a\u202eb`},
		"a stray byte is shown":       {"a\x9bb", `a\x9bb`},
		"a line separator is shown":   {"a\u2028b", `a\u2028b`},
		"the empty string is empty":   {"", ""},
		"a truncated rune is visible": {"a\xe2\x82", `a\xe2\x82`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Printable(tc.in); got != tc.want {
				t.Errorf("Printable(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if got := Printable(hostile); hasControl(got) {
		t.Errorf("the hostile text still holds a control character: %q", got)
	}
	if got := printableLine("a\nb\tc\x1b"); got != `a\nb\tc\x1b` {
		t.Errorf("printableLine = %q", got)
	}
}

func hostileJob() server.JobView {
	return server.JobView{
		ID: "01JOB", Kind: store.JobKindReport, State: store.JobFailed, SessionID: "01SESS",
		Goal:       "goal " + hostile,
		Error:      "error " + hostile,
		Verdict:    "- conduct: failed (" + hostile + ")",
		Unfinished: "unfinished " + hostile,
		Deliverables: []server.DeliverableView{
			{Kind: store.DeliverableReport, Status: store.DeliverableVerified, Path: "/workspace/out/" + hostile + ".md", Title: hostile, Detail: hostile},
			{Kind: store.DeliverablePR, Status: store.DeliverableMissing, Branch: hostile, Repository: "a/b", Platform: "p", Detail: hostile},
		},
	}
}

// @decision(DL-136)
func TestEveryAgentWrittenFieldOfAJobIsPrintedSafely(t *testing.T) {
	var out bytes.Buffer
	env := &Env{Out: &out, Err: &bytes.Buffer{}}
	if err := writeJobDetail(env, hostileJob()); err != nil {
		t.Fatal(err)
	}
	if hasControl(out.String()) {
		t.Errorf("`jobs show` printed a control character:\n%q", out.String())
	}
	if !strings.Contains(out.String(), `\x1b`) {
		t.Errorf("the escape is not shown:\n%q", out.String())
	}

	out.Reset()
	j := hostileJob()
	if err := writeJobTable(env, []server.JobView{j}, j.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if hasControl(out.String()) {
		t.Errorf("`jobs` printed a control character:\n%q", out.String())
	}
}

// @decision(DL-136)
func TestJobsWaitPrintsAnAgentsWordsSafely(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "job_1", "session_id": "s", "kind": "pr", "state": "failed", "goal": "g", "error": hostile, "verdict": hostile})
	}))
	t.Cleanup(api.Close)
	env := Env{Client: client.New(api.URL, "tok"), JobPoll: time.Millisecond}
	out, _, _ := runWith(t, env, "jobs", "wait", "job_1")
	if hasControl(out) || !strings.Contains(out, `\x1b`) {
		t.Errorf("`jobs wait` printed:\n%q", out)
	}
	// --json is JSON-encoded, which escapes control characters itself.
	out, _, _ = runWith(t, env, "jobs", "wait", "job_1", "--json")
	if hasControl(out) {
		t.Errorf("--json printed a control character:\n%q", out)
	}
}

func reportServer(t *testing.T) *client.Client {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/file") {
			_, _ = io.WriteString(w, "# Report\n"+hostile+"\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"job_1","session_id":"s","kind":"report","state":"succeeded","goal":"g","deliverables":[`+
			`{"kind":"report","status":"verified","path":"/workspace/out/r.md","title":"r"}]}`)
	}))
	t.Cleanup(api.Close)
	return client.New(api.URL, "tok")
}

// A report goes raw to a pipe or a file, where a script wants it, and
// with its control characters shown to a terminal.
//
// @decision(DL-136)
func TestAReportIsPrintedRawToAPipeAndSafelyToATerminal(t *testing.T) {
	c := reportServer(t)
	for name, tc := range map[string]struct {
		terminal bool
		safe     bool
	}{"a pipe": {false, false}, "a terminal": {true, true}} {
		t.Run(name, func(t *testing.T) {
			env := Env{Client: c, StdoutIsTerminal: func() bool { return tc.terminal }}
			out, _, err := runWith(t, env, "jobs", "report", "job_1")
			if err != nil {
				t.Fatal(err)
			}
			if got := !hasControl(out); got != tc.safe {
				t.Errorf("safe = %v, want %v:\n%q", got, tc.safe, out)
			}
		})
	}
}

// @decision(DL-136)
func TestALaunchNoteAndAFailureReasonAreFilteredBeforeTheyAreShown(t *testing.T) {
	var errBuf bytes.Buffer
	env := &Env{Out: &bytes.Buffer{}, Err: &errBuf}
	rep := &launchReporter{env: env}
	rep.Note(server.NoteEvent{Message: hostile})
	if got := errBuf.String(); hasControl(got) || !strings.Contains(got, `\x1b`) {
		t.Errorf("the launch note reached the terminal as %q", got)
	}
}

// @decision(DL-136)
func TestSyncBackListsWhatChangedAndCapsThePaths(t *testing.T) {
	res := git.SyncResult{InPlace: true, Branch: "main", Commits: 2, RangeFrom: "abc123456789", RangeTo: "def123456789"}
	for i := 0; i < 33; i++ {
		res.Changes = append(res.Changes, git.PathChange{Status: 'M', Path: fmt.Sprintf("f%02d.txt", i)})
	}
	res.Changes[1] = git.PathChange{Status: 'A', Path: "new\x1b[2J.txt"}
	res.Changes[2] = git.PathChange{Status: 'D', Path: "gone.txt"}
	lines := syncChangeLines(res)
	// The range, 30 paths and the "and N more" line.
	if len(lines) != 1+maxSyncPaths+1 {
		t.Fatalf("%d lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if lines[0] != "  2 commits added: abc123456789..def123456789" {
		t.Errorf("range line = %q", lines[0])
	}
	if lines[len(lines)-1] != "  and 3 more" {
		t.Errorf("last line = %q", lines[len(lines)-1])
	}
	all := strings.Join(lines, "\n")
	if hasControl(strings.ReplaceAll(all, "\n", "")) || !strings.Contains(all, `A new\x1b[2J.txt`) || !strings.Contains(all, "D gone.txt") {
		t.Errorf("paths are not shown safely:\n%q", all)
	}

	var errBuf, outBuf bytes.Buffer
	printSyncResult(&Env{Out: &outBuf, Err: &errBuf}, res)
	if outBuf.Len() != 0 {
		t.Errorf("a sync result wrote to stdout: %q", outBuf.String())
	}
	if !strings.Contains(errBuf.String(), "Synced to main") || !strings.Contains(errBuf.String(), "gone.txt") {
		t.Errorf("stderr = %q", errBuf.String())
	}

	few := git.SyncResult{InPlace: true, Branch: "main", Changes: []git.PathChange{{Status: 'M', Path: "a"}}}
	if got := syncChangeLines(few); len(got) != 1 || got[0] != "  M a" {
		t.Errorf("a short list = %q", got)
	}
}

// @decision(DL-136)
func TestJSONOutputEscapesWhatATerminalActsOn(t *testing.T) {
	for name, write := range map[string]func(*bytes.Buffer, any) error{
		"indented": func(b *bytes.Buffer, v any) error { return writeJSON(b, v) },
		"one line": func(b *bytes.Buffer, v any) error { return writeJSONLine(b, v) },
	} {
		var out bytes.Buffer
		if err := write(&out, map[string]string{"text": hostile + "\x7f"}); err != nil {
			t.Fatal(err)
		}
		if hasControl(out.String()) || strings.ContainsRune(out.String(), 0x7f) {
			t.Errorf("%s: raw control character in %q", name, out.String())
		}
		var back map[string]string
		if err := json.Unmarshal(out.Bytes(), &back); err != nil || back["text"] != hostile+"\x7f" {
			t.Errorf("%s: the document does not decode to the same text: %v", name, err)
		}
	}
}
