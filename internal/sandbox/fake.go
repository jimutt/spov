package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Provider for tests. It records the specs it was
// given so callers can assert on what spov would have created, and lets a
// test force failures at each step.
//
// @decision(DL-029)
type Fake struct {
	mu sync.Mutex

	// Created records every spec passed to Create, in order.
	Created []Spec

	// CreateErr, when set, is returned by Create instead of a handle.
	CreateErr error
	// ListErr, when set, fails List.
	ListErr error
	// TunnelsErr, TerminateErr, DetachErr and ExecErr fail the
	// corresponding handle method.
	TunnelsErr   error
	ConnectErr   error
	TerminateErr error
	DetachErr    error
	ExecErr      error
	// SetEgressErr fails SetEgress.
	//
	// @decision(DL-031)
	SetEgressErr error
	// ExecResults maps a joined command to its canned result.
	ExecResults map[string]ExecResult
	// Now supplies creation timestamps; defaults to time.Now.
	Now func() time.Time
	// BaseURL is the tunnel host handed out to created sandboxes.
	BaseURL string
	// ConnectBaseURL is the edge host handed out by ConnectToken. Empty
	// falls back to BaseURL.
	ConnectBaseURL string
	// ConnectTokens records every (id, port) a token was minted for.
	ConnectTokens []ConnectRequest
	// ConnectTokenValue, when set, is handed out instead of a generated
	// opaque token, so a test can supply a real JWT shape.
	ConnectTokenValue string
	// SidecarExecErr fails SidecarExecInput; SidecarCheckErr fails
	// SidecarRunning; StartSidecarErr fails StartSidecar.
	SidecarExecErr  error
	SidecarCheckErr error
	StartSidecarErr error
	// ProxyImageErr fails EnsureProxyImage; ProxyBakes records the
	// binaries it was given.
	//
	// @decision(DL-034)
	ProxyImageErr error
	ProxyBakes    [][]byte
	// BrokenProxyImages fail a Create whose sidecar uses one of them,
	// until RebakeProxyImage repairs it; ProxyRebakes counts those.
	//
	// @decision(DL-034)
	BrokenProxyImages map[string]bool
	ProxyRebakes      int
	// NameHeldFor is how many further Creates a sandbox's name is refused
	// for after a failed start, as Modal holds it briefly (spike S-I4).
	NameHeldFor int
	held        map[string]int
	// SnapshotErrs fails ExitSnapshot for a sandbox ID, and
	// RefuseExitSnapshots makes Create answer as Modal does when it no
	// longer takes the option. DeletedImages records DeleteImage.
	//
	// @decision(DL-084)
	SnapshotErrs        map[string]error
	RefuseExitSnapshots bool
	DeletedImages       []string

	byID   map[string]*FakeHandle
	byName map[string]*FakeHandle
	seq    int
}

// ConnectRequest records one ConnectToken call.
type ConnectRequest struct {
	// Metadata is what the caller attached to the token.
	//
	// @decision(DL-030)
	Metadata  string
	SandboxID string
	Port      int
}

