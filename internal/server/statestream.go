package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/store"
)

const (
	// StateFeedInterval is how often the session feed re-reads the
	// store.
	//
	// @decision(DL-018)
	StateFeedInterval = 2 * time.Second
	// StreamLifetime bounds one SSE connection. The cookie is checked
	// when a stream opens and never again, so an immortal stream would
	// outlive the login that authorised it.
	//
	// @decision(DL-018) @decision(DL-007)
	StreamLifetime = 5 * time.Minute
	// HeartbeatInterval keeps an idle stream from looking dead to a
	// proxy. huma sends nothing of its own.
	//
	// @decision(DL-018)
	HeartbeatInterval = 20 * time.Second
	// LogPageSize bounds one read of a session's stored log. A stream
	// that opens from the beginning of a long session drains in pages
	// as fast as the client takes them, rather than holding the whole
	// log in memory to send it.
	//
	// @decision(DL-018)
	LogPageSize = 200
)

// SessionsEvent is the caller's sessions as of At: their own, or the
// org's for an admin. It is whole every time rather than a delta, so a
// client that missed one has nothing to reconcile.
//
// @decision(DL-018)
type SessionsEvent struct {
	Sessions []SessionView `json:"sessions"`
	At       time.Time     `json:"at"`
}

// EndEvent closes a stream in an orderly way. It is not an error and
// says nothing to the user: on "lifetime" the client reopens, and on
// "stopped" the session has nothing more to say and it does not.
//
// @decision(DL-018)
type EndEvent struct {
	Reason string    `json:"reason" enum:"lifetime,stopped" doc:"Why the server closed the stream"`
	At     time.Time `json:"at"`
}

// sessionFeed delivers a caller's sessions whenever they may have
// changed, and is the seam a LISTEN/NOTIFY implementation replaces
// without the handler knowing.
//
// @decision(DL-018)
type sessionFeed interface {
	// Subscribe sends the caller's sessions as StreamList pages them:
	// the newest limit, then every live session older than those.
	//
	// @decision(DL-012)
	Subscribe(ctx context.Context, p auth.Principal, limit int) (<-chan []store.Session, error)
}

// pollFeed re-reads the caller's sessions on a fixed interval and sends
// only what changed.
//
// @decision(DL-018)
type pollFeed struct {
	list     func(context.Context, auth.Principal, int) ([]store.Session, error)
	interval time.Duration
	// stamp adds to the fingerprint whatever the list renders that does
	// not live on the session row. Nil contributes nothing.
	//
	// @decision(DL-018)
	stamp func(context.Context, auth.Principal, []store.Session) string
	// warn reports a read that failed; nil discards.
	warn func(string)
}

// mark is the fingerprint's second half.
func (f pollFeed) mark(ctx context.Context, p auth.Principal, sessions []store.Session) string {
	if f.stamp == nil {
		return ""
	}
	return f.stamp(ctx, p, sessions)
}

