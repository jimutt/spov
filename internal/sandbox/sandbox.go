// Package sandbox defines the provider interface spov uses to run OpenCode
// servers, and the Modal implementation of it.
//
// @decision(DL-029)
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// State is the lifecycle state of a sandbox.
type State string

// Modal reports only whether a sandbox has exited, so spov distinguishes
// running from stopped and nothing finer. A sandbox that is still booting
// reports as running.
const (
	StateRunning State = "running"
	StateStopped State = "stopped"
)

// Spec describes a sandbox to create. It is provider-agnostic: nothing in
// it refers to Modal.
type Spec struct {
	// Name is optional and unique within the app.
	Name string
	// Image identifies the pre-published image to boot.
	Image string
	// ImageID, when set, is the image to boot instead, an exit snapshot
	// by its ID. Image then only names what the sandbox is recorded
	// against and is not resolved.
	//
	// @decision(DL-084) @decision(DL-088)
	ImageID string
	// ExitSnapshot asks the provider to keep a snapshot of the main
	// container's filesystem when the sandbox ends, however it ends.
	//
	// @decision(DL-084)
	ExitSnapshot bool
	// Command is the entrypoint, passed without a shell wrapper.
	Command []string
	// Env are non-secret environment variables for the entrypoint.
	Env map[string]string
	// Secrets are credentials for the entrypoint. They arrive as
	// environment variables like Env, but the provider redacts them in its
	// control plane and logs.
	//
	// @decision(DL-036)
	Secrets map[string]string
	// Ports are exposed over TLS-terminated tunnels. spov leaves this
	// empty: a sandbox with no exposed port has no public URL at all,
	// and is reached only through a connect token.
	//
	// @decision(DL-030)
	Ports []int
	// ReadyPort, when non-zero, gates readiness on a TCP probe. The
	// probe runs against the container port, so it does not require the
	// port to be exposed.
	//
	// @decision(DL-030)
	ReadyPort int

	CPU       float64
	MemoryMiB int
	// CPULimit and MemoryLimitMiB are hard ceilings; zero means none.
	// A limit below its request is refused.
	//
	// @decision(DL-029)
	CPULimit       float64
	MemoryLimitMiB int
	Timeout        time.Duration
	IdleTimeout    time.Duration
	// Regions and Cloud constrain where the sandbox runs. Empty lets the
	// provider choose.
	//
	// @decision(DL-029)
	Regions []string
	Cloud   string
	// Workdir is the directory the entrypoint starts in. Empty keeps
	// whatever the entrypoint itself sets.
	//
	// @decision(DL-029)
	Workdir string

	// Tags are attached at creation and are the only filterable
	// discovery mechanism.
	Tags map[string]string

	// Egress is what the sandbox may reach. Required.
	//
	// @decision(DL-031)
	Egress EgressPolicy
	// InboundCIDRs restricts who may reach the sandbox through the
	// provider's edge. Empty allows all.
	//
	// @decision(DL-030)
	InboundCIDRs []string
	// Sidecar, when set, is started beside the main container before
	// Create waits for readiness, so an entrypoint may wait on it.
	//
	// @decision(DL-034)
	Sidecar *Sidecar
}

// Sidecar is a second container in the sandbox, with its own processes,
// filesystem and egress, reachable from the main container by Name.
//
// @decision(DL-034)
type Sidecar struct {
	// Name is the container's name and its host name from main.
	Name string
	// Image is the published image it boots.
	Image   string
	Command []string
	Env     map[string]string
	// Secrets reach the sidecar only, never the main container.
	Secrets map[string]string
	// Egress is what the sidecar may reach. It is fixed at creation.
	Egress EgressPolicy
}

// Info is a provider-agnostic view of one sandbox.
type Info struct {
	ID        string
	Name      string
	State     State
	Tags      map[string]string
	CreatedAt time.Time
	// ExitCode is set once the sandbox has stopped.
	ExitCode *int
}

// CostPerCPUHour and CostPerGiBHour are Modal's approximate sandbox
// price in USD, for showing cost rather than billing it. Modal bills
// max(requested, actual), so an estimate from the request is a floor.
//
// @decision(DL-029) @decision(DL-009)
const (
	CostPerCPUHour = 0.135
	CostPerGiBHour = 0.024
)

// HourlyCost is the estimated price of a sandbox of this size, per hour.
//
// @decision(DL-029)
func HourlyCost(cpu float64, memoryMiB int) float64 {
	return cpu*CostPerCPUHour + float64(memoryMiB)/1024*CostPerGiBHour
}

