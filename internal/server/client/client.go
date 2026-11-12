// Package client is the Go client for the spov HTTP API. It is written by
// hand against the server's own request and response types, so the two
// cannot drift apart without a compile error.
//
// @decision(DL-002) @decision(DL-005)
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/server"
)

// Client talks to one spov server as one principal.
//
// @decision(DL-002)
type Client struct {
	// BaseURL is the server's address, without the /api/v1 prefix.
	BaseURL string
	// Token is the bearer credential. It is sent and never printed.
	Token string
	// HTTP is the transport; nil means a client with a request timeout
	// that streams disable for themselves.
	HTTP *http.Client
	// UserAgent identifies the client build to the server.
	UserAgent string
}

// New builds a client for baseURL authenticating with token.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token}
}

// DefaultTimeout bounds a plain request. Streams are unbounded.
const DefaultTimeout = 30 * time.Second

// Error is a problem response from the server (RFC 9457).
//
// @decision(DL-005)
type Error struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Type   string `json:"type"`
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	if e.Title != "" {
		return e.Title
	}
	return http.StatusText(e.Status)
}

// StatusOf returns the HTTP status behind err, or zero when err is not a
// server problem.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// IsNotFound reports whether the server answered 404.
func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// IsSessionStopped reports whether the server refused because the
// session is no longer running.
func IsSessionStopped(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Type == server.ProblemTypeSessionStopped
}

// ErrNoServer means the client has no address to talk to.
var ErrNoServer = errors.New("client: no server URL")

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: DefaultTimeout}
}

// newRequest builds an authenticated request for an API path.
func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	if c.BaseURL == "" {
		return nil, ErrNoServer
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+server.APIPrefix+strings.TrimPrefix(path, "/"), rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	return req, nil
}

// do sends a request and decodes a JSON body into out when out is not
// nil. Any status outside 2xx is returned as an *Error.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, redactURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return problemOf(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("client: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// problemOf turns a failed response into an Error, whatever the server
// managed to send.
func problemOf(resp *http.Response) error {
	e := &Error{Status: resp.StatusCode}
	// Bounded: the body of an error response is one problem document,
	// not something a misbehaving server should be able to inflate.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(raw, e) != nil || (e.Title == "" && e.Detail == "") {
		e.Title = http.StatusText(resp.StatusCode)
		if text := strings.TrimSpace(string(raw)); text != "" && !strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "<") {
			e.Detail = text
		}
	}
	e.Status = resp.StatusCode
	return e
}

// redactURL strips credentials a transport error might echo from a URL.
// The client never puts a credential in a URL, so this is belt and
// braces against a server address configured with one.
func redactURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if u, perr := url.Parse(uerr.URL); perr == nil && u.User != nil {
			u.User = url.UserPassword("xxx", "xxx")
			uerr.URL = u.Redacted()
		}
		return uerr
	}
	return err
}

// Me returns the caller's principal, which is also the cheapest probe of
// whether the server is up and the token is accepted.
func (c *Client) Me(ctx context.Context) (server.MeView, error) {
	var out server.MeView
	err := c.do(ctx, http.MethodGet, "me", nil, &out)
	return out, err
}

// CreateSession asks for a session and returns its ID. The launch runs on
// the server; Follow reports its progress.
func (c *Client) CreateSession(ctx context.Context, req server.SessionRequest) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "sessions", req, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// GetSession resolves a session by ID or name.
func (c *Client) GetSession(ctx context.Context, ref string) (server.SessionView, error) {
	var out server.SessionView
	err := c.do(ctx, http.MethodGet, "sessions/"+url.PathEscape(ref), nil, &out)
	return out, err
}

// Page names one page of a listing: the states to keep, how many, and
// the cursor of the page before it. Zero is the newest page of the
// server's default size.
//
// @decision(DL-012)
type Page struct {
	States []string
	Limit  int
	Before string
}