// NewFake returns a Fake ready for use.
func NewFake() *Fake {
	return &Fake{
		byID:        make(map[string]*FakeHandle),
		byName:      make(map[string]*FakeHandle),
		ExecResults: make(map[string]ExecResult),
		BaseURL:     "https://sandbox.test",
	}
}

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Create records the spec and returns a running handle.
func (f *Fake) Create(ctx context.Context, spec Spec) (Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Created = append(f.Created, spec)
	if f.CreateErr != nil {
		return nil, f.CreateErr
	}
	if n := f.held[spec.Name]; n > 0 {
		f.held[spec.Name] = n - 1
		return nil, fmt.Errorf("sandbox: name %q: %w", spec.Name, ErrNameInUse)
	}
	if spec.Sidecar != nil && f.BrokenProxyImages[spec.Sidecar.Image] {
		if f.NameHeldFor > 0 {
			if f.held == nil {
				f.held = map[string]int{}
			}
			f.held[spec.Name] = f.NameHeldFor
		}
		return nil, fmt.Errorf("sandbox %s could not start its sidecar: %w", spec.Name, ErrSidecarStart)
	}

	// Modal names are unique within an app, so a duplicate must fail here
	// rather than silently shadowing the earlier sandbox.
	if spec.Name != "" {
		if _, taken := f.byName[spec.Name]; taken {
			return nil, fmt.Errorf("sandbox: name %q: %w", spec.Name, ErrNameInUse)
		}
	}

	f.seq++
	id := fmt.Sprintf("sb-fake-%d", f.seq)
	tags := maps.Clone(spec.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	if _, ok := tags[TagCreatedAt]; !ok {
		tags[TagCreatedAt] = f.now().UTC().Format(time.RFC3339)
	}

	h := &FakeHandle{
		provider:     f,
		exitSnapshot: spec.ExitSnapshot && !f.RefuseExitSnapshots,
		info: Info{
			ID:        id,
			Name:      spec.Name,
			State:     StateRunning,
			Tags:      tags,
			CreatedAt: f.now(),
		},
		tunnels: make(map[int]string, len(spec.Ports)),
	}
	if spec.Sidecar != nil {
		h.sidecar = spec.Sidecar.Name
	}
	for _, port := range spec.Ports {
		h.tunnels[port] = fmt.Sprintf("%s/%s-%d", f.BaseURL, id, port)
	}
	f.byID[id] = h
	if spec.Name != "" {
		f.byName[spec.Name] = h
	}
	return h, nil
}

// Get resolves a handle previously returned by Create.
func (f *Fake) Get(ctx context.Context, ref string) (Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.byID[ref]
	if !ok {
		h, ok = f.byName[ref]
	}
	if !ok {
		return nil, fmt.Errorf("sandbox %q: %w", ref, ErrNotFound)
	}
	return h, nil
}

// List returns handles carrying every tag in tags, newest first.
func (f *Fake) List(ctx context.Context, tags map[string]string) ([]Info, error) {
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []Info
	for _, h := range f.byID {
		if matchesTags(h.info.Tags, tags) {
			out = append(out, h.snapshot())
		}
	}
	slices.SortFunc(out, func(a, b Info) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// Specs returns every spec passed to Create, in order. Created is
// appended to from the launch goroutine, so a test that watches for a
// launch must read it through this rather than off the field.
func (f *Fake) Specs() []Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Spec(nil), f.Created...)
}

// Running counts handles that have not been terminated.
// Handles returns every handle the fake has created, by sandbox ID.
func (f *Fake) Handles() map[string]*FakeHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]*FakeHandle, len(f.byID))
	for id, h := range f.byID {
		out[id] = h
	}
	return out
}

func (f *Fake) Running() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, h := range f.byID {
		if h.info.State == StateRunning {
			n++
		}
	}
	return n
}

func matchesTags(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// FakeHandle is the Handle a Fake hands out. Its recorded state is behind
// accessor methods so that tests can read it while other goroutines are
// still using the handle.
type FakeHandle struct {
	provider *Fake
	info     Info
	tunnels  map[int]string

	terminated   bool
	detached     bool
	exitSnapshot bool
	execs        [][]string
	inputs       [][]byte
	egress       []EgressPolicy

	sidecar      string
	sidecarExecs []SidecarExec
	started      []Sidecar
}

// SidecarExec records one SidecarExecInput call.
type SidecarExec struct {
	Sidecar string
	Command []string
	Stdin   []byte
}

// KillSidecar makes the sandbox's sidecar gone, as an exited one is to
// Modal's lookup.
func (h *FakeHandle) KillSidecar() {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	h.sidecar = ""
}

// SidecarRunning reports whether the named sidecar is up.
//
// @decision(DL-034)
func (h *FakeHandle) SidecarRunning(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if h.provider.SidecarCheckErr != nil {
		return false, h.provider.SidecarCheckErr
	}
	return !h.terminated && name != "" && h.sidecar == name, nil
}

// StartSidecar records the sidecar and brings it up.
//
// @decision(DL-034)
func (h *FakeHandle) StartSidecar(ctx context.Context, sc Sidecar) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := sc.Validate(); err != nil {
		return err
	}
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if err := h.usable(); err != nil {
		return err
	}
	h.started = append(h.started, sc)
	if h.provider.StartSidecarErr != nil {
		return h.provider.StartSidecarErr
	}
	h.sidecar = sc.Name
	return nil
}

// StartedSidecars returns every StartSidecar call, in order.
func (h *FakeHandle) StartedSidecars() []Sidecar {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	return slices.Clone(h.started)
}

// AddTags merges tags into the sandbox's.
//
// @decision(DL-034)
func (h *FakeHandle) AddTags(ctx context.Context, tags map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if h.info.Tags == nil {
		h.info.Tags = map[string]string{}
	}
	maps.Copy(h.info.Tags, tags)
	return nil
}

// ErrExecNoDeadline is what the fake answers an exec whose context has no
// deadline: a program in the sandbox that never exits would hold such a
// call forever, so every caller bounds its own.
//
// @decision(DL-135)
var ErrExecNoDeadline = errors.New("sandbox: exec without a deadline on its context")

func requireDeadline(ctx context.Context, command []string) error {
	if _, ok := ctx.Deadline(); ok {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrExecNoDeadline, strings.Join(command, " "))
}

// DropTag removes a tag, as anything that replaced the tag set would.
func (h *FakeHandle) DropTag(key string) {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	delete(h.info.Tags, key)
}

