package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
)

// deviceServer is a spov server good enough to drive `spov login`: the
// two device endpoints and /api/v1/me, with the approval decided by the
// test rather than a human.
type deviceServer struct {
	*httptest.Server
	mu sync.Mutex
	// approved decides what the next poll returns.
	approved bool
	denied   bool
	// issued is the token handed out, so a test can check what was
	// stored.
	issued string
	// revoked records a DELETE of the current token.
	revoked bool
	// name is what the CLI proposed for the token.
	name string
	// polls counts token requests.
	polls int
	// refuse, when set, is the description a 429 to the next code
	// request carries.
	refuse string
}

func newDeviceServer(t *testing.T) *deviceServer {
	t.Helper()
	s := &deviceServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.name = r.Form.Get("name")
		refuse := s.refuse
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if refuse != "" {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "slow_down", "error_description": refuse})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "the-device-code", "user_code": "BCDF-GHJK",
			"verification_uri":          s.URL + "/device",
			"verification_uri_complete": s.URL + "/device?code=BCDF-GHJK",
			"expires_in":                600,
			// One second is the floor: x/oauth2's ticker waits one
			// full tick before the first request, so any test driving
			// runServerLogin through the real client pays it. Kept
			// deliberately, since these tests exist to prove spov's
			// login works with the client it actually ships with.
			//
			// @decision(DL-006)
			"interval": 1,
		})
	})
	mux.HandleFunc("POST /device/token", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.polls++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case s.denied:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
		case s.approved:
			s.issued = "spov_pat_" + strings.Repeat("a", 43) + "000000"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": s.issued, "token_type": "bearer",
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		}
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		want := "Bearer " + s.issued
		s.mu.Unlock()
		if s.issued == "" || r.Header.Get("Authorization") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_id": "user-1", "org_id": "default", "email": "alice@example.com",
		})
	})
	mux.HandleFunc("DELETE /api/v1/me/tokens/current", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.revoked = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *deviceServer) approve() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approved = true
}

func (s *deviceServer) deny() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denied = true
}

// loginEnv is an Env pointed at a device server with a memory
// credential store.
func loginEnv(t *testing.T, srv *deviceServer) (*Env, credentials.Store) {
	t.Helper()
	creds := credentials.NewMemoryStore()
	env := &Env{
		Out: &strings.Builder{}, Err: &strings.Builder{},
		Environ:     config.MapEnv{Vars: map[string]string{ServerEnvVar: srv.URL}},
		Credentials: creds,
		Workdir:     t.TempDir(),
	}
	return env, creds
}

// TestServerLoginStoresTheToken is the whole CLI half: the flow runs,
// and the token lands under this server's key.
//
// @decision(DL-006)
func TestServerLoginStoresTheToken(t *testing.T) {
	srv := newDeviceServer(t)
	env, creds := loginEnv(t, srv)
	srv.approve()

	if err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true}); err != nil {
		t.Fatal(err)
	}

	key, err := credentials.ServerKey(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := creds.Get(key)
	if err != nil {
		t.Fatalf("no token stored: %v", err)
	}
	srv.mu.Lock()
	want := srv.issued
	srv.mu.Unlock()
	if stored != want {
		t.Errorf("stored %q, want the issued token", stored)
	}

	// The code and URL are printed for a human to act on.
	printed := env.Err.(*strings.Builder).String()
	if !strings.Contains(printed, "BCDF-GHJK") {
		t.Errorf("the user code was not printed: %s", printed)
	}
	if !strings.Contains(printed, srv.URL+"/device") {
		t.Errorf("the verification URI was not printed: %s", printed)
	}
	// And the identity is confirmed against the server.
	out := env.Out.(*strings.Builder).String()
	if !strings.Contains(out, "alice@example.com") {
		t.Errorf("the confirmation does not name the user: %s", out)
	}
}

// TestServerLoginNeverPrintsTheToken: it goes to the credential store,
// and a terminal is a place secrets get pasted into issues.
//
// @decision(DL-006)
func TestServerLoginNeverPrintsTheToken(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)
	srv.approve()
	if err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true}); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	token := srv.issued
	srv.mu.Unlock()

	for _, stream := range []string{env.Out.(*strings.Builder).String(), env.Err.(*strings.Builder).String()} {
		if strings.Contains(stream, token) {
			t.Errorf("the token was printed: %s", stream)
		}
	}
}

