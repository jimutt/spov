package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
)

func testHosted(t *testing.T, vars map[string]string) (*hostedCredentials, *server.CredentialStore) {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(make([]byte, auth.KeyBytes))
	sealer, err := auth.NewSealer(map[int]string{1: key})
	if err != nil {
		t.Fatal(err)
	}
	creds := &server.CredentialStore{
		Store:  store.NewMemory().Credentials(),
		Sealer: sealer,
		Now:    func() time.Time { return time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC) },
	}
	env := &Env{
		Out:     &bytes.Buffer{},
		Err:     &bytes.Buffer{},
		Environ: config.MapEnv{Vars: vars},
	}
	cfg := config.Config{Platforms: []config.Platform{
		{Name: "gh", Kind: config.KindGitHub, Account: "alice"},
		{Name: "cf", Kind: config.KindForgejo, BaseURL: "https://codefloe.com", Account: "alice"},
	}}
	return &hostedCredentials{
		env: env, cfg: cfg, store: creds,
		warn:    func(string) {},
		orgKeys: map[string]string{},
	}, creds
}

// The user's own credential must win over the deployment's.
func TestUserTierBeatsOrgTier(t *testing.T) {
	h, creds := testHosted(t, map[string]string{
		"SPOV_PLATFORM_TOKEN_CF": "org-forgejo-token",
	})
	ctx := context.Background()
	if err := creds.Put(ctx, "default", "u1", server.ForgejoPlatform("codefloe.com"), "user-forgejo-token", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	token, tier, _ := h.tokenFor(ctx, "default", "u1", h.cfg.Platforms[1], server.TierPolicy{})
	if tier != server.TierUser {
		t.Errorf("tier = %q, want user", tier)
	}
	if token != "user-forgejo-token" {
		t.Errorf("token = %q, want the user's", token)
	}
}

// A user without their own credential falls back to the deployment's.
func TestOrgTierIsTheFallback(t *testing.T) {
	h, _ := testHosted(t, map[string]string{
		"SPOV_PLATFORM_TOKEN_CF": "org-forgejo-token",
	})
	token, tier, _ := h.tokenFor(context.Background(), "default", "u1", h.cfg.Platforms[1], server.TierPolicy{})
	if tier != server.TierOrg {
		t.Errorf("tier = %q, want org", tier)
	}
	if token != "org-forgejo-token" {
		t.Errorf("token = %q, want the org's", token)
	}
}

// One user's credential must never be resolved for another.
func TestTokensAreScopedToTheUser(t *testing.T) {
	h, creds := testHosted(t, nil)
	ctx := context.Background()
	if err := creds.Put(ctx, "default", "victim", server.PlatformGitHub, "github_pat_victim", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	token, tier, reason := h.tokenFor(ctx, "default", "attacker", h.cfg.Platforms[0], server.TierPolicy{})
	if token != "" || tier != server.TierNone {
		t.Errorf("another user's token leaked: %q (%s)", token, tier)
	}
	if !strings.Contains(reason, "spov credentials add") {
		t.Errorf("reason = %q; it should say what to do", reason)
	}
}

// No credential at either tier is reported with a reason, not silently.
func TestNoCredentialSaysWhatToDo(t *testing.T) {
	h, _ := testHosted(t, nil)
	for _, tc := range []struct {
		name string
		p    config.Platform
		want string
	}{
		{"github", h.cfg.Platforms[0], "spov credentials add github"},
		{"forgejo", h.cfg.Platforms[1], "spov credentials add forgejo:codefloe.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tier, reason := h.tokenFor(context.Background(), "default", "u1", tc.p, server.TierPolicy{})
			if tier != server.TierNone {
				t.Fatalf("tier = %q, want none", tier)
			}
			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.want)
			}
		})
	}
}

// The GitHub App is assembled from its own variables, as the org tier.
func TestGitHubAppIsTheOrgTier(t *testing.T) {
	h, _ := testHosted(t, map[string]string{
		"SPOV_GITHUB_APP_ID":          "12345",
		"SPOV_GITHUB_APP_PRIVATE_KEY": "-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----",
	})
	token, tier, _ := h.tokenFor(context.Background(), "default", "u1", h.cfg.Platforms[0], server.TierPolicy{})
	if tier != server.TierOrg {
		t.Errorf("tier = %q, want org", tier)
	}
	if !strings.Contains(token, `"app_id":"12345"`) {
		t.Errorf("the App credential was not assembled: %q", token)
	}
}

// A half-configured App must not count as a credential.
func TestGitHubAppNeedsBothParts(t *testing.T) {
	h, _ := testHosted(t, map[string]string{"SPOV_GITHUB_APP_ID": "12345"})
	_, tier, _ := h.tokenFor(context.Background(), "default", "u1", h.cfg.Platforms[0], server.TierPolicy{})
	if tier != server.TierNone {
		t.Errorf("tier = %q, want none for a key-less App", tier)
	}
}

