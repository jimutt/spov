// Package console is spov's client for the OpenCode Console's OAuth
// device flow: starting a sign-in, polling for the user's approval, and
// refreshing a grant. It holds no state; the server stores the grant.
//
// @decision(DL-015)
package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultURL is the console's base URL, which every endpoint is under.
	DefaultURL = "https://opencode.ai/console"
	// ClientID is the OAuth client spov signs in as. It is OpenCode's
	// own CLI client, until the OpenCode team registers one for spov.
	//
	// @decision(DL-015)
	ClientID = "opencode-cli"

	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// maxBody bounds what is read from a console response.
	maxBody = 1 << 20
)

// ErrInvalidGrant means the console refused a refresh token. The grant
// is dead: a reused refresh token revokes it, and so does the console.
//
// @decision(DL-015)
var ErrInvalidGrant = errors.New("console: the grant is no longer valid")

// Client calls the console. The zero value is not usable; set UserAgent.
type Client struct {
	// BaseURL defaults to DefaultURL.
	BaseURL string
	// UserAgent identifies spov on every call it makes itself.
	//
	// @decision(DL-015)
	UserAgent string
	// HTTP defaults to a client with a 30 s timeout.
	HTTP *http.Client
}

// Device is a started sign-in: what the user must open and enter.
type Device struct {
	// Code is the device code the server polls with. It is a secret
	// until the attempt ends.
	Code            string
	UserCode        string
	VerificationURL string
	ExpiresAt       time.Time
	Interval        time.Duration
}

// Grant is a signed-in console account.
type Grant struct {
	Access    string
	Refresh   string
	ExpiresAt time.Time
	// OrgID is the organization the grant is scoped to, when the console
	// says.
	OrgID string
}

// PollState is where a sign-in stands after one poll.
type PollState string

const (
	// PollPending means the user has not approved yet.
	PollPending PollState = "pending"
	// PollApproved means the grant is issued.
	PollApproved PollState = "approved"
	// PollRetry means the console asked for a slower poll or answered a
	// transient error; the attempt is still open.
	PollRetry PollState = "retry"
	// PollFailed means the attempt is over: denied, expired or refused.
	PollFailed PollState = "failed"
)

// PollResult is one poll's answer.
type PollResult struct {
	State PollState
	Grant Grant
	// Reason is the console's error code for a retry or a failure.
	Reason string
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultURL
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Start begins a device sign-in.
//
// @decision(DL-015)
func (c *Client) Start(ctx context.Context, now time.Time) (Device, error) {
	var out struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	status, body, err := c.post(ctx, "/auth/device/code", map[string]any{
		"client_id": ClientID, "supports_org_scope": true,
	})
	if err != nil {
		return Device{}, err
	}
	if status != http.StatusOK {
		return Device{}, fmt.Errorf("console: starting a sign-in answered %d: %s", status, errorText(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Device{}, fmt.Errorf("console: starting a sign-in: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURIComplete == "" {
		return Device{}, errors.New("console: the sign-in answer is missing a code or a URL")
	}
	verify, err := c.resolve(out.VerificationURIComplete)
	if err != nil {
		return Device{}, err
	}
	interval := time.Duration(out.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 600
	}
	return Device{
		Code:            out.DeviceCode,
		UserCode:        out.UserCode,
		VerificationURL: verify,
		ExpiresAt:       now.Add(time.Duration(out.ExpiresIn) * time.Second),
		Interval:        interval,
	}, nil
}

// resolve turns the console's verification link, which may be relative
// to the console's origin, into an absolute https URL on that origin.
func (c *Client) resolve(ref string) (string, error) {
	base, err := url.Parse(c.base() + "/")
	if err != nil {
		return "", err
	}
	u, err := base.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("console: bad verification URL: %w", err)
	}
	if u.Host != base.Host || u.Scheme != base.Scheme {
		return "", fmt.Errorf("console: the verification URL %q is not on %s", u.String(), base.Host)
	}
	return u.String(), nil
}

// Poll asks once whether the user has approved a sign-in.
//
// @decision(DL-015)
func (c *Client) Poll(ctx context.Context, deviceCode string, now time.Time) (PollResult, error) {
	status, body, err := c.post(ctx, "/auth/device/token", map[string]any{
		"grant_type": deviceGrantType, "device_code": deviceCode, "client_id": ClientID,
	})
	if err != nil {
		return PollResult{}, err
	}
	if status == http.StatusOK {
		g, err := grantFrom(body, now)
		if err != nil {
			return PollResult{}, err
		}
		return PollResult{State: PollApproved, Grant: g}, nil
	}
	code := errorCode(body)
	switch code {
	case "authorization_pending":
		return PollResult{State: PollPending}, nil
	case "slow_down", "server_error", "temporarily_unavailable":
		return PollResult{State: PollRetry, Reason: code}, nil
	case "":
		if status >= 500 {
			return PollResult{State: PollRetry, Reason: fmt.Sprintf("status %d", status)}, nil
		}
		return PollResult{State: PollFailed, Reason: fmt.Sprintf("status %d", status)}, nil
	}
	return PollResult{State: PollFailed, Reason: code}, nil
}

// Refresh exchanges a refresh token for a new grant. The previous access
// token stops working, and the previous refresh token must never be sent
// again.
//
// @decision(DL-015)
func (c *Client) Refresh(ctx context.Context, refresh string, now time.Time) (Grant, error) {
	status, body, err := c.post(ctx, "/auth/device/token", map[string]any{
		"grant_type": "refresh_token", "refresh_token": refresh, "client_id": ClientID,
	})
	if err != nil {
		return Grant{}, err
	}
	if status == http.StatusOK {
		return grantFrom(body, now)
	}
	if errorCode(body) == "invalid_grant" {
		return Grant{}, ErrInvalidGrant
	}
	return Grant{}, fmt.Errorf("console: refresh answered %d: %s", status, errorText(body))
}

func grantFrom(body []byte, now time.Time) (Grant, error) {
	var out struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    int64   `json:"expires_in"`
		OrgID        *string `json:"org_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Grant{}, fmt.Errorf("console: reading a grant: %w", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.ExpiresIn <= 0 {
		return Grant{}, errors.New("console: the grant is missing a token or an expiry")
	}
	g := Grant{
		Access:    out.AccessToken,
		Refresh:   out.RefreshToken,
		ExpiresAt: now.Add(time.Duration(out.ExpiresIn) * time.Second),
	}
	if out.OrgID != nil {
		g.OrgID = *out.OrgID
	}
	return g, nil
}

func (c *Client) post(ctx context.Context, path string, body map[string]any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.http().Do(req)
	if err != nil {
		// The URL carries no secret; the body is not in the error.
		return 0, nil, fmt.Errorf("console: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, fmt.Errorf("console: reading %s: %w", path, err)
	}
	return resp.StatusCode, data, nil
}

// errorCode is the OAuth error code in a console answer, if any.
func errorCode(body []byte) string {
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Error
}

// errorText is the console's error code and description, never the rest
// of the body, which could carry a token.
func errorText(body []byte) string {
	var out struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &out) != nil || out.Error == "" {
		return "no error code"
	}
	if out.Description == "" {
		return out.Error
	}
	return out.Error + ": " + out.Description
}
