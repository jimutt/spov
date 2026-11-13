package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// Device is spov's own RFC 8628 authorization server: the CLI asks it
// for a code, a human approves that code in a browser, and the CLI
// exchanges it for a spov token. DL-007's GitHub login is how the human
// proves who they are on the approval page.
//
// @decision(DL-006)
type Device struct {
	Store store.Store
	// PublicURL is the origin the verification URI is built from; it is
	// what the CLI prints and what the human types into a browser.
	PublicURL string
	// Sessions resolves the approval page's logged-in user. Nil means
	// no login is configured, and the approval page is unreachable.
	Login *Login
	// TokenLifetime is how long an issued token lasts. Zero means
	// DefaultTokenLifetime.
	TokenLifetime time.Duration
	// Interval is the polling floor in seconds. Zero means
	// DeviceInterval; a test lowers it so it does not wait.
	//
	// @decision(DL-006)
	Interval int
	// Now supplies timestamps; nil means time.Now.
	Now func() time.Time
	// Warn receives one line per problem; nil discards.
	Warn func(string)
	// attempts rate-limits wrong user codes on the approval page.
	attempts    *attemptLimiter
	limiterOnce sync.Once
}

// Device flow paths and parameters. They sit outside APIPrefix: a client
// polling for a token has no credential yet, by construction.
//
// @decision(DL-006)
const (
	DeviceCodePath = "/device/code"
	// DeviceVerifyPath is the approval page a human visits.
	DeviceVerifyPath = "/device"
	DeviceGrantType  = "urn:ietf:params:oauth:grant-type:device_code"
	// DeviceClientID is the only client there is.
	DeviceClientID = "spov-cli"
)

// deviceTokenPath is where the CLI polls. It is distinct from the
// approval page, which is a GET on /device.
const deviceTokenPath = "/device/token"

const (
	// DeviceCodeLifetime is how long a human has to approve.
	//
	// @decision(DL-006)
	DeviceCodeLifetime = 10 * time.Minute
	// DeviceInterval is the polling floor, in seconds.
	//
	// @decision(DL-006)
	DeviceInterval = 5
	// DeviceSlowDown is added to a code's interval each time it is
	// polled too fast.
	//
	// @decision(DL-006)
	DeviceSlowDown = 5
	// DefaultTokenLifetime is how long an issued CLI token lasts.
	//
	// @decision(DL-006)
	DefaultTokenLifetime = 90 * 24 * time.Hour
	// MaxTokenName bounds the name a CLI sends, which is shown on the
	// approval page.
	MaxTokenName = 120
)

// register mounts the device flow on mux.
//
// @decision(DL-006)
func (d *Device) register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+DeviceCodePath, d.handleCode)
	mux.HandleFunc("POST "+deviceTokenPath, d.handleToken)
	mux.HandleFunc("GET "+DeviceVerifyPath, d.handleVerifyPage)
	mux.HandleFunc("POST "+DeviceVerifyPath, d.handleApprove)
}

func (d *Device) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func (d *Device) warn(msg string) {
	if d.Warn != nil {
		d.Warn(msg)
	}
}

// interval is the polling floor this server advertises.
func (d *Device) interval() int {
	if d.Interval > 0 {
		return d.Interval
	}
	return DeviceInterval
}

func (d *Device) tokenLifetime() time.Duration {
	if d.TokenLifetime > 0 {
		return d.TokenLifetime
	}
	return DefaultTokenLifetime
}

