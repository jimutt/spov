package credproxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var (
	anthropic = Provider{Env: "ANTHROPIC_API_KEY", ProviderAPI: ProviderAPI{Host: "api.anthropic.com", Header: HeaderAPIKey}, Key: "sk-ant-api03-real"}
	gemini    = Provider{Env: "GEMINI_API_KEY", ProviderAPI: ProviderAPI{Host: "generativelanguage.googleapis.com", Header: HeaderGoogAPIKey}, Key: "AIzareal"}
	openai    = Provider{Env: "OPENAI_API_KEY", ProviderAPI: ProviderAPI{Host: "api.openai.com", Header: HeaderBearer}, Key: "sk-proj-real"}
)

func newKeyStore(t *testing.T, cfg SidecarConfig) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "credentials.json"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Each provider's key goes in its own header, on its own host, on any
// method and path, and never to another provider's host.
//
// @decision(DL-034)
func TestAProvidersKeyIsSetInItsOwnHeaderOnItsOwnHost(t *testing.T) {
	s := newKeyStore(t, SidecarConfig{Platforms: []Platform{forgejo}, Providers: []Provider{anthropic, gemini, openai}})
	for _, c := range []struct {
		method, url, header, want string
	}{
		{http.MethodPost, "https://api.anthropic.com/v1/messages", "X-Api-Key", "sk-ant-api03-real"},
		{http.MethodDelete, "https://api.anthropic.com/v1/files/f", "X-Api-Key", "sk-ant-api03-real"},
		{http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models/m:streamGenerateContent", "X-Goog-Api-Key", "AIzareal"},
		{http.MethodGet, "https://api.openai.com/v1/models", "Authorization", "Bearer sk-proj-real"},
	} {
		r := originForm(httptest.NewRequest(c.method, c.url, nil))
		if !s.Rewrite(r) || r.Header.Get(c.header) != c.want {
			t.Errorf("%s %s: %s = %q", c.method, c.url, c.header, r.Header.Get(c.header))
		}
		for _, other := range []string{"X-Api-Key", "X-Goog-Api-Key", "Authorization"} {
			if other != c.header && r.Header.Get(other) != "" {
				t.Errorf("%s: another provider's header %s was set", c.url, other)
			}
		}
		if got := s.Secrets(r); len(got) != 1 || got[0] != strings.TrimPrefix(c.want, "Bearer ") {
			t.Errorf("%s: secrets %v", c.url, got)
		}
		if !s.Streamed(r) {
			t.Errorf("%s: a provider's stream would be read", c.url)
		}
	}
	for _, url := range []string{"https://api.anthropic.com.evil.example/v1/messages", "https://api.mistral.ai/v1/chat"} {
		r := originForm(httptest.NewRequest(http.MethodPost, url, nil))
		if s.Rewrite(r) || len(r.Header) != 0 {
			t.Errorf("%s got a key: %v", url, r.Header)
		}
	}
	forge := originForm(httptest.NewRequest(http.MethodGet, "https://codefloe.com/api/v1/repos/o/r", nil))
	if !s.Rewrite(forge) || s.Streamed(forge) {
		t.Error("a forge request lost its credential or would stream unread")
	}
}

// Every header a provider's key can go in is one the proxy removes from
// the client's request first, so a client's own value never survives.
//
// @decision(DL-034)
func TestEveryKeyHeaderIsRemovedFromTheClientsRequest(t *testing.T) {
	for _, h := range []string{HeaderAPIKey, HeaderGoogAPIKey, HeaderBearer} {
		if !slices.Contains(credentialHeaders, http.CanonicalHeaderKey(h)) {
			t.Errorf("%s is not in credentialHeaders", h)
		}
	}
}

// @decision(DL-034)
func TestAProviderIsValidatedBeforeItIsTrusted(t *testing.T) {
	for name, cfg := range map[string]SidecarConfig{
		"no variable":    {Providers: []Provider{{ProviderAPI: anthropic.ProviderAPI, Key: "k"}}},
		"bad host":       {Providers: []Provider{{Env: "K", ProviderAPI: ProviderAPI{Host: "https://x.com", Header: HeaderBearer}, Key: "k"}}},
		"a port":         {Providers: []Provider{{Env: "K", ProviderAPI: ProviderAPI{Host: "x.com:8443", Header: HeaderBearer}, Key: "k"}}},
		"unknown header": {Providers: []Provider{{Env: "K", ProviderAPI: ProviderAPI{Host: "x.com", Header: "cookie"}, Key: "k"}}},
		"no key":         {Providers: []Provider{{Env: "K", ProviderAPI: anthropic.ProviderAPI}}},
		"a newline":      {Providers: []Provider{{Env: "K", ProviderAPI: anthropic.ProviderAPI, Key: "sk-ant-x\n"}}},
		"twice":          {Providers: []Provider{anthropic, anthropic}},
		"a shared host":  {Providers: []Provider{anthropic, {Env: "OTHER", ProviderAPI: anthropic.ProviderAPI, Key: "k"}}},
		"a forge's host": {Platforms: []Platform{forgejo}, Providers: []Provider{{Env: "K", ProviderAPI: ProviderAPI{Host: "codefloe.com", Header: HeaderBearer}, Key: "k"}}},
	} {
		if _, err := NewStore(filepath.Join(t.TempDir(), "c.json"), cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (SidecarConfig{Providers: []Provider{anthropic}}).Validate(); err != nil {
		t.Errorf("a config with keys and no platform: %v", err)
	}
}

// The providers' hosts are decrypted beside the forges'.
//
// @decision(DL-034)
func TestTheDecryptedHostsIncludeTheProviders(t *testing.T) {
	got := SidecarConfig{Platforms: []Platform{forgejo}, Providers: []Provider{anthropic, gemini}}.DecryptHosts()
	want := []string{"codefloe.com", "api.anthropic.com", "generativelanguage.googleapis.com"}
	if !slices.Equal(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
}

// A placeholder keeps a provider key's prefix and nothing else of it.
//
// @decision(DL-034)
func TestAKeyPlaceholderKeepsOnlyThePrefix(t *testing.T) {
	for real, prefix := range map[string]string{
		"sk-ant-api03-secret": "sk-ant-api03-",
		"sk-or-v1-secret":     "sk-or-v1-",
		"sk-proj-secret":      "sk-proj-",
		"sk-secret":           "sk-",
		"gsk_secret":          "gsk_",
		"xai-secret":          "xai-",
		"AIzaSecret":          "AIza",
		"mistralsecret":       "",
	} {
		ph, err := KeyPlaceholder(real)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(ph, prefix+"spovplaceholder") || strings.Contains(ph, "secret") || strings.Contains(ph, "Secret") {
			t.Errorf("%s: %s", real, ph)
		}
	}
}

// A provider's event stream reaches the client as it is sent, unread,
// while a JSON body that echoes the key is scrubbed.
//
// @decision(DL-034)
func TestAProvidersStreamPassesThroughAndItsErrorsAreScrubbed(t *testing.T) {
	// The rig's upstream certificate names *.example.com.
	model := anthropic
	model.Host = "api.example.com"
	ca, _ := testCA(t, model.Host)
	store := newKeyStore(t, SidecarConfig{Providers: []Provider{model}})
	release := make(chan struct{})
	r := newRig(t, Config{
		CA: ca, Decrypt: func(h string) bool { return h == model.Host },
		Rewrite: store.Rewrite, Secrets: store.Secrets, Streamed: store.Streamed,
	}, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/error" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid x-api-key `+req.Header.Get("X-Api-Key")+`"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "event: two\n\n")
	})

	req, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1/messages", strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", "sk-ant-api03-spovplaceholder00")
	req.Header.Set("Authorization", "Bearer from-the-client")
	resp, err := r.client.Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	close(release)
	_ = resp.Body.Close()
	if err != nil || line != "event: one\n" {
		t.Errorf("the first event did not arrive before the stream ended: %q %v", line, err)
	}
	if seen := r.lastSeen(t).Header; seen.Get("X-Api-Key") != "sk-ant-api03-real" || seen.Get("Authorization") != "" {
		t.Errorf("the provider saw x-api-key %q, authorization %q", seen.Get("X-Api-Key"), seen.Get("Authorization"))
	}

	resp, err = r.client.Post("https://api.example.com/v1/error", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), "sk-ant-api03-real") || !strings.Contains(string(body), scrubbed) {
		t.Errorf("an error body echoing the key reached the client as %s", body)
	}
}

