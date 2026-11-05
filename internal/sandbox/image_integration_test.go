//go:build integration

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestIntegrationEnsureImage builds and publishes the spov base image,
// then boots a sandbox from it and checks the toolchain the entrypoint
// depends on is actually present.
func TestIntegrationEnsureImage(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	spec := ImageSpec{OpenCodeVersion: testOpenCodeVersion()}

	start := time.Now()
	name, err := p.EnsureImage(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	t.Logf("image %q ready after %s", name, time.Since(start).Round(100*time.Millisecond))

	// A second call must resolve the published image rather than rebuild.
	start = time.Now()
	again, err := p.EnsureImage(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if again != name {
		t.Errorf("second call returned %q, want %q", again, name)
	}
	t.Logf("reuse took %s", time.Since(start).Round(time.Millisecond))

	sb, err := p.Create(ctx, Spec{
		Image:       name,
		Command:     []string{"sleep", "infinity"},
		Ports:       []int{testPort},
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     10 * time.Minute,
		IdleTimeout: 2 * time.Minute,
		Tags:        BuildTags(TagSet{User: "integration-test", CreatedAt: time.Now()}),
		Egress:      EgressOpen(),
	})
	if err != nil {
		t.Fatalf("create from the built image: %v", err)
	}
	t.Cleanup(func() { _ = sb.Terminate(context.WithoutCancel(ctx)) })

	checks := []struct {
		name    string
		command string
		want    string
	}{
		{"ripgrep is preinstalled", "command -v rg", "/rg"},
		{"git is present", "git --version", "git version"},
		{"bash is present", "bash --version", "GNU bash"},
		{"ca-certificates are present", "ls /etc/ssl/certs/ca-certificates.crt", "ca-certificates.crt"},
		{"opencode is baked in", "ls -l /root/.opencode/bin/opencode", "opencode"},
		{"workspace exists", "test -d /workspace && echo yes", "yes"},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			res, err := sb.Exec(ctx, []string{"bash", "-c", c.command})
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != 0 || !strings.Contains(res.Stdout, c.want) {
				t.Errorf("%q: exit=%d stdout=%q stderr=%q", c.command, res.ExitCode, res.Stdout, res.Stderr)
			}
		})
	}

	t.Run("the baked binary reports the pinned version", func(t *testing.T) {
		// Run in place, from the lazily loaded image layer, as the
		// entrypoint runs it.
		//
		// @decision(DL-032)
		res, err := sb.Exec(ctx, []string{"bash", "-c", "/root/.opencode/bin/opencode --version"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Stdout, testOpenCodeVersion()) {
			t.Errorf("version = %q / %q, want %s", res.Stdout, res.Stderr, testOpenCodeVersion())
		}
	})
}
