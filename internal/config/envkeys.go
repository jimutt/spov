package config

import (
	"reflect"
	"time"
)

// buildEnvKeys derives the set of SPOV_* addressable config keys from the
// Config struct's koanf tags, so the two cannot drift apart. Only scalar
// fields are addressable; slices (platforms, repos) are file-only.
//
// @decision(DL-026)
func buildEnvKeys() map[string]bool {
	keys := make(map[string]bool)
	walkScalars(reflect.TypeOf(Config{}), "", keys)
	return keys
}

func walkScalars(t reflect.Type, prefix string, keys map[string]bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("koanf")
		if tag == "" {
			continue
		}
		name := prefix + tag
		switch {
		case f.Type == reflect.TypeOf(time.Duration(0)):
			keys[name] = true
		case f.Type.Kind() == reflect.Struct:
			walkScalars(f.Type, name+delim, keys)
		case f.Type.Kind() == reflect.Slice, f.Type.Kind() == reflect.Map:
			// Lists and maps are not addressable through the environment.
			//
			// @decision(DL-096)
		default:
			keys[name] = true
		}
	}
}
