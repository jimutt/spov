//go:build integration

package session

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
)

// These tests create real, billable Modal sandboxes. Run them with:
//
//	go test -tags integration ./internal/session/ -run Integration -v
//
// They need Modal credentials (~/.modal.toml or MODAL_TOKEN_*) and the
// published base image; set SPOV_TEST_IMAGE to override the default.

const integrationApp = "spov-test"

// integrationVersion is the pin.
const integrationVersion = config.DefaultOpenCodeVersion

// integrationImage names the published base image these tests boot. It
// is derived from ImageSpec rather than written out, so a version bump
// cannot leave it pointing at an image nobody builds.
func integrationImage() string {
	if img := os.Getenv("SPOV_TEST_IMAGE"); img != "" {
		return img
	}
	return sandbox.ImageSpec{OpenCodeVersion: integrationVersion}.Name()
}

// TestIntegrationAttachRecoversPassword is the claim DL-025 rests on:
// that a session's password can be read back out of a running sandbox by
// a process that never launched it. It launches a real session, throws
// away the password, and reattaches with only the sandbox name.
func TestIntegrationAttachRecoversPassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	provider, err := sandbox.NewModalProvider(ctx, sandbox.ModalConfig{AppName: integrationApp})
	if err != nil {
		t.Fatalf("connect to Modal (need credentials in ~/.modal.toml): %v", err)
	}

	name := "spov-attach-" + time.Now().UTC().Format("20060102-150405")
	launcher := &Launcher{Sandboxes: provider, Progress: testWriter{t}}

	sess, err := launcher.Launch(ctx, Options{
		Name:  name,
		User:  "integration-test",
		Image: integrationImage(),
		Selections: []git.Selection{{Platform: publicPlatform{}, Repos: []git.Repo{{
			Owner: "modal-labs", Name: "libmodal",
			CloneURL: "https://github.com/modal-labs/libmodal.git",
		}}}},
		Port:        opencode.DefaultPort,
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     10 * time.Minute,
		IdleTimeout: 2 * time.Minute,
		Egress:      sandbox.EgressOpen(),
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// Cleanup uses a detached context so it still runs if ctx is done.
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := sess.Close(c); err != nil {
			t.Errorf("terminate: %v", err)
		}
	})

	launched := sess.Password
	if launched == "" {
		t.Fatal("the launcher produced no password")
	}

	// Attach as a process that knows only the sandbox name: no stored
	// password, recovery straight from the sandbox.
	attacher := &Attacher{
		Sandboxes: provider,
		Progress:  testWriter{t},
		RecoverPassword: func(ctx context.Context, h sandbox.Handle, _ sandbox.Info) (string, error) {
			return PasswordFromSandbox(ctx, h)
		},
	}

	start := time.Now()
	attached, err := attacher.Attach(ctx, AttachOptions{Ref: name, Port: opencode.DefaultPort})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Logf("attached in %s", time.Since(start).Round(time.Millisecond))

	if attached.Password != launched {
		t.Error("the recovered password does not match the one the session was launched with")
	}
	if !attached.Health.Healthy {
		t.Errorf("the server is not healthy: %+v", attached.Health)
	}
	if attached.Health.Version != integrationVersion {
		t.Errorf("server version = %q, want %q", attached.Health.Version, integrationVersion)
	}
	if attached.URL == "" {
		t.Error("no tunnel URL")
	}
	if attached.Info.ID != sess.Info.ID {
		t.Errorf("attached to %s, want %s", attached.Info.ID, sess.Info.ID)
	}

	// Attaching must leave the sandbox running: it is the way back into a
	// session, not a way out of one.
	info, err := attached.Handle.Info(ctx)
	if err != nil {
		t.Fatalf("Info after attach: %v", err)
	}
	if info.State != sandbox.StateRunning {
		t.Errorf("the sandbox is %s after attaching, want running", info.State)
	}
}

// TestIntegrationAttachRejectsStoppedSandbox verifies against real Modal
// that a terminated sandbox is refused up front rather than hanging on a
// health check.
func TestIntegrationAttachRejectsStoppedSandbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	provider, err := sandbox.NewModalProvider(ctx, sandbox.ModalConfig{AppName: integrationApp})
	if err != nil {
		t.Fatalf("connect to Modal: %v", err)
	}

	name := "spov-attach-stopped-" + time.Now().UTC().Format("20060102-150405")
	h, err := provider.Create(ctx, sandbox.Spec{
		Name:        name,
		Image:       integrationImage(),
		Command:     []string{"sleep", "300"},
		Ports:       []int{opencode.DefaultPort},
		CPU:         0.25,
		MemoryMiB:   1024,
		Timeout:     5 * time.Minute,
		IdleTimeout: 2 * time.Minute,
		Tags:        sandbox.BuildTags(sandbox.TagSet{Name: name, User: "integration-test", CreatedAt: time.Now()}),
		Egress:      sandbox.EgressOpen(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Terminate up front; the cleanup is a belt-and-braces second attempt.
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = h.Terminate(c)
	})
	info, err := h.Info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if err := h.Terminate(ctx); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	// Resolved by ID, not by name: Modal's by-name lookup does not return
	// terminated sandboxes at all, so only the ID path reaches the state
	// check this test is about.
	attacher := &Attacher{Sandboxes: provider, HealthTimeout: 30 * time.Second}
	start := time.Now()
	_, err = attacher.Attach(ctx, AttachOptions{Ref: info.ID, Port: opencode.DefaultPort, Password: "irrelevant"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error attaching to a stopped sandbox")
	}
	t.Logf("refused in %s: %v", elapsed.Round(time.Millisecond), err)
	if !errors.Is(err, ErrStopped) {
		t.Errorf("err = %v, want ErrStopped", err)
	}
	// The point of the up-front state check is that it does not wait for
	// a health timeout that can never succeed.
	if elapsed > 20*time.Second {
		t.Errorf("took %s to refuse a stopped sandbox; the state check is not running first", elapsed)
	}
}

// testWriter routes progress lines into the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// publicPlatform bootstraps a public repository with no credentials,
// which is all these tests need: they are about attaching, not about git.
type publicPlatform struct{}

func (publicPlatform) Name() string { return "github-public" }
func (publicPlatform) Host() string { return "github.com" }
func (publicPlatform) Kind() string { return "github" }
func (publicPlatform) ListRepos(context.Context) ([]git.Repo, error) {
	return nil, nil
}
func (publicPlatform) IssueSandboxToken(context.Context, []git.Repo, time.Duration) (git.Token, error) {
	return git.Token{}, nil
}
func (publicPlatform) TokenScope() git.Scope {
	return git.Scope{Description: "none: public repositories only"}
}

// ErrUnsupported rather than a nil answer: this platform holds no
// credential, so it cannot ask a forge anything. Returning nil, nil
// would claim the query ran and found no pull request, which is gate
// 2's "the job did not deliver" instead of "spov could not check".
//
// @decision(DL-050)
func (publicPlatform) FindPullRequest(context.Context, git.RepoRef, string) (*git.PullRequest, error) {
	return nil, git.ErrUnsupported
}
