package server

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// Approval-page rate limits. A user code is 20^8, so guessing is not the
// threat these numbers address; a human mistyping is, and a lockout
// keeps a wrong-code loop from becoming an oracle.
//
// @decision(DL-006)
const (
	MaxCodeAttempts  = 5
	AttemptWindow    = 10 * time.Minute
	AttemptLockout   = 15 * time.Minute
	attemptSweepIdle = time.Hour
)

// handleVerifyPage shows the approval page, behind DL-007's login and
// behind a browser session specifically.
//
// @decision(DL-006)
func (d *Device) handleVerifyPage(w http.ResponseWriter, r *http.Request) {
	p, ok := approvingUser(r)
	if !ok {
		d.redirectToLogin(w, r)
		return
	}
	// ?code= comes from verification_uri_complete, so the human need
	// not retype it; it is prefilled, never acted on without a submit.
	d.renderApproval(w, r, approvalPage{
		Code: auth.NormalizeUserCode(r.URL.Query().Get("code")),
		User: p.Email,
	})
}

// handleApprove acts on the submitted code.
//
// @decision(DL-006)
func (d *Device) handleApprove(w http.ResponseWriter, r *http.Request) {
	p, ok := approvingUser(r)
	if !ok {
		d.redirectToLogin(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		d.renderApproval(w, r, approvalPage{User: p.Email, Error: "That form could not be read."})
		return
	}
	code := auth.NormalizeUserCode(r.FormValue("code"))
	deny := r.FormValue("action") == "deny"

	if until, locked := d.limiter().lockedUntil(p.UserID, d.now()); locked {
		d.renderApproval(w, r, approvalPage{
			User:  p.Email,
			Error: "Too many wrong codes. Try again in " + until.Round(time.Minute).String() + ".",
		})
		return
	}

	dc, err := d.Store.DeviceCodes().ByUserCode(r.Context(), code)
	switch {
	case errors.Is(err, store.ErrNotFound):
		d.wrongCode(w, r, p)
		return
	case err != nil:
		d.warn("device: looking up a user code: " + err.Error())
		d.renderApproval(w, r, approvalPage{User: p.Email, Error: "Something went wrong. Try again."})
		return
	}
	if d.now().After(dc.ExpiresAt) || dc.Status != store.DevicePending {
		// An expired or already-answered code is reported the same way
		// as an unknown one, so the page never confirms that a code
		// exists to someone who did not have it.
		d.wrongCode(w, r, p)
		return
	}
	// The second step: the code resolved, so show what is being
	// approved and require a deliberate action.
	if r.FormValue("confirm") == "" {
		d.renderApproval(w, r, approvalPage{
			Code: code, User: p.Email, TokenName: dc.Name, Confirm: true,
		})
		return
	}

	now := d.now()
	dc.Status = store.DeviceApproved
	dc.UserID = p.UserID
	dc.OrgID = p.OrgID
	if deny {
		dc.Status = store.DeviceDenied
		dc.UserID = ""
	}
	dc.UpdatedAt = now
	if err := d.Store.DeviceCodes().Update(r.Context(), dc); err != nil {
		d.warn("device: recording an approval: " + err.Error())
		d.renderApproval(w, r, approvalPage{User: p.Email, Error: "That could not be saved. Try again."})
		return
	}
	// Only a completed approval clears the attempt counter; resolving a
	// code alone does not.
	//
	// @decision(DL-006)
	d.limiter().reset(p.UserID)
	d.renderApproval(w, r, approvalPage{User: p.Email, Done: true, Denied: deny, TokenName: dc.Name})
}

// approvingUser is the human this request is acting as, if there is
// one. A principal carrying a TokenID is refused here too, as the
// second of two locks on the same door (Handler already routes only a
// browser authenticator to this page).
//
// @decision(DL-006)
func approvingUser(r *http.Request) (auth.Principal, bool) {
	p, ok := auth.From(r.Context())
	if !ok || p.UserID == "" || p.TokenID != "" {
		return auth.Principal{}, false
	}
	return p, true
}

// wrongCode counts the attempt and answers without saying whether the
// code exists.
//
// @decision(DL-006)
func (d *Device) wrongCode(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	d.limiter().fail(p.UserID, d.now())
	d.renderApproval(w, r, approvalPage{
		User:  p.Email,
		Error: "That code is not valid. Check it and try again — codes expire after 10 minutes.",
	})
}

// redirectToLogin sends an unauthenticated visitor through DL-007's
// login and back here.
//
// @decision(DL-006)
func (d *Device) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if d.Login == nil {
		http.Error(w, "this server has no login configured, so it cannot approve a CLI", http.StatusNotFound)
		return
	}
	next := r.URL.RequestURI()
	http.Redirect(w, r, LoginPath+"?next="+url.QueryEscape(next), http.StatusSeeOther)
}

