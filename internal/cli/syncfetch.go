package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/session"
)

// syncSource is where a sync-back comes from: the archive route, and
// the JSON route an older server answers instead.
type syncSource struct {
	archive func(ctx context.Context) (io.ReadCloser, error)
	export  func(ctx context.Context) (server.SyncExportView, error)
}

// archiveFetchTimeout bounds a sync-back's transfer and spool: the
// server's own bound on the archive, and a minute for the request.
//
// @decision(DL-178)
const archiveFetchTimeout = session.ArchiveTimeout + time.Minute

// progressFrom is the declared size over which a sync-back says how
// large it is and shows its progress.
const progressFrom = 16 << 20

// errSyncInterrupted is a sync-back the user stopped during its
// transfer.
var errSyncInterrupted = errors.New("the sync-back was interrupted before it was applied; nothing was changed")

// fetchSync takes a sync-back into a spool under the checkout's git
// directory. A Ctrl-C or SIGTERM during the transfer ends it and
// removes the spool.
// The caller lands the spool and removes it.
//
// @decision(DL-178)
func (e *Env) fetchSync(ctx context.Context, root string, src syncSource) (git.SpooledSync, error) {
	dir, err := git.NewSyncSpool(ctx, root)
	if err != nil {
		return git.SpooledSync{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, archiveFetchTimeout)
	defer cancel()
	interrupted := atomic.Bool{}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			interrupted.Store(true)
			cancel()
		case <-ctx.Done():
		}
	}()

	spool, err := e.fetchInto(ctx, dir, src)
	if err != nil {
		_ = os.RemoveAll(dir)
		if interrupted.Load() {
			return git.SpooledSync{}, errSyncInterrupted
		}
		return git.SpooledSync{}, err
	}
	return spool, nil
}

func (e *Env) fetchInto(ctx context.Context, dir string, src syncSource) (git.SpooledSync, error) {
	body, err := src.archive(ctx)
	if client.IsUnservedRoute(err) {
		return e.fetchJSON(ctx, dir, src)
	}
	if err != nil {
		return git.SpooledSync{}, err
	}
	defer func() { _ = body.Close() }()
	progress := &syncProgress{env: e}
	defer progress.stop()
	spool, err := git.SpoolSyncArchive(progress.reader(body), dir, progress.start)
	if err != nil {
		return git.SpooledSync{}, err
	}
	return spool, nil
}

// fetchJSON takes a sync-back from a server that has only the JSON
// route, into the same kind of spool.
//
// @decision(DL-178)
func (e *Env) fetchJSON(ctx context.Context, dir string, src syncSource) (git.SpooledSync, error) {
	view, err := src.export(ctx)
	if err != nil {
		return git.SpooledSync{}, err
	}
	st := git.ExportedState{Branch: view.Branch, BaseCommit: view.BaseCommit, HeadCommit: view.HeadCommit, Deleted: view.Deleted}
	if view.Bundle != "" {
		if st.Bundle, err = base64.StdEncoding.DecodeString(view.Bundle); err != nil {
			return git.SpooledSync{}, fmt.Errorf("decoding exported bundle: %w", err)
		}
	}
	if view.WorktreeTar != "" {
		if st.WorktreeTar, err = base64.StdEncoding.DecodeString(view.WorktreeTar); err != nil {
			return git.SpooledSync{}, fmt.Errorf("decoding exported worktree archive: %w", err)
		}
	}
	return git.SpoolExported(dir, st)
}

// holdInterrupts keeps Ctrl-C from ending the process while a landing
// writes into the checkout, saying so once; the returned func lets go.
//
// @decision(DL-178)
func (e *Env) holdInterrupts() func() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		said := false
		for {
			select {
			case <-sigs:
				if !said {
					said = true
					_, _ = fmt.Fprintln(e.Err, "Writing the sync-back into the checkout; it stops when that is done.")
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}

// syncProgress says how large a sync-back is once its metadata arrives
// and, on a terminal, redraws the bytes received at most twice a second.
//
// @decision(DL-178)
type syncProgress struct {
	env      *Env
	received atomic.Int64
	once     sync.Once
	quit     chan struct{}
	wg       sync.WaitGroup
}

func (p *syncProgress) reader(r io.Reader) io.Reader {
	return readFunc(func(b []byte) (int, error) {
		n, err := r.Read(b)
		p.received.Add(int64(n))
		return n, err
	})
}

func (p *syncProgress) start(m git.SyncMeta) {
	total := m.ObjectBytes + m.WorktreeBytes
	if total <= progressFrom {
		return
	}
	var parts []string
	if m.Commits > 0 {
		parts = append(parts, plural(m.Commits, "commit"))
	}
	if m.Files > 0 {
		parts = append(parts, plural(m.Files, "file"))
	}
	_, _ = fmt.Fprintf(p.env.Err, "Receiving %s (%s)...\n", strings.Join(parts, " and "), mebibytesText(total))
	if !p.env.stderrIsTerminal() {
		return
	}
	p.quit = make(chan struct{})
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				_, _ = fmt.Fprintf(p.env.Err, "\r  %s received", mebibytesText(p.received.Load()))
			case <-p.quit:
				_, _ = fmt.Fprintf(p.env.Err, "\r  %s received\n", mebibytesText(p.received.Load()))
				return
			}
		}
	}()
}

func (p *syncProgress) stop() {
	p.once.Do(func() {
		if p.quit != nil {
			close(p.quit)
			p.wg.Wait()
		}
	})
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(b []byte) (int, error) { return f(b) }

func mebibytesText(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KiB", (n+1023)>>10)
	}
	return fmt.Sprintf("%d MiB", (n+(1<<19))>>20)
}

// plural is n and the noun, with an s when n is not one.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
