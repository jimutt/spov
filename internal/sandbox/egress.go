package sandbox

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// EgressPolicy is what a sandbox may reach. It is a required part of a
// Spec, and both dimensions must be sent at creation since Modal refuses
// to narrow a sandbox created without them. An empty list means nothing
// is allowed on that dimension; "open" is spelled with explicit
// allow-all entries.
//
// @decision(DL-031)
type EgressPolicy struct {
	// Preset names where this policy came from. Required.
	Preset string
	// CIDRs is the outbound IP allowlist. It governs all traffic,
	// including non-TLS.
	CIDRs []string
	// Domains is the outbound domain allowlist, matched by SNI on TLS
	// connections. Entries may be wildcards like *.example.com, which
	// match subdomains but not the bare domain.
	Domains []string
}

// The preset names. A policy assembled by hand carries PresetCustom.
//
// @decision(DL-031)
const (
	PresetOpen    = "open"
	PresetInstall = "install"
	PresetAgent   = "agent"
	PresetCustom  = "custom"
)

// The allow-all allowlist entries.
//
// @decision(DL-031)
const (
	AllowAllCIDRs   = "0.0.0.0/0"
	AllowAllDomains = "*"
)

// The domains the presets are built from. Extending these lists changes
// what a named preset means, so it is a decision, not an implementation
// detail.
//
// @decision(DL-031)
var (
	// GitHostDomains are GitHub's hosts. The hosts of the other forges a
	// session uses are added at launch (WithForgeHosts).
	GitHostDomains = []string{
		"github.com", "api.github.com", "codeload.github.com",
		"objects.githubusercontent.com", "*.githubusercontent.com",
	}
	// RegistryDomains are the package registries and toolchain
	// downloads a dependency install needs, for Go, Node, Python, Rust,
	// Java, .NET, Ruby, PHP and Debian packages, and for mise and
	// Playwright installing a version on first use.
	//
	// @decision(DL-031) @decision(DL-032)
	RegistryDomains = []string{
		// Debian
		"deb.debian.org", "security.debian.org",
		// Go
		"proxy.golang.org", "sum.golang.org", "storage.googleapis.com",
		"dl.google.com", "go.dev",
		// Node and Bun
		"registry.npmjs.org", "nodejs.org", "bun.sh", "*.bun.sh",
		// Python and uv
		"pypi.org", "files.pythonhosted.org", "astral.sh",
		// Rust
		"crates.io", "static.crates.io", "index.crates.io",
		"static.rust-lang.org", "sh.rustup.rs",
		// Java: Maven Central, Gradle, JDK distributions
		"repo1.maven.org", "repo.maven.apache.org",
		"maven-central.storage-download.googleapis.com",
		"plugins.gradle.org", "plugins-artifacts.gradle.org",
		"services.gradle.org", "downloads.gradle.org",
		"api.adoptium.net", "api.sdkman.io", "download.java.net",
		// .NET: NuGet and SDK downloads
		"api.nuget.org", "nuget.org", "*.nuget.org",
		"builds.dotnet.microsoft.com", "download.visualstudio.microsoft.com",
		"dotnetcli.blob.core.windows.net", "dot.net",
		// Ruby gems and PHP's Composer
		"rubygems.org", "index.rubygems.org",
		"repo.packagist.org", "packagist.org",
		// mise's version index, and Sigstore's trust root, which mise
		// reads to verify GitHub attestations
		"mise-versions.jdx.dev", "tuf-repo-cdn.sigstore.dev",
		// Playwright's browsers, for a project on another version
		"cdn.playwright.dev", "playwright.download.prss.microsoft.com",
	}
	// ModelAPIDomains are the model providers OpenCode talks to.
	ModelAPIDomains = []string{
		"api.anthropic.com", "api.openai.com",
		"generativelanguage.googleapis.com", "openrouter.ai",
		"opencode.ai", "*.opencode.ai",
	}
	// AgentServiceDomains are the hosted services OpenCode's built-in
	// tools call: web search is pinned to Exa (opencode.WebSearchProvider).
	AgentServiceDomains = []string{"mcp.exa.ai"}
)

// EgressOpen allows everything. It is what an interactive session gets
// by default: the user is at the keyboard, and a toolchain that cannot
// reach a registry is a support problem rather than a security win.
//
// @decision(DL-031)
func EgressOpen() EgressPolicy {
	return EgressPolicy{Preset: PresetOpen, CIDRs: []string{AllowAllCIDRs}, Domains: []string{AllowAllDomains}}
}

// EgressInstall allows the forges, the package registries and the model
// APIs over TLS, and nothing by IP. It is the phase for bringing a
// workspace up.
//
// @decision(DL-031)
func EgressInstall() EgressPolicy {
	return EgressPolicy{Preset: PresetInstall, Domains: domains(GitHostDomains, RegistryDomains, ModelAPIDomains, AgentServiceDomains)}
}

// EgressAgent allows the forges and the model APIs, and nothing else.
// It is what an agent running unattended should have: enough to read a
// repository, think, and push a branch.
//
// @decision(DL-031)
func EgressAgent() EgressPolicy {
	return EgressPolicy{Preset: PresetAgent, Domains: domains(GitHostDomains, ModelAPIDomains, AgentServiceDomains)}
}

// EgressPreset returns the policy a preset name refers to.
//
// @decision(DL-031)
func EgressPreset(name string) (EgressPolicy, error) {
	switch name {
	case PresetOpen:
		return EgressOpen(), nil
	case PresetInstall:
		return EgressInstall(), nil
	case PresetAgent:
		return EgressAgent(), nil
	default:
		return EgressPolicy{}, fmt.Errorf("sandbox: unknown egress preset %q (want %s)", name, strings.Join(EgressPresets(), ", "))
	}
}

