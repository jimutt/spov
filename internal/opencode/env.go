package opencode

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/toolchain"
)

// BasicAuthUser is the username the OpenCode server expects.
//
// @decision(DL-030)
const BasicAuthUser = "opencode"

// PasswordEnvVar carries the Basic auth password to the server.
const PasswordEnvVar = "OPENCODE_PASSWORD"

// passwordBytes is the entropy of a generated session password.
//
// @decision(DL-030)
const passwordBytes = 32

// GeneratePassword returns a fresh session password. An empty password
// disables OpenCode's auth entirely, so generation failure must be fatal
// to the caller rather than silently yielding "".
//
// @decision(DL-030)
func GeneratePassword() (string, error) {
	b := make([]byte, passwordBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("opencode: generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sandboxPath is passed explicitly: a Dockerfile ENV is not inherited by
// the sandbox entrypoint. The toolchain's directories come first, so a
// repository's pinned version, served by mise's shims, wins.
//
// @decision(DL-032)
var sandboxPath = strings.Join(append(toolchain.PathDirs(),
	"/root/.opencode/bin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"), ":")

// EnvConfig describes the environment the server runs with.
//
// @decision(DL-036)
type EnvConfig struct {
	// Password is the Basic auth password. Required.
	Password string
	// ProviderKeys are model provider credentials, injected as
	// environment variables only.
	ProviderKeys map[string]string
	// Addons contribute their environment, such as where Playwright's
	// browsers are, so every process in the sandbox sees it.
	//
	// @decision(DL-033)
	Addons []toolchain.Selection
}

// Env renders the non-secret environment for the sandbox entrypoint:
// PATH and HOME, which a Dockerfile ENV does not supply.
//
// @decision(DL-036) @decision(DL-032)
func (c EnvConfig) Env() (map[string]string, error) {
	if _, err := c.Secrets(); err != nil {
		return nil, err
	}
	env := toolchain.Env(c.Addons)
	env["HOME"] = "/root"
	env["PATH"] = sandboxPath
	return env, nil
}

// Secrets renders the credentials for the sandbox entrypoint. Provider
// keys are injected only here: a credential POSTed to OpenCode's API is
// stored in SQLite and permanently outranks the environment variable.
//
// @decision(DL-036)
func (c EnvConfig) Secrets() (map[string]string, error) {
	if c.Password == "" {
		return nil, fmt.Errorf("opencode: password must not be empty; an empty password disables authentication")
	}
	secrets := map[string]string{PasswordEnvVar: c.Password}
	reserved := map[string]bool{"HOME": true, "PATH": true, PasswordEnvVar: true, credproxy.EnvMainCA: true}
	for k := range credproxy.MainEnv() {
		reserved[k] = true
	}

	for k, v := range c.ProviderKeys {
		if k == "" {
			return nil, fmt.Errorf("opencode: provider key with an empty name")
		}
		if reserved[k] {
			return nil, fmt.Errorf("opencode: provider key %q would overwrite a required variable", k)
		}
		secrets[k] = v
	}
	return secrets, nil
}

// ProviderKeyNames lists the provider key names in sorted order. It exists
// so callers can log which keys were injected without logging values.
//
// @decision(DL-036)
func (c EnvConfig) ProviderKeyNames() []string {
	names := make([]string, 0, len(c.ProviderKeys))
	for k := range c.ProviderKeys {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}
