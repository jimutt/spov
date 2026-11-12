package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/console"
	"github.com/jimutt/spov/internal/jobs"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// Tenant isolation. In a one-organisation deployment every user shares
// the org_id the store filters on, so Service.Get and Service.GetJob
// (and the owner filter in the two listings) are the whole wall between
// users. These tests are the permanent form of a one-off sweep: user B
// calls every route against user A's resources, and the set of routes
// is read from the API document so a route added without a case here
// fails the build.
//
// A new operation must be added to exactly one table below: `byRef`
// (it names a session or job by ID or name), `byOtherID` (it names some
// other per-user resource by ID), `ownNamespace` (it is keyed by the
// caller alone), `adminOnly`, `creations`, or `notScoped` with a
// comment saying why nothing of one user's is reachable through it.
//
// @decision(DL-006) @decision(DL-005)

const (
	aSecretCredential = "github_pat_A_PRIVATE_TOKEN"
	aSecretRouted     = "npm_A_PRIVATE_SECRET"
	aMessageText      = "A's private words about the roadmap"
	aReportBody       = "# A's private report\nthe numbers are A's"
	aVerifiedAs       = "owner-of-" + aSecretCredential
	aEmail            = "a-private@example.com"
	strangerOrg       = "org-stranger"
)

// route is one request of a sweep: an operation as the OpenAPI document
// keys it ("METHOD /path/{placeholder}"), the values that fill its
// placeholders, and a body valid enough to get past request validation,
// which runs before any handler and would otherwise answer 422 for
// every case and prove nothing.
type route struct {
	op    string
	body  any
	query string
	// refs are the values of the route's main placeholder, {ref} for a
	// session or {id} for a job, one request each.
	refs []string
	// ids fills every other placeholder.
	ids map[string]string
	// ownerRef is the {ref} the owner's control request uses; the
	// first of refs when empty.
	ownerRef string
}

func (r route) method() string { return strings.SplitN(r.op, " ", 2)[0] }

func (r route) path(ref string) string {
	p := strings.SplitN(r.op, " ", 2)[1]
	main := "{ref}"
	if !strings.Contains(p, main) {
		main = "{id}"
	}
	if len(r.refs) > 0 {
		p = strings.ReplaceAll(p, main, url.PathEscape(ref))
	}
	for k, v := range r.ids {
		p = strings.ReplaceAll(p, "{"+k+"}", url.PathEscape(v))
	}
	if r.query != "" {
		p += "?" + r.query
	}
	return p
}

// requests returns the paths the route is sent to: one per ref, or the
// single path when it names none.
func (r route) requests() []string {
	if len(r.refs) == 0 {
		return []string{r.path("")}
	}
	var out []string
	for _, ref := range r.refs {
		out = append(out, r.path(ref))
	}
	return out
}

// tenantWorld is user A's belongings, set up through the API as A.
type tenantWorld struct {
	t       *testing.T
	f       *apiFixture
	login   *Login
	console *console.Fake

	a, b, c, admin auth.Principal

	run, stopped store.Session
	job, syncJob store.Job
	jobSession   store.Session

	// sandboxHits counts requests that reached any of A's sandboxes.
	sandboxHits atomic.Int32
	hitsBase    int32

	askID, formID, msgID           string
	tokenID, webSessionID, attempt string
}

// markers are strings that exist only in A's resources. No response to
// anyone else may contain one.
func (w *tenantWorld) markers() []string {
	return []string{
		aSecretCredential, aSecretRouted, aMessageText, aReportBody, aVerifiedAs, aEmail,
		w.webSessionID, w.tokenID, w.attempt,
		w.run.SandboxID, w.askID, w.formID,
	}
}

func (w *tenantWorld) as(p auth.Principal) {
	w.f.auth.inner = auth.StaticAuthenticator{Principal: p}
}

