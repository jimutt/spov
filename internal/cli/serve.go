package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
	"github.com/jimutt/spov/internal/telemetry"
	"github.com/jimutt/spov/internal/version"
	"github.com/jimutt/spov/internal/webui"
)

// DefaultListen is where `spov serve` listens when not told otherwise.
// Loopback: the local authenticator is a single shared token, which is
// the right boundary for one machine and the wrong one for a network.
//
// @decision(DL-002)
const DefaultListen = "127.0.0.1:8080"

// newServeCmd runs the spov server. It is the process that holds the
// Modal credential, the platform tokens and the session passwords; every
// other command is a client of it.
//
// @decision(DL-002) @decision(DL-005)
func newServeCmd(env *Env) *cobra.Command {
	var opts serveOptions
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the spov server",
		Long: "Run the spov server: the process that creates sandboxes and mints their credentials.\n\n" +
			"Clients authenticate with the bearer token in " + ServerTokenEnvVar + ", which the server reads from its own environment.\n\n" +
			"With --local the server is the one the CLI starts for itself: it listens on an ephemeral loopback port, " +
			"generates its own token, records both in the state file for the CLI to find, and exits once idle.",
		Args: cobra.NoArgs,
		// A server, and each of its subcommands, reads the user file and
		// the environment only.
		//
		// @decision(DL-026)
		PersistentPreRun: func(*cobra.Command, []string) {
			env.ServerConfig = true
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("listen") {
				opts.Listen = ""
			}
			return runServe(cmd.Context(), env, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Listen, "listen", DefaultListen, "address to listen on (127.0.0.1:0 with --local, which accepts only a loopback address)")
	cmd.Flags().BoolVar(&opts.Local, "local", false, "run as the CLI's own local server: loopback, generated token, state file, idle exit")
	cmd.Flags().StringVar(&opts.StateFile, "state-file", "", "where --local records its address and token (default $XDG_STATE_HOME/spov/server.json)")
	cmd.AddCommand(newOpenAPICmd(env), newMigrateCmd(env), newImageCmd(env))
	return cmd
}

// serveOptions are the serve flags. An empty Listen means the default
// for the mode.
type serveOptions struct {
	Listen    string
	Local     bool
	StateFile string
	// idleExit overrides IdleExit; zero means the default. Tests only.
	idleExit time.Duration
}

// PublicURLEnvVar names the externally visible origin, which a server
// behind a proxy needs: it is what cookies, OAuth redirects and the
// browser guard are checked against.
//
// @decision(DL-010)
const PublicURLEnvVar = "SPOV_PUBLIC_URL"

// ListenEnvVar names the address to bind, for a deployment that has no
// command line to pass --listen on. The flag wins when given.
//
// @decision(DL-010)
const ListenEnvVar = "SPOV_LISTEN"

// TokensInSandboxEnvVar lets a hosted server built without the credential
// proxy start anyway, with every sandbox holding its forge tokens and
// model provider keys.
//
// @decision(DL-034)
const TokensInSandboxEnvVar = "SPOV_TOKENS_IN_SANDBOX"

// proxyCheck refuses a hosted server built without the credential proxy,
// unless its operator has said to run without it.
//
// @decision(DL-034)
func proxyCheck(haveProxy, local bool, optOut string) error {
	if haveProxy || local || optOut == "1" {
		return nil
	}
	return fmt.Errorf("this binary was built without the credential proxy, so every sandbox would hold its forge tokens and model provider keys: "+
		"run scripts/build-proxy.sh before go build, or set %s=1 to run without it", TokensInSandboxEnvVar)
}

// proxyImageFor returns the launcher's source of the proxy image: nil
// without a binary or a provider that can bake one, and otherwise a
// function that bakes on first use and remembers the name. A failed bake
// is tried again on the next launch.
//
// @decision(DL-034)
func proxyImageFor(provider sandbox.Provider, binary []byte) func(context.Context) (string, error) {
	if p := newProxyImages(provider, binary); p != nil {
		return p.ensure
	}
	return nil
}

// proxyImages is the server's proxy image: baked on first use, and baked
// again at most once per process after a sidecar fails to start on it,
// so a Modal outage does not turn every launch into a bake.
//
// @decision(DL-034)
type proxyImages struct {
	builder sandbox.ProxyImageBuilder
	binary  []byte
	// registry, when set, records the image's use and says whether
	// pruning deleted it (DL-016).
	registry store.ImageStore
	now      func() time.Time
	useMu    sync.Mutex
	lastUse  time.Time

	mu   sync.Mutex
	name string
	// rebakeMu serialises re-bakes without holding up ensure, which
	// answers from name meanwhile.
	rebakeMu sync.Mutex
	rebaked  bool
}

// newProxyImages is nil without a binary or a provider that can bake.
func newProxyImages(provider sandbox.Provider, binary []byte) *proxyImages {
	builder, ok := provider.(sandbox.ProxyImageBuilder)
	if !ok || len(binary) == 0 {
		return nil
	}
	return &proxyImages{builder: builder, binary: binary}
}

func (p *proxyImages) ensure(ctx context.Context) (string, error) {
	p.mu.Lock()
	if p.name == "" {
		n, err := p.resolve(ctx)
		if err != nil {
			p.mu.Unlock()
			return "", err
		}
		p.name = n
	}
	name := p.name
	p.mu.Unlock()
	p.use(ctx, name)
	return name, nil
}

// resolve bakes the image on first use, and again when the registry says
// pruning deleted it, since its name still resolves then.
//
// @decision(DL-016)
func (p *proxyImages) resolve(ctx context.Context) (string, error) {
	if p.registry == nil {
		return p.builder.EnsureProxyImage(ctx, p.binary)
	}
	name := sandbox.ProxyImageName(p.binary)
	if img, err := p.registry.Get(ctx, name); err == nil && img.DeletedAt != nil {
		n, err := p.builder.RebakeProxyImage(ctx, p.binary)
		if err != nil {
			return "", err
		}
		if err := p.registry.Record(ctx, store.Image{Name: n, Kind: store.ImageProxy, PublishedAt: p.now()}); err != nil {
			return "", fmt.Errorf("record image %s: %w", n, err)
		}
		return n, nil
	}
	return p.builder.EnsureProxyImage(ctx, p.binary)
}

// use records a launch's use of the image, at most hourly.
//
// @decision(DL-016)
func (p *proxyImages) use(ctx context.Context, name string) {
	if p.registry == nil {
		return
	}
	now := p.now()
	p.useMu.Lock()
	due := now.Sub(p.lastUse) >= time.Hour
	if due {
		p.lastUse = now
	}
	p.useMu.Unlock()
	if !due {
		return
	}
	if err := p.registry.Use(ctx, store.Image{Name: name, Kind: store.ImageProxy}, now); err != nil {
		p.useMu.Lock()
		p.lastUse = time.Time{}
		p.useMu.Unlock()
	}
}

// errAlreadyRebaked refuses a second re-bake in one process.
var errAlreadyRebaked = errors.New("the proxy image was already baked again once since this server started")

// rebake bakes the image again unless that already happened. A bake cut
// short by its context does not count.
func (p *proxyImages) rebake(ctx context.Context) (string, error) {
	p.rebakeMu.Lock()
	defer p.rebakeMu.Unlock()
	if p.rebaked {
		return "", errAlreadyRebaked
	}
	n, err := p.builder.RebakeProxyImage(ctx, p.binary)
	if ctx.Err() == nil {
		p.rebaked = true
	}
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.name = n
	p.mu.Unlock()
	return n, nil
}

// IdleExit is how long a local server stays up with nothing to do.
//
// @decision(DL-002)
const IdleExit = 60 * time.Second

// newOpenAPICmd prints the API document without binding a port.
//
// @decision(DL-005)
func newOpenAPICmd(env *Env) *cobra.Command {
	var downgrade bool
	cmd := &cobra.Command{
		Use:   "openapi",
		Short: "Print the OpenAPI document for the server's API",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			// The document depends only on the registered operations, so
			// the server is built over empty dependencies.
			srv, err := server.New(server.Deps{Store: store.NewMemory(), Auth: auth.StaticAuthenticator{}}, DefaultListen)
			if err != nil {
				return err
			}
			defer srv.Close()
			doc, err := srv.OpenAPI(downgrade)
			if err != nil {
				return err
			}
			_, err = env.Out.Write(doc)
			return err
		},
	}
	cmd.Flags().BoolVar(&downgrade, "3.0", false, "print OpenAPI 3.0 rather than 3.1")
	return cmd
}