// SidecarExecs returns every SidecarExecInput call, in order.
func (h *FakeHandle) SidecarExecs() []SidecarExec {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	return slices.Clone(h.sidecarExecs)
}

// SidecarExecInput records the call. A sandbox created without that
// sidecar answers ErrNoSidecar.
//
// @decision(DL-034)
func (h *FakeHandle) SidecarExecInput(ctx context.Context, sidecar string, command []string, stdin []byte) (ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecResult{}, err
	}
	// @decision(DL-135)
	if err := requireDeadline(ctx, command); err != nil {
		return ExecResult{}, err
	}
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if err := h.usable(); err != nil {
		return ExecResult{}, err
	}
	if sidecar != h.sidecar {
		return ExecResult{}, fmt.Errorf("sandbox %s: %q: %w", h.info.ID, sidecar, ErrNoSidecar)
	}
	h.sidecarExecs = append(h.sidecarExecs, SidecarExec{Sidecar: sidecar, Command: slices.Clone(command), Stdin: slices.Clone(stdin)})
	if h.provider.SidecarExecErr != nil {
		return ExecResult{}, h.provider.SidecarExecErr
	}
	return ExecResult{}, nil
}

// EnsureProxyImage records the bake and returns the image's name.
//
// @decision(DL-034)
func (f *Fake) EnsureProxyImage(ctx context.Context, binary []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ProxyBakes = append(f.ProxyBakes, slices.Clone(binary))
	if f.ProxyImageErr != nil {
		return "", f.ProxyImageErr
	}
	return ProxyImageName(binary), nil
}

// RebakeProxyImage records the re-bake and repairs the image.
//
// @decision(DL-034)
func (f *Fake) RebakeProxyImage(ctx context.Context, binary []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ProxyRebakes++
	if f.ProxyImageErr != nil {
		return "", f.ProxyImageErr
	}
	name := ProxyImageName(binary)
	delete(f.BrokenProxyImages, name)
	return name, nil
}

// WasTerminated reports whether Terminate succeeded on this handle.
func (h *FakeHandle) WasTerminated() bool {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	return h.terminated
}

// WasDetached reports whether Detach ran on this handle.
func (h *FakeHandle) WasDetached() bool {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	return h.detached
}

// ExecLog returns the commands passed to Exec, in order.
func (h *FakeHandle) ExecLog() [][]string {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	out := make([][]string, len(h.execs))
	for i, c := range h.execs {
		out[i] = slices.Clone(c)
	}
	return out
}

// errDetached matches the real provider, which rejects every operation on
// a sandbox that has been detached or terminated.
var errDetached = errors.New("sandbox: handle is detached")

// usable reports whether the handle still accepts operations. The caller
// must hold the provider lock.
func (h *FakeHandle) usable() error {
	if h.detached || h.terminated {
		return errDetached
	}
	return nil
}

func (h *FakeHandle) snapshot() Info {
	info := h.info
	info.Tags = maps.Clone(h.info.Tags)
	return info
}

func (h *FakeHandle) Info(context.Context) (Info, error) {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	return h.snapshot(), nil
}

func (h *FakeHandle) Tunnels(context.Context) (map[int]string, error) {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if err := h.usable(); err != nil {
		return nil, err
	}
	if h.provider.TunnelsErr != nil {
		return nil, h.provider.TunnelsErr
	}
	return maps.Clone(h.tunnels), nil
}

// ConnectToken hands out a deterministic credential and records the
// request, so a test can assert what was minted and for which port.
func (h *FakeHandle) ConnectToken(_ context.Context, port int, metadata string) (Connect, error) {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if err := h.usable(); err != nil {
		return Connect{}, err
	}
	if h.provider.ConnectErr != nil {
		return Connect{}, h.provider.ConnectErr
	}
	if len(metadata) > MaxConnectMetadata {
		return Connect{}, fmt.Errorf("sandbox: connect metadata is %d bytes, over the %d limit", len(metadata), MaxConnectMetadata)
	}
	h.provider.ConnectTokens = append(h.provider.ConnectTokens, ConnectRequest{SandboxID: h.info.ID, Port: port, Metadata: metadata})
	base := h.provider.ConnectBaseURL
	if base == "" {
		base = h.provider.BaseURL
	}
	if base == "" {
		base = "https://connect.example"
	}
	token := h.provider.ConnectTokenValue
	if token == "" {
		token = fmt.Sprintf("fake-token-%s-%d-%d", h.info.ID, port, len(h.provider.ConnectTokens))
	}
	return Connect{
		URL:   fmt.Sprintf("%s/connect/%s/%d", strings.TrimSuffix(base, "/"), h.info.ID, port),
		Token: token,
	}, nil
}

