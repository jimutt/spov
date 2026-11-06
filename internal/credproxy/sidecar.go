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

// JavaTrustStore is the PKCS#12 trust store every JVM in the main
// container reads, built from SystemBundle by the entrypoint, and
// JavaTrustPassword its password, which guards nothing: the store holds
// only public certificates.
//
// @decision(DL-076)
const (
	JavaTrustStore    = "/etc/spov/java-truststore.p12"
	JavaTrustPassword = "spov-trust"
)

// JavaTrustCommand builds JavaTrustStore from SystemBundle, marking each
// certificate trusted as the JDK needs (openssl 3.2's -jdktrust, which
// the base image's Debian release has).
//
// @decision(DL-076)
func JavaTrustCommand() string {
	return "mkdir -p '" + path.Dir(JavaTrustStore) + "' && openssl pkcs12 -export -nokeys -in '" + SystemBundle +
		"' -jdktrust anyExtendedKeyUsage -passout pass:" + JavaTrustPassword + " -out '" + JavaTrustStore + ".tmp' && mv '" +
		JavaTrustStore + ".tmp' '" + JavaTrustStore + "'"
}

// JavaToolOptions points every JVM at JavaTrustStore and the proxy,
// which Java does not find through HTTPS_PROXY.
//
// @decision(DL-076)
func JavaToolOptions() string {
	return strings.Join([]string{
		"-Djavax.net.ssl.trustStore=" + JavaTrustStore, "-Djavax.net.ssl.trustStoreType=PKCS12",
		"-Djavax.net.ssl.trustStorePassword=" + JavaTrustPassword,
		"-Dhttps.proxyHost=" + SidecarName, "-Dhttps.proxyPort=" + strconv.Itoa(Port),
		"-Dhttp.proxyHost=" + SidecarName, "-Dhttp.proxyPort=" + strconv.Itoa(Port),
		"-Dhttp.nonProxyHosts=localhost|127.0.0.1|main",
	}, " ")
}

// InstallCACommand is the exec that makes the main container trust a new
// sidecar's CA, with the certificate on stdin: it replaces MainCAFile and
// adds the certificate to SystemBundle. A process already running keeps
// the trust it started with.
//
// @decision(DL-034)
func InstallCACommand() []string {
	return []string{"sh", "-c", "mkdir -p '" + path.Dir(MainCAFile) + "' && cat > '" + MainCAFile +
		"' && cat '" + MainCAFile + "' >> '" + SystemBundle + "'" +
		// The Java trust store follows the bundle, for JVMs started later.
		// @decision(DL-076)
		" && { ! command -v openssl >/dev/null || " + JavaTrustCommand() + "; }"}
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
