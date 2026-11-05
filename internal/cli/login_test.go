package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server/client"
)

// loginFixture is a configured two-platform environment with an empty
// credential store, which is what a first login starts from.
type loginFixture struct {
	env      Env
	store    *credentials.MemoryStore
	platform *git.Fake
	keyFile  string
	prompted []string
	reply    string
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	wd := t.TempDir()
	home := t.TempDir()
	writeUserConfig(t, home, `platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
  - name: gh
    kind: github
    account: alice
`)

	f := &loginFixture{
		store:    credentials.NewMemoryStore(),
		platform: git.NewFake(),
		keyFile:  filepath.Join(t.TempDir(), "app.pem"),
	}
	// A syntactically plausible key; login does not parse it, and the
	// platform is faked.
	if err := os.WriteFile(f.keyFile,
		[]byte("-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// No SPOV_SERVER: these tests are about this machine's store, which
	// only a local server reads.
	f.env = Env{
		Workdir:     wd,
		Environ:     config.MapEnv{Vars: map[string]string{"HOME": home, "XDG_CONFIG_HOME": home}},
		Credentials: f.store,
		// A server that refuses at once: the local listing asks it only
		// for the OpenCode line, and must not spawn one here.
		Client: client.New("http://127.0.0.1:1", ""),
		NewPlatform: func(context.Context, config.Platform, string) (git.Platform, error) {
			return f.platform, nil
		},
		PromptSecret: func(prompt string) (string, error) {
			f.prompted = append(f.prompted, prompt)
			return f.reply, nil
		},
	}
	return f
}

func (f *loginFixture) stored(t *testing.T, platform string) string {
	t.Helper()
	key, err := credentials.PlatformKey(platform)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Get(key)
	if err != nil {
		t.Fatalf("nothing stored for %s: %v", platform, err)
	}
	return got
}

func TestLoginForgejoStoresTheToken(t *testing.T) {
	f := newLoginFixture(t)

	out, _, err := runWith(t, f.env, "credentials", "add", "codefloe", "--token", "tok-123")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.stored(t, "codefloe"); got != "tok-123" {
		t.Errorf("stored %q, want the token", got)
	}
	if !strings.Contains(out, "codefloe") {
		t.Errorf("output does not name the platform:\n%s", out)
	}
}

// TestLoginPromptsWhenTheTokenIsOmitted keeps the token off the command
// line, where it would land in shell history and the process list.
//
// @decision(DL-008)
func TestLoginPromptsWhenTheTokenIsOmitted(t *testing.T) {
	f := newLoginFixture(t)
	f.reply = "prompted-token"

	if _, _, err := runWith(t, f.env, "credentials", "add", "codefloe"); err != nil {
		t.Fatal(err)
	}
	if len(f.prompted) != 1 {
		t.Fatalf("prompted %d times, want 1", len(f.prompted))
	}
	if got := f.stored(t, "codefloe"); got != "prompted-token" {
		t.Errorf("stored %q, want the prompted value", got)
	}
}

// TestLoginGitHubBuildsTheAppCredential: the user supplies a key path and
// spov does the JSON encoding, which is the whole point of the command.
//
// @decision(DL-008) @decision(DL-041)
func TestLoginGitHubBuildsTheAppCredential(t *testing.T) {
	f := newLoginFixture(t)
	f.platform.PlatformKind = "github"

	if _, _, err := runWith(t, f.env, "credentials", "add", "gh",
		"--app-id", "Iv23liABC", "--key-file", f.keyFile, "--installation-id", "42"); err != nil {
		t.Fatal(err)
	}

	var cred git.GitHubCredential
	if err := json.Unmarshal([]byte(f.stored(t, "gh")), &cred); err != nil {
		t.Fatalf("what was stored is not a GitHub credential: %v", err)
	}
	if cred.AppID != "Iv23liABC" {
		t.Errorf("AppID = %q", cred.AppID)
	}
	if cred.InstallationID != 42 {
		t.Errorf("InstallationID = %d", cred.InstallationID)
	}
	if !strings.Contains(cred.PrivateKey, "BEGIN RSA PRIVATE KEY") {
		t.Errorf("the key file contents did not reach the credential: %q", cred.PrivateKey)
	}
	// Round-trips through the parser the launch path uses.
	if _, err := git.ParseGitHubCredential(f.stored(t, "gh")); err != nil {
		t.Errorf("the stored credential does not parse: %v", err)
	}
}

func TestLoginGitHubInstallationIDIsOptional(t *testing.T) {
	f := newLoginFixture(t)
	f.platform.PlatformKind = "github"

	if _, _, err := runWith(t, f.env, "credentials", "add", "gh", "--app-id", "a", "--key-file", f.keyFile); err != nil {
		t.Fatal(err)
	}
	cred, err := git.ParseGitHubCredential(f.stored(t, "gh"))
	if err != nil {
		t.Fatal(err)
	}
	if cred.InstallationID != 0 {
		t.Errorf("InstallationID = %d, want 0 when not given", cred.InstallationID)
	}
}

// TestLoginRejectsMismatchedFlags: GitHub flags on a Forgejo platform,
// and the reverse, are user errors worth naming.
func TestLoginRejectsMismatchedFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"github flags on forgejo", []string{"credentials", "add", "codefloe", "--app-id", "x"}, "forgejo"},
		{"token on github", []string{"credentials", "add", "gh", "--token", "x"}, "App credential"},
		{"github without app id", []string{"credentials", "add", "gh", "--key-file", "/x"}, "app-id"},
		{"github without key file", []string{"credentials", "add", "gh", "--app-id", "x"}, "key-file"},
		{"unknown platform", []string{"credentials", "add", "nope", "--token", "x"}, "unknown platform"},
		{"a platform given to login", []string{"login", "codefloe"}, "spov credentials add codefloe"},
		// `spov login` alone targets the server; with none configured
		// it says so.
		//
		// @decision(DL-006)
		{"neither", []string{"login"}, "no server configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.platform.PlatformKind = "github"
			_, _, err := runWith(t, f.env, tt.args...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoginGitHubReportsAMissingKeyFile(t *testing.T) {
	f := newLoginFixture(t)
	f.platform.PlatformKind = "github"

	_, _, err := runWith(t, f.env, "credentials", "add", "gh", "--app-id", "a", "--key-file", "/no/such/key.pem")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "private key") {
		t.Errorf("err = %v", err)
	}
}

// TestLoginVerifiesBeforeStoring is the reason the command contacts the
// platform at all: a bad credential must fail here, not partway through
// creating a billable sandbox.
//
// @decision(DL-008)
func TestLoginVerifiesBeforeStoring(t *testing.T) {
	f := newLoginFixture(t)
	f.platform.ListErr = errors.New("401 unauthorized")

	_, _, err := runWith(t, f.env, "credentials", "add", "codefloe", "--token", "wrong")
	if err == nil {
		t.Fatal("a rejected credential was accepted")
	}
	if !strings.Contains(err.Error(), "not stored") {
		t.Errorf("err = %v; it should say the credential was not stored", err)
	}
	if len(f.store.Snapshot()) != 0 {
		t.Errorf("a credential that failed verification was stored: %v", f.store.Snapshot())
	}
}

// TestLoginVerificationErrorHidesTheSecret: a rejected token is the one
// case where the server is handed the secret and answers with it —
// Forgejo replies `access token does not exist [sha: <token>]`.
//
// @decision(DL-008)
func TestLoginVerificationErrorHidesTheSecret(t *testing.T) {
	f := newLoginFixture(t)
	f.platform.ListErr = errors.New(`access token does not exist [sha: SUPERSECRET123]`)

	_, stderr, err := runWith(t, f.env, "credentials", "add", "codefloe", "--token", "SUPERSECRET123")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "SUPERSECRET123") {
		t.Errorf("the error carries the token: %v", err)
	}
	if strings.Contains(stderr, "SUPERSECRET123") {
		t.Errorf("the token was printed:\n%s", stderr)
	}
}

// TestLoginNoVerifyStoresWithoutListing covers the offline path.
//
// @decision(DL-008)
func TestLoginNoVerifyStoresWithoutListing(t *testing.T) {
	f := newLoginFixture(t)
	f.platform.ListErr = errors.New("no network")

	if _, _, err := runWith(t, f.env, "credentials", "add", "codefloe", "--token", "tok", "--no-verify"); err != nil {
		t.Fatalf("--no-verify still verified: %v", err)
	}
	if got := f.stored(t, "codefloe"); got != "tok" {
		t.Errorf("stored %q", got)
	}
}

func TestLoginReportsAStoreFailure(t *testing.T) {
	f := newLoginFixture(t)
	f.store.SetErr = errors.New("keyring is locked")

	_, _, err := runWith(t, f.env, "credentials", "add", "codefloe", "--token", "tok")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "keyring is locked") {
		t.Errorf("err = %v", err)
	}
}