// EstimatedCost is the estimated price of running a sandbox of this
// size for d. A negative duration counts as none.
//
// @decision(DL-009) @decision(DL-029)
func EstimatedCost(cpu float64, memoryMiB int, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return HourlyCost(cpu, memoryMiB) * d.Hours()
}

// Age reports how long the sandbox has existed, for cost reporting.
//
// @decision(DL-029)
func (i Info) Age(now time.Time) time.Duration {
	if i.CreatedAt.IsZero() {
		return 0
	}
	return now.Sub(i.CreatedAt)
}

// ExecResult is the outcome of a one-off command inside a sandbox.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Provider runs sandboxes. Implementations must not leak provider SDK
// types across this interface.
//
// @decision(DL-029)
type Provider interface {
	// Create starts a sandbox and waits for it to become ready.
	Create(ctx context.Context, spec Spec) (Handle, error)
	// Get resolves an existing sandbox by ID or name.
	Get(ctx context.Context, ref string) (Handle, error)
	// List returns sandboxes matching every tag in tags.
	List(ctx context.Context, tags map[string]string) ([]Info, error)
	// ExitSnapshot is the image ID of the exit snapshot of the sandbox
	// with this ID, waiting at most wait for it. A sandbox created
	// without one answers ErrNoExitSnapshot, one still being taken
	// ErrSnapshotPending, one whose snapshot will never exist
	// ErrSnapshotFailed, and one the provider no longer knows
	// ErrNotFound.
	//
	// @decision(DL-084)
	ExitSnapshot(ctx context.Context, sandboxID string, wait time.Duration) (string, error)
}

// The outcomes of ExitSnapshot that are not an image.
//
// @decision(DL-084)
var (
	ErrNoExitSnapshot  = errors.New("sandbox: created without an exit snapshot")
	ErrSnapshotPending = errors.New("sandbox: the exit snapshot is still being taken")
	ErrSnapshotFailed  = errors.New("sandbox: the exit snapshot was not taken")
)

// ImageBuilder is a provider that can build and publish the base image.
// It is kept out of Provider because a launch never builds an image; the
// one caller type-asserts for it.
//
// @decision(DL-032)
type ImageBuilder interface {
	EnsureImage(ctx context.Context, spec ImageSpec) (string, error)
	// HasImage reports whether name is published. Only a genuine miss is
	// false; a failed lookup is an error.
	//
	// @decision(DL-016)
	HasImage(ctx context.Context, name string) (bool, error)
	// BuildImage builds spec and publishes it under its name without
	// looking the name up first, for a name whose image was deleted:
	// Modal still resolves a deleted image's name.
	//
	// @decision(DL-016)
	BuildImage(ctx context.Context, spec ImageSpec) (string, error)
	// ImageID is the ID of the image name is published as.
	//
	// @decision(DL-016)
	ImageID(ctx context.Context, name string) (string, error)
	// DeleteImage deletes the image with this ID. A sandbox already
	// running from it keeps running.
	//
	// @decision(DL-016)
	DeleteImage(ctx context.Context, id string) error
}

// ImageDeleter deletes an image by its ID, an exit snapshot included.
// ImageBuilder has the same method; this is the part pruning snapshots
// needs.
//
// @decision(DL-086)
type ImageDeleter interface {
	DeleteImage(ctx context.Context, id string) error
}

// ErrImageNotFound wraps a launch's failure to resolve an image nobody
// has published. The error itself is an *ImageNotFoundError naming the
// image, since a launch resolves the main image and the sidecar's.
//
// @decision(DL-016)
var ErrImageNotFound = errors.New("image not published")

// ImageNotFoundError is a lookup of a name nobody has published.
//
// @decision(DL-016)
type ImageNotFoundError struct {
	Name string
	Err  error
}

func (e *ImageNotFoundError) Error() string {
	return fmt.Sprintf("sandbox: resolve image %q: %v: %v", e.Name, ErrImageNotFound, e.Err)
}

func (e *ImageNotFoundError) Unwrap() []error { return []error{ErrImageNotFound, e.Err} }

