package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
)

// templateFlags are --template and --no-template on a launch.
//
// @decision(DL-122)
type templateFlags struct {
	Name string
	None bool
}

func addTemplateFlags(env *Env, cmd *cobra.Command, t *templateFlags, key string) {
	cmd.Flags().StringVar(&t.Name, "template", "",
		"launch from one of your sandbox templates (see `spov templates`): its egress, credentials, variables, add-ons and"+
			" git access apply, under this command's flags and in place of your config files' settings; "+key+" when unset")
	cmd.Flags().BoolVar(&t.None, "no-template", false, "launch without the template "+key+" names")
	cmd.MarkFlagsMutuallyExclusive("template", "no-template")
	_ = cmd.RegisterFlagCompletionFunc("template", completeTemplates(env))
}

// templateFor is the template a launch names: the flag's, else the user
// file's key. configured says it came from the key.
//
// @decision(DL-122)
func templateFor(cfg config.Config, t templateFlags, job bool) (name, key string, configured bool) {
	key, fromConfig := "sandbox.template", cfg.Sandbox.Template
	if job {
		key, fromConfig = "sandbox.job_template", cfg.Sandbox.JobTemplate
	}
	switch {
	case t.None:
		return "", key, false
	case t.Name != "":
		return t.Name, key, false
	}
	return fromConfig, key, fromConfig != ""
}

// underTemplate takes the config files' values for the settings a
// template covers out of cfg, so that only a flag overrides the
// template, and says once what it replaced. The credential narrowing
// stays: it only takes away.
//
// @decision(DL-122)
func underTemplate(env *Env, cfg *config.Config, name string, job bool) {
	sb := &cfg.Sandbox
	access, accessKey := &sb.GitAccess, "sandbox.git_access"
	if job {
		access, accessKey = &sb.JobGitAccess, "sandbox.job_git_access"
	}
	var replaced []string
	drop := func(set bool, what string) {
		if set {
			replaced = append(replaced, what)
		}
	}
	drop(sb.Egress != "" && sb.Egress != config.Defaults().Sandbox.Egress, "sandbox.egress")
	drop(len(sb.EgressExtraDomains) > 0, "sandbox.egress_extra_domains")
	drop(len(sb.Addons) > 0, "sandbox.addons")
	drop(len(sb.Apt) > 0, "sandbox.apt")
	drop(len(sb.Env) > 0, "sandbox.env")
	drop(*access != "", accessKey)
	sb.Egress, sb.EgressExtraDomains, sb.Addons, sb.Apt, sb.Env = "", nil, nil, nil, nil
	*access = ""
	if len(replaced) > 0 {
		env.deferWarning(fmt.Sprintf("note: the template %s takes the place of %s from your configuration for this launch",
			name, strings.Join(replaced, ", ")))
	}
}

// checkConfiguredTemplate refuses a template the config names and the
// server no longer holds, naming the key.
//
// @decision(DL-122)
func checkConfiguredTemplate(ctx context.Context, c *client.Client, name, key string) error {
	_, err := c.GetTemplate(ctx, name)
	switch {
	case client.IsNotFound(err):
		return fmt.Errorf("%s names the sandbox template %s, which you do not hold: change the key, or pass --no-template", key, name)
	case err != nil:
		return fmt.Errorf("%s names the sandbox template %s: %w; pass --no-template to launch without it", key, name, templateError(err))
	}
	return nil
}

