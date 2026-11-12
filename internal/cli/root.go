// Package cli assembles the spov command tree.
//
// @decision(DL-001)
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/proxybin"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/version"
	"github.com/jimutt/spov/internal/webui"
)

// Env carries the process-level dependencies every command needs, so that
// commands can be exercised in tests without touching the real
// environment.
//
// The CLI is a client of the spov API: every command that touches a
// session goes through Client. The provider and credential fields exist
// for `spov serve`, which is the process that holds those.
//
// @decision(DL-002)
type Env struct {
	cachedConfig *config.Result

	Out     io.Writer
	Err     io.Writer
	Environ config.Env
	Workdir string
	// Credentials is the secret store. Nil means resolve the real one
	// lazily, so commands that need no secrets never probe the keyring.
	Credentials credentials.Store
	// Client is the API client. Nil means resolve a server from the
	// --server flag, SPOV_SERVER, and config on first use. Guarded by
	// clientMu: the proxy mints from its own goroutines.
	//
	// @decision(DL-002)
	Client *client.Client
	// clientMu is a pointer so Env stays copyable; NewRoot sets it, and
	// a bare Env used directly in tests gets one on first use.
	clientMu *sync.Mutex
	// ServerURL is the --server flag.
	//
	// @decision(DL-002)
	ServerURL string
	// StateDir overrides where the local server's state file and log
	// live. Empty means $XDG_STATE_HOME/spov or ~/.local/state/spov.
	//
	// @decision(DL-002)
	StateDir string
	// Spawn starts the local server. Nil means exec this binary with
	// `serve --local`, detached; tests substitute it.
	//
	// @decision(DL-002)
	Spawn func(ctx context.Context) error
	// SpawnTimeout bounds the wait for a spawned server; zero means
	// SpawnTimeout.
	SpawnTimeout time.Duration
	// ServerConfig loads configuration from the user file and the
	// environment only, without the project file: `spov serve` sets it.
	//
	// @decision(DL-026)
	ServerConfig bool
	// CompletionTimeout bounds a shell completion's lookup; zero means
	// completionTimeout.
	//
	// @decision(DL-023)
	CompletionTimeout time.Duration
	// AttachPoll is how often `spov attach` re-reads a session it is
	// waiting on; zero means attachPoll.
	//
	// @decision(DL-025)
	AttachPoll time.Duration
	// JobPoll is how often `spov jobs wait` re-reads a job; zero means
	// jobPoll.
	//
	// @decision(DL-103)
	JobPoll time.Duration
	// StderrIsTerminal overrides the check that decides whether a wait
	// draws progress lines; nil asks the terminal.
	//
	// @decision(DL-103)
	StderrIsTerminal func() bool
	// StdoutIsTerminal overrides the check that decides whether a
	// report is printed raw (to a pipe or a file) or with its control
	// characters made visible (to a terminal); nil asks the terminal.
	//
	// @decision(DL-136)
	StdoutIsTerminal func() bool
	// ProcessAlive reports whether a pid is running. Nil means signal 0.
	ProcessAlive func(pid int) bool
	// StopProcess stops a server of another build. Nil means SIGTERM and
	// a bounded wait.
	StopProcess func(pid int)
	// Provider supplies sandboxes to `spov serve`. Nil means connect to
	// Modal on first use.
	Provider sandbox.Provider
	// NewProvider builds the provider when Provider is nil; defaults to
	// connecting to Modal.
	NewProvider func(ctx context.Context, cfg config.Config) (sandbox.Provider, error)
	// ResolveAddons resolves partial add-on versions. Nil asks mise's
	// and the apt repositories' indexes.
	//
	// @decision(DL-033)
	ResolveAddons server.AddonResolver
	// Models is the model list. Nil reads models.dev's mirror (DL-013);
	// a test sets a fixed one.
	Models server.ModelCatalogue
	// Now supplies timestamps; defaults to time.Now.
	Now func() time.Time
	// Stdin is handed to the OpenCode TUI. Nil means os.Stdin.
	Stdin *os.File
	// RunTUI hands the terminal to the OpenCode client. Nil means run the
	// real one; tests substitute it to exercise the launch flow without a
	// terminal.
	RunTUI func(ctx context.Context, tui opencode.TUI) (int, error)
	// LocateBinary finds the local opencode. Nil means the real locator.
	LocateBinary func(ctx context.Context, version string) (opencode.Located, error)
	// NewPlatform builds a git platform. Nil means the real one, which
	// contacts the server during construction.
	NewPlatform func(ctx context.Context, p config.Platform, token string) (git.Platform, error)
	// PlatformToken resolves one platform's credential. Nil reads this
	// machine's credential store; hosted mode installs a resolver that
	// answers per user, the user's own token before the deployment's.
	//
	// @decision(DL-042)
	PlatformToken func(ctx context.Context, p config.Platform) (string, error)
	// GhToken reads gh's token for a host. Nil runs `gh auth token`.
	//
	// @decision(DL-083)
	GhToken func(ctx context.Context, host string) (string, error)
	// SelectRepos asks the user which repositories to bootstrap when none
	// are configured. Nil disables the interactive picker, which is what
	// tests and non-interactive runs want. Repositories carry their
	// platform, since the list spans every configured platform.
	//
	// @decision(DL-028)
	SelectRepos func(repos []RepoChoice) ([]RepoChoice, error)
	// LocalIdentity reads the machine's git identity from a directory.
	// Nil runs git; tests substitute it.
	//
	// @decision(DL-044)
	LocalIdentity func(ctx context.Context, dir string) (git.Identity, error)
	// NewHealthChecker builds the client that checks the OpenCode server
	// through the loopback proxy. Nil means the real HTTP client.
	NewHealthChecker func(url, password string) session.HealthChecker
	// PromptSecret reads a secret without echoing it. Nil reads from the
	// terminal; tests substitute it.
	PromptSecret func(prompt string) (string, error)
	// Confirm asks a yes-or-no question, defaulting to no. Nil asks on
	// the terminal, and answers no where there is none.
	//
	// @decision(DL-090)
	Confirm func(prompt string) (bool, error)
	// LookPath finds an executable. Nil means exec.LookPath.
	//
	// @decision(DL-025)
	LookPath func(name string) (string, error)
	// ReadFile reads a file. Nil means os.ReadFile. It exists so the
	// WSL kernel probe can be steered in tests.
	//
	// @decision(DL-025)
	ReadFile func(name string) ([]byte, error)
	// OpenBrowser points a browser at a URL. Nil means the real opener.
	//
	// @decision(DL-025)
	OpenBrowser func(ctx context.Context, url string)
	// WaitForInterrupt blocks until the user interrupts a --web session.
	// Nil waits on SIGINT and SIGTERM.
	//
	// @decision(DL-025)
	WaitForInterrupt func(ctx context.Context) error
	// StartClientProxy brings up the loopback proxy fronting a session,
	// minting through the server. Nil means the real one.
	//
	// @decision(DL-030)
	StartClientProxy func(ctx context.Context, c session.Connector, warn func(string)) (*proxy.Proxy, error)
	// NoLaunchView disables the drawn launch view even on a terminal.
	//
	// @decision(DL-024)
	NoLaunchView bool
	// activeView is the launch view currently drawing, if any.
	activeView *launchView
	// warnings are messages produced while the launch view was up. They
	// are held until the terminal is the user's again, since printing
	// under the alternate screen paints them onto a discarded frame.
	//
	// @decision(DL-024)
	warnings []string
	// HealthTimeout bounds the check that the OpenCode server answers
	// through the proxy. Zero uses ProxyHealthTimeout.
	HealthTimeout time.Duration
	// ProxyBinary returns the credential proxy binary `spov serve` bakes
	// into the sidecar's image, or nil without one. Nil uses the one this
	// binary embeds.
	//
	// @decision(DL-034)
	ProxyBinary func() []byte
}

