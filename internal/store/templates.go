package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Template is a sandbox template: a user's named set of sandbox settings
// that a launch may take. A setting left zero is not part of it. It
// holds no secret.
//
// @decision(DL-119)
type Template struct {
	OrgID  string `json:"-"`
	UserID string `json:"-"`
	Name   string `json:"name"`
	TemplateSettings
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// TemplateSettings is what a template stores under its name.
//
// @decision(DL-119)
type TemplateSettings struct {
	Description        string   `json:"description,omitempty"`
	Egress             string   `json:"egress,omitempty"`
	EgressExtraDomains []string `json:"egress_extra_domains,omitempty"`
	// Credentials names the routed credentials a launch attaches, and
	// NoCredentials attaches none. Neither set is the launch's default.
	Credentials   []string          `json:"credentials,omitempty"`
	NoCredentials bool              `json:"no_credentials,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Addons        []string          `json:"addons,omitempty"`
	Apt           []string          `json:"apt,omitempty"`
	GitAccess     GitAccess         `json:"git_access,omitempty"`
}

func (t Template) clone() Template {
	t.EgressExtraDomains = slices.Clone(t.EgressExtraDomains)
	t.Credentials = slices.Clone(t.Credentials)
	t.Env = maps.Clone(t.Env)
	t.Addons = slices.Clone(t.Addons)
	t.Apt = slices.Clone(t.Apt)
	return t
}

// TemplateStore keeps each user's sandbox templates: Postgres when
// hosted, a file beside the state file locally.
//
// @decision(DL-119)
type TemplateStore interface {
	// List is the user's templates, by name.
	List(ctx context.Context, orgID, userID string) ([]Template, error)
	// Get is one of them, or ErrNotFound.
	Get(ctx context.Context, orgID, userID, name string) (Template, error)
	// Put creates the template or replaces it whole, stamped at t.UpdatedAt.
	Put(ctx context.Context, t Template) error
	// Delete reports whether there was anything to remove.
	Delete(ctx context.Context, orgID, userID, name string) (bool, error)
}

// TemplatesName is the local server's template file.
const TemplatesName = "templates.json"

// FileTemplates is a TemplateStore in a JSON file, for a local server.
// With no Path it is held in memory, for tests.
//
// @decision(DL-119)
type FileTemplates struct {
	Path string

	mu  sync.Mutex
	mem []fileTemplate
}

type fileTemplate struct {
	OrgID  string `json:"org_id"`
	UserID string `json:"user_id"`
	Template
}

type templateFile struct {
	Version   int            `json:"version"`
	Templates []fileTemplate `json:"templates"`
}

func (f *FileTemplates) List(_ context.Context, orgID, userID string) ([]Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all, err := f.read()
	if err != nil {
		return nil, err
	}
	var out []Template
	for _, t := range all {
		if t.OrgID == orgID && t.UserID == userID {
			out = append(out, t.template())
		}
	}
	slices.SortFunc(out, func(a, b Template) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (f *FileTemplates) Get(_ context.Context, orgID, userID, name string) (Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all, err := f.read()
	if err != nil {
		return Template{}, err
	}
	if i := findTemplate(all, orgID, userID, name); i >= 0 {
		return all[i].template(), nil
	}
	return Template{}, ErrNotFound
}

func (f *FileTemplates) Put(_ context.Context, t Template) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	all, err := f.read()
	if err != nil {
		return err
	}
	row := fileTemplate{OrgID: t.OrgID, UserID: t.UserID, Template: t.clone()}
	if i := findTemplate(all, t.OrgID, t.UserID, t.Name); i >= 0 {
		all[i] = row
	} else {
		all = append(all, row)
	}
	return f.write(all)
}

func (f *FileTemplates) Delete(_ context.Context, orgID, userID, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all, err := f.read()
	if err != nil {
		return false, err
	}
	i := findTemplate(all, orgID, userID, name)
	if i < 0 {
		return false, nil
	}
	return true, f.write(slices.Delete(all, i, i+1))
}

func (t fileTemplate) template() Template {
	out := t.clone()
	out.OrgID, out.UserID = t.OrgID, t.UserID
	return out
}

func findTemplate(all []fileTemplate, orgID, userID, name string) int {
	return slices.IndexFunc(all, func(t fileTemplate) bool {
		return t.OrgID == orgID && t.UserID == userID && t.Name == name
	})
}

// read and write begin every error with "store: ", so an API answer
// gives the client a fixed message and the path goes to the log.
//
// @decision(DL-135)
func (f *FileTemplates) read() ([]fileTemplate, error) {
	if f.Path == "" {
		return slices.Clone(f.mem), nil
	}
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: sandbox templates: %w", err)
	}
	var file templateFile
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("store: sandbox templates: read %s: %w", f.Path, err)
	}
	if file.Version != 1 {
		return nil, fmt.Errorf("store: sandbox templates: %s has version %d, which this build does not read", f.Path, file.Version)
	}
	return file.Templates, nil
}

// write replaces the file through a rename, so a reader never sees half
// of one.
func (f *FileTemplates) write(all []fileTemplate) error {
	if f.Path == "" {
		f.mem = all
		return nil
	}
	if all == nil {
		all = []fileTemplate{}
	}
	b, err := json.MarshalIndent(templateFile{Version: 1, Templates: all}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return fmt.Errorf("store: sandbox templates: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), TemplatesName+".*")
	if err != nil {
		return fmt.Errorf("store: sandbox templates: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: sandbox templates: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: sandbox templates: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: sandbox templates: %w", err)
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return fmt.Errorf("store: sandbox templates: %w", err)
	}
	return nil
}
