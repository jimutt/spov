package sandbox

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestEgressPresetsAllowWhatTheyName.
//
// @decision(DL-031)
func TestEgressPresetsAllowWhatTheyName(t *testing.T) {
	open, install, agent := EgressOpen(), EgressInstall(), EgressAgent()

	if !open.Open() {
		t.Error("the open preset does not allow everything")
	}
	for _, p := range []EgressPolicy{install, agent} {
		if p.Open() {
			t.Errorf("the %s preset allows everything", p.Preset)
		}
		// Nothing by IP: an empty CIDR list is what blocks non-TLS
		// traffic, and a nil one would mean open at the provider.
		if len(p.CIDRs) != 0 {
			t.Errorf("the %s preset allows cidrs %v", p.Preset, p.CIDRs)
		}
		for _, want := range domains(GitHostDomains, ModelAPIDomains, AgentServiceDomains) {
			if !slices.Contains(p.Domains, want) {
				t.Errorf("the %s preset does not allow %s", p.Preset, want)
			}
		}
	}
	for _, reg := range RegistryDomains {
		if !slices.Contains(install.Domains, reg) {
			t.Errorf("install does not allow the registry %s", reg)
		}
		if slices.Contains(agent.Domains, reg) {
			t.Errorf("agent allows the registry %s; an unattended agent installs nothing", reg)
		}
	}
	if len(install.Domains) != len(domains(install.Domains)) {
		t.Error("the install preset repeats a domain")
	}
}

// TestEgressPresetByName.
//
// @decision(DL-031)
func TestEgressPresetByName(t *testing.T) {
	for _, name := range EgressPresets() {
		p, err := EgressPreset(name)
		if err != nil || p.Preset != name {
			t.Errorf("EgressPreset(%q) = %+v, %v", name, p, err)
		}
	}
	_, err := EgressPreset("wide-open")
	if err == nil {
		t.Fatal("an unknown preset was accepted")
	}
	for _, name := range EgressPresets() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not offer %q: %v", name, err)
		}
	}
	if _, err := EgressPreset(""); err == nil {
		t.Error("an empty preset name was accepted")
	}
}

// TestEgressWithDomains: adding a domain makes a preset custom, because
// it is no longer what the name describes.
//
// @decision(DL-031)
func TestEgressWithDomains(t *testing.T) {
	base := EgressAgent()
	got := base.WithDomains("registry.internal.example", "api.anthropic.com")
	if got.Preset != PresetCustom {
		t.Errorf("preset = %q, want custom", got.Preset)
	}
	if !slices.Contains(got.Domains, "registry.internal.example") {
		t.Error("the extra domain was dropped")
	}
	if len(got.Domains) != len(base.Domains)+1 {
		t.Errorf("a duplicate domain was added twice: %v", got.Domains)
	}
	if slices.Contains(base.Domains, "registry.internal.example") {
		t.Error("WithDomains mutated the policy it was called on")
	}
	if same := base.WithDomains(); same.Preset != base.Preset {
		t.Error("adding nothing changed the preset")
	}
	if only := base.WithDomains("api.anthropic.com"); only.Preset != base.Preset {
		t.Errorf("adding a domain already allowed changed the preset to %q", only.Preset)
	}
}

// TestEgressPolicyValidate.
//
// @decision(DL-031)
func TestEgressPolicyValidate(t *testing.T) {
	if err := (EgressPolicy{}).Validate(); err == nil {
		t.Fatal("a zero policy validated; a forgotten policy must not mean open")
	}
	for _, p := range []EgressPolicy{EgressOpen(), EgressInstall(), EgressAgent()} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", p.Preset, err)
		}
	}
	bad := map[string]EgressPolicy{
		"bad cidr":       {Preset: PresetCustom, CIDRs: []string{"10.0.0.1"}},
		"url as domain":  {Preset: PresetCustom, Domains: []string{"https://example.com"}},
		"path in domain": {Preset: PresetCustom, Domains: []string{"example.com/path"}},
		"inner wildcard": {Preset: PresetCustom, Domains: []string{"api.*.example.com"}},
		"bare label":     {Preset: PresetCustom, Domains: []string{"localhost"}},
		"trailing dot":   {Preset: PresetCustom, Domains: []string{"example.com."}},
		"empty domain":   {Preset: PresetCustom, Domains: []string{""}},
		"wildcard only":  {Preset: PresetCustom, Domains: []string{"*."}},
		"port in domain": {Preset: PresetCustom, Domains: []string{"example.com:443"}},
	}
	for name, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("%s validated: %+v", name, p)
		}
	}
	ok := EgressPolicy{Preset: PresetCustom, CIDRs: []string{"10.0.0.0/8"}, Domains: []string{"*.example.com", "api.example.com", AllowAllDomains}}
	if err := ok.Validate(); err != nil {
		t.Errorf("a good policy was refused: %v", err)
	}
}