// proxyBinary is the credential proxy this server launches with.
func (e *Env) proxyBinary() []byte {
	if e.ProxyBinary != nil {
		return e.ProxyBinary()
	}
	return proxybin.Binary()
}

// ProxyHealthTimeout bounds the client's own health check. The server
// already waited for the sandbox to be healthy; this only proves that
// the path through the loopback proxy works, so it is short.
//
// @decision(DL-002) @decision(DL-030)
const ProxyHealthTimeout = 30 * time.Second

// provider returns the sandbox provider, building it on first use. Only
// `spov serve` calls it.
func (e *Env) provider(ctx context.Context) (sandbox.Provider, error) {
	if e.Provider != nil {
		return e.Provider, nil
	}
	cfg, err := e.config()
	if err != nil {
		return nil, err
	}
	newProvider := e.NewProvider
	if newProvider == nil {
		newProvider = e.defaultProvider
	}
	p, err := newProvider(ctx, cfg.Config)
	if err != nil {
		return nil, err
	}
	e.Provider = p
	return p, nil
}

// defaultProvider connects to Modal using the configured app and
// environment.
//
// @decision(DL-029)
func (e *Env) defaultProvider(ctx context.Context, cfg config.Config) (sandbox.Provider, error) {
	return sandbox.NewModalProvider(ctx, sandbox.ModalConfig{
		AppName:     cfg.Modal.App,
		Environment: cfg.Modal.Environment,
		Credentials: e.modalCredentials(),
		// @decision(DL-084)
		Warn: func(msg string) { _, _ = fmt.Fprintln(e.Err, "spov serve: "+msg) },
	})
}

