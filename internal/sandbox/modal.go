package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	modal "github.com/modal-labs/modal-client/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// readyTimeout bounds how long Create waits for the readiness probe. The
// phase-0 spike measured 3.2-4.4s from create to a healthy server; this is
// generous enough to absorb a cold image pull.
//
// @decision(DL-029)
const readyTimeout = 3 * time.Minute

// tunnelTimeout bounds the wait for tunnel assignment.
const tunnelTimeout = 60 * time.Second

// ModalProvider runs sandboxes on Modal via the official Go SDK. It is the
// only place in spov that imports the SDK.
//
// @decision(DL-029)
type ModalProvider struct {
	client *modal.Client
	app    *modal.App
	env    string

	mu     sync.Mutex
	images map[string]*modal.Image

	warn func(string)
	// noExitSnapshots is set once Modal refuses the exit-snapshot
	// option, so later creates do not ask again.
	noExitSnapshots atomic.Bool
}

// ModalConfig configures a ModalProvider.
type ModalConfig struct {
	// AppName scopes every sandbox; created if missing.
	AppName string
	// Environment selects the Modal environment; empty uses the profile
	// default.
	Environment string
	// Credentials are the Modal token to use. A zero value falls back
	// to the ambient profile (~/.modal.toml or MODAL_TOKEN_*), which is
	// how local mode works; a hosted server always passes them.
	//
	// @decision(DL-010)
	Credentials ModalCredentials
	// Warn reports something an operator should hear about once, such
	// as Modal refusing exit snapshots. Nil discards it.
	//
	// @decision(DL-084)
	Warn func(string)
}

// ModalCredentials is a Modal API token. It is passed in rather than
// read from the ambient profile, so a container with no config file can
// hold an identity of its own.
//
// @decision(DL-010)
type ModalCredentials struct {
	TokenID     string
	TokenSecret string
	Environment string
}

// Set reports whether both halves of the token are present.
func (c ModalCredentials) Set() bool { return c.TokenID != "" && c.TokenSecret != "" }

// ModalMaxThrottleWait bounds how long the SDK waits out a rate limit
// before returning an error. Unset, it waits indefinitely, which turns a
// throttled workspace into a hung request rather than a failed one.
//
// @decision(DL-010)
const ModalMaxThrottleWait = 30 * time.Second

