package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// runWebSession opens the web UI and holds the session open until the
// user interrupts spov. The browser goes through the loopback proxy, which
// holds the credentials and dies with this process.
//
// @decision(DL-025) @decision(DL-030)
func runWebSession(ctx context.Context, env *Env, url string) error {
	_, _ = fmt.Fprintf(env.Err, "\nWeb UI: %s\n", url)
	_, _ = fmt.Fprintf(env.Err, "Opening it in your browser. Press Ctrl-C to end the session.\n")

	env.openBrowser(ctx, url)

	if err := env.waitForInterrupt(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(env.Err)
	return nil
}

// openBrowser points a browser at the session URL.
//
// @decision(DL-025)
func (e *Env) openBrowser(ctx context.Context, url string) {
	if e.OpenBrowser != nil {
		e.OpenBrowser(ctx, url)
		return
	}
	openBrowser(url, e)
}

// waitForInterrupt blocks until the user interrupts spov or the context
// is cancelled. A signal is the expected way out and is not an error.
//
// @decision(DL-025)
func (e *Env) waitForInterrupt(ctx context.Context) error {
	if e.WaitForInterrupt != nil {
		return e.WaitForInterrupt(ctx)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	select {
	case <-sig:
		return nil
	case <-ctx.Done():
		// A cancelled context here is the same interrupt arriving through
		// the command's own context, so it ends the session rather than
		// failing it.
		return nil
	}
}