// handleCode starts a device flow: it mints the pair of codes and tells
// the client where to send its human.
//
// @decision(DL-006)
func (d *Device) handleCode(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the request body could not be parsed")
		return
	}
	if id := r.Form.Get("client_id"); id != "" && id != DeviceClientID {
		oauthError(w, http.StatusBadRequest, "invalid_client", "unknown client")
		return
	}
	name := deviceTokenName(r.Form.Get("name"))

	code, hash, err := auth.NewDeviceCode()
	if err != nil {
		d.warn("device: " + err.Error())
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	now := d.now()
	// The user code must be unique among the codes a human could still
	// be typing, or the approval page could not resolve it. A retry is
	// cheaper than a transaction over a 2.6e10 space.
	var userCode string
	for attempt := 0; ; attempt++ {
		if userCode, err = auth.NewUserCode(); err != nil {
			d.warn("device: " + err.Error())
			oauthError(w, http.StatusInternalServerError, "server_error", "")
			return
		}
		if _, err := d.Store.DeviceCodes().ByUserCode(r.Context(), userCode); errors.Is(err, store.ErrNotFound) {
			break
		} else if err != nil {
			d.warn("device: checking a user code: " + err.Error())
			oauthError(w, http.StatusInternalServerError, "server_error", "")
			return
		}
		if attempt >= 5 {
			d.warn("device: could not find a free user code")
			oauthError(w, http.StatusInternalServerError, "server_error", "")
			return
		}
	}

	dc := store.DeviceCode{
		ID:              store.NewID(now),
		OrgID:           DefaultOrgID,
		Hash:            hash,
		UserCode:        userCode,
		Status:          store.DevicePending,
		Name:            name,
		ExpiresAt:       now.Add(DeviceCodeLifetime),
		IntervalSeconds: d.interval(),
		CreatedAt:       now,
	}
	if err := d.Store.DeviceCodes().Create(r.Context(), dc); err != nil {
		d.warn("device: creating a device code: " + err.Error())
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}

	verify := d.verificationURI()
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               code,
		"user_code":                 userCode,
		"verification_uri":          verify,
		"verification_uri_complete": verify + "?code=" + url.QueryEscape(userCode),
		"expires_in":                int(DeviceCodeLifetime / time.Second),
		"interval":                  d.interval(),
	})
}

// verificationURI is where the human goes. It is built from the public
// origin, since the CLI prints it for someone to type elsewhere.
func (d *Device) verificationURI() string {
	return strings.TrimSuffix(d.PublicURL, "/") + DeviceVerifyPath
}

// deviceTokenName bounds and cleans the name a CLI proposes for its
// token. It is shown on the approval page and stored, so it is trimmed
// of control characters rather than trusted.
//
// @decision(DL-006)
func deviceTokenName(raw string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(raw))
	if len(clean) > MaxTokenName {
		clean = clean[:MaxTokenName]
	}
	if clean == "" {
		return "spov CLI"
	}
	return clean
}

// handleToken is the CLI's poll. Its vocabulary is RFC 8628 §3.5, and
// x/oauth2's client depends on the exact error codes.
//
// @decision(DL-006)
func (d *Device) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the request body could not be parsed")
		return
	}
	if got := r.Form.Get("grant_type"); got != DeviceGrantType {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "")
		return
	}
	presented := r.Form.Get("device_code")
	if presented == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "device_code is required")
		return
	}

	ctx := r.Context()
	dc, err := d.Store.DeviceCodes().ByHash(ctx, auth.HashDeviceCode(presented))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Unknown, or already exchanged: a device code buys one
			// token, and the row goes when it does.
			oauthError(w, http.StatusBadRequest, "invalid_grant", "")
			return
		}
		d.warn("device: looking up a device code: " + err.Error())
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	now := d.now()
	if now.After(dc.ExpiresAt) {
		oauthError(w, http.StatusBadRequest, "expired_token", "")
		return
	}
	// Polling faster than the interval raises it, per §3.5. The client
	// honours the raise, so a badly behaved one throttles itself.
	if dc.LastPolledAt != nil && now.Sub(*dc.LastPolledAt) < time.Duration(dc.IntervalSeconds)*time.Second {
		dc.IntervalSeconds += DeviceSlowDown
		dc.LastPolledAt = &now
		if err := d.Store.DeviceCodes().Update(ctx, dc); err != nil {
			d.warn("device: recording a poll: " + err.Error())
		}
		oauthError(w, http.StatusBadRequest, "slow_down", "")
		return
	}
	dc.LastPolledAt = &now

	switch dc.Status {
	case store.DeviceDenied:
		// The row goes: there is nothing left to poll for.
		if err := d.Store.DeviceCodes().Delete(ctx, dc.ID); err != nil {
			d.warn("device: deleting a denied code: " + err.Error())
		}
		oauthError(w, http.StatusBadRequest, "access_denied", "")
		return
	case store.DeviceApproved:
	default:
		if err := d.Store.DeviceCodes().Update(ctx, dc); err != nil {
			d.warn("device: recording a poll: " + err.Error())
		}
		oauthError(w, http.StatusBadRequest, "authorization_pending", "")
		return
	}

	token, err := d.issue(ctx, dc, now)
	// @decision(DL-141)
	if errors.Is(err, errNotAPerson) {
		d.warn("device: refused a token for a user who is not a person")
		if err := d.Store.DeviceCodes().Delete(ctx, dc.ID); err != nil {
			d.warn("device: deleting a refused code: " + err.Error())
		}
		oauthError(w, http.StatusBadRequest, "access_denied", "")
		return
	}
	if err != nil {
		d.warn("device: issuing a token: " + err.Error())
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "bearer",
		"expires_in":   int(d.tokenLifetime() / time.Second),
	})
}