// NewModalProvider connects to Modal and resolves the app. Credentials
// come from cfg when set, otherwise from the ambient profile.
//
// @decision(DL-029) @decision(DL-010)
func NewModalProvider(ctx context.Context, cfg ModalConfig) (*ModalProvider, error) {
	if cfg.AppName == "" {
		return nil, fmt.Errorf("sandbox: modal provider needs an app name")
	}
	env := cfg.Environment
	if cfg.Credentials.Environment != "" {
		env = cfg.Credentials.Environment
	}
	wait := ModalMaxThrottleWait
	params := &modal.ClientParams{MaxThrottleWait: &wait, Environment: env}
	if cfg.Credentials.Set() {
		params.TokenID = cfg.Credentials.TokenID
		params.TokenSecret = cfg.Credentials.TokenSecret
	}
	client, err := modal.NewClientWithOptions(params)
	if err != nil {
		return nil, fmt.Errorf("sandbox: connect to modal: %w", err)
	}
	cfg.Environment = env
	app, err := client.Apps.FromName(ctx, cfg.AppName, &modal.AppFromNameParams{
		CreateIfMissing: true,
		Environment:     cfg.Environment,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve modal app %q: %w", cfg.AppName, err)
	}
	return &ModalProvider{
		client: client,
		app:    app,
		env:    cfg.Environment,
		images: make(map[string]*modal.Image),
		warn:   cfg.Warn,
	}, nil
}

// Close releases the client's gRPC connection. Sandboxes keep running:
// closing the provider ends this process's link to Modal, not the work.
//
// @decision(DL-010)
func (p *ModalProvider) Close() error {
	if p.client == nil {
		return nil
	}
	p.client.Close()
	return nil
}

// Create starts a sandbox on the Sandbox v2 backend and waits for
// readiness.
//
// @decision(DL-029)
func (p *ModalProvider) Create(ctx context.Context, spec Spec) (Handle, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	image, err := p.bootImage(ctx, spec)
	if err != nil {
		return nil, err
	}
	params, err := p.createParams(ctx, spec)
	if err != nil {
		return nil, err
	}

	sb, err := p.client.Sandboxes.ExperimentalCreate(ctx, p.app, image, params)
	// An experimental option Modal no longer takes must not block a
	// launch: create once more without it, and never ask again.
	//
	// @decision(DL-084)
	if err != nil && params.ExperimentalOptions[exitSnapshotOption] != nil && exitSnapshotRefused(err) {
		if p.noExitSnapshots.CompareAndSwap(false, true) && p.warn != nil {
			p.warn(fmt.Sprintf("Modal refused exit snapshots (%s); sandboxes are created without them until this server restarts, so sessions resume from their transcripts", err))
		}
		delete(params.ExperimentalOptions, exitSnapshotOption)
		sb, err = p.client.Sandboxes.ExperimentalCreate(ctx, p.app, image, params)
	}
	if err != nil {
		var exists modal.AlreadyExistsError
		if errors.As(err, &exists) || status.Code(err) == codes.AlreadyExists {
			return nil, fmt.Errorf("sandbox: create: %w: %w", ErrNameInUse, err)
		}
		// A deleted image's name still resolves, and only the create
		// says it is gone.
		//
		// @decision(DL-016)
		if imageGone(err) {
			p.mu.Lock()
			delete(p.images, spec.Image)
			p.mu.Unlock()
			return nil, fmt.Errorf("sandbox: create: %w", &ImageNotFoundError{Name: spec.Image, Err: err})
		}
		return nil, fmt.Errorf("sandbox: create: %w", err)
	}
	h := &modalHandle{sb: sb, p: p}

	// The sidecar starts before the readiness wait: the entrypoint waits
	// for it, and readiness waits for the entrypoint.
	//
	// @decision(DL-034)
	if spec.Sidecar != nil {
		if err := p.createSidecar(ctx, sb, *spec.Sidecar); err != nil {
			return nil, h.cleanupAfterFailedStart(ctx, "could not start its sidecar", err)
		}
	}

	if spec.ReadyPort != 0 {
		if err := sb.WaitUntilReady(ctx, readyTimeout, nil); err != nil {
			return nil, h.cleanupAfterFailedStart(ctx, "never became ready", err)
		}
	}
	return h, nil
}

// exitSnapshotOption is Modal's experimental create option.
//
// @decision(DL-084)
const exitSnapshotOption = "enable_exit_snapshot"

// exitSnapshotRefused reports whether a create failed because Modal
// does not take the exit-snapshot option.
func exitSnapshotRefused(err error) bool {
	var invalid modal.InvalidError
	if !errors.As(err, &invalid) && status.Code(err) != codes.InvalidArgument {
		return false
	}
	return strings.Contains(err.Error(), exitSnapshotOption)
}

// bootImage is the image a sandbox boots: a snapshot by its ID, else
// the published image by name.
//
// @decision(DL-088)
func (p *ModalProvider) bootImage(ctx context.Context, spec Spec) (*modal.Image, error) {
	if spec.ImageID == "" {
		return p.image(ctx, spec.Image)
	}
	img, err := p.client.Images.FromID(ctx, spec.ImageID, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, &ImageNotFoundError{Name: spec.ImageID, Err: err}
		}
		return nil, fmt.Errorf("sandbox: resolve image %s: %w", spec.ImageID, err)
	}
	return img, nil
}

