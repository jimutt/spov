package sandbox

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// MaxTimeout is the hard ceiling on sandbox lifetime. A sandbox bills for
// its whole wall-clock life, so spov refuses to create one that could
// outlive a working day.
//
// @decision(DL-029)
const MaxTimeout = 12 * time.Hour

// MinIdleTimeout is the shortest idle timeout spov accepts. The client's
// proxy heartbeats at a quarter of it, floored at 10 s (DL-030), so a
// shorter window cannot be kept alive by an attached client.
//
// @decision(DL-030) @decision(DL-029)
const MinIdleTimeout = time.Minute

// ValidWorkdir reports whether a working directory is usable. Empty
// keeps whatever the entrypoint sets.
//
// @decision(DL-029)
func ValidWorkdir(dir string) error {
	if dir != "" && !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("sandbox: workdir %q must be an absolute path", dir)
	}
	return nil
}

// Clouds are the provider clouds a sandbox may be pinned to. Empty lets
// the provider choose.
//
// @decision(DL-029)
var Clouds = []string{"aws", "gcp", "oci", "auto"}

// ValidCloud reports whether a cloud name is one the provider knows.
//
// @decision(DL-029)
func ValidCloud(cloud string) error {
	if cloud == "" || slices.Contains(Clouds, cloud) {
		return nil
	}
	return fmt.Errorf("sandbox: unknown cloud %q (want one of %s)", cloud, strings.Join(Clouds, ", "))
}

// Validate checks a Spec before it reaches the provider, so that a bad
// configuration fails locally rather than after a billable create.
//
// @decision(DL-029)
func (s Spec) Validate() error {
	if s.Image == "" {
		return fmt.Errorf("sandbox: spec needs an image")
	}
	if len(s.Command) == 0 {
		return fmt.Errorf("sandbox: spec needs a command")
	}
	if s.CPU <= 0 {
		return fmt.Errorf("sandbox: cpu must be positive, got %v", s.CPU)
	}
	if s.MemoryMiB <= 0 {
		return fmt.Errorf("sandbox: memory must be positive, got %d MiB", s.MemoryMiB)
	}
	if s.CPULimit != 0 && s.CPULimit < s.CPU {
		return fmt.Errorf("sandbox: cpu limit %v is below the request of %v", s.CPULimit, s.CPU)
	}
	if s.MemoryLimitMiB != 0 && s.MemoryLimitMiB < s.MemoryMiB {
		return fmt.Errorf("sandbox: memory limit %d MiB is below the request of %d MiB", s.MemoryLimitMiB, s.MemoryMiB)
	}
	if err := ValidWorkdir(s.Workdir); err != nil {
		return err
	}
	if err := ValidCloud(s.Cloud); err != nil {
		return err
	}
	// An unset policy would be sent as no allowlist at all, which the
	// provider reads as unrestricted.
	//
	// @decision(DL-031)
	if err := s.Egress.Validate(); err != nil {
		return err
	}
	for _, cidr := range s.InboundCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return fmt.Errorf("sandbox: inbound cidr %q: %w", cidr, err)
		}
	}
	if s.Timeout <= 0 {
		return fmt.Errorf("sandbox: timeout must be positive, got %s", s.Timeout)
	}
	if s.Timeout > MaxTimeout {
		return fmt.Errorf("sandbox: timeout %s exceeds the %s maximum", s.Timeout, MaxTimeout)
	}
	if s.IdleTimeout < MinIdleTimeout {
		return fmt.Errorf("sandbox: idle timeout must be at least %s, got %s", MinIdleTimeout, s.IdleTimeout)
	}
	if s.IdleTimeout > s.Timeout {
		return fmt.Errorf("sandbox: idle timeout %s exceeds timeout %s", s.IdleTimeout, s.Timeout)
	}
	if s.ReadyPort < 0 || s.ReadyPort > 65535 {
		return fmt.Errorf("sandbox: readiness port %d is out of range", s.ReadyPort)
	}
	seenPorts := make(map[int]bool, len(s.Ports))
	for _, p := range s.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("sandbox: port %d is out of range", p)
		}
		if seenPorts[p] {
			return fmt.Errorf("sandbox: port %d is listed twice", p)
		}
		seenPorts[p] = true
	}
	if s.Sidecar != nil {
		if err := s.Sidecar.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks a sidecar before it reaches the provider.
//
// @decision(DL-034)
func (c Sidecar) Validate() error {
	switch {
	case c.Name == "" || c.Name == "main":
		return fmt.Errorf("sandbox: sidecar name %q is empty or reserved", c.Name)
	case c.Image == "":
		return fmt.Errorf("sandbox: sidecar %q needs an image", c.Name)
	case len(c.Command) == 0:
		return fmt.Errorf("sandbox: sidecar %q needs a command", c.Name)
	}
	if err := c.Egress.Validate(); err != nil {
		return fmt.Errorf("sandbox: sidecar %q: %w", c.Name, err)
	}
	return nil
}
