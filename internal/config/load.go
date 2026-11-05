package config

import (
	"fmt"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"

	"github.com/go-viper/mapstructure/v2"
)

// EnvPrefix is the prefix of environment variables spov reads.
const EnvPrefix = "SPOV_"

const delim = "."

// Sources records where each layer of a merged config came from, so
// `spov config show` can explain the result.
type Sources struct {
	UserFile    string
	ProjectFile string
	// EnvVars lists the SPOV_* variables that mapped to a real config
	// field. Variables that named nothing are reported in IgnoredEnvVars.
	EnvVars []string
	// IgnoredEnvVars lists SPOV_* variables that did not name a config
	// field, so a typo is visible rather than silently dropped.
	IgnoredEnvVars []string
	// UserFileError explains why the user-level layer was skipped, when it
	// was skipped for a reason other than the file being absent.
	UserFileError string
}

// Result is a merged configuration together with its provenance.
type Result struct {
	Config  Config
	Sources Sources
}

// Load merges the default, user, project and environment layers, later
// layers overriding earlier ones.
//
// Note that list-valued keys (repos, addons) are replaced by a later
// layer, not merged into it. A project file may not set the keys in
// userOnly.
//
// @decision(DL-026)
func Load(env Env, workdir string) (Result, error) {
	return load(env, workdir, true)
}

// LoadUser merges the default, user and environment layers, without a
// project file: a server's configuration.
//
// @decision(DL-026)
func LoadUser(env Env) (Result, error) {
	return load(env, "", false)
}

// userOnly are the top-level sections a project file may not set,
// because they decide where a credential is sent, and where each
// belongs instead. server holds only url.
//
// @decision(DL-026)
var userOnly = []userOnlyKey{
	{"server", "server.url", "SPOV_SERVER or --server"},
	{"platforms", "", ""},
}

type userOnlyKey struct{ key, shown, instead string }

func (u userOnlyKey) name() string {
	if u.shown != "" {
		return u.shown
	}
	return u.key
}

func load(env Env, workdir string, project bool) (Result, error) {
	var res Result
	k := koanf.New(delim)

	// structs.Provider reflects over a static struct and never fails.
	_ = k.Load(structs.Provider(Defaults(), "koanf"), nil)

	userPath, err := UserConfigPath(env)
	switch {
	case err != nil:
		// Not fatal: the project file and environment can still carry a
		// complete config. Record why the layer was skipped.
		res.Sources.UserFileError = err.Error()
	default:
		loaded, err := loadFile(k, userPath)
		if err != nil {
			return res, err
		}
		if loaded {
			res.Sources.UserFile = userPath
		}
	}

	if project {
		projectPath, err := FindProjectFile(workdir)
		if err != nil {
			return res, fmt.Errorf("config: locate %s: %w", ProjectFileName, err)
		}
		if projectPath != "" {
			if err := loadProject(k, projectPath, userPath); err != nil {
				return res, err
			}
			res.Sources.ProjectFile = projectPath
		}
	}

	used, ignored := loadEnv(k, env)
	res.Sources.EnvVars = used
	res.Sources.IgnoredEnvVars = ignored

	if err := k.UnmarshalWithConf("", &res.Config, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			Result:           &res.Config,
			WeaklyTypedInput: true,
			ErrorUnused:      true,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				durationHook,
			),
		},
	}); err != nil {
		return res, fmt.Errorf("config: %s", cleanDecodeError(err))
	}

	if err := Validate(res.Config); err != nil {
		return res, err
	}
	return res, nil
}

// loadProject merges the project file into k, refusing one that names a
// userOnly section at all: an empty or null one replaces the user
// file's in the merge just as a value would.
//
// @decision(DL-026)
func loadProject(k *koanf.Koanf, path, userPath string) error {
	raw, err := file.Provider(path).ReadBytes()
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	top, err := yaml.Parser().Unmarshal(raw)
	if err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	if userPath == "" {
		userPath = "~/.config/spov/config.yaml"
	}
	for _, u := range userOnly {
		if _, set := top[u.key]; !set {
			continue
		}
		where := "your user file, " + userPath
		if u.instead != "" {
			where += ", or " + u.instead
		}
		return fmt.Errorf("config: %s sets %s, which a project file may not set because it decides where"+
			" your credentials are sent; set it in %s", path, u.name(), where)
	}
	_, err = loadFile(k, path)
	return err
}

// loadFile merges path into k if it exists. It reports whether the file was
// present.
func loadFile(k *koanf.Koanf, path string) (bool, error) {
	if !fileExists(path) {
		return false, nil
	}
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return false, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return true, nil
}

// loadEnv merges SPOV_* variables and reports which were applied and which
// named no config field.
//
// @decision(DL-026)
func loadEnv(k *koanf.Koanf, e Env) (used, ignored []string) {
	provider := env.Provider(delim, env.Opt{
		Prefix:      EnvPrefix,
		EnvironFunc: e.Environ,
		TransformFunc: func(name, value string) (string, any) {
			key := envKey(name)
			if key == "" {
				ignored = append(ignored, name)
				return "", nil
			}
			// An empty value means "unset" rather than "set to empty":
			// `export SPOV_MODAL_APP=$UNSET` must not clobber a
			// configured value.
			if value == "" {
				ignored = append(ignored, name)
				return "", nil
			}
			used = append(used, name)
			return key, value
		},
	})
	// The env provider reads from memory and never fails.
	_ = k.Load(provider, nil)
	return used, ignored
}

// envKey converts SPOV_SANDBOX_TIMEOUT to sandbox.timeout. It returns ""
// for any name that does not address a scalar config field, so that typos
// and unsupported keys are reported rather than silently merged.
//
// List-valued config (platforms, repos) is not addressable through the
// environment; those live in the user or project file.
func envKey(name string) string {
	rest := strings.ToLower(strings.TrimPrefix(name, EnvPrefix))
	if rest == "" {
		return ""
	}
	if envKeys[rest] {
		return rest
	}
	section, field, ok := strings.Cut(rest, "_")
	if !ok {
		return ""
	}
	key := section + delim + field
	if !envKeys[key] {
		return ""
	}
	return key
}

// envKeys is the set of config keys addressable through SPOV_* variables,
// derived from the Config struct's koanf tags.
var envKeys = buildEnvKeys()

// cleanDecodeError tidies mapstructure's multi-line error text, whose
// top-level key errors read as "” has invalid keys: x" before v2.5 and
// "'config.Config' has invalid keys: x" from it.
func cleanDecodeError(err error) string {
	msg := err.Error()
	for _, root := range []string{"''", "'config.Config'"} {
		msg = strings.ReplaceAll(msg, root+" has invalid keys", "unknown top-level keys")
	}
	msg = strings.ReplaceAll(msg, "decoding failed due to the following error(s):\n\n", "")
	return strings.ReplaceAll(msg, "\n", "; ")
}
