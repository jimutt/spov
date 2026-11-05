package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/store"
)

// newFlyCmd launches a session: ask the server for one, follow its
// launch, connect through the loopback proxy, hand the terminal to the
// TUI, and tear the session down when the TUI exits.
//
// @decision(DL-023) @decision(DL-025) @decision(DL-024)
func newFlyCmd(env *Env) *cobra.Command {
	var (
		keep                 bool
		name                 string
		allowVersionMismatch bool
		noAttach             bool
		web                  bool
		repos                []string
		gitName              string
		gitEmail             string
		model                string
		gitAccess            string
		sync                 bool
		noSync               bool
	)
	cmd := &cobra.Command{
		Use:   "fly",
		Short: "Launch an OpenCode session in a new sandbox",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFly(cmd.Context(), env, flyOptions{
				Keep:                 keep,
				Name:                 name,
				Model:                model,
				AllowVersionMismatch: allowVersionMismatch,
				NoAttach:             noAttach,
				Web:                  web,
				Repos:                repos,
				Identity:             git.Identity{Name: gitName, Email: gitEmail},
				GitAccess:            gitAccess,
				Sync:                 sync,
				NoSync:               noSync,
			})
		},
	}
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the sandbox running when the TUI exits")
	cmd.Flags().StringVar(&name, "name", "", "session name (defaults to a generated one)")
	cmd.Flags().BoolVar(&allowVersionMismatch, "allow-version-mismatch", false,
		"attach even if the local and sandbox OpenCode versions differ")
	cmd.Flags().BoolVar(&noAttach, "no-attach", false, "launch the sandbox and print how to attach, without starting the TUI")
	cmd.Flags().BoolVar(&web, "web", false, "open the OpenCode web UI in a browser instead of the terminal TUI")
	cmd.MarkFlagsMutuallyExclusive("web", "no-attach")
	cmd.Flags().StringArrayVar(&repos, "repo", nil,
		"repository to bootstrap as owner/name, or platform:owner/name (repeatable); overrides the configured list")
	cmd.Flags().StringVar(&gitName, "git-name", "", "name for commits made in the sandbox (defaults to your git config)")
	cmd.Flags().StringVar(&gitEmail, "git-email", "", "email for commits made in the sandbox (defaults to your git config)")
	// @decision(DL-014)
	cmd.Flags().StringVar(&model, "model", "",
		"model to start on as provider/model, e.g. anthropic/claude-sonnet-5; changeable in the TUI")
	// @decision(DL-043)
	cmd.Flags().StringVar(&gitAccess, "git-access", "",
		"how far the sandbox's git credentials reach: repos (this session's) or all (every repository they can);"+
			" sandbox.git_access, then your account default, when unset")
	// @decision(DL-027)
	cmd.Flags().BoolVar(&sync, "sync", false,
		"seed the sandbox from the current clean local git branch and sync repository changes back when the session closes")
	cmd.Flags().BoolVar(&noSync, "no-sync", false,
		"do not seed from or sync back to the local git checkout, overriding sandbox.sync in config")
	cmd.MarkFlagsMutuallyExclusive("sync", "no-sync")
	cmd.Flags().BoolVar(&env.NoLaunchView, "plain", false,
		"report progress as plain lines instead of the full-screen launch view")
	return cmd
}

type flyOptions struct {
	Keep                 bool
	Name                 string
	AllowVersionMismatch bool
	NoAttach             bool
	// Web opens the browser instead of the TUI.
	//
	// @decision(DL-025)
	Web bool
	// Repos are --repo values, which take precedence over config.
	Repos []string
	// Identity overrides the git identity for this launch. Empty fields
	// fall back to config and then to the local git config.
	//
	// @decision(DL-044)
	Identity git.Identity
	// Model is OpenCode's own provider/model identifier. Empty lets the
	// server resolve it — the caller's stored default, then whatever
	// OpenCode resolves — and the person can change it in the TUI,
	// which is why an interactive session gets a default rather than a
	// pinned model.
	//
	// @decision(DL-014)
	Model string
	// GitAccess is --git-access; empty falls back to sandbox.git_access.
	//
	// @decision(DL-043)
	GitAccess string
	// Sync and NoSync override sandbox.sync for this launch.
	//
	// @decision(DL-027)
	Sync   bool
	NoSync bool
}

