//go:build integration

// Phase-2 spike 3: does a Modal client built from explicit credentials
// (DL-010) work in a container that has no ~/.modal.toml?
//
//	go test -tags integration ./internal/sandbox/ -run Phase2 -v
//
// The first two tests are free: they only construct clients. The last one
// creates a billable sandbox and terminates it via t.Cleanup.
package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modal-labs/modal-client/go"
)

// creds returns service-user credentials from the environment, or the
// ambient profile's, so the spike can run before one is provisioned.
func creds(t *testing.T) (id, secret, env string) {
	t.Helper()
	id = os.Getenv("SPOV_SPIKE_MODAL_TOKEN_ID")
	secret = os.Getenv("SPOV_SPIKE_MODAL_TOKEN_SECRET")
	if id == "" || secret == "" {
		id = os.Getenv("MODAL_TOKEN_ID")
		secret = os.Getenv("MODAL_TOKEN_SECRET")
	}
	if id == "" || secret == "" {
		t.Skip("set SPOV_SPIKE_MODAL_TOKEN_ID/_SECRET (or MODAL_TOKEN_ID/_SECRET) to run")
	}
	return id, secret, os.Getenv("MODAL_ENVIRONMENT")
}

// TestPhase2ExplicitCredentialsNoConfigFile is the spike's core question.
// It hides both the config file and the home directory, which is what the
// container looks like, and asserts the client still works.
func TestPhase2ExplicitCredentialsNoConfigFile(t *testing.T) {
	id, secret, env := creds(t)

	// A directory with no .modal.toml, named as HOME. readConfigFile
	// resolves the path through os.UserHomeDir, so an empty HOME is the
	// interesting case and gets its own test below.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MODAL_CONFIG_PATH", "")
	// Ambient env credentials must not be what makes this pass.
	t.Setenv("MODAL_TOKEN_ID", "")
	t.Setenv("MODAL_TOKEN_SECRET", "")
	t.Setenv("MODAL_PROFILE", "")

	wait := 30 * time.Second
	client, err := modal.NewClientWithOptions(&modal.ClientParams{
		TokenID:         id,
		TokenSecret:     secret,
		Environment:     env,
		MaxThrottleWait: &wait,
	})
	if err != nil {
		t.Fatalf("NewClientWithOptions with no config file: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	// Constructing a client does not talk to Modal, so make a real call.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	app, err := client.Apps.FromName(ctx, testAppName, &modal.AppFromNameParams{
		CreateIfMissing: true,
		Environment:     env,
	})
	if err != nil {
		t.Fatalf("Apps.FromName with explicit credentials: %v", err)
	}
	t.Logf("resolved app %s with no config file present", app.AppID)
}

