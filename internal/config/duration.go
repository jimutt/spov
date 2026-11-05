package config

import (
	"fmt"
	"reflect"
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