// ModalTokenEnvVars name the Modal credential in the environment. A
// hosted server holds its own token; a laptop usually has ~/.modal.toml
// instead, which the SDK reads when these are unset.
//
// @decision(DL-010)
const (
	ModalTokenIDEnvVar     = "MODAL_TOKEN_ID"
	ModalTokenSecretEnvVar = "MODAL_TOKEN_SECRET"
	ModalEnvironmentEnvVar = "MODAL_ENVIRONMENT"
)

// modalCredentials reads the Modal token from the environment. Reading
// ambient configuration happens here and never in the provider, so a
// container's identity and a laptop's take the same path.
//
// @decision(DL-010)
func (e *Env) modalCredentials() sandbox.ModalCredentials {
	return sandbox.ModalCredentials{
		TokenID:     e.Environ.Getenv(ModalTokenIDEnvVar),
		TokenSecret: e.Environ.Getenv(ModalTokenSecretEnvVar),
		Environment: e.Environ.Getenv(ModalEnvironmentEnvVar),
	}
}

// ServerEnvVar and ServerTokenEnvVar name the server and its credential
// in the environment.
//
// @decision(DL-002)
const (
	ServerEnvVar      = "SPOV_SERVER"
	ServerTokenEnvVar = "SPOV_SERVER_TOKEN"
)

// client resolves the API client, first match wins: the --server flag,
// SPOV_SERVER, server.url in config — all of which name a server
// someone else runs, authenticated with SPOV_SERVER_TOKEN — and
// otherwise the local server, found through its state file or spawned.
// No token is ever printed.
//
// @decision(DL-002)
func (e *Env) client(ctx context.Context) (*client.Client, error) {
	defer e.lock()()
	return e.clientLocked(ctx)
}

// lock takes the client lock and returns its release. The mutex is
// created here for an Env that did not go through NewRoot; such an Env
// is a test's, used from one goroutine.
func (e *Env) lock() func() {
	if e.clientMu == nil {
		e.clientMu = &sync.Mutex{}
	}
	e.clientMu.Lock()
	return e.clientMu.Unlock
}

func (e *Env) clientLocked(ctx context.Context) (*client.Client, error) {
	if e.Client != nil {
		return e.Client, nil
	}
	url, err := e.configuredServerURL()
	if err != nil {
		return nil, err
	}
	var c *client.Client
	if url == "" {
		if c, err = e.localClient(ctx); err != nil {
			return nil, err
		}
	} else {
		if err := config.ValidServerURL(url); err != nil {
			return nil, fmt.Errorf("server URL: %w", err)
		}
		c = e.hostedClient(url)
	}
	c.UserAgent = "spov/" + version.Get().Version
	e.Client = c
	return c, nil
}

// hostedClient addresses a hosted server. The environment's token wins,
// so a CI job can pass one without touching the credential store;
// otherwise the token `spov login` stored for this host.
//
// @decision(DL-006)
func (e *Env) hostedClient(url string) *client.Client {
	token := e.Environ.Getenv(ServerTokenEnvVar)
	if token == "" {
		token = e.storedServerToken(url)
	}
	return client.New(url, token)
}

// reconnect drops the client that stale points at and resolves again.
// A local server exits when idle, so a client that outlives it — a
// session's proxy, a `stop` after a long TUI run — finds or spawns a
// new one rather than failing. Concurrent callers that saw the same
// stale client resolve once between them.
//
// @decision(DL-002)
func (e *Env) reconnect(ctx context.Context, stale *client.Client) (*client.Client, error) {
	defer e.lock()()
	if e.Client == stale {
		e.Client = nil
	}
	return e.clientLocked(ctx)
}