// limiter returns the attempt limiter, creating it on first use.
func (d *Device) limiter() *attemptLimiter {
	d.limiterOnce.Do(func() { d.attempts = newAttemptLimiter() })
	return d.attempts
}

// attemptLimiter counts wrong codes per user, in-memory and therefore
// per replica.
//
// @decision(DL-006)
type attemptLimiter struct {
	mu   sync.Mutex
	byID map[string]*attemptRecord
}

type attemptRecord struct {
	count       int
	first       time.Time
	lockedUntil time.Time
	touched     time.Time
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{byID: map[string]*attemptRecord{}}
}

// lockedUntil reports how long a user is locked out for, if they are.
func (l *attemptLimiter) lockedUntil(id string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.byID[id]
	if !ok || now.After(rec.lockedUntil) {
		return 0, false
	}
	return rec.lockedUntil.Sub(now), true
}

// fail records a wrong code and locks the user out at the limit.
func (l *attemptLimiter) fail(id string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	rec, ok := l.byID[id]
	if !ok || now.Sub(rec.first) > AttemptWindow {
		rec = &attemptRecord{first: now}
		l.byID[id] = rec
	}
	rec.count++
	rec.touched = now
	if rec.count >= MaxCodeAttempts {
		rec.lockedUntil = now.Add(AttemptLockout)
		rec.count = 0
		rec.first = now
	}
}

// reset clears a user's attempts. Only a completed approval calls it.
func (l *attemptLimiter) reset(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byID, id)
}

// sweep drops records nobody has touched for attemptSweepIdle, so the
// map does not grow with every user who ever mistyped. Called under the
// lock.
//
// A record is only dropped once its lockout has also passed. That check
// is redundant while attemptSweepIdle exceeds AttemptLockout — a record
// idle for an hour cannot still be inside a fifteen-minute lockout —
// and it is kept so that shortening the sweep interval cannot silently
// hand a locked-out user a clean slate. TestSweepNeverLiftsALockout
// pins the relationship.
func (l *attemptLimiter) sweep(now time.Time) {
	for id, rec := range l.byID {
		if now.Sub(rec.touched) > attemptSweepIdle && now.After(rec.lockedUntil) {
			delete(l.byID, id)
		}
	}
}

// approvalPage is what the template renders.
type approvalPage struct {
	Code      string
	User      string
	TokenName string
	Error     string
	// Confirm shows the "this is what you are approving" step.
	Confirm bool
	// Done and Denied show the outcome.
	Done   bool
	Denied bool
}

func (d *Device) renderApproval(w http.ResponseWriter, _ *http.Request, page approvalPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	status := http.StatusOK
	if page.Error != "" {
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	if err := approvalTemplate.Execute(w, page); err != nil {
		d.warn("device: rendering the approval page: " + err.Error())
	}
}

// approvalTemplate is the whole approval UI. html/template escapes every
// field, which matters because TokenName comes from whatever called
// /device/code.
//
// @decision(DL-006)
var approvalTemplate = template.Must(template.New("approve").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>spov</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font:16px/1.5 system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;color:#111}
main{max-width:30rem;padding:2rem}h1{font-size:1.25rem}p{color:#444}
input[type=text]{font:inherit;font-family:ui-monospace,monospace;letter-spacing:.1em;padding:.5rem;width:12rem}
button{font:inherit;padding:.5rem 1rem;margin-right:.5rem}
.err{color:#a00}.what{background:#f4f4f4;padding:1rem;border-radius:.5rem}</style></head>
<body><main>
{{if .Done}}
  {{if .Denied}}<h1>Denied</h1><p>That sign-in was refused. Nothing was issued.</p>
  {{else}}<h1>Approved</h1><p>Your terminal should be signed in. You can close this page.</p>{{end}}
{{else if .Confirm}}
  <h1>Approve this sign-in?</h1>
  <div class="what">
    <p><strong>{{.TokenName}}</strong> is asking to act as <strong>{{.User}}</strong>.</p>
    <p>It will be able to do anything you can, until the token expires or you revoke it.</p>
  </div>
  <form method="post">
    <input type="hidden" name="code" value="{{.Code}}">
    <input type="hidden" name="confirm" value="1">
    <button type="submit" name="action" value="approve">Approve</button>
    <button type="submit" name="action" value="deny">Deny</button>
  </form>
{{else}}
  <h1>Sign in to the spov CLI</h1>
  <p>Enter the code your terminal is showing. Signed in as {{.User}}.</p>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <form method="post">
    <input type="text" name="code" value="{{.Code}}" placeholder="XXXX-XXXX"
           autocomplete="off" autocapitalize="characters" spellcheck="false" autofocus>
    <button type="submit">Continue</button>
  </form>
{{end}}
</main></body></html>
`))
