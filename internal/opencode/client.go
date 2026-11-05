package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Health is the server's reported identity, read from /api/info. The
// server answering with a version is what makes it healthy; the
// endpoint has no health field of its own.
//
// @decision(DL-025) @decision(DL-035)
type Health struct {
	Healthy bool   `json:"-"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// InfoPath is the server's identity endpoint, which readiness, the
// version handshake and the proxy's heartbeat all read.
//
// @decision(DL-035)
const InfoPath = "/api/info"

// Client talks to one OpenCode server over its tunnel.
//
// @decision(DL-025)
type Client struct {
	BaseURL  string
	Password string
	HTTP     *http.Client
}

// NewClient returns a client for a server reachable at baseURL.
func NewClient(baseURL, password string) *Client {
	return &Client{
		BaseURL:  baseURL,
		Password: password,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
}

// ErrUnauthorized means the password was rejected.
var ErrUnauthorized = errors.New("opencode: unauthorized")

// Health fetches the server's identity.
//
// @decision(DL-025) @decision(DL-035)
func (c *Client) Health(ctx context.Context) (Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+InfoPath, nil)
	if err != nil {
		return Health{}, fmt.Errorf("opencode: health request: %w", err)
	}
	// An empty password means the caller reaches the server through the
	// proxy, which supplies the credential.
	//
	// @decision(DL-030)
	if c.Password != "" {
		req.SetBasicAuth(BasicAuthUser, c.Password)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Health{}, fmt.Errorf("opencode: health request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return Health{}, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("opencode: health returned %s", resp.Status)
	}

	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return Health{}, fmt.Errorf("opencode: decode health: %w", err)
	}
	h.Healthy = h.Version != ""
	return h, nil
}

// WaitHealthy polls the health endpoint until the server reports healthy
// or ctx is done.
//
// @decision(DL-025)
func (c *Client) WaitHealthy(ctx context.Context, interval time.Duration) (Health, error) {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	for {
		h, err := c.Health(ctx)
		switch {
		case err == nil && h.Healthy:
			return h, nil
		case errors.Is(err, ErrUnauthorized):
			// Retrying cannot fix a wrong password.
			return Health{}, err
		case err != nil:
			lastErr = err
		default:
			lastErr = fmt.Errorf("opencode: server reported unhealthy")
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return Health{}, fmt.Errorf("opencode: server did not become healthy (%w); last error: %w", ctx.Err(), lastErr)
			}
			return Health{}, fmt.Errorf("opencode: server did not become healthy: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// VersionMismatchError reports that a sandbox is not running the build it
// was expected to. The local client's own mismatch is
// ClientVersionMismatchError, a separate type since the two are
// different sentences about different machines.
//
// @decision(DL-035)
type VersionMismatchError struct {
	// Expected is the version the sandbox was supposed to be running.
	// Source names where that number came from: the server's own pin, a
	// config file, or spov's built-in default.
	//
	// @decision(DL-035)
	Expected string
	Source   string
	// Reported is what the sandbox itself answered.
	Reported string
}

func (e VersionMismatchError) Error() string {
	source := e.Source
	if source == "" {
		source = "the configured version"
	}
	return fmt.Sprintf("opencode: version mismatch: %s is %s, and the sandbox is running %s",
		source, e.Expected, e.Reported)
}

// CheckVersion compares what a sandbox reports against the version it
// was expected to run, naming where that expectation came from.
//
// @decision(DL-035)
func CheckVersion(expected, source, reported string) error {
	if expected == "" || reported == "" {
		return fmt.Errorf("opencode: cannot compare versions (%s %q, sandbox %q)",
			sourceOr(source), expected, reported)
	}
	if expected != reported {
		return VersionMismatchError{Expected: expected, Source: source, Reported: reported}
	}
	return nil
}

func sourceOr(source string) string {
	if source == "" {
		return "expected"
	}
	return source
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}