// TestSpecRequiresAnEgressPolicy: the provider reads an unset allowlist
// as unrestricted, so this is the guard that stops a forgotten policy
// from becoming an open sandbox.
//
// @decision(DL-031)
func TestSpecRequiresAnEgressPolicy(t *testing.T) {
	spec := Spec{
		Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, IdleTimeout: time.Minute,
	}
	if err := spec.Validate(); err == nil {
		t.Fatal("a spec with no egress policy validated")
	}
	spec.Egress = EgressOpen()
	if err := spec.Validate(); err != nil {
		t.Fatalf("a spec with a policy was refused: %v", err)
	}
	spec.InboundCIDRs = []string{"not-a-cidr"}
	if err := spec.Validate(); err == nil {
		t.Error("a bad inbound cidr validated")
	}
}

// TestSpecValidatesTheNewLimits.
//
// @decision(DL-029)
func TestSpecValidatesTheNewLimits(t *testing.T) {
	base := Spec{
		Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, IdleTimeout: time.Minute, Egress: EgressOpen(),
	}
	cases := map[string]func(*Spec){
		"cpu limit below request":    func(s *Spec) { s.CPULimit = 0.25 },
		"memory limit below request": func(s *Spec) { s.MemoryLimitMiB = 512 },
		"relative workdir":           func(s *Spec) { s.Workdir = "workspace" },
		"unknown cloud":              func(s *Spec) { s.Cloud = "azure" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := base
			mutate(&spec)
			if err := spec.Validate(); err == nil {
				t.Error("validated")
			}
		})
	}
	good := base
	good.CPULimit, good.MemoryLimitMiB, good.Workdir, good.Cloud = 1, 2048, "/workspace", "aws"
	if err := good.Validate(); err != nil {
		t.Errorf("a good spec was refused: %v", err)
	}
	for _, cloud := range Clouds {
		spec := base
		spec.Cloud = cloud
		if err := spec.Validate(); err != nil {
			t.Errorf("cloud %q: %v", cloud, err)
		}
	}
}

