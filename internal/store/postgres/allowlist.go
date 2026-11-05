package postgres

import (
	"context"
	"strings"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

type pgAllowlist struct{ s *Store }

// normalizeAllowEmail lowercases and trims, which is what makes the
// (org_id, email) primary key a case-insensitive uniqueness constraint.
// It is applied to every argument, not only on write, so a caller may
// pass whatever case a person typed.
//
// @decision(DL-007)
func normalizeAllowEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (p pgAllowlist) List(ctx context.Context, orgID string) ([]store.AllowlistEntry, error) {
	rows, err := p.s.q.ListAllowlist(ctx, orgID)
	if err != nil {
		return nil, mapErr("list allowlist", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]store.AllowlistEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, allowlistFromRow(r))
	}
	return out, nil
}

func (p pgAllowlist) Get(ctx context.Context, orgID, email string) (store.AllowlistEntry, error) {
	row, err := p.s.q.GetAllowlistEntry(ctx, sqlcgen.GetAllowlistEntryParams{
		OrgID: orgID, Email: normalizeAllowEmail(email),
	})
	if err != nil {
		return store.AllowlistEntry{}, mapErr("get allowlist entry", err)
	}
	return allowlistFromRow(row), nil
}

func (p pgAllowlist) Put(ctx context.Context, e store.AllowlistEntry) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = p.s.now()
	}
	return mapErr("put allowlist entry", p.s.q.PutAllowlistEntry(ctx, sqlcgen.PutAllowlistEntryParams{
		OrgID:     e.OrgID,
		Email:     normalizeAllowEmail(e.Email),
		Admin:     e.Admin,
		CreatedAt: ts(e.CreatedAt),
	}))
}

func (p pgAllowlist) Delete(ctx context.Context, orgID, email string) error {
	n, err := p.s.q.DeleteAllowlistEntry(ctx, sqlcgen.DeleteAllowlistEntryParams{
		OrgID: orgID, Email: normalizeAllowEmail(email),
	})
	if err != nil {
		return mapErr("delete allowlist entry", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// CountAdmins counts the org's admin entries, through the partial index
// the migration adds for it. Inside a transaction it first takes the
// org's advisory lock, releasing with the transaction, so there is no
// unlock to leak on an error path. Outside a transaction the lock is
// skipped — the same shape as SessionStore.CountLive.
//
// @decision(DL-007) @decision(DL-009)
func (p pgAllowlist) CountAdmins(ctx context.Context, orgID string) (int, error) {
	if p.s.inTx {
		if err := p.s.q.LockAllowlist(ctx, orgID); err != nil {
			return 0, mapErr("lock allowlist", err)
		}
	}
	n, err := p.s.q.CountAllowlistAdmins(ctx, orgID)
	if err != nil {
		return 0, mapErr("count allowlist admins", err)
	}
	return int(n), nil
}

func allowlistFromRow(r sqlcgen.AllowlistEntry) store.AllowlistEntry {
	return store.AllowlistEntry{
		OrgID:     r.OrgID,
		Email:     r.Email,
		Admin:     r.Admin,
		CreatedAt: goTime(r.CreatedAt),
	}
}
