package config

import (
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/sandbox"
)

// valid returns a minimal config that passes Validate, which each test then
// breaks in exactly one way.
func valid() Config {
	c := Defaults()
	c.Platforms = []Platform{
		{Name: "codefloe", Kind: KindForgejo, BaseURL: "https://codefloe.com"},
		{Name: "gh", Kind: KindGitHub},
	}
	c.Repos = []Repo{
		{Platform: "codefloe", Owner: "alice", Name: "spov"},
		{Platform: "gh", Owner: "acme", Name: "spov", Dir: "spov-gh"},
	}
	c.Landing = "spov"
	return c
}

func TestValidateAcceptsGoodConfig(t *testing.T) {
	if err := Validate(valid()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := Validate(Defaults()); err != nil {
		t.Fatalf("Validate(Defaults()): %v", err)
	}
}

// Every kind is accepted, and a GitLab project may sit in a subgroup.
//
// @decision(DL-041)
func TestValidateAcceptsEveryKind(t *testing.T) {
	c := valid()
	c.Platforms = append(c.Platforms,
		Platform{Name: "gt", Kind: KindGitea, BaseURL: "https://gitea.com"},
		Platform{Name: "gl", Kind: KindGitLab},
	)
	c.Repos = append(c.Repos,
		Repo{Platform: "gt", Owner: "me", Name: "app", Dir: "gt-app"},
		Repo{Platform: "gl", Owner: "group/sub", Name: "project"},
	)
	if err := Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(*Config)
		want   string
	}{
		{"wrong schema version", func(c *Config) { c.Version = 99 }, "schema version"},
		{"zero cpu", func(c *Config) { c.Sandbox.CPU = 0 }, "sandbox.cpu"},
		{"negative memory", func(c *Config) { c.Sandbox.MemoryMiB = -1 }, "memory_mib"},
		{"zero timeout", func(c *Config) { c.Sandbox.Timeout = 0 }, "sandbox.timeout"},
		{"timeout over cap", func(c *Config) { c.Sandbox.Timeout = 13 * time.Hour }, "maximum"},
		{"zero idle timeout", func(c *Config) { c.Sandbox.IdleTimeout = 0 }, "idle_timeout"},
		{"idle over timeout", func(c *Config) {
			c.Sandbox.Timeout = time.Hour
			c.Sandbox.IdleTimeout = 2 * time.Hour
		}, "exceeds sandbox.timeout"},
		{"no opencode version", func(c *Config) { c.OpenCode.Version = "" }, "opencode.version"},
		// @decision(DL-075)
		{"a bad routed name", func(c *Config) { c.Sandbox.Credentials = []string{"Bad_Name"} }, "sandbox.credentials"},
		{"none beside others", func(c *Config) { c.Sandbox.JobCredentials = []string{"none", "a"} }, "sandbox.job_credentials"},
		{"a wildcard need", func(c *Config) { c.Sandbox.Needs = []string{"*.acme.com"} }, "sandbox.needs"},
		// @decision(DL-043)
		{"unknown git access", func(c *Config) { c.Sandbox.GitAccess = "everything" }, "sandbox.git_access"},
		{"unknown job git access", func(c *Config) { c.Sandbox.JobGitAccess = "All" }, "sandbox.job_git_access"},
		{"port too low", func(c *Config) { c.OpenCode.Port = 0 }, "opencode.port"},
		{"port too high", func(c *Config) { c.OpenCode.Port = 70000 }, "opencode.port"},
		{"nameless platform", func(c *Config) { c.Platforms[0].Name = "" }, "no name"},
		{"duplicate platform", func(c *Config) { c.Platforms[1].Name = c.Platforms[0].Name }, "duplicate platform"},
		{"unknown platform kind", func(c *Config) { c.Platforms[0].Kind = "bitbucket" }, "unknown kind"},
		{"gitea without base url", func(c *Config) { c.Platforms[0].Kind = KindGitea; c.Platforms[0].BaseURL = "" }, "base_url"},
		{"nested owner off gitlab", func(c *Config) { c.Repos[0].Owner = "group/sub" }, "more than one segment"},
		{"forgejo without base url", func(c *Config) { c.Platforms[0].BaseURL = "" }, "base_url"},
		{"non-http base url", func(c *Config) { c.Platforms[0].BaseURL = "codefloe.com" }, "http(s) URL"},
		{"repo without owner", func(c *Config) { c.Repos[0].Owner = "" }, "owner and name"},
		{"repo without name", func(c *Config) { c.Repos[0].Name = "" }, "owner and name"},
		{"repo without platform", func(c *Config) { c.Repos[0].Platform = "" }, "no platform"},
		{"repo with unknown platform", func(c *Config) { c.Repos[0].Platform = "nope" }, "unknown platform"},
		{"colliding repo dirs", func(c *Config) { c.Repos[1].Dir = "" }, "both bootstrap into directory"},
		{"landing not a repo dir", func(c *Config) { c.Landing = "elsewhere" }, "landing"},
		{"no modal app", func(c *Config) { c.Modal.App = "" }, "modal.app"},
		{"platform name with a slash", func(c *Config) { c.Platforms[0].Name = "a/b" }, "may only contain"},
		{"platform name with a space", func(c *Config) { c.Platforms[0].Name = "my platform" }, "may only contain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.break_(&c)
			err := Validate(c)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestRepoHelpers(t *testing.T) {
	r := Repo{Owner: "alice", Name: "spov"}
	if r.Slug() != "alice/spov" {
		t.Errorf("Slug() = %q", r.Slug())
	}
	if r.Dirname() != "spov" {
		t.Errorf("Dirname() = %q, want the repo name", r.Dirname())
	}
	r.Dir = "custom"
	if r.Dirname() != "custom" {
		t.Errorf("Dirname() = %q, want the explicit dir", r.Dirname())
	}
}

func TestValidateRejectsUnsafeCloneDirs(t *testing.T) {
	unsafe := []string{
		"/etc",
		"../escape",
		"a/../../b",
		"./here",
		"sub/../..",
		"..",
	}
	for _, dir := range unsafe {
		t.Run(dir, func(t *testing.T) {
			c := valid()
			c.Repos[0].Dir = dir
			c.Landing = ""
			if err := Validate(c); err == nil {
				t.Errorf("Validate accepted clone directory %q", dir)
			}
		})
	}
}

func TestValidateAcceptsSafeCloneDirs(t *testing.T) {
	safe := []string{"spov", "sub/dir", "with-dash", "with_underscore", "a/b/c"}
	for _, dir := range safe {
		t.Run(dir, func(t *testing.T) {
			c := valid()
			c.Repos[0].Dir = dir
			c.Landing = dir
			if err := Validate(c); err != nil {
				t.Errorf("Validate rejected clone directory %q: %v", dir, err)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"codefloe", true},
		{"my-platform", true},
		{"my_platform_2", true},
		{"", true}, // emptiness is caught separately, with a better message
		{"a/b", false},
		{"a b", false},
		{"a.b", false},
		{"a:b", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := validName(tt.in); got != tt.want {
				t.Errorf("validName(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestValidateSandboxLimitsAndEgress covers the keys DL-029 added: each
// one is refused locally rather than after a billable create.
//
// @decision(DL-029) @decision(DL-031)
func TestValidateSandboxLimitsAndEgress(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"cpu limit below request": {
			func(c *Config) { c.Sandbox.CPU, c.Sandbox.CPULimit = 1, 0.5 }, "cpu_limit",
		},
		"memory limit below request": {
			func(c *Config) { c.Sandbox.MemoryMiB, c.Sandbox.MemoryLimitMiB = 2048, 1024 }, "memory_limit_mib",
		},
		"gpu is refused loudly": {
			func(c *Config) { c.Sandbox.GPU = "A100" }, "gpu is not available",
		},
		"unknown cloud": {
			func(c *Config) { c.Sandbox.Cloud = "azure" }, "cloud",
		},
		"relative workdir": {
			func(c *Config) { c.Sandbox.Workdir = "workspace" }, "workdir",
		},
		"unknown egress preset": {
			func(c *Config) { c.Sandbox.Egress = "wide-open" }, "egress",
		},
		"bad extra domain": {
			func(c *Config) { c.Sandbox.EgressExtraDomains = []string{"https://evil.example"} }, "egress_extra_domains",
		},
		"bad inbound cidr": {
			func(c *Config) { c.Sandbox.InboundCIDRs = []string{"203.0.113.1"} }, "inbound_cidrs",
		},
		"idle timeout below the floor": {
			func(c *Config) { c.Sandbox.IdleTimeout = 30 * time.Second }, "idle_timeout",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			tc.mutate(&cfg)
			err := Validate(cfg)
			if err == nil {
				t.Fatal("validated")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}

	good := Defaults()
	good.Sandbox.CPULimit = 2
	good.Sandbox.MemoryLimitMiB = 4096
	good.Sandbox.Cloud = "aws"
	good.Sandbox.Regions = []string{"us-east-1"}
	good.Sandbox.Workdir = "/workspace"
	good.Sandbox.Egress = sandbox.PresetAgent
	good.Sandbox.EgressExtraDomains = []string{"registry.internal.example"}
	good.Sandbox.InboundCIDRs = []string{"203.0.113.0/24"}
	if err := Validate(good); err != nil {
		t.Errorf("a fully configured sandbox was refused: %v", err)
	}
}

// TestDefaultsAreValidAndOpen: an interactive session reaches whatever
// its toolchain needs unless the user narrows it.
//
// @decision(DL-031)
func TestDefaultsAreValidAndOpen(t *testing.T) {
	cfg := Defaults()
	if err := Validate(cfg); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
	if cfg.Sandbox.Egress != sandbox.PresetOpen {
		t.Errorf("default egress = %q, want %q", cfg.Sandbox.Egress, sandbox.PresetOpen)
	}
	policy, err := sandbox.EgressPreset(cfg.Sandbox.Egress)
	if err != nil || !policy.Open() {
		t.Errorf("the default preset is not open: %+v, %v", policy, err)
	}
}
