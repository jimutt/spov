package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/toolchain"
)

// Validate checks a merged config for internal consistency. It reports the
// first problem found, phrased so the user can fix the file it came from.
//
// @decision(DL-026) @decision(DL-029)
func Validate(c Config) error {
	if c.Version != SchemaVersion {
		return fmt.Errorf("config: unsupported schema version %d (spov understands %d)", c.Version, SchemaVersion)
	}
	if c.Sandbox.CPU <= 0 {
		return fmt.Errorf("config: sandbox.cpu must be positive, got %v", c.Sandbox.CPU)
	}
	if c.Sandbox.MemoryMiB <= 0 {
		return fmt.Errorf("config: sandbox.memory_mib must be positive, got %d", c.Sandbox.MemoryMiB)
	}
	if c.Sandbox.Timeout <= 0 {
		return fmt.Errorf("config: sandbox.timeout must be positive, got %s", c.Sandbox.Timeout)
	}
	if c.Sandbox.Timeout > MaxSandboxTimeout {
		return fmt.Errorf("config: sandbox.timeout %s exceeds the %s maximum", c.Sandbox.Timeout, MaxSandboxTimeout)
	}
	if c.Sandbox.IdleTimeout < MinIdleTimeout {
		return fmt.Errorf("config: sandbox.idle_timeout must be at least %s, got %s", MinIdleTimeout, c.Sandbox.IdleTimeout)
	}
	if c.Sandbox.CPULimit != 0 && c.Sandbox.CPULimit < c.Sandbox.CPU {
		return fmt.Errorf("config: sandbox.cpu_limit %v is below sandbox.cpu %v", c.Sandbox.CPULimit, c.Sandbox.CPU)
	}
	if c.Sandbox.MemoryLimitMiB != 0 && c.Sandbox.MemoryLimitMiB < c.Sandbox.MemoryMiB {
		return fmt.Errorf("config: sandbox.memory_limit_mib %d is below sandbox.memory_mib %d", c.Sandbox.MemoryLimitMiB, c.Sandbox.MemoryMiB)
	}
	if err := ValidServerURL(c.Server.URL); err != nil {
		return fmt.Errorf("config: server.url: %w", err)
	}
	// @decision(DL-043)
	for _, kv := range [][2]string{{"sandbox.git_access", c.Sandbox.GitAccess}, {"sandbox.job_git_access", c.Sandbox.JobGitAccess}} {
		if v := kv[1]; v != "" && v != "repos" && v != "all" {
			return fmt.Errorf("config: %s must be repos or all, got %q", kv[0], v)
		}
	}
	// @decision(DL-049)
	if d := c.Sandbox.JobMaxDuration; d != 0 && d < time.Minute {
		return fmt.Errorf("config: sandbox.job_max_duration %s is below the 1m minimum", d)
	}
	// @decision(DL-033)
	if _, err := toolchain.Parse(c.Sandbox.Addons); err != nil {
		return fmt.Errorf("config: sandbox.addons: %w", err)
	}
	if err := sandbox.ValidatePackages(c.Sandbox.Apt); err != nil {
		return fmt.Errorf("config: sandbox.apt: %w", err)
	}
	if c.Sandbox.Image != "" && (len(c.Sandbox.Addons) > 0 || len(c.Sandbox.Apt) > 0) {
		return fmt.Errorf("config: sandbox.image names an image spov did not build, so it cannot take sandbox.addons or sandbox.apt; drop one or the other")
	}
	if c.Sandbox.GPU != "" {
		return fmt.Errorf("config: sandbox.gpu is not available: spov uses Modal's v2 sandbox backend, which has no GPU support (DL-029)")
	}
	if err := sandbox.ValidCloud(c.Sandbox.Cloud); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := sandbox.ValidWorkdir(c.Sandbox.Workdir); err != nil {
		return fmt.Errorf("config: sandbox.workdir: %w", err)
	}
	// The preset and its extra domains are resolved and validated here
	// so a bad egress rule fails at load rather than at create.
	//
	// @decision(DL-031)
	policy, err := sandbox.EgressPreset(c.Sandbox.Egress)
	if err != nil {
		return fmt.Errorf("config: sandbox.egress: %w", err)
	}
	if err := policy.WithDomains(c.Sandbox.EgressExtraDomains...).Validate(); err != nil {
		return fmt.Errorf("config: sandbox.egress_extra_domains: %w", err)
	}
	for _, cidr := range c.Sandbox.InboundCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return fmt.Errorf("config: sandbox.inbound_cidrs: %q: %w", cidr, err)
		}
	}
	if c.Sandbox.IdleTimeout > c.Sandbox.Timeout {
		return fmt.Errorf("config: sandbox.idle_timeout %s exceeds sandbox.timeout %s", c.Sandbox.IdleTimeout, c.Sandbox.Timeout)
	}
	if c.OpenCode.Version == "" {
		return fmt.Errorf("config: opencode.version must be set")
	}
	if c.OpenCode.Port < 1 || c.OpenCode.Port > 65535 {
		return fmt.Errorf("config: opencode.port %d is out of range", c.OpenCode.Port)
	}
	if c.Modal.App == "" {
		return fmt.Errorf("config: modal.app must be set")
	}

	seen := make(map[string]bool, len(c.Platforms))
	kinds := make(map[string]PlatformKind, len(c.Platforms))
	for i, p := range c.Platforms {
		if p.Name == "" {
			return fmt.Errorf("config: platforms[%d] has no name", i)
		}
		if !validName(p.Name) {
			return fmt.Errorf("config: platform name %q may only contain letters, digits, '-' and '_'", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("config: duplicate platform %q", p.Name)
		}
		seen[p.Name] = true
		kinds[p.Name] = p.Kind
		// @decision(DL-041)
		if !git.KnownKind(string(p.Kind)) {
			return fmt.Errorf("config: platform %q has unknown kind %q (want one of %s)",
				p.Name, p.Kind, strings.Join(git.Kinds, ", "))
		}
		if (p.Kind == KindForgejo || p.Kind == KindGitea) && p.BaseURL == "" {
			return fmt.Errorf("config: platform %q is a %s platform and needs base_url", p.Name, p.Kind)
		}
		if p.BaseURL != "" && !strings.HasPrefix(p.BaseURL, "https://") && !strings.HasPrefix(p.BaseURL, "http://") {
			return fmt.Errorf("config: platform %q base_url %q must be an http(s) URL", p.Name, p.BaseURL)
		}
		// @decision(DL-042)
		switch p.Prefer {
		case "", PreferPersonal, PreferShared:
		default:
			return fmt.Errorf("config: platform %q prefer %q is neither %q nor %q", p.Name, p.Prefer, PreferPersonal, PreferShared)
		}
	}

	dirs := make(map[string]bool, len(c.Repos))
	for i, r := range c.Repos {
		if r.Owner == "" || r.Name == "" {
			return fmt.Errorf("config: repos[%d] needs both owner and name", i)
		}
		if r.Platform == "" {
			return fmt.Errorf("config: repo %s has no platform", r.Slug())
		}
		if !seen[r.Platform] {
			return fmt.Errorf("config: repo %s references unknown platform %q", r.Slug(), r.Platform)
		}
		// @decision(DL-041)
		if err := git.CheckRepoPath(string(kinds[r.Platform]), r.Owner, r.Name); err != nil {
			return fmt.Errorf("config: repo %s: %w", r.Slug(), err)
		}
		dir := r.Dirname()
		if err := validWorkspaceDir(dir); err != nil {
			return fmt.Errorf("config: repo %s: %w", r.Slug(), err)
		}
		if dirs[dir] {
			return fmt.Errorf("config: two repos both bootstrap into directory %q", dir)
		}
		dirs[dir] = true
	}

	if c.Landing != "" && !dirs[c.Landing] {
		return fmt.Errorf("config: landing %q is not one of the configured repo directories", c.Landing)
	}
	return nil
}

// ValidServerURL accepts an empty URL or an absolute http(s) URL with a
// host and no credentials.
//
// @decision(DL-002)
func ValidServerURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must start with http:// or https://", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	if u.User != nil {
		return fmt.Errorf("must not carry credentials; the token comes from SPOV_SERVER_TOKEN")
	}
	return nil
}

// validName reports whether s is safe to use as a credential-store key.
//
// @decision(DL-026)
func validName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// validWorkspaceDir rejects directory names that would escape the sandbox
// workspace root when repos are cloned into it.
//
// @decision(DL-026)
func validWorkspaceDir(dir string) error {
	if dir == "" {
		return errors.New("clone directory is empty")
	}
	if path.IsAbs(dir) || strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, `\\`) {
		return fmt.Errorf("clone directory %q must be relative to the workspace root", dir)
	}
	if dir != path.Clean(dir) {
		return fmt.Errorf("clone directory %q is not a clean path", dir)
	}
	for _, part := range strings.Split(dir, "/") {
		if part == ".." || part == "." {
			return fmt.Errorf("clone directory %q must not traverse outside the workspace root", dir)
		}
	}
	return nil
}
