package postgres

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// TestTimeRoundTrip: a zero time must become NULL rather than year zero,
// which is what distinguishes "never stopped" from "stopped at the epoch".
//
// @decision(DL-003)
func TestTimeRoundTrip(t *testing.T) {
	if got := ts(time.Time{}); got.Valid {
		t.Error("a zero time became a value rather than NULL")
	}
	if got := tsPtr(nil); got.Valid {
		t.Error("a nil pointer became a value rather than NULL")
	}
	if got := goTime(pgtype.Timestamptz{}); !got.IsZero() {
		t.Errorf("NULL became %v", got)
	}
	if got := goTimePtr(pgtype.Timestamptz{}); got != nil {
		t.Errorf("NULL became %v", got)
	}

	when := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	if got := goTime(ts(when)); !got.Equal(when) {
		t.Errorf("round trip gave %v, want %v", got, when)
	}
	if got := goTimePtr(tsPtr(&when)); got == nil || !got.Equal(when) {
		t.Errorf("pointer round trip gave %v", got)
	}

	// Times come back as UTC whatever the server's zone, so comparisons
	// in the rest of the code do not depend on it.
	east := time.FixedZone("east", 5*3600)
	local := when.In(east)
	if got := goTime(ts(local)); got.Location() != time.UTC || !got.Equal(when) {
		t.Errorf("got %v in %v", got, got.Location())
	}
}

