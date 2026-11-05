//go:build integration

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestIntegrationForgeCLIs builds the image with the forge CLIs and
// checks both are present and runnable.
//
// @decision(DL-032)
func TestIntegrationForgeCLIs(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	start := time.Now()
	image, err := p.EnsureImage(ctx, ImageSpec{OpenCodeVersion: testOpenCodeVersion()})
	if err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	t.Logf("image %q ready after %s", image, time.Since(start).Round(time.Second))

	sb, err := p.Create(ctx, Spec{
		Image:       image,
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
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = sb.Terminate(context.WithoutCancel(ctx)) })

	checks := []struct {
		name    string
		command string
		want    string
	}{
		// tea prints "Version: 0.15.1\tgolang: ...", gh prints
		// "gh version 2.98.0 (...)", so assert on the pinned version
		// rather than a phrase.
		{"tea is installed at the pinned version", "tea --version", TeaVersion},
		{"gh is installed at the pinned version", "gh --version", GhVersion},
		{"tea is on PATH", "command -v tea", "/usr/local/bin/tea"},
		{"gh is on PATH", "command -v gh", "/usr/local/bin/gh"},
		// @decision(DL-032)
		{"glab is installed at the pinned version", "glab --version", GlabVersion},
		{"glab is on PATH", "command -v glab", "/usr/local/bin/glab"},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			res, err := sb.Exec(ctx, []string{"bash", "-c", c.command})
			if err != nil {
				t.Fatal(err)
			}
			out := res.Stdout + res.Stderr
			if res.ExitCode != 0 || !strings.Contains(strings.ToLower(out), strings.ToLower(c.want)) {
				t.Errorf("%q: exit=%d out=%q", c.command, res.ExitCode, out)
			}
		})
	}
}