// runFly is the whole launch flow, kept separate from cobra so it can be
// tested end to end against an in-process server.
//
// @decision(DL-023) @decision(DL-025) @decision(DL-024)
func runFly(ctx context.Context, env *Env, opts flyOptions) error {
	res, err := env.config()
	if err != nil {
		return err
	}
	cfg := res.Config

	// @decision(DL-027)
	syncMode := opts.Sync || (cfg.Sandbox.Sync && !opts.NoSync)
	var (
		refs    []server.RepoRef
		syncReq *server.SessionSyncRequest
	)
	if syncMode {
		// Validate the local git checkout before spawning or contacting
		// the server so a dirty or non-git directory fails immediately.
		if _, err := git.InspectLocalCheckout(ctx, env.Workdir, platformKindsByHost(cfg), false); err != nil {
			return err
		}
	}

	c, err := env.client(ctx)
	if err != nil {
		return err
	}
	if syncMode {
		ref, sreq, _, err := env.resolveSyncLaunch(ctx, c, cfg, opts.Repos)
		if err != nil {
			return err
		}
		refs = []server.RepoRef{ref}
		syncReq = sreq
	} else {
		refs, err = env.resolveRepoRefs(ctx, c, cfg, opts.Repos)
		if err != nil {
			return err
		}
	}

	name := opts.Name
	if name == "" {
		name = generateName(env.now())
	}

	// The drawn view replaces the plain progress lines only on a
	// terminal. Its teardown is deferred rather than called at the end
	// of the happy path: the alternate screen is a global side effect,
	// so a failure or a Ctrl-C must restore the terminal too.
	//
	// @decision(DL-024)
	if view := env.launchView(); view != nil {
		view.Open()
		defer view.Close()
		defer func() { env.activeView = nil }()
		env.activeView = view
	}

	identity := env.gitIdentity(ctx, cfg, opts.Identity)
	if warning := identityWarning(identity); warning != "" {
		env.deferWarning(warning)
	}

	req := sessionRequest(cfg, opts, name, refs, identity)
	req.Sync = syncReq
	if syncMode && len(refs) == 1 {
		repoDir := git.SingleRepoDir(refs[0].Owner, refs[0].Name)
		if req.Landing == "" || req.Landing != repoDir {
			req.Landing = repoDir
		}
	}
	view, err := env.launchSession(ctx, c, req)
	if err != nil {
		return err
	}

	sess, err := env.connectSession(ctx, c, view)
	if err != nil {
		// The sandbox is up and billing; the failure is local. The
		// server already proved the sandbox healthy, so it is left
		// running for `spov attach` to retry rather than terminated on
		// this machine's say-so.
		closeView(env)
		printLaunchedSummary(env, view, "")
		printAttachHintMode(env, name, syncMode)
		return err
	}

	if opts.NoAttach {
		closeView(env)
		printSessionSummary(env, sess)
		printAttachHintMode(env, name, syncMode)
		detach(env, sess)
		return nil
	}

	// The web UI is served by the sandbox, so it needs no local binary
	// and skips that half of the gate. The server's own version is still
	// checked: a sandbox running a different build is worth refusing
	// whichever client is about to talk to it.
	//
	// @decision(DL-025) @decision(DL-035)
	if opts.Web {
		expected, source := expectedOpenCodeVersion(ctx, env, cfg)
		if err := checkServerVersion(env, expected, source, sess, opts.AllowVersionMismatch); err != nil {
			closeView(env)
			printSessionSummary(env, sess)
			printAttachHintMode(env, name, syncMode)
			detach(env, sess)
			return err
		}
		closeView(env)
		printSessionSummary(env, sess)
		if err := runWebSession(ctx, env, sess.URL); err != nil {
			printAttachHintMode(env, name, syncMode)
			detach(env, sess)
			return err
		}
		if opts.Keep {
			printAttachHintMode(env, name, syncMode)
			detach(env, sess)
			return nil
		}
		if syncMode {
			if err := env.syncSessionBack(ctx, c, cfg, sess.View); err != nil {
				printSyncFailureHint(env, name)
				detach(env, sess)
				return err
			}
		}
		return terminateSession(ctx, env, c, sess)
	}

	// Prepared while the view is still up: finding the client and
	// checking the version gates is dead time the user would otherwise
	// spend looking at a bare terminal between the view and OpenCode.
	//
	// @decision(DL-024)
	binary, err := prepareTUI(ctx, env, cfg, sess, opts)
	if err != nil {
		closeView(env)
		printSessionSummary(env, sess)
		printAttachHintMode(env, name, syncMode)
		detach(env, sess)
		return err
	}

	// The view is closed immediately before the handover, so the
	// alternate screen gives way to OpenCode's own rather than to the
	// user's shell (DL-025 step 3 still holds: spov's view is gone
	// before opencode is spawned).
	closeView(env)
	if err := handOver(ctx, env, sess, binary, ""); err != nil {
		printSessionSummary(env, sess)
		printAttachHintMode(env, name, syncMode)
		detach(env, sess)
		return err
	}

	// Printed after the TUI exits rather than before it starts: this is
	// when the attach and down commands are useful, and printing them
	// beforehand is what put a bare terminal between the two screens.
	//
	// @decision(DL-024)
	printSessionSummary(env, sess)

	if opts.Keep {
		printAttachHintMode(env, name, syncMode)
		detach(env, sess)
		return nil
	}

	// @decision(DL-027)
	if syncMode {
		if err := env.syncSessionBack(ctx, c, cfg, sess.View); err != nil {
			printSyncFailureHint(env, name)
			detach(env, sess)
			return err
		}
	}

	return terminateSession(ctx, env, c, sess)
}