// query is the page as the listing's parameters. The states are one
// comma-separated parameter, not one per state: the server keeps only
// the first of a repeated parameter and drops the rest silently, so the
// repeated form filters on one state and looks like an empty result.
//
// @decision(DL-045) @decision(DL-012)
func (p Page) query() string {
	q := url.Values{}
	if len(p.States) > 0 {
		q.Set("state", strings.Join(p.States, ","))
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Before != "" {
		q.Set("before", p.Before)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// SessionPage is one page of the session listing, as the API sends it.
//
// @decision(DL-012)
type SessionPage struct {
	Sessions []server.SessionView `json:"sessions"`
	Next     string               `json:"next,omitempty"`
	Total    int                  `json:"total"`
}

// ListSessions reads one page of the sessions the caller may see,
// newest first.
//
// @decision(DL-012)
func (c *Client) ListSessions(ctx context.Context, p Page) (SessionPage, error) {
	var out SessionPage
	err := c.do(ctx, http.MethodGet, "sessions"+p.query(), nil, &out)
	return out, err
}

// Conversations lists the session's recorded conversations. The IDs are
// OpenCode's own (`ses_…`), which is what the client's --session flag
// takes.
//
// It is a store read: the recorder is the only thing that reads the
// sandbox, so this costs the sandbox's idle clock nothing.
//
// @decision(DL-025) @decision(DL-018)
func (c *Client) Conversations(ctx context.Context, ref string) ([]server.ConversationView, error) {
	var out struct {
		Conversations []server.ConversationView `json:"conversations"`
	}
	err := c.do(ctx, http.MethodGet, "sessions/"+url.PathEscape(ref)+"/messages", nil, &out)
	return out.Conversations, err
}

// Resume reopens a stopped session in a new sandbox.
//
// @decision(DL-087)
func (c *Client) Resume(ctx context.Context, ref string, req server.ResumeRequest) (server.SessionView, error) {
	var out server.SessionView
	err := c.do(ctx, http.MethodPost, "sessions/"+url.PathEscape(ref)+"/resume", req, &out)
	return out, err
}

// DeleteSession terminates a session's sandbox. A session that is already
// stopped is not an error.
func (c *Client) DeleteSession(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "sessions/"+url.PathEscape(ref), nil, nil)
}

// ListTokens lists the caller's CLI tokens.
//
// @decision(DL-006)
func (c *Client) ListTokens(ctx context.Context) ([]server.TokenView, error) {
	var out struct {
		Tokens []server.TokenView `json:"tokens"`
	}
	if err := c.do(ctx, http.MethodGet, "me/tokens", nil, &out); err != nil {
		return nil, err
	}
	return out.Tokens, nil
}

// RevokeToken revokes one of the caller's CLI tokens.
//
// @decision(DL-006)
func (c *Client) RevokeToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "me/tokens/"+url.PathEscape(id), nil, nil)
}

// RevokeCurrentToken revokes the token this client authenticates with,
// which is what `spov server-logout` does.
//
// @decision(DL-006)
func (c *Client) RevokeCurrentToken(ctx context.Context) error {
	return c.RevokeToken(ctx, server.CurrentToken)
}

// ListCredentials reports which platform credentials the caller has
// stored. No credential is returned; there is no endpoint that returns
// one.
//
// @decision(DL-008)
func (c *Client) ListCredentials(ctx context.Context) ([]server.CredentialView, bool, error) {
	var out struct {
		Credentials []server.CredentialView `json:"credentials"`
		Enabled     bool                    `json:"enabled"`
	}
	if err := c.do(ctx, http.MethodGet, "me/credentials", nil, &out); err != nil {
		return nil, false, err
	}
	return out.Credentials, out.Enabled, nil
}

// PutCredential stores the caller's credential for one platform. The
// server verifies it against the platform before storing it.
//
// @decision(DL-008)
func (c *Client) PutCredential(ctx context.Context, platform, token string) (server.CredentialView, error) {
	body := struct {
		Token string `json:"token"`
	}{Token: token}
	var out server.CredentialView
	err := c.do(ctx, http.MethodPut, "me/credentials/"+url.PathEscape(platform), body, &out)
	return out, err
}

// DeleteCredential removes the caller's credential for one platform.
//
// @decision(DL-008)
func (c *Client) DeleteCredential(ctx context.Context, platform string) error {
	return c.do(ctx, http.MethodDelete, "me/credentials/"+url.PathEscape(platform), nil, nil)
}

// ListRoutedCredentials lists the caller's routed credentials, never
// their secrets.
//
// @decision(DL-073)
func (c *Client) ListRoutedCredentials(ctx context.Context) ([]server.RoutedCredentialView, error) {
	var out struct {
		RoutedCredentials []server.RoutedCredentialView `json:"routed_credentials"`
	}
	if err := c.do(ctx, http.MethodGet, "me/routed-credentials", nil, &out); err != nil {
		return nil, err
	}
	return out.RoutedCredentials, nil
}