// runServe serves until the context ends.
//
// @decision(DL-002)
func runServe(ctx context.Context, env *Env, opts serveOptions) error {
	// Everything started here ends with this function, not with the
	// caller's context.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if opts.Local && opts.Listen != "" && !loopbackListen(opts.Listen) {
		return fmt.Errorf("--local listens only on loopback, not %s; run a hosted server (without --local) to serve a network", opts.Listen)
	}
	res, err := env.config()
	if err != nil {
		return err
	}
	cfg := res.Config

	if err := migrateOnStart(ctx, env, opts); err != nil {
		return err
	}

	listen := opts.Listen
	token := env.Environ.Getenv(ServerTokenEnvVar)
	var statePath string
	if opts.Local {
		// The local server's token is its own, never the environment's:
		// the state file is how the CLI learns it, and its mode is what
		// scopes it to this user.
		if token, err = newLocalToken(); err != nil {
			return err
		}
		if statePath = opts.StateFile; statePath == "" {
			if statePath, err = env.stateFilePath(); err != nil {
				return err
			}
		}
		if listen == "" {
			listen = "127.0.0.1:0"
		}
	} else {
		if token == "" {
			return fmt.Errorf("%s is not set; clients authenticate with it, so the server refuses to start without one", ServerTokenEnvVar)
		}
		if listen == "" {
			// Loopback unless told otherwise: a container must set
			// SPOV_LISTEN=:8080 to be reachable from outside its
			// network namespace, and every deployment file does.
			//
			// @decision(DL-010)
			if listen = env.Environ.Getenv(ListenEnvVar); listen == "" {
				listen = DefaultListen
			}
		}
	}
	provider, err := env.provider(ctx)
	if err != nil {
		return err
	}
	if err := proxyCheck(env.proxyBinary() != nil, opts.Local, env.Environ.Getenv(TokensInSandboxEnvVar)); err != nil {
		return err
	}
	deps, closeStore, err := env.serverDeps(ctx, cfg, provider, token, opts.Local)
	if err != nil {
		return err
	}
	// Metrics are exported only when configured, and never by a local
	// server. Started before anything that records on them.
	//
	// @decision(DL-011)
	if !opts.Local {
		run, err := telemetry.Start(ctx, telemetry.Options{
			PrometheusListen: cfg.Metrics.PrometheusListen,
			Getenv:           env.Environ.Getenv,
			Version:          version.Get().Version,
			Warn:             deps.Warn,
		})
		if err != nil {
			closeStore()
			return err
		}
		defer func() {
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			run.Shutdown(flushCtx)
		}()
		if run.PrometheusAddr != "" {
			_, _ = fmt.Fprintf(env.Err, "spov: serving Prometheus metrics on http://%s/metrics\n", run.PrometheusAddr)
		}
		deps.Metrics = run.Metrics
		if deps.Jobs != nil {
			deps.Jobs.Metrics = run.Metrics
		}
		if deps.Variants != nil {
			deps.Variants.OnFinished = run.Metrics.ImageBuilt
		}
	}
	// @decision(DL-010)
	deps.PublicURL = env.Environ.Getenv(PublicURLEnvVar)
	// A replica whose migrations are behind the binary it is running
	// must stay out of the load balancer's rotation.
	//
	// @decision(DL-010)
	if url := env.Environ.Getenv(DatabaseURLEnvVar); url != "" && !opts.Local {
		deps.Ready = migrationsAtHead(url)
	}
	// Browser login, in hosted mode only: local mode's one credential
	// is the bearer token in its state file.
	//
	// stopSessions ends the session store's cleanup sweep, called in
	// the ordered drain below rather than deferred, since a deferred
	// call would run after closeStore.
	//
	// @decision(DL-007)
	stopSessions := func() {}
	if !opts.Local {
		stop, err := env.wireLogin(ctx, &deps)
		if err != nil {
			closeStore()
			return err
		}
		stopSessions = stop
	}
	// Two-tier credentials: each user's own before the deployment's.
	// Local mode keeps one tier, which is the machine's own store.
	//
	// @decision(DL-042)
	if err := env.wireCredentials(&deps, cfg, opts.Local); err != nil {
		closeStore()
		stopSessions()
		return err
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := ln.Addr().String()
	srv, err := server.New(deps, addr)
	if err != nil {
		_ = ln.Close()
		return err
	}
	defer srv.Close()
	// Every variant build, on a launch's path or in the background, is
	// recorded as the newest build of its request.
	//
	// @decision(DL-016)
	if deps.Variants != nil {
		deps.Variants.OnBuilt = srv.Service().RecordVariant
	}

	// The reconciler is what makes listings honest: it adopts sandboxes
	// this process did not start and notices the ones that stopped.
	//
	// @decision(DL-004)
	recon := env.newReconciler(deps, provider, srv)
	// A local server reconciles once before it announces itself: the
	// CLI that spawned it may be about to `attach` or `stop` a session
	// a previous server launched, and must not see 404 for a sandbox
	// that is running. The provider call is well within the spawn
	// budget.
	//
	// @decision(DL-002)
	if opts.Local {
		if err := recon.Once(ctx); err != nil {
			deps.Warn("reconcile: " + err.Error())
		}
	}
	// Exactly one replica reconciles. Without the lock every replica
	// runs the loop, which means N× the provider calls, concurrent
	// adoptions of the same sandbox, and — the part that matters — the
	// stale-`starting` sweep arbitrating launches it is not running,
	// concluding a slow but healthy launch on another replica.
	//
	// Local mode is one process against no database, so it reconciles
	// directly; there is no lock to take.
	//
	// @decision(DL-004)
	// The recorder runs where the reconciler runs: one replica captures
	// every running session's conversation into the store.
	//
	// @decision(DL-017)
	rec := server.NewRecorder(srv.Service(), deps.Store, env.now, deps.Warn)
	srv.Service().OnStop = rec.Final
	// And the other end: a launch tells the recorder to attach at once,
	// rather than leaving a short interactive session to finish inside
	// the 30 s poll window.
	//
	// @decision(DL-017)
	srv.Service().OnStart = rec.Wake

	// Stopped in the ordered drain below, not by a defer. Nil-safe so
	// local mode — which has no supervisor — drains the same way.
	stopRiver := func() {}
	if deps.Jobs != nil {
		env.wireJobs(deps, srv, rec, cfg.Prep)
		stop, err := env.startRiver(ctx, deps.Store, deps.Jobs, deps.Warn)
		if err != nil {
			// The same cleanup every other error path here does. A
			// bare return would leak the pool and the live sessions.
			closeStore()
			stopSessions()
			return err
		}
		// NOT deferred: a defer runs after the ordered drain below,
		// which closes the pool River is still writing to. It is
		// stopped in the drain instead, before closeStore.
		stopRiver = stop
	}

	reconDone := make(chan struct{})
	runReconciler := env.reconcilerRunner(opts.Local, deps.Warn)
	go func() {
		defer close(reconDone)
		runReconciler(ctx, func(ctx context.Context) {
			var wg sync.WaitGroup
			wg.Go(func() { recon.Run(ctx) })
			// The sweep re-queues unfinished jobs nothing is working,
			// and runs where the reconciler runs for the same reason:
			// N replicas each re-queueing every job on every tick is
			// N× the work for no more safety.
			//
			// @decision(DL-045) @decision(DL-004)
			if deps.Jobs != nil {
				wg.Go(func() { deps.Jobs.RunSweep(ctx) })
			}
			// Re-minting writes into sandboxes, so it runs on one
			// replica too.
			//
			// @decision(DL-021)
			wg.Go(func() { srv.Service().RunRenewals(ctx) })
			// Recreating a sidecar writes into a sandbox too.
			//
			// @decision(DL-034)
			wg.Go(func() { srv.Service().RunSidecarChecks(ctx) })
			// Deleting images is a deployment-wide write, so it runs on
			// one replica too; a local server prunes at start-up.
			//
			// @decision(DL-016)
			wg.Go(func() { srv.Service().RunImagePrune(ctx) })
			// Collecting and pruning exit snapshots, the same way.
			//
			// @decision(DL-085) @decision(DL-086)
			wg.Go(func() { srv.Service().RunSnapshots(ctx) })
			// Shared state is reported only while this replica holds
			// the lock, and withdrawn when it stops holding it.
			//
			// @decision(DL-011)
			wg.Go(func() { srv.Service().RunMetricsSnapshot(ctx) })
			rec.Run(ctx)
			wg.Wait()
		})
	}()

	hs := srv.NewHTTPServer(addr)
	idle := newIdleTracker(env.now)
	hs.Handler = idle.wrap(hs.Handler)
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	// The address is the one line a supervisor needs; a `--listen :0`
	// caller has no other way to learn the port.
	_, _ = fmt.Fprintf(env.Err, "spov server listening on http://%s\n", addr)
	// Said at startup rather than discovered at the first request: a
	// binary from a plain `go build` serves an explanatory page instead
	// of the dashboard, which is easy to mistake for a broken deploy.
	//
	// @decision(DL-055)
	if !webui.Built(srv.WebUI()) {
		deps.Warn("this binary was built without the web app; run scripts/build-web.sh before go build")
	}
	// @decision(DL-034)
	proxyBinary := env.proxyBinary()
	if proxyBinary == nil {
		deps.Warn("this binary was built without the credential proxy, so sandboxes hold their forge tokens and model provider keys; run scripts/build-proxy.sh before go build")
	}
	// The proxy's image is baked in the background at startup, so the
	// first launch does not wait for it. A launch that comes first bakes
	// the same image under the same name, and a failure here is retried
	// by the next launch.
	//
	// @decision(DL-034)
	if bake := proxyImageFor(provider, proxyBinary); bake != nil {
		go func() {
			bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			name, err := bake(bctx)
			if err != nil {
				deps.Warn("the credential proxy's image is not baked yet; the first launch will try again: " + err.Error())
				return
			}
			_, _ = fmt.Fprintf(env.Err, "spov serve: credential proxy image %s is ready\n", name)
		}()
	}

	if opts.Local {
		st := localState{PID: os.Getpid(), URL: "http://" + addr, Token: token, Version: version.Get().String(), StartedAt: env.now()}
		if err := writeState(statePath, st); err != nil {
			return fmt.Errorf("writing the state file: %w", err)
		}
		// Only this server's own file is removed, and it is removed
		// before the listener drains rather than after: a CLI that reads
		// the file during a shutdown would otherwise find a server that
		// answers its probe and refuses its next call.
		defer removeOwnState(statePath, st.PID)
		idleAfter := opts.idleExit
		if idleAfter == 0 {
			idleAfter = IdleExit
		}
		go idle.watch(ctx, cancel, deps.Store, idleAfter)
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	if opts.Local {
		removeOwnState(statePath, os.Getpid())
	}
	// Ordered drain: stop accepting and let in-flight requests finish,
	// then stop the reconciler, then close the provider and the store.
	// In-flight launches are not awaited — they continue in the
	// sandbox, and DL-003's recovery finishes them on another replica.
	//
	// @decision(DL-010)
	budget := ShutdownBudget
	if opts.Local {
		budget = LocalShutdownBudget
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancelShutdown()
	err = hs.Shutdown(shutdownCtx)
	// The reconciler is stopped before the provider it calls, and both
	// go after the listener: an in-flight request may still be using
	// either. Sandboxes keep running; only this process's links close.
	cancel()
	<-reconDone
	closeProvider(provider, deps.Warn)
	// The job queue goes before the pool it works on: a worker still
	// finishing a pass would otherwise write to a closed pool.
	//
	// @decision(DL-045)
	stopRiver()
	// Before the pool it queries.
	stopSessions()
	closeStore()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// closeProvider releases a provider that holds a connection. Not every
// provider does — the fake does not — so this is an optional interface
// rather than a method on Provider.
//
// @decision(DL-010)
func closeProvider(p sandbox.Provider, warn func(string)) {
	c, ok := p.(interface{ Close() error })
	if !ok {
		return
	}
	if err := c.Close(); err != nil && warn != nil {
		warn("closing the sandbox provider: " + err.Error())
	}
}

const (
	// ShutdownBudget bounds the drain in hosted mode. It sits below
	// Kubernetes' 30 s default grace period and Kamal's drain timeout,
	// so the orchestrator never has to kill the process.
	//
	// @decision(DL-010)
	ShutdownBudget = 20 * time.Second
	// LocalShutdownBudget is shorter: a local server has one client,
	// and a person waiting on their terminal.
	//
	// @decision(DL-002) @decision(DL-010)
	LocalShutdownBudget = 10 * time.Second
)

// serverDeps assembles the server from the configuration and credential
// store: the Modal provider, platform tokens from the credential store, provider keys from the
// environment, and the sandbox defaults from config.
//
// @decision(DL-002) @decision(DL-005)
func (e *Env) serverDeps(ctx context.Context, cfg config.Config, provider sandbox.Provider, token string, localMode bool) (server.Deps, func(), error) {
	local, err := auth.NewLocal(token)
	if err != nil {
		return server.Deps{}, nil, err
	}
	// A configured database is what makes a replica shareable: without
	// it every replica has its own sessions, and two of them disagree
	// about every listing.
	//
	// @decision(DL-010) @decision(DL-003)
	st, closeStore, err := e.openStore(ctx, localMode)
	if err != nil {
		return server.Deps{}, nil, err
	}
	warn := func(msg string) { _, _ = fmt.Fprintln(e.Err, "spov serve: "+msg) }
	launcher := &session.Launcher{
		Sandboxes:     provider,
		Now:           e.now,
		NewClient:     e.NewHealthChecker,
		HealthTimeout: e.HealthTimeout,
	}
	// @decision(DL-033) @decision(DL-016)
	images, stateFile := e.imageRegistry(st, warn)
	var proxyName string
	// @decision(DL-034)
	if proxies := newProxyImages(provider, e.proxyBinary()); proxies != nil {
		proxies.registry, proxies.now = images, e.now
		launcher.ProxyImage = proxies.ensure
		launcher.RebakeProxyImage = proxies.rebake
		proxyName = sandbox.ProxyImageName(proxies.binary)
	}
	// @decision(DL-016)
	builder, _ := provider.(sandbox.ImageBuilder)
	if builder != nil {
		launcher.Variants = &session.VariantBuilder{Builder: builder, Warn: warn}
	}
	sb := cfg.Sandbox
	snapshots, err := snapshotSettings(cfg.Snapshots, st, launcher.ProxyImage != nil)
	if err != nil {
		closeStore()
		return server.Deps{}, nil, err
	}
	return server.Deps{
		// @decision(DL-084) @decision(DL-086)
		Snapshots: snapshots,
		// @decision(DL-033) @decision(DL-016)
		Images:   images,
		Resolver: e.addonResolver(stateFile, warn),
		// @decision(DL-013)
		Models:         e.modelSource(stateFile, warn),
		Builder:        builder,
		Variants:       launcher.Variants,
		ProxyImageName: proxyName,
		Store:          st,
		Provider:       provider,
		Forge:          &configForge{env: e, cfg: cfg, warn: warn},
		Launch:         server.NewLaunch(launcher),
		// @decision(DL-034)
		ProxyImage: launcher.ProxyImage,
		Auth:       local,
		Defaults: server.Defaults{
			Image: imageName(cfg),
			// What the deployment pins, published for clients. The
			// image is derived from it unless sandbox.image overrides
			// one, which is the operator's own claim about the build.
			//
			// @decision(DL-035)
			OpenCodeVersion: cfg.OpenCode.Version,
			Port:            cfg.OpenCode.Port,
			Sandbox: store.SandboxConfig{
				CPU: sb.CPU, CPULimit: sb.CPULimit, MemoryMiB: sb.MemoryMiB, MemoryLimitMiB: sb.MemoryLimitMiB,
				Timeout: sb.Timeout, IdleTimeout: sb.IdleTimeout,
				Regions: sb.Regions, Cloud: sb.Cloud, Workdir: sb.Workdir, InboundCIDRs: sb.InboundCIDRs,
			},
			Egress:             sb.Egress,
			EgressExtraDomains: sb.EgressExtraDomains,
			// @decision(DL-016) @decision(DL-033)
			BuildOnDemand: cfg.Modal.BuildOnDemand,
			Addons:        sb.Addons,
			Apt:           sb.Apt,
		},
		// Per-user limits for a hosted server; local mode has one user
		// and the defaults are generous enough to be invisible.
		//
		// @decision(DL-009)
		Quotas: server.Quotas{
			MaxConcurrentSessions: cfg.Quotas.MaxConcurrentSessions,
			MaxConcurrentJobs:     cfg.Quotas.MaxConcurrentJobs,
			MaxJobDuration:        cfg.Quotas.MaxJobDuration,
			MaxTimeout:            cfg.Quotas.MaxTimeout,
			MaxCPU:                cfg.Quotas.MaxCPU,
			MaxMemoryMiB:          cfg.Quotas.MaxMemoryMiB,
		},
		// Every credential of a local server is its user's own.
		//
		// @decision(DL-043)
		GitAllowAll:  localMode || cfg.Git.AllowAll,
		ProviderKeys: e.providerKeys(),
		Identity:     git.Identity{Name: cfg.Git.Name, Email: cfg.Git.Email},
		User:         e.username(),
		Now:          e.now,
		Warn:         warn,
		// The supervisor exists only where River can: it is a durable
		// queue on Postgres, and local mode is one process against no
		// database. A local server refuses a job and says why.
		//
		// @decision(DL-046) @decision(DL-045)
		Jobs: newSupervisor(st, provider, warn),
	}, closeStore, nil
}

// snapshotSettings is the server's snapshot setting: nil, taking none,
// unless snapshots are enabled, the proxy holds the secrets and the
// sessions are kept in Postgres, where a resume can find them.
//
// @decision(DL-084) @decision(DL-086)
func snapshotSettings(c config.Snapshots, st store.Store, proxy bool) (*server.SnapshotSettings, error) {
	if !c.Enabled {
		return nil, nil
	}
	s := server.SnapshotSettings{Retention: c.Retention, MaxPerUser: c.MaxPerUser}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if _, persistent := st.(*postgres.Store); !persistent || !proxy {
		return nil, nil
	}
	return &s, nil
}

// newSupervisor builds the job supervisor, or nil where there is no
// database to run a queue on.
//
// Its Queue is left unset here and filled in by serve once River's
// client exists: the client needs the supervisor to build its worker,
// so the two are necessarily wired in that order.
//
// @decision(DL-045) @decision(DL-046)
func newSupervisor(st store.Store, provider sandbox.Provider, warn func(string)) *jobs.Supervisor {
	if _, ok := st.(*postgres.Store); !ok {
		return nil
	}
	return &jobs.Supervisor{Store: st, Provider: provider, Warn: warn}
}

// reconcilerRunner decides how the reconcile loop is run: under the
// DL-004 advisory lock when there is a database to take it in, directly
// otherwise.
//
// A hosted deployment without the lock has every replica reconciling,
// which is N× the provider calls, concurrent adoptions of the same
// sandbox, and a stale-`starting` sweep arbitrating launches the
// sweeping replica is not running. Local mode is one process against no
// database, so there is no lock to take.
//
// @decision(DL-004)
func (e *Env) reconcilerRunner(local bool, warn func(string)) func(context.Context, func(context.Context)) {
	url := e.Environ.Getenv(DatabaseURLEnvVar)
	if url == "" || local {
		return func(ctx context.Context, work func(context.Context)) { work(ctx) }
	}
	leader := &postgres.Leader{URL: url, Warn: warn}
	return leader.Run
}

// openStore returns the store the server runs on and a function that
// releases it. A configured database gives Postgres; anything else —
// every local run — gives the memory store, which needs no closing.
//
// @decision(DL-010)
func (e *Env) openStore(ctx context.Context, local bool) (store.Store, func(), error) {
	// Local mode never opens a database, whatever the environment says:
	// an exported SPOV_DATABASE_URL is a hosted deployment's variable,
	// not an instruction to the server someone's CLI just spawned.
	//
	// @decision(DL-003)
	url := e.Environ.Getenv(DatabaseURLEnvVar)
	if url == "" || local {
		return store.NewMemory(), func() {}, nil
	}
	pg, err := postgres.Open(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	return pg, pg.Close, nil
}

// configForge resolves repositories on the configured platforms. It is
// the CLI's platform logic behind the server's interface.
//
// @decision(DL-005) @decision(DL-002)
type configForge struct {
	env  *Env
	cfg  config.Config
	warn func(string)
}

func (f *configForge) ListRepos(ctx context.Context) ([]git.PlatformRepo, []string, error) {
	repos, failures, err := f.env.listAllRepos(ctx, f.cfg)
	if err != nil {
		return nil, nil, err
	}
	// A platform that could not be listed does not fail the listing:
	// tokens for different platforms expire independently, and the
	// repositories the user wants may all be on the ones that answered.
	// The failures travel to the client, which is where the user is.
	//
	// @decision(DL-028)
	for _, msg := range failures {
		f.warn(msg)
	}
	return repos, failures, nil
}

func (f *configForge) Selections(ctx context.Context, refs []store.RepoRef) ([]git.Selection, error) {
	apiRefs := make([]server.RepoRef, len(refs))
	for i, r := range refs {
		apiRefs[i] = server.RepoRef(r)
	}
	return f.env.selectionsFor(ctx, f.cfg, apiRefs)
}

// Platforms builds each configured platform the caller holds a
// credential for; one without a credential is left out, and one that
// cannot be built is left out with a warning, so it cannot fail a
// launch that did not select it.
//
// @decision(DL-043)
func (f *configForge) Platforms(ctx context.Context) ([]git.Platform, error) {
	var out []git.Platform
	for _, p := range f.cfg.Platforms {
		if _, err := f.env.platformToken(ctx, p); err != nil {
			continue
		}
		platform, err := f.env.platform(ctx, p)
		if err != nil {
			f.warn(fmt.Sprintf("platform %s is left out of git access all: %s", p.Name, redact.Error(err)))
			continue
		}
		out = append(out, platform)
	}
	return out, nil
}

// newLocalToken generates the local bearer credential: 32 random bytes,
// base64url.
//
// @decision(DL-002) @decision(DL-006)
func newLocalToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating the server token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// removeOwnState deletes the state file if it still names this process.
func removeOwnState(path string, pid int) {
	st, err := readState(path)
	if err != nil || st == nil || st.PID != pid {
		return
	}
	_ = os.Remove(path)
}

// idleTracker knows whether the server has anything to do: a request in
// flight (an SSE stream is one), or recent activity. Starting sessions
// are checked in the store, since a launch outlives the request that
// asked for it.
//
// @decision(DL-002)
type idleTracker struct {
	now      func() time.Time
	mu       sync.Mutex
	inFlight int
	last     time.Time
}

func newIdleTracker(now func() time.Time) *idleTracker {
	return &idleTracker{now: now, last: now()}
}

func (t *idleTracker) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.mu.Lock()
		t.inFlight++
		t.mu.Unlock()
		defer func() {
			t.mu.Lock()
			t.inFlight--
			t.last = t.now()
			t.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// idle reports whether nothing has happened for at least d.
func (t *idleTracker) idle(d time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inFlight == 0 && t.now().Sub(t.last) >= d
}

// watch stops the server once it has been idle for d with no session
// starting. Kept sessions do not hold it open: their sandboxes are
// their own record, and the next server adopts them.
//
// @decision(DL-002)
func (t *idleTracker) watch(ctx context.Context, stop context.CancelFunc, st store.Store, d time.Duration) {
	tick := time.NewTicker(min(d/4, 5*time.Second))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !t.idle(d) {
			continue
		}
		live, err := st.Sessions().ListLive(ctx)
		if err != nil {
			continue
		}
		starting := false
		for _, s := range live {
			if s.State == store.StateStarting {
				starting = true
				break
			}
		}
		if !starting {
			stop()
			return
		}
	}
}

// wireLogin sets up a hosted server's browser login and CLI tokens on
// deps, when an OAuth app is configured, and starts their background
// work under ctx. The returned function ends the session store's
// cleanup sweep.
//
// @decision(DL-007) @decision(DL-006)
func (e *Env) wireLogin(ctx context.Context, deps *server.Deps) (func(), error) {
	login, stop, err := e.newLogin(ctx, deps.Store, deps.PublicURL, deps.Warn)
	if err != nil {
		return nil, err
	}
	if login != nil {
		deps.Login = login
		// Cookie first, then the CLI token, then the single shared
		// bearer.
		cookies := &auth.CookieAuthenticator{
			Sessions:  login.Sessions,
			Users:     deps.Store.Users(),
			Allowlist: login.Allowlist,
			OrgID:     server.DefaultOrgID,
		}
		// The approval page authenticates with the cookie alone.
		deps.BrowserAuth = cookies
		deps.Auth = auth.Chain{
			cookies,
			&auth.TokenAuthenticator{
				Tokens:    deps.Store.Tokens(),
				Users:     deps.Store.Users(),
				Allowlist: login.Allowlist,
				Now:       e.now,
				Warn:      deps.Warn,
			},
			deps.Auth,
		}
		// Every replica re-reads the allowlist on its own timer.
		//
		// @decision(DL-007)
		go login.Allowlist.Run(ctx)

		deps.Device = &server.Device{
			Store:     deps.Store,
			PublicURL: deps.PublicURL,
			Login:     login,
			Now:       e.now,
			Warn:      deps.Warn,
		}
		// Expired device codes are reaped in the background.
		go deps.Device.Sweep(ctx)
	}
	// Outside the branch above: a hosted server without login must still
	// trap SIGHUP.
	go watchAllowlist(ctx, login, deps.Warn)
	return stop, nil
}

// newReconciler is the reconciler `spov serve` runs over deps.
//
// @decision(DL-004)
func (e *Env) newReconciler(deps server.Deps, provider sandbox.Provider, srv *server.Server) *server.Reconciler {
	return &server.Reconciler{
		Store: deps.Store, Provider: provider, Now: e.now, Warn: deps.Warn,
		DefaultOrg: auth.LocalID, DefaultOwner: auth.LocalID,
		Forget:        srv.Service().ForgetSession,
		LaunchTimeout: deps.LaunchTimeout,
		// The backstop: a sandbox older than any session should be is
		// terminated whatever the store says.
		//
		// @decision(DL-009)
		MaxSessionAge: deps.Quotas.MaxSessionAge(),
		// @decision(DL-011)
		Metrics: deps.Metrics,
	}
}

// wireJobs gives the supervisor its seams. The supervisor's own half:
// the launch, the opening turn and the two end-of-job reads all belong
// to the Service, which owns the launcher and the credentials. The
// supervisor holds them as functions so its sequencing stays testable
// without either. The scenario suites call this too, so they test the
// wiring that ships.
//
// @decision(DL-045) @decision(DL-022)
func (env *Env) wireJobs(deps server.Deps, srv *server.Server, rec *server.Recorder, prepCfg prep.Config) {
	svc := srv.Service()
	deps.Jobs.Provision = svc.ProvisionJob
	deps.Jobs.Open = svc.OpenJobGoal
	deps.Jobs.Adopt = svc.AdoptJobSandbox
	deps.Jobs.OpenOut = svc.OpenJobOut
	deps.Jobs.Evidence = svc.CollectJobEvidence
	deps.Jobs.Outcome = svc.CollectJobOutcome
	deps.Jobs.Mark = svc.MarkJobEvent
	// Nil when no provider is configured, which skips the pass and
	// opens each job on its goal alone.
	//
	// @decision(DL-047)
	deps.Jobs.Prepare = env.prepareGoal(prepCfg, deps.Models, deps.Warn)
	// Gate 2 for a pr job. The forge is resolved for the job's
	// owner, by the same path the launch resolves it.
	//
	// @decision(DL-050)
	deps.Jobs.FindPullRequest = jobs.FindPullRequestVia(svc.JobOwnerForge, deps.Store)
	// Nothing else stops a finished job's sandbox.
	//
	// @decision(DL-050)
	deps.Jobs.Terminate = svc.TerminateJobSandbox
	// One last read before the gates, so the verdict sees the
	// run's final turn.
	//
	// @decision(DL-050)
	deps.Jobs.CatchUp = svc.CatchUpJobSession
	// The supervisor nudges the recorder the moment it opens a
	// job, rather than leaving it to the reconciler's poll. The
	// poll stays the backstop.
	//
	// @decision(DL-017)
	deps.Jobs.Wake = rec.Wake
	// Writing `steered` enacts the pause rather than merely
	// recording it, so the row's state always implies the plugin's.
	//
	// @decision(DL-054)
	deps.Jobs.Loop = svc.SetJobLoopPaused
	// @decision(DL-049)
	deps.Jobs.EndRun = svc.EndJobRun
	deps.Jobs.Now = env.now
}