// newTemplatesCmd is `spov templates`: the caller's sandbox templates,
// which are made in the web app's Settings.
//
// @decision(DL-122) @decision(DL-023)
func newTemplatesCmd(env *Env) *cobra.Command {
	var jsonOut bool
	list := func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd.Context())
		if err != nil {
			return err
		}
		views, err := c.ListTemplates(cmd.Context())
		if err != nil {
			return templateError(err)
		}
		if jsonOut {
			return writeJSON(env.Out, struct {
				Templates []server.TemplateView `json:"templates"`
			}{orEmpty(views)})
		}
		if len(views) == 0 {
			_, err := fmt.Fprintln(env.Err, "no sandbox templates; make one in the web app's Settings")
			return err
		}
		w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tSETS\tDESCRIPTION")
		for _, v := range views {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", v.Name, templateSummary(v), v.Description)
		}
		return w.Flush()
	}
	cmd := &cobra.Command{
		Use:   "templates",
		Short: "List your sandbox templates",
		Long: "A sandbox template is a named set of sandbox settings (egress, routed credentials, variables, add-ons," +
			" git access) that a launch takes with --template. Templates are made and changed in the web app's Settings.",
		Args: cobra.NoArgs,
		RunE: list,
	}
	listCmd := &cobra.Command{Use: "list", Short: "List your sandbox templates", Args: cobra.NoArgs, RunE: list}
	for _, c := range []*cobra.Command{cmd, listCmd} {
		c.Flags().BoolVar(&jsonOut, "json", false, "print the templates as JSON")
	}
	var showJSON bool
	show := &cobra.Command{
		Use:               "show <name>",
		Short:             "Show what a sandbox template sets",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTemplates(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			v, err := c.GetTemplate(cmd.Context(), args[0])
			if err != nil {
				return templateError(err)
			}
			if showJSON {
				return writeJSON(env.Out, v)
			}
			return printTemplate(env, v)
		},
	}
	show.Flags().BoolVar(&showJSON, "json", false, "print the template as JSON")
	remove := &cobra.Command{
		Use:               "remove <name>",
		Short:             "Remove a sandbox template",
		Long:              "Remove a sandbox template. Sessions launched from it keep the settings they were given.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeTemplates(env),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			if _, err := c.GetTemplate(cmd.Context(), args[0]); err != nil {
				return templateError(err)
			}
			if err := c.DeleteTemplate(cmd.Context(), args[0]); err != nil {
				return templateError(err)
			}
			_, err = fmt.Fprintf(env.Out, "removed the sandbox template %s\n", args[0])
			return err
		},
	}
	cmd.AddCommand(listCmd, show, remove)
	return cmd
}

// templateError words a server that keeps no templates.
func templateError(err error) error {
	if client.StatusOf(err) == 501 {
		return errors.New("this server keeps no sandbox templates")
	}
	return err
}

// templateSummary is one line of what a template sets.
func templateSummary(v server.TemplateView) string {
	var parts []string
	if v.Egress != "" {
		s := "egress " + v.Egress
		if n := len(v.EgressExtraDomains); n > 0 {
			s += fmt.Sprintf(" +%d", n)
		}
		parts = append(parts, s)
	}
	switch {
	case v.NoCredentials:
		parts = append(parts, "no credentials")
	case len(v.Credentials) > 0:
		parts = append(parts, "credentials "+strings.Join(v.Credentials, ","))
	}
	if n := len(v.Env); n > 0 {
		parts = append(parts, fmt.Sprintf("%d variables", n))
	}
	if pkgs := append(slices.Clone(v.Addons), v.Apt...); len(pkgs) > 0 {
		parts = append(parts, strings.Join(pkgs, ","))
	}
	if v.GitAccess != "" {
		parts = append(parts, "git access "+string(v.GitAccess))
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, "; ")
}

func printTemplate(env *Env, v server.TemplateView) error {
	w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	row := func(k, val string) {
		if val != "" {
			_, _ = fmt.Fprintf(w, "%s\t%s\n", k, val)
		}
	}
	row("name", v.Name)
	row("description", v.Description)
	row("egress", v.Egress)
	row("extra domains", strings.Join(v.EgressExtraDomains, ", "))
	if v.NoCredentials {
		row("credentials", "none")
	} else {
		row("credentials", strings.Join(v.Credentials, ", "))
	}
	row("not held", strings.Join(v.MissingCredentials, ", "))
	names := make([]string, 0, len(v.Env))
	for name := range v.Env {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, name := range names {
		label := ""
		if i == 0 {
			label = "variables"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s=%s\n", label, name, v.Env[name])
	}
	row("add-ons", strings.Join(v.Addons, ", "))
	row("apt", strings.Join(v.Apt, ", "))
	row("git access", string(v.GitAccess))
	return w.Flush()
}

// completeTemplates offers the caller's template names, from a server
// that is already up.
//
// @decision(DL-122) @decision(DL-023)
func completeTemplates(env *Env) cobra.CompletionFunc {
	return completeWithin(env, func(ctx context.Context, prefix string) []cobra.Completion {
		c, ok := env.existingClient(ctx)
		if !ok {
			return nil
		}
		views, err := c.ListTemplates(ctx)
		if err != nil {
			return nil
		}
		var out []cobra.Completion
		for _, v := range views {
			if strings.HasPrefix(v.Name, prefix) {
				out = append(out, v.Name)
			}
		}
		slices.SortFunc(out, cmp.Compare[cobra.Completion])
		return out
	})
}
