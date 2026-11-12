package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
)

const attachPort = 4096

// runningSandbox seeds the fake provider with one running sandbox.
func runningSandbox(t *testing.T, p *sandbox.Fake, name string) sandbox.Handle {
	t.Helper()
	h, err := p.Create(context.Background(), sandbox.Spec{
		Name:        name,
		Image:       "img",
		Command:     []string{"true"},
		Ports:       []int{attachPort},
		CPU:         0.5,
		MemoryMiB:   2048,
		Timeout:     time.Hour,
		IdleTimeout: time.Minute,
		Tags:        sandbox.BuildTags(sandbox.TagSet{Name: name, User: "alice", Repos: []string{"acme/app", "acme/lib"}, CreatedAt: time.Now()}),
		Egress:      sandbox.EgressOpen(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func testAttacher(p *sandbox.Fake, health opencode.Health) *Attacher {
	return &Attacher{
		Sandboxes:     p,
		NewClient:     func(string, string) HealthChecker { return &fakeHealth{health: health} },
		HealthTimeout: time.Second,
	}
}

func TestAttachReturnsAHealthySession(t *testing.T) {
	p := sandbox.NewFake()
	runningSandbox(t, p, "sess")
	a := testAttacher(p, opencode.Health{Healthy: true, Version: "v1"})

	sess, err := a.Attach(context.Background(), AttachOptions{
		Ref: "sess", Port: attachPort, Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Password != "pw" {
		t.Errorf("password = %q", sess.Password)
	}
	if sess.Health.Version != "v1" {
		t.Errorf("health = %+v", sess.Health)
	}
	if sess.URL == "" {
		t.Error("no tunnel URL")
	}
	if got := strings.Join(sess.Clones, ","); got != "acme/app,acme/lib" {
		t.Errorf("clones = %q, want them recovered from the tags", got)
	}
}

// TestAttachDoesNotRevokeTokens: the tokens belong to the launching
// process and the session is still using them. Closing an attached
// session must not pull them out from under it.
//
// @decision(DL-025)
func TestAttachDoesNotRevokeTokens(t *testing.T) {
	p := sandbox.NewFake()
	runningSandbox(t, p, "sess")
	a := testAttacher(p, opencode.Health{Healthy: true, Version: "v1"})

	sess, err := a.Attach(context.Background(), AttachOptions{
		Ref: "sess", Port: attachPort, Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.revokeTokens != nil {
		t.Error("an attached session carries a token revoker it does not own")
	}
}

func TestAttachRejectsAStoppedSandbox(t *testing.T) {
	p := sandbox.NewFake()
	h := runningSandbox(t, p, "sess")
	if err := h.Terminate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := testAttacher(p, opencode.Health{Healthy: true})

	_, err := a.Attach(context.Background(), AttachOptions{Ref: "sess", Port: attachPort, Password: "pw"})
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
}

func TestAttachNeedsAReference(t *testing.T) {
	a := testAttacher(sandbox.NewFake(), opencode.Health{Healthy: true})
	if _, err := a.Attach(context.Background(), AttachOptions{Port: attachPort}); err == nil {
		t.Fatal("expected an error")
	}
}

// TestAttachReportsAnUnreachableSandbox: without a connect token there is
// no way in, so attach fails rather than returning a session that cannot
// be talked to.
//
// @decision(DL-030)
func TestAttachReportsAnUnreachableSandbox(t *testing.T) {
	p := sandbox.NewFake()
	runningSandbox(t, p, "sess")
	p.ConnectErr = errors.New("no connect token")
	a := testAttacher(p, opencode.Health{Healthy: true})

	_, err := a.Attach(context.Background(), AttachOptions{
		Ref: "sess", Port: attachPort, Password: "pw",
	})
	if err == nil || !strings.Contains(err.Error(), "connect token") {
		t.Fatalf("err = %v; it should report why the sandbox is unreachable", err)
	}
}

func TestAttachWithoutARecoverer(t *testing.T) {
	p := sandbox.NewFake()
	runningSandbox(t, p, "sess")
	a := testAttacher(p, opencode.Health{Healthy: true})

	// No password and no recoverer: nothing to try.
	_, err := a.Attach(context.Background(), AttachOptions{Ref: "sess", Port: attachPort})
	if !errors.Is(err, ErrNoPassword) {
		t.Fatalf("err = %v, want ErrNoPassword", err)
	}
}

func TestAttachUsesTheRecoverer(t *testing.T) {
	p := sandbox.NewFake()
	runningSandbox(t, p, "sess")
	a := testAttacher(p, opencode.Health{Healthy: true, Version: "v1"})

	var gotID string
	a.RecoverPassword = func(_ context.Context, _ sandbox.Handle, info sandbox.Info) (string, error) {
		gotID = info.ID
		return "recovered", nil
	}

	sess, err := a.Attach(context.Background(), AttachOptions{Ref: "sess", Port: attachPort})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Password != "recovered" {
		t.Errorf("password = %q", sess.Password)
	}
	if gotID == "" {
		t.Error("the recoverer was not told which sandbox to recover for")
	}
}

// TestAttachTreatsAnEmptyRecoveredPasswordAsAFailure: an empty password
// disables OpenCode's auth entirely, so it must never be accepted as a
// successful recovery.
//
// @decision(DL-025) @decision(DL-030)
func TestAttachTreatsAnEmptyRecoveredPasswordAsAFailure(t *testing.T) {
	p := sandbox.NewFake()
	runningSandbox(t, p, "sess")
	a := testAttacher(p, opencode.Health{Healthy: true})
	a.RecoverPassword = func(context.Context, sandbox.Handle, sandbox.Info) (string, error) {
		return "", nil
	}

	_, err := a.Attach(context.Background(), AttachOptions{Ref: "sess", Port: attachPort})
	if !errors.Is(err, ErrNoPassword) {
		t.Fatalf("err = %v, want ErrNoPassword", err)
	}
}

func TestAttachPropagatesRecoveryFailure(t *testing.T) {
	p := sandbox.NewFake()
	runningSandbox(t, p, "sess")
	a := testAttacher(p, opencode.Health{Healthy: true})
	boom := errors.New("exec refused")
	a.RecoverPassword = func(context.Context, sandbox.Handle, sandbox.Info) (string, error) {
		return "", boom
	}

	_, err := a.Attach(context.Background(), AttachOptions{Ref: "sess", Port: attachPort})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the recovery failure", err)
	}
}

func TestAttachUnknownSandbox(t *testing.T) {
	a := testAttacher(sandbox.NewFake(), opencode.Health{Healthy: true})
	_, err := a.Attach(context.Background(), AttachOptions{Ref: "nope", Port: attachPort, Password: "pw"})
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestPasswordFromSandbox covers reading the password back out of a
// running container, which is what makes a cross-machine attach possible.
//
// @decision(DL-025)
func TestPasswordFromSandbox(t *testing.T) {
	tests := []struct {
		name string
		// result and execErr set up the fake sandbox's response.
		result  sandbox.ExecResult
		execErr error
		// want is the password expected on success; wantErr is the
		// sentinel a failure must wrap, and wantAnyErr covers failures
		// that carry no sentinel of their own.
		want       string
		wantErr    error
		wantAnyErr bool
	}{
		{
			name:   "trailing newline is stripped",
			result: sandbox.ExecResult{Stdout: "s3cret\n"},
			want:   "s3cret",
		},
		{
			name:   "CRLF is stripped",
			result: sandbox.ExecResult{Stdout: "s3cret\r\n"},
			want:   "s3cret",
		},
		{
			name:   "no trailing newline",
			result: sandbox.ExecResult{Stdout: "s3cret"},
			want:   "s3cret",
		},
		{
			name:    "unset variable",
			result:  sandbox.ExecResult{ExitCode: 1},
			wantErr: ErrNoPassword,
		},
		{
			name:    "empty value",
			result:  sandbox.ExecResult{Stdout: "\n"},
			wantErr: ErrNoPassword,
		},
		{
			name:       "exec fails",
			execErr:    errors.New("sandbox is gone"),
			wantAnyErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := sandbox.NewFake()
			p.ExecResults["printenv "+opencode.PasswordEnvVar] = tt.result
			p.ExecErr = tt.execErr
			h := runningSandbox(t, p, "sess")

			got, err := PasswordFromSandbox(context.Background(), h)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantAnyErr:
				if err == nil {
					t.Fatal("expected an error")
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if got != tt.want {
					t.Errorf("password = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

// TestPasswordFromSandboxErrorsHideTheSecret: an error string travels
// into logs, so it must never carry the password.
//
// @decision(DL-025) @decision(DL-030)
func TestPasswordFromSandboxErrorsHideTheSecret(t *testing.T) {
	p := sandbox.NewFake()
	// A non-zero exit with the password on stdout: the value is present
	// but must not reach the error.
	p.ExecResults["printenv "+opencode.PasswordEnvVar] = sandbox.ExecResult{
		ExitCode: 1,
		Stdout:   "leaked-secret\n",
		Stderr:   "leaked-secret\n",
	}
	h := runningSandbox(t, p, "sess")

	_, err := PasswordFromSandbox(context.Background(), h)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "leaked-secret") {
		t.Errorf("the error carries the password: %v", err)
	}
}

func TestClonesFromTags(t *testing.T) {
	if got := clonesFromTags(nil); got != nil {
		t.Errorf("got %v, want nil for absent tags", got)
	}
	if got := clonesFromTags(map[string]string{sandbox.TagRepos: ""}); got != nil {
		t.Errorf("got %v, want nil for an empty tag", got)
	}
	got := clonesFromTags(map[string]string{sandbox.TagRepos: "a/b,c/d"})
	if len(got) != 2 || got[0] != "a/b" || got[1] != "c/d" {
		t.Errorf("got %v", got)
	}
}

func TestDisplayRef(t *testing.T) {
	// Given a name that matches what the user typed, echo it back.
	if got := displayRef("sess", sandbox.Info{ID: "sb-1", Name: "sess"}); got != "sess" {
		t.Errorf("got %q", got)
	}
	// Given an ID, the name is more recognisable, so show both.
	got := displayRef("sb-1", sandbox.Info{ID: "sb-1", Name: "sess"})
	if !strings.Contains(got, "sess") || !strings.Contains(got, "sb-1") {
		t.Errorf("got %q, want both the name and the id", got)
	}
	// An unnamed sandbox has nothing to add.
	if got := displayRef("sb-1", sandbox.Info{ID: "sb-1"}); got != "sb-1" {
		t.Errorf("got %q", got)
	}
}

// TestAPasswordReadIsBounded is DL-130: printenv runs inside the
// sandbox, where the agent can replace it with anything.
func TestAPasswordReadIsBounded(t *testing.T) {
	p := sandbox.NewFake()
	h := runningSandbox(t, p, "bounded")
	p.ExecResults["printenv "+opencode.PasswordEnvVar] = sandbox.ExecResult{Stdout: strings.Repeat("x", maxPasswordOutput+1)}
	if _, err := PasswordFromSandbox(t.Context(), h); !errors.Is(err, sandbox.ErrOutputTooLarge) {
		t.Errorf("err = %v, want ErrOutputTooLarge", err)
	}
}
