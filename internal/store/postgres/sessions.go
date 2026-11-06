package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

type pgSessions struct{ s *Store }

func (p pgSessions) Create(ctx context.Context, sess store.Session) error {
	request, clones, credentials, err := sessionJSON(sess)
	if err != nil {
		return err
	}
	booted, err := bootedJSON(sess)
	if err != nil {
		return err
	}
	routed, err := routedJSON(sess)
	if err != nil {
		return err
	}
	now := p.s.now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	return mapErr("create session", p.s.q.CreateSession(ctx, sqlcgen.CreateSessionParams{
		ID:          sess.ID,
		OrgID:       sess.OrgID,
		OwnerID:     sess.OwnerID,
		Name:        sess.Name,
		State:       string(sess.State),
		SandboxID:   sess.SandboxID,
		Request:     request,
		Clones:      clones,
		Landing:     sess.Landing,
		Error:       sess.Error,
		CreatedAt:   ts(sess.CreatedAt),
		UpdatedAt:   ts(sess.UpdatedAt),
		StoppedAt:   tsPtr(sess.StoppedAt),
		Credentials: credentials,
		Booted:      booted,
		// @decision(DL-075)
		RoutedCredentials: routed,
	}))
}

// Lock reads the row with `for no key update`, which holds it against
// other writers of the row until the transaction ends, while inserts that
// only reference it (conversations, jobs) go ahead.
//
// @decision(DL-065)
func (p pgSessions) Lock(ctx context.Context, orgID, id string) error {
	if _, err := p.s.q.LockSession(ctx, sqlcgen.LockSessionParams{ID: id, OrgID: orgID}); err != nil {
		return mapErr("lock session", err)
	}
	return nil
}

func (p pgSessions) Get(ctx context.Context, orgID, ref string) (store.Session, error) {
	row, err := p.s.q.GetSessionByID(ctx, sqlcgen.GetSessionByIDParams{ID: ref, OrgID: orgID})
	if err == nil {
		return sessionFromRow(row)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.Session{}, mapErr("get session", err)
	}
	byName, err := p.s.q.GetLiveSessionByName(ctx, sqlcgen.GetLiveSessionByNameParams{OrgID: orgID, Name: ref})
	if err != nil {
		return store.Session{}, mapErr("get session by name", err)
	}
	return sessionFromRow(byName)
}

// List is hand-written pgx: sqlc cannot express an optional filter, and
// the alternative is a query per combination.
//
// @decision(DL-003) @decision(DL-012)
func (p pgSessions) List(ctx context.Context, orgID string, f store.SessionFilter) ([]store.Session, error) {
	var sb strings.Builder
	args := sessionWhere(&sb, "select *", orgID, f)
	if f.Before != "" {
		args = append(args, f.Before)
		fmt.Fprintf(&sb, " and id < $%d", len(args))
	}
	sb.WriteString(" order by id desc")
	if f.Limit > 0 {
		args = append(args, f.Limit)
		fmt.Fprintf(&sb, " limit $%d", len(args))
	}

	rows, err := p.s.db.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, mapErr("list sessions", err)
	}
	// Binding by name rather than position: a column added to the
	// migration and regenerated into sqlcgen.Session would otherwise be
	// a runtime scan mismatch that only the integration test catches.
	scanned, err := pgx.CollectRows(rows, pgx.RowToStructByName[sqlcgen.Session])
	if err != nil {
		return nil, mapErr("list sessions", err)
	}
	return sessionsFromRows(scanned)
}

// Count counts what List would return without its page.
//
// @decision(DL-012)
func (p pgSessions) Count(ctx context.Context, orgID string, f store.SessionFilter) (int, error) {
	var sb strings.Builder
	args := sessionWhere(&sb, "select count(*)", orgID, f)
	var n int
	if err := p.s.db.QueryRow(ctx, sb.String(), args...).Scan(&n); err != nil {
		return 0, mapErr("count sessions", err)
	}
	return n, nil
}