// TestLoginProviderStoresUnderTheProviderKind.
//
// @decision(DL-008)
func TestLoginProviderStoresUnderTheProviderKind(t *testing.T) {
	f := newLoginFixture(t)

	if _, _, err := runWith(t, f.env, "credentials", "add", "OPENROUTER_API_KEY", "--token", "sk-or-1"); err != nil {
		t.Fatal(err)
	}
	key, _ := credentials.ProviderKey("OPENROUTER_API_KEY")
	got, err := f.store.Get(key)
	if err != nil || got != "sk-or-1" {
		t.Errorf("stored %q, %v", got, err)
	}
}

// TestLoginProviderWarnsWhenTheEnvironmentWins: the stored value would
// silently not be the one used.
//
// @decision(DL-008) @decision(DL-036)
func TestLoginProviderWarnsWhenTheEnvironmentWins(t *testing.T) {
	f := newLoginFixture(t)
	f.env.Environ.(config.MapEnv).Vars["OPENROUTER_API_KEY"] = "from-env"

	_, stderr, err := runWith(t, f.env, "credentials", "add", "OPENROUTER_API_KEY", "--token", "stored")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "precedence") {
		t.Errorf("no note that the environment wins:\n%s", stderr)
	}
}

// A provider name that is not an environment variable is refused, and
// a lower-case name is a platform, not a provider key.
//
// @decision(DL-008)
func TestLoginProviderRejectsABadName(t *testing.T) {
	f := newLoginFixture(t)
	for _, name := range []string{"provider:has-dash", "provider:has space"} {
		_, _, err := runWith(t, f.env, "credentials", "add", name, "--token", "x")
		if err == nil || !strings.Contains(err.Error(), "environment variable name") {
			t.Errorf("%q: err = %v", name, err)
		}
	}
	if got := parseCredentialName("lower_case"); got.Platform != "lower_case" {
		t.Errorf("lower_case parsed as %+v", got)
	}
}