// TestSessionJSONEncodesEmptyClonesAsAnArray: the column is NOT NULL, so
// a nil slice must still be valid JSON.
//
// @decision(DL-003)
func TestSessionJSONEncodesEmptyClonesAsAnArray(t *testing.T) {
	_, clones, credentials, err := sessionJSON(store.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if string(clones) != "[]" {
		t.Errorf("nil clones encoded as %q, want []", clones)
	}
	if string(credentials) != "[]" {
		t.Errorf("nil credentials encoded as %q, want []", credentials)
	}
	_, clones, _, err = sessionJSON(store.Session{Clones: []string{"o/r"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(clones) != `["o/r"]` {
		t.Errorf("clones encoded as %q", clones)
	}
}

// TestSessionRoundTripsThroughItsRow covers the jsonb columns, which are
// the only place a session's shape can be lost.
//
// @decision(DL-003)
func TestSessionRoundTripsThroughItsRow(t *testing.T) {
	stopped := time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC)
	in := store.Session{
		ID: "s1", OrgID: "org", OwnerID: "u1", Name: "alpha",
		State: store.StateRunning, SandboxID: "sb-1",
		Request: store.SessionRequest{
			Name:    "alpha",
			Repos:   []store.RepoRef{{Platform: "github", Owner: "o", Name: "r"}},
			Sandbox: store.SandboxConfig{CPU: 2, MemoryMiB: 4096, Timeout: 2 * time.Hour, Regions: []string{"us-east"}},
			Egress:  "agent",
		},
		Clones:    []string{"o/r"},
		Landing:   "/root/r",
		Error:     "",
		StoppedAt: &stopped,
		// @decision(DL-043)
		Credentials: []store.SessionCredential{{Platform: "github", Kind: store.CredentialRenewed}},
		// @decision(DL-021)
		GitTokenMintedAt:  &stopped,
		GitTokenExpiresAt: &stopped,
	}
	request, clones, credentials, err := sessionJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	row := sqlcgen.Session{
		ID: in.ID, OrgID: in.OrgID, OwnerID: in.OwnerID, Name: in.Name,
		State: string(in.State), SandboxID: in.SandboxID,
		Request: request, Clones: clones, Landing: in.Landing, Error: in.Error,
		CreatedAt: ts(time.Now()), UpdatedAt: ts(time.Now()), StoppedAt: tsPtr(in.StoppedAt),
		Credentials:      credentials,
		GitTokenMintedAt: tsPtr(in.GitTokenMintedAt), GitTokenExpiresAt: tsPtr(in.GitTokenExpiresAt),
	}
	out, err := sessionFromRow(row)
	if err != nil {
		t.Fatal(err)
	}
	if out.Request.Sandbox.Timeout != 2*time.Hour {
		t.Errorf("timeout survived as %v", out.Request.Sandbox.Timeout)
	}
	if len(out.Request.Repos) != 1 || out.Request.Repos[0].Owner != "o" {
		t.Errorf("repos = %+v", out.Request.Repos)
	}
	if len(out.Clones) != 1 || out.Clones[0] != "o/r" {
		t.Errorf("clones = %v", out.Clones)
	}
	if out.Request.Egress != "agent" || out.Request.Sandbox.Regions[0] != "us-east" {
		t.Errorf("got %+v", out.Request)
	}
	if out.StoppedAt == nil || !out.StoppedAt.Equal(stopped) {
		t.Errorf("stopped at %v", out.StoppedAt)
	}
	if len(out.Credentials) != 1 || out.Credentials[0] != in.Credentials[0] {
		t.Errorf("credentials = %+v", out.Credentials)
	}
	if out.GitTokenMintedAt == nil || out.GitTokenExpiresAt == nil || !out.GitTokenExpiresAt.Equal(stopped) {
		t.Errorf("git token times = %v, %v", out.GitTokenMintedAt, out.GitTokenExpiresAt)
	}
}

// TestSessionFromRowRejectsCorruptJSON: a bad row must be an error, not a
// silently empty session.
//
// @decision(DL-003)
func TestSessionFromRowRejectsCorruptJSON(t *testing.T) {
	if _, err := sessionFromRow(sqlcgen.Session{ID: "s1", Request: []byte("{not json")}); err == nil {
		t.Error("corrupt request json was accepted")
	}
	if _, err := sessionFromRow(sqlcgen.Session{ID: "s1", Clones: []byte("{not json")}); err == nil {
		t.Error("corrupt clones json was accepted")
	}
	if _, err := sessionsFromRows([]sqlcgen.Session{{ID: "s1", Request: []byte("nope")}}); err == nil {
		t.Error("a corrupt row in a listing was accepted")
	}
	if rows, err := sessionsFromRows(nil); err != nil || rows != nil {
		t.Errorf("empty listing = %v, %v", rows, err)
	}
}

// TestMapErrTranslatesPostgresCodes: the store's callers branch on
// ErrNotFound and ErrConflict, so the translation is load-bearing.
//
// @decision(DL-003)
func TestMapErrTranslatesPostgresCodes(t *testing.T) {
	if err := mapErr("op", nil); err != nil {
		t.Errorf("nil became %v", err)
	}
	if err := mapErr("op", pgx.ErrNoRows); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no rows became %v", err)
	}
	if err := mapErr("op", &pgconn.PgError{Code: "23505"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("unique violation became %v", err)
	}
	fk := mapErr("op", &pgconn.PgError{Code: "23503", ConstraintName: "sessions_org_id_fkey"})
	if !errors.Is(fk, store.ErrNotFound) {
		t.Errorf("foreign key violation became %v", fk)
	}
	// The constraint name is what tells a missing org from a missing
	// session; without it the failure reads as "no such session".
	if !strings.Contains(fk.Error(), "sessions_org_id_fkey") {
		t.Errorf("the foreign key error does not name the constraint: %v", fk)
	}
	// Anything else keeps its cause and says what failed.
	other := &pgconn.PgError{Code: "42P01", Message: "no such table"}
	err := mapErr("list sessions", other)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		t.Errorf("an unrelated error was translated: %v", err)
	}
	if !errors.Is(err, other) {
		t.Errorf("the cause was lost: %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "list sessions") {
		t.Errorf("error %q does not say what failed", got)
	}
}

// TestRowsConvertToTheirStoreTypes covers the remaining converters, which
// are where a renamed column silently becomes a zero value.
//
// @decision(DL-003)
func TestRowsConvertToTheirStoreTypes(t *testing.T) {
	now := time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)

	m := mintFromRow(sqlcgen.Mint{ID: "m1", OrgID: "org", SessionID: "s1", UserID: "u1",
		Port: 4096, ExpiresAt: ts(later), CreatedAt: ts(now)})
	if m.Port != 4096 || !m.ExpiresAt.Equal(later) || !m.CreatedAt.Equal(now) {
		t.Errorf("mint = %+v", m)
	}

	u := userFromRow(sqlcgen.User{ID: "u1", OrgID: "org", Email: "a@example.com", Name: "Ada",
		GithubLogin: "ada", Admin: true, GitName: "Ada", GitEmail: "ada@example.com",
		DefaultModel: "anthropic/claude-sonnet-5",
		LastLoginAt:  ts(now), CreatedAt: ts(now), UpdatedAt: ts(later)})
	if !u.Admin || u.GithubLogin != "ada" || u.GitEmail != "ada@example.com" {
		t.Errorf("user = %+v", u)
	}
	// @decision(DL-014)
	if u.DefaultModel != "anthropic/claude-sonnet-5" {
		t.Errorf("the default model did not survive the row: %+v", u)
	}
	if u.LastLoginAt == nil || !u.LastLoginAt.Equal(now) {
		t.Errorf("last login = %v", u.LastLoginAt)
	}

	tok := tokenFromRow(sqlcgen.ApiToken{ID: "t1", OrgID: "org", UserID: "u1",
		TokenHash: []byte("h"), Name: "laptop", ExpiresAt: ts(later), CreatedAt: ts(now)})
	if string(tok.Hash) != "h" || tok.Name != "laptop" || tok.RevokedAt != nil {
		t.Errorf("token = %+v", tok)
	}
	if !tok.Usable(now) || tok.Usable(later.Add(time.Second)) {
		t.Error("Usable disagrees with the expiry")
	}

	d := deviceCodeFromRow(sqlcgen.DeviceCode{ID: "d1", OrgID: "org", DeviceCodeHash: []byte("dc"),
		UserCode: "BCDF-GHJK", Status: "approved", ExpiresAt: ts(later), IntervalS: 10, CreatedAt: ts(now)})
	if d.Status != store.DeviceApproved || d.UserCode != "BCDF-GHJK" || d.IntervalSeconds != 10 {
		t.Errorf("device code = %+v", d)
	}
	if d.LastPolledAt != nil {
		t.Errorf("last polled = %v, want nil", d.LastPolledAt)
	}

	c := credentialFromRow(sqlcgen.UserCredential{ID: "c1", OrgID: "org", UserID: "u1",
		Platform: "github", Ciphertext: []byte("x"), Nonce: []byte("n"), KeyVersion: 2,
		VerifiedAs: "ada", CreatedAt: ts(now)})
	if c.KeyVersion != 2 || string(c.Ciphertext) != "x" || c.VerifiedAs != "ada" {
		t.Errorf("credential = %+v", c)
	}
}

// TestRevokedTokenIsNeverUsable: revocation must win over an expiry that
// is still in the future.
//
// @decision(DL-006)
func TestRevokedTokenIsNeverUsable(t *testing.T) {
	now := time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	tok := store.APIToken{ExpiresAt: &future, RevokedAt: &now}
	if tok.Usable(now) {
		t.Error("a revoked token with a future expiry is usable")
	}
	// A token with no expiry at all never expires.
	forever := store.APIToken{}
	if !forever.Usable(now.Add(1000 * time.Hour)) {
		t.Error("a token with no expiry expired")
	}
}

// TestSessionsFromRowsConvertsEveryRow.
//
// @decision(DL-003)
func TestSessionsFromRowsConvertsEveryRow(t *testing.T) {
	rows := []sqlcgen.Session{
		{ID: "s1", OrgID: "org", State: "running", Request: []byte(`{"Name":"a"}`), Clones: []byte(`["o/r"]`)},
		{ID: "s2", OrgID: "org", State: "stopped"},
	}
	out, err := sessionsFromRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Request.Name != "a" || out[1].State != store.StateStopped {
		t.Errorf("got %+v", out)
	}
	// An empty jsonb column is a valid session with no request, not an
	// error: the columns default to '{}' and '[]'.
	if len(out[1].Clones) != 0 {
		t.Errorf("clones = %v, want none", out[1].Clones)
	}
}

// TestSessionJSONEncodesTheWholeRequest guards against a new field being
// added to SessionRequest and silently not persisting.
//
// @decision(DL-003)
func TestSessionJSONEncodesTheWholeRequest(t *testing.T) {
	in := store.Session{Request: store.SessionRequest{
		Name: "a", Landing: "/root", Image: "img", Egress: "agent",
		GitName: "Ada", GitEmail: "ada@example.com",
		Model: "anthropic/claude-sonnet-5",
		Sync:  &store.SessionSync{Branch: "feat/sync", BaseCommit: strings.Repeat("f", 40)},
	}}
	request, _, _, err := sessionJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Ada", "ada@example.com", "agent", "img", "/root",
		"anthropic/claude-sonnet-5", "feat/sync",
	} {
		if !strings.Contains(string(request), want) {
			t.Errorf("encoded request %q is missing %q", request, want)
		}
	}
}

// @decision(DL-017)
func TestConversationRowsConvert(t *testing.T) {
	at := pgtype.Timestamptz{Time: time.Date(2026, 9, 2, 8, 0, 0, 0, time.FixedZone("x", 3600)), Valid: true}

	conv := conversationFromRow(sqlcgen.Conversation{ID: "c1", OrgID: "org", SessionID: "s1", OpencodeID: "ses_1", Title: "t", CreatedAt: at, UpdatedAt: at})
	if conv.OpenCodeID != "ses_1" || conv.Title != "t" || conv.CreatedAt.Location() != time.UTC {
		t.Errorf("conversation = %+v", conv)
	}

	msg := messageFromRow(sqlcgen.Message{ID: "m1", OrgID: "org", SessionID: "s1", ConversationID: "c1", OpencodeID: "msg_1", Role: "user", Text: "hi", CreatedAt: at})
	if msg.OpenCodeID != "msg_1" || !msg.At.IsZero() || msg.CreatedAt.IsZero() {
		t.Errorf("a message with no time should have a zero At and a set CreatedAt: %+v", msg)
	}

	conv1 := "c1"
	seq7 := int64(7)
	ev := eventFromRow(sqlcgen.Event{ID: "e1", OrgID: "org", SessionID: "s1", ConversationID: &conv1, Source: "opencode", Seq: &seq7, Pos: 12, Name: "n", Raw: []byte(`{}`), At: at, CreatedAt: at})
	if ev.ConversationID != "c1" || ev.Source != store.SourceOpenCode || ev.Seq != 7 || ev.Pos != 12 || string(ev.Raw) != `{}` {
		t.Errorf("event = %+v", ev)
	}
	mark := eventFromRow(sqlcgen.Event{ID: "e2", Source: "spov", Name: "spov.recorder.gap", At: at, CreatedAt: at})
	if mark.ConversationID != "" || mark.Source != store.SourceSpov {
		t.Errorf("a marker's conversation should be empty: %+v", mark)
	}
	// A null seq is a row that has none, and the model says so with 0.
	//
	// @decision(DL-017)
	if mark.Seq != 0 {
		t.Errorf("a null seq became %d, not 0", mark.Seq)
	}
	eph := eventFromRow(sqlcgen.Event{ID: "e3", OrgID: "org", SessionID: "s1", ConversationID: &conv1,
		Source: "ephemeral", Name: "permission.asked", Raw: []byte(`{"type":"permission.asked"}`), At: at, CreatedAt: at})
	if eph.Source != store.SourceEphemeral || eph.Seq != 0 || eph.ConversationID != "c1" {
		t.Errorf("ephemeral event = %+v", eph)
	}
	// The message's frame round-trips, which is what gate 3 will
	// re-project tool parts out of.
	withRaw := messageFromRow(sqlcgen.Message{ID: "m2", ConversationID: "c1", OpencodeID: "msg_2",
		Raw: []byte(`{"parts":[]}`), CreatedAt: at})
	if string(withRaw.Raw) != `{"parts":[]}` {
		t.Errorf("message raw = %q", withRaw.Raw)
	}
}

// @decision(DL-045)
func TestJobRowsConvert(t *testing.T) {
	at := pgtype.Timestamptz{Time: time.Date(2026, 9, 3, 8, 0, 0, 0, time.FixedZone("x", 3600)), Valid: true}
	started := pgtype.Timestamptz{Time: time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC), Valid: true}

	j := jobFromRow(sqlcgen.Job{
		ID: "j1", OrgID: "org", OwnerID: "u1", SessionID: "s1",
		Kind: "pr", State: "running", Goal: "fix the build", Error: "",
		Attempt: 2, CreatedAt: at, UpdatedAt: at, StartedAt: started,
	})
	if j.Kind != store.JobKindPR || j.State != store.JobRunning || j.Attempt != 2 {
		t.Errorf("job = %+v", j)
	}
	if j.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt is not UTC: %v", j.CreatedAt)
	}
	// A running job has started and not stopped, and the nullable
	// columns must come back as nil rather than as a zero time — the
	// difference is what "has not happened yet" means to the caller.
	if j.StartedAt == nil || !j.StartedAt.Equal(started.Time) {
		t.Errorf("StartedAt = %v", j.StartedAt)
	}
	if j.StoppedAt != nil {
		t.Errorf("StoppedAt = %v, want nil for a running job", j.StoppedAt)
	}

	if got := jobsFromRows(nil); got != nil {
		t.Errorf("no rows should convert to a nil slice, got %+v", got)
	}
	rows := jobsFromRows([]sqlcgen.Job{
		{ID: "a", Kind: "pr", State: "queued", CreatedAt: at, UpdatedAt: at},
		{ID: "b", Kind: "report", State: "succeeded", CreatedAt: at, UpdatedAt: at},
	})
	if len(rows) != 2 || rows[0].ID != "a" || rows[1].Kind != store.JobKindReport {
		t.Errorf("rows = %+v", rows)
	}
}