// SetEgress records the narrowed policy.
//
// @decision(DL-031)
func (h *FakeHandle) SetEgress(_ context.Context, policy EgressPolicy) error {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if err := h.usable(); err != nil {
		return err
	}
	if h.provider.SetEgressErr != nil {
		return h.provider.SetEgressErr
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	h.egress = append(h.egress, policy)
	return nil
}

// EgressLog is every policy SetEgress was called with, in order.
//
// @decision(DL-031)
func (h *FakeHandle) EgressLog() []EgressPolicy {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	return slices.Clone(h.egress)
}

func (h *FakeHandle) Exec(ctx context.Context, command []string) (ExecResult, error) {
	return h.ExecInput(ctx, command, nil)
}

// ExecInputs returns the standard input each Exec was given, in order,
// beside ExecLog's commands.
func (h *FakeHandle) ExecInputs() [][]byte {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	out := make([][]byte, len(h.inputs))
	for i, in := range h.inputs {
		out[i] = slices.Clone(in)
	}
	return out
}

func (h *FakeHandle) ExecInput(ctx context.Context, command []string, stdin []byte) (ExecResult, error) {
	return h.ExecBounded(ctx, command, stdin, MaxExecOutput)
}

// ExecBounded answers a canned result over maxOutput as a real sandbox
// does.
//
// @decision(DL-130)
func (h *FakeHandle) ExecBounded(ctx context.Context, command []string, stdin []byte, maxOutput int) (ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecResult{}, err
	}
	// @decision(DL-135)
	if err := requireDeadline(ctx, command); err != nil {
		return ExecResult{}, err
	}
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if err := h.usable(); err != nil {
		return ExecResult{}, err
	}
	h.execs = append(h.execs, slices.Clone(command))
	h.inputs = append(h.inputs, slices.Clone(stdin))
	if h.provider.ExecErr != nil {
		return ExecResult{}, h.provider.ExecErr
	}
	if res, ok := h.provider.ExecResults[strings.Join(command, " ")]; ok {
		return BoundOutput(res, maxOutput)
	}
	return ExecResult{}, nil
}

func (h *FakeHandle) Terminate(ctx context.Context) error {
	// A real Terminate on a cancelled context fails, which is exactly the
	// failure cleanup code has to avoid by detaching its context.
	if err := ctx.Err(); err != nil {
		return err
	}
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if h.provider.TerminateErr != nil {
		return h.provider.TerminateErr
	}
	h.terminated = true
	h.info.State = StateStopped
	zero := 0
	h.info.ExitCode = &zero
	return nil
}

func (h *FakeHandle) Detach() error {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if h.provider.DetachErr != nil {
		return h.provider.DetachErr
	}
	h.detached = true
	return nil
}

// ID is the sandbox's ID.
func (h *FakeHandle) ID() string {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	return h.info.ID
}

// End stops the sandbox as its idle or hard timeout would, with no
// Terminate.
func (h *FakeHandle) End() {
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	h.info.State = StateStopped
	code := 0
	h.info.ExitCode = &code
}

// ExitSnapshot implements Provider: a sandbox created with one that has
// stopped has the image "im-snap-<id>".
//
// @decision(DL-084)
func (f *Fake) ExitSnapshot(ctx context.Context, sandboxID string, _ time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.SnapshotErrs[sandboxID]; err != nil {
		return "", err
	}
	h, ok := f.byID[sandboxID]
	switch {
	case !ok:
		return "", fmt.Errorf("sandbox %s: %w", sandboxID, ErrNotFound)
	case !h.exitSnapshot:
		return "", fmt.Errorf("sandbox %s: %w", sandboxID, ErrNoExitSnapshot)
	case h.info.State == StateRunning:
		return "", fmt.Errorf("sandbox %s: %w", sandboxID, ErrSnapshotPending)
	}
	return "im-snap-" + sandboxID, nil
}

// DeleteImage records the deletion, as ImageDeleter.
func (f *Fake) DeleteImage(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeletedImages = append(f.DeletedImages, id)
	return nil
}

// Deleted is a copy of DeletedImages.
func (f *Fake) Deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.DeletedImages)
}

// Reattach makes every detached, unterminated handle usable again, as a
// new server process's Get of a running sandbox returns a live handle
// where the old process's had been detached on its way out.
func (f *Fake) Reattach() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.byID {
		if !h.terminated {
			h.detached = false
		}
	}
}

// Compile-time checks that the fake satisfies the interfaces.
var (
	_ Provider          = (*Fake)(nil)
	_ ProxyImageBuilder = (*Fake)(nil)
	_ ImageDeleter      = (*Fake)(nil)
	_ Handle            = (*FakeHandle)(nil)
)