// ExitSnapshot implements Provider, from the sandbox ID alone, so a
// server that did not create the sandbox can still collect it.
//
// @decision(DL-084) @decision(DL-085)
func (p *ModalProvider) ExitSnapshot(ctx context.Context, sandboxID string, wait time.Duration) (string, error) {
	sb, err := p.client.Sandboxes.FromID(ctx, sandboxID, nil)
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("sandbox %s: %w", sandboxID, ErrNotFound)
		}
		return "", fmt.Errorf("sandbox %s: %w", sandboxID, err)
	}
	img, err := sb.ExperimentalGetExitSnapshot(ctx, &modal.SandboxExperimentalGetExitSnapshotParams{Timeout: &wait})
	if err == nil {
		return img.ImageID, nil
	}
	var (
		invalid  modal.InvalidError
		timeout  modal.TimeoutError
		creation modal.SnapshotCreationError
		missing  modal.NotFoundError
	)
	switch {
	case errors.As(err, &invalid):
		return "", fmt.Errorf("sandbox %s: %w", sandboxID, ErrNoExitSnapshot)
	case errors.As(err, &timeout):
		return "", fmt.Errorf("sandbox %s: %w", sandboxID, ErrSnapshotPending)
	case errors.As(err, &creation):
		return "", fmt.Errorf("sandbox %s: %w: %w", sandboxID, ErrSnapshotFailed, err)
	case errors.As(err, &missing):
		return "", fmt.Errorf("sandbox %s: %w", sandboxID, ErrNotFound)
	}
	return "", fmt.Errorf("sandbox %s: exit snapshot: %w", sandboxID, err)
}

// cleanupAfterFailedStart terminates a sandbox that never became ready and
// returns the error to report. A failure to terminate is escalated into
// the message with the sandbox ID, since it is left running and billing.
//
// @decision(DL-029)
func (h *modalHandle) cleanupAfterFailedStart(ctx context.Context, what string, cause error) error {
	detail := ""
	// v2 sandboxes expose no stdio, but the entrypoint's exit code is
	// available and distinguishes "crashed" from "still starting".
	if code, err := h.sb.Poll(context.WithoutCancel(ctx), nil); err == nil && code != nil {
		detail = fmt.Sprintf(" (entrypoint exited with code %d)", *code)
	}

	termCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if termErr := h.Terminate(termCtx); termErr != nil {
		return fmt.Errorf("sandbox %s %s%s (%w); it could NOT be terminated (%w) and is %w: run `spov stop %s`",
			h.sb.SandboxID, what, detail, cause, termErr, ErrStillRunning, h.sb.SandboxID)
	}
	return fmt.Errorf("sandbox %s%s: %w", what, detail, cause)
}

// createParams translates a Spec into Modal's create parameters.
//
// @decision(DL-031) @decision(DL-029)
func (p *ModalProvider) createParams(ctx context.Context, spec Spec) (*modal.SandboxCreateParams, error) {
	// Clone the caller's maps and slices: the SDK holds them for the
	// lifetime of the request.
	params := &modal.SandboxCreateParams{
		Name:           spec.Name,
		Command:        slices.Clone(spec.Command),
		Env:            maps.Clone(spec.Env),
		EncryptedPorts: slices.Clone(spec.Ports),
		CPU:            spec.CPU,
		MemoryMiB:      spec.MemoryMiB,
		Timeout:        spec.Timeout,
		IdleTimeout:    spec.IdleTimeout,
		Tags:           maps.Clone(spec.Tags),
	}

	// Both allowlists are always sent. Modal refuses to restrict a sandbox
	// created with open (nil) network access, and an empty allowlist
	// blocks everything, so "open" is sent as the allow-all entries.
	//
	// @decision(DL-031)
	params.OutboundCIDRAllowlist = &modal.Allowlist{Entries: slices.Clone(spec.Egress.CIDRs)}
	params.OutboundDomainAllowlist = &modal.Allowlist{Entries: slices.Clone(spec.Egress.Domains)}
	params.InboundCIDRAllowlist = slices.Clone(spec.InboundCIDRs)
	params.CPULimit = spec.CPULimit
	params.MemoryLimitMiB = spec.MemoryLimitMiB
	params.Regions = slices.Clone(spec.Regions)
	params.Cloud = spec.Cloud
	params.Workdir = spec.Workdir

	if spec.ReadyPort != 0 {
		probe, err := modal.NewTCPProbe(spec.ReadyPort, nil)
		if err != nil {
			return nil, fmt.Errorf("sandbox: readiness probe: %w", err)
		}
		params.ReadinessProbe = probe
	}

	// Credentials go through an ephemeral Modal Secret rather than plain
	// Env, so they are redacted in Modal's own control plane and logs.
	// They reach the entrypoint as environment variables either way.
	if len(spec.Secrets) > 0 {
		secret, err := p.client.Secrets.FromMap(ctx, maps.Clone(spec.Secrets), &modal.SecretFromMapParams{
			Environment: p.env,
		})
		if err != nil {
			return nil, fmt.Errorf("sandbox: create secret: %w", err)
		}
		params.Secrets = []*modal.Secret{secret}
	}
	// @decision(DL-084)
	if spec.ExitSnapshot && !p.noExitSnapshots.Load() {
		params.ExperimentalOptions = map[string]any{exitSnapshotOption: true}
	}
	return params, nil
}