// sessionRequest is the API request for a launch: what the flags and
// config ask for, and nothing the server defaults better. Only an
// explicitly configured image is sent, so a hosted server's own image
// default is not overridden by the client's.
//
// @decision(DL-002) @decision(DL-029) @decision(DL-031)
func sessionRequest(cfg config.Config, opts flyOptions, name string, refs []server.RepoRef, identity git.Identity) server.SessionRequest {
	sb := cfg.Sandbox
	return server.SessionRequest{
		Name:    name,
		Repos:   refs,
		Landing: landingFor(cfg, opts),
		Image:   sb.Image,
		// @decision(DL-033)
		Addons: sb.Addons,
		Apt:    sb.Apt,
		Egress: sb.Egress,
		Sandbox: server.SandboxConfigView{
			CPU: sb.CPU, CPULimit: sb.CPULimit, MemoryMiB: sb.MemoryMiB, MemoryLimitMiB: sb.MemoryLimitMiB,
			TimeoutSecs: int(sb.Timeout / time.Second), IdleSecs: int(sb.IdleTimeout / time.Second),
			Regions: sb.Regions, Cloud: sb.Cloud, Workdir: sb.Workdir, InboundCIDRs: sb.InboundCIDRs,
		},
		Git:   server.GitIdentityView{Name: identity.Name, Email: identity.Email},
		Model: opts.Model,
		// @decision(DL-043)
		GitAccess: store.GitAccess(firstNonEmpty(opts.GitAccess, sb.GitAccess)), GitAccessPreferred: opts.GitAccess == "" && sb.GitAccess != "",
		// @decision(DL-042)
		CredentialPreference: credentialPreference(cfg),
	}
}