// An upper-case platform name in config is that platform, not a provider
// variable of the same shape; and an alias several platforms answer to is
// refused rather than guessed.
//
// @decision(DL-008)
func TestLocalNamesAreNotGuessed(t *testing.T) {
	f := newLoginFixture(t)
	writeUserConfig(t, f.env.Environ.(config.MapEnv).Vars["XDG_CONFIG_HOME"], `platforms:
  - name: CF
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
  - name: cf2
    kind: forgejo
    base_url: https://codefloe.com
    account: other
`)
	if _, _, err := runWith(t, f.env, "credentials", "add", "CF", "--token", "tok"); err != nil {
		t.Fatalf("add CF: %v", err)
	}
	if got := f.stored(t, "CF"); got != "tok" {
		t.Errorf("CF stored %q", got)
	}
	_, _, err := runWith(t, f.env, "credentials", "add", "forgejo:codefloe.com", "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "CF, cf2") {
		t.Errorf("an ambiguous alias: %v", err)
	}
	out, _, _ := runWith(t, f.env, "credentials")
	if strings.Contains(out, "(forgejo:codefloe.com)") {
		t.Errorf("the listing offers an alias that is ambiguous:\n%s", out)
	}
}

// TestLoginProviderAcceptsAnUnknownVariable: the providerEnvVars list is
// what spov forwards from its own environment, not the set of keys a
// user may store.
func TestLoginProviderAcceptsAnUnknownVariable(t *testing.T) {
	f := newLoginFixture(t)
	if _, _, err := runWith(t, f.env, "credentials", "add", "SOME_NEW_PROVIDER_KEY", "--token", "x"); err != nil {
		t.Errorf("an unlisted provider variable was rejected: %v", err)
	}
}

