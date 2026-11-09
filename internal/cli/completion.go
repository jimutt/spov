package cli

import (
	"cmp"
	"context"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/version"
)

// Shell completion of the names commands take: sessions, jobs and
// credentials. A completion asks only a server that is already up, and
// any failure or timeout offers nothing.
//
// @decision(DL-023)

// completionTimeout bounds a completion's whole lookup, a keyring read
// included.
const completionTimeout = time.Second

// existingClient is the server a completion may ask: the configured
// hosted one, or the local server the state file names, if it answers
// and is this build. It never spawns a server, signs in, or stops a
// server of another build, as Env.client may.
//
// @decision(DL-023) @decision(DL-002)
func (e *Env) existingClient(ctx context.Context) (*client.Client, bool) {
	if e.Client != nil {
		return e.Client, true
	}
	url, err := e.configuredServerURL()
	if err != nil {
		return nil, false
	}
	if url != "" {
		if config.ValidServerURL(url) != nil {
			return nil, false
		}
		c := e.hostedClient(url)
		c.UserAgent = "spov/" + version.Get().Version
		return c, true
	}
	path, err := e.stateFilePath()
	if err != nil {
		return nil, false
	}
	st, err := readState(path)
	if err != nil || st == nil || st.Version != version.Get().String() {
		return nil, false
	}
	c, ok := e.liveLocal(ctx, st)
	if ok {
		c.UserAgent = "spov/" + version.Get().Version
	}
	return c, ok
}

// completeWithin runs list for a command's first argument, bounded by
// the completion timeout. The lookup runs aside so that a call which
// ignores the context, such as a keyring read, cannot hold the shell.
//
// @decision(DL-023)
func completeWithin(env *Env, list func(ctx context.Context, toComplete string) []cobra.Completion) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		const none = cobra.ShellCompDirectiveNoFileComp
		if len(args) > 0 {
			return nil, none
		}
		parent := cmd.Context()
		if parent == nil {
			parent = context.Background()
		}
		timeout := env.CompletionTimeout
		if timeout <= 0 {
			timeout = completionTimeout
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		got := make(chan []cobra.Completion, 1)
		go func() { got <- list(ctx, toComplete) }()
		select {
		case out := <-got:
			return out, none | cobra.ShellCompDirectiveKeepOrder
		case <-ctx.Done():
			return nil, none
		}
	}
}

// completeSessions offers the live sessions by name, and by ID once
// what is typed starts one. Nothing once --all is given, which takes no
// session.
//
// @decision(DL-023)
func completeSessions(env *Env) cobra.CompletionFunc {
	list := completeWithin(env, func(ctx context.Context, prefix string) []cobra.Completion {
		c, ok := env.existingClient(ctx)
		if !ok {
			return nil
		}
		// Live sessions only, which a page of the live states holds
		// whole. @decision(DL-012)
		page, err := c.ListSessions(ctx, client.Page{States: liveStates(), Limit: server.MaxPageSize})
		if err != nil {
			return nil
		}
		now := env.now()
		var out []cobra.Completion
		for _, s := range liveOnly(page.Sessions) {
			about := string(s.State) + ", " + formatAge(now.Sub(s.CreatedAt))
			if s.Name != "" && strings.HasPrefix(s.Name, prefix) {
				out = append(out, cobra.CompletionWithDesc(s.Name, about))
			}
			if (s.Name == "" || prefix != "") && strings.HasPrefix(s.ID, prefix) {
				if s.Name != "" {
					about = s.Name + ", " + about
				}
				out = append(out, cobra.CompletionWithDesc(s.ID, about))
			}
		}
		return out
	})
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if all := cmd.Flags().Lookup("all"); all != nil && all.Changed {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return list(cmd, args, toComplete)
	}
}

// completeJobs offers job IDs, unfinished first and newest first within
// each; unfinishedOnly leaves the finished out, for the acts that a
// finished job refuses.
//
// @decision(DL-023)
func completeJobs(env *Env, unfinishedOnly bool) cobra.CompletionFunc {
	return completeWithin(env, func(ctx context.Context, prefix string) []cobra.Completion {
		c, ok := env.existingClient(ctx)
		if !ok {
			return nil
		}
		var states []string
		if unfinishedOnly {
			for _, st := range store.JobCounts {
				states = append(states, string(st))
			}
		}
		// The newest page: an older job is not offered. @decision(DL-012)
		page, err := c.ListJobs(ctx, client.Page{States: states})
		if err != nil {
			return nil
		}
		jobs := page.Jobs
		slices.SortStableFunc(jobs, func(a, b server.JobView) int {
			if a.State.Terminal() != b.State.Terminal() {
				if a.State.Terminal() {
					return 1
				}
				return -1
			}
			return b.CreatedAt.Compare(a.CreatedAt)
		})
		var out []cobra.Completion
		for _, j := range jobs {
			if (unfinishedOnly && j.State.Terminal()) || !strings.HasPrefix(j.ID, prefix) {
				continue
			}
			out = append(out, cobra.CompletionWithDesc(j.ID, string(j.State)+": "+firstLine(j.Goal, 40)))
		}
		return out
	})
}

// completeSyncJobs offers the sync jobs `jobs apply` and `jobs release`
// take: held ones, and for apply also unfinished ones, from the newest
// page of sync jobs.
//
// @decision(DL-102) @decision(DL-023)
func completeSyncJobs(env *Env, unfinishedToo bool) cobra.CompletionFunc {
	return completeWithin(env, func(ctx context.Context, prefix string) []cobra.Completion {
		c, ok := env.existingClient(ctx)
		if !ok {
			return nil
		}
		page, err := c.ListJobs(ctx, client.Page{})
		if err != nil {
			return nil
		}
		var out []cobra.Completion
		for _, j := range page.Jobs {
			if j.Kind != store.JobKindSync || !strings.HasPrefix(j.ID, prefix) {
				continue
			}
			held := j.Sync != nil && j.Sync.Held
			if held || (unfinishedToo && !j.State.Terminal()) {
				word := string(j.State)
				if held {
					word += ", held"
				}
				out = append(out, cobra.CompletionWithDesc(j.ID, word+": "+firstLine(j.Goal, 40)))
			}
		}
		return out
	})
}

// completeCredentials offers the names `credentials remove` takes, from
// where `spov credentials` lists them: this machine's store with a
// local server, the server's listing with a hosted one.
//
// @decision(DL-023) @decision(DL-008)
func completeCredentials(env *Env) cobra.CompletionFunc {
	return completeWithin(env, func(ctx context.Context, prefix string) []cobra.Completion {
		hosted, err := env.hostedServer()
		if err != nil {
			return nil
		}
		var views []server.CredentialView
		if hosted {
			c, ok := env.existingClient(ctx)
			if !ok {
				return nil
			}
			if views, _, err = c.ListCredentials(ctx); err != nil {
				return nil
			}
		} else if _, views, err = heldLocally(env, io.Discard); err != nil {
			return nil
		}
		var out []cobra.Completion
		for _, v := range views {
			if name := displayCredential(v.Platform); strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
				out = append(out, name)
			}
		}
		slices.SortFunc(out, cmp.Compare[cobra.Completion])
		return out
	})
}