// TestPhase2NoHomeDirectory is the distroless case: the nonroot user has
// no home and HOME may be unset. os.UserHomeDir returns an error then, and
// readConfigFile propagates it — before the explicit credentials in
// ClientParams are ever applied. If this fails, DL-010 must set HOME in
// the image or the provider must pass a Config.
func TestPhase2NoHomeDirectory(t *testing.T) {
	id, secret, env := creds(t)

	t.Setenv("HOME", "")
	t.Setenv("MODAL_TOKEN_ID", "")
	t.Setenv("MODAL_TOKEN_SECRET", "")
	t.Setenv("MODAL_PROFILE", "")

	wait := 30 * time.Second
	client, err := modal.NewClientWithOptions(&modal.ClientParams{
		TokenID:         id,
		TokenSecret:     secret,
		Environment:     env,
		MaxThrottleWait: &wait,
	})
	if err != nil {
		t.Fatalf("NewClientWithOptions with HOME unset: %v\n"+
			"DL-010 must then set HOME in the image or point MODAL_CONFIG_PATH at a file", err)
	}
	t.Cleanup(func() { client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := client.Apps.FromName(ctx, testAppName, &modal.AppFromNameParams{
		CreateIfMissing: true,
		Environment:     env,
	}); err != nil {
		t.Fatalf("Apps.FromName with HOME unset: %v", err)
	}
	t.Log("client works with no home directory at all")
}

// TestPhase2BadCredentialsFailClearly checks the failure mode an operator
// meets with a mistyped token: it must be an authentication error on first
// use, not a hang or a confusing config error.
func TestPhase2BadCredentialsFailClearly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MODAL_TOKEN_ID", "")
	t.Setenv("MODAL_TOKEN_SECRET", "")
	t.Setenv("MODAL_PROFILE", "")

	wait := 10 * time.Second
	client, err := modal.NewClientWithOptions(&modal.ClientParams{
		TokenID:         "ak-0000000000000000000000",
		TokenSecret:     "as-0000000000000000000000",
		MaxThrottleWait: &wait,
	})
	if err != nil {
		t.Logf("construction refused bad credentials outright: %v", err)
		return
	}
	t.Cleanup(func() { client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	start := time.Now()
	_, err = client.Apps.FromName(ctx, testAppName, &modal.AppFromNameParams{})
	if err == nil {
		t.Fatal("bad credentials were accepted")
	}
	t.Logf("bad credentials rejected after %s: %v", time.Since(start).Round(time.Millisecond), err)
	if ctx.Err() != nil {
		t.Fatal("bad credentials hung until the deadline rather than failing")
	}
}

// TestPhase2ProviderWithExplicitCredentials exercises the shape DL-010
// gives NewModalProvider: credentials passed in, nothing ambient read.
// It creates a **billable** sandbox and terminates it in cleanup.
func TestPhase2ProviderWithExplicitCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a billable sandbox")
	}
	id, secret, env := creds(t)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("MODAL_TOKEN_ID", "")
	t.Setenv("MODAL_TOKEN_SECRET", "")
	t.Setenv("MODAL_PROFILE", "")

	wait := 30 * time.Second
	client, err := modal.NewClientWithOptions(&modal.ClientParams{
		TokenID:         id,
		TokenSecret:     secret,
		Environment:     env,
		MaxThrottleWait: &wait,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	app, err := client.Apps.FromName(ctx, testAppName, &modal.AppFromNameParams{
		CreateIfMissing: true,
		Environment:     env,
	})
	if err != nil {
		t.Fatalf("app: %v", err)
	}

	image := client.Images.FromRegistry("alpine:3.20", nil)
	sb, err := client.Sandboxes.Create(ctx, app, image, &modal.SandboxCreateParams{
		Command: []string{"sleep", "60"},
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	// Cleanup runs with a context that outlives a cancelled test context,
	// so a billable sandbox is never left behind.
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if _, err := sb.Terminate(c, nil); err != nil {
			t.Errorf("terminate %s: %v", sb.SandboxID, err)
		}
	})
	t.Logf("created and will terminate sandbox %s using a service-user client", sb.SandboxID)
}

// TestPhase2NoHomeWithConfigPath is the remedy for the failure above:
// MODAL_CONFIG_PATH is consulted before the homedir lookup, so pointing it
// at a file (even an empty one) keeps a HOME-less container working.
// ClientParams.Config cannot serve this purpose — its type is unexported.
func TestPhase2NoHomeWithConfigPath(t *testing.T) {
	id, secret, env := creds(t)

	empty := filepath.Join(t.TempDir(), "modal.toml")
	if err := os.WriteFile(empty, []byte(""), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("HOME", "")
	t.Setenv("MODAL_CONFIG_PATH", empty)
	t.Setenv("MODAL_TOKEN_ID", "")
	t.Setenv("MODAL_TOKEN_SECRET", "")
	t.Setenv("MODAL_PROFILE", "")

	wait := 30 * time.Second
	client, err := modal.NewClientWithOptions(&modal.ClientParams{
		TokenID:         id,
		TokenSecret:     secret,
		Environment:     env,
		MaxThrottleWait: &wait,
	})
	if err != nil {
		t.Fatalf("NewClientWithOptions with MODAL_CONFIG_PATH set: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := client.Apps.FromName(ctx, testAppName, &modal.AppFromNameParams{
		CreateIfMissing: true,
		Environment:     env,
	}); err != nil {
		t.Fatalf("Apps.FromName: %v", err)
	}
	t.Log("MODAL_CONFIG_PATH at an empty file makes a HOME-less container work")
}

// TestPhase2HomeSetToNonexistent is the other remedy: HOME pointing at a
// path that does not exist. readConfigFile treats a missing file as an
// empty config, so this should work and is the cheaper fix in an image.
func TestPhase2HomeSetToNonexistent(t *testing.T) {
	id, secret, env := creds(t)

	t.Setenv("HOME", "/nonexistent")
	t.Setenv("MODAL_CONFIG_PATH", "")
	t.Setenv("MODAL_TOKEN_ID", "")
	t.Setenv("MODAL_TOKEN_SECRET", "")
	t.Setenv("MODAL_PROFILE", "")

	wait := 30 * time.Second
	client, err := modal.NewClientWithOptions(&modal.ClientParams{
		TokenID:         id,
		TokenSecret:     secret,
		Environment:     env,
		MaxThrottleWait: &wait,
	})
	if err != nil {
		t.Fatalf("NewClientWithOptions with HOME=/nonexistent: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := client.Apps.FromName(ctx, testAppName, &modal.AppFromNameParams{
		CreateIfMissing: true,
		Environment:     env,
	}); err != nil {
		t.Fatalf("Apps.FromName: %v", err)
	}
	t.Log("HOME at a nonexistent path is enough; the missing file is treated as empty config")
}
