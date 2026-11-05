package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/redact"
)

const (
	// MaxSandboxReadBytes bounds one response read from a sandbox. A
	// conversation is text; anything near this is not one.
	//
	// @decision(DL-018)
	MaxSandboxReadBytes = 8 << 20
	// SandboxReadTimeout bounds one plain read. The event stream is
	// long-lived and is not bounded by it.
	//
	// @decision(DL-018)
	SandboxReadTimeout = 30 * time.Second
	// MaxLogFrameBytes bounds one frame of the event stream, mirroring
	// the Go client's per-event limit.
	//
	// @decision(DL-018)
	MaxLogFrameBytes = 1 << 20
	// DialHeaderTimeout bounds how long the sandbox may take to answer
	// with headers. The body that follows is unbounded by design.
	//
	// @decision(DL-018)
	DialHeaderTimeout = 30 * time.Second
)

// The whole of this file is the recorder's, and the recorder's alone.
// Anything added here inherits that constraint.
//
// @decision(DL-018)

// ErrSandboxUnauthorized is a sandbox refusing the credential it was
// given. It is its own error because a cached connection has to be able
// to tell "this token no longer works" from every other way a request
// can fail: the first is fixed by minting again, and nothing else is.
//
// @decision(DL-030)
var ErrSandboxUnauthorized = errors.New("server: the sandbox refused the connection credential")

// sandboxStatus turns a response status the caller did not want into an
// error, naming an authentication failure as one.
//
// 403 counts: the connect token is what the edge checks, and a token it
// will not accept can be refused either way.
//
// @decision(DL-030)
func sandboxStatus(what string, code int, status string) error {
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return fmt.Errorf("%s: %w (%s)", what, ErrSandboxUnauthorized, status)
	}
	return fmt.Errorf("%s: the sandbox answered %s", what, status)
}

// The acts sandboxStatus is called for. Each names the ACT and not the
// actor, so the message says "the sandbox" once rather than twice.
const (
	actRead       = "reading the conversation"
	actOpenStream = "opening the event stream"
	actReadFile   = "reading a file"
	actWrite      = "writing"
)

// openSandboxEvents opens the volatile event stream over a connection
// already minted. A held-open stream does not reset Modal's idle
// timeout.
//
// @decision(DL-018) @decision(DL-030) @decision(DL-017)
func openSandboxEvents(ctx context.Context, conn Connection) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, conn.URL+"/api/event", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.AddCookie(&http.Cookie{Name: proxy.TokenCookie, Value: conn.Token})
	req.SetBasicAuth(conn.User, conn.Password)
	// No whole-request timeout — the stream is long-lived by design —
	// but the headers must arrive promptly, or a sandbox that accepts
	// the connection and says nothing leaves the reader on an empty
	// stream with no error to show. Redirects are refused: they are the
	// one way this credential could be carried somewhere it was not
	// minted for.
	client := &http.Client{
		Transport:     &http.Transport{ResponseHeaderTimeout: DialHeaderTimeout},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open the sandbox event stream: %s", redact.Error(err))
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, sandboxStatus(actOpenStream, resp.StatusCode, resp.Status)
	}
	return resp.Body, nil
}

// sandboxGet performs one authenticated read against the sandbox: the
// connect token as a cookie, Basic for OpenCode.
//
// @decision(DL-018) @decision(DL-030)
func sandboxGet(ctx context.Context, url, token, user, password string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.AddCookie(&http.Cookie{Name: proxy.TokenCookie, Value: token})
	req.SetBasicAuth(user, password)
	// Redirects are refused: they are the one way this credential could
	// be carried to a host it was not minted for.
	client := &http.Client{
		Timeout:       SandboxReadTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read the sandbox: %s", redact.Error(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, sandboxStatus(actRead, resp.StatusCode, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, MaxSandboxReadBytes))
}

// readSSEFrames delivers each frame's data to fn until the stream ends,
// fn returns false, or ctx is cancelled. Multi-line data is joined, and
// comments are skipped.
//
// @decision(DL-018) @decision(DL-017)
func readSSEFrames(ctx context.Context, body io.Reader, fn func(raw string) bool) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLogFrameBytes)
	var data []string
	var size int
	flush := func() bool {
		size = 0
		if len(data) == 0 {
			return true
		}
		raw := strings.Join(data, "\n")
		data = nil
		return fn(raw)
	}
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data:"):
			size += len(line)
			if size > MaxLogFrameBytes {
				return fmt.Errorf("an event exceeded %d bytes", MaxLogFrameBytes)
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			if !flush() {
				return nil
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	flush()
	return sc.Err()
}