// createSidecar starts a sidecar in a sandbox. Its secrets go through an
// ephemeral Modal Secret, as the main container's do, and its egress is
// fixed here: the SDK cannot change a running sidecar's.
//
// @decision(DL-034)
func (p *ModalProvider) createSidecar(ctx context.Context, sb *modal.Sandbox, sc Sidecar) error {
	image, err := p.image(ctx, sc.Image)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSidecarStart, err)
	}
	params := &modal.SidecarCreateParams{
		Command: slices.Clone(sc.Command),
		Env:     maps.Clone(sc.Env),
	}
	if !sc.Egress.Open() {
		params.OutboundCIDRAllowlist = &modal.Allowlist{Entries: slices.Clone(sc.Egress.CIDRs)}
		params.OutboundDomainAllowlist = &modal.Allowlist{Entries: slices.Clone(sc.Egress.Domains)}
	}
	if len(sc.Secrets) > 0 {
		secret, err := p.client.Secrets.FromMap(ctx, maps.Clone(sc.Secrets), &modal.SecretFromMapParams{Environment: p.env})
		if err != nil {
			return fmt.Errorf("sandbox: create sidecar secret: %w", err)
		}
		params.Secrets = []*modal.Secret{secret}
	}
	if _, err := sb.ExperimentalSidecars.Create(ctx, sc.Name, image, params); err != nil {
		return fmt.Errorf("%w: sidecar %q: %w", ErrSidecarStart, sc.Name, err)
	}
	return nil
}

// image resolves a published image by name, caching the lookup.
func (p *ModalProvider) image(ctx context.Context, name string) (*modal.Image, error) {
	p.mu.Lock()
	img, ok := p.images[name]
	p.mu.Unlock()
	if ok {
		return img, nil
	}
	img, err := p.client.Images.FromName(ctx, name, &modal.ImageFromNameParams{
		Environment: p.env,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, &ImageNotFoundError{Name: name, Err: err}
		}
		return nil, fmt.Errorf("sandbox: resolve image %q: %w", name, err)
	}
	p.mu.Lock()
	p.images[name] = img
	p.mu.Unlock()
	return img, nil
}

// Get resolves a sandbox by ID (sb-...) or by name.
//
// @decision(DL-029)
func (p *ModalProvider) Get(ctx context.Context, ref string) (Handle, error) {
	if ref == "" {
		return nil, fmt.Errorf("sandbox: empty reference")
	}
	var (
		sb  *modal.Sandbox
		err error
	)
	byID := isSandboxID(ref)
	if byID {
		sb, err = p.client.Sandboxes.FromID(ctx, ref, nil)
	} else {
		sb, err = p.client.Sandboxes.ExperimentalFromName(ctx, p.app.Name, ref,
			&modal.SandboxExperimentalFromNameParams{Environment: p.env})
	}
	if err != nil {
		return nil, p.resolveErr(ref, byID, err)
	}

	h := &modalHandle{sb: sb, p: p}
	// FromID is a pure constructor: it never contacts Modal, so an
	// unknown ID would otherwise surface as a confusing failure at some
	// later call. Probe once here so callers can rely on ErrNotFound.
	if _, err := h.sb.Poll(ctx, nil); err != nil {
		return nil, p.resolveErr(ref, byID, err)
	}
	return h, nil
}

