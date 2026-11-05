package opencode

import (
	"os/exec"
	"strings"
	"testing"
)

// TestEntrypointWritesTheGitIdentity: without this the sandbox has no
// identity and every commit is attributed to git's guess.
//
// @decision(DL-044)
func TestEntrypointWritesTheGitIdentity(t *testing.T) {
	e := Entrypoint{
		Port:     4096,
		Clones:   []Clone{{URL: "https://example.com/o/r.git", Dir: "r"}},
		Landing:  "r",
		GitName:  "Alice Åström",
		GitEmail: "alice@example.com",
	}
	script, err := e.Script()
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`git config --global user.name 'Alice Åström'`,
		`git config --global user.email 'alice@example.com'`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the entrypoint does not contain %q", want)
		}
	}

	// Set before the server starts, or the first commit misses it.
	if strings.Index(script, "user.email") > strings.Index(script, "exec "+imageBinary) {
		t.Error("the identity is configured after the server is started")
	}
}

// TestEntrypointOmitsAnEmptyIdentity: half an identity is a real
// configuration, and an empty `git config` value is worse than none —
// it sets the key to the empty string rather than leaving it unset.
//
// @decision(DL-044)
func TestEntrypointOmitsAnEmptyIdentity(t *testing.T) {
	tests := []struct {
		name      string
		e         Entrypoint
		wantName  bool
		wantEmail bool
	}{
		{"neither", Entrypoint{Port: 4096}, false, false},
		{"only name", Entrypoint{Port: 4096, GitName: "N"}, true, false},
		{"only email", Entrypoint{Port: 4096, GitEmail: "e@x"}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, err := tt.e.Script()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(script, "user.name"); got != tt.wantName {
				t.Errorf("user.name present = %v, want %v", got, tt.wantName)
			}
			if got := strings.Contains(script, "user.email"); got != tt.wantEmail {
				t.Errorf("user.email present = %v, want %v", got, tt.wantEmail)
			}
		})
	}
}

// TestGitIdentityIsQuoted runs the generated lines under bash and reads
// the value back, so what is asserted is what git would receive rather
// than the shape of the escaping.
//
// @decision(DL-044)
func TestGitIdentityIsQuoted(t *testing.T) {
	hostile := []struct{ name, value string }{
		{"single quote", `O'Brien`},
		{"command substitution", `x$(whoami)y`},
		{"semicolon", `A; touch /tmp/spov-pwned`},
		{"backtick", "a`id`b"},
		{"newline", "line1\nline2"},
		{"non-ascii", "Alice Åström"},
	}
	for _, h := range hostile {
		t.Run(h.name, func(t *testing.T) {
			var b strings.Builder
			Entrypoint{GitName: h.value}.writeGitIdentity(&b)

			// Echo what the shell passes as git's final argument.
			script := strings.Replace(b.String(),
				"git config --global user.name ", "printf '%s' ", 1)
			out, err := exec.Command("bash", "-c", script).Output()
			if err != nil {
				t.Fatalf("the generated line failed under bash: %v", err)
			}
			if string(out) != h.value {
				t.Errorf("the shell delivered %q, want %q", out, h.value)
			}
		})
	}
}
