// Package credentials stores the secrets spov needs: Modal tokens, git
// platform tokens and per-sandbox OpenCode passwords.
//
// @decision(DL-008)
package credentials

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrNotFound means no secret is stored under that key.
var ErrNotFound = errors.New("credentials: not found")

// Store holds secrets. Implementations must never log or wrap a secret
// value into an error.
//
// @decision(DL-008)
type Store interface {
	Get(key string) (string, error)
	Set(key, secret string) error
	Delete(key string) error
	// Name identifies the backend for `spov config show`, so a user can
	// tell where their secrets actually live.
	Name() string
}

// Key namespaces. A key is "<kind>:<id>", which keeps the flat key space
// of the OS keyring usable and makes stored entries self-describing.
const (
	// KindPlatform holds a git platform token, keyed by platform name.
	KindPlatform = "platform"
	// KindSandbox holds a per-sandbox OpenCode password, keyed by
	// sandbox ID.
	KindSandbox = "sandbox"
	// KindProvider holds a model provider API key, keyed by env var name.
	KindProvider = "provider"
	// KindServer holds a spov CLI token for a hosted server, keyed by
	// the server's host. One token per host, so two servers do not
	// share a credential.
	//
	// @decision(DL-006)
	KindServer = "server"
)

// Key builds a namespaced credential key.
//
// @decision(DL-008)
func Key(kind, id string) (string, error) {
	if kind == "" || id == "" {
		return "", fmt.Errorf("credentials: key needs both a kind and an id")
	}
	if strings.ContainsAny(kind, ":") || strings.ContainsAny(id, ":") {
		return "", fmt.Errorf("credentials: kind and id must not contain ':' (got %q, %q)", kind, id)
	}
	return kind + ":" + id, nil
}

// PlatformKey is the key holding a git platform's token.
func PlatformKey(platform string) (string, error) { return Key(KindPlatform, platform) }

// SandboxKey is the key holding a sandbox's OpenCode password.
func SandboxKey(sandboxID string) (string, error) { return Key(KindSandbox, sandboxID) }

// ProviderKey is the key holding a model provider API key.
func ProviderKey(envVar string) (string, error) { return Key(KindProvider, envVar) }

// ServerKey is the key holding the CLI token for a hosted server. It is
// derived from the URL's host so that the same server is one entry
// however its URL was written — a trailing slash, a path, or a
// different case in the hostname all resolve to the same key.
//
// The host's ":" is replaced, since a key's id may not contain one.
//
// @decision(DL-006)
func ServerKey(serverURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		return "", fmt.Errorf("credentials: server url %q: %w", serverURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("credentials: server url %q needs an http or https scheme", serverURL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("credentials: server url %q has no host", serverURL)
	}
	// A default port is dropped so https://x and https://x:443 are one
	// entry, and the port's colon becomes "_" for the flat key space.
	host := strings.ToLower(u.Host)
	if h, port, err := net.SplitHostPort(host); err == nil {
		if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
			host = h
		} else {
			host = h + "_" + port
		}
	}
	// The scheme is part of the identity: http://x and https://x are
	// different origins, and a token minted for one must never be sent
	// to the other. Only http is marked, so an https entry — the normal
	// case — keeps the plain host as its key.
	if u.Scheme == "http" {
		host = "http_" + host
	}
	return Key(KindServer, host)
}
