package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCloneURLsCannotBecomeGitOptions is a security regression test.
// Shell quoting is not enough on its own: a quoted "--upload-pack=CMD" is
// a single shell word but still an option to git, and git executes its
// value. Clone URLs and directory names both come from remote APIs.
//
// @decision(DL-036)
func TestCloneURLsCannotBecomeGitOptions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	marker := filepath.Join(t.TempDir(), "pwned")
	hostile := "--upload-pack=touch " + marker + " && git-upload-pack"

	t.Run("validation rejects it outright", func(t *testing.T) {
		e := Entrypoint{Clones: []Clone{{URL: hostile, Dir: "r"}}, Landing: "r", Port: DefaultPort}
		if err := e.Validate(); err == nil {
			t.Fatal("a URL that git would read as an option was accepted")
		}
	})

	// Even if validation were bypassed, the rendered command must place
	// "--" before the URL so git treats it as an operand.
	t.Run("the rendered command separates options from operands", func(t *testing.T) {
		e := Entrypoint{
			Clones:  []Clone{{URL: "https://example.com/r.git", Dir: "r"}},
			Landing: "r",
			Port:    DefaultPort,
		}
		script, err := e.Script()
		if err != nil {
			t.Fatal(err)
		}
		var clone string
		for _, l := range strings.Split(script, "\n") {
			if strings.HasPrefix(l, "git clone") {
				clone = l
			}
		}
		if !strings.Contains(clone, " -- ") {
			t.Fatalf("clone command has no -- separator: %q", clone)
		}

		// Run the real thing with the hostile URL substituted after the
		// separator: git must treat it as a repository name and fail,
		// not execute it.
		probe := strings.Replace(clone, "'https://example.com/r.git'", shellQuote(hostile), 1)
		probe = strings.Replace(probe, "'/workspace/r'", shellQuote(filepath.Join(t.TempDir(), "out")), 1)

		cmd := exec.Command("bash", "-c", probe)
		cmd.Dir = t.TempDir()
		out, _ := cmd.CombinedOutput()

		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("git executed the injected command; output: %s", out)
		}
	})
}
