package config

import "errors"

// ErrNoHome is returned when neither XDG_CONFIG_HOME nor a home directory
// can be determined.
var ErrNoHome = errors.New("config: cannot determine home directory")
