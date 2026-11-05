package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/prep"
)

func newConfigCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect spov configuration",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}
	cmd.AddCommand(newConfigShowCmd(env))
	return cmd
}

// newConfigShowCmd prints the merged configuration and where each layer
// came from.
//
// @decision(DL-026)
func newConfigShowCmd(env *Env) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the merged configuration and its sources",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			res, err := config.Load(env.Environ, env.Workdir)
			if err != nil {
				return err
			}
			backend := env.credentialBackend()
			stored := env.storedCredentials(res.Config)
			switch format {
			case "yaml":
				return writeConfigYAML(env.Out, res, backend, stored)
			case "json":
				return writeConfigJSON(env.Out, res, backend, stored)
			default:
				return fmt.Errorf("unknown --format %q (want yaml or json)", format)
			}
		},
	}
	cmd.Flags().StringVar(&format, "format", "yaml", "output format: yaml or json")
	return cmd
}

// shownConfig is the wire shape of `spov config show`, with durations
// rendered as human strings rather than nanosecond counts.
type shownConfig struct {
	Version   int               `json:"version" yaml:"version"`
	Modal     config.Modal      `json:"modal" yaml:"modal"`
	Sandbox   shownSandbox      `json:"sandbox" yaml:"sandbox"`
	OpenCode  config.OpenCode   `json:"opencode" yaml:"opencode"`
	Platforms []config.Platform `json:"platforms,omitempty" yaml:"platforms,omitempty"`
	Repos     []config.Repo     `json:"repos,omitempty" yaml:"repos,omitempty"`
	Landing   string            `json:"landing,omitempty" yaml:"landing,omitempty"`
	// Git is shown by value rather than by presence, unlike the
	// credential summary: an identity is not a secret, it is stamped on
	// every commit the sandbox makes. Omitted when unset, since what is
	// then used is the launching machine's git config.
	//
	// @decision(DL-044)
	Git *config.Git `json:"git,omitempty" yaml:"git,omitempty"`
	// Server, Quotas, Prep and Metrics are shown whole: config show is
	// how a user sees every merged value, and a key it leaves out looks
	// unset.
	Server  config.Server  `json:"server" yaml:"server"`
	Quotas  shownQuotas    `json:"quotas" yaml:"quotas"`
	Prep    prep.Config    `json:"prep" yaml:"prep"`
	Metrics config.Metrics `json:"metrics" yaml:"metrics"`
	// Credentials names the backend secrets are stored in, so a user can
	// tell where their tokens actually live.
	//
	// @decision(DL-008)
	Credentials string `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	// Stored reports which credentials the store holds. Presence only:
	// printing even a prefix of a secret invites it into a paste of this
	// output.
	//
	// @decision(DL-008)
	Stored  *shownStored `json:"stored,omitempty" yaml:"stored,omitempty"`
	Sources shownSources `json:"sources" yaml:"sources"`
}

// shownStored lists what the credential store holds, by name.
//
// @decision(DL-008)
type shownStored struct {
	Platforms []string `json:"platforms,omitempty" yaml:"platforms,omitempty"`
	Providers []string `json:"providers,omitempty" yaml:"providers,omitempty"`
}

// shownSandbox is config.Sandbox with its durations as strings. Every
// key is here; TestTheConfigurationSampleIsComplete fails when one is
// missing.
type shownSandbox struct {
	CPU                float64  `json:"cpu" yaml:"cpu"`
	MemoryMiB          int      `json:"memory_mib" yaml:"memory_mib"`
	CPULimit           float64  `json:"cpu_limit" yaml:"cpu_limit"`
	MemoryLimitMiB     int      `json:"memory_limit_mib" yaml:"memory_limit_mib"`
	Timeout            string   `json:"timeout" yaml:"timeout"`
	IdleTimeout        string   `json:"idle_timeout" yaml:"idle_timeout"`
	Image              string   `json:"image,omitempty" yaml:"image,omitempty"`
	Regions            []string `json:"regions,omitempty" yaml:"regions,omitempty"`
	Cloud              string   `json:"cloud,omitempty" yaml:"cloud,omitempty"`
	Workdir            string   `json:"workdir,omitempty" yaml:"workdir,omitempty"`
	Egress             string   `json:"egress" yaml:"egress"`
	EgressExtraDomains []string `json:"egress_extra_domains,omitempty" yaml:"egress_extra_domains,omitempty"`
	InboundCIDRs       []string `json:"inbound_cidrs,omitempty" yaml:"inbound_cidrs,omitempty"`
	GPU                string   `json:"gpu,omitempty" yaml:"gpu,omitempty"`
	GitAccess          string   `json:"git_access,omitempty" yaml:"git_access,omitempty"`
	JobGitAccess       string   `json:"job_git_access,omitempty" yaml:"job_git_access,omitempty"`
	// @decision(DL-049)
	JobMaxDuration string `json:"job_max_duration,omitempty" yaml:"job_max_duration,omitempty"`
	// @decision(DL-027)
	Sync bool `json:"sync,omitempty" yaml:"sync,omitempty"`
	// @decision(DL-033)
	Addons []string `json:"addons,omitempty" yaml:"addons,omitempty"`
	Apt    []string `json:"apt,omitempty" yaml:"apt,omitempty"`
}

// shownQuotas is config.Quotas with its durations as strings.
type shownQuotas struct {
	MaxConcurrentSessions int     `json:"max_concurrent_sessions" yaml:"max_concurrent_sessions"`
	MaxConcurrentJobs     int     `json:"max_concurrent_jobs" yaml:"max_concurrent_jobs"`
	MaxJobDuration        string  `json:"max_job_duration" yaml:"max_job_duration"`
	MaxTimeout            string  `json:"max_timeout" yaml:"max_timeout"`
	MaxCPU                float64 `json:"max_cpu" yaml:"max_cpu"`
	MaxMemoryMiB          int     `json:"max_memory_mib" yaml:"max_memory_mib"`
}

type shownSources struct {
	UserFile       string   `json:"user_file,omitempty" yaml:"user_file,omitempty"`
	UserFileError  string   `json:"user_file_error,omitempty" yaml:"user_file_error,omitempty"`
	ProjectFile    string   `json:"project_file,omitempty" yaml:"project_file,omitempty"`
	EnvVars        []string `json:"env_vars,omitempty" yaml:"env_vars,omitempty"`
	IgnoredEnvVars []string `json:"ignored_env_vars,omitempty" yaml:"ignored_env_vars,omitempty"`
}

// shownGit omits the section when nothing is configured, so the output
// does not imply spov has an identity it does not have.
//
// @decision(DL-044)
func shownGit(g config.Git) *config.Git {
	if g.Name == "" && g.Email == "" && !g.AllowAll {
		return nil
	}
	return &g
}

func newShownConfig(res config.Result, credentialBackend string, stored *shownStored) shownConfig {
	c := res.Config
	return shownConfig{
		Version:  c.Version,
		Modal:    c.Modal,
		OpenCode: c.OpenCode,
		Sandbox: shownSandbox{
			CPU:                c.Sandbox.CPU,
			MemoryMiB:          c.Sandbox.MemoryMiB,
			CPULimit:           c.Sandbox.CPULimit,
			MemoryLimitMiB:     c.Sandbox.MemoryLimitMiB,
			Timeout:            c.Sandbox.Timeout.String(),
			IdleTimeout:        c.Sandbox.IdleTimeout.String(),
			Image:              c.Sandbox.Image,
			Regions:            c.Sandbox.Regions,
			Cloud:              c.Sandbox.Cloud,
			Workdir:            c.Sandbox.Workdir,
			Egress:             c.Sandbox.Egress,
			EgressExtraDomains: c.Sandbox.EgressExtraDomains,
			InboundCIDRs:       c.Sandbox.InboundCIDRs,
			GPU:                c.Sandbox.GPU,
			GitAccess:          c.Sandbox.GitAccess,
			JobGitAccess:       c.Sandbox.JobGitAccess,
			JobMaxDuration:     configuredDuration(c.Sandbox.JobMaxDuration),
			Sync:               c.Sandbox.Sync,
			Addons:             c.Sandbox.Addons,
			Apt:                c.Sandbox.Apt,
		},
		Server: c.Server,
		Quotas: shownQuotas{
			MaxConcurrentSessions: c.Quotas.MaxConcurrentSessions,
			MaxConcurrentJobs:     c.Quotas.MaxConcurrentJobs,
			MaxJobDuration:        c.Quotas.MaxJobDuration.String(),
			MaxTimeout:            c.Quotas.MaxTimeout.String(),
			MaxCPU:                c.Quotas.MaxCPU,
			MaxMemoryMiB:          c.Quotas.MaxMemoryMiB,
		},
		Prep:        c.Prep,
		Metrics:     c.Metrics,
		Platforms:   c.Platforms,
		Repos:       c.Repos,
		Landing:     c.Landing,
		Git:         shownGit(c.Git),
		Credentials: credentialBackend,
		Stored:      stored,
		Sources: shownSources{
			UserFile:       res.Sources.UserFile,
			UserFileError:  res.Sources.UserFileError,
			ProjectFile:    res.Sources.ProjectFile,
			EnvVars:        res.Sources.EnvVars,
			IgnoredEnvVars: res.Sources.IgnoredEnvVars,
		},
	}
}

func writeConfigYAML(w io.Writer, res config.Result, backend string, stored *shownStored) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(newShownConfig(res, backend, stored)); err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	return enc.Close()
}

func writeConfigJSON(w io.Writer, res config.Result, backend string, stored *shownStored) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(newShownConfig(res, backend, stored)); err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	return nil
}

// storedCredentials reports which credentials the store holds, by name.
// It probes only the platforms in config and the provider variables spov
// knows about, since the store cannot be enumerated.
//
// A store that errors yields no summary rather than a wrong one: absence
// here would otherwise read as "nothing stored".
//
// @decision(DL-008)
func (e *Env) storedCredentials(cfg config.Config) *shownStored {
	if e.Credentials == nil {
		return nil
	}
	var out shownStored

	for _, p := range cfg.Platforms {
		key, err := credentials.PlatformKey(p.Name)
		if err != nil {
			continue
		}
		if _, err := e.Credentials.Get(key); err == nil {
			out.Platforms = append(out.Platforms, p.Name)
		}
	}
	for _, name := range providerEnvVars {
		key, err := credentials.ProviderKey(name)
		if err != nil {
			continue
		}
		if _, err := e.Credentials.Get(key); err == nil {
			out.Providers = append(out.Providers, name)
		}
	}

	if len(out.Platforms) == 0 && len(out.Providers) == 0 {
		return nil
	}
	return &out
}
