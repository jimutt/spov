//go:build integration

package sandbox

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/credproxy"
)

// TestIntegrationScenarioKillSidecar terminates one running session's
// credential proxy sidecar, so a combined scenario can watch the
// reconciler recreate it (DL-022 step 3). The product has no way to do
// this and must not gain one; this is the scenario's tool, not a test of
// spov. Opt in with the sandbox's ID:
//
//	SPOV_SCENARIO_KILL_SIDECAR=sb-... MODAL_ENVIRONMENT=spov-scenarios \
//	  go test -tags integration ./internal/sandbox/ -run ScenarioKillSidecar -v
//
// @decision(DL-022)
func TestIntegrationScenarioKillSidecar(t *testing.T) {
	id := os.Getenv("SPOV_SCENARIO_KILL_SIDECAR")
	if id == "" {
		t.Skip("set SPOV_SCENARIO_KILL_SIDECAR to a sandbox ID to terminate its proxy sidecar")
	}
	if !isSandboxID(id) {
		t.Fatalf("%q is not a sandbox ID (sb-...)", id)
	}
	env := os.Getenv("MODAL_ENVIRONMENT")
	if env == "" {
		t.Fatal("set MODAL_ENVIRONMENT to the scenario's environment, so nothing is touched elsewhere")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, err := NewModalProvider(ctx, ModalConfig{AppName: "spov", Environment: env})
	if err != nil {
		t.Fatalf("connect to Modal: %v", err)
	}
	h, err := p.Get(ctx, id)
	if err != nil {
		t.Fatalf("find %s: %v", id, err)
	}
	sb := h.(*modalHandle).sb
	tags, err := sb.GetTags(ctx, nil)
	if err != nil {
		t.Fatalf("tags of %s: %v", id, err)
	}
	// A second death ends the session, and a sandbox found by ID may be
	// another environment's: only a spov sandbox whose sidecar has never
	// been recreated is touched.
	switch {
	case tags[TagSpov] != "1":
		t.Fatalf("%s is not a spov sandbox", id)
	case tags[TagProxy] != "1":
		t.Fatalf("%s carries no %q tag, so it has no sidecar to kill", id, TagProxy)
	case tags[TagProxyRecreated] != "":
		t.Fatalf("%s's sidecar was recreated once already; killing it again ends the session", id)
	}
	c, err := sb.ExperimentalSidecars.Get(ctx, credproxy.SidecarName, nil)
	if err != nil {
		t.Fatalf("the %s sidecar of %s: %v", credproxy.SidecarName, id, err)
	}
	code, err := c.Terminate(ctx, nil)
	if err != nil {
		t.Fatalf("terminate the sidecar: %v", err)
	}
	t.Logf("terminated the %s sidecar of %s (exit %d) at %s; tags were %v", credproxy.SidecarName, id, code,
		time.Now().UTC().Format(time.RFC3339), tags)
}
