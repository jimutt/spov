package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/sandbox"
)

// AttachOptions describes the sandbox to reconnect to.
type AttachOptions struct {
	// Ref is the sandbox name or ID the user gave.
	Ref string
	// Port is the port the OpenCode server listens on.
	Port int
	// Password, when set, is used as-is. Empty means recover it.
	Password string
}

// ErrStopped means the sandbox exists but is no longer running.
var ErrStopped = errors.New("session: the sandbox has stopped")

// ErrNoPassword means the session password could not be recovered.
var ErrNoPassword = errors.New("session: the session password could not be recovered")

// Attacher reconnects to a sandbox that is already running. It is the
// read-only counterpart to Launcher: it never creates or terminates a
// sandbox.
//
// @decision(DL-025)
type Attacher struct {
	Sandboxes sandbox.Provider
	// NewClient builds a client for the server; defaults to the real one.
	NewClient func(url, password string) HealthChecker
	// RecoverPassword supplies the password when the caller has none.
	// Nil means the password must be supplied by the caller.
	RecoverPassword func(ctx context.Context, h sandbox.Handle, info sandbox.Info) (string, error)
	// HealthTimeout bounds the wait for the server to answer.
	HealthTimeout time.Duration
	// Progress receives human-readable progress lines. Nil discards them.
	Progress io.Writer
	// StartProxy brings up the loopback proxy fronting the sandbox. Nil
	// means the real one.
	//
	// @decision(DL-030)
	StartProxy func(ctx context.Context, h sandbox.Handle, port int, password string, warn func(string)) (*proxy.Proxy, error)
}

// Attach resolves a running sandbox and returns a Session for it.
//
// The returned Session deliberately carries no token revoker: the tokens
// belong to the launching process, and attaching must not revoke
// credentials the session is still using.
//
// @decision(DL-025)
func (a *Attacher) Attach(ctx context.Context, opts AttachOptions) (*Session, error) {
	if opts.Ref == "" {
		return nil, fmt.Errorf("session: attach needs a sandbox name or id")
	}

	handle, err := a.Sandboxes.Get(ctx, opts.Ref)
	if err != nil {
		return nil, err
	}

	info, err := handle.Info(ctx)
	if err != nil {
		return nil, err
	}
	// Checked before any recovery or health work: attaching to a stopped
	// sandbox can only ever fail, and it fails much more clearly here
	// than as a health timeout minutes later.
	if info.State != sandbox.StateRunning {
		return nil, fmt.Errorf("%w: %s is %s", ErrStopped, displayRef(opts.Ref, info), info.State)
	}

	password := opts.Password
	if password == "" {
		if a.RecoverPassword == nil {
			return nil, ErrNoPassword
		}
		password, err = a.RecoverPassword(ctx, handle, info)
		if err != nil {
			return nil, err
		}
		if password == "" {
			return nil, ErrNoPassword
		}
	}

	// An attaching process mints its own connect token: it holds the
	// Modal credential, and the launching process's token is neither
	// shared nor recoverable.
	//
	// @decision(DL-030)
	prox, err := a.startProxy(ctx, handle, opts.Port, password)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = prox.Close()
		}
	}()

	a.progress("waiting for the OpenCode server...")
	healthCtx, cancel := context.WithTimeout(ctx, a.healthTimeout())
	defer cancel()

	// @decision(DL-030)
	health, err := a.newClient(prox.URL, "").WaitHealthy(healthCtx, 250*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}

	// Landing is deliberately left empty: the directory the server
	// started in was chosen by the launching process and is recorded
	// nowhere an attaching one can read. Callers that print it must
	// tolerate its absence on an attached session.
	return &Session{
		Handle:   handle,
		Info:     info,
		URL:      prox.URL,
		Password: password,
		Health:   health,
		Clones:   clonesFromTags(info.Tags),
		proxy:    prox,
	}, nil
}

// PasswordReadTimeout bounds reading the password back out of a sandbox.
//
// @decision(DL-135)
const PasswordReadTimeout = 30 * time.Second

// maxPasswordOutput is far above any password spov generates.
//
// @decision(DL-130)
const maxPasswordOutput = 4 << 10

// PasswordFromSandbox reads OPENCODE_PASSWORD back out of the running
// container. A process started by Exec inherits the sandbox's secret
// environment, so this recovers the password for a session launched on
// another machine or whose local record was lost.
//
// @decision(DL-025) @decision(DL-036)
func PasswordFromSandbox(ctx context.Context, h sandbox.Handle) (string, error) {
	// @decision(DL-135)
	ctx, cancel := context.WithTimeout(ctx, PasswordReadTimeout)
	defer cancel()
	res, err := h.ExecBounded(ctx, []string{"printenv", opencode.PasswordEnvVar}, nil, maxPasswordOutput)
	if err != nil {
		return "", fmt.Errorf("session: reading the password from the sandbox: %w", err)
	}
	// printenv exits non-zero when the variable is unset, which is a
	// meaningfully different failure from a broken exec: the sandbox is
	// running something other than a spov entrypoint.
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%w: the sandbox has no %s set", ErrNoPassword, opencode.PasswordEnvVar)
	}
	// printenv adds one trailing newline, so only that is stripped.
	// This assumes the password contains no newline of its own, which
	// holds for GeneratePassword's base64url alphabet: a generator that
	// could emit one would need a different transport than exec stdout.
	password := strings.TrimRight(res.Stdout, "\r\n")
	if password == "" {
		return "", fmt.Errorf("%w: the sandbox reported an empty %s", ErrNoPassword, opencode.PasswordEnvVar)
	}
	return password, nil
}

// clonesFromTags recovers the bootstrapped repo list from the sandbox
// tags. The launching process recorded it there, and it is the only
// record available to a different process.
//
// @decision(DL-025)
func clonesFromTags(tags map[string]string) []string {
	return sandbox.ReposFromTags(tags)
}

// displayRef names the sandbox the way the user is most likely to
// recognise it.
func displayRef(ref string, info sandbox.Info) string {
	if info.Name != "" && info.Name != ref {
		return fmt.Sprintf("%s (%s)", info.Name, info.ID)
	}
	return ref
}

// startProxy brings up the proxy for an attached sandbox.
//
// @decision(DL-030)
func (a *Attacher) startProxy(ctx context.Context, h sandbox.Handle, port int, password string) (*proxy.Proxy, error) {
	start := a.StartProxy
	if start == nil {
		start = StartProxy
	}
	return start(ctx, h, port, password, a.progress)
}

func (a *Attacher) newClient(url, password string) HealthChecker {
	if a.NewClient != nil {
		return a.NewClient(url, password)
	}
	return opencode.NewClient(url, password)
}

func (a *Attacher) healthTimeout() time.Duration {
	if a.HealthTimeout > 0 {
		return a.HealthTimeout
	}
	return defaultHealthTimeout
}

func (a *Attacher) progress(msg string) {
	if a.Progress == nil {
		return
	}
	_, _ = fmt.Fprintln(a.Progress, msg)
}