// credentialPreference is the config's per-platform credential
// preference as a request sends it: only the platforms that prefer the
// organisation's, since personal-first is the server's default.
//
// @decision(DL-042)
func credentialPreference(cfg config.Config) map[string]string {
	var out map[string]string
	for _, p := range cfg.Platforms {
		if p.Prefer == config.PreferShared {
			if out == nil {
				out = map[string]string{}
			}
			out[p.Name] = string(server.TierOrg)
		}
	}
	return out
}

// firstNonEmpty is the first of its arguments that is not empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// attachTUI gates on the version and then hands over the terminal.
//
// @decision(DL-025)
func attachTUI(ctx context.Context, env *Env, cfg config.Config, sess *liveSession, conversation string, opts flyOptions) error {
	binary, err := prepareTUI(ctx, env, cfg, sess, opts)
	if err != nil {
		return err
	}
	return handOver(ctx, env, sess, binary, conversation)
}

// prepareTUI does everything the handover needs that can fail: finding
// the local client and checking both version gates. It is separated so
// it can run while the launch view is still up, since a process spawn
// and a comparison are dead time the user would otherwise watch on a
// bare terminal.
//
// Its warnings go to the caller rather than being printed here: writing
// to the terminal under the alternate screen would paint them onto a
// frame that is about to be discarded.
//
// @decision(DL-024) @decision(DL-025)
func prepareTUI(ctx context.Context, env *Env, cfg config.Config, sess *liveSession, opts flyOptions) (string, error) {
	// Resolved once and used for both gates.
	//
	// @decision(DL-035)
	expected, source := expectedOpenCodeVersion(ctx, env, cfg)
	client, err := env.locateBinary(ctx, expected)
	if err != nil {
		var mismatch opencode.ClientVersionMismatchError
		switch {
		case !asVersionMismatch(err, &mismatch) || mismatch.Unbridgeable:
			return "", err
		case !opts.AllowVersionMismatch:
			return "", fmt.Errorf("%w\nre-run with --allow-version-mismatch to use it anyway", err)
		}
		env.deferWarning(fmt.Sprintf("spov: %v (continuing because --allow-version-mismatch was given)", err))
	} else if note := driftNote(client, expected, source); note != "" {
		env.deferWarning(note)
	}

	if err := checkServerVersion(env, expected, source, sess, opts.AllowVersionMismatch); err != nil {
		return "", err
	}
	return client.Path, nil
}

// driftNote says how a usable local client differs from the pin: a
// friendly note for another patch, a warning for another minor release.
// source names where the pin came from, as expectedOpenCodeVersion words
// it.
//
// @decision(DL-035)
func driftNote(client opencode.Located, expected, source string) string {
	switch client.Drift {
	case opencode.PatchDrift:
		return fmt.Sprintf("Using your local %s %s; %s %s.", opencode.BinaryName, client.Version, source, expected)
	case opencode.MinorDrift:
		return fmt.Sprintf("spov: warning: your local %s is %s, and %s %s;"+
			" a client from another minor release may not match the server.", opencode.BinaryName, client.Version, source, expected)
	}
	return ""
}

// expectedOpenCodeVersion resolves which build this session's sandbox
// should be running, and names where that number came from. A pin that
// cannot be read is not an error: an unreachable endpoint, an older
// server that has none, and a scheme this build cannot read all mean
// the old behaviour.
//
// @decision(DL-035)
func expectedOpenCodeVersion(ctx context.Context, env *Env, cfg config.Config) (version, source string) {
	// The client the caller already resolved, never a fresh one: it
	// can spawn a local server, and starting a daemon as a side effect
	// of a version handshake would be a surprising way to pay for it.
	if env.Client != nil {
		// An empty version means the deployment pins nothing, which is
		// an answer and not a pin.
		if pin, err := env.Client.OpenCodePin(ctx); err == nil && pin.OpenCode != "" {
			return pin.OpenCode, "this deployment pins"
		}
	}
	if cfg.OpenCode.Version == config.DefaultOpenCodeVersion {
		return cfg.OpenCode.Version, "spov's built-in default is"
	}
	return cfg.OpenCode.Version, "your configuration says"
}

