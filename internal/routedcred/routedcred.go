// Package routedcred is a routed credential without its secret: what is
// stored beside the sealed secret, listed, and recorded on a session. It
// also holds the local server's index of them, since the keyring that
// holds the secrets cannot enumerate.
//
// @decision(DL-073)
package routedcred

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jimutt/spov/internal/credproxy"
)

// Attach modes: every session by default, or only a launch that names
// the credential.
//
// @decision(DL-075)
const (
	AttachAlways = "always"
	AttachAsked  = "asked"
)

// Limits on how many routed credentials an owner holds.
//
// @decision(DL-073) @decision(DL-074)
const (
	MaxPerUser = 32
	MaxPerOrg  = 64
)

// KeyPrefix starts a routed credential's key wherever credentials of
// several kinds share a key space: user_credentials' platform column and
// this machine's credential store.
//
// @decision(DL-073)
const KeyPrefix = "route:"

// Key is the key a routed credential's secret is stored under.
func Key(name string) string { return KeyPrefix + name }

// IsKey reports whether a stored key is a routed credential's.
func IsKey(key string) bool { return strings.HasPrefix(key, KeyPrefix) }

// Route is a routed credential without its secret.
//
// @decision(DL-072) @decision(DL-073)
type Route struct {
	Name   string   `json:"name"`
	Hosts  []string `json:"hosts"`
	Paths  []string `json:"paths,omitempty"`
	Scheme string   `json:"scheme" enum:"bearer,basic,header"`
	// User is the Basic username.
	User string `json:"user,omitempty"`
	// Header is the header a header-scheme secret is set in.
	Header string `json:"header,omitempty"`
	// Preset names the preset the credential was made with, if any, and
	// URL, Scope and RepoID are its options: the registry's address,
	// npm's scope, and Maven's repository id.
	//
	// @decision(DL-076)
	Preset string `json:"preset,omitempty" enum:"npm,maven,nuget,pypi"`
	URL    string `json:"url,omitempty"`
	Scope  string `json:"scope,omitempty"`
	RepoID string `json:"repo_id,omitempty"`
	// Attach is AttachAlways or AttachAsked.
	Attach string `json:"attach,omitempty" enum:"always,asked"`
}

// Normal is the route with its defaults filled in and its hosts
// lowercased, as it is stored and sealed.
func (r Route) Normal() Route {
	if r.Attach == "" {
		r.Attach = AttachAlways
	}
	r.Name = strings.TrimSpace(r.Name)
	hosts := make([]string, len(r.Hosts))
	for i, h := range r.Hosts {
		hosts[i] = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	}
	r.Hosts = hosts
	return r
}

// Validate checks the route as the proxy will, with any secret.
func (r Route) Validate() error {
	if r.Attach != AttachAlways && r.Attach != AttachAsked {
		return fmt.Errorf("routed credential %s: attach is %q, not always or asked", r.Name, r.Attach)
	}
	// A preset's options are checked by remaking it, so a stored route
	// cannot carry a URL its hosts do not cover.
	//
	// @decision(DL-076)
	if r.Preset != "" {
		again, err := FromPreset(r.Name, r.Preset, PresetOptions{URL: r.URL, Scope: r.Scope, RepoID: r.RepoID,
			User: r.User, Paths: r.Paths, Attach: r.Attach})
		if err != nil {
			return fmt.Errorf("routed credential %s: %w", r.Name, err)
		}
		if !slices.Equal(again.Hosts, r.Hosts) || again.Scheme != r.Scheme {
			return fmt.Errorf("routed credential %s: its hosts or scheme are not its %s preset's", r.Name, r.Preset)
		}
	} else if r.URL != "" || r.Scope != "" || r.RepoID != "" {
		return fmt.Errorf("routed credential %s: url, scope and repo_id are a preset's", r.Name)
	}
	return r.validateBase()
}

// validateBase checks what the proxy checks, with any secret.
func (r Route) validateBase() error {
	if err := r.WithSecret("x").Validate(); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "credproxy: "))
	}
	return nil
}

// WithSecret is the route as the sidecar holds it.
func (r Route) WithSecret(secret string) credproxy.Route {
	return credproxy.Route{Name: r.Name, Hosts: r.Hosts, Paths: r.Paths, Scheme: r.Scheme,
		User: r.User, Header: r.Header, Secret: secret}
}

// Encode is the route's canonical encoding: the bytes stored beside the
// secret and bound into its seal.
func (r Route) Encode() []byte {
	b, _ := json.Marshal(r.Normal())
	return b
}

// Decode reads a stored route back, normalised.
func Decode(b []byte) (Route, error) {
	var r Route
	if err := json.Unmarshal(b, &r); err != nil {
		return Route{}, fmt.Errorf("routed credential: %w", err)
	}
	return r.Normal(), nil
}

// AAD binds a sealed secret to its route as well as to its row: base is
// the row's own AAD, and the route's canonical encoding follows it, so a
// route changed beside the ciphertext no longer opens it.
//
// @decision(DL-073)
func AAD(base []byte, r Route) []byte {
	out := append([]byte{}, base...)
	out = append(out, 0)
	return append(out, r.Encode()...)
}