// issue mints the token and consumes the device code. The delete is what
// makes a device code single-use: a second poll finds nothing and gets
// invalid_grant.
//
// @decision(DL-006)
func (d *Device) issue(ctx context.Context, dc store.DeviceCode, now time.Time) (string, error) {
	token, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	expires := now.Add(d.tokenLifetime())
	rec := store.APIToken{
		ID:        store.NewID(now),
		OrgID:     dc.OrgID,
		UserID:    dc.UserID,
		Hash:      auth.HashToken(token),
		Name:      dc.Name,
		ExpiresAt: &expires,
		CreatedAt: now,
	}
	// The token row and the code's removal go together: a token that
	// exists with a code that also does could be issued twice. Only a
	// person's row gets one; a code approved for any other is refused
	// and then deleted.
	//
	// @decision(DL-141)
	err = d.Store.Tx(ctx, func(s store.Store) error {
		u, err := s.Users().Get(ctx, dc.OrgID, dc.UserID)
		if err != nil {
			return err
		}
		if !u.Person() {
			return errNotAPerson
		}
		if err := s.Tokens().Create(ctx, rec); err != nil {
			return err
		}
		return s.DeviceCodes().Delete(ctx, dc.ID)
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// errNotAPerson is a device code approved for a row that is not a
// person's.
var errNotAPerson = errors.New("the device code's user is not a person")

// oauthError writes an RFC 6749 §5.2 error body. x/oauth2 parses these,
// and the device-flow client branches on the code.
//
// @decision(DL-006)
func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := map[string]string{"error": code}
	if description != "" {
		body["error_description"] = description
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// isDevicePath reports whether a path belongs to the device flow's
// unauthenticated half: the CLI has no credential when it calls these.
// The approval page is not among them — it requires a login.
//
// @decision(DL-006)
func isDevicePath(p string) bool {
	switch p {
	case DeviceCodePath, deviceTokenPath:
		return true
	}
	return false
}

// DeviceSweepInterval is how often expired device codes are reaped.
//
// @decision(DL-006)
const DeviceSweepInterval = 10 * time.Minute

// Sweep deletes device codes past their expiry until ctx ends.
//
// @decision(DL-006)
func (d *Device) Sweep(ctx context.Context) {
	tick := time.NewTicker(DeviceSweepInterval)
	defer tick.Stop()
	for {
		// Once at start, so a restart clears whatever accumulated while
		// the process was down.
		d.sweepOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// sweepOnce reaps in one pass.
//
// @decision(DL-006)
func (d *Device) sweepOnce(ctx context.Context) {
	n, err := d.Store.DeviceCodes().DeleteExpired(ctx, d.now())
	if err != nil {
		// A failed sweep is not worth failing anything over: the rows
		// are inert, and the next pass tries again.
		if ctx.Err() == nil {
			d.warn("device: reaping expired codes: " + err.Error())
		}
		return
	}
	if n > 0 {
		d.warn("device: reaped " + strconv.Itoa(n) + " expired device code(s)")
	}
}
