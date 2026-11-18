package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
)

func (f *initFixture) setenv(k, v string) {
	f.env.Environ.(config.MapEnv).Vars[k] = v
}

// onATerminal makes the fixture's stdout and stderr a colour terminal.
func (f *initFixture) onATerminal() {
	f.env.StdoutIsTerminal = func() bool { return true }
	f.env.StderrIsTerminal = func() bool { return true }
	f.setenv("TERM", "xterm-256color")
	f.setenv("COLORTERM", "truecolor")
}

// initCases are runs that between them print an ok step, a missing step
// whose fix failed, a waiting step, and both closing blocks.
var initCases = map[string]func(*initFixture){
	"not ready, with a failed fix": func(f *initFixture) {
		f.accErr = errors.New("bad credentials")
		_, _ = f.provider.EnsureImage(context.Background(), sandbox.ImageSpec{OpenCodeVersion: config.DefaultOpenCodeVersion})
	},
	"ready": func(f *initFixture) { f.ready() },
	"ready without a client": func(f *initFixture) {
		f.ready()
		f.env.LocateBinary = func(context.Context, string) (opencode.Located, error) {
			return opencode.Located{}, errors.New("opencode is not installed")
		}
	},
	"waiting for Modal": func(f *initFixture) {
		f.setenv("MODAL_TOKEN_ID", "")
		f.setenv("MODAL_TOKEN_SECRET", "")
	},
}

// The terminal's output is the plain output with styling added: the same
// lines in the same order, every line within the width.
//
// @decision(DL-152)
func TestInitIsStyledOnATerminalWithTheSameText(t *testing.T) {
	for name, setup := range initCases {
		t.Run(name, func(t *testing.T) {
			plainRun := newInitFixture(t, "")
			setup(plainRun)
			plain, _, _ := plainRun.run()
			plain = strings.ReplaceAll(plain, plainRun.cfgHome, "CONFIG")

			styledRun := newInitFixture(t, "")
			setup(styledRun)
			styledRun.onATerminal()
			styled, _, _ := styledRun.run()
			styled = strings.ReplaceAll(styled, styledRun.cfgHome, "CONFIG")

			if strings.Contains(plain, "\x1b") {
				t.Errorf("the plain output has an escape sequence:\n%q", plain)
			}
			if !strings.Contains(styled, "\x1b[") {
				t.Errorf("the terminal's output is not styled:\n%s", styled)
			}
			if got := stripStyle(styled); got != plain {
				t.Errorf("the terminal's text differs from the plain text.\nterminal:\n%s\nplain:\n%s", got, plain)
			}
			for _, line := range strings.Split(plain, "\n") {
				if w := lipgloss.Width(line); w > initWidth && strings.Contains(strings.TrimSpace(line), " ") {
					t.Errorf("a line is %d columns wide: %q", w, line)
				}
			}
		})
	}
}

// Without a colour terminal there is nothing but text.
//
// @decision(DL-152)
func TestInitIsPlainWithoutAColourTerminal(t *testing.T) {
	for name, setup := range map[string]func(*initFixture){
		"a pipe":   func(f *initFixture) { f.setenv("TERM", "xterm-256color") },
		"NO_COLOR": func(f *initFixture) { f.onATerminal(); f.setenv("NO_COLOR", "1") },
		"TERM=dumb": func(f *initFixture) {
			f.onATerminal()
			f.setenv("TERM", "dumb")
		},
		"no TERM": func(f *initFixture) { f.onATerminal(); delete(f.env.Environ.(config.MapEnv).Vars, "TERM") },
	} {
		t.Run(name, func(t *testing.T) {
			for _, c := range []string{"not ready, with a failed fix", "ready"} {
				f := newInitFixture(t, "")
				initCases[c](f)
				setup(f)
				out, stderr, _ := f.run()
				if strings.Contains(out+stderr+strings.Join(f.asked, ""), "\x1b") {
					t.Errorf("%s: an escape sequence was written:\n%q", c, out)
				}
			}
		})
	}
}

// A failed fix is an error line, and the closing block names the commands.
//
// @decision(DL-152)
func TestInitSetsAFailedFixAndTheNextCommandsApart(t *testing.T) {
	f := newInitFixture(t, "")
	f.accErr = errors.New("bad credentials")
	out, _, _ := f.run()
	if !strings.Contains(out, "\n     error: could not check gh's login with GitHub (bad credentials);") {
		t.Errorf("the failed fix is not an error line:\n%s", out)
	}
	g := newInitFixture(t, "")
	g.ready()
	out, _, _ = g.run()
	if !strings.Contains(out, "\n    cd your-repo && spov fly\n") || !strings.Contains(out, "\n    spov fly --repo owner/name\n") {
		t.Errorf("the next commands are not lines of their own:\n%s", out)
	}
}

// A question reads as one, styled only where stderr is a terminal.
//
// @decision(DL-152)
func TestInitQuestionsArePrompts(t *testing.T) {
	f := newInitFixture(t, "")
	f.onATerminal()
	f.env.StderrIsTerminal = func() bool { return false }
	f.no = "Build it now"
	_, _, _ = f.run()
	if len(f.asked) == 0 {
		t.Fatal("nothing was asked")
	}
	for _, q := range f.asked {
		if strings.Contains(q, "\x1b") {
			t.Errorf("a question to a stderr that is not a terminal is styled: %q", q)
		}
	}
	if q := f.asked[0]; !strings.HasPrefix(q, "     ? ") || !strings.HasSuffix(q, " [y/N] ") {
		t.Errorf("the question is not a prompt: %q", q)
	}

	st := newInitStyle(true, colorprofile.TrueColor, true, initWidth)
	var b strings.Builder
	if got := chooseFrom(strings.NewReader("2\n"), &b, st, "Which `one`?", []string{"a", "b"}); got != 1 {
		t.Errorf("chose %d", got)
	}
	want := "     ? Which `one`?\n       1) a\n       2) b\n       Choose 1-2: "
	if !strings.Contains(b.String(), "\x1b[") || stripStyle(b.String()) != want {
		t.Errorf("the styled choice is %q, want %q styled", stripStyle(b.String()), want)
	}
}

// Prose wraps under its indent, a code word keeps its spaces, and a
// terminal that knows only 256 colours gets them.
//
// @decision(DL-152)
func TestInitProseWrapsUnderItsIndent(t *testing.T) {
	st := newInitStyle(false, colorprofile.NoTTY, true, 30)
	got := st.prose(5, roleText, "run `spov credentials add github` from /home/x/.config/spov, or see https://example.com.")
	want := []string{
		"     run",
		"     `spov credentials add github`",
		"     from",
		"     /home/x/.config/spov, or",
		"     see https://example.com.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("wrapped as\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	colour := newInitStyle(true, colorprofile.ANSI256, true, initWidth)
	line := colour.prose(0, roleDim, "MODAL_TOKEN_ID (/tmp/x), `a b`")[0]
	if strings.Contains(line, "38;2;") || !strings.Contains(line, "38;5;") || stripStyle(line) != "MODAL_TOKEN_ID (/tmp/x), `a b`" {
		t.Errorf("a 256-colour line: %q", line)
	}
	var code int
	for _, w := range colour.words("MODAL_TOKEN_ID (/tmp/x), `a b` OpenCode Go / Zen", roleDim) {
		for _, g := range w {
			if g.role == roleCode {
				code++
			}
		}
	}
	if code != 3 {
		t.Errorf("%d runs are code, want the variable, the path and the span", code)
	}
}
