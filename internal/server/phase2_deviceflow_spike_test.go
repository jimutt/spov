//go:build integration

// Spike: does x/oauth2's device-flow client interoperate with the
// hand-rolled RFC 8628 server DL-006 describes?
//
//	go test -tags integration ./internal/server/ -run Phase2Device -v
//
// Nothing here touches the network or Modal; it is tagged only to keep a
// spike out of the default run.
package server_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/crc32"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// userCodeAlphabet is DL-006's: no vowels, so no words form, and no
// characters that are read for one another.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// deviceCodeState is one row of what DL-006 stores in device_codes.
type deviceCodeState struct {
	userCode  string
	status    string // pending, approved, denied
	expiresAt time.Time
	interval  int64
	lastPoll  time.Time
	issued    bool
}

// spikeServer is the smallest server DL-006 describes: the three
// endpoints, the token format, and the RFC 8628 error vocabulary.
type spikeServer struct {
	mu sync.Mutex
	// keyed by sha256 of the device code, as DL-006 stores it
	codes map[[32]byte]*deviceCodeState
	// user code -> device code hash, for the approval page
	byUserCode map[string][32]byte
	// sha256 of issued tokens
	tokens map[[32]byte]string

	now func() time.Time
}

func newSpikeServer() *spikeServer {
	return &spikeServer{
		codes:      map[[32]byte]*deviceCodeState{},
		byUserCode: map[string][32]byte{},
		tokens:     map[[32]byte]string{},
		now:        time.Now,
	}
}

func randomUserCode() (string, error) {
	b := make([]byte, 8)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(userCodeAlphabet))))
		if err != nil {
			return "", err
		}
		b[i] = userCodeAlphabet[n.Int64()]
	}
	return string(b[:4]) + "-" + string(b[4:]), nil
}

// mintToken builds DL-006's token: the prefix, 32 random bytes in base62,
// and a 6-character base62 CRC32 of the random part, so a malformed token
// is refused before any database hit.
func mintToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	body := base62(raw)
	sum := crc32.ChecksumIEEE([]byte(body))
	check := base62(bigEndian32(sum))
	for len(check) < 6 {
		check = "0" + check
	}
	return "spov_pat_" + body + check[:6], nil
}

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func base62(b []byte) string {
	n := new(big.Int).SetBytes(b)
	if n.Sign() == 0 {
		return "0"
	}
	base := big.NewInt(62)
	mod := new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, base62Alphabet[mod.Int64()])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func bigEndian32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

