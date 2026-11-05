//go:build integration

package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// TestIntegrationImageDeleteSpike (DL-016) measures what deleting a published
// image does, for pruning variants: whether its name still resolves,
// whether a sandbox already running from it keeps working, and whether
// the same recipe rebuilds from cache. Set SPOV_PRUNE_SPIKE=1; it
// builds one tiny image.
func TestIntegrationImageDeleteSpike(t *testing.T) {
	if os.Getenv("SPOV_PRUNE_SPIKE") != "1" {
		t.Skip("set SPOV_PRUNE_SPIKE=1")
	}
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	stamp := time.Now().UTC().Format("20060102150405")
	recipe := func() *modal.Image {
		return p.client.Images.FromRegistry("debian:trixie-slim", nil).
			DockerfileCommands([]string{"RUN echo spov-prune-spike-" + stamp + " > /spike"}, nil)
	}
	name := "spov-prune-spike-" + stamp
	start := time.Now()
	built, err := recipe().Build(ctx, p.app, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := built.Publish(ctx, name, &modal.ImagePublishParams{Environment: p.env}); err != nil {
		t.Fatal(err)
	}
	t.Logf("RESULT built and published %s (%s) in %s", name, built.ImageID, time.Since(start).Round(time.Second))

	byName, err := p.client.Images.FromName(ctx, name, &modal.ImageFromNameParams{Environment: p.env})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RESULT FromName gives %s (same as the build: %v)", byName.ImageID, byName.ImageID == built.ImageID)

	sb, err := p.client.Sandboxes.ExperimentalCreate(ctx, p.app, byName, &modal.SandboxCreateParams{
		Command: []string{"sleep", "infinity"}, Timeout: 10 * time.Minute,
		Tags: map[string]string{"spov-prune-spike": stamp},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = sb.Terminate(context.WithoutCancel(ctx), nil) })
	h := &modalHandle{sb: sb}
	exec := func(label string) {
		res, err := h.Exec(ctx, []string{"cat", "/spike"})
		if err != nil {
			t.Logf("RESULT %s: exec failed: %v", label, err)
			return
		}
		t.Logf("RESULT %s: exec exit %d, %q", label, res.ExitCode, strings.TrimSpace(res.Stdout))
	}
	exec("before the delete")

	start = time.Now()
	if err := p.client.Images.Delete(ctx, built.ImageID, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	t.Logf("RESULT deleted %s in %s", built.ImageID, time.Since(start).Round(time.Millisecond))

	exec("running sandbox, after the delete")
	if _, err := p.client.Images.FromName(ctx, name, &modal.ImageFromNameParams{Environment: p.env}); err != nil {
		t.Logf("RESULT FromName after the delete: %v", err)
	} else {
		t.Log("RESULT FromName after the delete still resolves")
	}
	if ok, err := p.HasImage(ctx, name); err == nil {
		t.Logf("RESULT HasImage after the delete: %v", ok)
	} else {
		t.Logf("RESULT HasImage after the delete: error %v", err)
	}
	if err := p.client.Images.Delete(ctx, built.ImageID, nil); err != nil {
		t.Logf("RESULT a second delete: %v", err)
	}

	sb2, err := p.client.Sandboxes.ExperimentalCreate(ctx, p.app, byName, &modal.SandboxCreateParams{
		Command: []string{"sleep", "infinity"}, Timeout: 5 * time.Minute,
		Tags: map[string]string{"spov-prune-spike": stamp + "-2"},
	})
	if err != nil {
		t.Logf("RESULT a new sandbox from the deleted image: %v", err)
	} else {
		t.Log("RESULT a new sandbox from the deleted image started")
		_, _ = sb2.Terminate(context.WithoutCancel(ctx), nil)
	}

	start = time.Now()
	again, err := recipe().Build(ctx, p.app, nil)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	t.Logf("RESULT the same recipe rebuilt as %s (same id: %v) in %s", again.ImageID, again.ImageID == built.ImageID, time.Since(start).Round(time.Millisecond))
	if err := again.Publish(ctx, name, &modal.ImagePublishParams{Environment: p.env}); err != nil {
		t.Logf("RESULT republishing the name: %v", err)
	} else {
		t.Log("RESULT republished the name")
	}
	if err := p.client.Images.Delete(ctx, again.ImageID, nil); err != nil {
		t.Logf("RESULT cleanup delete: %v", err)
	}
}
