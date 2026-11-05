package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/session"
)

// liveSession is a session this process has a loopback proxy for. It is
// what the CLI hands to the TUI or the browser; it holds no credential
// itself, the proxy does.
//
// @decision(DL-030)
type liveSession struct {
	// View is the session as the server last showed it.
	View server.SessionView
	// URL is the loopback proxy address. It dies with this process.
	URL string
	// Health is what the OpenCode server reported through the proxy.
	Health opencode.Health
	proxy  *proxy.Proxy
}

// Detach stops the proxy and leaves the session running.
func (s *liveSession) Detach() error {
	if s.proxy == nil {
		return nil
	}
	err := s.proxy.Close()
	s.proxy = nil
	return err
}

// Name is what to call the session in hints: its name, or its ID for a
// session that somehow has none.
func (s *liveSession) Name() string {
	if s.View.Name != "" {
		return s.View.Name
	}
	return s.View.ID
}

// connectSession brings up the loopback proxy for a running session and
// health-checks through it, so a proxy that cannot reach the sandbox fails
// here rather than as a broken TUI.
//
// @decision(DL-030)
func (e *Env) connectSession(ctx context.Context, c *client.Client, view server.SessionView) (*liveSession, error) {
	connector := session.ConnectorFunc(func(ctx context.Context) (session.Connection, error) {
		// The current client each time: an earlier mint may have
		// replaced it. The proxy calls this from its own goroutines,
		// so nothing here is written without the lock.
		cur, err := e.client(ctx)
		if err != nil {
			return session.Connection{}, err
		}
		conn, err := cur.Connect(ctx, view.ID)
		if lostServer(ctx, err) {
			// A local server exited while this session ran. Resolve
			// again, which spawns one, and connect by name, since the
			// new server knows the session under a new ID.
			//
			// @decision(DL-002)
			fresh, rerr := e.reconnect(ctx, cur)
			if rerr != nil {
				return session.Connection{}, err
			}
			conn, err = fresh.Connect(ctx, viewName(view))
		}
		if err != nil {
			return session.Connection{}, err
		}
		return session.Connection{
			URL: conn.URL, Token: conn.Token, User: conn.User, Password: conn.Password,
			IdleTimeout: time.Duration(conn.IdleTimeoutSecs) * time.Second,
		}, nil
	})
	prox, err := e.startClientProxy(ctx, connector)
	if err != nil {
		return nil, err
	}

	timeout := e.HealthTimeout
	if timeout == 0 {
		timeout = ProxyHealthTimeout
	}
	healthCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	health, err := e.newHealthChecker(prox.URL, "").WaitHealthy(healthCtx, 250*time.Millisecond)
	if err != nil {
		_ = prox.Close()
		return nil, fmt.Errorf("the session is running but cannot be reached through the local proxy: %w", err)
	}
	return &liveSession{View: view, URL: prox.URL, Health: health, proxy: prox}, nil
}

func (e *Env) startClientProxy(ctx context.Context, c session.Connector) (*proxy.Proxy, error) {
	warn := func(msg string) { e.deferWarning("spov: " + msg) }
	if e.StartClientProxy != nil {
		return e.StartClientProxy(ctx, c, warn)
	}
	return session.StartClientProxy(ctx, c, warn)
}

func (e *Env) newHealthChecker(url, password string) session.HealthChecker {
	if e.NewHealthChecker != nil {
		return e.NewHealthChecker(url, password)
	}
	return opencode.NewClient(url, password)
}

// launchReporter feeds the server's launch events into the drawn view
// when there is one, or prints the plain progress lines otherwise.
//
// @decision(DL-024)
type launchReporter struct {
	env      *Env
	steps    session.StepReporter
	progress io.Writer
	view     server.SessionView
	problem  server.ProblemView
}

func (r *launchReporter) Step(ev server.StepEvent) {
	step := session.Step(ev.Step)
	switch ev.State {
	case "started":
		if r.steps != nil {
			r.steps.Start(step)
		} else if r.progress != nil {
			_, _ = fmt.Fprintf(r.progress, "%s...\n", step)
		}
	case "done":
		if r.steps != nil {
			r.steps.Done(step)
		}
	}
}

func (r *launchReporter) Note(ev server.NoteEvent) {
	if r.steps != nil {
		r.steps.Note(ev.Message)
		return
	}
	// Held rather than printed when a view is up, for the same reason
	// every other warning is; without one this prints immediately.
	r.env.deferWarning("spov: " + ev.Message)
}

func (r *launchReporter) Ready(ev server.ReadyEvent) { r.view = ev.Session }

func (r *launchReporter) Failed(ev server.ErrorEvent) { r.problem = ev.Problem }

// launchSession creates a session and follows its launch to the end.
// It returns the ready session; a failed launch is an error carrying the
// server's problem.
//
// @decision(DL-024)
func (e *Env) launchSession(ctx context.Context, c *client.Client, req server.SessionRequest) (server.SessionView, error) {
	id, err := c.CreateSession(ctx, req)
	if err != nil {
		return server.SessionView{}, err
	}

	rep := &launchReporter{env: e, progress: e.Err}
	if e.activeView != nil {
		rep.steps, rep.progress = e.activeView, nil
	}
	err = c.Follow(ctx, id, rep, client.FollowOptions{})
	switch {
	case err == nil:
		return rep.view, nil
	case errors.Is(err, client.ErrLaunchFailed):
		// The server has already torn down whatever it created; the
		// problem is the whole story.
		return server.SessionView{}, fmt.Errorf("launch failed: %s", rep.problem.Detail)
	case ctx.Err() != nil:
		// The user gave up: a delete cancels the server's launch and
		// terminates whatever exists. Bounded, and detached from the
		// cancelled context.
		//
		// @decision(DL-029)
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminateTimeout)
		defer cancel()
		if derr := c.DeleteSession(stopCtx, id); derr != nil {
			return server.SessionView{}, fmt.Errorf("%w; the launch could not be cancelled (%w), so the sandbox may still be running: %s",
				err, derr, stopHint(req.Name))
		}
		return server.SessionView{}, fmt.Errorf("%w; the launch was cancelled", err)
	default:
		// Lost the server mid-launch. The session may be running and
		// billing; say how to stop it.
		return server.SessionView{}, fmt.Errorf("%w\nthe launch may still have succeeded; %s", err, stopHint(req.Name))
	}
}