// TestServerLoginSendsATokenName, which is what the approval page shows
// so a person can tell which terminal is asking.
//
// @decision(DL-006)
func TestServerLoginSendsATokenName(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)
	srv.approve()
	if err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true, Name: "my laptop"}); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	got := srv.name
	srv.mu.Unlock()
	if got != "my laptop" {
		t.Errorf("the server saw name %q", got)
	}
}

// TestServerLoginReportsADenial in words, not as an RFC error code.
//
// @decision(DL-006)
func TestServerLoginReportsADenial(t *testing.T) {
	srv := newDeviceServer(t)
	env, creds := loginEnv(t, srv)
	srv.deny()

	err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true})
	if err == nil {
		t.Fatal("a denied login succeeded")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("err = %v, want it to say the login was denied", err)
	}
	if strings.Contains(err.Error(), "access_denied") {
		t.Errorf("the RFC code reached the user: %v", err)
	}
	// Nothing was stored.
	key, _ := credentials.ServerKey(srv.URL)
	if _, err := creds.Get(key); !errors.Is(err, credentials.ErrNotFound) {
		t.Error("a denied login stored a token")
	}
}

// A server that refuses to start a sign-in, because too many were
// started from this address, is quoted rather than dumped as an
// oauth2 error.
//
// @decision(DL-133)
func TestServerLoginPrintsTheServersRefusalToStart(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)
	const refusal = "Too many sign-ins were started from your address. Try again in 2 seconds."
	srv.mu.Lock()
	srv.refuse = refusal
	srv.mu.Unlock()

	err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true})
	if err == nil || err.Error() != "starting the login: "+refusal {
		t.Fatalf("err = %v, want the server's own words", err)
	}
}

// TestServerLoginNeedsAServer: local mode has no login, and saying so is
// more useful than an empty-URL error.
//
// @decision(DL-006)
func TestServerLoginNeedsAServer(t *testing.T) {
	env := &Env{
		Out: &strings.Builder{}, Err: &strings.Builder{},
		Environ: config.MapEnv{Vars: map[string]string{}},
		Workdir: t.TempDir(), Credentials: credentials.NewMemoryStore(),
	}
	err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true})
	if err == nil {
		t.Fatal("a login with no server succeeded")
	}
	if !strings.Contains(err.Error(), "no server configured") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "local server needs no login") {
		t.Errorf("err = %v, want it to explain local mode", err)
	}
}

// TestServerLogoutRevokesThenForgets.
//
// @decision(DL-006)
func TestServerLogoutRevokesThenForgets(t *testing.T) {
	srv := newDeviceServer(t)
	env, creds := loginEnv(t, srv)
	srv.approve()
	if err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true}); err != nil {
		t.Fatal(err)
	}

	if err := runServerLogout(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	revoked := srv.revoked
	srv.mu.Unlock()
	if !revoked {
		t.Error("logout did not revoke the token server-side")
	}
	key, _ := credentials.ServerKey(srv.URL)
	if _, err := creds.Get(key); !errors.Is(err, credentials.ErrNotFound) {
		t.Error("logout left the token on disk")
	}
}

// TestServerLogoutForgetsEvenWhenRevokeFails: leaving the token on disk
// after someone asked to be signed out is the worse failure.
//
// @decision(DL-006)
func TestServerLogoutForgetsEvenWhenRevokeFails(t *testing.T) {
	srv := newDeviceServer(t)
	env, creds := loginEnv(t, srv)
	srv.approve()
	if err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true}); err != nil {
		t.Fatal(err)
	}
	// The server is gone by the time logout runs.
	srv.Close()

	if err := runServerLogout(context.Background(), env); err != nil {
		t.Fatalf("logout failed rather than warning: %v", err)
	}
	key, _ := credentials.ServerKey(srv.URL)
	if _, err := creds.Get(key); !errors.Is(err, credentials.ErrNotFound) {
		t.Error("the token survived a logout whose revoke failed")
	}
	warned := env.Err.(*strings.Builder).String()
	if !strings.Contains(warned, "did not revoke") {
		t.Errorf("no warning about the failed revoke: %s", warned)
	}
}

