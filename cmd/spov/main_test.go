package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/cli"
)

func TestRunVersion(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"version"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errBuf.String())
	}
	if !strings.HasPrefix(out.String(), "spov ") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestRunReportsErrorsOnStderr(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"config", "show", "--format", "toml"}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(errBuf.String(), "spov: ") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"nope"}, &out, &errBuf); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// A command that exits by an outcome sets the code and, when it has
// nothing to add, prints nothing; every other error is 1.
//
// @decision(DL-103)
func TestExitStatusFollowsAnExitError(t *testing.T) {
	for _, c := range []struct {
		err    error
		code   int
		stderr string
	}{
		{nil, 0, ""},
		{errors.New("boom"), 1, "spov: boom\n"},
		{&cli.ExitError{Code: 4}, 4, ""},
		{&cli.ExitError{Code: 6, Err: errors.New("gave up")}, 6, "spov: gave up\n"},
		{fmt.Errorf("wrapped: %w", &cli.ExitError{Code: 130}), 130, ""},
	} {
		var errBuf bytes.Buffer
		if got := exitStatus(c.err, &errBuf); got != c.code || errBuf.String() != c.stderr {
			t.Errorf("%v: code %d, stderr %q; want %d, %q", c.err, got, errBuf.String(), c.code, c.stderr)
		}
	}
}

// An error can quote what a sandbox wrote, so its text reaches the
// terminal with the control characters made visible.
//
// @decision(DL-136)
func TestExitStatusPrintsAnErrorWithItsControlCharactersShown(t *testing.T) {
	for _, err := range []error{
		errors.New("exec failed: \x1b[2Jstderr\r"),
		&cli.ExitError{Code: 4, Err: errors.New("exec failed: \x1b[2Jstderr\r")},
	} {
		var errBuf bytes.Buffer
		exitStatus(err, &errBuf)
		if got := errBuf.String(); strings.ContainsAny(strings.TrimSuffix(got, "\n"), "\x1b\r") || !strings.Contains(got, `\x1b[2J`) {
			t.Errorf("%v: stderr = %q", err, got)
		}
	}
}