// EgressPresets lists the preset names a user may choose.
func EgressPresets() []string { return []string{PresetOpen, PresetInstall, PresetAgent} }

// WithForgeHosts returns the policy with the hosts of the forges a
// session uses allowed. A preset that allows the forges means this
// session's forges, so the preset keeps its name; an open policy is
// returned as it is.
//
// @decision(DL-031)
func (p EgressPolicy) WithForgeHosts(hosts ...string) EgressPolicy {
	if p.Open() || (p.Preset != PresetInstall && p.Preset != PresetAgent) {
		return p
	}
	var bare []string
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if name, _, err := net.SplitHostPort(h); err == nil {
			h = name
		}
		// An IP or a single-label name is no domain; a forge there is
		// reachable only under the open preset.
		if h == "" || strings.Trim(h, "[]") != h || !strings.Contains(h, ".") {
			continue
		}
		if _, err := netip.ParseAddr(h); err == nil {
			continue
		}
		bare = append(bare, h)
	}
	return EgressPolicy{Preset: p.Preset, CIDRs: slices.Clone(p.CIDRs), Domains: domains(p.Domains, bare)}
}

// WithRouteHosts returns the policy with the hosts of a session's routed
// credentials allowed, keeping the preset's name, as forge hosts are. Unlike
// WithForgeHosts it also adds them to a custom policy, which a launch's
// extra domains have already made of its preset. An open policy is
// returned as it is.
//
// @decision(DL-072)
func (p EgressPolicy) WithRouteHosts(hosts ...string) EgressPolicy {
	if p.Open() || len(hosts) == 0 {
		return p
	}
	return EgressPolicy{Preset: p.Preset, CIDRs: slices.Clone(p.CIDRs), Domains: domains(p.Domains, hosts)}
}

// WithDomains returns the policy with more domains allowed. The preset
// becomes custom, since it is no longer what the name describes.
//
// @decision(DL-031)
func (p EgressPolicy) WithDomains(extra ...string) EgressPolicy {
	out := EgressPolicy{Preset: p.Preset, CIDRs: slices.Clone(p.CIDRs), Domains: domains(p.Domains, extra)}
	if !slices.Equal(out.Domains, p.Domains) {
		out.Preset = PresetCustom
	}
	return out
}

// Open reports whether the policy allows unrestricted egress.
func (p EgressPolicy) Open() bool {
	return slices.Contains(p.CIDRs, AllowAllCIDRs) && slices.Contains(p.Domains, AllowAllDomains)
}

// Validate reports why the policy cannot be used.
//
// @decision(DL-031)
func (p EgressPolicy) Validate() error {
	if p.Preset == "" {
		return fmt.Errorf("sandbox: an egress policy is required; use one of %s", strings.Join(EgressPresets(), ", "))
	}
	// Extra domains on top of an open policy do nothing, and a user who
	// wrote them meant to narrow something.
	if p.Open() && len(p.Domains) > 1 {
		return fmt.Errorf("sandbox: the %s preset already allows everything, so extra domains have no effect; choose %s or %s to narrow",
			PresetOpen, PresetInstall, PresetAgent)
	}
	for _, cidr := range p.CIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return fmt.Errorf("sandbox: egress cidr %q: %w", cidr, err)
		}
	}
	for _, d := range p.Domains {
		if err := validDomain(d); err != nil {
			return err
		}
	}
	return nil
}

// validDomain accepts a hostname or a leading-wildcard pattern. It is
// deliberately strict: a domain with a scheme or a path silently matches
// nothing, which reads as an egress rule that is not working.
func validDomain(d string) error {
	if d == AllowAllDomains {
		return nil
	}
	rest, wildcard := strings.CutPrefix(d, "*.")
	switch {
	case rest == "":
		return fmt.Errorf("sandbox: egress domain %q is empty", d)
	case strings.ContainsAny(rest, "/:*? "):
		return fmt.Errorf("sandbox: egress domain %q must be a hostname, not a URL or pattern", d)
	}
	labels := strings.Split(rest, ".")
	if len(labels) < 2 && !wildcard {
		return fmt.Errorf("sandbox: egress domain %q is not a fully qualified name", d)
	}
	// An IP belongs in the CIDR list: domains are matched by SNI, so an
	// address here silently matches nothing.
	if _, err := netip.ParseAddr(rest); err == nil {
		return fmt.Errorf("sandbox: egress domain %q is an IP address; put it in the cidr list", d)
	}
	for _, label := range labels {
		if err := validLabel(label, d); err != nil {
			return err
		}
	}
	return nil
}

// validLabel checks one dot-separated part of a hostname.
func validLabel(label, domain string) error {
	if label == "" {
		return fmt.Errorf("sandbox: egress domain %q has an empty label", domain)
	}
	if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return fmt.Errorf("sandbox: egress domain %q has a label starting or ending with a hyphen", domain)
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		case r > 127: // an internationalised name, punycode or not
		default:
			return fmt.Errorf("sandbox: egress domain %q contains %q, which is not valid in a hostname", domain, r)
		}
	}
	return nil
}

// domains merges lists, lowercasing and removing duplicates while
// keeping a stable order. Hostnames are case-insensitive, so folding
// here means GitHub.com and github.com are one entry rather than two.
func domains(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, list := range lists {
		for _, d := range list {
			d = strings.ToLower(strings.TrimSpace(d))
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}
