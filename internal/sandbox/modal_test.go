package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// newTestProvider returns a ModalProvider with no client, usable only for
// the pure translation paths that do not call Modal.
func newTestProvider() *ModalProvider {
	return &ModalProvider{env: "test"}
}

func TestCreateParamsTranslatesSpec(t *testing.T) {
	p := newTestProvider()
	spec := validSpec()
	spec.Name = "morning"
	spec.Env = map[string]string{"OPENCODE_PASSWORD": "secret"}
	spec.Tags = BuildTags(TagSet{Name: "morning", User: "alice", Repos: []string{"alice/spov"}, CreatedAt: time.Now()})

	params, err := p.createParams(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}

	if params.Name != "morning" {
		t.Errorf("Name = %q", params.Name)
	}
	if params.CPU != spec.CPU || params.MemoryMiB != spec.MemoryMiB {
		t.Errorf("resources = %v/%d", params.CPU, params.MemoryMiB)
	}
	if params.Timeout != spec.Timeout || params.IdleTimeout != spec.IdleTimeout {
		t.Errorf("timeouts = %s/%s", params.Timeout, params.IdleTimeout)
	}
	if len(params.EncryptedPorts) != 1 || params.EncryptedPorts[0] != 4096 {
		t.Errorf("EncryptedPorts = %v; the port must be TLS-terminated", params.EncryptedPorts)
	}
	if params.UnencryptedPorts != nil {
		t.Errorf("UnencryptedPorts = %v, want none", params.UnencryptedPorts)
	}
	if params.ReadinessProbe == nil {
		t.Error("no readiness probe was configured")
	}
	if params.Env["OPENCODE_PASSWORD"] != "secret" {
		t.Error("the password must reach the sandbox through the environment")
	}
	// Secrets must never appear in the command line, where other
	// processes in the sandbox could read them from /proc.
	for _, arg := range params.Command {
		if arg == "secret" {
			t.Error("a secret leaked into the command")
		}
	}
	if params.Tags[TagSpov] != "1" {
		t.Errorf("Tags = %v, want the spov marker", params.Tags)
	}
}