// TestServerLogoutWithoutATokenSaysSo rather than failing.
//
// @decision(DL-006)
func TestServerLogoutWithoutATokenSaysSo(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)
	if err := runServerLogout(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if out := env.Out.(*strings.Builder).String(); !strings.Contains(out, "Not signed in") {
		t.Errorf("out = %q", out)
	}
}

// TestStoredServerTokenIsUsedForRequests: after a login, every command
// authenticates without SPOV_SERVER_TOKEN.
//
// @decision(DL-006)
func TestStoredServerTokenIsUsedForRequests(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)
	srv.approve()
	if err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true}); err != nil {
		t.Fatal(err)
	}
	// A fresh client, as a later command would build.
	env.Client = nil
	c, err := env.client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	me, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("the stored token did not authenticate: %v", err)
	}
	if me.Email != "alice@example.com" {
		t.Errorf("me = %+v", me)
	}
}

// TestTheEnvironmentTokenWinsOverTheStoredOne, so a CI job can pass one
// without touching the credential store.
//
// @decision(DL-006)
func TestTheEnvironmentTokenWinsOverTheStoredOne(t *testing.T) {
	srv := newDeviceServer(t)
	env, creds := loginEnv(t, srv)
	key, _ := credentials.ServerKey(srv.URL)
	if err := creds.Set(key, "spov_pat_stored"); err != nil {
		t.Fatal(err)
	}
	env.Environ = config.MapEnv{Vars: map[string]string{
		ServerEnvVar: srv.URL, ServerTokenEnvVar: "from-the-environment",
	}}
	if got := env.storedServerToken(srv.URL); got != "spov_pat_stored" {
		t.Errorf("storedServerToken = %q", got)
	}
	// The client prefers the environment.
	c, err := env.client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Me(context.Background()); err == nil {
		t.Error("the environment token was accepted by a server that never issued it")
	}
}

// TestExplainAuthPointsAt401s.
//
// @decision(DL-006)
func TestExplainAuthPointsAt401s(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)

	// A 401 from the server, unwrapped by the client.
	c, err := env.client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, meErr := c.Me(context.Background())
	if meErr == nil {
		t.Fatal("expected a 401")
	}
	explained := env.ExplainAuth(meErr)
	if !strings.Contains(explained.Error(), "spov login") {
		t.Errorf("err = %v, want it to name the fix", explained)
	}
	if !strings.Contains(explained.Error(), srv.URL) {
		t.Errorf("err = %v, want it to name the server", explained)
	}

	// With a token from the environment the advice is different: the
	// token is wrong, not missing.
	env.Environ = config.MapEnv{Vars: map[string]string{
		ServerEnvVar: srv.URL, ServerTokenEnvVar: "nope",
	}}
	if got := env.ExplainAuth(meErr).Error(); !strings.Contains(got, ServerTokenEnvVar) {
		t.Errorf("err = %v, want it to name the variable", got)
	}

	// Anything that is not a 401 passes through untouched — the same
	// error value, not merely an equal one.
	other := errors.New("something else")
	if got := env.ExplainAuth(other); !errors.Is(got, other) || got.Error() != other.Error() {
		t.Errorf("a non-401 was rewritten: %v", got)
	}
	if env.ExplainAuth(nil) != nil {
		t.Error("nil was rewritten")
	}
}

// TestDeviceConfigSetsAuthStyleInParams pins the spike's finding: left
// unset, x/oauth2 probes, which doubles every poll and masks
// access_denied so a denied login hangs until the code expires.
//
// @decision(DL-006)
func TestDeviceConfigSetsAuthStyleInParams(t *testing.T) {
	cfg := deviceConfig("https://spov.example.com/")
	if cfg.Endpoint.AuthStyle != oauth2.AuthStyleInParams {
		t.Error("AuthStyle is not InParams, so the client will probe")
	}
	if cfg.Endpoint.DeviceAuthURL != "https://spov.example.com/device/code" {
		t.Errorf("device auth URL = %q", cfg.Endpoint.DeviceAuthURL)
	}
	if cfg.Endpoint.TokenURL != "https://spov.example.com/device/token" {
		t.Errorf("token URL = %q", cfg.Endpoint.TokenURL)
	}
	// A trailing slash on the server URL must not double up.
	if strings.Contains(cfg.Endpoint.TokenURL, "//device") {
		t.Errorf("token URL has a doubled slash: %q", cfg.Endpoint.TokenURL)
	}
}