// checkServerVersion gates on the version the sandbox actually reports,
// against what this deployment says it should be.
//
// It needs no local client, so it applies to a browser session as much
// as to the TUI.
//
// @decision(DL-035) @decision(DL-025)
func checkServerVersion(env *Env, expected, source string, sess *liveSession, allowMismatch bool) error {
	err := opencode.CheckVersion(expected, source, sess.Health.Version)
	if err == nil {
		return nil
	}
	if !allowMismatch {
		return fmt.Errorf("%w\nre-run with --allow-version-mismatch to continue anyway", err)
	}
	env.deferWarning(fmt.Sprintf("spov: %v (continuing because --allow-version-mismatch was given)", err))
	return nil
}

// handOver gives the terminal to OpenCode. An empty conversation opens
// the client's home screen, which is what a fresh sandbox wants.
//
// @decision(DL-025)
func handOver(ctx context.Context, env *Env, sess *liveSession, binary, conversation string) error {
	_, err := env.runTUI(ctx, opencode.TUI{
		BinaryPath: binary,
		ServerURL:  sess.URL,
		Session:    conversation,
		Stdin:      env.Stdin,
	})
	return err
}

// locateBinary finds the local opencode client.
func (e *Env) locateBinary(ctx context.Context, version string) (opencode.Located, error) {
	if e.LocateBinary != nil {
		return e.LocateBinary(ctx, version)
	}
	return opencode.Locator{DataDir: e.dataDir(), HomeDir: e.homeDir()}.Locate(ctx, version)
}

// runTUI hands the terminal to the OpenCode client.
func (e *Env) runTUI(ctx context.Context, tui opencode.TUI) (int, error) {
	if e.RunTUI != nil {
		return e.RunTUI(ctx, tui)
	}
	return tui.Run(ctx)
}

func asVersionMismatch(err error, target *opencode.ClientVersionMismatchError) bool {
	return errors.As(err, target)
}

// generateName produces a readable, unique-enough session name.
func generateName(now time.Time) string {
	return "spov-" + now.UTC().Format("20060102-150405")
}

// imageName resolves the base image, falling back to the version-derived
// default. It is the server's default; the client sends only an
// explicitly configured image.
//
// @decision(DL-032)
func imageName(cfg config.Config) string {
	if cfg.Sandbox.Image != "" {
		return cfg.Sandbox.Image
	}
	return sandbox.ImageSpec{OpenCodeVersion: cfg.OpenCode.Version}.Name()
}

// landingFor resolves the directory the session starts in. A configured
// `landing:` names a repo from the configured list, so it cannot apply
// when --repo replaced that list: the session would fail on a directory
// nothing cloned.
//
// @decision(DL-023) @decision(DL-037)
func landingFor(cfg config.Config, opts flyOptions) string {
	if len(opts.Repos) > 0 {
		return ""
	}
	return cfg.Landing
}

// providerEnvVars are the model provider credentials spov forwards from
// its own environment into the sandbox. Without one the server starts
// and is reachable, but cannot talk to a model. It is DL-013's catalogue
// rather than a second list of the same eight.
//
// @decision(DL-036) @decision(DL-013)
var providerEnvVars = prep.ProviderKeyEnvs()

// providerKeys collects the provider credentials present in spov's
// environment. They are injected as environment variables only: a
// credential POSTed to OpenCode's API is stored in SQLite and permanently
// outranks the environment. `spov serve` is what calls this.
//
// @decision(DL-036)
func (e *Env) providerKeys() map[string]string {
	keys := map[string]string{}
	for _, name := range providerEnvVars {
		// The environment wins over the store, so an exported key is
		// always the one used and a stored key only fills a gap.
		//
		// @decision(DL-008)
		if v := e.Environ.Getenv(name); v != "" {
			keys[name] = v
			continue
		}
		if v, err := e.storedProviderKey(name); err == nil && v != "" {
			keys[name] = v
		}
	}
	return keys
}