// oauthError writes an RFC 6749 §5.2 error body, which is what
// x/oauth2 parses out of a non-2xx response.
func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func (s *spikeServer) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /device/code", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if r.Form.Get("client_id") != "spov-cli" {
			oauthError(w, http.StatusBadRequest, "invalid_client")
			return
		}

		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error")
			return
		}
		deviceCode := base64.RawURLEncoding.EncodeToString(raw)
		userCode, err := randomUserCode()
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error")
			return
		}

		sum := sha256.Sum256([]byte(deviceCode))
		s.mu.Lock()
		s.codes[sum] = &deviceCodeState{
			userCode:  userCode,
			status:    "pending",
			expiresAt: s.now().Add(10 * time.Minute),
			interval:  1, // 5 in production; 1 keeps the spike quick
		}
		s.byUserCode[userCode] = sum
		s.mu.Unlock()

		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               deviceCode,
			"user_code":                 userCode,
			"verification_uri":          base + "/device",
			"verification_uri_complete": base + "/device?code=" + userCode,
			"expires_in":                600,
			"interval":                  1,
		})
	})

	mux.HandleFunc("POST /device/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if got := r.Form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
			oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
			return
		}
		sum := sha256.Sum256([]byte(r.Form.Get("device_code")))

		s.mu.Lock()
		defer s.mu.Unlock()
		st, ok := s.codes[sum]
		if !ok {
			oauthError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		now := s.now()
		if now.After(st.expiresAt) {
			oauthError(w, http.StatusBadRequest, "expired_token")
			return
		}

		// Polling faster than the interval raises it by 5 s, per §3.5.
		if !st.lastPoll.IsZero() && now.Sub(st.lastPoll) < time.Duration(st.interval)*time.Second {
			st.interval += 5
			st.lastPoll = now
			oauthError(w, http.StatusBadRequest, "slow_down")
			return
		}
		st.lastPoll = now

		switch st.status {
		case "pending":
			oauthError(w, http.StatusBadRequest, "authorization_pending")
		case "denied":
			oauthError(w, http.StatusBadRequest, "access_denied")
		case "approved":
			if st.issued {
				// A device code buys exactly one token.
				oauthError(w, http.StatusBadRequest, "invalid_grant")
				return
			}
			token, err := mintToken()
			if err != nil {
				oauthError(w, http.StatusInternalServerError, "server_error")
				return
			}
			st.issued = true
			s.tokens[sha256.Sum256([]byte(token))] = "user@example.com"
			delete(s.byUserCode, st.userCode)
			delete(s.codes, sum)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": token,
				"token_type":   "bearer",
			})
		}
	})

	// The approval page, standing in for the logged-in human.
	mux.HandleFunc("POST /device/approve", func(w http.ResponseWriter, r *http.Request) {
		code := strings.ToUpper(r.URL.Query().Get("code"))
		decision := r.URL.Query().Get("decision")
		s.mu.Lock()
		defer s.mu.Unlock()
		sum, ok := s.byUserCode[code]
		if !ok {
			// A wrong code never reveals whether it exists.
			w.WriteHeader(http.StatusOK)
			return
		}
		s.codes[sum].status = decision
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

// config is what DL-006's CLI builds. AuthStyle is explicit: left
// unset, x/oauth2 probes, sending client_id in the Authorization header
// first and retrying in the body on *any* error. That doubles every poll
// and, worse, replaces the server's real error with the second attempt's,
// so access_denied never reaches the caller.
func (s *spikeServer) config(base string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: "spov-cli",
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: base + "/device/code",
			TokenURL:      base + "/device/token",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

// probingConfig leaves AuthStyle unset, to demonstrate the failure.
func (s *spikeServer) probingConfig(base string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: "spov-cli",
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: base + "/device/code",
			TokenURL:      base + "/device/token",
		},
	}
}

// TestPhase2DeviceFlowHappyPath is the spike's core question: does the
// real client complete a flow against this server?
func TestPhase2DeviceFlowHappyPath(t *testing.T) {
	s := newSpikeServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	cfg := s.config(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		t.Fatalf("DeviceAuth: %v", err)
	}
	t.Logf("user_code=%s verification_uri=%s interval=%d expires=%s",
		da.UserCode, da.VerificationURI, da.Interval, time.Until(da.Expiry).Round(time.Second))

	if len(da.UserCode) != 9 || da.UserCode[4] != '-' {
		t.Errorf("user code %q is not XXXX-XXXX", da.UserCode)
	}
	if da.VerificationURIComplete == "" {
		t.Error("no verification_uri_complete")
	}
	if da.Expiry.IsZero() {
		t.Error("client did not parse expires_in")
	}

	// The human approves while the client polls.
	go func() {
		time.Sleep(1500 * time.Millisecond)
		res, err := srv.Client().Post(
			srv.URL+"/device/approve?code="+da.UserCode+"&decision=approved", "", nil)
		if err == nil {
			_ = res.Body.Close()
		}
	}()

	tok, err := cfg.DeviceAccessToken(ctx, da)
	if err != nil {
		t.Fatalf("DeviceAccessToken: %v", err)
	}
	if !strings.HasPrefix(tok.AccessToken, "spov_pat_") {
		t.Errorf("token %q lacks the spov_pat_ prefix", tok.AccessToken)
	}
	if tok.TokenType != "bearer" && tok.TokenType != "Bearer" {
		t.Errorf("token type %q", tok.TokenType)
	}
	t.Logf("issued a %d-character token, type %q", len(tok.AccessToken), tok.TokenType)

	// DL-006: a device code buys exactly one token.
	if _, err := cfg.DeviceAccessToken(ctx, da); err == nil {
		t.Error("the same device code was redeemed twice")
	}
}

// TestPhase2DeviceFlowDenied checks that a denial reaches the client as an
// error rather than a hang, which is what the CLI reports to the user.
func TestPhase2DeviceFlowDenied(t *testing.T) {
	s := newSpikeServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	cfg := s.config(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		t.Fatalf("DeviceAuth: %v", err)
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		res, err := srv.Client().Post(
			srv.URL+"/device/approve?code="+da.UserCode+"&decision=denied", "", nil)
		if err == nil {
			_ = res.Body.Close()
		}
	}()

	_, err = cfg.DeviceAccessToken(ctx, da)
	if err == nil {
		t.Fatal("a denied request returned a token")
	}
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		t.Fatalf("error %v is not a RetrieveError, so the CLI cannot tell denial from a fault", err)
	}
	if re.ErrorCode != "access_denied" {
		t.Errorf("error code %q, want access_denied", re.ErrorCode)
	}
	t.Logf("denial surfaced as %q", re.ErrorCode)
}

// TestPhase2DeviceFlowExpired checks the expiry path. The client sets its
// own deadline from expires_in, so an expired code can surface as either
// the server's expired_token or the client's deadline.
func TestPhase2DeviceFlowExpired(t *testing.T) {
	s := newSpikeServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	cfg := s.config(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		t.Fatalf("DeviceAuth: %v", err)
	}

	// Expire it server-side, as if the human never acted.
	s.mu.Lock()
	for _, st := range s.codes {
		st.expiresAt = time.Now().Add(-time.Second)
	}
	s.mu.Unlock()

	_, err = cfg.DeviceAccessToken(ctx, da)
	if err == nil {
		t.Fatal("an expired code returned a token")
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorCode != "expired_token" {
			t.Errorf("error code %q, want expired_token", re.ErrorCode)
		}
		t.Logf("expiry surfaced as %q", re.ErrorCode)
		return
	}
	t.Logf("expiry surfaced as a client-side error: %v", err)
}

