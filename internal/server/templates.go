package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/store"
)

const (
	// MaxTemplatesPerUser bounds the sandbox templates one user holds.
	MaxTemplatesPerUser = 32
	// MaxTemplateDescription bounds a template's description.
	MaxTemplateDescription = 200
)

// TemplateView is a sandbox template as the API carries it.
//
// @decision(DL-119)
type TemplateView struct {
	Name string `json:"name"`
	TemplateSettingsView
	// MissingCredentials are the routed credentials it lists that the
	// caller does not hold now; a launch from it is refused until they
	// are stored again or the template is changed.
	MissingCredentials []string  `json:"missing_credentials,omitempty" doc:"Listed routed credentials the caller does not hold now"`
	UpdatedAt          time.Time `json:"updated_at,omitzero"`
}

// TemplateSettingsView is what a template sets. A setting left out is
// not part of the template.
//
// @decision(DL-119)
type TemplateSettingsView struct {
	Description        string            `json:"description,omitempty" maxLength:"200"`
	Egress             string            `json:"egress,omitempty" doc:"Egress preset name"`
	EgressExtraDomains []string          `json:"egress_extra_domains,omitempty" maxItems:"16" doc:"Domains added to the preset; they need a preset, and not open"`
	Credentials        []string          `json:"credentials,omitempty" maxItems:"96" doc:"Attach exactly these routed credentials, asked-only ones included"`
	NoCredentials      bool              `json:"no_credentials,omitempty" doc:"Attach no routed credential"`
	Env                map[string]string `json:"env,omitempty" doc:"Environment variables, as name to value; not secret"`
	Addons             []string          `json:"addons,omitempty" maxItems:"16"`
	Apt                []string          `json:"apt,omitempty" maxItems:"32"`
	GitAccess          store.GitAccess   `json:"git_access,omitempty" enum:"repos,all"`
}

func templateView(t store.Template, missing []string) TemplateView {
	return TemplateView{Name: t.Name, TemplateSettingsView: TemplateSettingsView(t.TemplateSettings),
		MissingCredentials: missing, UpdatedAt: t.UpdatedAt}
}

// errInvalidTemplate marks a template the caller can fix, answered 422.
var errInvalidTemplate = errors.New("invalid sandbox template")

// checkTemplate normalises a template and checks each setting as a
// launch that used it would be checked. Whether the caller holds a
// credential it lists is a launch's question, not a write's.
//
// @decision(DL-119)
func (s *Service) checkTemplate(t store.Template) (store.Template, error) {
	bad := func(format string, a ...any) (store.Template, error) {
		return store.Template{}, fmt.Errorf("%w: %s", errInvalidTemplate, fmt.Sprintf(format, a...))
	}
	t.Description = strings.TrimSpace(t.Description)
	if !credproxy.ValidRouteName(t.Name) {
		return bad("%q cannot name a template: use lowercase letters, digits and -, starting with a letter, at most %d characters",
			t.Name, credproxy.MaxRouteName)
	}
	if t.Name == NoCredentials {
		return bad("%s is reserved", NoCredentials)
	}
	if len([]rune(t.Description)) > MaxTemplateDescription {
		return bad("the description is longer than %d characters", MaxTemplateDescription)
	}
	// An empty list is the setting left out, except for credentials,
	// where it could be read either way.
	if len(t.EgressExtraDomains) == 0 {
		t.EgressExtraDomains = nil
	}
	if len(t.Env) == 0 {
		t.Env = nil
	}
	if len(t.Addons) == 0 {
		t.Addons = nil
	}
	if len(t.Apt) == 0 {
		t.Apt = nil
	}
	switch {
	case t.Credentials != nil && len(t.Credentials) == 0:
		return bad("credentials is an empty list; attach none with no_credentials, or leave it out for the default")
	case t.NoCredentials && len(t.Credentials) > 0:
		return bad("credentials and no_credentials are both set")
	case len(t.EgressExtraDomains) > 0 && t.Egress == "":
		return bad("egress_extra_domains need an egress preset in the same template")
	}
	if err := checkRoutedRequest(store.RoutedRequest{Names: t.Credentials, None: t.NoCredentials}); err != nil {
		return bad("%s", err)
	}
	// The launch's own checks, on a request that holds only what the
	// template sets.
	probe := applyTemplate(store.SessionRequest{
		Name:  "template",
		Repos: []store.RepoRef{{Platform: "p", Owner: "o", Name: "n"}},
	}, t)
	if err := s.validateRequest(s.withDefaults(probe)); err != nil {
		return bad("%s", strings.TrimPrefix(err.Error(), ErrInvalidRequest.Error()+": "))
	}
	return t, nil
}

