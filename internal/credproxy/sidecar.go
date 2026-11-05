package credproxy

import (
	"path"
	"strconv"
	"strings"
)

// The sidecar's layout, shared by the server that starts it and the
// binary that runs in it.
//
// @decision(DL-034)
const (
	// SidecarName is the sidecar's name, which is also its host name
	// from the main container.
	SidecarName = "proxy"
	// Port is where the proxy listens in the sidecar.
	Port = 3128
	// BinaryPath is where the proxy image carries the binary.
	BinaryPath = "/usr/local/bin/spov-proxy"
	// CredentialsFile holds the platforms' credentials inside the
	// sidecar, readable by its owner only.
	CredentialsFile = "/run/spov-proxy/credentials.json"

	// EnvCACert, EnvCAKey and EnvConfig are what the sidecar is started
	// with. The key and the config are secrets.
	EnvCACert = "SPOV_PROXY_CA_CERT"
	EnvCAKey  = "SPOV_PROXY_CA_KEY"
	EnvConfig = "SPOV_PROXY_CONFIG"

	// EnvMainCA carries the CA certificate to the main container's
	// entrypoint, which installs it at MainCAFile and adds it to
	// SystemBundle. MainCAFile is where update-ca-certificates looks, so
	// a package install that rebuilds the bundle keeps the CA in it.
	EnvMainCA    = "SPOV_PROXY_CA"
	MainCAFile   = "/usr/local/share/ca-certificates/spov-proxy.crt"
	SystemBundle = "/etc/ssl/certs/ca-certificates.crt"
)

// URL is the proxy's address from the main container.
func URL() string {
	return "http://" + SidecarName + ":" + strconv.Itoa(Port)
}

// MainEnv is what points the main container's clients at the proxy and
// makes them trust its CA. Both spellings of the proxy variables are
// set, since tools disagree on which they read. Clients that read none
// of them go out directly, without a credential.
//
// @decision(DL-034)
func MainEnv() map[string]string {
	noProxy := strings.Join([]string{"localhost", "127.0.0.1", "::1", "main"}, ",")
	env := map[string]string{
		// OpenCode's own runtime reads this; the rest read the bundle.
		"NODE_EXTRA_CA_CERTS": MainCAFile,
		"SSL_CERT_FILE":       SystemBundle,
		"REQUESTS_CA_BUNDLE":  SystemBundle,
	}
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY"} {
		env[k] = URL()
		env[strings.ToLower(k)] = URL()
	}
	env["NO_PROXY"] = noProxy
	env["no_proxy"] = noProxy
	return env
}

// InstallCACommand is the exec that makes the main container trust a new
// sidecar's CA, with the certificate on stdin: it replaces MainCAFile and
// adds the certificate to SystemBundle. A process already running keeps
// the trust it started with.
//
// @decision(DL-034)
func InstallCACommand() []string {
	return []string{"sh", "-c", "mkdir -p '" + path.Dir(MainCAFile) + "' && cat > '" + MainCAFile +
		"' && cat '" + MainCAFile + "' >> '" + SystemBundle + "'"}
}

// SetTokenCommand is the exec a re-mint runs in the sidecar, with the
// token on stdin.
//
// @decision(DL-021)
func SetTokenCommand(platform string) []string {
	return []string{BinaryPath, "set-token", platform}
}

// SetKeyCommand is the exec that replaces one model provider's key in
// the sidecar, with the key on stdin.
//
// @decision(DL-015)
func SetKeyCommand(env string) []string {
	return []string{BinaryPath, "set-key", env}
}