func TestLogoutRemovesTheCredential(t *testing.T) {
	f := newLoginFixture(t)
	key, _ := credentials.PlatformKey("codefloe")
	if err := f.store.Set(key, "tok"); err != nil {
		t.Fatal(err)
	}

	out, _, err := runWith(t, f.env, "credentials", "remove", "codefloe")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Removed") {
		t.Errorf("output = %q", out)
	}
	if _, err := f.store.Get(key); !errors.Is(err, credentials.ErrNotFound) {
		t.Error("the credential is still stored")
	}
}

// TestLogoutOnNothingStoredIsNotAnError: the end state the user asked
// for is the same either way.
//
// @decision(DL-008)
func TestLogoutOnNothingStoredIsNotAnError(t *testing.T) {
	f := newLoginFixture(t)

	out, _, err := runWith(t, f.env, "credentials", "remove", "codefloe")
	if err != nil {
		t.Fatalf("logging out of a platform with nothing stored failed: %v", err)
	}
	if !strings.Contains(out, "No stored credential") {
		t.Errorf("output = %q", out)
	}
}

func TestLogoutProvider(t *testing.T) {
	f := newLoginFixture(t)
	key, _ := credentials.ProviderKey("OPENROUTER_API_KEY")
	if err := f.store.Set(key, "sk"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := runWith(t, f.env, "credentials", "remove", "OPENROUTER_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Get(key); !errors.Is(err, credentials.ErrNotFound) {
		t.Error("the provider key is still stored")
	}
}

func TestLogoutRejectsBadArguments(t *testing.T) {
	f := newLoginFixture(t)
	for _, args := range [][]string{
		{"logout"},
		{"logout", "codefloe"},
	} {
		if _, _, err := runWith(t, f.env, args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

// TestProviderKeysPreferTheEnvironment: a stored key fills a gap, it does
// not override an exported one.
//
// @decision(DL-008) @decision(DL-036)
func TestProviderKeysPreferTheEnvironment(t *testing.T) {
	store := credentials.NewMemoryStore()
	key, _ := credentials.ProviderKey("OPENROUTER_API_KEY")
	if err := store.Set(key, "from-store"); err != nil {
		t.Fatal(err)
	}
	anthropic, _ := credentials.ProviderKey("ANTHROPIC_API_KEY")
	if err := store.Set(anthropic, "anthropic-stored"); err != nil {
		t.Fatal(err)
	}

	env := &Env{
		Credentials: store,
		Environ:     config.MapEnv{Vars: map[string]string{"OPENROUTER_API_KEY": "from-env"}},
	}
	keys := env.providerKeys()

	if got := keys["OPENROUTER_API_KEY"]; got != "from-env" {
		t.Errorf("OPENROUTER_API_KEY = %q, want the environment to win", got)
	}
	if got := keys["ANTHROPIC_API_KEY"]; got != "anthropic-stored" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the stored value to fill the gap", got)
	}
}

func TestProviderKeysWithoutAStore(t *testing.T) {
	env := &Env{Environ: config.MapEnv{Vars: map[string]string{"OPENAI_API_KEY": "k"}}}
	keys := env.providerKeys()
	if keys["OPENAI_API_KEY"] != "k" {
		t.Errorf("keys = %v", keys)
	}
}

// TestConfigShowReportsStoredCredentials by name only.
//
// @decision(DL-008)
func TestConfigShowReportsStoredCredentials(t *testing.T) {
	f := newLoginFixture(t)
	platformKey, _ := credentials.PlatformKey("codefloe")
	if err := f.store.Set(platformKey, "SECRET-TOKEN"); err != nil {
		t.Fatal(err)
	}
	providerKey, _ := credentials.ProviderKey("OPENROUTER_API_KEY")
	if err := f.store.Set(providerKey, "SECRET-KEY"); err != nil {
		t.Fatal(err)
	}

	out, _, err := runWith(t, f.env, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stored:", "codefloe", "OPENROUTER_API_KEY"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	// gh has nothing stored, so it must not be listed under `stored:`.
	stored := out[strings.Index(out, "stored:"):]
	if strings.Contains(stored, "- gh\n") {
		t.Errorf("a platform with no credential is listed as stored:\n%s", stored)
	}
	for _, secret := range []string{"SECRET-TOKEN", "SECRET-KEY"} {
		if strings.Contains(out, secret) {
			t.Errorf("`config show` printed a secret (%s):\n%s", secret, out)
		}
	}
}

func TestConfigShowOmitsStoredWhenEmpty(t *testing.T) {
	f := newLoginFixture(t)
	out, _, err := runWith(t, f.env, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "stored:") {
		t.Errorf("an empty store produced a `stored:` section:\n%s", out)
	}
}

// TestRedactSecretHidesAGitHubPrivateKey: a GitHub credential is JSON,
// so the key travels escaped. Both forms must be scrubbed.
//
// @decision(DL-008)
func TestRedactSecretHidesAGitHubPrivateKey(t *testing.T) {
	key := "-----BEGIN RSA PRIVATE KEY-----\nSECRETKEYBODY\n-----END RSA PRIVATE KEY-----\n"
	cred, err := json.Marshal(git.GitHubCredential{AppID: "a", PrivateKey: key})
	if err != nil {
		t.Fatal(err)
	}

	// The raw key, as an unescaped component might log it.
	got := redactSecret("failed with "+key, string(cred))
	if strings.Contains(got, "SECRETKEYBODY") {
		t.Errorf("the raw key survived redaction: %q", got)
	}
	// The whole JSON credential, as the secret itself.
	got = redactSecret("failed with "+string(cred), string(cred))
	if strings.Contains(got, "SECRETKEYBODY") {
		t.Errorf("the JSON credential survived redaction: %q", got)
	}
}

func TestRedactSecretHandlesAnEmptySecret(t *testing.T) {
	if got := redactSecret("nothing to hide", ""); got != "nothing to hide" {
		t.Errorf("got %q", got)
	}
}

// TestRedactSecretAlsoScrubsCredentialURLs: git errors name the remote,
// which may carry a credential.
func TestRedactSecretAlsoScrubsCredentialURLs(t *testing.T) {
	got := redactSecret("failed: https://user:hunter2@example.com/r.git", "unrelated")
	if strings.Contains(got, "hunter2") {
		t.Errorf("a credential URL survived: %q", got)
	}
}

// TestLoginPromptRejectsAnEmptyValue: an empty credential would be
// stored and then fail confusingly at launch.
func TestLoginPromptRejectsAnEmptyValue(t *testing.T) {
	f := newLoginFixture(t)
	f.reply = "   "

	if _, _, err := runWith(t, f.env, "credentials", "add", "codefloe"); err == nil {
		t.Fatal("an empty token was accepted")
	}
	if len(f.store.Snapshot()) != 0 {
		t.Error("an empty token was stored")
	}
}

func TestLoginProviderPromptsWhenTheValueIsOmitted(t *testing.T) {
	f := newLoginFixture(t)
	f.reply = "sk-prompted"

	if _, _, err := runWith(t, f.env, "credentials", "add", "OPENROUTER_API_KEY"); err != nil {
		t.Fatal(err)
	}
	key, _ := credentials.ProviderKey("OPENROUTER_API_KEY")
	if got, _ := f.store.Get(key); got != "sk-prompted" {
		t.Errorf("stored %q, want the prompted value", got)
	}
}

func TestLoginPromptFailureIsReported(t *testing.T) {
	f := newLoginFixture(t)
	f.env.PromptSecret = func(string) (string, error) {
		return "", errors.New("no terminal")
	}
	if _, _, err := runWith(t, f.env, "credentials", "add", "codefloe"); err == nil {
		t.Fatal("expected the prompt failure to surface")
	}
	if _, _, err := runWith(t, f.env, "credentials", "add", "OPENROUTER_API_KEY"); err == nil {
		t.Fatal("expected the prompt failure to surface for a provider key")
	}
}

// TestLoginWithoutACredentialStore must not panic.
func TestLoginWithoutACredentialStore(t *testing.T) {
	f := newLoginFixture(t)
	f.env.Credentials = nil

	if _, _, err := runWith(t, f.env, "credentials", "add", "codefloe", "--token", "t"); err == nil {
		t.Error("expected an error with no credential store")
	}
	if _, _, err := runWith(t, f.env, "credentials", "add", "X_KEY", "--token", "t"); err == nil {
		t.Error("expected an error with no credential store")
	}
	if _, _, err := runWith(t, f.env, "credentials", "remove", "codefloe"); err == nil {
		t.Error("expected an error with no credential store")
	}
}

// TestLogoutReportsADeleteFailure: the credential is still there, so
// saying nothing would be a lie.
func TestLogoutReportsADeleteFailure(t *testing.T) {
	f := newLoginFixture(t)
	key, _ := credentials.PlatformKey("codefloe")
	if err := f.store.Set(key, "tok"); err != nil {
		t.Fatal(err)
	}
	f.store.DeleteErr = errors.New("keyring is locked")

	if _, _, err := runWith(t, f.env, "credentials", "remove", "codefloe"); err == nil {
		t.Fatal("a failed delete was reported as success")
	}
}

// TestConfigShowSurvivesABrokenStore: a store that errors must not make
// `config show` fail, since it reports much more than credentials.
//
// @decision(DL-008)
func TestConfigShowSurvivesABrokenStore(t *testing.T) {
	f := newLoginFixture(t)
	f.store.GetErr = errors.New("keyring is locked")

	out, _, err := runWith(t, f.env, "config", "show")
	if err != nil {
		t.Fatalf("a broken store failed `config show`: %v", err)
	}
	if strings.Contains(out, "stored:") {
		t.Errorf("a broken store reported credentials it could not read:\n%s", out)
	}
}

// TestRedactSecretHidesAJSONEscapedKey: a server echoing a private key
// inside a JSON body escapes its newlines, so an exact-value comparison
// misses it, and the key body would reach the user's terminal.
//
// @decision(DL-008)
func TestRedactSecretHidesAJSONEscapedKey(t *testing.T) {
	key := "-----BEGIN RSA PRIVATE KEY-----\nSECRETKEYBODY\n-----END RSA PRIVATE KEY-----\n"
	cred, err := json.Marshal(git.GitHubCredential{AppID: "a", PrivateKey: key})
	if err != nil {
		t.Fatal(err)
	}
	escaped, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		text string
	}{
		{"raw", "failed: " + key},
		{"json-escaped", `{"error":"bad key ` + string(escaped[1:len(escaped)-1]) + `"}`},
		{"whole credential", "sent " + string(cred)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := redactSecret(tt.text, string(cred))
			if strings.Contains(got, "SECRETKEYBODY") {
				t.Errorf("the key survived redaction: %s", got)
			}
		})
	}
}

// TestRedactSecretScrubsAnyPEMBlock: a key that is not the one being
// stored has no business reaching the terminal either.
//
// @decision(DL-008)
func TestRedactSecretScrubsAnyPEMBlock(t *testing.T) {
	got := redactSecret("-----BEGIN PRIVATE KEY-----\nOTHERKEY\n-----END PRIVATE KEY-----", "unrelated-token")
	if strings.Contains(got, "OTHERKEY") {
		t.Errorf("an unrelated PEM block survived: %s", got)
	}
}

// TestRedactSecretKeepsShortValuesReadable: a one-character credential
// would rewrite unrelated words, turning "connection refused" into
// "conn***ction r***fus***d" exactly when the diagnostic is needed.
//
// @decision(DL-008)
func TestRedactSecretKeepsShortValuesReadable(t *testing.T) {
	got := redactSecret("dial tcp: connection refused: no route to host", "e")
	if got != "dial tcp: connection refused: no route to host" {
		t.Errorf("a short secret shredded the message: %q", got)
	}
	// A credential of realistic length is still removed.
	got = redactSecret("rejected token ghp_realistic_length_token", "ghp_realistic_length_token")
	if strings.Contains(got, "ghp_realistic_length_token") {
		t.Errorf("a real token survived: %q", got)
	}
}

// TestLoginTrimsThePromptedSecret: a token pasted with a stray space
// would be stored verbatim and fail at the next launch.
//
// @decision(DL-008)
func TestLoginTrimsThePromptedSecret(t *testing.T) {
	f := newLoginFixture(t)
	f.reply = "  tok-with-space\n"

	if _, _, err := runWith(t, f.env, "credentials", "add", "codefloe"); err != nil {
		t.Fatal(err)
	}
	if got := f.stored(t, "codefloe"); got != "tok-with-space" {
		t.Errorf("stored %q, want it trimmed", got)
	}
}

func TestLoginTrimsAProviderValue(t *testing.T) {
	f := newLoginFixture(t)

	if _, _, err := runWith(t, f.env, "credentials", "add", "OPENROUTER_API_KEY", "--token", " sk-or-1 "); err != nil {
		t.Fatal(err)
	}
	key, _ := credentials.ProviderKey("OPENROUTER_API_KEY")
	if got, _ := f.store.Get(key); got != "sk-or-1" {
		t.Errorf("stored %q, want it trimmed", got)
	}
}

// TestLoginProviderRejectsPlatformFlags: silently ignoring them would
// accept a mistyped command and store the wrong thing.
//
// @decision(DL-008)
func TestLoginProviderRejectsPlatformFlags(t *testing.T) {
	for _, args := range [][]string{
		{"credentials", "add", "X_KEY", "--token", "t", "--app-id", "a"},
		{"credentials", "add", "X_KEY", "--token", "t", "--key-file", "/x"},
		{"credentials", "add", "X_KEY", "--token", "t", "--installation-id", "1"},
	} {
		f := newLoginFixture(t)
		if _, _, err := runWith(t, f.env, args...); err == nil {
			t.Errorf("%v was accepted", args)
		} else if len(f.store.Snapshot()) != 0 {
			t.Errorf("%v stored something despite failing", args)
		}
	}
}

// A platform has one name in every mode: its name in config, or the name
// a hosted server uses for it (github, forgejo:<host>). Listing and
// removing take the same names.
//
// @decision(DL-008)
func TestLocalCredentialsTakeTheServersNames(t *testing.T) {
	f := newLoginFixture(t)
	f.reply = "tok-123"
	if _, _, err := runWith(t, f.env, "credentials", "add", "forgejo:CodeFloe.com"); err != nil {
		t.Fatalf("add forgejo:<host>: %v", err)
	}
	if got := f.stored(t, "codefloe"); got != "tok-123" {
		t.Errorf("stored under codefloe = %q", got)
	}
	if _, _, err := runWith(t, f.env, "credentials", "add", "github", "--app-id", "a", "--key-file", f.keyFile); err != nil {
		t.Fatalf("add github: %v", err)
	}
	if f.stored(t, "gh") == "" {
		t.Error("github did not reach the platform configured as gh")
	}
	if _, _, err := runWith(t, f.env, "credentials", "add", "OPENROUTER_API_KEY", "--token", "sk-or"); err != nil {
		t.Fatal(err)
	}

	out, _, err := runWith(t, f.env, "credentials", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"Stored on this machine", "codefloe  (forgejo:codefloe.com)", "gh  (github)",
		"OPENROUTER_API_KEY", "a key stored under another name is not listed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"tok-123", "sk-or", "PRIVATE KEY"} {
		if strings.Contains(out, secret) {
			t.Errorf("the listing shows %q", secret)
		}
	}

	// --json takes the hosted listing's shape, a platform under its
	// configured name and a provider key as the server keys it.
	//
	// @decision(DL-023)
	out, stderr, err := runWith(t, f.env, "credentials", "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var body credentialsBody
	decodeOnly(t, out, &body)
	var names []string
	for _, c := range body.Credentials {
		names = append(names, c.Platform)
	}
	if got := strings.Join(names, " "); got != "codefloe gh provider:OPENROUTER_API_KEY" || !body.Enabled {
		t.Errorf("list --json named %q:\n%s", got, out)
	}
	if !strings.Contains(stderr, "a key stored under another name is not listed") {
		t.Errorf("the note on what is checked left stderr:\n%s", stderr)
	}
	// remove completes the same names, with a provider key bare, from
	// the store alone.
	//
	// @decision(DL-023)
	quiet := f.env
	quiet.Spawn = func(context.Context) error { t.Error("a completion spawned a server"); return nil }
	if got := strings.Join(completions(t, quiet, "credentials", "remove", ""), " "); got != "OPENROUTER_API_KEY codefloe gh" {
		t.Errorf("remove offers %s", got)
	}

	out, _, err = runWith(t, f.env, "credentials", "remove", "github")
	if err != nil || !strings.Contains(out, "Removed the credential for gh from this machine") {
		t.Fatalf("remove github: %q, %v", out, err)
	}
	out, _, _ = runWith(t, f.env, "credentials")
	if strings.Contains(out, "gh  (github)") || !strings.Contains(out, "codefloe") {
		t.Errorf("after removing github:\n%s", out)
	}
}
