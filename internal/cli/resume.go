package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// newResumeCmd is `spov resume`: reopen a stopped session in a new
// sandbox and attach to it, as `spov fly` attaches to a new one.
//
// @decision(DL-090) @decision(DL-023) @decision(DL-104)
func newResumeCmd(env *Env) *cobra.Command {
	var opts resumeOptions
	cmd := &cobra.Command{
		Use:   "resume <name|id>",
		Short: "Reopen a stopped session in a new sandbox and attach to it",
		Long: "Reopen a stopped session in a new sandbox: from its exit snapshot when one is kept, with the\n" +
			"workspace as it was, otherwise from its transcript, with the repositories cloned fresh.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeResumable(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Timeout < 0 {
				return fmt.Errorf("--timeout must not be negative")
			}
			opts.Ref = args[0]
			return runResume(cmd.Context(), env, opts)
		},
	}
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "the resumed sandbox's lifetime (default: the one the session was launched with)")
	cmd.Flags().BoolVar(&opts.Keep, "keep", false, "leave the sandbox running when the TUI exits")
	cmd.Flags().BoolVar(&opts.NoAttach, "no-attach", false, "resume and print how to attach, without starting the TUI")
	cmd.Flags().BoolVar(&opts.Yes, "yes", false, "resume from the transcript without asking, where uncommitted work is gone")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "print the resumed session as JSON and do not attach")
	cmd.MarkFlagsMutuallyExclusive("json", "keep")
	cmd.Flags().BoolVar(&opts.Sync, "sync", false,
		"sync the session's repository changes back to the current checkout when the TUI exits; refused for a session that was not synced or outside its checkout")
	cmd.Flags().BoolVar(&opts.NoSync, "no-sync", false,
		"stop the session when the TUI exits without syncing its repository changes back")
	cmd.MarkFlagsMutuallyExclusive("sync", "no-sync")
	return cmd
}

type resumeOptions struct {
	Ref      string
	Timeout  time.Duration
	Keep     bool
	NoAttach bool
	Yes      bool
	JSON     bool
	// Sync and NoSync are --sync and --no-sync: a synced session syncs back
	// on exit by default, inside its checkout.
	//
	// @decision(DL-104)
	Sync   bool
	NoSync bool
}

