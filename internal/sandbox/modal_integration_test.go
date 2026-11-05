//go:build integration

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode/contract"
)

// These tests create real, billable Modal sandboxes. Run them with:
//
//	go test -tags integration ./internal/sandbox/ -run Integration -v
//
// They need Modal credentials (~/.modal.toml or MODAL_TOKEN_*) and a
// published image; set SPOV_TEST_IMAGE to override the default.

const (
	testAppName = "spov-test"
	testPort    = 4096
)

// testOpenCodeVersion is the build under test: the env override if set,
// which is how a spike is re-run against a newer build without moving
// the pin, else the pin. The pin is read as the contract's version,
// which a unit test holds equal to it, because internal/config imports
// this package.
//
// @decision(DL-035)
func testOpenCodeVersion() string {
	if v := os.Getenv("SPOV_TEST_OPENCODE_VERSION"); v != "" {
		return v
	}
	return contract.Version()
}

// testImage names the published base image these tests boot. It is
// derived from ImageSpec rather than written out, so a version bump
// cannot leave it pointing at an image nobody builds — which is what a
// hardcoded name from the phase-0 spike did.
func testImage() string {
	if img := os.Getenv("SPOV_TEST_IMAGE"); img != "" {
		return img
	}
	return ImageSpec{OpenCodeVersion: testOpenCodeVersion()}.Name()
}

func newIntegrationProvider(t *testing.T) *ModalProvider {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p, err := NewModalProvider(ctx, ModalConfig{AppName: testAppName})
	if err != nil {
		t.Fatalf("connect to Modal (need credentials in ~/.modal.toml): %v", err)
	}
	return p
}

func randomPassword(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// TestIntegrationSandboxLifecycle covers the path spov depends on:
// create, become ready, serve over an authenticated tunnel, terminate.
func TestIntegrationSandboxLifecycle(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	password := randomPassword(t)
	name := fmt.Sprintf("spov-test-%d", time.Now().UnixNano())

	spec := Spec{
		Name:  name,
		Image: testImage(),
		Command: []string{"bash", "-c",
			"set -e; cd /root; exec /root/.opencode/bin/opencode serve --hostname 0.0.0.0 --port 4096"},
		Env: map[string]string{
			"OPENCODE_PASSWORD": password,
			"HOME":              "/root",
			"PATH":              "/root/.opencode/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
		Egress:      EgressOpen(),
		Ports:       []int{testPort},
		ReadyPort:   testPort,
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     15 * time.Minute,
		IdleTimeout: 5 * time.Minute,
		Tags:        BuildTags(TagSet{Name: name, User: "integration-test", CreatedAt: time.Now()}),
	}

	start := time.Now()
	h, err := p.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Terminate even if the test fails, so a failure never leaks a
	// billable sandbox.
	t.Cleanup(func() {
		if err := h.Terminate(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("cleanup terminate: %v", err)
		}
	})
	t.Logf("ready after %s", time.Since(start).Round(100*time.Millisecond))

	info, err := h.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != StateRunning {
		t.Errorf("state = %q, want running", info.State)
	}
	if info.Tags[TagSpov] != "1" {
		t.Errorf("tags = %v", info.Tags)
	}
	if info.Age(time.Now()) <= 0 {
		t.Error("age was not derived from the created_at tag")
	}

	tunnels, err := h.Tunnels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	url := tunnels[testPort]
	if !strings.HasPrefix(url, "https://") {
		t.Fatalf("tunnel URL %q is not HTTPS", url)
	}

	t.Run("health endpoint requires the password", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/info", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated request returned %d, want 401", resp.StatusCode)
		}
	})

	t.Run("health endpoint answers with the password", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/info", nil)
		req.SetBasicAuth("opencode", password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("authenticated request returned %d, want 200", resp.StatusCode)
		}
	})

	// The sandbox is useless without egress: it must reach the model API
	// and git hosts. An empty allowlist blocks all traffic, so this is the
	// check that catches a mis-specified network policy.
	t.Run("egress works from inside the sandbox", func(t *testing.T) {
		res, err := h.Exec(ctx, []string{"bash", "-c",
			"curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://api.github.com"})
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 0 || !strings.Contains(res.Stdout, "200") {
			t.Errorf("outbound HTTPS failed: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
		}
	})

	t.Run("exec runs inside the sandbox", func(t *testing.T) {
		res, err := h.Exec(ctx, []string{"bash", "-c", "echo spov-ok"})
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 0 || !strings.Contains(res.Stdout, "spov-ok") {
			t.Errorf("exec result = %+v", res)
		}
	})

	t.Run("list finds it by tag", func(t *testing.T) {
		infos, err := p.List(ctx, OwnedTags())
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range infos {
			if i.ID == info.ID {
				return
			}
		}
		t.Errorf("sandbox %s not in the tag-filtered list of %d", info.ID, len(infos))
	})

	t.Run("get resolves it by id", func(t *testing.T) {
		got, err := p.Get(ctx, info.ID)
		if err != nil {
			t.Fatal(err)
		}
		gotInfo, err := got.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if gotInfo.ID != info.ID {
			t.Errorf("id = %q, want %q", gotInfo.ID, info.ID)
		}
	})
}

func TestIntegrationGetUnknownSandbox(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// `spov attach` and `spov stop` branch on ErrNotFound, so the
	// sentinel matters more than the message. A malformed reference and a
	// well-formed but stale one must both reach it.
	refs := map[string]string{
		"malformed id":          "sb-doesnotexist000000000",
		"well-formed but stale": "sb-00000000000000000000AA",
		"unknown session name":  "spov-test-no-such-session",
	}
	for name, ref := range refs {
		t.Run(name, func(t *testing.T) {
			_, err := p.Get(ctx, ref)
			if err == nil {
				t.Fatalf("Get(%q) succeeded for a sandbox that does not exist", ref)
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestIntegrationCreateFailureTerminates checks that a sandbox whose
// entrypoint never becomes ready is torn down rather than left billing.
func TestIntegrationCreateFailureTerminates(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	spec := Spec{
		Image:       testImage(),
		Command:     []string{"bash", "-c", "echo failing on purpose >&2; exit 1"},
		Ports:       []int{testPort},
		ReadyPort:   testPort,
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     5 * time.Minute,
		IdleTimeout: 1 * time.Minute,
		Tags:        BuildTags(TagSet{User: "integration-test", CreatedAt: time.Now()}),
		Egress:      EgressOpen(),
	}

	_, err := p.Create(ctx, spec)
	if err == nil {
		t.Fatal("expected create to fail when the entrypoint exits")
	}
	if !strings.Contains(err.Error(), "never became ready") {
		t.Fatalf("error = %v", err)
	}
	// v2 exposes no stdio, so the exit code is the only diagnostic
	// available; without it the user cannot tell a crash from a slow boot.
	if !strings.Contains(err.Error(), "entrypoint exited with code") {
		t.Errorf("error %q does not report the entrypoint exit code", err)
	}
	// A failed cleanup must be shouted about, not folded into the same
	// message shape as a successful one.
	if strings.Contains(err.Error(), "still billing") {
		t.Errorf("the sandbox was not cleaned up: %v", err)
	}
	t.Logf("failure reported as: %v", err)
}
