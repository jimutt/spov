package opencode

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/toolchain"
)

// The bounds on a session's own environment variables.
//
// @decision(DL-097)
const (
	MaxUserEnvVars     = 64
	MaxUserEnvName     = 128
	MaxUserEnvValue    = 4 << 10
	MaxUserEnvTotal    = 32 << 10
	userEnvSecretsHint = "; put a token in a routed credential (spov credentials add <name> --host ...) instead"
)

// ErrUserEnvTooLarge is a set of variables over the count or size bound.
var ErrUserEnvTooLarge = errors.New("too many environment variables")

var userEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// refusedUserEnvPrefixes are the prefixes no session variable may have.
//
// @decision(DL-097)
var refusedUserEnvPrefixes = []string{"SPOV_", "GIT_", "LD_", "XDG_", "MISE_", "OPENCODE_PTY_", "OPENCODE_SSH_ASKPASS_"}

// refusedUserEnv are the shell, proxy and trust-store variables no
// session variable may set.
//
// @decision(DL-097)
var refusedUserEnv = []string{
	"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "IFS",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	"JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS",
}

// forgeCredentialEnv are the forge CLIs' token and host overrides, which
// outrank the files spov writes for them.
//
// @decision(DL-097)
var forgeCredentialEnv = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST", "GH_CONFIG_DIR",
	"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "GITLAB_HOST", "GITLAB_URI", "GL_HOST", "GLAB_CONFIG_DIR",
	"GITEA_TOKEN", "GITEA_SERVER_URL",
}

// RefusedOpenCodeEnv are the OPENCODE_* variables a session may not set,
// read out of the pinned build; every other OPENCODE_* is allowed. A pin
// bump compares the new build's OPENCODE_* names with the last pin's.
//
// @decision(DL-097)
var RefusedOpenCodeEnv = []string{
	PasswordEnvVar, "OPENCODE_SERVER_PASSWORD", "OPENCODE_API_KEY",
	"OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CONFIG_DIR", "OPENCODE_CLI_CONFIG_CONTENT",
	"OPENCODE_CONFIG_PROJECT_DISABLE", "OPENCODE_DISABLE_PROJECT_CONFIG",
	"OPENCODE_MODELS_URL", "OPENCODE_MODELS_PATH",
	"OPENCODE_CLIENT", "OPENCODE_ROUTE", "OPENCODE_SESSION_ID",
}

// SpovEnvNames are the variables spov sets in a session's main container
// whatever it attaches: the base, the add-ons', the proxy's and the
// provider keys. A launch adds placeholders and tokens, all SPOV_*.
//
// @decision(DL-097)
func SpovEnvNames(addons []toolchain.Selection, providerKeyEnvs []string) []string {
	names := []string{"HOME", "PATH", PasswordEnvVar, credproxy.EnvMainCA}
	names = slices.AppendSeq(names, maps.Keys(toolchain.Env(addons)))
	names = slices.AppendSeq(names, maps.Keys(credproxy.MainEnv()))
	return append(names, providerKeyEnvs...)
}

// CheckUserEnv refuses a session's own environment variables when any
// sets a name in spov, or one DL-097 refuses, or breaks a bound. Names
// are compared without regard to case. The first problem, by name, is
// the one reported.
//
// @decision(DL-097)
func CheckUserEnv(env map[string]string, spov []string) error {
	if len(env) > MaxUserEnvVars {
		return fmt.Errorf("%w: %d; the limit is %d", ErrUserEnvTooLarge, len(env), MaxUserEnvVars)
	}
	taken := map[string]bool{}
	for _, n := range spov {
		taken[strings.ToUpper(n)] = true
	}
	total := 0
	for _, name := range slices.Sorted(maps.Keys(env)) {
		value := env[name]
		total += len(name) + len(value)
		if why := userEnvProblem(name, value, taken); why != "" {
			return fmt.Errorf("environment variable %q %s", name, why)
		}
	}
	if total > MaxUserEnvTotal {
		return fmt.Errorf("%w: they hold %d bytes; the limit is %d", ErrUserEnvTooLarge, total, MaxUserEnvTotal)
	}
	return nil
}

func userEnvProblem(name, value string, taken map[string]bool) string {
	upper := strings.ToUpper(name)
	switch {
	case len(name) > MaxUserEnvName:
		return fmt.Sprintf("is a name over %d bytes", MaxUserEnvName)
	case !userEnvName.MatchString(name):
		return "is not a variable name (letters, digits and _, not starting with a digit)"
	case len(value) > MaxUserEnvValue:
		return fmt.Sprintf("has a value over %d bytes", MaxUserEnvValue)
	case strings.ContainsRune(value, 0):
		return "has a NUL byte in its value"
	case taken[upper]:
		return "is one spov sets itself"
	case slices.Contains(forgeCredentialEnv, upper), upper == "OPENCODE_API_KEY":
		return "would override the credential spov sets up" + userEnvSecretsHint
	case slices.Contains(refusedUserEnv, upper), slices.Contains(RefusedOpenCodeEnv, upper):
		return "is one spov keeps for itself, since it changes how spov's setup or OpenCode works"
	}
	for _, p := range refusedUserEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return "starts with " + p + ", which spov keeps for itself"
		}
	}
	return ""
}

// WithUserEnv adds a session's own variables to the main container's
// environment, refusing any that env or secrets, everything the launch
// set, already holds.
//
// @decision(DL-096) @decision(DL-097)
func WithUserEnv(env, secrets, user map[string]string) error {
	set := slices.AppendSeq(slices.Collect(maps.Keys(env)), maps.Keys(secrets))
	if err := CheckUserEnv(user, set); err != nil {
		return err
	}
	maps.Copy(env, user)
	return nil
}