// resolveErr maps a lookup failure onto ErrNotFound where that is what the
// user means. A malformed sandbox ID comes back as InvalidArgument rather
// than NotFound, but only on the by-ID path: on the by-name path an
// InvalidArgument is a genuine bad argument (a bad environment, say) and
// must not be disguised as a missing sandbox.
//
// @decision(DL-067)
func (p *ModalProvider) resolveErr(ref string, byID bool, err error) error {
	var nf modal.NotFoundError
	missing := errors.As(err, &nf) || status.Code(err) == codes.NotFound
	if missing || (byID && status.Code(err) == codes.InvalidArgument) {
		return fmt.Errorf("sandbox %q: %w", ref, ErrNotFound)
	}
	return fmt.Errorf("sandbox: resolve %q: %w", ref, err)
}

// List returns v2 sandboxes in the app carrying all the given tags. v2
// sandboxes are invisible to the v1 List call.
//
// @decision(DL-029)
func (p *ModalProvider) List(ctx context.Context, tags map[string]string) ([]Info, error) {
	seq, err := p.client.Sandboxes.ExperimentalList(ctx, &modal.SandboxExperimentalListParams{
		AppID: p.app.AppID,
		Tags:  tags,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: list: %w", err)
	}
	var out []Info
	for sb, err := range seq {
		if err != nil {
			return nil, fmt.Errorf("sandbox: list: %w", err)
		}
		info, err := (&modalHandle{sb: sb}).Info(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// modalHandle is a live reference to one Modal sandbox.
type modalHandle struct {
	sb *modal.Sandbox
	// p starts sidecars; nil for a handle the bake made for itself.
	p *ModalProvider
}

func (h *modalHandle) Info(ctx context.Context) (Info, error) {
	info := Info{ID: h.sb.SandboxID, State: StateRunning}

	code, err := h.sb.Poll(ctx, nil)
	if err != nil {
		return Info{}, fmt.Errorf("sandbox: poll %s: %w", h.sb.SandboxID, err)
	}
	if code != nil {
		info.State = StateStopped
		info.ExitCode = code
	}

	tags, err := h.sb.GetTags(ctx, nil)
	if err != nil {
		return Info{}, fmt.Errorf("sandbox: tags for %s: %w", h.sb.SandboxID, err)
	}
	info.Tags = tags
	// Age drives the cost display, so a malformed timestamp must not
	// silently report a long-running sandbox as brand new.
	if created, ok := tags[TagCreatedAt]; ok {
		t, err := time.Parse(time.RFC3339, created)
		if err != nil {
			return Info{}, fmt.Errorf("sandbox %s: unparseable %s tag %q: %w", h.sb.SandboxID, TagCreatedAt, created, err)
		}
		info.CreatedAt = t
	}
	info.Name = tags[TagName]
	return info, nil
}

func (h *modalHandle) Tunnels(ctx context.Context) (map[int]string, error) {
	tunnels, err := h.sb.Tunnels(ctx, tunnelTimeout, nil)
	if err != nil {
		return nil, fmt.Errorf("sandbox: tunnels for %s: %w", h.sb.SandboxID, err)
	}
	out := make(map[int]string, len(tunnels))
	for port, t := range tunnels {
		out[port] = t.URL()
	}
	return out, nil
}

// ConnectToken mints a Modal connect token for a container port. The
// token is bound to this sandbox and port and expires on its own; Modal
// offers no way to revoke one, so terminating the sandbox is the only
// revocation.
//
// @decision(DL-030)
func (h *modalHandle) ConnectToken(ctx context.Context, port int, metadata string) (Connect, error) {
	if len(metadata) > MaxConnectMetadata {
		return Connect{}, fmt.Errorf("sandbox: connect metadata is %d bytes, over the %d limit", len(metadata), MaxConnectMetadata)
	}
	creds, err := h.sb.CreateConnectToken(ctx, &modal.SandboxCreateConnectTokenParams{Port: port, UserMetadata: metadata})
	if err != nil {
		return Connect{}, fmt.Errorf("sandbox: connect token for %s: %w", h.sb.SandboxID, err)
	}
	if creds.URL == "" || creds.Token == "" {
		return Connect{}, fmt.Errorf("sandbox: connect token for %s was empty", h.sb.SandboxID)
	}
	return Connect{URL: creds.URL, Token: creds.Token}, nil
}

// SetEgress narrows a running sandbox's egress.
//
// @decision(DL-031)
func (h *modalHandle) SetEgress(ctx context.Context, policy EgressPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	err := h.sb.UpdateNetworkPolicy(ctx, &modal.SandboxUpdateNetworkPolicyParams{
		OutboundCIDRAllowlist:   &modal.Allowlist{Entries: slices.Clone(policy.CIDRs)},
		OutboundDomainAllowlist: &modal.Allowlist{Entries: slices.Clone(policy.Domains)},
	})
	if err != nil {
		return fmt.Errorf("sandbox: set egress on %s: %w", h.sb.SandboxID, err)
	}
	return nil
}

func (h *modalHandle) Exec(ctx context.Context, command []string) (ExecResult, error) {
	return h.ExecInput(ctx, command, nil)
}

// ExecInput writes stdin and closes it before waiting, so a command that
// reads to the end of its input does not block.
//
// @decision(DL-021)
func (h *modalHandle) ExecInput(ctx context.Context, command []string, stdin []byte) (ExecResult, error) {
	return h.ExecBounded(ctx, command, stdin, MaxExecOutput)
}

// @decision(DL-130)
func (h *modalHandle) ExecBounded(ctx context.Context, command []string, stdin []byte, maxOutput int) (ExecResult, error) {
	ctx, cancel := execContext(ctx)
	defer cancel()
	proc, err := h.sb.Exec(ctx, command, nil)
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: exec: %w", err)
	}
	return runProcess(ctx, proc, stdin, maxOutput)
}

// DefaultExecTimeout bounds an exec whose caller set no deadline. Every
// caller sets its own; this is the net under one that did not.
//
// @decision(DL-135)
const DefaultExecTimeout = 15 * time.Minute

// proxyBinaryWriteTimeout bounds writing the proxy binary into the bake
// sandbox.
//
// @decision(DL-135)
const proxyBinaryWriteTimeout = 5 * time.Minute

// execContext is ctx with a deadline: its own, or DefaultExecTimeout.
//
// @decision(DL-135)
func execContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, DefaultExecTimeout)
}

// SidecarExecInput runs a command in a sidecar, as ExecInput does in
// the main container.
//
// @decision(DL-034) @decision(DL-021)
func (h *modalHandle) SidecarExecInput(ctx context.Context, sidecar string, command []string, stdin []byte) (ExecResult, error) {
	ctx, cancel := execContext(ctx)
	defer cancel()
	c, err := h.sb.ExperimentalSidecars.Get(ctx, sidecar, nil)
	if err != nil {
		if isNotFound(err) {
			return ExecResult{}, fmt.Errorf("sandbox %s: %q: %w", h.sb.SandboxID, sidecar, ErrNoSidecar)
		}
		return ExecResult{}, fmt.Errorf("sandbox: sidecar %q: %w", sidecar, err)
	}
	proc, err := c.Exec(ctx, command, nil)
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: exec in sidecar %q: %w", sidecar, err)
	}
	return runProcess(ctx, proc, stdin, MaxExecOutput)
}

