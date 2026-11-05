package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// UserFileName is the basename of the user-level config file.
const UserFileName = "config.yaml"

// ProjectFileName is the basename of the project-level config file.
const ProjectFileName = ".spov.yaml"

// UserConfigPath returns the user-level config file path, honouring
// XDG_CONFIG_HOME and falling back to ~/.config.
//
// @decision(DL-026)
func UserConfigPath(env Env) (string, error) {
	if env == nil {
		return "", fmt.Errorf("config: no environment to read")
	}
	if dir := env.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "spov", UserFileName), nil
	}
	home, err := env.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "spov", UserFileName), nil
}

// FindProjectFile walks up from dir looking for .spov.yaml and returns the
// first match. It returns "" (and no error) when none is found.
//
// @decision(DL-026)
func FindProjectFile(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(abs, ProjectFileName)
		if fileExists(candidate) {
			return candidate, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", nil
		}
		abs = parent
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Env abstracts the process environment so layering can be tested without
// mutating global state.
type Env interface {
	Getenv(key string) string
	Environ() []string
	UserHomeDir() (string, error)
}

// OSEnv is the real process environment.
type OSEnv struct{}

func (OSEnv) Getenv(key string) string     { return os.Getenv(key) }
func (OSEnv) Environ() []string            { return os.Environ() }
func (OSEnv) UserHomeDir() (string, error) { return os.UserHomeDir() }

// MapEnv is an in-memory Env for tests.
type MapEnv struct {
	Vars map[string]string
	Home string
}

func (m MapEnv) Getenv(key string) string { return m.Vars[key] }

func (m MapEnv) Environ() []string {
	out := make([]string, 0, len(m.Vars))
	for k, v := range m.Vars {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

func (m MapEnv) UserHomeDir() (string, error) {
	if m.Home == "" {
		return "", ErrNoHome
	}
	return m.Home, nil
}