// ProxyImageBuilder bakes the sidecar proxy's binary into an image and
// returns its published name. The name is derived from the binary, so a
// bake happens once per proxy version.
//
// @decision(DL-034)
type ProxyImageBuilder interface {
	EnsureProxyImage(ctx context.Context, binary []byte) (string, error)
	// RebakeProxyImage bakes and publishes the image again under the same
	// name, whether or not the name resolves: a deleted image's name can.
	//
	// @decision(DL-034)
	RebakeProxyImage(ctx context.Context, binary []byte) (string, error)
}

// Handle is a live reference to one sandbox.
//
// @decision(DL-029)
type Handle interface {
	Info(ctx context.Context) (Info, error)
	// Tunnels maps exposed container ports to their public URLs.
	Tunnels(ctx context.Context) (map[int]string, error)
	// ConnectToken mints a credential for reaching a container port
	// through the provider's edge. Metadata reaches the container as a
	// verified header and is length-capped by the provider.
	//
	// @decision(DL-030)
	ConnectToken(ctx context.Context, port int, metadata string) (Connect, error)
	// SetEgress narrows what a running sandbox may reach. Only a policy
	// declared at creation can be changed later, which is why one is
	// always sent.
	//
	// @decision(DL-031)
	SetEgress(ctx context.Context, policy EgressPolicy) error
	Exec(ctx context.Context, command []string) (ExecResult, error)
	// ExecInput is Exec with stdin: a secret passed this way appears in
	// no argv inside the sandbox and in no provider log of the command.
	//
	// @decision(DL-021)
	ExecInput(ctx context.Context, command []string, stdin []byte) (ExecResult, error)
	// ExecBounded is ExecInput reading at most maxOutput bytes of each
	// output stream; over it the answer is ErrOutputTooLarge. Exec and
	// ExecInput read MaxExecOutput.
	//
	// @decision(DL-130)
	ExecBounded(ctx context.Context, command []string, stdin []byte, maxOutput int) (ExecResult, error)
	// SidecarExecInput is ExecInput in the named sidecar. A sandbox
	// without that sidecar answers ErrNoSidecar.
	//
	// @decision(DL-034) @decision(DL-021)
	SidecarExecInput(ctx context.Context, sidecar string, command []string, stdin []byte) (ExecResult, error)
	// SidecarRunning reports whether the named sidecar is running. An
	// exited sidecar is not found (spike S-I4), and reads as not running.
	// Neither lookup resets the sandbox's idle clock (spike S-I3).
	//
	// @decision(DL-034)
	SidecarRunning(ctx context.Context, name string) (bool, error)
	// StartSidecar starts a sidecar in the running sandbox, as Create
	// does. An exited one's name can be used again (spike S-I4).
	//
	// @decision(DL-034)
	StartSidecar(ctx context.Context, sc Sidecar) error
	// AddTags sets tags and keeps the rest. Modal's SetTags replaces every
	// tag (spike S-I4), so this sends the merged set.
	//
	// @decision(DL-034)
	AddTags(ctx context.Context, tags map[string]string) error
	// Terminate stops the sandbox and waits for it to stop.
	Terminate(ctx context.Context) error
	// Detach releases local resources without stopping the sandbox.
	Detach() error
}

// Connect is a credential for reaching one port of one sandbox through
// the provider's authenticated edge. The token is a bearer credential
// with a limited life; it is never logged or printed.
//
// @decision(DL-030)
type Connect struct {
	// URL is the edge endpoint for the port the token was minted for.
	URL string
	// Token authenticates to the edge.
	Token string
}

// MaxConnectMetadata is the longest metadata a token may carry. Modal
// rejects more than 512 bytes serialised.
//
// @decision(DL-030)
const MaxConnectMetadata = 512

// ErrNotFound is returned by Get when no sandbox matches the reference.
var ErrNotFound = errors.New("sandbox: not found")

// ErrNoSidecar is returned for a sidecar the sandbox does not have.
var ErrNoSidecar = errors.New("sandbox: no such sidecar")

// ErrSidecarStart is in the error of a Create whose sidecar could not be
// resolved or started, which a broken image is one cause of.
//
// @decision(DL-034)
var ErrSidecarStart = errors.New("sandbox: the sidecar did not start")

// ErrNameInUse is in the error of a Create whose name another sandbox
// holds. A terminated sandbox holds its name for a few seconds after
// Terminate returns (spike S-I4).
//
// @decision(DL-034)
var ErrNameInUse = errors.New("sandbox: the name is in use")

// ErrStillRunning is in the error of a Create that failed and could not
// terminate what it created, so the sandbox is still billing.
var ErrStillRunning = errors.New("sandbox: still running")