// SidecarRunning looks the sidecar up and polls it.
//
// @decision(DL-034)
func (h *modalHandle) SidecarRunning(ctx context.Context, name string) (bool, error) {
	c, err := h.sb.ExperimentalSidecars.Get(ctx, name, nil)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("sandbox: sidecar %q: %w", name, err)
	}
	code, err := c.Poll(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("sandbox: poll sidecar %q: %w", name, err)
	}
	return code == nil, nil
}

// StartSidecar starts a sidecar as Create does.
//
// @decision(DL-034)
func (h *modalHandle) StartSidecar(ctx context.Context, sc Sidecar) error {
	if h.p == nil {
		return errors.New("sandbox: this handle cannot start a sidecar")
	}
	if err := sc.Validate(); err != nil {
		return err
	}
	return h.p.createSidecar(ctx, h.sb, sc)
}

// AddTags merges tags into the sandbox's own and sets the whole set.
//
// @decision(DL-034)
func (h *modalHandle) AddTags(ctx context.Context, tags map[string]string) error {
	info, err := h.Info(ctx)
	if err != nil {
		return err
	}
	// Setting the merged set over a read that came back short would wipe
	// the tags spov finds its sandboxes by.
	if info.Tags[TagSpov] != "1" {
		return fmt.Errorf("sandbox %s: its tags read back without %s, so none are set", h.sb.SandboxID, TagSpov)
	}
	merged := maps.Clone(info.Tags)
	maps.Copy(merged, tags)
	if err := h.sb.SetTags(ctx, merged, nil); err != nil {
		return fmt.Errorf("sandbox: set tags: %w", err)
	}
	return nil
}

