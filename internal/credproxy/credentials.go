package credproxy

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Forge kinds the proxy knows how to authenticate. They are git's
// kinds, which this package cannot import;
// TestTheProxyHasAPolicyForEveryKind in session keeps the two lists
// together.
//
// @decision(DL-041)
const (
	KindGitHub  = "github"
	KindForgejo = "forgejo"
	KindGitea   = "gitea"
	KindGitLab  = "gitlab"
)

// Kinds is every kind the proxy has a policy for.
//
// @decision(DL-041)
var Kinds = []string{KindGitHub, KindForgejo, KindGitea, KindGitLab}

// knownKind reports whether the proxy has a policy for a kind. Every
// switch on a kind in this package answers for exactly these.
//
// @decision(DL-041)
func knownKind(kind string) bool { return slices.Contains(Kinds, kind) }

// Platform is one forge's credential as the sidecar holds it.
//
// @decision(DL-034)
type Platform struct {
	// Name is the platform's configured name, which a re-mint names.
	Name string `json:"name"`
	// Kind is one of the kinds knownKind accepts.
	Kind string `json:"kind"`
	// Host is the platform's host as its clone URLs name it, with a port
	// when the forge is not on 443.
	Host string `json:"host"`
	// User is the account the token authenticates as over git.
	User string `json:"user"`
	// Token is the real credential. It never leaves the sidecar.
	Token string `json:"token"`
	// Scoped means the forge itself narrows the token to the session's
	// repositories (a GitHub App), so it is added on any path of the
	// platform's hosts. Otherwise the endpoint policy applies.
	//
	// @decision(DL-034)
	Scoped bool `json:"scoped,omitempty"`
	// Repos are the session's repositories on this platform, as
	// "owner/name".
	Repos []string `json:"repos,omitempty"`
	// AllRepos adds the token for any repository on the platform's
	// hosts; the endpoint policy still applies.
	//
	// @decision(DL-043)
	AllRepos bool `json:"allRepos,omitempty"`
	// BasePath is a Forgejo's, Gitea's or GitLab's path below its host,
	// such as "/forgejo", empty at the root.
	BasePath string `json:"basePath,omitempty"`
	// ProjectIDs are the numeric IDs of the session's GitLab projects,
	// which GitLab's API also names a project by.
	//
	// @decision(DL-034)
	ProjectIDs []string `json:"projectIds,omitempty"`
}

// Hosts are the targets a platform's credential is used on, as HostKey
// names them: GitHub's API and upload hosts beside github.com, and the
// platform's own host otherwise, which is where GitHub Enterprise and
// Forgejo serve both git and the API.
func (p Platform) Hosts() []string {
	host := RequestKey(p.Host)
	if p.Kind == KindGitHub && host == "github.com" {
		return []string{"github.com", "api.github.com", "uploads.github.com"}
	}
	return []string{host}
}

// authorization is the header value for a request to host and path.
// git over HTTP authenticates with Basic auth. GitHub's API takes the
// token as a bearer: on github.com that is its API and upload hosts, and
// on GitHub Enterprise everything under /api/. Forgejo and Gitea take
// Basic auth for both git and their API. A kind without a policy gets
// nothing.
//
// @decision(DL-041)
func (p Platform) authorization(host, path string) string {
	switch p.Kind {
	case KindGitHub:
		if host == "api.github.com" || host == "uploads.github.com" || strings.HasPrefix(path, "/api/") {
			return "Bearer " + p.Token
		}
		return basic(p.User, p.Token)
	case KindForgejo, KindGitea:
		return basic(p.User, p.Token)
	// GitLab's API takes a bearer token, never Basic; git takes Basic.
	//
	// @decision(DL-034)
	case KindGitLab:
		if base, ok := cleanBase(p.BasePath); ok && strings.HasPrefix(path, "/"+strings.Join(append(base, "api"), "/")+"/") {
			return "Bearer " + p.Token
		}
		return basic(p.User, p.Token)
	}
	return ""
}

func basic(user, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
}

// SidecarConfig is everything the proxy is started with. It reaches the
// sidecar as a secret and is written to a file only the sidecar can read.
//
// @decision(DL-034)
type SidecarConfig struct {
	Platforms []Platform `json:"platforms"`
	// Providers are the model provider keys, each used only on its own
	// provider's API host.
	//
	// @decision(DL-034)
	Providers []Provider `json:"providers,omitempty"`
}