func newTenantWorld(t *testing.T) *tenantWorld {
	t.Helper()
	w := &tenantWorld{t: t}
	w.a = auth.Principal{UserID: "u-a", OrgID: auth.LocalID, Email: aEmail}
	w.b = auth.Principal{UserID: "u-b", OrgID: auth.LocalID, Email: "b@example.com"}
	w.admin = auth.Principal{UserID: "u-admin", OrgID: auth.LocalID, Email: "admin@example.com", Admin: true}
	w.c = auth.Principal{UserID: "u-c", OrgID: strangerOrg, Email: "c@elsewhere.example"}

	sessions := scs.New()
	sessions.Store = memstore.New()
	w.console = console.NewFake()
	t.Cleanup(w.console.Close)
	// A's sandboxes answer from here, and count who got through to them.
	sb := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		w.sandboxHits.Add(1)
		_, _ = rw.Write([]byte("{}"))
	}))
	t.Cleanup(sb.Close)
	q := &recordingQueue{}
	w.f = newAPIFixtureWith(t, func(d *Deps) {
		d.Provider.(*sandbox.Fake).ConnectBaseURL = sb.URL
		d.Jobs = &jobs.Supervisor{Store: d.Store, Provider: d.Provider, Queue: q.insert, Warn: d.Warn}
		sealer := testSealer(t)
		d.RoutedCredentials = &SealedRoutedCredentials{Store: d.Store.Credentials(), Sealer: sealer}
		d.Credentials = &CredentialStore{Store: d.Store.Credentials(), Sealer: sealer}
		d.Verifier = ownerVerifier{}
		d.Console = &console.Client{BaseURL: w.console.URL, UserAgent: "spov/test"}
		d.ConsoleGrants = &MemoryConsoleStore{}
		d.OrgApps = &OrgApps{Store: d.Store, Sealer: sealer}
		d.OrgGitHub = &fakeOrgGitHub{}
		d.OrgRoutedCredentials = &SealedOrgRoutedCredentials{Store: d.Store, Sealer: sealer}
		// A login makes the browser-session routes exist; the cookie
		// flow itself is not used, the principal is static.
		w.login = &Login{Sessions: sessions, Users: d.Store.Users(), OrgID: auth.LocalID}
		d.Login = w.login
	})
	f := w.f
	// Streams end at once, so a request as the owner does not hang.
	f.srv.streamLifetime = 50 * time.Millisecond
	f.srv.heartbeat = time.Hour

	ctx := context.Background()
	for _, u := range []auth.Principal{w.a, w.b, w.admin, w.c} {
		if err := f.store.Users().Create(ctx, store.User{ID: u.UserID, OrgID: u.OrgID, Email: u.Email}); err != nil {
			t.Fatal(err)
		}
	}

	w.as(w.a)
	// A running session and a stopped one.
	w.run = w.launch("a-run")
	w.stopped = w.launch("a-stopped")
	if resp, body := f.do(http.MethodDelete, "/api/v1/sessions/"+w.stopped.ID, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("stopping A's session: %d %s", resp.StatusCode, body)
	}
	w.stopped, _ = f.store.Sessions().Get(ctx, auth.LocalID, w.stopped.ID)
	if w.stopped.State != store.StateStopped {
		t.Fatalf("A's second session is %s, want stopped", w.stopped.State)
	}

	// What A said, and what the agent asked A.
	conv := seedConversation(t, f, w.run, "ses_a", "A's conversation")
	// A person's message as the recorder stored it: the message row, and
	// the inbox event that cancelling one is checked against.
	w.msgID, w.askID, w.formID = realInboxMessageID, "per_a1", "frm_a1"
	seedMessage(t, f, conv, w.msgID, "user", aMessageText, recordedAt)
	seedEvent(t, f, store.Event{
		OrgID: w.run.OrgID, SessionID: w.run.ID, ConversationID: conv.ID, Source: store.SourceOpenCode, Seq: 30,
		Name: jobs.EventInboxEnqueued, Text: aMessageText, At: recordedAt,
		Raw: []byte(strings.Replace(realInboxFrame, "reply with ok", aMessageText, 1)),
	})
	askOn(t, f.srv.svc, w.run, eventPermissionAsked, w.askID, store.SourceEphemeral)
	askOn(t, f.srv.svc, w.run, eventFormCreated, w.formID, store.SourceEphemeral)

	// A job with its session and one delivered report.
	view, err := f.srv.svc.CreateJob(ctx, w.a, jobRequest())
	if err != nil {
		t.Fatal(err)
	}
	w.job, err = f.store.Jobs().Get(ctx, auth.LocalID, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.jobSession, err = f.store.Sessions().Get(ctx, auth.LocalID, w.job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Jobs().PutJobFile(ctx, auth.LocalID, store.JobFile{
		JobID: w.job.ID, Path: "report.md", ContentType: "text/markdown",
		Size: int64(len(aReportBody)), Body: []byte(aReportBody),
	}); err != nil {
		t.Fatal(err)
	}

	// A sync job, the only kind the sync routes answer for. Its session
	// is put in the store rather than launched; what matters is the owner.
	syncSess := store.Session{
		ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: w.a.UserID,
		Name: "job-a-sync", State: store.StateRunning, SandboxID: w.run.SandboxID,
		Request: store.SessionRequest{Egress: "agent"},
	}
	if err := f.store.Sessions().Create(ctx, syncSess); err != nil {
		t.Fatal(err)
	}
	w.syncJob = store.Job{
		ID: store.NewID(time.Now()), OrgID: auth.LocalID, OwnerID: w.a.UserID,
		SessionID: syncSess.ID, Kind: store.JobKindSync, State: store.JobSucceeded, Goal: "sync A's checkout",
	}
	if err := f.store.Jobs().Create(ctx, w.syncJob); err != nil {
		t.Fatal(err)
	}

	// Credentials: a stored one, a routed one, an OpenCode sign-in.
	w.put("/api/v1/me/credentials/github", map[string]string{"token": aSecretCredential})
	w.put("/api/v1/me/routed-credentials/acme-npm", map[string]any{"secret": aSecretRouted, "route": acmeNPM})
	resp, body := f.do(http.MethodPost, "/api/v1/me/opencode/connect", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A's OpenCode sign-in: %d %s", resp.StatusCode, body)
	}
	var attempt OpenCodeAttemptView
	if err := json.Unmarshal(body, &attempt); err != nil {
		t.Fatal(err)
	}
	w.attempt = attempt.ID

	// An API token and a browser login.
	w.tokenID = store.NewID(time.Now())
	if err := f.store.Tokens().Create(ctx, store.APIToken{
		ID: w.tokenID, OrgID: auth.LocalID, UserID: w.a.UserID, Hash: auth.HashToken("spov_a_token"), Name: "a laptop",
	}); err != nil {
		t.Fatal(err)
	}
	sctx, err := sessions.Load(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	sessions.Put(sctx, auth.SessionKeyUserID, w.a.UserID)
	cookie, _, err := sessions.Commit(sctx)
	if err != nil {
		t.Fatal(err)
	}
	w.webSessionID = tokenID(cookie)
	w.hitsBase = w.sandboxHits.Load()
	w.as(w.b)
	return w
}

func (w *tenantWorld) launch(name string) store.Session {
	w.t.Helper()
	id := w.f.create(repoReq(name))
	w.f.events(id)
	sess, err := w.f.store.Sessions().Get(context.Background(), auth.LocalID, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return sess
}

func (w *tenantWorld) put(path string, body any) {
	w.t.Helper()
	if resp, b := w.f.do(http.MethodPut, path, body); resp.StatusCode != http.StatusOK {
		w.t.Fatalf("PUT %s: %d %s", path, resp.StatusCode, b)
	}
}

// sessionRefs names A's sessions every way a caller can: by ID, by
// name, and for a job's session by its job-<id> name.
func (w *tenantWorld) sessionRefs() []string {
	return []string{
		w.run.ID, w.run.Name, w.stopped.ID, w.stopped.Name,
		w.jobSession.ID, w.jobSession.Name,
	}
}

// send makes one request as p and returns the status a client is told
// and the body. A stream reports its failure as an error event on a 200
// response, so that event's status is the answer.
func (w *tenantWorld) send(p auth.Principal, method, path string, body any) (int, string) {
	w.t.Helper()
	prev := w.f.auth.inner
	w.as(p)
	defer func() { w.f.auth.inner = prev }()
	// A route that opens a stream it should have refused would wait for
	// ever; the bound turns that into a failure naming the route.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, raw := w.f.do(method, path, body, func(r *http.Request) { *r = *r.WithContext(ctx) })
	status := resp.StatusCode
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, ev := range parseEvents(raw) {
			var e ErrorEvent
			if json.Unmarshal(ev.Data, &e) == nil && e.Problem.Status != 0 {
				status = e.Problem.Status
			}
		}
	}
	return status, string(raw)
}

// A's world as the stores hold it, to compare before and after B acts.
func (w *tenantWorld) snapshot() string {
	w.t.Helper()
	w.requireSandboxesUntouched()
	ctx := context.Background()
	st := w.f.store
	out := map[string]any{}
	sessions, err := st.Sessions().List(ctx, auth.LocalID, store.SessionFilter{OwnerID: w.a.UserID, Limit: 500})
	if err != nil {
		w.t.Fatal(err)
	}
	var rows []string
	for _, s := range sessions {
		evs, _ := st.Conversations().ListEvents(ctx, s.OrgID, s.ID, 0, 1000)
		msgs, _ := st.Conversations().ListMessages(ctx, s.OrgID, s.ID)
		rows = append(rows, fmt.Sprintf("%s %s %s sandbox=%s events=%d messages=%d", s.ID, s.Name, s.State, s.SandboxID, len(evs), len(msgs)))
	}
	sort.Strings(rows)
	out["sessions"] = rows
	js, err := st.Jobs().List(ctx, auth.LocalID, store.JobFilter{OwnerID: w.a.UserID})
	if err != nil {
		w.t.Fatal(err)
	}
	var jrows []string
	for _, j := range js {
		files, _ := st.Jobs().JobFiles(ctx, j.OrgID, j.ID)
		jrows = append(jrows, fmt.Sprintf("%s %s owner=%s files=%d", j.ID, j.State, j.OwnerID, len(files)))
	}
	sort.Strings(jrows)
	out["jobs"] = jrows
	// A's sandboxes, by state: B's own launches add sandboxes of B's.
	infos, _ := w.f.provider.List(ctx, nil)
	var srows []string
	for _, in := range infos {
		for _, s := range sessions {
			if s.SandboxID == in.ID {
				srows = append(srows, fmt.Sprintf("%s %v", in.ID, in.State))
			}
		}
	}
	sort.Strings(srows)
	out["sandboxes"] = srows
	creds, _ := st.Credentials().ListByUser(ctx, auth.LocalID, w.a.UserID)
	var crows []string
	for _, c := range creds {
		crows = append(crows, fmt.Sprintf("%s %x", c.Platform, c.Ciphertext))
	}
	sort.Strings(crows)
	out["credentials"] = crows
	toks, _ := st.Tokens().ListByUser(ctx, auth.LocalID, w.a.UserID)
	var trows []string
	for _, tk := range toks {
		trows = append(trows, fmt.Sprintf("%s revoked=%v", tk.ID, tk.RevokedAt))
	}
	out["tokens"] = trows
	web, _ := w.login.sessionsOf(ctx, w.a.UserID)
	var wrows []string
	for _, s := range web {
		wrows = append(wrows, s.id)
	}
	out["web"] = wrows
	// The sign-in and the user row are read through the API as A.
	_, console := w.send(w.a, http.MethodGet, "/api/v1/me/opencode", nil)
	out["opencode"] = console
	u, _ := st.Users().Get(ctx, auth.LocalID, w.a.UserID)
	out["user"] = fmt.Sprintf("%+v", u)
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}

// operations are the API document's operations keyed "METHOD path".
func operationsOf(api huma.API) map[string]*huma.Operation {
	out := map[string]*huma.Operation{}
	for p, item := range api.OpenAPI().Paths {
		for m, op := range map[string]*huma.Operation{
			http.MethodGet: item.Get, http.MethodPost: item.Post, http.MethodPut: item.Put,
			http.MethodPatch: item.Patch, http.MethodDelete: item.Delete,
			http.MethodHead: item.Head, http.MethodOptions: item.Options, http.MethodTrace: item.Trace,
		} {
			if op != nil {
				out[m+" "+p] = op
			}
		}
	}
	return out
}

// byRef are the routes that name one of A's sessions or jobs. Every
// one answers 404 to B, whether B names it by ID, by session name or by
// the job-<id> name of a job's session.
func (w *tenantWorld) byRef() []route {
	sess := w.sessionRefs()
	jobIDs := []string{w.job.ID, w.syncJob.ID}
	msg := map[string]string{"id": w.msgID}
	return []route{
		{op: "GET /api/v1/sessions/{ref}", refs: sess},
		{op: "GET /api/v1/sessions/{ref}/events", refs: sess},
		{op: "GET /api/v1/sessions/{ref}/log", refs: sess},
		{op: "GET /api/v1/sessions/{ref}/log", refs: sess, query: "after=0"},
		{op: "GET /api/v1/sessions/{ref}/messages", refs: sess},
		{op: "POST /api/v1/sessions/{ref}/messages", refs: sess, body: map[string]string{"text": "hello from B"}},
		{op: "DELETE /api/v1/sessions/{ref}/messages/{id}", refs: sess, ids: msg},
		{op: "POST /api/v1/sessions/{ref}/interrupt", refs: sess, body: map[string]any{}},
		{op: "POST /api/v1/sessions/{ref}/permissions/{id}", refs: sess, ids: map[string]string{"id": w.askID},
			body: map[string]string{"decision": "once"}},
		{op: "POST /api/v1/sessions/{ref}/forms/{id}", refs: sess, ids: map[string]string{"id": w.formID},
			body: map[string]any{"answer": map[string]any{"q0": "Blue"}}},
		{op: "DELETE /api/v1/sessions/{ref}/forms/{id}", refs: sess, ids: map[string]string{"id": w.formID}},
		{op: "POST /api/v1/sessions/{ref}/model", refs: sess, body: map[string]string{"model": "openrouter/some-model"}},
		{op: "POST /api/v1/sessions/{ref}/sync-export", refs: sess},
		{op: "POST /api/v1/sessions/{ref}/connect", refs: sess},
		{op: "POST /api/v1/sessions/{ref}/resume", refs: sess, body: map[string]any{}, ownerRef: w.stopped.ID},

		{op: "GET /api/v1/jobs/{id}", refs: jobIDs},
		{op: "GET /api/v1/jobs/{id}/file", refs: jobIDs, query: "path=report.md"},
		{op: "GET /api/v1/jobs/{id}/file", refs: jobIDs, query: "path=report.md&download=true"},
		{op: "POST /api/v1/jobs/{id}/resume", refs: jobIDs},
		{op: "POST /api/v1/jobs/{id}/sync-export", refs: jobIDs, ownerRef: w.syncJob.ID},
		{op: "POST /api/v1/jobs/{id}/release", refs: jobIDs, ownerRef: w.syncJob.ID, body: map[string]any{}},

		// The destructive ones last, so the owner's control requests
		// leave the world intact for the ones before them.
		{op: "DELETE /api/v1/jobs/{id}", refs: jobIDs},
		{op: "DELETE /api/v1/sessions/{ref}", refs: sess, ownerRef: w.jobSession.ID},
	}
}

// byOtherID are routes that name another kind of per-user resource by
// an ID the caller supplies; B names A's, and is told it does not exist.
func (w *tenantWorld) byOtherID() []route {
	return []route{
		{op: "DELETE /api/v1/me/tokens/{id}", ids: map[string]string{"id": w.tokenID}},
		{op: "DELETE /api/v1/me/sessions/{id}", ids: map[string]string{"id": w.webSessionID}},
		{op: "GET /api/v1/me/opencode/connect/{id}", ids: map[string]string{"id": w.attempt}},
	}
}

// ownNamespace are routes keyed by the caller alone, with no ID of
// anyone else's to name. B uses each, and what B reads has nothing of
// A's in it and what B writes (A's own platform, route name or sign-in
// included) leaves A's untouched. The expected status is 2xx.
func (w *tenantWorld) ownNamespace() []route {
	return []route{
		{op: "GET /api/v1/me"},
		{op: "PATCH /api/v1/me", body: map[string]string{"git_name": "B"}},
		{op: "GET /api/v1/me/usage"},
		{op: "GET /api/v1/me/sessions"},
		{op: "GET /api/v1/me/tokens"},
		{op: "GET /api/v1/me/credentials"},
		{op: "PUT /api/v1/me/credentials/{platform}", ids: map[string]string{"platform": "github"},
			body: map[string]string{"token": "github_pat_B_OWN"}},
		{op: "DELETE /api/v1/me/credentials/{platform}", ids: map[string]string{"platform": "github"}},
		{op: "GET /api/v1/me/routed-credentials"},
		{op: "PUT /api/v1/me/routed-credentials/{name}", ids: map[string]string{"name": "acme-npm"},
			body: map[string]any{"secret": "npm_B_OWN", "route": acmeNPM}},
		{op: "DELETE /api/v1/me/routed-credentials/{name}", ids: map[string]string{"name": "acme-npm"}},
		{op: "GET /api/v1/me/opencode"},
		{op: "DELETE /api/v1/me/opencode"},
		{op: "POST /api/v1/me/opencode/connect"},

		// The two listings, plainly and with A's IDs as the filter or
		// the cursor, which must narrow what B sees and never widen it.
		{op: "GET /api/v1/sessions"},
		{op: "GET /api/v1/sessions", query: "before=" + w.run.ID},
		{op: "GET /api/v1/sessions", query: "name=" + w.run.Name},
		{op: "GET /api/v1/sessions", query: "state=running"},
		{op: "GET /api/v1/sessions/events"},
		{op: "GET /api/v1/jobs"},
		{op: "GET /api/v1/jobs", query: "before=" + w.job.ID},
		{op: "GET /api/v1/jobs", query: "session=" + w.jobSession.ID},
		{op: "GET /api/v1/jobs", query: "state=queued"},
	}
}

// creations make something new owned by the caller; they name nothing
// of anyone's.
func (w *tenantWorld) creations() []route {
	return []route{
		{op: "POST /api/v1/sessions", body: repoReq("b-new")},
		{op: "POST /api/v1/jobs", body: jobRequest()},
	}
}

// notScoped are the operations with no per-user resource behind them,
// and why. A new entry needs the same reason.
var notScoped = map[string]string{
	"GET /api/v1/models": "the model catalogue, the same for everyone; only whether a key exists is per caller",
	"GET /api/v1/repos":  "the forges' repositories as the caller's own credential sees them; reads no spov row",
	"GET /api/v1/org/routed-credentials": "the organisation's routed credentials, shared with every member by design: " +
		"names and routes, never secrets (DL-074)",
}

// adminOnly are the operations a non-administrator is refused with 403.
// Every operation the document tags "admin" must be here.
func (w *tenantWorld) adminOnly() []route {
	return []route{
		{op: "GET /api/v1/users"},
		{op: "PATCH /api/v1/users/{id}", ids: map[string]string{"id": w.a.UserID},
			body: map[string]any{"quota": map[string]any{"max_concurrent_sessions": 1}}},
		{op: "GET /api/v1/allowlist"},
		{op: "PUT /api/v1/allowlist/{email}", ids: map[string]string{"email": "b@example.com"}, body: map[string]any{"admin": true}},
		{op: "DELETE /api/v1/allowlist/{email}", ids: map[string]string{"email": aEmail}},
		{op: "GET /api/v1/org/github"},
		{op: "PUT /api/v1/org/github/{platform}", ids: map[string]string{"platform": "gh"},
			body: OrgGitHubApp{AppID: "1", PrivateKey: "x"}},
		{op: "DELETE /api/v1/org/github/{platform}", ids: map[string]string{"platform": "gh"}},
		{op: "PUT /api/v1/org/routed-credentials/{name}", ids: map[string]string{"name": "acme-npm"},
			body: map[string]any{"secret": "npm_B_ORG", "route": acmeNPM}},
		{op: "DELETE /api/v1/org/routed-credentials/{name}", ids: map[string]string{"name": "acme-npm"}},
	}
}

// covered lists every operation the tables name.
func (w *tenantWorld) covered() map[string]string {
	out := map[string]string{}
	add := func(table string, rs []route) {
		for _, r := range rs {
			out[r.op] = table
		}
	}
	add("byRef", w.byRef())
	add("byOtherID", w.byOtherID())
	add("ownNamespace", w.ownNamespace())
	add("adminOnly", w.adminOnly())
	add("creations", w.creations())
	for op := range notScoped {
		out[op] = "notScoped"
	}
	return out
}

// leaks reports which of A's markers a response carries.
func (w *tenantWorld) leaks(body string) []string {
	var found []string
	for _, m := range w.markers() {
		if m != "" && strings.Contains(body, m) {
			found = append(found, m)
		}
	}
	return found
}

// TestEveryOperationIsInTheTenantIsolationSweep: the sweep is built
// from the API document, so an operation added without deciding how it
// is scoped fails here, naming it.
//
// @decision(DL-006) @decision(DL-005)
func TestEveryOperationIsInTheTenantIsolationSweep(t *testing.T) {
	w := newTenantWorld(t)
	ops := operationsOf(w.f.srv.api)
	covered := w.covered()
	for _, key := range sortedOps(ops) {
		if _, ok := covered[key]; !ok {
			t.Errorf("%s is in the API but in no table of tenantisolation_test.go: add it to byRef, byOtherID, "+
				"ownNamespace, adminOnly or creations, or to notScoped with the reason no user's data is behind it", key)
		}
	}
	for _, key := range sortedOps(covered) {
		if _, ok := ops[key]; !ok {
			t.Errorf("the sweep names %s, which the API no longer has: remove it", key)
		}
	}
	// Every operation the document tags admin is in the admin table.
	admin := map[string]bool{}
	for _, r := range w.adminOnly() {
		admin[r.op] = true
	}
	for key, op := range ops {
		tagged := false
		for _, tag := range op.Tags {
			tagged = tagged || tag == "admin"
		}
		if tagged && !admin[key] {
			t.Errorf("%s is tagged admin but is not in adminOnly, so nothing checks a non-admin is refused", key)
		}
		if admin[key] && !tagged {
			t.Errorf("%s is in adminOnly but is not tagged admin in the API document", key)
		}
	}
}

func sortedOps[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestNoRouteAnswersAnotherUser: B, an ordinary user of the same
// organisation, calls every route against A's resources. Anything that
// names A's session or job is 404 (never 403, which would confirm it
// exists, and never 200), every listing B reads holds nothing of A's,
// no response carries a byte of what only A holds, and A's world is
// the same afterwards.
//
// @decision(DL-006) @decision(DL-005)
func TestNoRouteAnswersAnotherUser(t *testing.T) {
	w := newTenantWorld(t)
	before := w.snapshot()
	var sent int

	check := func(r route, path string, want func(status int) bool, wantText string) {
		t.Helper()
		status, body := w.send(w.b, r.method(), path, r.body)
		sent++
		if !want(status) {
			t.Errorf("B: %s %s = %d, want %s\n%s", r.method(), path, status, wantText, body)
		}
		if l := w.leaks(body); len(l) > 0 {
			t.Errorf("B: %s %s = %d carries what only A holds: %q\n%s", r.method(), path, status, l, body)
		}
	}
	notFound := func(s int) bool { return s == http.StatusNotFound }
	ok := func(s int) bool { return s >= 200 && s < 300 }

	for _, r := range w.byRef() {
		for _, path := range r.requests() {
			check(r, path, notFound, "404")
		}
	}
	for _, r := range w.byOtherID() {
		for _, path := range r.requests() {
			check(r, path, notFound, "404")
		}
	}
	for _, r := range w.ownNamespace() {
		for _, path := range r.requests() {
			check(r, path, ok, "2xx")
		}
	}
	if after := w.snapshot(); after != before {
		t.Errorf("B's requests changed A's world:\nbefore %s\nafter  %s", before, after)
	}
	// B's own writes took: the sweep did something.
	if _, body := w.send(w.b, http.MethodGet, "/api/v1/me", nil); !strings.Contains(body, "B") {
		t.Errorf("B's own write did not apply, so the own-namespace routes were not exercised: %s", body)
	}
	if sent < 100 {
		t.Errorf("only %d requests were sent", sent)
	}
}

// TestTheOwnerReachesWhatTheSweepAsksFor is the control for the sweep:
// each by-reference request, sent by A, is not a 404. Without it a typo
// in a path or an ID would pass the sweep for the wrong reason.
//
// @decision(DL-006)
func TestTheOwnerReachesWhatTheSweepAsksFor(t *testing.T) {
	w := newTenantWorld(t)
	for _, r := range append(w.byRef(), w.byOtherID()...) {
		ref := r.ownerRef
		if ref == "" && len(r.refs) > 0 {
			ref = r.refs[0]
		}
		path := r.path(ref)
		status, body := w.send(w.a, r.method(), path, r.body)
		t.Logf("A: %s %s = %d", r.method(), path, status)
		if status == http.StatusNotFound {
			t.Errorf("A: %s %s = 404, so the sweep proves nothing for this route:\n%s", r.method(), path, body)
		}
	}
}

// TestACreatedSessionOrJobIsTheCallersAlone: what B creates is B's, in
// B's listings, and A's world does not change.
//
// @decision(DL-006)
func TestACreatedSessionOrJobIsTheCallersAlone(t *testing.T) {
	w := newTenantWorld(t)
	before := w.snapshot()
	for _, r := range w.creations() {
		status, body := w.send(w.b, r.method(), r.path(""), r.body)
		if status != http.StatusAccepted {
			t.Fatalf("B: %s = %d %s", r.op, status, body)
		}
	}
	sessions, _ := w.f.store.Sessions().List(context.Background(), auth.LocalID, store.SessionFilter{OwnerID: w.b.UserID, Limit: 50})
	if len(sessions) < 2 { // the session and the job's
		t.Errorf("B owns %d sessions after creating one session and one job", len(sessions))
	}
	jobs, _ := w.f.store.Jobs().List(context.Background(), auth.LocalID, store.JobFilter{OwnerID: w.b.UserID})
	if len(jobs) != 1 || jobs[0].OwnerID != w.b.UserID {
		t.Errorf("B's job = %+v", jobs)
	}
	// A creates under no name of B's reachable ID, and sees none of B's.
	_, list := w.send(w.a, http.MethodGet, "/api/v1/sessions", nil)
	if strings.Contains(list, "b-new") {
		t.Errorf("A's listing shows B's new session: %s", list)
	}
	if after := w.snapshot(); after != before {
		t.Errorf("B's creations changed A's world:\nbefore %s\nafter  %s", before, after)
	}
}

// TestAnAdministratorOnlyRouteRefusesANonAdministrator: B is not an
// admin, and each admin route says 403 before it reads anything.
//
// @decision(DL-006) @decision(DL-007)
func TestAnAdministratorOnlyRouteRefusesANonAdministrator(t *testing.T) {
	w := newTenantWorld(t)
	before := w.snapshot()
	for _, r := range w.adminOnly() {
		status, body := w.send(w.b, r.method(), r.path(""), r.body)
		if status != http.StatusForbidden {
			t.Errorf("B (not an admin): %s %s = %d, want 403\n%s", r.method(), r.path(""), status, body)
		}
		if l := w.leaks(body); len(l) > 0 {
			t.Errorf("B: %s carries what only A holds: %q", r.op, l)
		}
	}
	if after := w.snapshot(); after != before {
		t.Errorf("a refused admin request changed A's world:\nbefore %s\nafter  %s", before, after)
	}
}

// TestAnAdminSeesButAStrangerOrgDoesNot: an administrator of the same
// organisation reaches A's session and job, and an administrator of
// another organisation, or an ordinary user of it, reaches nothing: 404
// on every route that names one, and nothing of A's in what they list.
//
// @decision(DL-006)
func TestAnAdminSeesButAStrangerOrgDoesNot(t *testing.T) {
	w := newTenantWorld(t)
	for _, ref := range w.sessionRefs() {
		// An admin resolves a session by ID and by a live name.
		if status, body := w.send(w.admin, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(ref), nil); status != http.StatusOK &&
			ref != w.stopped.Name {
			t.Errorf("the org's admin: GET session %s = %d, want 200\n%s", ref, status, body)
		}
	}
	if status, body := w.send(w.admin, http.MethodGet, "/api/v1/jobs/"+w.job.ID, nil); status != http.StatusOK {
		t.Errorf("the org's admin: GET job = %d\n%s", status, body)
	}
	if status, body := w.send(w.admin, http.MethodGet, "/api/v1/sessions", nil); status != http.StatusOK || !strings.Contains(body, w.run.ID) {
		t.Errorf("the org's admin does not list A's session: %d\n%s", status, body)
	}

	before := w.snapshot()
	stranger := w.c
	strangerAdmin := w.c
	strangerAdmin.Admin = true
	for name, p := range map[string]auth.Principal{"a user of another org": stranger, "an admin of another org": strangerAdmin} {
		for _, table := range [][]route{w.byRef(), w.byOtherID()} {
			for _, r := range table {
				for _, path := range r.requests() {
					status, body := w.send(p, r.method(), path, r.body)
					if status != http.StatusNotFound {
						t.Errorf("%s: %s %s = %d, want 404\n%s", name, r.method(), path, status, body)
					}
				}
			}
		}
		for _, r := range w.ownNamespace() {
			for _, path := range r.requests() {
				status, body := w.send(p, r.method(), path, r.body)
				if status >= 300 {
					t.Errorf("%s: %s %s = %d\n%s", name, r.method(), path, status, body)
				}
				if l := w.leaks(body); len(l) > 0 {
					t.Errorf("%s: %s %s = %d carries what only A holds: %q\n%s", name, r.method(), path, status, l, body)
				}
			}
		}
		// The admin table: refused or empty, never A's.
		for _, r := range w.adminOnly() {
			status, body := w.send(p, r.method(), r.path(""), r.body)
			if l := w.leaks(body); len(l) > 0 {
				t.Errorf("%s: %s %s = %d carries what only A holds: %q\n%s", name, r.method(), r.path(""), status, l, body)
			}
			if !p.Admin && status != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d, want 403", name, r.method(), r.path(""), status)
			}
		}
	}
	if after := w.snapshot(); after != before {
		t.Errorf("a stranger org changed A's world:\nbefore %s\nafter  %s", before, after)
	}
}

// ownerVerifier verifies a credential "as" a name made from the secret,
// so what a listing says of a credential names whose it is.
type ownerVerifier struct{}

func (ownerVerifier) Verify(_ context.Context, _, secret string) (Verified, error) {
	return Verified{As: "owner-of-" + secret}, nil
}

// requireSandboxesUntouched fails when anything reached one of A's
// sandboxes since the world was built: only A's own requests may.
func (w *tenantWorld) requireSandboxesUntouched() {
	w.t.Helper()
	if n := w.sandboxHits.Load() - w.hitsBase; n != 0 {
		w.t.Errorf("%d requests reached A's sandboxes on someone else's behalf", n)
	}
}