// Subscribe reads once before returning, so a caller whose very first
// read fails learns that instead of opening a stream that looks like an
// empty session list.
func (f pollFeed) Subscribe(ctx context.Context, p auth.Principal, limit int) (<-chan []store.Session, error) {
	first, err := f.list(ctx, p, limit)
	if err != nil {
		return nil, err
	}
	interval := f.interval
	if interval <= 0 {
		interval = StateFeedInterval
	}
	// Buffered by one and written only by this goroutine: the send below
	// races the consumer's own teardown, and an unbuffered channel would
	// block a feed nobody is reading until its context is cancelled.
	out := make(chan []store.Session, 1)
	go func() {
		defer close(out)
		out <- first
		prev := fingerprint(first) + f.mark(ctx, p, first)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cur, err := f.list(ctx, p, limit)
			if err != nil {
				// Transient by assumption: the next tick retries, and a
				// stream that survives a blip is worth more than one
				// that ends on it. A permanent failure ends the stream
				// at its lifetime bound instead.
				if ctx.Err() == nil && f.warn != nil {
					f.warn(fmt.Sprintf("session feed: %v", err))
				}
				continue
			}
			fp := fingerprint(cur) + f.mark(ctx, p, cur)
			if fp == prev {
				continue
			}
			prev = fp
			select {
			case out <- cur:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// fingerprint is what "changed" means for the feed, for everything on
// the session row. What the list renders from elsewhere is
// pollFeed.stamp.
//
// @decision(DL-018)
func fingerprint(sessions []store.Session) string {
	var b strings.Builder
	for _, s := range sessions {
		fmt.Fprintf(&b, "%s:%s:%d:%s:%s\n",
			s.ID, s.State, s.UpdatedAt.UnixNano(), s.SandboxID, s.Error)
	}
	return b.String()
}

// sessionStreamInput sizes the stream's page.
//
// @decision(DL-012)
type sessionStreamInput struct {
	Limit int `query:"limit" minimum:"0" doc:"How many of the newest sessions to send, besides every live one: 50 when unset, at most 200"`
}

// streamSessions keeps the caller's session list honest. Its contents
// are Service.StreamList's output for this principal, so authorisation
// is inherited rather than reimplemented here.
//
// @decision(DL-018) @decision(DL-012)
func (s *Server) streamSessions(ctx context.Context, in *sessionStreamInput, send sseSender) {
	// The feed goroutine outlives this function otherwise.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	asked := 0
	if in != nil {
		asked = in.Limit
	}
	updates, err := s.feed.Subscribe(ctx, principal(ctx), pageSize(asked))
	if err != nil {
		_ = send.Data(ErrorEvent{Problem: problemOf(err)})
		return
	}

	// A timer rather than a context deadline, so the lifetime bound can
	// be reported as an orderly end while a client that simply went away
	// is not reported at all.
	life := time.NewTimer(s.streamLifetime)
	defer life.Stop()
	beat := time.NewTicker(s.heartbeat)
	defer beat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			_ = send.Data(EndEvent{Reason: "lifetime", At: s.deps.Now()})
			return
		case <-beat.C:
			if err := send.Comment("keepalive"); err != nil {
				return
			}
		case sessions, ok := <-updates:
			if !ok {
				return
			}
			// One clock for the whole event: costs are measured against
			// elapsed time, so reading it per session would date the
			// entries in a single event a few microseconds apart.
			now := s.deps.Now()
			ev := SessionsEvent{At: now, Sessions: s.viewsOf(ctx, principal(ctx), sessions, now)}
			if err := send.Data(ev); err != nil {
				return
			}
		}
	}
}

// sseSender is the part of huma's sse.Sender the stream handlers use.
// An interface, so a test can watch a stream without an HTTP server —
// huma's own Sender satisfies it.
//
// @decision(DL-018)
type sseSender interface {
	Data(any) error
	Comment(string) error
}

// problemOf renders an error as the problem an event carries, routing
// through toHumaError so a failure inside a stream is classified
// exactly as the same failure on a plain request.
//
// @decision(DL-018)
func problemOf(err error) ProblemView {
	status := http.StatusInternalServerError
	var se huma.StatusError
	mapped := toHumaError(err)
	if errors.As(mapped, &se) {
		status = se.GetStatus()
	}
	// An unexpected store error is answered with the fixed message the
	// API gives, never its text.
	//
	// @decision(DL-135)
	if status == http.StatusInternalServerError && unexpectedStoreError(err) {
		return ProblemView{Title: http.StatusText(status), Status: status, Detail: mapped.Error()}
	}
	return ProblemView{Title: http.StatusText(status), Status: status, Detail: redact.Error(err)}
}

// eventBatch is one read of a session's stored log: the rows after the
// cursor, and whether that read reached the end of what is stored.
//
// @decision(DL-018)
type eventBatch struct {
	Events   []store.Event
	CaughtUp bool
}

// eventFeed delivers a session's stored events from a position onwards,
// and is the seam a LISTEN/NOTIFY implementation replaces without the
// handler knowing.
//
// @decision(DL-018)
type eventFeed interface {
	Subscribe(ctx context.Context, orgID, sessionID string, after int64) (<-chan eventBatch, error)
}

// pollEvents drains the stored log in pages and then re-reads it on a
// fixed interval. The store is the source, so a slow reader falls
// behind rather than losing events: the cursor is where it got to.
//
// @decision(DL-018)
type pollEvents struct {
	list     func(ctx context.Context, orgID, sessionID string, after int64, limit int) ([]store.Event, error)
	interval time.Duration
	// page is LogPageSize unless a test shortens it.
	page int
	// warn reports a read that failed; nil discards.
	warn func(string)
}

// Subscribe reads once before returning, so a caller whose very first
// read fails learns that instead of opening a stream that looks like an
// empty log.
func (f pollEvents) Subscribe(ctx context.Context, orgID, sessionID string, after int64) (<-chan eventBatch, error) {
	page := f.page
	if page <= 0 {
		page = LogPageSize
	}
	interval := f.interval
	if interval <= 0 {
		interval = StateFeedInterval
	}
	first, err := f.list(ctx, orgID, sessionID, after, page)
	if err != nil {
		return nil, err
	}
	out := make(chan eventBatch, 1)
	go func() {
		defer close(out)
		cursor, batch := after, first
		for {
			// A short read is the end of what is stored; a full one may
			// not be, so the next page is fetched without waiting.
			caughtUp := len(batch) < page
			if n := len(batch); n > 0 {
				cursor = batch[n-1].Pos
			}
			select {
			case out <- eventBatch{Events: batch, CaughtUp: caughtUp}:
			case <-ctx.Done():
				return
			}
			if caughtUp && !wait(ctx, interval) {
				return
			}
			var ok bool
			if batch, ok = f.read(ctx, orgID, sessionID, cursor, page, interval); !ok {
				return
			}
		}
	}()
	return out, nil
}

// read retries until the store answers or ctx ends. A failed read is
// never reported as an empty batch: that would look like the end of the
// log, which is what ends the stream.
func (f pollEvents) read(ctx context.Context, orgID, sessionID string, cursor int64, page int, interval time.Duration) ([]store.Event, bool) {
	said := false
	for {
		evs, err := f.list(ctx, orgID, sessionID, cursor, page)
		if err == nil {
			return evs, true
		}
		if ctx.Err() != nil {
			return nil, false
		}
		// Transient by assumption, as the session feed's is: a stream
		// that survives a blip is worth more than one that ends on it.
		// Said once per outage rather than once per attempt: this feed
		// is per session, so a store that is down would otherwise log
		// every two seconds for every session anyone is watching.
		if !said && f.warn != nil {
			said = true
			f.warn(fmt.Sprintf("session log feed: %v", err))
		}
		if !wait(ctx, interval) {
			return nil, false
		}
	}
}

// wait sleeps unless ctx ends first, reporting whether it slept.
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