// lostServer reports whether err means no server answered at all — as
// opposed to an answer, or the caller giving up — which is when a local
// server may simply have exited.
func lostServer(ctx context.Context, err error) bool {
	return err != nil && client.StatusOf(err) == 0 && ctx.Err() == nil
}

// config loads and caches the merged configuration.
func (e *Env) config() (config.Result, error) {
	if e.cachedConfig != nil {
		return *e.cachedConfig, nil
	}
	load := func() (config.Result, error) { return config.Load(e.Environ, e.Workdir) }
	// @decision(DL-026)
	if e.ServerConfig {
		load = func() (config.Result, error) { return config.LoadUser(e.Environ) }
	}
	res, err := load()
	if err != nil {
		return config.Result{}, err
	}
	e.cachedConfig = &res
	return res, nil
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// credentialBackend names the store secrets live in, without creating one
// if the caller did not supply it.
//
// @decision(DL-008)
func (e *Env) credentialBackend() string {
	if e.Credentials == nil {
		return ""
	}
	return e.Credentials.Name()
}

// NewRoot builds the spov command tree.
//
// @decision(DL-001) @decision(DL-023)
func NewRoot(env Env) *cobra.Command {
	// Every other field is optional with a sensible default, so a nil
	// environment should behave the same rather than panicking deep in
	// config loading.
	if env.Environ == nil {
		env.Environ = config.OSEnv{}
	}
	e := &env
	e.clientMu = &sync.Mutex{}
	root := &cobra.Command{
		Use:           "spov",
		Short:         "Launch OpenCode sessions in on-demand Modal sandboxes",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Get().Version,
	}
	root.SetOut(env.Out)
	root.SetErr(env.Err)
	root.SetVersionTemplate("{{.Version}}\n")
	// @decision(DL-002)
	root.PersistentFlags().StringVar(&e.ServerURL, "server", "", "spov server URL (default: $SPOV_SERVER, then server.url in config)")

	root.AddCommand(newVersionCmd(e))
	root.AddCommand(newConfigCmd(e))
	root.AddCommand(newFlyCmd(e))
	root.AddCommand(newAttachCmd(e))
	// @decision(DL-090)
	root.AddCommand(newResumeCmd(e))
	root.AddCommand(newLoginCmd(e))
	root.AddCommand(newLogoutCmd(e))
	root.AddCommand(newWhoamiCmd(e))
	// @decision(DL-042)
	root.AddCommand(newCredentialsCmd(e))
	root.AddCommand(newLsCmd(e))
	// Autonomous jobs: `spov jobs create` starts one, and the rest of the
	// group reports on them and acts on them, all over the server API.
	//
	// @decision(DL-045) @decision(DL-023)
	root.AddCommand(newJobsCmd(e))
	root.AddCommand(newStopCmd(e))
	root.AddCommand(newServeCmd(e))
	return root
}

func newVersionCmd(env *Env) *cobra.Command {
	var short, embedded bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the spov version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			info := version.Get()
			if short {
				_, err := fmt.Fprintln(env.Out, info.Version)
				return err
			}
			if _, err := fmt.Fprintln(env.Out, info.String()); err != nil || !embedded {
				return err
			}
			_, err := fmt.Fprint(env.Out, embeddedReport(webui.Built(webui.Dist()), len(proxybin.Binary()) > 0))
			return err
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print only the version number")
	// @decision(DL-079)
	cmd.Flags().BoolVar(&embedded, "embedded", false, "also say whether the web app and the credential proxy are built in")
	return cmd
}

// embeddedReport says whether the binary carries the real web app and
// proxy or their placeholders, one `name: built|placeholder` line each,
// which the release check reads.
//
// @decision(DL-079)
func embeddedReport(web, proxy bool) string {
	state := func(built bool) string {
		if built {
			return "built"
		}
		return "placeholder"
	}
	return "web: " + state(web) + "\nproxy: " + state(proxy) + "\n"
}

// deferWarning holds a message until the launch view is gone. Without a
// view it prints immediately, so nothing is swallowed.
//
// @decision(DL-024)
func (e *Env) deferWarning(msg string) {
	if e.activeView == nil {
		_, _ = fmt.Fprintln(e.Err, msg)
		return
	}
	e.warnings = append(e.warnings, msg)
}

// flushWarnings prints anything held back while the view was up.
//
// @decision(DL-024)
func (e *Env) flushWarnings() {
	for _, msg := range e.warnings {
		_, _ = fmt.Fprintln(e.Err, msg)
	}
	e.warnings = nil
}