// Without Streamed, a credentialed event stream is read and scrubbed like
// any other body.
//
// @decision(DL-034)
func TestAForgesEventStreamIsScrubbed(t *testing.T) {
	ca, _ := testCA(t, "forge.example.com")
	r := newRig(t, Config{CA: ca, Decrypt: forgeOnly, Rewrite: addToken, Secrets: func(*http.Request) []string { return []string{"real-token"} }},
		func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+req.Header.Get("Authorization")+"\n\n")
		})
	resp, err := r.client.Get("https://forge.example.com/events")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), "real-token") {
		t.Errorf("a forge's stream reached the client unread: %s", body)
	}
}

// OpenCode's console token is set only on the paths its provider lists:
// the console config, the model gateway, Zen and web search. Every other
// path on the host is forwarded without a credential, and a path that
// could be normalised past the list matches nothing.
//
// @decision(DL-015)
func TestAProviderWithPathsGetsItsKeyOnlyThere(t *testing.T) {
	opencode := Provider{Env: "OPENCODE_API_KEY", ProviderAPI: ProviderAPI{
		Host: "opencode.ai", Header: HeaderBearer,
		Paths: []string{"/console/api/v2/config", "/inference/", "/zen/", "/console/api/websearch"},
	}, Key: "st_real"}
	s := newKeyStore(t, SidecarConfig{Providers: []Provider{opencode}})
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/console/api/v2/config", true},
		{"/inference/go/openai/v1/chat/completions", true},
		{"/inference/go/anthropic/v1/messages", true},
		{"/zen/go/v1/chat/completions", true},
		{"/console/api/websearch", true},
		{"/inference", true},
		{"/console/api/v2/config/extra", false},
		{"/console/api/user", false},
		{"/console/api/orgs", false},
		{"/console/billing", false},
		{"/inference/../console/api/user", false},
		{"/inference/%2e%2e/console/api/user", false},
		{"/inferencex/v1", false},
		{"/", false},
	} {
		r := originForm(httptest.NewRequest(http.MethodPost, "https://opencode.ai"+c.path, nil))
		got := s.Rewrite(r)
		if got != c.want || (r.Header.Get("Authorization") == "Bearer st_real") != c.want {
			t.Errorf("%s: credentialed %v (Authorization %q), want %v", c.path, got, r.Header.Get("Authorization"), c.want)
		}
	}
}

