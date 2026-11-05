package main

import (
	"bytes"
	"strings"
	"testing"
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
