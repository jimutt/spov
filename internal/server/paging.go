package server

import (
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/store"
)

// The page sizes of the session and job listings: the size a request
// that names none gets, and the most one may ask for. A larger request
// is clamped rather than refused.
//
// @decision(DL-012)
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// pageSize is the size a request asked for, defaulted and clamped.
//
// @decision(DL-012)
func pageSize(asked int) int {
	if asked <= 0 {
		return DefaultPageSize
	}
	return min(asked, MaxPageSize)
}

// pageOf trims a read of size+1 rows to size, and returns the cursor for
// the page after it: the last ID kept, or "" when the extra row shows
// nothing older matches.
//
// @decision(DL-012)
func pageOf[T any](rows []T, size int, id func(T) string) ([]T, string) {
	if len(rows) <= size {
		return rows, ""
	}
	rows = rows[:size]
	return rows, id(rows[size-1])
}

// sessionFilterOf turns the listing's state parameter into a filter,
// refusing a state that does not exist. One comma-separated value, as
// the job listing takes (DL-057).
//
// @decision(DL-012)
func sessionFilterOf(states []string) (store.SessionFilter, error) {
	f := store.SessionFilter{}
	seen := map[store.SessionState]bool{}
	for _, group := range states {
		for _, raw := range strings.Split(group, ",") {
			st := store.SessionState(strings.TrimSpace(raw))
			switch st {
			case "":
				continue
			case store.StateStarting, store.StateRunning, store.StateStopped, store.StateFailed:
			default:
				return f, huma.Error422UnprocessableEntity("no such session state: " + string(st))
			}
			if !seen[st] {
				seen[st] = true
				f.States = append(f.States, st)
			}
		}
	}
	return f, nil
}