// sessionWhere writes the select and the filter's conditions, and
// returns the arguments they bind.
func sessionWhere(sb *strings.Builder, sel, orgID string, f store.SessionFilter) []any {
	sb.WriteString(sel + " from sessions where org_id = $1")
	args := []any{orgID}
	if f.OwnerID != "" {
		args = append(args, f.OwnerID)
		fmt.Fprintf(sb, " and owner_id = $%d", len(args))
	}
	if len(f.States) > 0 {
		states := make([]string, len(f.States))
		for i, st := range f.States {
			states[i] = string(st)
		}
		args = append(args, states)
		fmt.Fprintf(sb, " and state = any($%d)", len(args))
	}
	return args
}

func (p pgSessions) Update(ctx context.Context, sess store.Session) error {
	request, clones, credentials, err := sessionJSON(sess)
	if err != nil {
		return err
	}
	booted, err := bootedJSON(sess)
	if err != nil {
		return err
	}
	routed, err := routedJSON(sess)
	if err != nil {
		return err
	}
	n, err := p.s.q.UpdateSession(ctx, sqlcgen.UpdateSessionParams{
		ID:          sess.ID,
		OrgID:       sess.OrgID,
		OwnerID:     sess.OwnerID,
		Name:        sess.Name,
		State:       string(sess.State),
		SandboxID:   sess.SandboxID,
		Request:     request,
		Clones:      clones,
		Landing:     sess.Landing,
		Error:       sess.Error,
		UpdatedAt:   ts(p.s.now()),
		StoppedAt:   tsPtr(sess.StoppedAt),
		Credentials: credentials,
		Booted:      booted,
		// @decision(DL-075)
		RoutedCredentials: routed,
	})
	if err != nil {
		return mapErr("update session", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// CountLive counts a user's starting and running sessions, through the
// partial index the migration adds for it. Inside a transaction it
// first takes a transaction-scoped advisory lock on the owner, which
// makes a concurrency quota a limit rather than a suggestion; the lock
// releases with the transaction, so there is no unlock to leak on an
// error path. Outside a transaction it is skipped.
//
// @decision(DL-009) @decision(DL-003)
func (p pgSessions) CountLive(ctx context.Context, orgID, ownerID string) (int, error) {
	if p.s.inTx {
		if err := p.s.q.LockOwnerForQuota(ctx, sqlcgen.LockOwnerForQuotaParams{
			Hashtext: orgID, Hashtext_2: ownerID,
		}); err != nil {
			return 0, mapErr("lock owner for quota", err)
		}
	}
	n, err := p.s.q.CountLiveSessionsForOwner(ctx, sqlcgen.CountLiveSessionsForOwnerParams{
		OrgID: orgID, OwnerID: ownerID,
	})
	if err != nil {
		return 0, mapErr("count live sessions", err)
	}
	return int(n), nil
}

// RequestCancel records a cancel against a starting session. It is a
// conditional update rather than a read-then-write: a launch that
// commits between the two would otherwise have its running row marked
// cancelled by a delete that should have taken the terminate path.
//
// @decision(DL-003)
func (p pgSessions) RequestCancel(ctx context.Context, orgID, id string, at time.Time) (bool, error) {
	n, err := p.s.q.RequestSessionCancel(ctx, sqlcgen.RequestSessionCancelParams{
		ID:                id,
		OrgID:             orgID,
		CancelRequestedAt: tsPtr(&at),
	})
	if err != nil {
		return false, mapErr("request session cancel", err)
	}
	return n > 0, nil
}

// CancelRequested reports whether a cancel was recorded, taking the row
// lock so a caller inside a transaction serialises against the delete.
//
// @decision(DL-003)
func (p pgSessions) CancelRequested(ctx context.Context, orgID, id string) (bool, error) {
	got, err := p.s.q.SessionCancelRequested(ctx, sqlcgen.SessionCancelRequestedParams{ID: id, OrgID: orgID})
	if err != nil {
		return false, mapErr("session cancel requested", err)
	}
	return got, nil
}

func (p pgSessions) BySandbox(ctx context.Context, sandboxID string) (store.Session, error) {
	if sandboxID == "" {
		return store.Session{}, store.ErrNotFound
	}
	row, err := p.s.q.GetSessionBySandbox(ctx, sandboxID)
	if err != nil {
		return store.Session{}, mapErr("session by sandbox", err)
	}
	return sessionFromRow(row)
}

func (p pgSessions) ListLive(ctx context.Context) ([]store.Session, error) {
	rows, err := p.s.q.ListLiveSessions(ctx)
	if err != nil {
		return nil, mapErr("list live sessions", err)
	}
	return sessionsFromRows(rows)
}

// SetProxyCA keeps the session's sealed proxy CA, replacing an earlier
// one; a session that is not this org's is ErrNotFound.
//
// @decision(DL-034)
func (p pgSessions) SetProxyCA(ctx context.Context, orgID, id string, ca store.SealedValue) error {
	n, err := p.s.q.PutSessionProxyCA(ctx, sqlcgen.PutSessionProxyCAParams{
		ID: id, OrgID: orgID, Ciphertext: ca.Ciphertext, Nonce: ca.Nonce,
		KeyVersion: int32(ca.KeyVersion), CreatedAt: ts(p.s.now()),
	})
	if err != nil {
		return mapErr("set session proxy CA", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ProxyCA reads the session's sealed proxy CA back.
//
// @decision(DL-034)
func (p pgSessions) ProxyCA(ctx context.Context, orgID, id string) (store.SealedValue, error) {
	row, err := p.s.q.GetSessionProxyCA(ctx, sqlcgen.GetSessionProxyCAParams{SessionID: id, OrgID: orgID})
	if err != nil {
		return store.SealedValue{}, mapErr("get session proxy CA", err)
	}
	return store.SealedValue{Ciphertext: row.Ciphertext, Nonce: row.Nonce, KeyVersion: int(row.KeyVersion)}, nil
}

// SetGitToken records the session's current renewable git token. Update
// leaves these columns alone, so an older copy of the row cannot undo it.
//
// @decision(DL-021)
func (p pgSessions) SetGitToken(ctx context.Context, orgID, id string, minted, expires time.Time) error {
	n, err := p.s.q.SetSessionGitToken(ctx, sqlcgen.SetSessionGitTokenParams{
		ID:                id,
		OrgID:             orgID,
		GitTokenMintedAt:  tsPtr(&minted),
		GitTokenExpiresAt: tsPtr(&expires),
		UpdatedAt:         ts(p.s.now()),
	})
	if err != nil {
		return mapErr("set session git token", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetOpenCodeGrant records which OpenCode Console grant generation the
// session holds. Update leaves the column alone.
//
// @decision(DL-015)
func (p pgSessions) SetOpenCodeGrant(ctx context.Context, orgID, id string, generation int) error {
	n, err := p.s.q.SetSessionOpenCodeGrant(ctx, sqlcgen.SetSessionOpenCodeGrantParams{
		ID:            id,
		OrgID:         orgID,
		OpencodeGrant: int32(generation),
		UpdatedAt:     ts(p.s.now()),
	})
	if err != nil {
		return mapErr("set session opencode grant", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// MarkStopped moves a live session to a stopped state, if it still is.
//
// @decision(DL-011)
func (p pgSessions) MarkStopped(ctx context.Context, orgID, id string, state store.SessionState, at time.Time, errText string) (bool, error) {
	n, err := p.s.q.MarkSessionStopped(ctx, sqlcgen.MarkSessionStoppedParams{
		ID: id, OrgID: orgID, State: string(state), StoppedAt: ts(at), Error: errText, UpdatedAt: ts(p.s.now()),
	})
	if err != nil {
		return false, mapErr("stop session", err)
	}
	// A stopped session's proxy CA has nothing left to serve.
	//
	// @decision(DL-034)
	if n == 1 {
		if err := p.s.q.DeleteSessionProxyCA(ctx, sqlcgen.DeleteSessionProxyCAParams{SessionID: id, OrgID: orgID}); err != nil {
			return true, mapErr("forget session proxy CA", err)
		}
		// @decision(DL-085)
		if err := p.s.q.EndCurrentRun(ctx, sqlcgen.EndCurrentRunParams{SessionID: id, OrgID: orgID, EndedAt: ts(at)}); err != nil {
			return true, mapErr("end the session's run", err)
		}
	}
	return n == 1, nil
}