// storedProviderKey reads a provider key from the credential store.
//
// @decision(DL-008)
func (e *Env) storedProviderKey(name string) (string, error) {
	if e.Credentials == nil {
		return "", fmt.Errorf("no credential store")
	}
	key, err := credentials.ProviderKey(name)
	if err != nil {
		return "", err
	}
	return e.Credentials.Get(key)
}

// homeDir is the user's home directory, used to find an opencode
// installed by the official script.
//
// @decision(DL-035)
func (e *Env) homeDir() string {
	home, err := e.Environ.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func (e *Env) username() string {
	if u := e.Environ.Getenv("USER"); u != "" {
		return u
	}
	return e.Environ.Getenv("USERNAME")
}

// dataDir is where spov keeps its own managed opencode copies.
//
// @decision(DL-035)
func (e *Env) dataDir() string {
	if dir := e.Environ.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir + "/spov"
	}
	home, err := e.Environ.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.local/share/spov"
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// closeView tears down the launch view and releases anything it held
// back, so warnings reach a terminal the user can actually read.
//
// @decision(DL-024)
func closeView(env *Env) {
	if view := env.activeView; view != nil {
		view.Close()
		env.activeView = nil
	}
	env.flushWarnings()
}

// gitIdentity resolves the author for commits made in the sandbox:
// flags, then config, then whatever git reports locally. Halves resolve
// independently, so configuring only an email keeps the detected name.
// It runs on the client, which is the only side that can read the
// user's git configuration; the result travels in the request.
//
// @decision(DL-044)
func (e *Env) gitIdentity(ctx context.Context, cfg config.Config, flags git.Identity) git.Identity {
	explicit := git.ResolveIdentity(flags, func() git.Identity {
		return git.Identity{Name: cfg.Git.Name, Email: cfg.Git.Email}
	})
	if explicit.Complete() {
		return explicit
	}

	local := e.LocalIdentity
	if local == nil {
		local = git.LocalIdentityIn
	}
	// Read from spov's working directory, which is what decides whether
	// an includeIf.gitdir rule applies.
	detected, err := local(ctx, e.Workdir)
	if err != nil {
		e.deferWarning(fmt.Sprintf("spov: warning: could not read your git identity: %v", err))
	}
	return git.ResolveIdentity(explicit, func() git.Identity { return detected })
}

// identityWarning explains what is missing and how to set it. git refuses
// to commit without both halves, and that refusal happens later, inside
// the sandbox, with no hint of where the value should have come from.
//
// @decision(DL-044)
func identityWarning(got git.Identity) string {
	var missing []string
	if got.Name == "" {
		missing = append(missing, "user.name")
	}
	if got.Email == "" {
		missing = append(missing, "user.email")
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"spov: warning: no git %s configured, so commits in the sandbox will fail; "+
			"set it locally with `git config --global %s \"...\"`, "+
			"add git.name/git.email to your spov config, or pass --git-name/--git-email",
		strings.Join(missing, " or "), missing[0])
}

// stopHint names the command that stops a sandbox spov could not reach.
// It is spelled out rather than inlined so every branch says it the
// same way.
//
// @decision(DL-029)
func stopHint(ref string) string {
	return fmt.Sprintf("`spov stop %s` ends it if you do not want it billing", ref)
}

// sessionError adds the context that makes a failed API call actionable.
//
// @decision(DL-025) @decision(DL-029)
func sessionError(ref string, err error) error {
	switch {
	case client.IsNotFound(err):
		return fmt.Errorf("no session %q; run `spov ls` to see what is running", ref)
	case client.IsSessionStopped(err):
		return fmt.Errorf("session %q is no longer running: %w", ref, err)
	default:
		return err
	}
}