// TestCreateParamsAlwaysSendsAllowlists covers the thing that cannot be
// retrofitted. Modal refuses to restrict a sandbox created with open
// network access, and an empty allowlist blocks all traffic rather than
// allowing it, so both allowlists must always be sent and "open" must be
// spelled as the allow-all entries.
func TestCreateParamsAlwaysSendsAllowlists(t *testing.T) {
	p := newTestProvider()

	t.Run("no policy sends allow-all, not empty", func(t *testing.T) {
		params, err := p.createParams(context.Background(), validSpec())
		if err != nil {
			t.Fatal(err)
		}
		if params.OutboundCIDRAllowlist == nil || params.OutboundDomainAllowlist == nil {
			t.Fatal("allowlists must always be sent so the policy stays adjustable")
		}
		if got := params.OutboundCIDRAllowlist.Entries; len(got) != 1 || got[0] != AllowAllCIDRs {
			t.Errorf("cidrs = %v, want [%s]; an empty allowlist blocks all egress", got, AllowAllCIDRs)
		}
		if got := params.OutboundDomainAllowlist.Entries; len(got) != 1 || got[0] != AllowAllDomains {
			t.Errorf("domains = %v, want [%s]", got, AllowAllDomains)
		}
	})

	t.Run("an open policy is sent as the allow-all entries", func(t *testing.T) {
		params, err := p.createParams(context.Background(), validSpec())
		if err != nil {
			t.Fatal(err)
		}
		if got := params.OutboundCIDRAllowlist.Entries; len(got) != 1 || got[0] != AllowAllCIDRs {
			t.Errorf("cidrs = %v; an open policy must not produce a blocking allowlist", got)
		}
		if got := params.OutboundDomainAllowlist.Entries; len(got) != 1 || got[0] != AllowAllDomains {
			t.Errorf("domains = %v", got)
		}
	})

	t.Run("a narrowed policy is carried through", func(t *testing.T) {
		spec := validSpec()
		spec.Egress = EgressAgent()
		spec.InboundCIDRs = []string{"203.0.113.0/24"}
		params, err := p.createParams(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if len(params.OutboundDomainAllowlist.Entries) != len(EgressAgent().Domains) {
			t.Errorf("domains = %v", params.OutboundDomainAllowlist.Entries)
		}
		// The agent preset allows nothing by IP, and an empty list is
		// what blocks it — not a nil one, which would mean open.
		if params.OutboundCIDRAllowlist == nil {
			t.Fatal("a nil cidr allowlist leaves the sandbox open")
		}
		if got := params.OutboundCIDRAllowlist.Entries; len(got) != 0 {
			t.Errorf("cidrs = %v, want none for the agent preset", got)
		}
		if len(params.InboundCIDRAllowlist) != 1 {
			t.Errorf("inbound = %v", params.InboundCIDRAllowlist)
		}
	})

	t.Run("network is never blocked outright", func(t *testing.T) {
		params, err := p.createParams(context.Background(), validSpec())
		if err != nil {
			t.Fatal(err)
		}
		if params.BlockNetwork {
			t.Error("BlockNetwork must stay false; the sandbox needs the model API and git")
		}
	})
}

func TestCreateParamsWithoutReadinessProbe(t *testing.T) {
	p := newTestProvider()
	spec := validSpec()
	spec.ReadyPort = 0

	params, err := p.createParams(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if params.ReadinessProbe != nil {
		t.Error("no readiness port was requested, but a probe was configured")
	}
}

func TestNewModalProviderRequiresAppName(t *testing.T) {
	_, err := NewModalProvider(context.Background(), ModalConfig{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "app name") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

// TestImageCacheShortCircuits proves image() returns a cached entry
// without touching the client, which is nil here: any call would panic.
func TestImageCacheShortCircuits(t *testing.T) {
	p := &ModalProvider{images: map[string]*modal.Image{"cached": {}}}

	img, err := p.image(context.Background(), "cached")
	if err != nil {
		t.Fatal(err)
	}
	if img == nil {
		t.Error("cached image was not returned")
	}
}

// TestCreateParamsCarriesTheNewFields: each key DL-029 added must reach
// the SDK's params, or a user configures something silently ignored.
//
// @decision(DL-029)
func TestCreateParamsCarriesTheNewFields(t *testing.T) {
	p := &ModalProvider{}
	spec := validSpec()
	spec.CPULimit = 2
	spec.MemoryLimitMiB = 4096
	spec.Regions = []string{"us-east-1", "eu-west-1"}
	spec.Cloud = "aws"
	spec.Workdir = "/workspace"
	spec.InboundCIDRs = []string{"203.0.113.0/24"}

	params, err := p.createParams(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if params.CPULimit != 2 {
		t.Errorf("cpu limit = %v", params.CPULimit)
	}
	if params.MemoryLimitMiB != 4096 {
		t.Errorf("memory limit = %d", params.MemoryLimitMiB)
	}
	if len(params.Regions) != 2 || params.Regions[0] != "us-east-1" {
		t.Errorf("regions = %v", params.Regions)
	}
	if params.Cloud != "aws" {
		t.Errorf("cloud = %q", params.Cloud)
	}
	if params.Workdir != "/workspace" {
		t.Errorf("workdir = %q", params.Workdir)
	}
	if len(params.InboundCIDRAllowlist) != 1 {
		t.Errorf("inbound = %v", params.InboundCIDRAllowlist)
	}
	// The caller's slices must not be shared with the SDK.
	spec.Regions[0] = "mutated"
	if params.Regions[0] == "mutated" {
		t.Error("the params alias the caller's regions")
	}
}

// TestModalCredentialsSet: a half-configured token is not a credential,
// and treating it as one would send an unauthenticated request instead
// of falling back to the ambient profile.
//
// @decision(DL-010)
func TestModalCredentialsSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    ModalCredentials
		want bool
	}{
		{"both", ModalCredentials{TokenID: "ak-1", TokenSecret: "as-1"}, true},
		{"id only", ModalCredentials{TokenID: "ak-1"}, false},
		{"secret only", ModalCredentials{TokenSecret: "as-1"}, false},
		{"neither", ModalCredentials{}, false},
		{"environment alone", ModalCredentials{Environment: "spov-hosted"}, false},
	} {
		if got := tc.c.Set(); got != tc.want {
			t.Errorf("%s: Set() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestModalThrottleWaitIsFinite: unset, the SDK waits out a rate limit
// indefinitely, which turns a throttled workspace into a hung request
// rather than a failed one.
//
// @decision(DL-010)
func TestModalThrottleWaitIsFinite(t *testing.T) {
	if ModalMaxThrottleWait <= 0 {
		t.Fatalf("ModalMaxThrottleWait = %v", ModalMaxThrottleWait)
	}
	if ModalMaxThrottleWait > time.Minute {
		t.Errorf("ModalMaxThrottleWait = %v, long enough to look like a hang", ModalMaxThrottleWait)
	}
}
