package opencode

import (
	"os/exec"
	"strings"
	"testing"
)

// TestScriptQuotingAgainstRealShell renders a script containing hostile
// input and checks that bash parses each clone as exactly one argument.
func TestScriptQuotingAgainstRealShell(t *testing.T) {
	hostile := []string{
		"https://example.com/r.git; touch /tmp/spov-pwned",
		"https://example.com/$(touch /tmp/spov-pwned2).git",
		"https://example.com/`touch /tmp/spov-pwned3`.git",
		"https://user:p@ss'word@example.com/r.git",
	}
	for _, url := range hostile {
		t.Run(url, func(t *testing.T) {
			e := Entrypoint{Clones: []Clone{{URL: url, Dir: "r"}}, Landing: "r", Port: DefaultPort}
			script, err := e.Script()
			if err != nil {
				t.Fatal(err)
			}
			var line string
			for _, l := range strings.Split(script, "\n") {
				if strings.HasPrefix(l, "git clone") {
					line = l
				}
			}
			// Replace `git clone --depth 1` with a printf that reports
			// each argument on its own line, then run it for real.
			probe := strings.Replace(line, "git clone --depth 1 --", `printf '%s\n'`, 1)
			out, err := exec.Command("bash", "-c", probe).Output()
			if err != nil {
				t.Fatalf("probe failed: %v", err)
			}
			got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
			if len(got) != 2 {
				t.Fatalf("expected 2 arguments, got %d: %q", len(got), got)
			}
			if got[0] != url {
				t.Errorf("URL was mangled: got %q, want %q", got[0], url)
			}
			if got[1] != "/workspace/r" {
				t.Errorf("dir = %q", got[1])
			}
		})
	}
}

// runCloneScript renders the real entrypoint and runs its clone section
// under bash with git stubbed, so failure handling is exercised against
// the shell rather than asserted about a string.
//
// @decision(DL-036)
func runCloneScript(t *testing.T, clones []Clone, failDir string) (int, string) {
	t.Helper()
	e := Entrypoint{Clones: clones, Landing: clones[0].Dir, Port: 4096}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}
	// Replace the cp and exec lines so only the clone section runs.
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		switch {
		case strings.HasPrefix(line, "cp "), strings.HasPrefix(line, "exec "), strings.HasPrefix(line, "cd "),
			strings.Contains(line, ScratchDir):
			continue
		}
		kept = append(kept, line)
	}
	body := strings.Join(kept, "\n")

	stub := `git() {
  target="$6"
  case "$target" in
    *` + failDir + `) return 1 ;;
  esac
  return 0
}
`
	full := stub + body + "\necho REACHED_END\n"
	cmd := exec.Command("bash", "-c", full)
	out, _ := cmd.CombinedOutput()
	return cmd.ProcessState.ExitCode(), string(out)
}

// TestScriptFailsWhenACloneFails is the property parallelism must not
// cost: a bare `wait` reports success even when a job failed, which
// would start the server over a missing repository.
//
// @decision(DL-036)
func TestScriptFailsWhenACloneFails(t *testing.T) {
	clones := []Clone{
		{URL: "https://x/a.git", Dir: "a"},
		{URL: "https://x/b.git", Dir: "b"},
		{URL: "https://x/c.git", Dir: "c"},
	}
	code, out := runCloneScript(t, clones, "b")
	t.Logf("exit=%d out=%q", code, out)
	if code == 0 {
		t.Error("a failing clone did not fail the script")
	}
	if strings.Contains(out, "REACHED_END") {
		t.Error("execution continued past the failed clone")
	}
}

func TestScriptSucceedsWhenAllClonesSucceed(t *testing.T) {
	clones := []Clone{
		{URL: "https://x/a.git", Dir: "a"},
		{URL: "https://x/b.git", Dir: "b"},
	}
	code, out := runCloneScript(t, clones, "nothing")
	t.Logf("exit=%d out=%q", code, out)
	if code != 0 {
		t.Errorf("exit=%d, want 0", code)
	}
	if !strings.Contains(out, "REACHED_END") {
		t.Error("the script did not reach the end")
	}
}

// TestScriptFailsInASecondBatch: with more clones than the concurrency
// cap the waits are emitted per batch, so each one has to propagate.
//
// @decision(DL-036)
func TestScriptFailsInASecondBatch(t *testing.T) {
	var clones []Clone
	for _, d := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		clones = append(clones, Clone{URL: "https://x/" + d + ".git", Dir: d})
	}
	code, out := runCloneScript(t, clones, "g")
	t.Logf("exit=%d out=%q", code, out)
	if code == 0 {
		t.Error("a failure in the second batch did not fail the script")
	}
}
