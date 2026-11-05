package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
)

// newAttachCmd reconnects to a session, waiting first for one that is
// still starting: the other half of `fly --keep`, `fly --no-attach` and
// `jobs create`, which all print `spov attach <name>` as the next step.
//
// @decision(DL-023) @decision(DL-025)
func newAttachCmd(env *Env) *cobra.Command {
	var (
		allowVersionMismatch bool
		noTUI                bool
		web                  bool
		noWait               bool
		timeout              time.Duration
		sync                 bool
	)
	cmd := &cobra.Command{
		Use:   "attach <name|id>",
		Short: "Join a spov session, waiting until it can be joined",
		Args:  cobra.ExactArgs(1),
		// @decision(DL-023)
		ValidArgsFunction: completeSessions(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout < 0 {
				return fmt.Errorf("--timeout must not be negative")
			}
			return runAttach(cmd.Context(), env, attachOptions{
				Ref:                  args[0],
				AllowVersionMismatch: allowVersionMismatch,
				NoTUI:                noTUI,
				Web:                  web,
				NoWait:               noWait,
				Timeout:              timeout,
				Sync:                 sync,
			})
		},
	}
	cmd.Flags().BoolVar(&allowVersionMismatch, "allow-version-mismatch", false,
		"attach even if the local and sandbox OpenCode versions differ")
	cmd.Flags().BoolVar(&noTUI, "no-tui", false, "print how to reach the sandbox without starting the TUI")
	cmd.Flags().BoolVar(&web, "web", false, "open the OpenCode web UI in a browser instead of the terminal TUI")
	cmd.MarkFlagsMutuallyExclusive("web", "no-tui")
	// @decision(DL-025)
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "fail at once if the session cannot be joined yet")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up waiting after this long (default: no limit)")
	cmd.MarkFlagsMutuallyExclusive("no-wait", "timeout")
	// @decision(DL-027)
	cmd.Flags().BoolVar(&sync, "sync", false,
		"sync the sandbox repository's commits and working-tree changes back to the local checkout on exit")
	return cmd
}

type attachOptions struct {
	Ref                  string
	AllowVersionMismatch bool
	NoTUI                bool
	// Web opens the browser instead of the TUI.
	//
	// @decision(DL-025)
	Web bool
	// NoWait fails at once on a session that cannot be joined yet, and
	// Timeout bounds the wait; zero is no limit.
	//
	// @decision(DL-025)
	NoWait  bool
	Timeout time.Duration
	// Sync exports the sandbox repository's state and lands it locally
	// when attach exits (while leaving the sandbox running).
	//
	// @decision(DL-027)
	Sync bool
}

// runAttach resolves the session, waits until it can be joined, connects
// through the server, and hands over the terminal. It never terminates
// the session: `attach` is a way back into a session, and `stop` is the
// way out of one. The CLI recovers no password; the server holds it.
//
// @decision(DL-025) @decision(DL-030)
func runAttach(ctx context.Context, env *Env, opts attachOptions) error {
	res, err := env.config()
	if err != nil {
		return err
	}
	cfg := res.Config

	c, err := env.client(ctx)
	if err != nil {
		return err
	}
	view, err := c.GetSession(ctx, opts.Ref)
	if err != nil {
		return sessionError(opts.Ref, err)
	}
	// @decision(DL-027)
	if opts.Sync {
		if _, err := env.verifySyncCheckout(ctx, cfg, view); err != nil {
			return err
		}
	}
	// Waited on until it can be joined, or refused, before connecting: a
	// stopped session fails much more clearly here than as a 409 from
	// the connect endpoint.
	//
	// @decision(DL-025)
	view, conv, err := awaitJoinable(ctx, env, c, view, opts)
	if err != nil {
		return err
	}
	if conv == "" {
		conv = openingConversation(ctx, env, c, view.ID)
	}

	sess, err := env.connectSession(ctx, c, view)
	if err != nil {
		// The session was running before spov attached and stays
		// running after: say how to stop it rather than leave it billing
		// unnoticed.
		return fmt.Errorf("%w\nthe sandbox is still running; %s", err, stopHint(viewName(view)))
	}
	ref := sess.Name()

	printAttachedSummary(env, sess)

	if opts.NoTUI {
		if opts.Sync {
			if err := env.syncSessionBack(ctx, c, cfg, sess.View); err != nil {
				printReachHintMode(env, ref, opts.Sync)
				detach(env, sess)
				return err
			}
		}
		printReachHintMode(env, ref, opts.Sync)
		detach(env, sess)
		return nil
	}

	// @decision(DL-025)
	if opts.Web {
		expected, source := expectedOpenCodeVersion(ctx, env, cfg)
		if err := checkServerVersion(env, expected, source, sess, opts.AllowVersionMismatch); err != nil {
			printReachHintMode(env, ref, opts.Sync)
			detach(env, sess)
			return err
		}
		if err := runWebSession(ctx, env, sess.URL); err != nil {
			printReachHintMode(env, ref, opts.Sync)
			detach(env, sess)
			return err
		}
		if opts.Sync {
			if err := env.syncSessionBack(ctx, c, cfg, sess.View); err != nil {
				printReachHintMode(env, ref, opts.Sync)
				detach(env, sess)
				return err
			}
		}
		printReachHintMode(env, ref, opts.Sync)
		detach(env, sess)
		return nil
	}

	// A failure here is a local one, so say how to get back in rather
	// than discarding a live session.
	if err := attachTUI(ctx, env, cfg, sess, conv, flyOptions{AllowVersionMismatch: opts.AllowVersionMismatch}); err != nil {
		printReachHintMode(env, ref, opts.Sync)
		detach(env, sess)
		return err
	}

	// @decision(DL-027)
	if opts.Sync {
		if err := env.syncSessionBack(ctx, c, cfg, sess.View); err != nil {
			printReachHintMode(env, ref, opts.Sync)
			detach(env, sess)
			return err
		}
	}

	printReachHintMode(env, ref, opts.Sync)
	detach(env, sess)
	return nil
}

