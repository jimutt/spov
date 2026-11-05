package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/jimutt/spov/internal/server"
)

// consoleWait waits between checks of a sign-in attempt. Tests replace
// it.
var consoleWait = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runOpenCodeLogin signs the caller in to the OpenCode Console through
// the server, which runs the device flow and keeps the grant. The CLI
// shows the code and waits; it never sees a token.
//
// @decision(DL-015)
func runOpenCodeLogin(ctx context.Context, env *Env, noBrowser bool) error {
	c, err := env.client(ctx)
	if err != nil {
		return err
	}
	a, err := c.StartOpenCodeConnection(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(env.Err, "\nTo connect OpenCode (Go or Zen), visit:\n\n    %s\n\nand check that it shows the code:\n\n    %s\n\n",
		a.VerificationURL, a.UserCode)
	if !noBrowser {
		openBrowser(a.VerificationURL, env)
	}
	_, _ = fmt.Fprintln(env.Err, "Waiting for approval...")
	interval := time.Duration(a.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for a.State == server.AttemptPending {
		if err := consoleWait(ctx, interval); err != nil {
			return err
		}
		if a, err = c.CheckOpenCodeConnection(ctx, a.ID); err != nil {
			return err
		}
	}
	switch a.State {
	case server.AttemptConnected:
		_, _ = fmt.Fprintln(env.Out, "Connected OpenCode. Sessions can now use the opencode-go/ and opencode/ models.")
		return nil
	case server.AttemptExpired:
		return fmt.Errorf("the code expired before it was approved; run `spov login opencode` again")
	default:
		return fmt.Errorf("connecting OpenCode failed: %s", a.Reason)
	}
}

// runOpenCodeLogout forgets the caller's OpenCode sign-in on the server.
//
// @decision(DL-015)
func runOpenCodeLogout(ctx context.Context, env *Env) error {
	c, err := env.client(ctx)
	if err != nil {
		return err
	}
	if err := c.DeleteOpenCodeConnection(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(env.Out, "Disconnected OpenCode. The sign-in stays valid at OpenCode until it expires;"+
		" end it in the OpenCode Console to revoke it now.")
	return nil
}