// TestPhase2SlowDownRaisesInterval checks the §3.5 behaviour DL-006
// implements: a too-fast poll is answered with slow_down and the client
// backs off rather than failing.
func TestPhase2SlowDownRaisesInterval(t *testing.T) {
	s := newSpikeServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	ctx := context.Background()
	cfg := s.config(srv.URL)
	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		t.Fatalf("DeviceAuth: %v", err)
	}

	// Poll twice in a row by hand, faster than the interval.
	poll := func() string {
		form := strings.NewReader(
			"client_id=spov-cli&grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=" + da.DeviceCode)
		req, _ := http.NewRequest("POST", srv.URL+"/device/token", form)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		defer res.Body.Close()
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&body)
		return body.Error
	}

	if got := poll(); got != "authorization_pending" {
		t.Fatalf("first poll: %q, want authorization_pending", got)
	}
	if got := poll(); got != "slow_down" {
		t.Fatalf("immediate second poll: %q, want slow_down", got)
	}

	s.mu.Lock()
	var interval int64
	for _, st := range s.codes {
		interval = st.interval
	}
	s.mu.Unlock()
	if interval != 6 {
		t.Errorf("interval is %d, want 6 (1 + 5)", interval)
	}
	t.Logf("slow_down raised the interval to %d", interval)
}

// TestPhase2TokenChecksum covers the claim that a malformed token is
// refused before any database hit.
func TestPhase2TokenChecksum(t *testing.T) {
	tok, err := mintToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Logf("token: %s… (%d chars)", tok[:20], len(tok))

	valid := func(s string) bool {
		if !strings.HasPrefix(s, "spov_pat_") {
			return false
		}
		rest := strings.TrimPrefix(s, "spov_pat_")
		if len(rest) < 7 {
			return false
		}
		body, check := rest[:len(rest)-6], rest[len(rest)-6:]
		want := base62(bigEndian32(crc32.ChecksumIEEE([]byte(body))))
		for len(want) < 6 {
			want = "0" + want
		}
		return check == want[:6]
	}

	if !valid(tok) {
		t.Error("a freshly minted token failed its own checksum")
	}
	// One flipped character in the body must break the checksum.
	bad := []byte(tok)
	i := len(bad) - 10
	if bad[i] == 'a' {
		bad[i] = 'b'
	} else {
		bad[i] = 'a'
	}
	if valid(string(bad)) {
		t.Error("a corrupted token passed the checksum")
	}
	if valid("spov_pat_short") || valid("nope") {
		t.Error("a malformed token passed the checksum")
	}
}

// TestPhase2UserCodeEntropy documents the alphabet's properties, since
// DL-006 leans on them for the approval page's rate limit to be enough.
func TestPhase2UserCodeEntropy(t *testing.T) {
	if strings.ContainsAny(userCodeAlphabet, "AEIOU") {
		t.Error("the alphabet contains vowels, so codes can spell words")
	}
	// The confusable pairs are 0/O, 1/I/L and 5/S. DL-006's alphabet keeps
	// L and S but excludes every character they are confused with, so no
	// pair of alphabet members is ambiguous — which is the property that
	// matters when a human retypes a code.
	for _, c := range "01IO" {
		if strings.ContainsRune(userCodeAlphabet, c) {
			t.Errorf("the alphabet contains %q, which pairs with a member", c)
		}
	}
	space := new(big.Int).Exp(big.NewInt(int64(len(userCodeAlphabet))), big.NewInt(8), nil)
	t.Logf("alphabet %d characters, 8 positions, space %s", len(userCodeAlphabet), space)
	if space.Cmp(big.NewInt(1e10)) < 0 {
		t.Errorf("space %s is under 10^10", space)
	}

	seen := map[string]bool{}
	for range 1000 {
		c, err := randomUserCode()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if seen[c] {
			t.Fatalf("duplicate user code %q in 1000 draws", c)
		}
		seen[c] = true
	}
}

// TestPhase2AuthStyleProbeMasksErrors pins the finding that shaped
// DL-006's client configuration: with AuthStyle unset, x/oauth2 masks a
// terminal error as a client-side timeout.
func TestPhase2AuthStyleProbeMasksErrors(t *testing.T) {
	s := newSpikeServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	cfg := s.probingConfig(srv.URL)
	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		t.Fatalf("DeviceAuth: %v", err)
	}

	// Deny immediately, so the only reason to keep polling is the probe.
	s.mu.Lock()
	for _, st := range s.codes {
		st.status = "denied"
	}
	s.mu.Unlock()

	_, err = cfg.DeviceAccessToken(ctx, da)
	if err == nil {
		t.Fatal("a denied request returned a token")
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "access_denied" {
		t.Fatal("the probe no longer masks the error; DL-006's AuthStyleInParams may be unnecessary — re-check before relying on it")
	}
	t.Logf("with AuthStyle unset the denial is masked as: %v", err)
}
