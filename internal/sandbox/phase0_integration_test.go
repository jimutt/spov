//go:build integration

package sandbox

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"

	"github.com/jimutt/spov/internal/prep"
)

// These are the phase-0 spikes of the control plane. They record no
// decision; each stays in the tree as the standing proof of a fact the
// control plane's decisions rest on. Both create billable
// sandboxes and terminate what they create.

// TestIntegrationIdleTimeoutCountsConnectTraffic answers the first
// question: does traffic through the connect endpoint reset IdleTimeout? Three
// sandboxes with IdleTimeout 2m run in parallel for 4½ minutes: a
// no-traffic control, a plain request every 30 s, and a held-open SSE
// stream. Requests count and an open stream does not, which is why the
// proxy heartbeats (DL-030); the test fails if either changes.
func TestIntegrationIdleTimeoutCountsConnectTraffic(t *testing.T) {
	const (
		idle    = 2 * time.Minute
		observe = idle*2 + 30*time.Second
	)
	p := newIntegrationProvider(t)

	cases := []struct {
		name    string
		traffic func(ctx context.Context, t *testing.T, c *http.Client, url string, cookie *http.Cookie, password string)
		wantDie bool
	}{
		{name: "no traffic", wantDie: true},
		{name: "one request every 30s", traffic: func(ctx context.Context, t *testing.T, c *http.Client, url string, cookie *http.Cookie, password string) {
			tick := time.NewTicker(30 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/info", nil)
					req.AddCookie(cookie)
					req.SetBasicAuth("opencode", password)
					resp, err := c.Do(req)
					if err != nil {
						t.Logf("request: %v", err)
						continue
					}
					resp.Body.Close()
				}
			}
		}},
		{name: "SSE stream held open", wantDie: true, traffic: func(ctx context.Context, t *testing.T, c *http.Client, url string, cookie *http.Cookie, password string) {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/event", nil)
			req.AddCookie(cookie)
			req.SetBasicAuth("opencode", password)
			resp, err := c.Do(req)
			if err != nil {
				t.Logf("sse: %v", err)
				return
			}
			defer resp.Body.Close()
			buf := make([]byte, 4096)
			for {
				if _, err := resp.Body.Read(buf); err != nil {
					t.Logf("sse ended: %v", err)
					return
				}
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), observe+4*time.Minute)
			defer cancel()

			sb, creds, password := startOpenCodeSandbox(t, ctx, p, "spov-idle", idle)
			cookie := &http.Cookie{Name: "_modal_connect_token", Value: creds.Token}
			client := &http.Client{Timeout: 0}

			trafficCtx, stopTraffic := context.WithCancel(ctx)
			defer stopTraffic()
			if tc.traffic != nil {
				go tc.traffic(trafficCtx, t, client, creds.URL, cookie, password)
			}

			start := time.Now()
			died := time.Duration(0)
			for time.Since(start) < observe {
				time.Sleep(15 * time.Second)
				code, err := sb.Poll(ctx, nil)
				if err != nil {
					t.Fatalf("poll: %v", err)
				}
				if code != nil {
					died = time.Since(start).Round(time.Second)
					t.Logf("sandbox exited with %d after %s", *code, died)
					break
				}
			}
			stopTraffic()

			switch {
			case tc.wantDie && died == 0:
				t.Errorf("still alive after %s with IdleTimeout %s: idle detection changed, so a heartbeat may no longer be needed", observe, idle)
			case tc.wantDie:
				t.Logf("died after %s: this traffic shape does not count as activity", died)
			case died != 0:
				t.Errorf("died after %s despite a request every 30s: connect requests no longer count as activity", died)
			default:
				t.Logf("alive after %s: connect requests count as activity", observe)
			}
		})
	}
}

// startOpenCodeSandbox boots the pinned OpenCode build with no exposed
// ports and mints a connect token for it, as DL-030 does.
func startOpenCodeSandbox(t *testing.T, ctx context.Context, p *ModalProvider, prefix string, idle time.Duration) (*modal.Sandbox, *modal.SandboxCreateConnectCredentials, string) {
	t.Helper()
	return startOpenCodeSandboxWith(t, ctx, p, prefix, idle, nil)
}

// startOpenCodeSandboxWith is startOpenCodeSandbox with files written
// into the container before the server starts, which is how a spike
// stands in for something the image would otherwise bake in.
func startOpenCodeSandboxWith(t *testing.T, ctx context.Context, p *ModalProvider, prefix string, idle time.Duration, files map[string]string) (*modal.Sandbox, *modal.SandboxCreateConnectCredentials, string) {
	t.Helper()
	return startOpenCodeSandboxEnv(t, ctx, p, prefix, idle, files, nil)
}

