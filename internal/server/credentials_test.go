package server

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

func testSealer(t *testing.T) *auth.Sealer {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(make([]byte, auth.KeyBytes))
	s, err := auth.NewSealer(map[int]string{1: key})
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	return s
}

func newTestCredentials(t *testing.T) (*CredentialStore, store.Store) {
	t.Helper()
	st := store.NewMemory()
	return &CredentialStore{
		Store:  st.Credentials(),
		Sealer: testSealer(t),
		Now:    func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) },
	}, st
}

func TestCredentialRoundTrip(t *testing.T) {
	c, _ := newTestCredentials(t)
	ctx := context.Background()
	const secret = "github_pat_11ABCDE_secret"

	if err := c.Put(ctx, "default", "user-1", PlatformGitHub, secret, Verified{As: "alice"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(ctx, "default", "user-1", PlatformGitHub)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != secret {
		t.Errorf("Get = %q, want %q", got, secret)
	}
}

// The stored row must not contain the plaintext anywhere: that is the
// whole point of sealing it.
func TestStoredRowHoldsNoPlaintext(t *testing.T) {
	c, st := newTestCredentials(t)
	ctx := context.Background()
	const secret = "github_pat_a_very_distinctive_secret"
	if err := c.Put(ctx, "default", "user-1", PlatformGitHub, secret, Verified{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rec, err := st.Credentials().Get(ctx, "default", "user-1", PlatformGitHub)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if strings.Contains(string(rec.Ciphertext), secret) {
		t.Error("the stored ciphertext contains the plaintext")
	}
	if len(rec.Nonce) == 0 {
		t.Error("the stored row has no nonce")
	}
}

// One user must never read another's credential, even for the same
// platform. The AAD is what enforces it below the store's own scoping.
func TestGetIsScopedToTheUser(t *testing.T) {
	c, _ := newTestCredentials(t)
	ctx := context.Background()
	if err := c.Put(ctx, "default", "victim", PlatformGitHub, "victim-secret", Verified{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(ctx, "default", "attacker", PlatformGitHub)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "" {
		t.Errorf("another user's credential leaked: %q", got)
	}
}

// A row copied into another user's name must fail to open rather than
// decrypt: the store's scoping is one layer, the AAD is the other.
func TestARowMovedBetweenUsersDoesNotOpen(t *testing.T) {
	c, st := newTestCredentials(t)
	ctx := context.Background()
	if err := c.Put(ctx, "default", "victim", PlatformGitHub, "victim-secret", Verified{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rec, err := st.Credentials().Get(ctx, "default", "victim", PlatformGitHub)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	// Straight database-level theft: same ciphertext, another owner.
	rec.ID = store.NewID(time.Now())
	rec.UserID = "attacker"
	if err := st.Credentials().Put(ctx, rec); err != nil {
		t.Fatalf("store Put: %v", err)
	}
	if _, err := c.Get(ctx, "default", "attacker", PlatformGitHub); err == nil {
		t.Fatal("a stolen row opened under another user")
	}
}

func TestGetMissingIsEmptyNotAnError(t *testing.T) {
	c, _ := newTestCredentials(t)
	got, err := c.Get(context.Background(), "default", "user-1", PlatformGitHub)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "" {
		t.Errorf("Get = %q, want empty", got)
	}
}

func TestPutReplaces(t *testing.T) {
	c, _ := newTestCredentials(t)
	ctx := context.Background()
	if err := c.Put(ctx, "default", "u", PlatformGitHub, "first", Verified{As: "a"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Put(ctx, "default", "u", PlatformGitHub, "second", Verified{As: "b"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(ctx, "default", "u", PlatformGitHub)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "second" {
		t.Errorf("Get = %q, want %q", got, "second")
	}
	views, err := c.List(ctx, "default", "u")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(views))
	}
	if views[0].VerifiedAs != "b" {
		t.Errorf("VerifiedAs = %q, want b", views[0].VerifiedAs)
	}
}

func TestDelete(t *testing.T) {
	c, _ := newTestCredentials(t)
	ctx := context.Background()
	if err := c.Put(ctx, "default", "u", PlatformGitHub, "secret", Verified{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Delete(ctx, "default", "u", PlatformGitHub); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := c.Get(ctx, "default", "u", PlatformGitHub)
	if err != nil || got != "" {
		t.Errorf("Get after Delete = %q, %v", got, err)
	}
}

// List must never carry a credential, and must not need to decrypt.
func TestListCarriesNoSecret(t *testing.T) {
	c, _ := newTestCredentials(t)
	ctx := context.Background()
	const secret = "a_distinctive_secret_value"
	if err := c.Put(ctx, "default", "u", PlatformGitHub, secret, Verified{As: "alice"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	views, err := c.List(ctx, "default", "u")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("List returned %d rows", len(views))
	}
	if !views[0].Present || views[0].Platform != PlatformGitHub {
		t.Errorf("view = %+v", views[0])
	}
	if strings.Contains(views[0].VerifiedAs, secret) {
		t.Error("the view carries the secret")
	}
}

// A key that no longer opens a row must be an error rather than a
// silent miss: falling through to the org tier would hide data loss.
func TestGetFailsLoudlyWhenTheKeyChanged(t *testing.T) {
	c, _ := newTestCredentials(t)
	ctx := context.Background()
	if err := c.Put(ctx, "default", "u", PlatformGitHub, "secret", Verified{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	other := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", auth.KeyBytes)))
	sealer, err := auth.NewSealer(map[int]string{1: other})
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	c.Sealer = sealer
	if _, err := c.Get(ctx, "default", "u", PlatformGitHub); err == nil {
		t.Fatal("Get succeeded under a rotated-away key")
	}
}

func TestPutRefusesAnOversizeSecret(t *testing.T) {
	c, _ := newTestCredentials(t)
	err := c.Put(context.Background(), "default", "u", PlatformGitHub,
		strings.Repeat("x", maxSecretLength+1), Verified{})
	if err == nil {
		t.Fatal("Put accepted an oversize secret")
	}
	if strings.Contains(err.Error(), "xxxx") {
		t.Errorf("the error echoes the secret: %v", err)
	}
}

func TestPutWithoutASealerFails(t *testing.T) {
	c := &CredentialStore{Store: store.NewMemory().Credentials()}
	if err := c.Put(context.Background(), "default", "u", PlatformGitHub, "s", Verified{}); err == nil {
		t.Fatal("Put succeeded with no sealer")
	}
}

// A nil store is what local mode without a key looks like; reading must
// be an empty answer rather than a panic.
func TestNilStoreReadsEmpty(t *testing.T) {
	var c *CredentialStore
	got, err := c.Get(context.Background(), "default", "u", PlatformGitHub)
	if err != nil || got != "" {
		t.Errorf("Get on a nil store = %q, %v", got, err)
	}
	views, err := c.List(context.Background(), "default", "u")
	if err != nil || len(views) != 0 {
		t.Errorf("List on a nil store = %v, %v", views, err)
	}
}

// A store error that is not "missing" must reach the caller: only a
// genuine absence is an empty answer.
func TestGetPropagatesAStoreError(t *testing.T) {
	want := errors.New("the database is down")
	c := &CredentialStore{Store: errCredentials{err: want}, Sealer: testSealer(t)}
	if _, err := c.Get(context.Background(), "org", "u", PlatformGitHub); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestListPropagatesAStoreError(t *testing.T) {
	want := errors.New("the database is down")
	c := &CredentialStore{Store: errCredentials{err: want}, Sealer: testSealer(t)}
	if _, err := c.List(context.Background(), "org", "u"); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestPutRefusesAnEmptySecret(t *testing.T) {
	c, _ := newTestCredentials(t)
	if err := c.Put(context.Background(), "org", "u", PlatformGitHub, "", Verified{}); err == nil {
		t.Fatal("an empty secret was stored")
	}
}

func TestDeleteOnANilStoreIsQuiet(t *testing.T) {
	var c *CredentialStore
	if err := c.Delete(context.Background(), "org", "u", PlatformGitHub); err != nil {
		t.Fatalf("Delete on a nil store: %v", err)
	}
}

// errCredentials is a store whose every call fails.
type errCredentials struct{ err error }

func (e errCredentials) Put(context.Context, store.UserCredential) error { return e.err }
func (e errCredentials) Get(context.Context, string, string, string) (store.UserCredential, error) {
	return store.UserCredential{}, e.err
}
func (e errCredentials) ListByUser(context.Context, string, string) ([]store.UserCredential, error) {
	return nil, e.err
}
func (e errCredentials) Delete(context.Context, string, string, string) error { return e.err }
func (e errCredentials) Lock(context.Context, string, string, string) error   { return e.err }

func TestValidPlatform(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     string
		wantErr string
	}{
		{"github", PlatformGitHub, ""},
		{"forgejo host", ForgejoPlatform("codefloe.com"), ""},
		{"provider", ProviderPlatform("ANTHROPIC_API_KEY"), ""},
		{"forgejo with no host", "forgejo:", "needs a host"},
		{"provider with no name", "provider:", "needs a variable name"},
		{"provider that is not a key", "provider:SHELL", "end in _API_KEY"},
		{"gitea host", "gitea:gitea.com", ""},
		{"gitlab host", "gitlab:gitlab.com", ""},
		{"gitlab with no host", "gitlab:", "gitlab:gitlab.com"},
		{"unknown", "gitlab", "not a platform"},
		{"empty", "", "not a platform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidPlatform(tc.key)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidPlatform(%q) = %v, want nil", tc.key, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidPlatform(%q) = nil, want an error", tc.key)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// Canonicalisation is what makes `remove` remove the row that `login`
// created. Without it a differently-spelled key reports success and
// leaves a live credential in place.
func TestCanonicalPlatform(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"github", "github"},
		{"GitHub", "github"},
		{"forgejo:codefloe.com", "forgejo:codefloe.com"},
		{"forgejo:CodeFloe.COM", "forgejo:codefloe.com"},
		{"FORGEJO:Codefloe.com", "forgejo:codefloe.com"},
		{"provider:ANTHROPIC_API_KEY", "provider:ANTHROPIC_API_KEY"},
		{"provider:anthropic_api_key", "provider:ANTHROPIC_API_KEY"},
		{"  github  ", "github"},
		{"Gitea:Gitea.com", "gitea:gitea.com"},
		{"GITLAB:GitLab.com", "gitlab:gitlab.com"},
	} {
		got, err := CanonicalPlatform(tc.in)
		if err != nil {
			t.Errorf("CanonicalPlatform(%q) = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("CanonicalPlatform(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"gitlab", "", "forgejo:", "provider:", "provider:SHELL"} {
		if _, err := CanonicalPlatform(bad); err == nil {
			t.Errorf("CanonicalPlatform(%q) was accepted", bad)
		}
	}
}

func TestForgejoPlatformLowercasesTheHost(t *testing.T) {
	if got := ForgejoPlatform("CodeFloe.COM"); got != "forgejo:codefloe.com" {
		t.Errorf("ForgejoPlatform = %q", got)
	}
}

// Every per-host key splits back into the kind and host it was made of.
//
// @decision(DL-041)
func TestAPerHostKeySplitsIntoItsKindAndHost(t *testing.T) {
	for _, kind := range HostKinds {
		got, host, ok := PlatformHost(HostPlatform(kind, "Git.Example.com"))
		if !ok || got != kind || host != "git.example.com" {
			t.Errorf("%s: %q %q %v", kind, got, host, ok)
		}
	}
	for _, key := range []string{"github", "provider:X_API_KEY", "gitlab:", "bitbucket:x"} {
		if _, _, ok := PlatformHost(key); ok {
			t.Errorf("%q split", key)
		}
	}
}

func TestMergeProviderKeys(t *testing.T) {
	org := map[string]string{"ANTHROPIC_API_KEY": "org-a", "OPENAI_API_KEY": "org-o"}
	user := map[string]string{"ANTHROPIC_API_KEY": "user-a", "GROQ_API_KEY": "user-g"}
	got := MergeProviderKeys(org, user)
	want := map[string]string{
		"ANTHROPIC_API_KEY": "user-a", // the user's overrides
		"OPENAI_API_KEY":    "org-o",  // the org's survives
		"GROQ_API_KEY":      "user-g", // the user's alone
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("merged[%q] = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("merged has %d keys, want %d: %v", len(got), len(want), got)
	}
	// The inputs must not be mutated: the org map is shared by every user.
	if org["ANTHROPIC_API_KEY"] != "org-a" {
		t.Error("MergeProviderKeys mutated the org map")
	}
}

// An empty user value must not blank out the deployment's key.
func TestMergeProviderKeysIgnoresEmptyUserValues(t *testing.T) {
	got := MergeProviderKeys(
		map[string]string{"ANTHROPIC_API_KEY": "org-a"},
		map[string]string{"ANTHROPIC_API_KEY": ""},
	)
	if got["ANTHROPIC_API_KEY"] != "org-a" {
		t.Errorf("merged = %q, want the org value", got["ANTHROPIC_API_KEY"])
	}
}
