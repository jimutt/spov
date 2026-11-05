package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDurationHook(t *testing.T) {
	durType := reflect.TypeOf(time.Duration(0))
	strType := reflect.TypeOf("")

	t.Run("parses a duration string", func(t *testing.T) {
		got, err := durationHook(strType, durType, "90m")
		if err != nil {
			t.Fatal(err)
		}
		if got != 90*time.Minute {
			t.Errorf("got %v, want 90m", got)
		}
	})

	t.Run("rejects a bare number", func(t *testing.T) {
		_, err := durationHook(reflect.TypeOf(0), durType, 3600)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "need a unit") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("rejects an unparsable string", func(t *testing.T) {
		if _, err := durationHook(strType, durType, "soon"); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("passes through non-duration targets", func(t *testing.T) {
		got, err := durationHook(strType, strType, "untouched")
		if err != nil {
			t.Fatal(err)
		}
		if got != "untouched" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("passes through values that are already durations", func(t *testing.T) {
		got, err := durationHook(durType, durType, 4*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if got != 4*time.Hour {
			t.Errorf("got %v", got)
		}
	})

	t.Run("passes through other kinds targeting a duration", func(t *testing.T) {
		got, err := durationHook(reflect.TypeOf(true), durType, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != true {
			t.Errorf("got %v", got)
		}
	})
}