// startOpenCodeSandboxEnv is startOpenCodeSandboxWith with extra
// environment for the server.
func startOpenCodeSandboxEnv(t *testing.T, ctx context.Context, p *ModalProvider, prefix string, idle time.Duration, files, extraEnv map[string]string) (*modal.Sandbox, *modal.SandboxCreateConnectCredentials, string) {
	t.Helper()
	return startOpenCodeSandboxOn(t, ctx, p, nil, prefix, idle, files, extraEnv, nil)
}

// spikeConfigPath is OpenCode's global config file in a spike's sandbox.
const spikeConfigPath = "/root/.config/opencode/opencode.json"

// withSpikeModel gives a spike's OpenCode the model SPOV_SPIKE_MODEL
// names (`provider/model`, e.g. `openrouter/z-ai/glm-5.3-flash`) and that
// provider's key from this process's environment. Unset, OpenCode runs a
// turn on its own default, which works without a key only while OpenCode
// offers a free one. A spike that writes its own config keeps it.
func withSpikeModel(t *testing.T, env, files map[string]string) map[string]string {
	t.Helper()
	ref := os.Getenv("SPOV_SPIKE_MODEL")
	if ref == "" {
		return files
	}
	if _, ok := files[spikeConfigPath]; ok {
		return files
	}
	provider, _, err := prep.LookupModel(ref)
	if err != nil {
		t.Fatalf("SPOV_SPIKE_MODEL: %v", err)
	}
	key := os.Getenv(provider.KeyEnv)
	if key == "" {
		t.Fatalf("SPOV_SPIKE_MODEL names %s, whose key %s is not set", provider.ID, provider.KeyEnv)
	}
	env[provider.KeyEnv] = key
	out := maps.Clone(files)
	if out == nil {
		out = map[string]string{}
	}
	out[spikeConfigPath] = fmt.Sprintf(`{"model":%q}`, ref)
	return out
}

// startOpenCodeSandboxOn is startOpenCodeSandboxEnv booted from image
// (the base image when nil), with Modal's experimental create options.
func startOpenCodeSandboxOn(t *testing.T, ctx context.Context, p *ModalProvider, from *modal.Image, prefix string, idle time.Duration, files, extraEnv map[string]string, options map[string]any) (*modal.Sandbox, *modal.SandboxCreateConnectCredentials, string) {
	t.Helper()
	password := randomPassword(t)
	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	env := map[string]string{
		"OPENCODE_PASSWORD": password,
		"HOME":              "/root",
		"PATH":              "/root/.opencode/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	files = withSpikeModel(t, env, files)
	maps.Copy(env, extraEnv)
	// Seeded through the environment, not the command line: the content
	// is base64 so nothing in it can end a shell word, and the paths are
	// literals this file controls.
	var seed strings.Builder
	for i, path := range slices.Sorted(maps.Keys(files)) {
		key := fmt.Sprintf("SPOV_SEED_%d", i)
		env[key] = base64.StdEncoding.EncodeToString([]byte(files[path]))
		fmt.Fprintf(&seed, "mkdir -p %q; printf %%s \"$%s\" | base64 -d > %q; ", filepath.Dir(path), key, path)
	}
	spec := Spec{
		Name:  name,
		Image: testImage(),
		Command: []string{"bash", "-c",
			"set -e; " + seed.String() +
				"cd /root; exec /root/.opencode/bin/opencode serve --hostname 0.0.0.0 --port 4096"},
		Env:         env,
		Egress:      EgressOpen(),
		Ports:       []int{testPort},
		ReadyPort:   testPort,
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     15 * time.Minute,
		IdleTimeout: idle,
		Tags:        BuildTags(TagSet{Name: name, User: "integration-test", CreatedAt: time.Now()}),
	}
	image := from
	if image == nil {
		var err error
		if image, err = p.image(ctx, spec.Image); err != nil {
			t.Fatal(err)
		}
	}
	params, err := p.createParams(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	params.EncryptedPorts = nil
	params.ExperimentalOptions = options

	sb, err := p.client.Sandboxes.ExperimentalCreate(ctx, p.app, image, params)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		if _, err := sb.Terminate(context.WithoutCancel(ctx), nil); err != nil {
			t.Logf("cleanup terminate: %v", err)
		}
	})
	if err := sb.WaitUntilReady(ctx, readyTimeout, nil); err != nil {
		t.Fatalf("ready: %v", err)
	}
	creds, err := sb.CreateConnectToken(ctx, &modal.SandboxCreateConnectTokenParams{Port: testPort})
	if err != nil {
		t.Fatalf("connect token: %v", err)
	}
	return sb, creds, password
}