// runResume says which way the session will come back, asks before a
// transcript resume on a terminal, resumes, and follows the new run as
// `spov fly` follows a launch: exiting the TUI stops the session again
// unless --keep, and Ctrl-C while it boots stops the new run.
//
// @decision(DL-090) @decision(DL-104)
func runResume(ctx context.Context, env *Env, opts resumeOptions) error {
	res, err := env.config()
	if err != nil {
		return err
	}
	c, err := env.client(ctx)
	if err != nil {
		return err
	}
	view, err := c.GetSession(ctx, opts.Ref)
	if err != nil {
		return sessionError(opts.Ref, err)
	}
	// The checkout is verified once, before anything starts: it decides
	// the base commit sent, whether the exit syncs back, and --sync.
	//
	// @decision(DL-104)
	synced := view.Sync != nil && !opts.NoSync
	var checkout git.LocalCheckout
	var cerr error
	if view.Sync != nil {
		checkout, cerr = env.verifySyncCheckout(ctx, res.Config, view)
	}
	if opts.Sync && view.Resume != nil {
		switch {
		case view.Sync == nil:
			return fmt.Errorf("session %s was not started from a checkout, so --sync has nothing to bring back", viewName(view))
		case cerr != nil:
			return fmt.Errorf("--sync needs the session's checkout: %w", cerr)
		}
	}
	if err := env.agreeToResume(view, opts.Yes); err != nil {
		return err
	}
	if synced && cerr != nil && !opts.Keep && !opts.NoAttach && !opts.JSON {
		_, _ = fmt.Fprintf(env.Err, "note: %v\n  leaving the TUI will keep the session running; `spov stop %s --sync` from the checkout brings its work back\n",
			cerr, viewName(view))
	}
	req := server.ResumeRequest{TimeoutSecs: int(opts.Timeout / time.Second)}
	// @decision(DL-098)
	req.SyncBundle = env.resumeSeed(ctx, view, checkout, cerr)
	name := viewName(view)
	view, err = c.Resume(ctx, view.ID, req)
	if err != nil {
		return resumeError(name, err)
	}
	if opts.JSON {
		return writeJSON(env.Out, view)
	}
	view, _, err = awaitJoinable(ctx, env, c, view, attachOptions{Ref: view.ID, StopsOnCancel: !opts.Keep})
	if err != nil {
		// The new run is what is billing; Ctrl-C while it boots ends it.
		if ctx.Err() != nil && !opts.Keep {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = c.DeleteSession(stopCtx, view.ID)
		}
		return err
	}
	sess, err := env.connectSession(ctx, c, view)
	if err != nil {
		printAttachHintMode(env, viewName(view), synced)
		return err
	}
	if opts.NoAttach {
		printSessionSummary(env, sess)
		printAttachHintMode(env, sess.Name(), synced)
		detach(env, sess)
		return nil
	}
	cfg := res.Config
	binary, err := prepareTUI(ctx, env, cfg, sess, flyOptions{})
	if err != nil {
		printSessionSummary(env, sess)
		printAttachHintMode(env, sess.Name(), synced)
		detach(env, sess)
		return err
	}
	if err := handOver(ctx, env, sess, binary, ""); err != nil {
		printSessionSummary(env, sess)
		printAttachHintMode(env, sess.Name(), synced)
		detach(env, sess)
		return err
	}
	printSessionSummary(env, sess)
	if opts.Keep {
		printAttachHintMode(env, sess.Name(), synced)
		detach(env, sess)
		return nil
	}
	// @decision(DL-104)
	if synced {
		if cerr != nil {
			// No checkout to apply the work to: it stays in the sandbox.
			printAttachHintMode(env, sess.Name(), true)
			_, _ = fmt.Fprintf(env.Err, "  discard: spov stop %s\n", sess.Name())
			detach(env, sess)
			return nil
		}
		if err := env.syncSessionBack(ctx, c, cfg, sess.View); err != nil {
			printSyncFailureHint(env, sess.Name())
			detach(env, sess)
			return err
		}
	}
	return terminateSession(ctx, env, c, sess)
}

// resumeWords is what a resume brings back, by mode, worded as the web
// app words it.
//
// @decision(DL-090)
func resumeWords(r *server.ResumeView) string {
	if r.Mode == server.ResumeSnapshot {
		words := "from its snapshot: the workspace comes back as it was when it stopped, uncommitted changes included"
		if r.Pending {
			words += " (the snapshot is still being taken, so the resume waits for it)"
		}
		return words
	}
	return "from its transcript: the repositories are cloned fresh and the conversation is restored; uncommitted changes and installed packages are gone"
}

// agreeToResume says how view would resume, and for a transcript resume
// asks on a terminal unless yes; off a terminal it needs yes.
//
// @decision(DL-090)
func (e *Env) agreeToResume(view server.SessionView, yes bool) error {
	name := viewName(view)
	if view.Resume == nil {
		return notResumable(view)
	}
	_, _ = fmt.Fprintf(e.Err, "resuming %s %s\n", name, resumeWords(view.Resume))
	if view.Resume.Mode != server.ResumeTranscript || yes {
		return nil
	}
	ok, err := e.confirm("resume it from the transcript? [y/N] ")
	if err != nil {
		return fmt.Errorf("%w; pass --yes to resume from the transcript", err)
	}
	if !ok {
		return fmt.Errorf("not resumed")
	}
	return nil
}

