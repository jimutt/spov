package cli

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/server/client"
)

// keyedFixture is a fixture whose server holds a credential key, so the
// /me/credentials endpoints are writable.
func keyedFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	vars := f.serverEnv.Environ.(config.MapEnv).Vars
	vars[CredentialKeyEnvVar] = base64.StdEncoding.EncodeToString(make([]byte, auth.KeyBytes))
	// The fixture's platform is a fake, so verification succeeds
	// against it rather than reaching a real forge.
	f.http.Close()
	f.srv.Close()
	f.startServer()
	f.env.Client = client.New(f.http.URL, f.token)
	asHosted(f)
	return f
}

// hostedFixture is the API fixture addressed as a hosted server, so the
// credential commands act on it rather than on this machine's store.
//
// @decision(DL-008)
func hostedFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	asHosted(f)
	return f
}

// asHosted names the fixture's server in SPOV_SERVER, the way a user
// points the CLI at a hosted one. Env.ServerURL is the --server flag,
// which NewRoot resets.
func asHosted(f *apiFixture) {
	f.env.Environ.(config.MapEnv).Vars[ServerEnvVar] = f.http.URL
}

// A server with no SPOV_CREDENTIAL_KEY must relay that reason rather
// than failing obscurely.
func TestCredentialsCommandWithoutAKey(t *testing.T) {
	f := hostedFixture(t)
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "github_pat_secret", nil }
	_, _, err := runWith(t, env, "credentials", "add", "github")
	if err == nil {
		t.Fatal("a credential was stored on a server with no key")
	}
	if !strings.Contains(err.Error(), "SPOV_CREDENTIAL_KEY") {
		t.Errorf("err = %v; it should name the missing key", err)
	}
}

// A server with no key holds nothing by construction, which is not the
// same as a user who has stored nothing. The listing must say which.
func TestCredentialsListOnAKeylessServer(t *testing.T) {
	f := hostedFixture(t)
	out, _, err := runWith(t, f.env, "credentials")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if !strings.Contains(out, "SPOV_CREDENTIAL_KEY") {
		t.Errorf("out = %q; it should say the server stores none", out)
	}
}

// On a server that can hold credentials, an empty listing means the
// user has stored none.
func TestCredentialsListEmpty(t *testing.T) {
	f := keyedFixture(t)
	out, _, err := runWith(t, f.env, "credentials")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if !strings.Contains(out, "No credentials stored") {
		t.Errorf("out = %q", out)
	}
}

// A prompted secret must not be echoed into stdout or stderr.
func TestLoginDoesNotEchoTheSecret(t *testing.T) {
	f := hostedFixture(t)
	const secret = "github_pat_a_distinctive_secret"
	env := f.env
	env.PromptSecret = func(string) (string, error) { return secret, nil }
	out, errOut, _ := runWith(t, env, "credentials", "add", "github")
	if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
		t.Errorf("the secret was echoed:\nout=%q\nerr=%q", out, errOut)
	}
}

// An empty token is refused before any request is made.
func TestLoginRefusesAnEmptyToken(t *testing.T) {
	f := hostedFixture(t)
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "   ", nil }
	_, _, err := runWith(t, env, "credentials", "add", "github")
	if err == nil {
		t.Fatal("an empty token was accepted")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v", err)
	}
}

// A hosted server verifies everything it stores, so --no-verify is a
// mistake worth naming rather than ignoring.
//
// @decision(DL-008)
func TestAHostedServerRefusesNoVerify(t *testing.T) {
	f := hostedFixture(t)
	_, _, err := runWith(t, f.env, "credentials", "add", "github", "--token", "t", "--no-verify")
	if err == nil || !strings.Contains(err.Error(), "checks what it stores") {
		t.Fatalf("err = %v", err)
	}
}