// runProcess feeds stdin to a started process, waits for it and reads
// at most maxOutput bytes of each output stream.
//
// @decision(DL-130)
func runProcess(ctx context.Context, proc *modal.ContainerProcess, stdin []byte, maxOutput int) (ExecResult, error) {
	if len(stdin) > 0 {
		if _, err := proc.Stdin.Write(stdin); err != nil {
			return ExecResult{}, fmt.Errorf("sandbox: exec stdin: %w", err)
		}
	}
	if err := proc.Stdin.Close(); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: exec stdin close: %w", err)
	}
	// A read that stops at its bound leaves the SDK's reader blocked on
	// the rest; closing the stream ends it.
	defer func() {
		_ = proc.Stdout.Close()
		_ = proc.Stderr.Close()
	}()
	code, err := proc.Wait(ctx, nil)
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: exec wait: %w", err)
	}
	stdout, err := readBounded(proc.Stdout, maxOutput)
	if err != nil {
		return ExecResult{}, err
	}
	stderr, err := readBounded(proc.Stderr, maxOutput)
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: code, Stdout: stdout, Stderr: stderr}, nil
}

// Terminate stops the sandbox and waits, so that the caller knows billing
// has stopped before it exits.
//
// @decision(DL-029)
func (h *modalHandle) Terminate(ctx context.Context) error {
	if _, err := h.sb.Terminate(ctx, &modal.SandboxTerminateParams{Wait: true}); err != nil {
		return fmt.Errorf("sandbox: terminate %s: %w", h.sb.SandboxID, err)
	}
	return nil
}

func (h *modalHandle) Detach() error {
	if err := h.sb.Detach(); err != nil {
		return fmt.Errorf("sandbox: detach %s: %w", h.sb.SandboxID, err)
	}
	return nil
}

// imageGone reports whether a create failed because its image no longer
// exists: Modal's NotFound naming an image.
func imageGone(err error) bool {
	var nf modal.NotFoundError
	if !errors.As(err, &nf) && status.Code(err) != codes.NotFound {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "image")
}

func isNotFound(err error) bool {
	var nf modal.NotFoundError
	if errors.As(err, &nf) {
		return true
	}
	switch status.Code(err) {
	case codes.NotFound, codes.InvalidArgument:
		return true
	default:
		return false
	}
}

// Compile-time checks that the Modal implementation satisfies the
// interfaces the rest of spov depends on.
var (
	_ Provider          = (*ModalProvider)(nil)
	_ ProxyImageBuilder = (*ModalProvider)(nil)
	_ Handle            = (*modalHandle)(nil)
)

// ModalProvider deletes pruned snapshots as it deletes images.
//
// @decision(DL-086)
var _ ImageDeleter = (*ModalProvider)(nil)