// offerResume asks, on a terminal, whether to resume the stopped session
// attach was asked to join, and resumes it on yes; attach then waits and
// joins as usual. Off a terminal it names `spov resume`.
//
// @decision(DL-090) @decision(DL-025)
func (e *Env) offerResume(ctx context.Context, c *client.Client, view server.SessionView) (server.SessionView, error) {
	name := viewName(view)
	_, _ = fmt.Fprintf(e.Err, "session %s has stopped; it can be resumed %s\n", name, resumeWords(view.Resume))
	ok, err := e.confirm("resume it? [y/N] ")
	if err != nil {
		return view, fmt.Errorf("session %s is %s; `spov resume %s` reopens it", name, view.State, name)
	}
	if !ok {
		return view, fmt.Errorf("not resumed; `spov resume %s` reopens it later", name)
	}
	req := server.ResumeRequest{}
	// @decision(DL-098) @decision(DL-104)
	if res, err := e.config(); err == nil && view.Sync != nil {
		checkout, cerr := e.verifySyncCheckout(ctx, res.Config, view)
		req.SyncBundle = e.resumeSeed(ctx, view, checkout, cerr)
	}
	resumed, err := c.Resume(ctx, view.ID, req)
	if err != nil {
		return view, resumeError(name, err)
	}
	return resumed, nil
}

// resumeSeed is a synced session's base commit, bundled from the checkout
// here, for a resume from its transcript; empty otherwise. When it cannot
// be sent it says why, since the resumed workspace may then lack the
// session's unpushed commits. The checkout and its verification error are
// the caller's one `verifySyncCheckout`, so the seed and the exit's sync
// cannot disagree.
//
// @decision(DL-098) @decision(DL-104)
func (e *Env) resumeSeed(ctx context.Context, view server.SessionView, checkout git.LocalCheckout, err error) string {
	if view.Sync == nil || view.Resume == nil || view.Resume.Mode != server.ResumeTranscript {
		return ""
	}
	if err == nil {
		// A session with no platform has no clone to build on, so its
		// bundle holds the whole history, as its launch's did.
		remote := checkout.Remote
		if len(view.Repos) == 1 && view.Repos[0].Platform == "" {
			remote = ""
		}
		var bundle []byte
		if bundle, err = git.BundleCommit(ctx, checkout.Root, remote, view.Sync.BaseCommit); err == nil {
			return base64.StdEncoding.EncodeToString(bundle)
		}
	}
	_, _ = fmt.Fprintf(e.Err, "note: the session's starting commit is not sent (%v); unless it was pushed, the resumed workspace"+
		" starts on the default branch without it\n", err)
	return ""
}

// notResumable explains a session that offers no resume.
func notResumable(view server.SessionView) error {
	name := viewName(view)
	switch {
	case view.State == store.StateRunning || view.State == store.StateStarting:
		return fmt.Errorf("session %s is %s; `spov attach %s` joins it", name, view.State, name)
	case view.Job != nil:
		return fmt.Errorf("session %s is job %s's, and a job is not resumed; `spov jobs show %s` shows how it ended", name, view.Job.ID, view.Job.ID)
	}
	return fmt.Errorf("session %s cannot be resumed: it never ran, it is not yours, or this server keeps no sessions; `spov fly` starts a new one", name)
}

// resumeError keeps the server's reason for a refused resume.
func resumeError(name string, err error) error {
	return fmt.Errorf("resume %s: %w", name, err)
}

// confirm asks a yes-or-no question on the terminal, defaulting to no.
//
// @decision(DL-090)
func (e *Env) confirm(prompt string) (bool, error) {
	if e.Confirm != nil {
		return e.Confirm(prompt)
	}
	if e.Stdin == nil || !term.IsTerminal(int(e.Stdin.Fd())) {
		return false, fmt.Errorf("no terminal to ask on")
	}
	_, _ = fmt.Fprint(e.Err, prompt)
	line, err := bufio.NewReader(e.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// completeResumable offers the caller's newest resumable sessions from a
// server that is already up.
//
// @decision(DL-090) @decision(DL-023)
func completeResumable(env *Env) cobra.CompletionFunc {
	return completeWithin(env, func(ctx context.Context, prefix string) []cobra.Completion {
		c, ok := env.existingClient(ctx)
		if !ok {
			return nil
		}
		page, err := c.ListSessions(ctx, client.Page{States: []string{string(store.StateStopped), string(store.StateFailed)}, Limit: server.MaxPageSize})
		if err != nil {
			return nil
		}
		var out []cobra.Completion
		for _, s := range page.Sessions {
			if s.Resume == nil || !strings.HasPrefix(s.Name, prefix) {
				continue
			}
			out = append(out, cobra.CompletionWithDesc(s.Name, s.Resume.Mode))
		}
		return out
	})
}
