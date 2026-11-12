package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

// Templates is the users' sandbox templates.
//
// @decision(DL-119)
func (s *Store) Templates() store.TemplateStore { return pgTemplates{s} }

type pgTemplates struct{ s *Store }

func templateFromRow(r sqlcgen.SandboxTemplate) (store.Template, error) {
	t := store.Template{OrgID: r.OrgID, UserID: r.UserID, Name: r.Name, UpdatedAt: goTime(r.UpdatedAt)}
	if err := json.Unmarshal(r.Settings, &t.TemplateSettings); err != nil {
		return store.Template{}, fmt.Errorf("store: decode sandbox template %s: %w", r.Name, err)
	}
	return t, nil
}

func (p pgTemplates) List(ctx context.Context, orgID, userID string) ([]store.Template, error) {
	rows, err := p.s.q.ListSandboxTemplates(ctx, sqlcgen.ListSandboxTemplatesParams{OrgID: orgID, UserID: userID})
	if err != nil {
		return nil, mapErr("list sandbox templates", err)
	}
	out := make([]store.Template, 0, len(rows))
	for _, r := range rows {
		t, err := templateFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (p pgTemplates) Get(ctx context.Context, orgID, userID, name string) (store.Template, error) {
	r, err := p.s.q.GetSandboxTemplate(ctx, sqlcgen.GetSandboxTemplateParams{OrgID: orgID, UserID: userID, Name: name})
	if err != nil {
		return store.Template{}, mapErr("get sandbox template", err)
	}
	return templateFromRow(r)
}

func (p pgTemplates) Put(ctx context.Context, t store.Template) error {
	settings, err := json.Marshal(t.TemplateSettings)
	if err != nil {
		return fmt.Errorf("store: encode sandbox template %s: %w", t.Name, err)
	}
	at := t.UpdatedAt
	if at.IsZero() {
		at = p.s.now()
	}
	return mapErr("put sandbox template", p.s.q.PutSandboxTemplate(ctx, sqlcgen.PutSandboxTemplateParams{
		OrgID: t.OrgID, UserID: t.UserID, Name: t.Name, Settings: settings, CreatedAt: ts(at),
	}))
}

func (p pgTemplates) Delete(ctx context.Context, orgID, userID, name string) (bool, error) {
	n, err := p.s.q.DeleteSandboxTemplate(ctx, sqlcgen.DeleteSandboxTemplateParams{OrgID: orgID, UserID: userID, Name: name})
	return n > 0, mapErr("delete sandbox template", err)
}