// A stored credential that cannot be decrypted must not fall through to
// the org tier: that would hide a lost key behind someone else's access.
func TestAnUnreadableCredentialDoesNotFallThrough(t *testing.T) {
	h, creds := testHosted(t, map[string]string{"SPOV_PLATFORM_TOKEN_CF": "org-token"})
	ctx := context.Background()
	if err := creds.Put(ctx, "default", "u1", server.ForgejoPlatform("codefloe.com"), "user-token", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	// Rotate the key away, as losing SPOV_CREDENTIAL_KEY would.
	other := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", auth.KeyBytes)))
	sealer, err := auth.NewSealer(map[int]string{1: other})
	if err != nil {
		t.Fatal(err)
	}
	creds.Sealer = sealer

	token, tier, reason := h.tokenFor(ctx, "default", "u1", h.cfg.Platforms[1], server.TierPolicy{})
	if tier != server.TierNone {
		t.Fatalf("tier = %q, want none; it fell through to %q", tier, token)
	}
	if !strings.Contains(reason, "store it again") {
		t.Errorf("reason = %q; it should tell the user to store it again", reason)
	}
}

func TestProviderKeysLayerPerVariable(t *testing.T) {
	h, creds := testHosted(t, nil)
	h.orgKeys = map[string]string{"ANTHROPIC_API_KEY": "org-a", "OPENAI_API_KEY": "org-o"}
	ctx := context.Background()
	if err := creds.Put(ctx, "default", "u1", server.ProviderPlatform("ANTHROPIC_API_KEY"), "user-a", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	keys, err := h.providerKeysFor(ctx, "default", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if keys["ANTHROPIC_API_KEY"] != "user-a" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the user's", keys["ANTHROPIC_API_KEY"])
	}
	if keys["OPENAI_API_KEY"] != "org-o" {
		t.Errorf("OPENAI_API_KEY = %q, want the deployment's", keys["OPENAI_API_KEY"])
	}
}

// A provider key the user stored but that cannot be opened must be
// withheld, not silently replaced by the deployment's: substituting
// would bill the deployment for work the user meant to put on their own
// account, which is the same reasoning tokenFor applies to git tokens.
func TestAnUnreadableProviderKeyIsNotReplaced(t *testing.T) {
	h, creds := testHosted(t, nil)
	h.orgKeys = map[string]string{"ANTHROPIC_API_KEY": "org-a", "OPENAI_API_KEY": "org-o"}
	ctx := context.Background()
	if err := creds.Put(ctx, "default", "u1", server.ProviderPlatform("ANTHROPIC_API_KEY"), "user-a", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	// Rotate the key away, as losing SPOV_CREDENTIAL_KEY would.
	other := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", auth.KeyBytes)))
	sealer, err := auth.NewSealer(map[int]string{1: other})
	if err != nil {
		t.Fatal(err)
	}
	creds.Sealer = sealer

	keys, err := h.providerKeysFor(ctx, "default", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := keys["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("an unreadable user key fell through to %q", got)
	}
	// A variable the user never stored still gets the deployment's.
	if keys["OPENAI_API_KEY"] != "org-o" {
		t.Errorf("OPENAI_API_KEY = %q, want the deployment's", keys["OPENAI_API_KEY"])
	}
}

// Another user must not inherit the first's provider key.
func TestProviderKeysAreScopedToTheUser(t *testing.T) {
	h, creds := testHosted(t, nil)
	h.orgKeys = map[string]string{"ANTHROPIC_API_KEY": "org-a"}
	ctx := context.Background()
	if err := creds.Put(ctx, "default", "u1", server.ProviderPlatform("ANTHROPIC_API_KEY"), "user-a", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	keys, err := h.providerKeysFor(ctx, "default", "u2")
	if err != nil {
		t.Fatal(err)
	}
	if keys["ANTHROPIC_API_KEY"] != "org-a" {
		t.Errorf("u2 got %q, want the deployment's key", keys["ANTHROPIC_API_KEY"])
	}
}

// The forge a user gets must carry their own tokens, and a platform
// they have no credential for must not resolve at all.
func TestForgeForCarriesTheUsersTokens(t *testing.T) {
	h, creds := testHosted(t, nil)
	ctx := context.Background()
	if err := creds.Put(ctx, "default", "u1", server.PlatformGitHub, "github_pat_mine", server.Verified{}); err != nil {
		t.Fatal(err)
	}
	forge, tiers, err := h.forgeFor(ctx, "default", "u1", server.TierPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if forge == nil {
		t.Fatal("forgeFor returned no forge")
	}
	byName := map[string]server.Tier{}
	for _, tr := range tiers {
		byName[tr.Platform] = tr.Tier
	}
	if byName["gh"] != server.TierUser {
		t.Errorf("gh tier = %q, want user", byName["gh"])
	}
	if byName["cf"] != server.TierNone {
		t.Errorf("cf tier = %q, want none", byName["cf"])
	}

	// The resolver hands back the user's token for the platform they
	// have one for, and refuses the one they do not.
	cf := h.cfg.Platforms[1]
	gh := h.cfg.Platforms[0]
	scoped := forge.(*configForge).env
	got, err := scoped.PlatformToken(ctx, gh)
	if err != nil || got != "github_pat_mine" {
		t.Errorf("gh token = %q, %v", got, err)
	}
	if _, err := scoped.PlatformToken(ctx, cf); err == nil {
		t.Error("a platform with no credential resolved a token")
	}
}

// A platform kind with no user tier resolves nothing rather than
// inventing a credential key for it.
func TestCredentialPlatformOfAnUnknownKind(t *testing.T) {
	if _, ok := credentialPlatform(config.Platform{Name: "x", Kind: "bitbucket"}); ok {
		t.Error("an unknown platform kind got a credential key")
	}
	// A Forgejo platform with no usable base URL has no host to key on.
	if _, ok := credentialPlatform(config.Platform{Name: "cf", Kind: config.KindForgejo}); ok {
		t.Error("a Forgejo platform with no base URL got a credential key")
	}
}

func TestAddCredentialHintOfAnUnknownKind(t *testing.T) {
	if got := addCredentialHint(config.Platform{Name: "x", Kind: "bitbucket"}); got == "" {
		t.Error("an unknown platform kind got no hint")
	}
	// A Forgejo platform with no base URL falls back to its name.
	if got := addCredentialHint(config.Platform{Name: "cf", Kind: config.KindForgejo}); !strings.Contains(got, "cf") {
		t.Errorf("hint = %q, want it to name the platform", got)
	}
}

func TestReasonForAnUnknownPlatform(t *testing.T) {
	if got := reasonFor(nil, "absent"); got == "" {
		t.Error("an unknown platform got no reason")
	}
}

// Without a resolver installed, platformToken reads this machine's
// credential store — which is local mode, and every CLI command.
func TestPlatformTokenReadsTheLocalStore(t *testing.T) {
	creds := credentials.NewMemoryStore()
	key, err := credentials.PlatformKey("cf")
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Set(key, "stored-token"); err != nil {
		t.Fatal(err)
	}
	env := &Env{Credentials: creds}
	got, err := env.platformToken(context.Background(), config.Platform{Name: "cf", Kind: config.KindForgejo})
	if err != nil {
		t.Fatalf("platformToken: %v", err)
	}
	if got != "stored-token" {
		t.Errorf("platformToken = %q", got)
	}
}

// An installed resolver takes precedence: that is how hosted mode
// answers per user.
func TestPlatformTokenPrefersTheResolver(t *testing.T) {
	creds := credentials.NewMemoryStore()
	key, _ := credentials.PlatformKey("cf")
	_ = creds.Set(key, "machine-token")
	env := &Env{
		Credentials: creds,
		PlatformToken: func(context.Context, config.Platform) (string, error) {
			return "per-user-token", nil
		},
	}
	got, err := env.platformToken(context.Background(), config.Platform{Name: "cf", Kind: config.KindForgejo})
	if err != nil {
		t.Fatalf("platformToken: %v", err)
	}
	if got != "per-user-token" {
		t.Errorf("platformToken = %q, want the resolver's", got)
	}
}

// No credential store and no resolver is a configuration error, not a
// silent empty token.
func TestPlatformTokenWithoutAStore(t *testing.T) {
	env := &Env{}
	if _, err := env.platformToken(context.Background(), config.Platform{Name: "cf"}); err == nil {
		t.Fatal("a platform resolved with no credential store")
	}
}

func TestOrgTokenEnvVar(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"codefloe", "SPOV_PLATFORM_TOKEN_CODEFLOE"},
		{"my-forge", "SPOV_PLATFORM_TOKEN_MY_FORGE"},
		{"gh.2", "SPOV_PLATFORM_TOKEN_GH_2"},
	} {
		if got := orgTokenEnvVar(tc.in); got != tc.want {
			t.Errorf("orgTokenEnvVar(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEveryForwardedProviderKeyIsStillForwarded guards the variables a
// sandbox needs against an edit to DL-013's catalogue, which is the only list of
// them.
//
// The eight are written out here on purpose. This is the guard, so it
// must not read the source the code reads — that would agree with
// itself however the catalogue changed. Dropping one means sandboxes
// silently stop receiving that key and every turn on it fails at the
// model call, long after the pick that offered it.
//
// Adding a provider is fine and this test says so; removing one is the
// thing to think twice about.
//
// @decision(DL-036) @decision(DL-013)
func TestEveryForwardedProviderKeyIsStillForwarded(t *testing.T) {
	have := map[string]bool{}
	for _, name := range providerEnvVars {
		have[name] = true
	}
	for _, want := range []string{
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"OPENROUTER_API_KEY",
		"GEMINI_API_KEY",
		"GROQ_API_KEY",
		"XAI_API_KEY",
		"DEEPSEEK_API_KEY",
		"MISTRAL_API_KEY",
	} {
		if !have[want] {
			t.Errorf("%s is no longer forwarded into sandboxes; a job that picks its"+
				" provider fails at the model call rather than at the pick", want)
		}
	}
}
