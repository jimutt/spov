package config

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"time"
)

// durationHook decodes durations from strings such as "4h". Bare numbers
// are rejected: YAML's `timeout: 3600` would otherwise decode as 3600
// nanoseconds, which passes validation and then behaves nothing like the
// hour the user meant.
//
// @decision(DL-026)
func durationHook(from, to reflect.Type, data any) (any, error) {
	durType := reflect.TypeOf(time.Duration(0))
	if to != durType || from == durType {
		// Not a duration target, or already decoded (the defaults layer
		// supplies real time.Duration values).
		return data, nil
	}
	switch from.Kind() {
	case reflect.String:
		s := data.(string)
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not a duration (want a value such as \"90m\" or \"4h\"): %w", s, err)
		}
		return d, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return nil, fmt.Errorf("durations need a unit: write %v as a string such as \"%vs\" or \"%vm\"", data, data, data)
	default:
		return data, nil
	}
}

// envMapHook refuses a sandbox.env value YAML read as something other
// than text, which a weakly typed decode would otherwise rewrite: true
// becomes "1" and 1.10 becomes "1.1". sandbox.env is the only
// map[string]string in Config.
//
// @decision(DL-096)
func envMapHook(_, to reflect.Type, data any) (any, error) {
	m, ok := data.(map[string]any)
	if to != reflect.TypeOf(map[string]string{}) || !ok {
		return data, nil
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if v := m[k]; v != nil {
			if _, ok := v.(string); !ok {
				return nil, fmt.Errorf("sandbox.env.%s is %v, which YAML reads as a %T rather than text; quote the value", k, v, v)
			}
		}
	}
	return data, nil
}
