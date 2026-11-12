package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// RunTemplates is the sandbox template store's contract. org must be an
// organisation the store accepts rows for.
//
// @decision(DL-119)
func RunTemplates(t *testing.T, org string, factory func(*testing.T) store.TemplateStore) {
	ctx := context.Background()
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	full := store.TemplateSettings{
		Description: "the backend", Egress: "install", EgressExtraDomains: []string{"pkg.acme.com"},
		Credentials: []string{"acme-npm"}, Env: map[string]string{"STAGE": "dev"},
		Addons: []string{"rust"}, Apt: []string{"jq"}, GitAccess: store.GitAccessAll,
	}

	t.Run("a template is written whole and read back", func(t *testing.T) {
		st := factory(t)
		if err := st.Put(ctx, store.Template{OrgID: org, UserID: "me", Name: "acme", TemplateSettings: full, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
		got, err := st.Get(ctx, org, "me", "acme")
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "acme" || got.OrgID != org || got.UserID != "me" || !got.UpdatedAt.Equal(at) ||
			got.Egress != "install" || got.Env["STAGE"] != "dev" || len(got.Credentials) != 1 || got.GitAccess != store.GitAccessAll ||
			got.Description != "the backend" || len(got.Addons) != 1 || len(got.Apt) != 1 || len(got.EgressExtraDomains) != 1 {
			t.Errorf("read back %+v", got)
		}
		// A put replaces every setting, never merges.
		later := at.Add(time.Hour)
		if err := st.Put(ctx, store.Template{OrgID: org, UserID: "me", Name: "acme",
			TemplateSettings: store.TemplateSettings{NoCredentials: true}, UpdatedAt: later}); err != nil {
			t.Fatal(err)
		}
		got, err = st.Get(ctx, org, "me", "acme")
		if err != nil || got.Egress != "" || got.Env != nil || got.Credentials != nil || !got.NoCredentials || !got.UpdatedAt.Equal(later) {
			t.Errorf("after a replace %+v, %v", got, err)
		}
	})

	t.Run("a user's templates are listed by name and are theirs alone", func(t *testing.T) {
		st := factory(t)
		for _, name := range []string{"b", "a-z", "a"} {
			if err := st.Put(ctx, store.Template{OrgID: org, UserID: "me", Name: name, UpdatedAt: at}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Put(ctx, store.Template{OrgID: org, UserID: "you", Name: "yours", UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
		list, err := st.List(ctx, org, "me")
		if err != nil || len(list) != 3 || list[0].Name != "a" || list[1].Name != "a-z" || list[2].Name != "b" {
			t.Errorf("list %+v, %v", list, err)
		}
		if _, err := st.Get(ctx, org, "me", "yours"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user's template: %v", err)
		}
		if removed, err := st.Delete(ctx, org, "me", "yours"); removed || err != nil {
			t.Errorf("removing another user's: %v, %v", removed, err)
		}
		if removed, err := st.Delete(ctx, org, "you", "yours"); !removed || err != nil {
			t.Errorf("removing one's own: %v, %v", removed, err)
		}
		if removed, err := st.Delete(ctx, org, "you", "yours"); removed || err != nil {
			t.Errorf("removing it again: %v, %v", removed, err)
		}
		if list, err := st.List(ctx, org, "you"); err != nil || len(list) != 0 {
			t.Errorf("an emptied list %+v, %v", list, err)
		}
	})
}