// TestRedactURLKeepsTheDeviceCodeOutOfErrors: x/oauth2 renders the
// request it made, and the token request carries the device code.
//
// @decision(DL-006)
func TestRedactURLKeepsTheDeviceCodeOutOfErrors(t *testing.T) {
	err := errors.New(`Post "https://spov.example.com/device/token": grant_type=x&device_code=SECRETVALUE&client_id=spov-cli failed`)
	got := redactURL(err).Error()
	if strings.Contains(got, "SECRETVALUE") {
		t.Errorf("the device code survived redaction: %s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("nothing was redacted: %s", got)
	}
	// The rest of the message is intact, since it is what diagnoses.
	if !strings.Contains(got, "client_id=spov-cli") {
		t.Errorf("redaction ate the rest of the message: %s", got)
	}
	// Nothing to redact passes through unchanged.
	plain := errors.New("connection refused")
	if got := redactURL(plain); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Errorf("a message with no code was rewritten: %v", got)
	}
	if redactURL(nil) != nil {
		t.Error("nil was rewritten")
	}
}

// TestDefaultTokenNameIdentifiesTheMachine.
//
// @decision(DL-006)
func TestDefaultTokenNameIdentifiesTheMachine(t *testing.T) {
	name := defaultTokenName()
	if name == "" {
		t.Fatal("empty token name")
	}
	if !strings.Contains(name, "spov") {
		t.Errorf("name %q does not mention spov", name)
	}
}

// TestWhoamiPrintsTheServersView.
//
// @decision(DL-006)
func TestWhoamiPrintsTheServersView(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)
	srv.approve()
	if err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true}); err != nil {
		t.Fatal(err)
	}
	env.Client = nil
	env.Out = &strings.Builder{}

	cmd := newWhoamiCmd(env)
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := env.Out.(*strings.Builder).String()
	if !strings.Contains(out, "alice@example.com") {
		t.Errorf("whoami printed %q", out)
	}
	if !strings.Contains(out, "user-1") || !strings.Contains(out, "default") {
		t.Errorf("whoami does not name the user and org: %q", out)
	}
}

// TestWhoamiReportsAnUnauthenticatedServer rather than printing an
// empty identity.
//
// @decision(DL-006)
func TestWhoamiReportsAnUnauthenticatedServer(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)
	cmd := newWhoamiCmd(env)
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("whoami succeeded with no credential")
	}
}

// TestDeviceErrorTranslatesTheRFCCodes: the wire vocabulary means
// nothing to a person.
//
// @decision(DL-006)
func TestDeviceErrorTranslatesTheRFCCodes(t *testing.T) {
	denied := &oauth2.RetrieveError{ErrorCode: "access_denied"}
	if got := deviceError(denied).Error(); !strings.Contains(got, "denied") || strings.Contains(got, "access_denied") {
		t.Errorf("access_denied became %q", got)
	}
	expired := &oauth2.RetrieveError{ErrorCode: "expired_token"}
	got := deviceError(expired).Error()
	if !strings.Contains(got, "expired") {
		t.Errorf("expired_token became %q", got)
	}
	if !strings.Contains(got, "again") {
		t.Errorf("the expiry message does not say what to do: %q", got)
	}
	// A cancelled wait is not a failure to explain away.
	if got := deviceError(context.Canceled).Error(); !strings.Contains(got, "gave up") {
		t.Errorf("a cancelled wait became %q", got)
	}
	// Anything else keeps its cause.
	other := errors.New("connection refused")
	if got := deviceError(other).Error(); !strings.Contains(got, "connection refused") {
		t.Errorf("an unknown failure lost its cause: %q", got)
	}
}

