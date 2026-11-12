package cli

import (
	"path/filepath"

	"github.com/jimutt/spov/internal/addonindex"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/statefile"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

// templateStore is where the deployment keeps sandbox templates: the
// database for a server on Postgres, a file beside server.json
// otherwise, and memory when no state directory can be found.
//
// @decision(DL-119)
func (e *Env) templateStore(st store.Store) store.TemplateStore {
	if pg, ok := st.(*postgres.Store); ok {
		return pg.Templates()
	}
	dir, err := e.stateDir()
	if err != nil {
		return &store.FileTemplates{}
	}
	return &store.FileTemplates{Path: filepath.Join(dir, store.TemplatesName)}
}

// imageRegistry is where the deployment records its images: the
// database's registry for a server on Postgres, the state file beside
// server.json otherwise. file is that state file, or nil. A state
// directory that cannot be found gives no registry at all, and launches
// boot the variant their request names.
//
// @decision(DL-033) @decision(DL-016)
func (e *Env) imageRegistry(st store.Store, warn func(string)) (store.ImageStore, *statefile.File) {
	if pg, ok := st.(*postgres.Store); ok {
		return pg.Images(), nil
	}
	dir, err := e.stateDir()
	if err != nil {
		return nil, nil
	}
	f := &statefile.File{Path: filepath.Join(dir, statefile.Name), Warn: warn}
	return f, f
}

// addonResolver resolves partial add-on versions, keeping its answers in
// file when there is one.
//
// @decision(DL-033)
func (e *Env) addonResolver(file *statefile.File, warn func(string)) server.AddonResolver {
	if e.ResolveAddons != nil {
		return e.ResolveAddons
	}
	r := &addonindex.Resolver{Now: e.now, Warn: warn}
	if file != nil {
		r.Persist = file
	}
	return r
}

// modelSource is the deployment's model list, kept in file when there
// is one (a local server) and in memory otherwise.
//
// @decision(DL-013)
func (e *Env) modelSource(file *statefile.File, warn func(string)) server.ModelCatalogue {
	if e.Models != nil {
		return e.Models
	}
	src := &modelcat.Source{Now: e.now, Warn: warn}
	if file != nil {
		src.Persist = file
	}
	return src
}
