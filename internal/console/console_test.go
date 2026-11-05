package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func client(f *Fake) *Client {
	return &Client{BaseURL: f.URL, UserAgent: "spov/test"}
}

func TestADeviceSignInIsApprovedAndIssuesAGrant(t *testing.T) {
	f := NewFake()
	defer f.Close()
	c := client(f)
	ctx := context.Background()

	d, err := c.Start(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.UserCode == "" || d.Code == "" || d.Interval != 5*time.Second || !d.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("device = %+v", d)
	}
	if !strings.HasPrefix(d.VerificationURL, f.URL+"/console/device?user_code=") {
		t.Errorf("verification URL %q is not absolute on the console", d.VerificationURL)
	}
	res, err := c.Poll(ctx, d.Code, now)
	if err != nil || res.State != PollPending {
		t.Fatalf("before approval: %+v, %v", res, err)
	}
	f.Approve(d.UserCode)
	res, err = c.Poll(ctx, d.Code, now)
	if err != nil || res.State != PollApproved {
		t.Fatalf("after approval: %+v, %v", res, err)
	}
	g := res.Grant
	if g.Access == "" || g.Refresh == "" || g.OrgID != "org_1" || !g.ExpiresAt.Equal(now.Add(30*24*time.Hour)) {
		t.Errorf("grant = %+v", g)
	}
	for _, ua := range f.UserAgents {
		if ua != "spov/test" {
			t.Errorf("a console call identified as %q, not as spov", ua)
		}
	}
}

func TestADeniedOrRetriedPollSaysSo(t *testing.T) {
	f := NewFake()
	defer f.Close()
	c := client(f)
	ctx := context.Background()
	d, _ := c.Start(ctx, now)

	f.PollError = "server_error"
	if res, err := c.Poll(ctx, d.Code, now); err != nil || res.State != PollRetry || res.Reason != "server_error" {
		t.Errorf("a console server_error: %+v, %v; want a retry", res, err)
	}
	f.PollError = "slow_down"
	if res, _ := c.Poll(ctx, d.Code, now); res.State != PollRetry {
		t.Errorf("slow_down: %+v; want a retry", res)
	}
	f.Deny(d.UserCode)
	if res, _ := c.Poll(ctx, d.Code, now); res.State != PollFailed || res.Reason != "access_denied" {
		t.Errorf("denied: %+v", res)
	}
}

func TestARefreshRotatesAndAReusedRefreshTokenEndsTheGrant(t *testing.T) {
	f := NewFake()
	defer f.Close()
	c := client(f)
	ctx := context.Background()
	d, _ := c.Start(ctx, now)
	f.Approve(d.UserCode)
	res, _ := c.Poll(ctx, d.Code, now)
	first := res.Grant

	second, err := c.Refresh(ctx, first.Refresh, now)
	if err != nil {
		t.Fatal(err)
	}
	if second.Access == first.Access || second.Refresh == first.Refresh {
		t.Fatalf("the refresh did not rotate: %+v", second)
	}
	if f.Valid(first.Access) || !f.Valid(second.Access) {
		t.Fatal("a refresh must end the previous access token and issue a working one")
	}
	if _, err := c.Refresh(ctx, first.Refresh, now); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("reusing a replaced refresh token: %v, want ErrInvalidGrant", err)
	}
	if f.Valid(second.Access) {
		t.Error("the reuse should have revoked the whole grant")
	}
	if _, err := c.Refresh(ctx, second.Refresh, now); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("the newest refresh token after a reuse: %v, want ErrInvalidGrant", err)
	}
}

func TestAVerificationURLOffTheConsoleIsRefused(t *testing.T) {
	var link string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code": "d", "user_code": "u", "verification_uri_complete": link,
		})
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	for _, bad := range []string{"https://evil.example/device", strings.Replace(srv.URL, "http://", "https://", 1) + "/device"} {
		link = bad
		if _, err := c.Start(context.Background(), now); err == nil {
			t.Errorf("verification link %q was accepted", bad)
		}
	}
	link = "/device?user_code=u"
	d, err := c.Start(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !d.ExpiresAt.Equal(now.Add(10*time.Minute)) || d.Interval != 5*time.Second {
		t.Errorf("an answer without expiry or interval gave %s and %s", d.ExpiresAt, d.Interval)
	}
}

func TestAnErrorNeverQuotesTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"access_token": "st_secret", "refresh_token": "rt_secret"})
	}))
	defer srv.Close()
	_, err := (&Client{BaseURL: srv.URL}).Refresh(context.Background(), "rt_x", now)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v; it must fail without quoting the body", err)
	}
}

// An error text carries the console's code and description and nothing
// else of the body, which could hold a token.
func TestAnErrorTextNeverQuotesTheRestOfTheBody(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"error":"invalid_grant","error_description":"expired","access_token":"sk-secret"}`, "invalid_grant: expired"},
		{`{"error":"slow_down"}`, "slow_down"},
		{`{"access_token":"sk-secret"}`, "no error code"},
		{`<html>sk-secret</html>`, "no error code"},
	} {
		got := errorText([]byte(tc.body))
		if got != tc.want || strings.Contains(got, "sk-secret") {
			t.Errorf("errorText(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}
