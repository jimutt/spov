package credproxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// The headers a model provider takes its key in. HeaderBearer is
// Authorization with a Bearer key.
//
// @decision(DL-034)
const (
	HeaderAPIKey     = "x-api-key"
	HeaderGoogAPIKey = "x-goog-api-key"
	HeaderBearer     = "authorization"
)

// ProviderAPI is where a model provider takes its key: its API host and
// the header the key goes in.
//
// @decision(DL-034)
type ProviderAPI struct {
	Host   string `json:"host"`
	Header string `json:"header"`
	// Paths, when set, are the paths the key is set on; every other
	// request to the host is forwarded without a credential. Empty means
	// any path. An entry may start with a method, as in "GET /a/b", and
	// then matches only that method.
	//
	// @decision(DL-015)
	Paths []string `json:"paths,omitempty"`
}

// splitMethod separates an optional leading method from a Paths entry.
func splitMethod(entry string) (method, path string) {
	if m, p, ok := strings.Cut(entry, " "); ok {
		return m, p
	}
	return "", entry
}

// allows reports whether the key is set on r. A path ending in "/" is a
// prefix and matches every path below it; any other matches exactly. A
// path with percent-encoding, a dot segment or an empty segment matches
// nothing, as in the forge policy.
//
// @decision(DL-015)
func (a ProviderAPI) allows(r *http.Request) bool {
	if len(a.Paths) == 0 {
		return true
	}
	segs, ok := cleanSegments(r)
	if !ok {
		return false
	}
	for _, entry := range a.Paths {
		method, p := splitMethod(entry)
		if method != "" && method != r.Method {
			continue
		}
		want := strings.Split(strings.Trim(p, "/"), "/")
		if (strings.HasSuffix(p, "/") && hasPrefix(segs, want)) || slices.Equal(segs, want) {
			return true
		}
	}
	return false
}

// Provider is one model provider's key as the sidecar holds it.
//
// @decision(DL-034)
type Provider struct {
	// Env is the key's variable, which names it in logs.
	Env string `json:"env"`
	ProviderAPI
	// Key is the real key. It never leaves the sidecar.
	Key string `json:"key"`
}

// validate checks one provider's entry.
func (p Provider) validate() error {
	switch {
	case p.Env == "":
		return fmt.Errorf("credproxy: a model provider has no key variable")
	case !validHost(p.Host) || strings.Contains(p.Host, ":"):
		return fmt.Errorf("credproxy: provider %s has a bad host %q", p.Env, p.Host)
	case p.Header != HeaderAPIKey && p.Header != HeaderGoogAPIKey && p.Header != HeaderBearer:
		return fmt.Errorf("credproxy: provider %s has unknown header %q", p.Env, p.Header)
	case p.Key == "":
		return fmt.Errorf("credproxy: provider %s has no key", p.Env)
	case strings.ContainsAny(p.Key, "\r\n\x00"):
		return fmt.Errorf("credproxy: provider %s's key is not a valid header value", p.Env)
	}
	for _, path := range p.Paths {
		if !validPath(path) {
			return fmt.Errorf("credproxy: provider %s has a bad path %q", p.Env, path)
		}
	}
	return nil
}

// validPath accepts "/a/b" or the prefix "/a/b/": at least one segment,
// none empty or a dot segment, and no percent-encoding. A leading method
// must be one of the standard ones, in capitals.
func validPath(entry string) bool {
	method, p := splitMethod(entry)
	if method == "" && p != entry {
		return false
	}
	switch method {
	case "", http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		return false
	}
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "%") {
		return false
	}
	rest := strings.TrimSuffix(p[1:], "/")
	if rest == "" {
		return false
	}
	for _, s := range strings.Split(rest, "/") {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return true
}

// setKey puts the key in the provider's own header. Every header it can
// set is one of credentialHeaders, so a client's value never survives.
func (p Provider) setKey(h http.Header) {
	if p.Header == HeaderBearer {
		h.Set("Authorization", "Bearer "+p.Key)
		return
	}
	h.Set(p.Header, p.Key)
}

// keyPrefixes are the prefixes the catalogue's providers put on their
// keys, longest first. A placeholder keeps the real key's.
var keyPrefixes = []string{"sk-ant-api03-", "sk-ant-", "sk-or-v1-", "sk-proj-", "sk-", "gsk_", "xai-", "AIza"}

// KeyPlaceholder is what the main container holds in place of a model
// provider's key: random, with the same prefix, and worthless anywhere
// but this proxy.
//
// @decision(DL-034)
func KeyPlaceholder(key string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("credproxy: placeholder: %w", err)
	}
	for _, prefix := range keyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return prefix + "spovplaceholder" + hex.EncodeToString(b), nil
		}
	}
	return "spovplaceholder" + hex.EncodeToString(b), nil
}
