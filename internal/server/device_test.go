package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// deviceFixture is a login fixture with the device flow wired up, so a
// test can drive the whole thing: a CLI asking for a code, a human
// approving it in a browser, and the token that comes back.
type deviceFixture struct {
	*loginFixture
	device *Device
	now    time.Time
}

func newDeviceFixture(t *testing.T, allowlist string) *deviceFixture {
	t.Helper()
	f := &deviceFixture{now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
	f.loginFixture = newLoginFixtureWith(t, allowlist, func(d *Deps, l *Login) {
		f.device = &Device{
			Store: d.Store,
			Login: l,
			// One second, so the client's mandatory first wait does
			// not cost five per exchange.
			Interval: 1,
			Now:      func() time.Time { return f.now },
			Warn:     d.Warn,
		}
		d.Device = f.device
		// The CLI token is a second way to the same principal.
		d.Auth = append(d.Auth.(auth.Chain), &auth.TokenAuthenticator{
			Tokens: d.Store.Tokens(), Users: d.Store.Users(),
			Allowlist: l.Allowlist, Now: func() time.Time { return f.now },
		})
	})
	// The verification URI is built from the public origin, which for a
	// test server is wherever it landed.
	f.device.PublicURL = f.http.URL
	return f
}

// oauthConfig is the client DL-006's CLI builds, pointed at the fixture.
func (f *deviceFixture) oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID: DeviceClientID,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: f.http.URL + DeviceCodePath,
			TokenURL:      f.http.URL + "/device/token",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

// approve walks a human through the approval page for a user code.
func (f *deviceFixture) approve(code string, action string) response {
	f.t.Helper()
	// Two steps: the code resolves, then the confirmation.
	form := url.Values{"code": {code}}
	if resp := f.postForm(DeviceVerifyPath, form); resp.StatusCode != http.StatusOK {
		f.t.Fatalf("the first approval step = %d: %s", resp.StatusCode, resp.Body)
	}
	form.Set("confirm", "1")
	form.Set("action", action)
	return f.postForm(DeviceVerifyPath, form)
}

// accessToken exchanges an approved device code directly, without
// x/oauth2's polling loop, which otherwise costs a real second per
// exchange (RFC 8628's whole-second interval). What these tests are
// about is the server's token endpoint, not the client library's
// timing; TestTheRealOAuthClientCompletesTheFlow keeps one path on the
// real client.
//
// @decision(DL-006)
func (f *deviceFixture) accessToken(deviceCode string) string {
	f.t.Helper()
	form := url.Values{
		"grant_type":  {DeviceGrantType},
		"device_code": {deviceCode},
		"client_id":   {DeviceClientID},
	}
	resp := f.postForm(deviceTokenPath, form)
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("exchanging the code = %d: %s", resp.StatusCode, resp.Body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		f.t.Fatalf("the token response is not JSON: %v: %s", err, resp.Body)
	}
	if out.AccessToken == "" {
		f.t.Fatalf("the token response carries no access_token: %s", resp.Body)
	}
	return out.AccessToken
}

// postForm submits a form with the fixture's cookie jar.
func (f *deviceFixture) postForm(path string, form url.Values) response {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.http.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return f.send(req)
}

// TestDeviceFlowIssuesAWorkingToken is the whole point: a CLI asks, a
// human approves, and the token authenticates the API.
//
// @decision(DL-006)
func TestDeviceFlowIssuesAWorkingToken(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("") // the human signs in first

	cfg := f.oauthConfig()
	ctx := context.Background()
	da, err := cfg.DeviceAuth(ctx, oauth2.SetAuthURLParam("name", "laptop · spov test"))
	if err != nil {
		t.Fatalf("device auth: %v", err)
	}
	if da.UserCode == "" || da.DeviceCode == "" {
		t.Fatal("the server returned an empty code")
	}
	if da.VerificationURI == "" {
		t.Fatal("no verification URI")
	}
	// The complete form carries the code, so a browser need not be told
	// it twice.
	if !strings.Contains(da.VerificationURIComplete, url.QueryEscape(da.UserCode)) {
		t.Errorf("verification_uri_complete %q does not carry the code", da.VerificationURIComplete)
	}

	if resp := f.approve(da.UserCode, "approve"); resp.StatusCode != http.StatusOK {
		t.Fatalf("approving = %d: %s", resp.StatusCode, resp.Body)
	}

	// Through x/oauth2's real polling loop, deliberately, and this is
	// the one test that pays its real-second cost: the CLI logs in
	// with this library, so something has to prove spov's endpoints
	// work with the real client and not only a hand-rolled form post.
	//
	// @decision(DL-006)
	token, err := cfg.DeviceAccessToken(ctx, da)
	if err != nil {
		t.Fatalf("exchanging the code: %v", err)
	}
	if _, err := auth.ParseToken(token.AccessToken); err != nil {
		t.Errorf("the issued token is malformed: %v", err)
	}

	// And it authenticates the API as the approving user.
	me := f.meWithToken(token.AccessToken)
	if me.Email != "alice@example.com" {
		t.Errorf("the token authenticated as %+v", me)
	}

	// The name the CLI proposed is what the listing shows.
	tokens := f.listTokensWith(token.AccessToken)
	if len(tokens) != 1 {
		t.Fatalf("got %d tokens", len(tokens))
	}
	if tokens[0].Name != "laptop · spov test" {
		t.Errorf("token name = %q", tokens[0].Name)
	}
	if !tokens[0].Current {
		t.Error("the token authenticating the request is not marked current")
	}
}

// meWithToken reads /api/v1/me with a bearer token and no cookie.
func (f *deviceFixture) meWithToken(token string) MeView {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/v1/me", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp := f.sendNoCookies(req)
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("/api/v1/me with a token = %d: %s", resp.StatusCode, resp.Body)
	}
	var me MeView
	if err := json.Unmarshal(resp.Body, &me); err != nil {
		f.t.Fatal(err)
	}
	return me
}