// The App flags configure this machine's credential, not the server's.
//
// @decision(DL-008)
func TestAHostedServerRefusesAppFlags(t *testing.T) {
	f := hostedFixture(t)
	_, _, err := runWith(t, f.env, "credentials", "add", "github", "--app-id", "123")
	if err == nil {
		t.Fatal("--app-id was accepted for a hosted server")
	}
	if !strings.Contains(err.Error(), "not an App") {
		t.Errorf("err = %v", err)
	}
}

// The whole round trip through the CLI: store, list, remove.
func TestCredentialsRoundTripThroughTheCLI(t *testing.T) {
	f := keyedFixture(t)
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "a-forgejo-token", nil }

	out, _, err := runWith(t, env, "credentials", "add", "forgejo:codefloe.com")
	if err != nil {
		t.Fatalf("credentials add forgejo: %v", err)
	}
	if !strings.Contains(out, "Stored forgejo:codefloe.com") {
		t.Errorf("out = %q", out)
	}

	out, _, err = runWith(t, env, "credentials")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if !strings.Contains(out, "forgejo:codefloe.com") {
		t.Errorf("the listing lost the credential: %q", out)
	}
	if strings.Contains(out, "a-forgejo-token") {
		t.Errorf("the listing shows the token: %q", out)
	}

	if _, _, err := runWith(t, env, "credentials", "remove", "forgejo:codefloe.com"); err != nil {
		t.Fatalf("credentials remove: %v", err)
	}
	out, _, err = runWith(t, env, "credentials")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if !strings.Contains(out, "No credentials stored") {
		t.Errorf("the credential survived the remove: %q", out)
	}
}

// A GitHub PAT verified through the fixture's platform stores and
// reports the account it belongs to.
func TestLoginGitHubPATStores(t *testing.T) {
	f := keyedFixture(t)
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "github_pat_secret", nil }
	out, _, err := runWith(t, env, "credentials", "add", "github")
	if err != nil {
		t.Fatalf("credentials add github: %v", err)
	}
	if !strings.Contains(out, "Stored github") {
		t.Errorf("out = %q", out)
	}
	if strings.Contains(out, "github_pat_secret") {
		t.Errorf("the output shows the token: %q", out)
	}
}

// A provider key goes to a hosted server like any other credential, and
// there is nothing to verify it against. It is named by its variable.
func TestLoginProviderOnServer(t *testing.T) {
	f := keyedFixture(t)
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "sk-ant-secret", nil }
	out, _, err := runWith(t, env, "credentials", "add", "ANTHROPIC_API_KEY")
	if err != nil {
		t.Fatalf("credentials add ANTHROPIC_API_KEY: %v", err)
	}
	if !strings.Contains(out, "Stored ANTHROPIC_API_KEY on the server") {
		t.Errorf("out = %q", out)
	}

	out, _, err = runWith(t, env, "credentials")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if !strings.Contains(out, "  ANTHROPIC_API_KEY") || strings.Contains(out, "provider:") {
		t.Errorf("the listing does not name the provider key as add takes it: %q", out)
	}
	if strings.Contains(out, "sk-ant-secret") {
		t.Errorf("the listing shows the key: %q", out)
	}
}

// A token the platform rejects must not be stored, and the CLI must
// relay why without echoing the token.
func TestLoginRelaysAVerificationFailure(t *testing.T) {
	f := keyedFixture(t)
	f.platform.ListErr = errors.New("401 Bad credentials")
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "a-bad-token", nil }
	_, _, err := runWith(t, env, "credentials", "add", "forgejo:codefloe.com")
	if err == nil {
		t.Fatal("a rejected token was stored")
	}
	if !strings.Contains(err.Error(), "rejected the credential") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(err.Error(), "a-bad-token") {
		t.Errorf("the error echoes the token: %v", err)
	}
}

// A token that authenticates but reaches nothing is refused, with a
// message that says what to do about it.
func TestLoginRefusesATokenThatReachesNothing(t *testing.T) {
	f := keyedFixture(t)
	f.platform.Repos = nil
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "a-narrow-token", nil }
	_, _, err := runWith(t, env, "credentials", "add", "forgejo:codefloe.com")
	if err == nil {
		t.Fatal("a token reaching no repositories was stored")
	}
	if !strings.Contains(err.Error(), "no repositories") {
		t.Errorf("err = %v", err)
	}
}

