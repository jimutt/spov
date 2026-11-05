package opencode

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

func TestGeneratePassword(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		p, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if p == "" {
			t.Fatal("an empty password disables authentication entirely")
		}
		raw, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil {
			t.Fatalf("password is not base64url: %v", err)
		}
		if len(raw) != passwordBytes {
			t.Fatalf("password carries %d bytes of entropy, want %d", len(raw), passwordBytes)
		}
		if seen[p] {
			t.Fatal("generated the same password twice")
		}
		seen[p] = true
		// It ends up in a URL and a Basic auth header.
		if strings.ContainsAny(p, "+/=: \n") {
			t.Fatalf("password %q contains characters that need escaping", p)
		}
	}
}

func TestEnvRequiresAPassword(t *testing.T) {
	// Both accessors validate, so neither can be used to bypass it.
	if _, err := (EnvConfig{}).Secrets(); err == nil {
		t.Error("Secrets accepted an empty password")
	}
	_, err := EnvConfig{}.Env()
	if err == nil {
		t.Fatal("an empty password must be rejected")
	}
	if !strings.Contains(err.Error(), "disables authentication") {
		t.Errorf("error %q does not explain why", err)
	}
}

// TestEnvAndSecretsAreSeparate: credentials travel to Modal through an
// ephemeral Secret, which its control plane redacts, while PATH and HOME
// are ordinary metadata.
//
// @decision(DL-036)
func TestEnvAndSecretsAreSeparate(t *testing.T) {
	c := EnvConfig{
		Password:     "s3cret",
		ProviderKeys: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"},
	}
	env, err := c.Env()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := c.Secrets()
	if err != nil {
		t.Fatal(err)
	}

	// A Dockerfile ENV is not inherited by the sandbox entrypoint, so
	// PATH and HOME must be set explicitly or the binary is not found.
	if env["HOME"] != "/root" {
		t.Errorf("HOME = %q", env["HOME"])
	}
	if !strings.Contains(env["PATH"], "/usr/local/bin") {
		t.Errorf("PATH = %q", env["PATH"])
	}
	// Web search is pinned in the config file, not the environment,
	// which GA does not read (DL-035).
	if len(env) != 2 {
		t.Errorf("env = %v, want HOME and PATH alone", env)
	}

	if secrets[PasswordEnvVar] != "s3cret" {
		t.Errorf("%s = %q", PasswordEnvVar, secrets[PasswordEnvVar])
	}
	if secrets["ANTHROPIC_API_KEY"] != "sk-ant-x" {
		t.Error("provider key was not injected")
	}

	// No credential may appear in the plain environment.
	for k, v := range env {
		if v == "s3cret" || v == "sk-ant-x" {
			t.Errorf("a credential leaked into plain env as %q", k)
		}
	}
}

func TestEnvRejectsClashingProviderKeys(t *testing.T) {
	for _, name := range []string{PasswordEnvVar, "PATH", "HOME"} {
		t.Run(name, func(t *testing.T) {
			c := EnvConfig{Password: "x", ProviderKeys: map[string]string{name: "clobber"}}
			if _, err := c.Secrets(); err == nil {
				t.Errorf("a provider key named %q was allowed to overwrite a required variable", name)
			}
		})
	}
}

func TestEnvRejectsEmptyProviderKeyName(t *testing.T) {
	c := EnvConfig{Password: "x", ProviderKeys: map[string]string{"": "v"}}
	if _, err := c.Secrets(); err == nil {
		t.Fatal("expected an error")
	}
}

func TestProviderKeyNamesAreSortedAndValueFree(t *testing.T) {
	c := EnvConfig{
		Password: "x",
		ProviderKeys: map[string]string{
			"OPENAI_API_KEY":    "sk-openai-secret",
			"ANTHROPIC_API_KEY": "sk-ant-secret",
		},
	}
	names := c.ProviderKeyNames()
	want := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
	if !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	// The names are logged, so the function must return keys only. Build
	// the expected set from the map's own keys so this stays honest if
	// the fixture changes.
	for _, n := range names {
		if _, ok := c.ProviderKeys[n]; !ok {
			t.Errorf("%q is not one of the configured key names", n)
		}
	}
	if len(names) != len(c.ProviderKeys) {
		t.Errorf("got %d names for %d keys", len(names), len(c.ProviderKeys))
	}
}