// TestIntegrationSnapshotPublish answers the second
// question: can a filesystem
// snapshot be published under a name, found again by that name, booted,
// and deleted? The structured-snapshot design rests on Image.Publish
// accepting a snapshot, which Modal does not document.
func TestIntegrationSnapshotPublish(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	boot := func(name string, image *modal.Image) *modal.Sandbox {
		t.Helper()
		params, err := p.createParams(ctx, Spec{
			Name:      name,
			Image:     testImage(),
			Command:   []string{"sleep", "infinity"},
			CPU:       0.5,
			MemoryMiB: 2048,
			Timeout:   10 * time.Minute,
			Tags:      BuildTags(TagSet{Name: name, User: "integration-test", CreatedAt: time.Now()}),
			Egress:    EgressOpen(),
		})
		if err != nil {
			t.Fatal(err)
		}
		sb, err := p.client.Sandboxes.ExperimentalCreate(ctx, p.app, image, params)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			if _, err := sb.Terminate(context.WithoutCancel(ctx), nil); err != nil {
				t.Logf("cleanup terminate %s: %v", name, err)
			}
		})
		return sb
	}
	exec := func(sb *modal.Sandbox, script string) string {
		t.Helper()
		res, err := (&modalHandle{sb: sb}).Exec(ctx, []string{"sh", "-c", script})
		if err != nil {
			t.Fatalf("exec %q: %v", script, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("exec %q: exit %d: %s%s", script, res.ExitCode, res.Stdout, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}

	base, err := p.image(ctx, testImage())
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UnixNano()
	marker := fmt.Sprintf("spov-snapshot-%d", stamp)
	first := boot(fmt.Sprintf("spov-snap-src-%d", stamp), base)
	exec(first, "mkdir -p /workspace && echo "+marker+" > /workspace/marker && echo ok")

	start := time.Now()
	snap, err := first.SnapshotFilesystem(ctx, &modal.SandboxSnapshotFilesystemParams{TTL: modal.NoExpiryTTL})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Logf("snapshot %s taken in %s", snap.ImageID, time.Since(start).Round(100*time.Millisecond))

	name := fmt.Sprintf("spov-spike-snapshot-%d", stamp)
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		if err := p.client.Images.Delete(context.WithoutCancel(ctx), snap.ImageID, nil); err != nil {
			t.Logf("cleanup delete image: %v", err)
		}
	})

	t.Run("publish accepts a snapshot", func(t *testing.T) {
		if err := snap.Publish(ctx, name, &modal.ImagePublishParams{Environment: p.env}); err != nil {
			t.Fatalf("publish: %v — a snapshot cannot be named; the structured-snapshot design does not hold", err)
		}
	})

	var found *modal.Image
	t.Run("found again by name", func(t *testing.T) {
		var err error
		found, err = p.client.Images.FromName(ctx, name, &modal.ImageFromNameParams{Environment: p.env})
		if err != nil {
			t.Fatalf("from name: %v", err)
		}
		t.Logf("resolved %s → %s", name, found.ImageID)
		if found.ImageID != snap.ImageID {
			t.Errorf("name resolves to %s, snapshot was %s", found.ImageID, snap.ImageID)
		}
	})
	if found == nil {
		return
	}

	t.Run("a sandbox booted from the name has the snapshot state", func(t *testing.T) {
		start := time.Now()
		second := boot(fmt.Sprintf("spov-snap-dst-%d", stamp), found)
		got := exec(second, "cat /workspace/marker")
		t.Logf("booted and read marker in %s", time.Since(start).Round(100*time.Millisecond))
		if got != marker {
			t.Errorf("marker = %q, want %q", got, marker)
		}
	})

	t.Run("delete leaves a dangling name that cannot boot", func(t *testing.T) {
		if err := p.client.Images.Delete(ctx, snap.ImageID, nil); err != nil {
			t.Fatalf("delete: %v", err)
		}
		deleted = true
		stale, err := p.client.Images.FromName(ctx, name, &modal.ImageFromNameParams{Environment: p.env})
		if err != nil {
			t.Logf("after delete, FromName: %v", err)
			return
		}
		// Deleting the image does not unpublish the name. What matters
		// for a snapshot index is whether the stale name can still boot.
		t.Logf("after delete the name still resolves to %s", stale.ImageID)
		params, err := p.createParams(ctx, Spec{
			Name: fmt.Sprintf("spov-snap-stale-%d", stamp), Image: testImage(),
			Command: []string{"true"}, CPU: 0.5, MemoryMiB: 2048, Timeout: 5 * time.Minute,
			Tags:   BuildTags(TagSet{Name: "stale", User: "integration-test", CreatedAt: time.Now()}),
			Egress: EgressOpen(),
		})
		if err != nil {
			t.Fatal(err)
		}
		sb, err := p.client.Sandboxes.ExperimentalCreate(ctx, p.app, stale, params)
		if err != nil {
			t.Logf("booting the stale name fails, as it should: %v", err)
			return
		}
		_, _ = sb.Terminate(context.WithoutCancel(ctx), nil)
		t.Error("a sandbox booted from a deleted image's name")
	})
}