// TestStoredServerTokenIsAbsentRatherThanAnError: a missing credential
// must let the request go out and the 401 explain, which is a better
// message than one about the keyring.
//
// @decision(DL-006)
func TestStoredServerTokenIsAbsentRatherThanAnError(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)
	if got := env.storedServerToken(srv.URL); got != "" {
		t.Errorf("storedServerToken = %q, want empty", got)
	}
	// No credential store at all is also empty, not a panic.
	env.Credentials = nil
	if got := env.storedServerToken(srv.URL); got != "" {
		t.Errorf("with no store, storedServerToken = %q", got)
	}
	// And a URL that is not a server URL.
	env, _ = loginEnv(t, srv)
	if got := env.storedServerToken("not a url"); got != "" {
		t.Errorf("with a bad URL, storedServerToken = %q", got)
	}
}

// TestServerLoginNeedsACredentialStore: there would be nowhere to keep
// the token, so it fails before printing a code nobody can use.
//
// @decision(DL-006)
func TestServerLoginNeedsACredentialStore(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)
	env.Credentials = nil
	err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true})
	if err == nil {
		t.Fatal("a login with nowhere to store the token succeeded")
	}
	if !strings.Contains(err.Error(), "credential store") {
		t.Errorf("err = %v", err)
	}
	// It failed before bothering the server.
	srv.mu.Lock()
	polls := srv.polls
	srv.mu.Unlock()
	if polls != 0 {
		t.Errorf("the server was polled %d times before the failure", polls)
	}

	// Logout has the same requirement.
	if err := runServerLogout(context.Background(), env); err == nil {
		t.Error("a logout with no credential store succeeded")
	}
}

// TestServerLoginReportsAnUnreachableServer, rather than hanging or
// printing a code for a server that is not there.
//
// @decision(DL-006)
func TestServerLoginReportsAnUnreachableServer(t *testing.T) {
	srv := newDeviceServer(t)
	env, _ := loginEnv(t, srv)
	srv.Close()

	err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true})
	if err == nil {
		t.Fatal("a login against a dead server succeeded")
	}
	if !strings.Contains(err.Error(), "starting the login") {
		t.Errorf("err = %v, want it to say which step failed", err)
	}
}

// TestServerLoginSucceedsWhenTheIdentityCannotBeReadBack: the token is
// stored and usable, so failing the command would be wrong.
//
// @decision(DL-006)
func TestServerLoginSucceedsWhenTheIdentityCannotBeReadBack(t *testing.T) {
	srv := newDeviceServer(t)
	env, creds := loginEnv(t, srv)
	srv.approve()
	// The token is issued, but /api/v1/me refuses it, as an old server
	// or a proxy might.
	srv.mu.Lock()
	srv.issued = ""
	srv.mu.Unlock()

	if err := runServerLogin(context.Background(), env, serverLoginOptions{NoBrowser: true}); err != nil {
		t.Fatalf("the login failed although a token was issued: %v", err)
	}
	key, _ := credentials.ServerKey(srv.URL)
	if _, err := creds.Get(key); err != nil {
		t.Errorf("the token was not stored: %v", err)
	}
	if out := env.Out.(*strings.Builder).String(); !strings.Contains(out, "Signed in") {
		t.Errorf("out = %q", out)
	}
}

// TestHostedServerURLPrefersTheFlag, then the environment, then config.
//
// @decision(DL-006) @decision(DL-002)
func TestHostedServerURLPrefersTheFlag(t *testing.T) {
	env := &Env{
		Out: &strings.Builder{}, Err: &strings.Builder{},
		Environ:   config.MapEnv{Vars: map[string]string{ServerEnvVar: "https://from-env.example.com"}},
		Workdir:   t.TempDir(),
		ServerURL: "https://from-flag.example.com",
	}
	got, err := env.hostedServerURL()
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://from-flag.example.com" {
		t.Errorf("hostedServerURL = %q, want the flag's", got)
	}

	env.ServerURL = ""
	if got, err = env.hostedServerURL(); err != nil || got != "https://from-env.example.com" {
		t.Errorf("hostedServerURL = %q, %v; want the environment's", got, err)
	}

	// A malformed URL is refused rather than passed to a request.
	env.Environ = config.MapEnv{Vars: map[string]string{ServerEnvVar: "not a url"}}
	if _, err := env.hostedServerURL(); err == nil {
		t.Error("a malformed server URL was accepted")
	}
}