// templateFor reads the template a launch names. A name the caller does
// not hold refuses the launch.
//
// @decision(DL-120)
func (s *Service) templateFor(ctx context.Context, p auth.Principal, name string) (*store.Template, error) {
	if name == "" {
		return nil, nil
	}
	if s.deps.Templates == nil {
		return nil, fmt.Errorf("%w: this server keeps no sandbox templates, so it cannot launch from %s", ErrInvalidRequest, name)
	}
	t, err := s.deps.Templates.Get(ctx, p.OrgID, p.UserID, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: you hold no sandbox template named %s", ErrInvalidRequest, name)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// applyTemplate puts a template's settings under the request's own: a
// field the request sets wins, variables merge by name, and add-ons and
// apt are taken or left as one pair. It runs before the server's
// defaults, which would otherwise make every field look set.
//
// The routed list it leaves is never empty and never beside a
// narrowing: attachRoutes reads an empty list as the default, and
// ignores a narrowing beside a list.
//
// @decision(DL-120)
func applyTemplate(req store.SessionRequest, t store.Template) store.SessionRequest {
	req.Template = t.Name
	if req.Egress == "" && t.Egress != "" {
		req.Egress = t.Egress
		if len(req.EgressExtraDomains) == 0 {
			req.EgressExtraDomains = slices.Clone(t.EgressExtraDomains)
		}
	}
	if len(req.Addons) == 0 && len(req.Apt) == 0 {
		req.Addons, req.Apt = slices.Clone(t.Addons), slices.Clone(t.Apt)
	}
	if len(t.Env) > 0 {
		env := maps.Clone(t.Env)
		maps.Copy(env, req.Env)
		req.Env = env
	}
	// Under the request's own value and over a saved preference, and
	// itself a preference.
	//
	// @decision(DL-043)
	if t.GitAccess != "" && (req.GitAccess == "" || req.GitAccessPreferred) {
		req.GitAccess, req.GitAccessPreferred = t.GitAccess, true
	}
	r := &req.Routed
	// A nil list is the setting left out. An empty one, which no write
	// accepts, attaches none rather than the default.
	if r.None || len(r.Names) > 0 || (!t.NoCredentials && t.Credentials == nil) {
		return req
	}
	names := slices.Clone(t.Credentials)
	// The configuration's narrowing still only takes away.
	//
	// @decision(DL-075)
	if len(r.Preferred) > 0 {
		names = slices.DeleteFunc(names, func(n string) bool { return !slices.Contains(r.Preferred, n) })
	}
	r.Preferred = nil
	r.Names, r.None = names, len(names) == 0
	if r.None {
		r.Names = nil
	}
	return req
}

// inTemplate names the template in a refusal of a launch that used one.
//
// @decision(DL-120)
func inTemplate(t *store.Template, err error) error {
	if t == nil || err == nil || !errors.Is(err, ErrInvalidRequest) {
		return err
	}
	return fmt.Errorf("%w (the launch used sandbox template %s)", err, t.Name)
}

// missingCredentials is the names a template lists that are not among
// the ones the caller holds.
func missingCredentials(t store.Template, held []string) []string {
	var out []string
	for _, name := range t.Credentials {
		if !slices.Contains(held, name) {
			out = append(out, name)
		}
	}
	return out
}

// heldNames is the names of the routed credentials a launch of the
// caller's could attach, their own and the organisation's, read from the
// listings: no secret is opened to answer a listing.
func (s *Service) heldNames(ctx context.Context, orgID, userID string) ([]string, error) {
	var out []string
	if src := s.deps.RoutedCredentials; src != nil {
		views, err := src.List(ctx, orgID, userID)
		if err != nil {
			return nil, err
		}
		for _, v := range views {
			if !v.Missing {
				out = append(out, v.Route.Name)
			}
		}
	}
	if src := s.deps.OrgRoutedCredentials; src != nil {
		views, err := src.List(ctx, orgID)
		if err != nil {
			return nil, err
		}
		for _, v := range views {
			out = append(out, v.Route.Name)
		}
	}
	return out, nil
}

type templatesOutput struct {
	Body struct {
		Templates []TemplateView `json:"templates"`
	}
}

type templateInput struct {
	Name string `path:"name"`
}

type putTemplateInput struct {
	Name string `path:"name"`
	Body TemplateSettingsView
}

type templateOutput struct {
	Body TemplateView
}

// registerTemplates serves /me/templates, scoped to the calling
// principal.
//
// @decision(DL-119)
func (s *Server) registerTemplates() {
	templates := func() (store.TemplateStore, error) {
		if s.deps.Templates == nil {
			return nil, huma.Error501NotImplemented("this server keeps no sandbox templates")
		}
		return s.deps.Templates, nil
	}
	// held is the caller's routed credentials, or none when they cannot
	// be read: the listing then marks nothing missing.
	held := func(ctx context.Context, p auth.Principal) ([]string, bool) {
		names, err := s.svc.heldNames(ctx, p.OrgID, p.UserID)
		return names, err == nil
	}
	view := func(t store.Template, cands []string, known bool) TemplateView {
		if !known {
			return templateView(t, nil)
		}
		return templateView(t, missingCredentials(t, cands))
	}

	register(s.api, huma.Operation{
		OperationID: "list-templates", Method: http.MethodGet, Path: "/api/v1/me/templates",
		Summary: "The caller's sandbox templates", Tags: []string{"templates"},
	}, func(ctx context.Context, _ *struct{}) (*templatesOutput, error) {
		src, err := templates()
		if err != nil {
			return nil, err
		}
		p := principal(ctx)
		list, err := src.List(ctx, p.OrgID, p.UserID)
		if err != nil {
			return nil, toHumaError(err)
		}
		cands, known := held(ctx, p)
		out := &templatesOutput{}
		out.Body.Templates = make([]TemplateView, 0, len(list))
		for _, t := range list {
			out.Body.Templates = append(out.Body.Templates, view(t, cands, known))
		}
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "get-template", Method: http.MethodGet, Path: "/api/v1/me/templates/{name}",
		Summary: "One of the caller's sandbox templates", Tags: []string{"templates"},
	}, func(ctx context.Context, in *templateInput) (*templateOutput, error) {
		src, err := templates()
		if err != nil {
			return nil, err
		}
		p := principal(ctx)
		t, err := src.Get(ctx, p.OrgID, p.UserID, in.Name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("you hold no sandbox template named " + in.Name)
		}
		if err != nil {
			return nil, toHumaError(err)
		}
		cands, known := held(ctx, p)
		return &templateOutput{Body: view(t, cands, known)}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "put-template", Method: http.MethodPut, Path: "/api/v1/me/templates/{name}",
		Summary: "Create a sandbox template, or replace it whole", Tags: []string{"templates"},
	}, func(ctx context.Context, in *putTemplateInput) (*templateOutput, error) {
		src, err := templates()
		if err != nil {
			return nil, err
		}
		p := principal(ctx)
		t, err := s.svc.checkTemplate(store.Template{OrgID: p.OrgID, UserID: p.UserID, Name: in.Name,
			TemplateSettings: store.TemplateSettings(in.Body), UpdatedAt: s.deps.Now().UTC()})
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		list, err := src.List(ctx, p.OrgID, p.UserID)
		if err != nil {
			return nil, toHumaError(err)
		}
		replaces := slices.ContainsFunc(list, func(x store.Template) bool { return x.Name == t.Name })
		if !replaces && len(list) >= MaxTemplatesPerUser {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf(
				"you already hold %d sandbox templates, the most a user may", MaxTemplatesPerUser))
		}
		if err := src.Put(ctx, t); err != nil {
			return nil, toHumaError(err)
		}
		cands, known := held(ctx, p)
		return &templateOutput{Body: view(t, cands, known)}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "delete-template", Method: http.MethodDelete, Path: "/api/v1/me/templates/{name}",
		Summary: "Remove a sandbox template", Tags: []string{"templates"},
	}, func(ctx context.Context, in *templateInput) (*struct{}, error) {
		src, err := templates()
		if err != nil {
			return nil, err
		}
		p := principal(ctx)
		if _, err := src.Delete(ctx, p.OrgID, p.UserID, in.Name); err != nil {
			return nil, toHumaError(err)
		}
		return nil, nil
	})
}