func (f *deviceFixture) listTokensWith(token string) []TokenView {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/v1/me/tokens", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp := f.sendNoCookies(req)
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("listing tokens = %d: %s", resp.StatusCode, resp.Body)
	}
	var out struct {
		Tokens []TokenView `json:"tokens"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Tokens
}

// TestDeviceFlowDeniedReachesTheClient. The spike found that without
// AuthStyleInParams the client's probe masks this and login hangs until
// the code expires; this proves the server's half.
//
// @decision(DL-006)
func TestDeviceFlowDeniedReachesTheClient(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")

	cfg := f.oauthConfig()
	ctx := context.Background()
	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resp := f.approve(da.UserCode, "deny"); resp.StatusCode != http.StatusOK {
		t.Fatalf("denying = %d: %s", resp.StatusCode, resp.Body)
	}

	// Polled directly: what is under test is the token endpoint's
	// answer to a denied code, not x/oauth2's mapping of it — and the
	// library waits a full RFC 8628 interval, a whole second, before
	// its first request.
	if got := f.poll(da.DeviceCode); got != "access_denied" {
		t.Fatalf("a denied login answered %q, want access_denied", got)
	}
	// Nothing was issued.
	if n := f.countTokens(); n != 0 {
		t.Errorf("a denied login left %d tokens", n)
	}
}

func (f *deviceFixture) countTokens() int {
	f.t.Helper()
	users, err := f.store.Users().List(context.Background(), DefaultOrgID)
	if err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, u := range users {
		toks, err := f.store.Tokens().ListByUser(context.Background(), u.OrgID, u.ID)
		if err != nil {
			f.t.Fatal(err)
		}
		n += len(toks)
	}
	return n
}

// TestPollingBeforeApprovalIsPending, which is what keeps the client
// waiting rather than failing.
//
// @decision(DL-006)
func TestPollingBeforeApprovalIsPending(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	code := f.startFlow()
	if got := f.poll(code); got != "authorization_pending" {
		t.Errorf("polling an unapproved code = %q", got)
	}
}

// TestPollingTooFastIsSlowedDown, per RFC 8628 §3.5.
//
// @decision(DL-006)
func TestPollingTooFastIsSlowedDown(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	code := f.startFlow()

	if got := f.poll(code); got != "authorization_pending" {
		t.Fatalf("the first poll = %q", got)
	}
	// Immediately again, inside the interval: refused, and the interval
	// is raised by DeviceSlowDown.
	if got := f.poll(code); got != "slow_down" {
		t.Errorf("an immediate second poll = %q, want slow_down", got)
	}
	// The original interval is no longer enough, and being refused
	// raises it again.
	base := f.device.interval()
	f.now = f.now.Add(time.Duration(base) * time.Second)
	if got := f.poll(code); got != "slow_down" {
		t.Errorf("a poll at the old interval = %q, want slow_down", got)
	}
	// Waiting out the interval as it now stands is honoured. Each
	// slow_down also resets the clock it is measured from, so the wait
	// runs from that last refusal.
	f.now = f.now.Add(time.Duration(base+2*DeviceSlowDown+1) * time.Second)
	if got := f.poll(code); got != "authorization_pending" {
		t.Errorf("a poll past the raised interval = %q", got)
	}
}

// TestAnExpiredCodeIsRefused.
//
// @decision(DL-006)
func TestAnExpiredCodeIsRefused(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	code := f.startFlow()
	f.now = f.now.Add(DeviceCodeLifetime + time.Minute)
	if got := f.poll(code); got != "expired_token" {
		t.Errorf("an expired code = %q", got)
	}
}

// TestADeviceCodeBuysExactlyOneToken: a replayed exchange must not mint
// a second credential.
//
// @decision(DL-006)
func TestADeviceCodeBuysExactlyOneToken(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	cfg := f.oauthConfig()
	ctx := context.Background()
	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.approve(da.UserCode, "approve")

	f.accessToken(da.DeviceCode)
	// The same device code again.
	f.now = f.now.Add(time.Minute)
	if got := f.poll(da.DeviceCode); got != "invalid_grant" {
		t.Errorf("a replayed exchange = %q, want invalid_grant", got)
	}
	if n := f.countTokens(); n != 1 {
		t.Errorf("a replayed exchange left %d tokens, want 1", n)
	}
}

// TestAnUnknownDeviceCodeIsInvalidGrant.
//
// @decision(DL-006)
func TestAnUnknownDeviceCodeIsInvalidGrant(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	if got := f.poll("not-a-real-device-code"); got != "invalid_grant" {
		t.Errorf("an unknown code = %q", got)
	}
}

// startFlow asks for a code and returns the device code.
func (f *deviceFixture) startFlow() string {
	f.t.Helper()
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return da.DeviceCode
}

// poll exchanges a device code once and returns the RFC error code, or
// "" when a token came back.
func (f *deviceFixture) poll(deviceCode string) string {
	f.t.Helper()
	form := url.Values{
		"grant_type":  {DeviceGrantType},
		"device_code": {deviceCode},
		"client_id":   {DeviceClientID},
	}
	resp := f.postForm("/device/token", form)
	var body struct {
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		f.t.Fatalf("poll returned %d %s", resp.StatusCode, resp.Body)
	}
	return body.Error
}

// TestTheApprovalPageNeedsALogin: it is where a human authorises a
// credential, so an anonymous visitor is sent to sign in.
//
// @decision(DL-006)
func TestTheApprovalPageNeedsALogin(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	resp := f.get(DeviceVerifyPath)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the approval page without a login = %d, want a redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, LoginPath) {
		t.Errorf("redirected to %q, want the login", loc)
	}
	// And it comes back afterwards.
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, url.QueryEscape(DeviceVerifyPath)) {
		t.Errorf("the login redirect %q does not return to the approval page", loc)
	}
}

// TestApprovingRequiresAConfirmation: the page must say what is being
// approved before it is, or it is a one-click grant of full access.
//
// @decision(DL-006)
func TestApprovingRequiresAConfirmation(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	da, err := f.oauthConfig().DeviceAuth(context.Background(),
		oauth2.SetAuthURLParam("name", "some-host · spov 1.2.3"))
	if err != nil {
		t.Fatal(err)
	}

	// The first submit shows what is being approved and grants nothing.
	resp := f.postForm(DeviceVerifyPath, url.Values{"code": {da.UserCode}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the first step = %d", resp.StatusCode)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "some-host") {
		t.Errorf("the page does not name what is asking: %s", body)
	}
	if !strings.Contains(body, "alice@example.com") {
		t.Errorf("the page does not name who it would act as: %s", body)
	}
	// Nothing is approved yet.
	if got := f.poll(da.DeviceCode); got != "authorization_pending" {
		t.Errorf("the first step approved the code: %q", got)
	}
}

// TestAWrongCodeDoesNotRevealWhetherItExists.
//
// @decision(DL-006)
func TestAWrongCodeDoesNotRevealWhetherItExists(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Approve it, so the code exists but is no longer pending.
	f.approve(da.UserCode, "approve")

	used := f.postForm(DeviceVerifyPath, url.Values{"code": {da.UserCode}})
	unknown := f.postForm(DeviceVerifyPath, url.Values{"code": {"ZZZZ-ZZZZ"}})
	if used.StatusCode != unknown.StatusCode {
		t.Errorf("a used code answered %d and an unknown one %d", used.StatusCode, unknown.StatusCode)
	}
	if string(used.Body) != string(unknown.Body) {
		t.Error("a used code and an unknown one produced different pages")
	}
}

// TestTooManyWrongCodesLocksOut, so a wrong-code loop cannot become an
// oracle.
//
// @decision(DL-006)
func TestTooManyWrongCodesLocksOut(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")

	for i := range MaxCodeAttempts {
		resp := f.postForm(DeviceVerifyPath, url.Values{"code": {"ZZZZ-ZZZZ"}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("wrong code %d = %d", i, resp.StatusCode)
		}
	}
	resp := f.postForm(DeviceVerifyPath, url.Values{"code": {"ZZZZ-ZZZZ"}})
	if !strings.Contains(string(resp.Body), "Too many") {
		t.Errorf("the %dth wrong code did not lock out: %s", MaxCodeAttempts+1, resp.Body)
	}
	// A correct code is refused while locked out, which is the point.
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	locked := f.postForm(DeviceVerifyPath, url.Values{"code": {da.UserCode}})
	if !strings.Contains(string(locked.Body), "Too many") {
		t.Errorf("a real code was accepted during the lockout: %s", locked.Body)
	}

	// It lifts.
	f.now = f.now.Add(AttemptLockout + time.Minute)
	after := f.postForm(DeviceVerifyPath, url.Values{"code": {da.UserCode}})
	if strings.Contains(string(after.Body), "Too many") {
		t.Errorf("the lockout did not lift: %s", after.Body)
	}
}

// TestTheApprovalPageEscapesTheTokenName: the name comes from whatever
// called /device/code, so it is attacker-controlled text on a page the
// victim is about to grant access on.
//
// @decision(DL-006)
func TestTheApprovalPageEscapesTheTokenName(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	const attack = `<script>alert(1)</script>`
	da, err := f.oauthConfig().DeviceAuth(context.Background(), oauth2.SetAuthURLParam("name", attack))
	if err != nil {
		t.Fatal(err)
	}
	resp := f.postForm(DeviceVerifyPath, url.Values{"code": {da.UserCode}})
	body := string(resp.Body)
	if strings.Contains(body, attack) {
		t.Errorf("the token name was not escaped: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("the name is missing entirely rather than escaped: %s", body)
	}
}

// TestADeviceCodeIsStoredHashed: a database dump must not yield codes
// that can be exchanged.
//
// @decision(DL-006)
func TestADeviceCodeIsStoredHashed(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The row is found by hash, and not by the code itself.
	if _, err := f.store.DeviceCodes().ByHash(context.Background(), auth.HashDeviceCode(da.DeviceCode)); err != nil {
		t.Fatalf("the code is not stored under its hash: %v", err)
	}
	if _, err := f.store.DeviceCodes().ByHash(context.Background(), []byte(da.DeviceCode)); !errors.Is(err, store.ErrNotFound) {
		t.Error("the raw device code resolves a row, so it is stored in the clear")
	}
}

// TestDeviceEndpointsAreOutsideTheAPI: a CLI polling for a token has no
// credential yet, which is what it is polling for.
//
// @decision(DL-006)
func TestDeviceEndpointsAreOutsideTheAPI(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	// No cookie, no bearer: both must still answer.
	resp := f.postFormNoCookies(DeviceCodePath, url.Values{})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST %s unauthenticated = %d", DeviceCodePath, resp.StatusCode)
	}
	poll := f.postFormNoCookies("/device/token", url.Values{
		"grant_type": {DeviceGrantType}, "device_code": {"nope"},
	})
	if poll.StatusCode == http.StatusUnauthorized {
		t.Error("the token endpoint demanded a credential")
	}
}

// postFormNoCookies submits a form without the fixture's cookie jar.
func (f *deviceFixture) postFormNoCookies(path string, form url.Values) response {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.http.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return f.sendNoCookies(req)
}

// TestTheAttemptLimiterForgetsIdleUsers, so the map does not grow with
// every user who ever mistyped a code.
//
// @decision(DL-006)
func TestTheAttemptLimiterForgetsIdleUsers(t *testing.T) {
	l := newAttemptLimiter()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	l.fail("user-1", now)
	if len(l.byID) != 1 {
		t.Fatalf("got %d records", len(l.byID))
	}
	// Long after, another user's failure sweeps the idle record.
	later := now.Add(2 * attemptSweepIdle)
	l.fail("user-2", later)
	if _, ok := l.byID["user-1"]; ok {
		t.Error("an idle record survived the sweep")
	}
	if _, ok := l.byID["user-2"]; !ok {
		t.Error("the sweep dropped the record it just made")
	}
}

// TestSweepNeverLiftsALockout: the sweep drops idle records, and a
// record still inside its lockout must not be one of them — dropping it
// would hand the user a clean slate.
//
// The two constants make this unreachable today (a record idle for an
// hour cannot be inside a fifteen-minute lockout), so the test pins the
// relationship rather than the branch: if the sweep interval is ever
// shortened below the lockout, the guard becomes load-bearing and this
// test starts exercising it.
//
// @decision(DL-006)
func TestSweepNeverLiftsALockout(t *testing.T) {
	if attemptSweepIdle <= AttemptLockout {
		t.Errorf("attemptSweepIdle (%s) is no longer than AttemptLockout (%s); "+
			"the sweep can now reach a locked-out record, so its guard is load-bearing",
			attemptSweepIdle, AttemptLockout)
	}

	// Whatever the constants, a locked-out record survives a sweep.
	l := newAttemptLimiter()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	for range MaxCodeAttempts {
		l.fail("locked", now)
	}
	until, locked := l.lockedUntil("locked", now)
	if !locked {
		t.Fatal("the user was not locked out")
	}
	// Sweep at the last moment the lockout is still running.
	stillLocked := now.Add(until - time.Second)
	l.sweep(stillLocked)
	if _, ok := l.byID["locked"]; !ok {
		t.Error("the sweep dropped a record whose lockout was still running")
	}
	if _, still := l.lockedUntil("locked", stillLocked); !still {
		t.Error("the lockout was lifted by a sweep")
	}
}

// TestTheAttemptWindowExpires: five wrong codes spread over an hour are
// not five in ten minutes.
//
// @decision(DL-006)
func TestTheAttemptWindowExpires(t *testing.T) {
	l := newAttemptLimiter()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	for range MaxCodeAttempts - 1 {
		l.fail("user-1", now)
	}
	// Past the window, the count restarts rather than tipping over.
	now = now.Add(AttemptWindow + time.Minute)
	l.fail("user-1", now)
	if _, locked := l.lockedUntil("user-1", now); locked {
		t.Error("attempts outside the window still locked the user out")
	}
}

// TestADeviceCodeRequestRefusesAnotherClient.
//
// @decision(DL-006)
func TestADeviceCodeRequestRefusesAnotherClient(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	resp := f.postFormNoCookies(DeviceCodePath, url.Values{"client_id": {"someone-else"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown client = %d, want 400", resp.StatusCode)
	}
	var body struct{ Error string }
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "invalid_client" {
		t.Errorf("error = %q, want invalid_client", body.Error)
	}
}

// TestTheTokenEndpointRefusesAnotherGrant.
//
// @decision(DL-006)
func TestTheTokenEndpointRefusesAnotherGrant(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"no grant type", url.Values{"device_code": {"x"}}, "unsupported_grant_type"},
		{"another grant", url.Values{"grant_type": {"authorization_code"}, "device_code": {"x"}}, "unsupported_grant_type"},
		{"no device code", url.Values{"grant_type": {DeviceGrantType}}, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.postFormNoCookies("/device/token", tc.form)
			var body struct{ Error string }
			if err := json.Unmarshal(resp.Body, &body); err != nil {
				t.Fatal(err)
			}
			if body.Error != tc.want {
				t.Errorf("error = %q, want %q", body.Error, tc.want)
			}
		})
	}
}

// TestTokenNameIsBoundedAndCleaned: it comes from whatever called
// /device/code and is shown on a page a human acts on.
//
// @decision(DL-006)
func TestTokenNameIsBoundedAndCleaned(t *testing.T) {
	if got := deviceTokenName(""); got != "spov CLI" {
		t.Errorf("an empty name = %q, want a default", got)
	}
	if got := deviceTokenName("   "); got != "spov CLI" {
		t.Errorf("a blank name = %q, want a default", got)
	}
	// Control characters would let a name rewrite a terminal's output
	// wherever it is echoed.
	if got := deviceTokenName("laptop\r\n\x1b[31mDANGER"); strings.ContainsAny(got, "\r\n\x1b") {
		t.Errorf("control characters survived: %q", got)
	}
	long := strings.Repeat("x", MaxTokenName*2)
	if got := deviceTokenName(long); len(got) > MaxTokenName {
		t.Errorf("a long name was not bounded: %d characters", len(got))
	}
}

// TestTheApprovalPageIsNotServedWithoutALogin: without one there is
// nobody who could approve.
//
// @decision(DL-006)
func TestTheApprovalPageIsNotServedWithoutALogin(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.device.Login = nil
	resp := f.get(DeviceVerifyPath)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the approval page with no login = %d, want 404", resp.StatusCode)
	}
}

// TestDeviceDefaultsApplyWhenUnset: the zero Device is the production
// one, so its defaults are what a real deployment runs on.
//
// @decision(DL-006)
func TestDeviceDefaultsApplyWhenUnset(t *testing.T) {
	d := &Device{}
	if got := d.interval(); got != DeviceInterval {
		t.Errorf("interval = %d, want %d", got, DeviceInterval)
	}
	if got := d.tokenLifetime(); got != DefaultTokenLifetime {
		t.Errorf("token lifetime = %s, want %s", got, DefaultTokenLifetime)
	}
	// Now defaults to the clock rather than the zero time, which would
	// expire every code the moment it was made.
	if d.now().IsZero() {
		t.Error("now() returned the zero time")
	}
	// A nil Warn discards rather than panicking.
	d.warn("nothing should happen")
}

// TestAnApprovedCodeCannotBeApprovedTwice: the second attempt looks
// like an unknown code, so a stale browser tab cannot re-authorise.
//
// @decision(DL-006)
func TestAnApprovedCodeCannotBeApprovedTwice(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.approve(da.UserCode, "approve")

	// The same code again, from the page.
	resp := f.postForm(DeviceVerifyPath, url.Values{"code": {da.UserCode}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("re-approving = %d, want it refused", resp.StatusCode)
	}
}

// TestApprovalAcceptsACodeTypedWithoutItsDash, since that is what a
// person retyping from a terminal often does.
//
// @decision(DL-006)
func TestApprovalAcceptsACodeTypedWithoutItsDash(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	typed := strings.ToLower(strings.ReplaceAll(da.UserCode, "-", ""))

	form := url.Values{"code": {typed}}
	if resp := f.postForm(DeviceVerifyPath, form); resp.StatusCode != http.StatusOK {
		t.Fatalf("a code typed without its dash = %d: %s", resp.StatusCode, resp.Body)
	}
	form.Set("confirm", "1")
	form.Set("action", "approve")
	if resp := f.postForm(DeviceVerifyPath, form); resp.StatusCode != http.StatusOK {
		t.Fatalf("confirming = %d", resp.StatusCode)
	}
	if got := f.poll(da.DeviceCode); got != "" {
		t.Errorf("the flow did not complete: %q", got)
	}
}

// TestTheApprovalPagePrefillsTheCodeFromTheCompleteURI, so the human
// need not retype it, and does not act on it without a submit.
//
// @decision(DL-006)
func TestTheApprovalPagePrefillsTheCodeFromTheCompleteURI(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	resp := f.get(DeviceVerifyPath + "?code=" + url.QueryEscape(da.UserCode))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the approval page = %d", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), da.UserCode) {
		t.Errorf("the code was not prefilled: %s", resp.Body)
	}
	// Merely visiting must not approve anything.
	if got := f.poll(da.DeviceCode); got != "authorization_pending" {
		t.Errorf("visiting the page approved the code: %q", got)
	}
}

// failingDeviceCodes injects failures into the device-code store, so
// the paths that answer server_error can be reached.
type failingDeviceCodes struct {
	store.DeviceCodeStore
	createErr error
	updateErr error
	byHashErr error
}

func (f failingDeviceCodes) Create(ctx context.Context, d store.DeviceCode) error {
	if f.createErr != nil {
		return f.createErr
	}
	return f.DeviceCodeStore.Create(ctx, d)
}

func (f failingDeviceCodes) Update(ctx context.Context, d store.DeviceCode) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	return f.DeviceCodeStore.Update(ctx, d)
}

func (f failingDeviceCodes) ByHash(ctx context.Context, hash []byte) (store.DeviceCode, error) {
	if f.byHashErr != nil {
		return store.DeviceCode{}, f.byHashErr
	}
	return f.DeviceCodeStore.ByHash(ctx, hash)
}

// faultyStore swaps in a device-code store that fails.
type faultyStore struct {
	store.Store
	codes store.DeviceCodeStore
}

func (s faultyStore) DeviceCodes() store.DeviceCodeStore { return s.codes }

// TestDeviceEndpointsReportAStoreFailureAsServerError rather than
// pretending the flow is progressing.
//
// @decision(DL-006)
func TestDeviceEndpointsReportAStoreFailureAsServerError(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	down := errors.New("the database is down")

	t.Run("asking for a code", func(t *testing.T) {
		f.device.Store = faultyStore{Store: f.store,
			codes: failingDeviceCodes{DeviceCodeStore: f.store.DeviceCodes(), createErr: down}}
		defer func() { f.device.Store = f.store }()

		resp := f.postFormNoCookies(DeviceCodePath, url.Values{})
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("a failed create = %d, want 500", resp.StatusCode)
		}
		var body struct{ Error string }
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Error != "server_error" {
			t.Errorf("error = %q", body.Error)
		}
		// The store's own message must not travel.
		if strings.Contains(string(resp.Body), "database is down") {
			t.Errorf("the response leaked the cause: %s", resp.Body)
		}
	})

	t.Run("polling", func(t *testing.T) {
		f.device.Store = faultyStore{Store: f.store,
			codes: failingDeviceCodes{DeviceCodeStore: f.store.DeviceCodes(), byHashErr: down}}
		defer func() { f.device.Store = f.store }()

		resp := f.postFormNoCookies("/device/token", url.Values{
			"grant_type": {DeviceGrantType}, "device_code": {"anything"},
		})
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("a failed lookup = %d, want 500", resp.StatusCode)
		}
		if strings.Contains(string(resp.Body), "database is down") {
			t.Errorf("the response leaked the cause: %s", resp.Body)
		}
	})
}

// TestApprovingReportsAStoreFailure rather than claiming success.
//
// @decision(DL-006)
func TestApprovingReportsAStoreFailure(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	f.device.Store = faultyStore{Store: f.store,
		codes: failingDeviceCodes{DeviceCodeStore: f.store.DeviceCodes(), updateErr: errors.New("the database is down")}}
	defer func() { f.device.Store = f.store }()

	form := url.Values{"code": {da.UserCode}, "confirm": {"1"}, "action": {"approve"}}
	resp := f.postForm(DeviceVerifyPath, form)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a failed approval = %d", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "could not be saved") {
		t.Errorf("the page does not say what happened: %s", resp.Body)
	}
	if strings.Contains(string(resp.Body), "database is down") {
		t.Errorf("the page leaked the cause: %s", resp.Body)
	}
}

// TestACLITokenCannotApproveADeviceFlow pins that the approval page
// does not run the whole authenticator chain, or a `spov_pat_` bearer
// resolves as the approving human.
//
// @decision(DL-006)
func TestACLITokenCannotApproveADeviceFlow(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	stolen := f.issueToken(t, "the stolen one")

	// The attacker holds only the token: no cookie anywhere.
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	approve := func(form url.Values) response {
		req, err := http.NewRequest(http.MethodPost, f.http.URL+DeviceVerifyPath, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+stolen)
		return f.sendNoCookies(req)
	}

	resp := approve(url.Values{"code": {da.UserCode}, "confirm": {"1"}, "action": {"approve"}})
	if resp.StatusCode == http.StatusOK {
		t.Errorf("a CLI token reached the approval page: %d", resp.StatusCode)
	}
	// Whatever the page answered, nothing may have been approved.
	if got := f.poll(da.DeviceCode); got != "authorization_pending" {
		t.Fatalf("a CLI token approved a device flow: poll = %q", got)
	}
	// And the page itself sends it to log in, since a token is not a
	// browser session.
	req, err := http.NewRequest(http.MethodGet, f.http.URL+DeviceVerifyPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+stolen)
	if got := f.sendNoCookies(req); got.StatusCode != http.StatusSeeOther {
		t.Errorf("GET with a CLI token = %d, want a redirect to the login", got.StatusCode)
	}
}

// TestTheSharedBearerCannotApproveADeviceFlow is the same hole through
// DL-006's shared bearer. A token minted for the non-existent `local`
// user would be unusable, but would let anyone holding the shared
// token write junk rows on demand.
//
// @decision(DL-006)
func TestTheSharedBearerCannotApproveADeviceFlow(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's chain has no shared bearer, so this stands in for
	// any non-cookie principal reaching the page.
	form := url.Values{"code": {da.UserCode}, "confirm": {"1"}, "action": {"approve"}}
	req, err := http.NewRequest(http.MethodPost, f.http.URL+DeviceVerifyPath, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := f.sendNoCookies(req); got.StatusCode == http.StatusOK {
		t.Errorf("an unauthenticated approval = %d, want a redirect", got.StatusCode)
	}
	if got := f.poll(da.DeviceCode); got != "authorization_pending" {
		t.Errorf("an unauthenticated request approved the flow: %q", got)
	}
}

// TestTheLockoutCannotBeResetByASelfIssuedCode pins that the counter
// clears only on a completed approval, never on a code merely
// resolving — or an attacker could mint their own code to reset it.
//
// @decision(DL-006)
func TestTheLockoutCannotBeResetByASelfIssuedCode(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")

	wrong := func() response {
		return f.postForm(DeviceVerifyPath, url.Values{"code": {"ZZZZ-ZZZZ"}})
	}
	for range MaxCodeAttempts - 1 {
		wrong()
	}
	// The attacker's own code, submitted to clear the counter.
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp := f.postForm(DeviceVerifyPath, url.Values{"code": {da.UserCode}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("resolving an own code = %d", resp.StatusCode)
	}
	// The count survived, so the next wrong guess is the fifth and
	// tips the lockout, which the guess after it reports.
	wrong()
	if body := string(wrong().Body); !strings.Contains(body, "Too many") {
		t.Errorf("the lockout was reset by a self-issued code: %s", body)
	}
}

// TestACompletedApprovalClearsTheAttemptCounter, so a person who
// mistyped a few times and then succeeded is not left near a lockout.
//
// @decision(DL-006)
func TestACompletedApprovalClearsTheAttemptCounter(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")
	for range MaxCodeAttempts - 1 {
		f.postForm(DeviceVerifyPath, url.Values{"code": {"ZZZZ-ZZZZ"}})
	}
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.approve(da.UserCode, "approve")

	// The counter is clear, so a fresh run of wrong guesses is needed
	// to lock out again.
	for i := range MaxCodeAttempts - 1 {
		if body := string(f.postForm(DeviceVerifyPath, url.Values{"code": {"ZZZZ-ZZZZ"}}).Body); strings.Contains(body, "Too many") {
			t.Fatalf("locked out after %d guesses following a success", i+1)
		}
	}
}

// TestExpiredDeviceCodesAreReaped: /device/code is unauthenticated, so
// without a sweep every abandoned flow leaks a row forever and each one
// keeps its user code reserved.
//
// @decision(DL-006)
func TestExpiredDeviceCodesAreReaped(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	for range 3 {
		if _, err := f.oauthConfig().DeviceAuth(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing is expired yet, so the sweep leaves them.
	f.device.sweepOnce(context.Background())
	da, err := f.oauthConfig().DeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.poll(da.DeviceCode); got != "authorization_pending" {
		t.Errorf("the sweep reaped a live code: %q", got)
	}

	// Past their lifetime they go.
	f.now = f.now.Add(DeviceCodeLifetime + time.Minute)
	f.device.sweepOnce(context.Background())
	if got := f.poll(da.DeviceCode); got != "invalid_grant" {
		t.Errorf("an expired code survived the sweep: %q, want invalid_grant", got)
	}
}

// TestBrowserHeadersOnEveryBrowserFacingPath checks every browser-facing
// path with the Sec-Fetch-* headers a real browser sends.
//
// @decision(DL-006) @decision(DL-007)
func TestBrowserHeadersOnEveryBrowserFacingPath(t *testing.T) {
	f := newDeviceFixture(t, "alice@example.com\n")
	f.doLogin("")

	// The headers a browser sends for each kind of request.
	sameOrigin := map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}
	navigation := map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}

	send := func(method, path string, headers map[string]string) int {
		req, err := http.NewRequest(method, f.http.URL+path, strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return f.send(req).StatusCode
	}

	for _, tc := range []struct {
		name, method, path string
		headers            map[string]string
		wantForbidden      bool
	}{
		// A person typing the URL or following a bookmark.
		{"the approval page, typed", http.MethodGet, DeviceVerifyPath, navigation, false},
		{"the login, typed", http.MethodGet, LoginPath, navigation, false},
		// Forms and fetches from spov's own pages.
		{"approving from our page", http.MethodPost, DeviceVerifyPath, sameOrigin, false},
		{"logging out from our page", http.MethodPost, LogoutPath, sameOrigin, false},
		{"the API from our page", http.MethodGet, "/api/v1/me", sameOrigin, false},
		// The OAuth provider navigating the browser back.
		{"the OAuth callback", http.MethodGet, CallbackPath, crossSite, false},
		// And what another site can cause. The approval POST is the one
		// that matters: a cross-site approval would be a zero-click
		// device-code phish.
		{"approving from another site", http.MethodPost, DeviceVerifyPath, crossSite, true},
		{"logging out from another site", http.MethodPost, LogoutPath, crossSite, true},
		{"the API from another site", http.MethodGet, "/api/v1/me", crossSite, true},
		{"the approval page from another site", http.MethodGet, DeviceVerifyPath, crossSite, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := send(tc.method, tc.path, tc.headers)
			if tc.wantForbidden && got != http.StatusForbidden {
				t.Errorf("%s %s = %d, want 403", tc.method, tc.path, got)
			}
			if !tc.wantForbidden && got == http.StatusForbidden {
				t.Errorf("%s %s = 403, which a real browser would hit", tc.method, tc.path)
			}
		})
	}
}

// reapingCodes is a device-code store whose reap is under the test's
// control.
type reapingCodes struct {
	store.DeviceCodeStore
	mu    sync.Mutex
	n     int
	err   error
	calls int
}

func (r *reapingCodes) DeleteExpired(context.Context, time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.n, r.err
}

func (r *reapingCodes) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type storeWithCodes struct {
	store.Store
	codes store.DeviceCodeStore
}

func (s storeWithCodes) DeviceCodes() store.DeviceCodeStore { return s.codes }

// The sweep reports what it reaped, and a failure is inert rather than
// fatal: the rows keep, and the next pass tries again.
//
// @decision(DL-006)
func TestTheDeviceSweepReportsWhatItReapedAndSurvivesFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		err  error
		want string
	}{
		{name: "reaped some", n: 3, want: "reaped 3"},
		{name: "reaped none", n: 0},
		{name: "the store refused", err: errors.New("db down"), want: "db down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemory()
			codes := &reapingCodes{DeviceCodeStore: st.DeviceCodes(), n: tc.n, err: tc.err}
			var said []string
			d := &Device{
				Store: storeWithCodes{Store: st, codes: codes},
				Now:   func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) },
				Warn:  func(s string) { said = append(said, s) },
			}
			d.sweepOnce(context.Background())
			if codes.count() != 1 {
				t.Fatalf("the store was asked to reap %d times, want 1", codes.count())
			}
			switch {
			case tc.want == "" && len(said) != 0:
				t.Errorf("a sweep with nothing to say said %v", said)
			case tc.want == "":
			case len(said) != 1 || !strings.Contains(said[0], tc.want):
				t.Errorf("warnings %v do not mention %q", said, tc.want)
			}
		})
	}
}

// A sweep that fails while shutting down says nothing: the context
// ending is the cause, and reporting it would turn every shutdown into
// a warning.
//
// @decision(DL-006)
func TestTheDeviceSweepIsQuietWhileShuttingDown(t *testing.T) {
	st := store.NewMemory()
	codes := &reapingCodes{DeviceCodeStore: st.DeviceCodes(), err: errors.New("db down")}
	var said []string
	d := &Device{
		Store: storeWithCodes{Store: st, codes: codes},
		Now:   time.Now,
		Warn:  func(s string) { said = append(said, s) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.sweepOnce(ctx)
	if len(said) != 0 {
		t.Errorf("a sweep cut short by shutdown warned: %v", said)
	}
}

// Sweep reaps once at start rather than waiting out its first interval,
// so a restart clears whatever accumulated while the process was down.
//
// @decision(DL-006)
func TestTheDeviceSweepReapsAtStartAndStopsWithItsContext(t *testing.T) {
	st := store.NewMemory()
	codes := &reapingCodes{DeviceCodeStore: st.DeviceCodes()}
	d := &Device{
		Store: storeWithCodes{Store: st, codes: codes},
		Now:   time.Now,
		Warn:  func(string) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Sweep(ctx); close(done) }()

	waitFor(t, "the first reap", func() bool { return codes.count() >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Sweep did not return when its context ended")
	}
	if n := codes.count(); n != 1 {
		t.Errorf("reaped %d times in a run shorter than one interval (%s), want 1", n, DeviceSweepInterval)
	}
}
