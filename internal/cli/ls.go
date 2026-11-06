package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// newLsCmd lists spov sessions. Together with `stop` it is the safety
// net against forgotten, billing sandboxes, so it ships alongside `fly`
// rather than later: a live session is listed on the first page however
// old it is.
//
// @decision(DL-023) @decision(DL-029) @decision(DL-002) @decision(DL-012)
func newLsCmd(env *Env) *cobra.Command {
	var asJSON bool
	var pf pageFlags
	cmd := &cobra.Command{
		Use:     "ls",
		Short:   "List spov sessions, newest first",
		Aliases: []string{"list"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := pf.check(); err != nil {
				return err
			}
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			l, err := pageThrough(cmd.Context(), func(ctx context.Context, p client.Page) ([]server.SessionView, string, int, error) {
				page, err := c.ListSessions(ctx, p)
				return page.Sessions, page.Next, page.Total, err
			}, pf, liveStates())
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(env.Out, sessionsBody{Sessions: orEmpty(l.rows), Next: l.next, Total: l.total})
			}
			if err := writeSessionTable(env, l.rows, env.now()); err != nil {
				return err
			}
			return writeFooter(env, l.footer("spov ls", "still running", pf))
		},
	}
	pf.add(cmd)
	jsonFlag(cmd, &asJSON)
	return cmd
}

// liveStates are the session states a first page always includes.
func liveStates() []string {
	out := make([]string, 0, len(store.LiveStates))
	for _, st := range store.LiveStates {
		out = append(out, string(st))
	}
	return out
}

// writeFooter prints a listing's footer, when it has one.
func writeFooter(env *Env, line string) error {
	if line == "" {
		return nil
	}
	_, err := fmt.Fprintln(env.Out, line)
	return err
}

// liveOnly keeps the sessions that have, or are getting, a sandbox.
func liveOnly(sessions []server.SessionView) []server.SessionView {
	out := sessions[:0:0]
	for _, s := range sessions {
		if s.State.Live() {
			out = append(out, s)
		}
	}
	return out
}

// writeSessionTable renders the session list. Age is shown because a
// forgotten sandbox bills for its whole lifetime.
//
// @decision(DL-029)
func writeSessionTable(env *Env, sessions []server.SessionView, now time.Time) error {
	if len(sessions) == 0 {
		_, err := fmt.Fprintln(env.Out, "No spov sessions.")
		return err
	}

	w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tID\tSTATE\tAGE\tREPOS"); err != nil {
		return err
	}
	for _, s := range sessions {
		name := s.Name
		if name == "" {
			name = "-"
		}
		slugs := make([]string, 0, len(s.Repos))
		for _, r := range s.Repos {
			slugs = append(slugs, r.Owner+"/"+r.Name)
		}
		if len(slugs) == 0 {
			// A sandbox the reconciler adopted has no request, only the
			// clones its tags recorded.
			slugs = s.Clones
		}
		repos := strings.Join(slugs, ",")
		if repos == "" {
			repos = "-"
		}
		// @decision(DL-090)
		state := string(s.State)
		if r := s.Resume; r != nil && r.Mode == server.ResumeSnapshot && r.Until != nil {
			state += " (resumable until " + r.Until.Local().Format("Jan 2") + ")"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			name, s.ID, state, formatAge(now.Sub(s.CreatedAt)), repos); err != nil {
			return err
		}
	}
	return w.Flush()
}

// formatAge renders a duration compactly, since the table is scanned
// rather than read.
func formatAge(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// displayName names a session with its sandbox, when it has one.
func displayName(v server.SessionView) string {
	name := viewName(v)
	if v.SandboxID != "" {
		return name + " (" + v.SandboxID + ")"
	}
	return name
}

// viewName is the session's name, or its ID for one that has none.
func viewName(v server.SessionView) string {
	if v.Name != "" {
		return v.Name
	}
	return v.ID
}
