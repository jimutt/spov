package sandbox

import (
	"errors"
	"fmt"
	"io"
)

// MaxExecOutput is how much of each output stream Exec and ExecInput
// read. The program that writes it runs inside the sandbox, where the
// agent can replace it.
//
// @decision(DL-130)
const MaxExecOutput = 64 << 10

// ErrOutputTooLarge is a command inside a sandbox that wrote more than
// its caller's bound. Nothing it wrote is returned.
//
// @decision(DL-130)
var ErrOutputTooLarge = errors.New("sandbox: the command's output exceeds its limit")

// readBounded reads r to its end when that is within limit bytes, and
// otherwise stops one byte past it and answers ErrOutputTooLarge.
//
// @decision(DL-130)
func readBounded(r io.Reader, limit int) (string, error) {
	if r == nil {
		return "", nil
	}
	if limit <= 0 {
		limit = MaxExecOutput
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if len(b) > limit {
		return "", fmt.Errorf("%w (%d bytes)", ErrOutputTooLarge, limit)
	}
	// A stream that breaks after the process exited still yields what
	// arrived, as it did before the bound.
	_ = err
	return string(b), nil
}

// BoundOutput applies limit to a result some other Handle produced, so
// a fake or a local shell refuses what a real sandbox would.
//
// @decision(DL-130)
func BoundOutput(res ExecResult, limit int) (ExecResult, error) {
	if limit <= 0 {
		limit = MaxExecOutput
	}
	if len(res.Stdout) > limit || len(res.Stderr) > limit {
		return ExecResult{}, fmt.Errorf("%w (%d bytes)", ErrOutputTooLarge, limit)
	}
	return res, nil
}

// copyBounded copies r to w as it arrives, at most limit bytes. A
// stream with more answers ErrOutputTooLarge, with the first limit
// bytes already written and none past them.
//
// @decision(DL-175)
func copyBounded(w io.Writer, r io.Reader, limit int64) (int64, error) {
	if r == nil {
		return 0, nil
	}
	n, err := io.CopyBuffer(w, io.LimitReader(r, limit), make([]byte, streamChunk))
	if err != nil {
		return n, err
	}
	if extra, _ := io.ReadFull(r, make([]byte, 1)); extra > 0 {
		return n, fmt.Errorf("%w (%d bytes)", ErrOutputTooLarge, limit)
	}
	return n, nil
}

// streamChunk is the most ExecStream holds of a stream at once.
//
// @decision(DL-175)
const streamChunk = 64 << 10