// PutRoutedCredential stores a routed credential, replacing its route
// and secret together.
//
// @decision(DL-073)
func (c *Client) PutRoutedCredential(ctx context.Context, r routedcred.Route, secret string) (server.RoutedCredentialView, error) {
	body := struct {
		Secret string           `json:"secret"`
		Route  routedcred.Route `json:"route"`
	}{Secret: secret, Route: r}
	var out server.RoutedCredentialView
	err := c.do(ctx, http.MethodPut, "me/routed-credentials/"+url.PathEscape(r.Name), body, &out)
	return out, err
}

// DeleteRoutedCredential removes one of the caller's routed credentials.
//
// @decision(DL-073)
func (c *Client) DeleteRoutedCredential(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "me/routed-credentials/"+url.PathEscape(name), nil, nil)
}

// ListTemplates lists the caller's sandbox templates.
//
// @decision(DL-119)
func (c *Client) ListTemplates(ctx context.Context) ([]server.TemplateView, error) {
	var out struct {
		Templates []server.TemplateView `json:"templates"`
	}
	if err := c.do(ctx, http.MethodGet, "me/templates", nil, &out); err != nil {
		return nil, err
	}
	return out.Templates, nil
}

// GetTemplate reads one of the caller's sandbox templates.
//
// @decision(DL-119)
func (c *Client) GetTemplate(ctx context.Context, name string) (server.TemplateView, error) {
	var out server.TemplateView
	err := c.do(ctx, http.MethodGet, "me/templates/"+url.PathEscape(name), nil, &out)
	return out, err
}

// DeleteTemplate removes one of the caller's sandbox templates.
//
// @decision(DL-119)
func (c *Client) DeleteTemplate(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "me/templates/"+url.PathEscape(name), nil, nil)
}

// ListOrgRoutedCredentials lists the organisation's routed credentials,
// never their secrets.
//
// @decision(DL-074)
func (c *Client) ListOrgRoutedCredentials(ctx context.Context) ([]server.RoutedCredentialView, error) {
	var out struct {
		RoutedCredentials []server.RoutedCredentialView `json:"routed_credentials"`
	}
	if err := c.do(ctx, http.MethodGet, "org/routed-credentials", nil, &out); err != nil {
		return nil, err
	}
	return out.RoutedCredentials, nil
}

// PutOrgRoutedCredential stores one of the organisation's routed
// credentials; only an admin may.
//
// @decision(DL-074)
func (c *Client) PutOrgRoutedCredential(ctx context.Context, r routedcred.Route, secret string) error {
	body := struct {
		Secret string           `json:"secret"`
		Route  routedcred.Route `json:"route"`
	}{Secret: secret, Route: r}
	return c.do(ctx, http.MethodPut, "org/routed-credentials/"+url.PathEscape(r.Name), body, nil)
}

// DeleteOrgRoutedCredential removes one; only an admin may.
//
// @decision(DL-074)
func (c *Client) DeleteOrgRoutedCredential(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "org/routed-credentials/"+url.PathEscape(name), nil, nil)
}

// OpenCodeConnection reports the caller's OpenCode Console sign-in.
//
// @decision(DL-015)
func (c *Client) OpenCodeConnection(ctx context.Context) (server.OpenCodeConnectionView, error) {
	var out server.OpenCodeConnectionView
	err := c.do(ctx, http.MethodGet, "me/opencode", nil, &out)
	return out, err
}

// StartOpenCodeConnection starts an OpenCode Console sign-in, which the
// server runs.
//
// @decision(DL-015)
func (c *Client) StartOpenCodeConnection(ctx context.Context) (server.OpenCodeAttemptView, error) {
	var out server.OpenCodeAttemptView
	err := c.do(ctx, http.MethodPost, "me/opencode/connect", nil, &out)
	return out, err
}

// CheckOpenCodeConnection reports where a sign-in attempt stands.
//
// @decision(DL-015)
func (c *Client) CheckOpenCodeConnection(ctx context.Context, id string) (server.OpenCodeAttemptView, error) {
	var out server.OpenCodeAttemptView
	err := c.do(ctx, http.MethodGet, "me/opencode/connect/"+url.PathEscape(id), nil, &out)
	return out, err
}