// Validate checks a config before the proxy trusts it.
func (c SidecarConfig) Validate() error {
	seen := map[string]bool{}
	for i, p := range c.Platforms {
		switch {
		case p.Name == "":
			return fmt.Errorf("credproxy: platform %d has no name", i)
		case seen[p.Name]:
			return fmt.Errorf("credproxy: platform %q is listed twice", p.Name)
		case !knownKind(p.Kind):
			return fmt.Errorf("credproxy: platform %q has unknown kind %q", p.Name, p.Kind)
		case !validHost(p.Host):
			return fmt.Errorf("credproxy: platform %q has a bad host %q", p.Name, p.Host)
		case p.Token == "":
			return fmt.Errorf("credproxy: platform %q has no token", p.Name)
		case !p.Scoped && !p.AllRepos && len(p.Repos) == 0:
			return fmt.Errorf("credproxy: platform %q has no repositories to use its token on", p.Name)
		}
		seen[p.Name] = true
	}
	// One credential per host: the proxy chooses a credential by host,
	// so two platforms on one would make the first listed win.
	//
	// @decision(DL-034)
	hosts := map[string]bool{}
	owner := map[string]string{}
	for _, p := range c.Platforms {
		for _, h := range p.Hosts() {
			if other, taken := owner[h]; taken && other != p.Name {
				return fmt.Errorf("credproxy: platforms %q and %q both have a credential for %s", other, p.Name, h)
			}
			owner[h] = p.Name
			hosts[h] = true
		}
	}
	envs := map[string]bool{}
	for _, p := range c.Providers {
		if err := p.validate(); err != nil {
			return err
		}
		host := RequestKey(p.Host)
		switch {
		case envs[p.Env]:
			return fmt.Errorf("credproxy: provider %s is listed twice", p.Env)
		case hosts[host]:
			return fmt.Errorf("credproxy: provider %s's host %s already has a credential", p.Env, host)
		}
		envs[p.Env], hosts[host] = true, true
	}
	return nil
}