// A path entry that names a method gets the key on that method only.
//
// @decision(DL-015)
func TestAProviderPathWithAMethodGetsItsKeyOnlyForThatMethod(t *testing.T) {
	opencode := Provider{Env: "OPENCODE_API_KEY", ProviderAPI: ProviderAPI{
		Host: "opencode.ai", Header: HeaderBearer,
		Paths: []string{"GET /console/api/v2/config", "/inference/"},
	}, Key: "st_real"}
	s := newKeyStore(t, SidecarConfig{Providers: []Provider{opencode}})
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/console/api/v2/config", true},
		{http.MethodPost, "/console/api/v2/config", false},
		{http.MethodPut, "/console/api/v2/config", false},
		{http.MethodDelete, "/console/api/v2/config", false},
		{http.MethodHead, "/console/api/v2/config", false},
		{http.MethodPost, "/inference/go/openai/v1/chat/completions", true},
		{http.MethodGet, "/inference/go/openai/v1/models", true},
	} {
		r := originForm(httptest.NewRequest(c.method, "https://opencode.ai"+c.path, nil))
		r.Header.Set("Authorization", "Bearer placeholder")
		got := s.Rewrite(r)
		if got != c.want || (r.Header.Get("Authorization") == "Bearer st_real") != c.want {
			t.Errorf("%s %s: credentialed %v (Authorization %q), want %v", c.method, c.path, got, r.Header.Get("Authorization"), c.want)
		}
	}
}

func TestABadProviderPathIsRefused(t *testing.T) {
	for _, p := range []string{"inference/", "/", "/a/../b", "//x", "get /x", "BREW /x", "GET  /x", "GET x", " /x"} {
		bad := Provider{Env: "OPENCODE_API_KEY", ProviderAPI: ProviderAPI{Host: "opencode.ai", Header: HeaderBearer, Paths: []string{p}}, Key: "k"}
		if err := (SidecarConfig{Providers: []Provider{bad}}).Validate(); err == nil {
			t.Errorf("path %q was accepted", p)
		}
	}
}

// A refreshed key written by set-key reaches the running proxy's next
// request, and a key for a variable the sidecar does not hold is refused.
//
// @decision(DL-015)
func TestASetKeyReachesTheRunningProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	s, err := NewStore(path, SidecarConfig{Providers: []Provider{openai}})
	if err != nil {
		t.Fatal(err)
	}
	if err := SetKey(path, "OPENAI_API_KEY", "sk-proj-newer\n"); err != nil {
		t.Fatal(err)
	}
	r := originForm(httptest.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil))
	if !s.Rewrite(r) || r.Header.Get("Authorization") != "Bearer sk-proj-newer" {
		t.Errorf("after set-key: %q", r.Header.Get("Authorization"))
	}
	if err := SetKey(path, "ANTHROPIC_API_KEY", "k"); err == nil {
		t.Error("a key the sidecar does not hold was accepted")
	}
	if err := SetKey(path, "OPENAI_API_KEY", "  "); err == nil {
		t.Error("an empty key was accepted")
	}
	if got := SetKeyCommand("OPENAI_API_KEY"); !slices.Equal(got, []string{BinaryPath, "set-key", "OPENAI_API_KEY"}) {
		t.Errorf("SetKeyCommand = %v", got)
	}
}
