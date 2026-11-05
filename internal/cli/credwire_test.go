package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

func envWith(vars map[string]string) *Env {
	return &Env{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Environ: config.MapEnv{Vars: vars}}
}

func validKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, auth.KeyBytes))
}

// No key configured is not an error: the server simply stores no user
// credentials, which is every local run.
func TestSealerWithoutAKeyIsNil(t *testing.T) {
	s, err := envWith(nil).sealer()
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	if s != nil {
		t.Error("a sealer was built with no key configured")
	}
}

func TestSealerBuildsFromTheEnvironment(t *testing.T) {
	s, err := envWith(map[string]string{CredentialKeyEnvVar: validKey()}).sealer()
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	if s == nil {
		t.Fatal("no sealer was built")
	}
}

// The whole rotation lifecycle, against the store. This is the test
// that catches a versioning scheme which seals new data under the
// version existing rows already carry — which makes every stored
// credential unreadable the first time an operator rotates.
func TestRotationLifecycle(t *testing.T) {
	keyA := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", auth.KeyBytes)))
	keyB := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", auth.KeyBytes)))
	ctx := context.Background()
	backing := store.NewMemory().Credentials()

	sealerFor := func(vars map[string]string) *server.CredentialStore {
		t.Helper()
		s, err := envWith(vars).sealer()
		if err != nil {
			t.Fatalf("sealer: %v", err)
		}
		return &server.CredentialStore{Store: backing, Sealer: s}
	}

	// Deployed with one key; a user stores a credential.
	c := sealerFor(map[string]string{CredentialKeyEnvVar: keyA})
	if err := c.Put(ctx, "org", "u", server.PlatformGitHub, "secret", server.Verified{}); err != nil {
		t.Fatal(err)
	}

	// Rotate: new key current, old key retiring. The row must still open.
	c = sealerFor(map[string]string{CredentialKeyEnvVar: keyB, CredentialKeyOldEnvVar: keyA})
	got, err := c.Get(ctx, "org", "u", server.PlatformGitHub)
	if err != nil {
		t.Fatalf("the credential did not survive the rotation: %v", err)
	}
	if got != "secret" {
		t.Errorf("Get = %q", got)
	}

	// Rewritten under the new key, the old one can be dropped.
	if err := c.Put(ctx, "org", "u", server.PlatformGitHub, "secret", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	c = sealerFor(map[string]string{CredentialKeyEnvVar: keyB})
	if got, err = c.Get(ctx, "org", "u", server.PlatformGitHub); err != nil {
		t.Fatalf("a rewritten row did not survive dropping the old key: %v", err)
	}
	if got != "secret" {
		t.Errorf("Get = %q", got)
	}
}

// The version comes from the key, so which variable a key is presented
// in cannot change what it opens.
func TestRotationIsOrderIndependent(t *testing.T) {
	keyA := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", auth.KeyBytes)))
	keyB := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", auth.KeyBytes)))
	ctx := context.Background()
	backing := store.NewMemory().Credentials()

	s, err := envWith(map[string]string{CredentialKeyEnvVar: keyA}).sealer()
	if err != nil {
		t.Fatal(err)
	}
	c := &server.CredentialStore{Store: backing, Sealer: s}
	if err := c.Put(ctx, "org", "u", server.PlatformGitHub, "secret", server.Verified{}); err != nil {
		t.Fatal(err)
	}

	// The operator swaps the two variables by mistake. The row still
	// opens, because keyA carries the version it sealed under.
	swapped, err := envWith(map[string]string{
		CredentialKeyEnvVar: keyA, CredentialKeyOldEnvVar: keyB,
	}).sealer()
	if err != nil {
		t.Fatal(err)
	}
	c = &server.CredentialStore{Store: backing, Sealer: swapped}
	if _, err := c.Get(ctx, "org", "u", server.PlatformGitHub); err != nil {
		t.Errorf("swapping the key variables made the credential unreadable: %v", err)
	}
}

// The old key alone would seal everything under a key the operator is
// trying to retire, so it is refused rather than silently used.
func TestSealerRefusesTheOldKeyAlone(t *testing.T) {
	_, err := envWith(map[string]string{CredentialKeyOldEnvVar: validKey()}).sealer()
	if err == nil {
		t.Fatal("the old key alone was accepted")
	}
	if !strings.Contains(err.Error(), CredentialKeyEnvVar) {
		t.Errorf("err = %v; it should name the current key", err)
	}
}

// A malformed key must fail the server's start rather than being
// ignored, and must not appear in the error.
func TestSealerRejectsABadKey(t *testing.T) {
	const bad = "c2hvcnQ=" // "short", base64: valid base64, wrong length
	_, err := envWith(map[string]string{CredentialKeyEnvVar: bad}).sealer()
	if err == nil {
		t.Fatal("a short key was accepted")
	}
	if strings.Contains(err.Error(), bad) {
		t.Errorf("the error carries the key: %v", err)
	}
}

// Local mode gets the store and verifier — so `spov login` works against
// a local server — but no per-user resolution.
func TestWireCredentialsLocalMode(t *testing.T) {
	deps := server.Deps{Store: store.NewMemory()}
	env := envWith(map[string]string{CredentialKeyEnvVar: validKey()})
	if err := env.wireCredentials(&deps, config.Config{}, true); err != nil {
		t.Fatalf("wireCredentials: %v", err)
	}
	if deps.Credentials == nil || deps.Verifier == nil {
		t.Error("local mode got no credential store or verifier")
	}
	if deps.UserForge != nil || deps.UserProviderKeys != nil {
		t.Error("local mode installed per-user resolution")
	}
}

// Hosted mode with a key gets the full two-tier resolution.
func TestWireCredentialsHosted(t *testing.T) {
	deps := server.Deps{Store: store.NewMemory(), Warn: func(string) {}}
	env := envWith(map[string]string{CredentialKeyEnvVar: validKey()})
	if err := env.wireCredentials(&deps, config.Config{}, false); err != nil {
		t.Fatalf("wireCredentials: %v", err)
	}
	if deps.UserForge == nil || deps.UserProviderKeys == nil {
		t.Error("hosted mode got no per-user resolution")
	}
	if deps.Credentials.Sealer == nil {
		t.Error("hosted mode got no sealer")
	}
}

// Hosted mode without a key must not install per-user resolution: there
// is nothing to read back, so every user keeps the deployment's
// credentials and the API refuses to store any.
func TestWireCredentialsHostedWithoutAKey(t *testing.T) {
	deps := server.Deps{Store: store.NewMemory(), Warn: func(string) {}}
	if err := envWith(nil).wireCredentials(&deps, config.Config{}, false); err != nil {
		t.Fatalf("wireCredentials: %v", err)
	}
	if deps.UserForge != nil {
		t.Error("per-user resolution was installed with no key")
	}
	if deps.Credentials.Sealer != nil {
		t.Error("a sealer was installed with no key")
	}
}

// A bad key must stop the server rather than starting one that cannot
// read the credentials it holds.
func TestWireCredentialsFailsOnABadKey(t *testing.T) {
	deps := server.Deps{Store: store.NewMemory()}
	env := envWith(map[string]string{CredentialKeyEnvVar: "not base64 at all !!"})
	if err := env.wireCredentials(&deps, config.Config{}, false); err == nil {
		t.Fatal("a bad key did not stop the wiring")
	}
}
