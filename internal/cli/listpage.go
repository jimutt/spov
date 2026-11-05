package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
)

// DefaultListLimit is how many rows `spov ls` and `spov jobs` show on a
// page when -n is not given.
//
// @decision(DL-012)
const DefaultListLimit = 5

// pageFlags are the paging flags `spov ls` and `spov jobs` share.
//
// @decision(DL-012)
type pageFlags struct {
	page, limit int
}

func (f *pageFlags) add(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.page, "page", 1, "which page to show, newest first")
	cmd.Flags().IntVarP(&f.limit, "limit", "n", DefaultListLimit, "how many to show on a page")
}

func (f pageFlags) check() error {
	if f.page < 1 {
		return fmt.Errorf("--page must be 1 or more")
	}
	if f.limit < 1 || f.limit > server.MaxPageSize {
		return fmt.Errorf("--limit must be between 1 and %d", server.MaxPageSize)
	}
	return nil
}

// listing is what a CLI list shows: one page of rows, newest first,
// then on the first page the live rows older than it.
//
// @decision(DL-012)
type listing[T any] struct {
	rows []T
	// extra counts the live rows appended after the page.
	extra int
	// total is how many rows the listing has in all; next is the
	// server's cursor after the page, empty on the last.
	total int
	next  string
}

// fetchPage reads one page of a listing: its rows, the cursor after it
// and the total.
type fetchPage[T any] func(context.Context, client.Page) ([]T, string, int, error)

// pageThrough reads page n of size rows by following the server's
// cursor, at most server.MaxPageSize rows a request, and on page 1 adds
// every row in live states older than the page.
//
// @decision(DL-012)
func pageThrough[T any](ctx context.Context, fetch fetchPage[T], f pageFlags, live []string) (listing[T], error) {
	var out listing[T]
	before := ""
	for skip := (f.page - 1) * f.limit; skip > 0; {
		step := min(skip, server.MaxPageSize)
		_, next, total, err := fetch(ctx, client.Page{Limit: step, Before: before})
		if err != nil {
			return out, err
		}
		out.total = total
		if next == "" {
			// The page asked for is past the end.
			return out, nil
		}
		before, skip = next, skip-step
	}
	rows, next, total, err := fetch(ctx, client.Page{Limit: f.limit, Before: before})
	if err != nil {
		return out, err
	}
	out.rows, out.next, out.total = rows, next, total
	if f.page != 1 || next == "" {
		return out, nil
	}
	for cursor := next; cursor != ""; {
		older, after, _, err := fetch(ctx, client.Page{States: live, Limit: server.MaxPageSize, Before: cursor})
		if err != nil {
			return out, err
		}
		out.rows = append(out.rows, older...)
		out.extra += len(older)
		cursor = after
	}
	return out, nil
}

// footer says where a page sits in the listing and how to go on, or ""
// when the whole listing fit on the page. command is the command as
// typed, such as `spov ls`; liveWord names the appended rows.
//
// @decision(DL-012)
func (l listing[T]) footer(command, liveWord string, f pageFlags) string {
	shown := len(l.rows) - l.extra
	if f.page == 1 && l.next == "" {
		return ""
	}
	if shown == 0 {
		return fmt.Sprintf("Page %d is past the end: %d in all.", f.page, l.total)
	}
	first := (f.page-1)*f.limit + 1
	line := fmt.Sprintf("%d–%d of %d", first, first+shown-1, l.total)
	if l.extra > 0 {
		line += fmt.Sprintf(", plus %d %s older than these", l.extra, liveWord)
	}
	if l.next != "" {
		next := command + " --page " + strconv.Itoa(f.page+1)
		if f.limit != DefaultListLimit {
			next += " -n " + strconv.Itoa(f.limit)
		}
		line += " · next: " + next
	}
	return line
}