// DeleteOpenCodeConnection forgets the caller's OpenCode sign-in.
//
// @decision(DL-015)
func (c *Client) DeleteOpenCodeConnection(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "me/opencode", nil, nil)
}

// UpdateIdentity sets the git author for the caller's sessions. A nil
// field is left as it was.
//
// @decision(DL-044)
func (c *Client) UpdateIdentity(ctx context.Context, name, email *string) (server.MeView, error) {
	body := struct {
		GitName  *string `json:"git_name,omitempty"`
		GitEmail *string `json:"git_email,omitempty"`
	}{GitName: name, GitEmail: email}
	var out server.MeView
	err := c.do(ctx, http.MethodPatch, "me", body, &out)
	return out, err
}

// Connect mints connection credentials for a running session.
//
// @decision(DL-030)
func (c *Client) Connect(ctx context.Context, ref string) (server.ConnectionView, error) {
	var out server.ConnectionView
	err := c.do(ctx, http.MethodPost, "sessions/"+url.PathEscape(ref)+"/connect", nil, &out)
	return out, err
}

// ExportSessionSync exports a running single-repository session's git
// commits and working-tree changes for local sync-back.
//
// @decision(DL-027)
func (c *Client) ExportSessionSync(ctx context.Context, ref string) (server.SyncExportView, error) {
	var out server.SyncExportView
	err := c.do(ctx, http.MethodPost, "sessions/"+url.PathEscape(ref)+"/sync-export", nil, &out)
	return out, err
}

// ListRepos lists the repositories the server can bootstrap, and the
// platforms it could not list.
//
// @decision(DL-028)
func (c *Client) ListRepos(ctx context.Context) (repos []server.RepoView, warnings []string, err error) {
	repos, warnings, _, err = c.ListReposFor(ctx, nil)
	return repos, warnings, err
}