// printReachHintMode says how to reach or stop a sandbox attach is
// leaving running. It differs from the fly hint by omitting the
// `spov attach` line: the user has just run that command, and by not
// calling the sandbox "still" running, which is news only when spov
// might have torn it down.
//
// @decision(DL-029) @decision(DL-025) @decision(DL-027)
func printReachHintMode(env *Env, ref string, synced bool) {
	_, _ = fmt.Fprintf(env.Err, "\nThe sandbox is running.\n")
	// No URL: the sandbox has no public address, and the loopback one
	// belongs to a proxy that is about to stop with this process.
	// Re-running with --web is how to get a browser onto it.
	//
	// @decision(DL-030) @decision(DL-025)
	_, _ = fmt.Fprintf(env.Err, "  web UI:  re-run with --web\n")
	if synced {
		_, _ = fmt.Fprintf(env.Err, "  stop:    spov stop %s --sync\n", ref)
		return
	}
	_, _ = fmt.Fprintf(env.Err, "  stop:    spov stop %s\n", ref)
}

// printAttachedSummary reports what was reconnected to, including age and
// cost: an attach is often the moment a user is deciding whether to keep
// a sandbox alive.
//
// @decision(DL-029) @decision(DL-025)
func printAttachedSummary(env *Env, sess *liveSession) {
	v := sess.View
	_, _ = fmt.Fprintf(env.Err, "\nAttached to %s, running OpenCode %s.\n", displayName(v), sess.Health.Version)
	for _, slug := range v.Clones {
		_, _ = fmt.Fprintf(env.Err, "  cloned %s\n", slug)
	}
	if age := env.now().Sub(v.CreatedAt); age > 0 {
		_, _ = fmt.Fprintf(env.Err, "  running for %s, about %s\n", formatAge(age), hourlyCost(v.Sandbox))
	}
}

// openingConversation is the conversation `attach` should open: the
// session's oldest, which is the one its sandbox started with.
//
// It never fails an attach. A session whose conversations cannot be read
// — or which has none yet, since the recorder attaches on the reconcile
// cycle — returns the empty string, and the client opens on its home
// screen.
//
// @decision(DL-025)
func openingConversation(ctx context.Context, env *Env, c *client.Client, id string) string {
	convs, err := c.Conversations(ctx, id)
	if err != nil {
		env.deferWarning(fmt.Sprintf("spov: could not read the session's conversation: %v", err))
		return ""
	}
	return firstConversation(convs)
}

// firstConversation is the oldest conversation with an ID. The server
// lists conversations in creation order; there is no timestamp on the
// view to sort by.
//
// @decision(DL-025)
func firstConversation(convs []server.ConversationView) string {
	for _, conv := range convs {
		if conv.ID != "" {
			return conv.ID
		}
	}
	return ""
}