// validHost accepts a host name with an optional numeric port.
func validHost(h string) bool {
	host, port, err := net.SplitHostPort(h)
	if err != nil {
		host, port = h, ""
	}
	if host == "" || strings.ContainsAny(host, "/:@ \t\n") {
		return false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// CAHosts are the names the sandbox CA must be able to vouch for: every
// decrypted target without its port.
func (c SidecarConfig) CAHosts() []string {
	var out []string
	for _, t := range c.DecryptHosts() {
		if h := bareHost(t); !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// DecryptHosts are every target some platform's credential or some
// provider's key is used on.
//
// @decision(DL-034)
func (c SidecarConfig) DecryptHosts() []string {
	var out []string
	for _, p := range c.Platforms {
		for _, h := range p.Hosts() {
			if !slices.Contains(out, h) {
				out = append(out, h)
			}
		}
	}
	for _, p := range c.Providers {
		if h := RequestKey(p.Host); !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// Store holds the sidecar's credentials in a file only it can read, and
// rereads the file when a re-mint has replaced it.
//
// @decision(DL-034) @decision(DL-021)
type Store struct {
	path string

	mu   sync.Mutex
	cfg  SidecarConfig
	file os.FileInfo
}

// NewStore returns a Store reading from path. A valid file already there
// wins over cfg: a proxy restarted in the same sidecar keeps the tokens
// re-mints wrote, rather than going back to the launch's. Otherwise cfg
// is written, readable by its owner only.
func NewStore(path string, cfg SidecarConfig) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if existing, err := readConfig(path); err != nil || existing.Validate() != nil {
		if err := writeConfig(path, cfg); err != nil {
			return nil, err
		}
	}
	s := &Store{path: path}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Platform returns the credential used on a target, reread from the file
// if it has been replaced. Every write renames a new file into place, so
// a different file means new contents, whatever its size or timestamp.
// Two platforms on one host is a configuration spov does not produce; the
// first listed would win, as it would in git's credential store.
func (s *Store) Platform(host string) (Platform, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, err := os.Stat(s.path); err == nil && !os.SameFile(info, s.file) {
		_ = s.reloadLocked()
	}
	for _, p := range s.cfg.Platforms {
		if slices.Contains(p.Hosts(), host) {
			return p, true
		}
	}
	return Platform{}, false
}

// Provider returns the model provider whose key is used on a target,
// reread from the file if a key has been replaced since.
//
// @decision(DL-034) @decision(DL-015)
func (s *Store) Provider(host string) (Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, err := os.Stat(s.path); err == nil && !os.SameFile(info, s.file) {
		_ = s.reloadLocked()
	}
	for _, p := range s.cfg.Providers {
		if RequestKey(p.Host) == host {
			return p, true
		}
	}
	return Provider{}, false
}

// Rewrite is the proxy's policy: a request to a platform's host gets
// that platform's credential when the endpoint policy allows it, a
// request to a model provider's API host gets its key on the provider's
// paths (any path when it lists none), and anything else is forwarded as
// an anonymous client's would be.
//
// @decision(DL-034) @decision(DL-015)
func (s *Store) Rewrite(r *http.Request) bool {
	target := RequestKey(r.Host)
	if m, ok := s.Provider(target); ok {
		if !m.allows(r) {
			return false
		}
		m.setKey(r.Header)
		return true
	}
	p, ok := s.Platform(target)
	if !ok || !p.allows(bareHost(target), r) {
		return false
	}
	auth := p.authorization(bareHost(target), r.URL.Path)
	if auth == "" {
		return false
	}
	r.Header.Set("Authorization", auth)
	return true
}

// Secrets are the forms of a request's credential an upstream might echo:
// the token and its Basic encoding, or a provider's key.
//
// @decision(DL-034)
func (s *Store) Secrets(r *http.Request) []string {
	target := RequestKey(r.Host)
	if m, ok := s.Provider(target); ok {
		return []string{m.Key}
	}
	p, ok := s.Platform(target)
	if !ok {
		return nil
	}
	return []string{p.Token, strings.TrimPrefix(basic(p.User, p.Token), "Basic ")}
}

// Streamed reports whether a response to r that is an event stream is
// passed through unread: one from a model provider, whose output streams.
//
// @decision(DL-034)
func (s *Store) Streamed(r *http.Request) bool {
	_, ok := s.Provider(RequestKey(r.Host))
	return ok
}

func (s *Store) reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked()
}

func (s *Store) reloadLocked() error {
	info, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("credproxy: %w", err)
	}
	cfg, err := readConfig(s.path)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.cfg, s.file = cfg, info
	return nil
}

// SetToken replaces one platform's token in the file at path, which a
// running proxy then rereads. It is what a re-mint runs in the sidecar,
// with the token on stdin. It holds the file's lock, which SetKey also
// takes, so concurrent updates take turns.
//
// @decision(DL-021) @decision(DL-034)
func SetToken(path, platform, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("credproxy: no token on stdin")
	}
	unlock, err := lockFile(path)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := readConfig(path)
	if err != nil {
		return err
	}
	found := false
	for i := range cfg.Platforms {
		if cfg.Platforms[i].Name == platform {
			cfg.Platforms[i].Token = token
			found = true
		}
	}
	if !found {
		return fmt.Errorf("credproxy: no platform %q", platform)
	}
	return writeConfig(path, cfg)
}

// SetKey replaces one model provider's key in the file at path, which a
// running proxy then rereads. It is what a refreshed OpenCode Console
// grant runs in the sidecar, with the key on stdin.
//
// @decision(DL-015)
func SetKey(path, env, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("credproxy: no key on stdin")
	}
	unlock, err := lockFile(path)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := readConfig(path)
	if err != nil {
		return err
	}
	found := false
	for i := range cfg.Providers {
		if cfg.Providers[i].Env == env {
			cfg.Providers[i].Key = key
			found = true
		}
	}
	if !found {
		return fmt.Errorf("credproxy: no provider key %s", env)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	return writeConfig(path, cfg)
}

func readConfig(path string) (SidecarConfig, error) {
	var cfg SidecarConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("credproxy: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("credproxy: read %s: %w", path, err)
	}
	return cfg, nil
}

// writeConfig replaces the file in one rename, so a reader sees the old
// config or the new one and never half of either.
func writeConfig(path string, cfg SidecarConfig) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return fmt.Errorf("credproxy: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credproxy: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credproxy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credproxy: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("credproxy: %w", err)
	}
	return nil
}

// tokenPrefixes are the prefixes GitHub and GitLab put on their tokens.
// A tool may branch on them, so a placeholder keeps the real token's.
//
// @decision(DL-034)
var tokenPrefixes = []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "glpat-"}

// Placeholder is what the main container holds in place of token: random,
// with the same prefix, and worthless anywhere but this proxy.
//
// @decision(DL-034)
func Placeholder(token string) (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("credproxy: placeholder: %w", err)
	}
	for _, prefix := range tokenPrefixes {
		if strings.HasPrefix(token, prefix) {
			return prefix + "spovplaceholder" + hex.EncodeToString(b), nil
		}
	}
	return hex.EncodeToString(b), nil
}