// terminateSession tears the session's sandbox down after the TUI exits.
//
// @decision(DL-029)
func terminateSession(ctx context.Context, env *Env, c *client.Client, sess *liveSession) error {
	_, _ = fmt.Fprintln(env.Err, "Terminating the sandbox...")
	detach(env, sess)

	// Detached from the caller's context so a Ctrl-C cannot prevent the
	// teardown it is meant to trigger, but bounded so a hung server does
	// not hang spov on the one operation the user most wants to finish.
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminateTimeout)
	defer cancel()

	if err := env.deleteSession(closeCtx, c, sess.View); err != nil {
		// The sandbox is still running and still billing. Say so.
		_, _ = fmt.Fprintf(env.Err, "spov: the sandbox could NOT be terminated and is still billing: %v\n", err)
		printAttachHint(env, sess.Name())
		return err
	}
	return nil
}

// terminateTimeout bounds teardown. Modal's terminate waits for the
// sandbox to stop, so an unbounded call could hang indefinitely.
//
// @decision(DL-029)
const terminateTimeout = time.Minute

// detach releases local resources while leaving the sandbox running. A
// failure is only a warning: the sandbox is reachable either way.
//
// @decision(DL-029)
func detach(env *Env, sess *liveSession) {
	if err := sess.Detach(); err != nil {
		_, _ = fmt.Fprintf(env.Err, "spov: warning: %v\n", err)
	}
}

// printSessionSummary reports what was launched, including the estimated
// hourly cost: a forgotten sandbox bills for its whole lifetime.
//
// @decision(DL-029)
func printSessionSummary(env *Env, sess *liveSession) {
	printLaunchedSummary(env, sess.View, sess.Health.Version)
}

// printLaunchedSummary is the summary for a session whose OpenCode
// version may not be known yet, because the proxy could not reach it.
func printLaunchedSummary(env *Env, v server.SessionView, version string) {
	if version == "" {
		version = "version unknown"
	}
	_, _ = fmt.Fprintf(env.Err, "\nSandbox %s is ready (%s).\n", v.SandboxID, version)
	for _, slug := range v.Clones {
		_, _ = fmt.Fprintf(env.Err, "  cloned %s\n", slug)
	}
	_, _ = fmt.Fprintf(env.Err, "  working directory: %s\n", v.Landing)
	_, _ = fmt.Fprintf(env.Err, "  timeout %s, idle timeout %s, about %s\n",
		time.Duration(v.Sandbox.TimeoutSecs)*time.Second, time.Duration(v.Sandbox.IdleSecs)*time.Second, hourlyCost(v.Sandbox))
}

// printAttachHint tells the user how to reach or stop a sandbox spov is
// leaving behind.
//
// @decision(DL-029) @decision(DL-027)
func printAttachHint(env *Env, name string) {
	printAttachHintMode(env, name, false)
}

func printAttachHintMode(env *Env, name string, synced bool) {
	_, _ = fmt.Fprintf(env.Err, "\nThe sandbox is still running.\n")
	// No URL is printed: the sandbox has no public address, and the
	// loopback one dies with this process.
	//
	// @decision(DL-030)
	if synced {
		_, _ = fmt.Fprintf(env.Err, "  attach:  spov attach %s --sync\n", name)
		_, _ = fmt.Fprintf(env.Err, "  web UI:  spov attach %s --web --sync\n", name)
		_, _ = fmt.Fprintf(env.Err, "  stop:    spov stop %s --sync\n", name)
		return
	}
	_, _ = fmt.Fprintf(env.Err, "  attach:  spov attach %s\n", name)
	_, _ = fmt.Fprintf(env.Err, "  web UI:  spov attach %s --web\n", name)
	_, _ = fmt.Fprintf(env.Err, "  stop:    spov stop %s\n", name)
}

// hourlyCost formats what a sandbox of this size costs per hour. The
// rates live in internal/sandbox, where the server's usage endpoint
// reaches them too.
//
// @decision(DL-029) @decision(DL-009)
func hourlyCost(s server.SandboxConfigView) string {
	return fmt.Sprintf("$%.2f/hour", sandbox.HourlyCost(s.CPU, s.MemoryMiB))
}

// deleteSession terminates a session by ID, then by name, since a
// replacement local server knows it under a new ID. If no server answers,
// a new one is resolved first.
//
// @decision(DL-002)
func (e *Env) deleteSession(ctx context.Context, c *client.Client, v server.SessionView) error {
	err := c.DeleteSession(ctx, v.ID)
	if lostServer(ctx, err) {
		fresh, rerr := e.reconnect(ctx, c)
		if rerr != nil {
			return err
		}
		c = fresh
		err = c.DeleteSession(ctx, v.ID)
	}
	if client.IsNotFound(err) && v.Name != "" {
		err = c.DeleteSession(ctx, v.Name)
	}
	return err
}