// TestFakeSetEgressRecordsAndValidates.
//
// @decision(DL-031)
func TestFakeSetEgressRecordsAndValidates(t *testing.T) {
	f := NewFake()
	h, err := f.Create(context.Background(), Spec{
		Name: "s", Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, IdleTimeout: time.Minute, Egress: EgressOpen(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetEgress(context.Background(), EgressAgent()); err != nil {
		t.Fatal(err)
	}
	fh := f.Handles()["sb-fake-1"]
	if log := fh.EgressLog(); len(log) != 1 || log[0].Preset != PresetAgent {
		t.Errorf("egress log = %+v", log)
	}
	if err := h.SetEgress(context.Background(), EgressPolicy{}); err == nil {
		t.Error("an empty policy was accepted")
	}
	if err := h.Terminate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.SetEgress(context.Background(), EgressAgent()); err == nil {
		t.Error("a terminated sandbox accepted a policy change")
	}
}

// TestWithDomainsNeverAliasesItsInput: EgressPolicy is a value type, and
// the no-extra-domains path is the common one — every default launch
// takes it.
//
// @decision(DL-031)
func TestWithDomainsNeverAliasesItsInput(t *testing.T) {
	base := EgressAgent()
	before := slices.Clone(base.Domains)
	got := base.WithDomains()
	if len(got.Domains) == 0 {
		t.Fatal("no domains")
	}
	got.Domains[0] = "MUTATED"
	got.CIDRs = append(got.CIDRs, "10.0.0.0/8")
	if !slices.Equal(base.Domains, before) {
		t.Errorf("writing through the result changed its input: %v", base.Domains)
	}
	if len(base.CIDRs) != 0 {
		t.Errorf("cidrs leaked: %v", base.CIDRs)
	}
}

// TestOpenPolicyRefusesExtraDomains: a user who added domains to the
// open preset meant to narrow something, and silently doing nothing is
// the wrong answer.
//
// @decision(DL-031)
func TestOpenPolicyRefusesExtraDomains(t *testing.T) {
	p := EgressOpen().WithDomains("registry.internal.example")
	if err := p.Validate(); err == nil {
		t.Fatal("an open policy with extra domains validated")
	} else if !strings.Contains(err.Error(), PresetAgent) {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
	if err := EgressOpen().Validate(); err != nil {
		t.Errorf("the plain open preset was refused: %v", err)
	}
}

// TestDomainsAreFoldedAndDeduplicated: hostnames are case-insensitive,
// so two spellings must not become two allowlist entries.
//
// @decision(DL-031)
func TestDomainsAreFoldedAndDeduplicated(t *testing.T) {
	p := EgressAgent()
	got := p.WithDomains("GITHUB.COM", " github.com ", "Example.COM")
	if n := len(got.Domains) - len(p.Domains); n != 1 {
		t.Errorf("adding two spellings of an allowed domain grew the list by %d", n)
	}
	for _, d := range got.Domains {
		if d != strings.ToLower(d) {
			t.Errorf("domain %q was not folded", d)
		}
	}
}

// TestValidDomainChecksEachLabel.
//
// @decision(DL-031)
func TestValidDomainChecksEachLabel(t *testing.T) {
	bad := []string{"a..b.com", "-bad.example.com", "bad-.example.com", "exa_mple.com", "1.2.3.4", "::1"}
	for _, d := range bad {
		if err := (EgressPolicy{Preset: PresetCustom, Domains: []string{d}}).Validate(); err == nil {
			t.Errorf("%q validated", d)
		}
	}
	good := []string{"*.com", "example.com", "*.example.com", "a-b.example.com", "xn--mnchen-3ya.de", "münchen.de"}
	for _, d := range good {
		if err := (EgressPolicy{Preset: PresetCustom, Domains: []string{d}}).Validate(); err != nil {
			t.Errorf("%q was refused: %v", d, err)
		}
	}
}

// TestSpecEnforcesTheIdleFloor: the floor lives here rather than only in
// config, so a request arriving over the API cannot bypass it and leave
// a sandbox no heartbeat can keep alive.
//
// @decision(DL-030) @decision(DL-029)
func TestSpecEnforcesTheIdleFloor(t *testing.T) {
	spec := Spec{
		Image: "img", Command: []string{"true"}, CPU: 0.5, MemoryMiB: 1024,
		Timeout: time.Hour, Egress: EgressOpen(),
	}
	for _, idle := range []time.Duration{0, time.Second, 30 * time.Second, MinIdleTimeout - time.Nanosecond} {
		spec.IdleTimeout = idle
		if err := spec.Validate(); err == nil {
			t.Errorf("idle timeout %s validated, below the %s floor", idle, MinIdleTimeout)
		}
	}
	spec.IdleTimeout = MinIdleTimeout
	if err := spec.Validate(); err != nil {
		t.Errorf("the floor itself was refused: %v", err)
	}
}

// A narrowed preset gains the session's forge hosts, without their port,
// and keeps its name; an open policy is left as it is.
//
// @decision(DL-031)
func TestAPresetReachesTheSessionsForges(t *testing.T) {
	got := EgressAgent().WithForgeHosts("GitLab.com", "git.example.com:8443", "")
	if got.Preset != PresetAgent {
		t.Errorf("preset = %q", got.Preset)
	}
	for _, want := range []string{"gitlab.com", "git.example.com"} {
		if !slices.Contains(got.Domains, want) {
			t.Errorf("%s not allowed: %v", want, got.Domains)
		}
	}
	if slices.Contains(got.Domains, "") || slices.Contains(got.Domains, "git.example.com:8443") {
		t.Errorf("domains = %v", got.Domains)
	}
	if open := EgressOpen().WithForgeHosts("gitlab.com"); !slices.Equal(open.Domains, EgressOpen().Domains) {
		t.Errorf("open = %+v", open)
	}
	if err := EgressInstall().WithForgeHosts("gitea.com").Validate(); err != nil {
		t.Error(err)
	}
	// An IP or a single-label host is no domain, and would fail the
	// launch if it were added.
	odd := EgressAgent().WithForgeHosts("10.0.0.5", "10.0.0.5:3000", "[fd00::1]:3000", "forgejo", "forgejo:3000")
	if !slices.Equal(odd.Domains, EgressAgent().Domains) {
		t.Errorf("domains = %v", odd.Domains)
	}
	if err := odd.Validate(); err != nil {
		t.Error(err)
	}
}