// ListReposFor is ListRepos with the platforms whose launch prefers the
// organisation's credential, and the tier each platform resolves to
// under that preference.
//
// @decision(DL-042)
func (c *Client) ListReposFor(ctx context.Context, preferShared []string) ([]server.RepoView, []string, []server.PlatformTier, error) {
	var out struct {
		Repos     []server.RepoView     `json:"repos"`
		Warnings  []string              `json:"warnings"`
		Platforms []server.PlatformTier `json:"platforms"`
	}
	path := "repos"
	if len(preferShared) > 0 {
		path += "?prefer_shared=" + url.QueryEscape(strings.Join(preferShared, ","))
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Repos, out.Warnings, out.Platforms, err
}

// CreateJob asks for an autonomous job and returns it. The run happens
// on the server; the job outlives the terminal that started it.
//
// @decision(DL-045)
func (c *Client) CreateJob(ctx context.Context, req server.JobRequest) (server.JobView, error) {
	var out server.JobView
	err := c.do(ctx, http.MethodPost, "jobs", req, &out)
	return out, err
}

// JobPage is one page of the job listing, as the API sends it.
//
// @decision(DL-012)
type JobPage struct {
	Jobs  []server.JobView `json:"jobs"`
	Next  string           `json:"next,omitempty"`
	Total int              `json:"total"`
}

// ListJobs reads one page of the jobs the caller may see, newest first.
//
// @decision(DL-045) @decision(DL-012)
func (c *Client) ListJobs(ctx context.Context, p Page) (JobPage, error) {
	var out JobPage
	err := c.do(ctx, http.MethodGet, "jobs"+p.query(), nil, &out)
	return out, err
}

// GetJob resolves one job by id.
//
// @decision(DL-045)
func (c *Client) GetJob(ctx context.Context, id string) (server.JobView, error) {
	var out server.JobView
	err := c.do(ctx, http.MethodGet, "jobs/"+url.PathEscape(id), nil, &out)
	return out, err
}

// maxJobFileBytes bounds a file read back, above the server's own
// collection limit.
const maxJobFileBytes = 16 << 20

// GetJobFile reads one file a job delivered, by its path under the out
// directory, with the type the server stored it as.
//
// @decision(DL-051)
func (c *Client) GetJobFile(ctx context.Context, id, path string) ([]byte, string, error) {
	q := url.Values{"path": {path}}
	req, err := c.newRequest(ctx, http.MethodGet, "jobs/"+url.PathEscape(id)+"/file?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "*/*")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("client: GET job file: %w", redactURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", problemOf(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJobFileBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("client: GET job file: %w", err)
	}
	if len(body) > maxJobFileBytes {
		return nil, "", fmt.Errorf("client: %s is larger than %d bytes", path, maxJobFileBytes)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// CancelJob cancels a job and terminates its sandbox.
//
// @decision(DL-045)
func (c *Client) CancelJob(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "jobs/"+url.PathEscape(id), nil, nil)
}

// ExportJobSync exports a sync job's state from its sandbox.
//
// @decision(DL-102)
func (c *Client) ExportJobSync(ctx context.Context, id string) (server.SyncExportView, error) {
	var out server.SyncExportView
	err := c.do(ctx, http.MethodPost, "jobs/"+url.PathEscape(id)+"/sync-export", nil, &out)
	return out, err
}

// ReleaseJob ends a held sync job's sandbox, recording an apply when
// applied, and answers the job's view.
//
// @decision(DL-102)
func (c *Client) ReleaseJob(ctx context.Context, id string, applied bool) (server.JobView, error) {
	var out server.JobView
	body := struct {
		Applied bool `json:"applied,omitempty"`
	}{applied}
	err := c.do(ctx, http.MethodPost, "jobs/"+url.PathEscape(id)+"/release", body, &out)
	return out, err
}

// ResumeJob hands a suspended job back to its loop.
//
// @decision(DL-054)
func (c *Client) ResumeJob(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "jobs/"+url.PathEscape(id)+"/resume", nil, nil)
}

// Steer sends a message into a running session. No CLI command wraps
// it; an attached CLI already talks to the sandbox through DL-030's
// proxy.
//
// @decision(DL-019)
func (c *Client) Steer(ctx context.Context, ref string, req server.SteerRequest) (server.SteeredMessage, error) {
	var out server.SteeredMessage
	err := c.do(ctx, http.MethodPost, "sessions/"+url.PathEscape(ref)+"/messages", req, &out)
	return out, err
}

// Interrupt stops the turn a session is running. `spov jobs interrupt`
// is the CLI command that calls it, for a job's session.
//
// @decision(DL-053) @decision(DL-023)
func (c *Client) Interrupt(ctx context.Context, ref string, req server.InterruptRequest) (server.Interrupted, error) {
	var out server.Interrupted
	err := c.do(ctx, http.MethodPost, "sessions/"+url.PathEscape(ref)+"/interrupt", req, &out)
	return out, err
}

// PinTimeout bounds the pin read. Short on purpose: it sits on the
// launch path and its failure means the client's own configuration decides, so waiting on it
// is a worse outcome than not asking.
//
// @decision(DL-035)
const PinTimeout = 5 * time.Second

// OpenCodePin asks the deployment which OpenCode build its sandboxes
// run. Unauthenticated and outside APIPrefix, so it needs none of
// newRequest: no bearer is sent, which is the point — a client wants
// this before it has one. A scheme this build does not know is not an
// answer.
//
// @decision(DL-035)
func (c *Client) OpenCodePin(ctx context.Context) (server.OpenCodePin, error) {
	var pin server.OpenCodePin
	if c.BaseURL == "" {
		return pin, ErrNoServer
	}
	ctx, cancel := context.WithTimeout(ctx, PinTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+server.OpenCodePinPath, nil)
	if err != nil {
		return pin, err
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return pin, fmt.Errorf("client: GET %s: %w", server.OpenCodePinPath, redactURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return server.OpenCodePin{}, problemOf(resp)
	}
	// Bounded: two fields from a server this client has not
	// authenticated to.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return server.OpenCodePin{}, fmt.Errorf("client: GET %s: %w", server.OpenCodePinPath, err)
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		return server.OpenCodePin{}, fmt.Errorf("client: GET %s: decode response: %w", server.OpenCodePinPath, err)
	}
	if pin.Scheme != server.PinScheme {
		return server.OpenCodePin{}, fmt.Errorf("client: %s answered scheme %d, and this build reads %d",
			server.OpenCodePinPath, pin.Scheme, server.PinScheme)
	}
	return pin, nil
}
