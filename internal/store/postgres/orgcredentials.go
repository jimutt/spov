package postgres

import (
	"context"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// OrgCredentials implements store.Store.
//
// @decision(DL-042)
func (s *Store) OrgCredentials() store.OrgCredentialStore { return pgOrgCredentials{s} }

// AdminEvents implements store.Store.
//
// @decision(DL-042)
func (s *Store) AdminEvents() store.AdminEventStore { return pgAdminEvents{s} }

type pgOrgCredentials struct{ s *Store }

func (p pgOrgCredentials) Put(ctx context.Context, c store.OrgCredential) error {
	now := p.s.now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	meta := c.Meta
	if len(meta) == 0 {
		meta = []byte("{}")
	}
	return mapErr("put org credential", p.s.q.PutOrgCredential(ctx, sqlcgen.PutOrgCredentialParams{
		ID: c.ID, OrgID: c.OrgID, Platform: c.Platform,
		Ciphertext: c.Ciphertext, Nonce: c.Nonce, KeyVersion: int32(c.KeyVersion),
		Meta: meta, UpdatedBy: c.UpdatedBy, CreatedAt: ts(c.CreatedAt), UpdatedAt: ts(c.UpdatedAt),
	}))
}

func (p pgOrgCredentials) Get(ctx context.Context, orgID, platform string) (store.OrgCredential, error) {
	r, err := p.s.q.GetOrgCredential(ctx, sqlcgen.GetOrgCredentialParams{OrgID: orgID, Platform: platform})
	if err != nil {
		return store.OrgCredential{}, mapErr("get org credential", err)
	}
	return store.OrgCredential{
		ID: r.ID, OrgID: r.OrgID, Platform: r.Platform,
		Ciphertext: r.Ciphertext, Nonce: r.Nonce, KeyVersion: int(r.KeyVersion),
		Meta: r.Meta, UpdatedBy: r.UpdatedBy, CreatedAt: goTime(r.CreatedAt), UpdatedAt: goTime(r.UpdatedAt),
	}, nil
}

func (p pgOrgCredentials) List(ctx context.Context, orgID string) ([]store.OrgCredential, error) {
	rows, err := p.s.q.ListOrgCredentials(ctx, orgID)
	if err != nil {
		return nil, mapErr("list org credentials", err)
	}
	var out []store.OrgCredential
	for _, r := range rows {
		out = append(out, store.OrgCredential{
			ID: r.ID, OrgID: r.OrgID, Platform: r.Platform,
			Ciphertext: r.Ciphertext, Nonce: r.Nonce, KeyVersion: int(r.KeyVersion),
			Meta: r.Meta, UpdatedBy: r.UpdatedBy, CreatedAt: goTime(r.CreatedAt), UpdatedAt: goTime(r.UpdatedAt),
		})
	}
	return out, nil
}

func (p pgOrgCredentials) Delete(ctx context.Context, orgID, platform string) error {
	n, err := p.s.q.DeleteOrgCredential(ctx, sqlcgen.DeleteOrgCredentialParams{OrgID: orgID, Platform: platform})
	if err != nil {
		return mapErr("delete org credential", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

type pgAdminEvents struct{ s *Store }

func (p pgAdminEvents) Append(ctx context.Context, e store.AdminEvent) error {
	if e.At.IsZero() {
		e.At = p.s.now()
	}
	return mapErr("append admin event", p.s.q.AppendAdminEvent(ctx, sqlcgen.AppendAdminEventParams{
		ID: e.ID, OrgID: e.OrgID, Actor: e.Actor, Action: e.Action, Detail: e.Detail, At: ts(e.At),
	}))
}

func (p pgAdminEvents) List(ctx context.Context, orgID string, limit int) ([]store.AdminEvent, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := p.s.q.ListAdminEvents(ctx, sqlcgen.ListAdminEventsParams{OrgID: orgID, Limit: int32(limit)})
	if err != nil {
		return nil, mapErr("list admin events", err)
	}
	out := make([]store.AdminEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.AdminEvent{ID: r.ID, OrgID: r.OrgID, Actor: r.Actor, Action: r.Action, Detail: r.Detail, At: goTime(r.At)})
	}
	return out, nil
}
