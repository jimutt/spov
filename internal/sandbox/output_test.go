package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// endless never stops writing, as a replaced printenv would not.
type endless struct{ read int }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.read += len(p)
	return len(p), nil
}

// TestExecOutputIsReadOnlyToItsBound is DL-130: output a sandbox's
// program writes is never read past the caller's limit, and an output
// over it is an error, not a truncated answer.
func TestExecOutputIsReadOnlyToItsBound(t *testing.T) {
	src := &endless{}
	if _, err := readBounded(src, 1024); !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("an endless stream: err = %v, want ErrOutputTooLarge", err)
	}
	// io.ReadAll grows its buffer in steps, so the read may run a
	// little past the limit, but never far.
	if src.read > 8*1024 {
		t.Errorf("read %d bytes of a stream bounded at 1024", src.read)
	}

	got, err := readBounded(strings.NewReader("secret\n"), 1024)
	if err != nil || got != "secret\n" {
		t.Errorf("a short stream = %q, %v", got, err)
	}
	exact := strings.Repeat("a", 1024)
	if got, err := readBounded(strings.NewReader(exact), 1024); err != nil || got != exact {
		t.Errorf("a stream of exactly the limit: len %d, %v", len(got), err)
	}
	if got, err := readBounded(nil, 1024); err != nil || got != "" {
		t.Errorf("no stream = %q, %v", got, err)
	}
	// No limit is the default one, never no bound.
	if _, err := readBounded(&endless{}, 0); !errors.Is(err, ErrOutputTooLarge) {
		t.Errorf("limit 0: err = %v, want ErrOutputTooLarge", err)
	}
}

// TestTheFakeRefusesOutputOverTheBound keeps the fake honest: a test
// that cans a large result sees what a real sandbox would answer.
func TestTheFakeRefusesOutputOverTheBound(t *testing.T) {
	p := NewFake()
	h, err := p.Create(t.Context(), validSpec())
	if err != nil {
		t.Fatal(err)
	}
	p.ExecResults["big"] = ExecResult{Stdout: strings.Repeat("x", MaxExecOutput+1)}
	if _, err := h.Exec(execCtx(t), []string{"big"}); !errors.Is(err, ErrOutputTooLarge) {
		t.Errorf("Exec over the default bound: err = %v", err)
	}
	res, err := h.ExecBounded(execCtx(t), []string{"big"}, nil, MaxExecOutput+1)
	if err != nil || len(res.Stdout) != MaxExecOutput+1 {
		t.Errorf("ExecBounded with room: len %d, %v", len(res.Stdout), err)
	}
	if _, err := BoundOutput(ExecResult{Stderr: "ab"}, 1); !errors.Is(err, ErrOutputTooLarge) {
		t.Errorf("stderr over the bound: err = %v", err)
	}
}