// --forgejo names a host the server has no platform for.
func TestLoginForgejoUnknownHost(t *testing.T) {
	f := keyedFixture(t)
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "token", nil }
	_, _, err := runWith(t, env, "credentials", "add", "forgejo:nowhere.example")
	if err == nil {
		t.Fatal("a token for an unconfigured host was stored")
	}
	if !strings.Contains(err.Error(), "no forgejo platform") {
		t.Errorf("err = %v", err)
	}
}

// An unknown platform is refused by the server before anything is
// stored.
func TestCredentialsRemoveRejectsAnUnknownPlatform(t *testing.T) {
	f := keyedFixture(t)
	_, _, err := runWith(t, f.env, "credentials", "remove", "gitlab")
	if err == nil {
		t.Fatal("an unknown platform was accepted")
	}
}

// `credentials remove` must take what the user typed at login as well
// as the key the listing shows — and must remove the row either way,
// rather than reporting success against a spelling that matches nothing.
func TestCredentialsRemoveAcceptsBothSpellings(t *testing.T) {
	for _, tc := range []struct{ name, stored, remove string }{
		{"canonical", "provider:ANTHROPIC_API_KEY", "provider:ANTHROPIC_API_KEY"},
		{"bare variable", "provider:ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"},
		{"lower case", "provider:ANTHROPIC_API_KEY", "anthropic_api_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := keyedFixture(t)
			env := f.env
			env.PromptSecret = func(string) (string, error) { return "sk-ant-secret", nil }
			if _, _, err := runWith(t, env, "credentials", "add", "ANTHROPIC_API_KEY"); err != nil {
				t.Fatalf("login: %v", err)
			}
			if _, _, err := runWith(t, env, "credentials", "remove", tc.remove); err != nil {
				t.Fatalf("remove %q: %v", tc.remove, err)
			}
			out, _, err := runWith(t, env, "credentials")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "No credentials stored") {
				t.Errorf("removing %q left %q", tc.remove, strings.TrimSpace(out))
			}
		})
	}
}

// A case-variant host must remove the row it names, not report success
// against nothing — a user who believes they revoked a token that is
// still live is the failure that matters here.
func TestCredentialsRemoveIsCaseInsensitive(t *testing.T) {
	f := keyedFixture(t)
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "a-token", nil }
	if _, _, err := runWith(t, env, "credentials", "add", "forgejo:codefloe.com"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, _, err := runWith(t, env, "credentials", "remove", "forgejo:CODEFLOE.COM"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	out, _, err := runWith(t, env, "credentials")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No credentials stored") {
		t.Errorf("the credential survived a case-variant remove: %q", strings.TrimSpace(out))
	}
}

// The configured hosts are named, since a bare host and a ported one
// are the common confusion.
func TestLoginForgejoUnknownHostNamesTheConfiguredOnes(t *testing.T) {
	f := keyedFixture(t)
	env := f.env
	env.PromptSecret = func(string) (string, error) { return "token", nil }
	_, _, err := runWith(t, env, "credentials", "add", "forgejo:nowhere.example")
	if err == nil {
		t.Fatal("a token for an unconfigured host was stored")
	}
	if !strings.Contains(err.Error(), "codefloe.com") {
		t.Errorf("err = %v; it should name the configured host", err)
	}
}

func TestCredentialsRemoveNamesWhatItRemoved(t *testing.T) {
	f := keyedFixture(t)
	out, _, err := runWith(t, f.env, "credentials", "remove", "github")
	if err != nil {
		t.Fatalf("credentials remove: %v", err)
	}
	if !strings.Contains(out, "Removed github") {
		t.Errorf("out = %q", out)
	}
}
