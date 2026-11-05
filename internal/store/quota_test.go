package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ip(v int) *int                     { return &v }
func fp(v float64) *float64             { return &v }
func dp(v time.Duration) *time.Duration { return &v }

func TestQuotaEmpty(t *testing.T) {
	if !(Quota{}).Empty() {
		t.Error("the zero quota is not empty")
	}
	for name, q := range map[string]Quota{
		"sessions": {MaxConcurrentSessions: ip(1)},
		"timeout":  {MaxTimeout: dp(time.Hour)},
		"cpu":      {MaxCPU: fp(1)},
		"memory":   {MaxMemoryMiB: ip(1024)},
	} {
		if q.Empty() {
			t.Errorf("a quota with %s set reports empty", name)
		}
	}
	// A limit of zero is a real limit — nothing at all — and must not
	// read as absence, which is why every field is a pointer.
	if (Quota{MaxConcurrentSessions: ip(0)}).Empty() {
		t.Error("a zero limit reads as no limit")
	}
}

// A stored row and a caller's copy must never share a pointer, or an
// edit to one silently changes the other.
func TestQuotaCloneIsDeep(t *testing.T) {
	in := Quota{MaxConcurrentSessions: ip(3), MaxTimeout: dp(time.Hour),
		MaxCPU: fp(2), MaxMemoryMiB: ip(4096)}
	out := in.Clone()
	*out.MaxConcurrentSessions = 99
	*out.MaxTimeout = time.Minute
	*out.MaxCPU = 99
	*out.MaxMemoryMiB = 99
	if *in.MaxConcurrentSessions != 3 || *in.MaxTimeout != time.Hour ||
		*in.MaxCPU != 2 || *in.MaxMemoryMiB != 4096 {
		t.Errorf("the clone shares pointers with its source: %+v", in)
	}
	if (Quota{}).Clone() != (Quota{}) {
		t.Error("cloning an empty quota invented fields")
	}
}

// The duration is stored as a string, which is what an admin types and
// what the config file uses; nanoseconds would be unreadable in a
// database row.
func TestQuotaJSONUsesDurationStrings(t *testing.T) {
	raw, err := json.Marshal(Quota{MaxTimeout: dp(8 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"8h0m0s"`) {
		t.Errorf("encoded = %s", raw)
	}
	var out Quota
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.MaxTimeout == nil || *out.MaxTimeout != 8*time.Hour {
		t.Errorf("round trip = %+v", out)
	}
}

// A row written before the string encoding still loads.
func TestQuotaJSONAcceptsNanoseconds(t *testing.T) {
	var q Quota
	if err := json.Unmarshal([]byte(`{"max_timeout":3600000000000}`), &q); err != nil {
		t.Fatal(err)
	}
	if q.MaxTimeout == nil || *q.MaxTimeout != time.Hour {
		t.Errorf("MaxTimeout = %v, want an hour", q.MaxTimeout)
	}
}

func TestQuotaJSONOmitsUnsetFields(t *testing.T) {
	raw, err := json.Marshal(Quota{MaxCPU: fp(2)})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"max_timeout", "max_concurrent_sessions", "max_memory_mib"} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("encoded %s carries %q", raw, absent)
		}
	}
}

func TestQuotaJSONRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		`{"max_timeout":"not a duration"}`,
		`{"max_timeout":true}`,
		`not json`,
	} {
		var q Quota
		if err := json.Unmarshal([]byte(bad), &q); err == nil {
			t.Errorf("Unmarshal accepted %s", bad)
		}
	}
}

func TestQuotaJSONEmptyObject(t *testing.T) {
	var q Quota
	if err := json.Unmarshal([]byte(`{}`), &q); err != nil {
		t.Fatal(err)
	}
	if !q.Empty() {
		t.Errorf("an empty object decoded to %+v", q)
	}
}

// Every Quota field survives Clone and a JSON round trip, which is how
// both stores keep an override. Reflection sets every field, so a field
// added to Quota and forgotten in either path fails here.
//
// @decision(DL-009) @decision(DL-049)
func TestEveryQuotaFieldSurvivesCloneAndJSON(t *testing.T) {
	var full Quota
	v := reflect.ValueOf(&full).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		p := reflect.New(f.Type().Elem())
		switch p.Elem().Kind() {
		case reflect.Int:
			p.Elem().SetInt(int64(7 + i))
		case reflect.Int64: // time.Duration
			p.Elem().SetInt(int64(time.Duration(i+1) * time.Hour))
		case reflect.Float64:
			p.Elem().SetFloat(1.5 + float64(i))
		default:
			t.Fatalf("field %s has a kind this test does not set", v.Type().Field(i).Name)
		}
		f.Set(p)
	}

	check := func(name string, got Quota) {
		t.Helper()
		gv := reflect.ValueOf(got)
		for i := range gv.NumField() {
			fn := gv.Type().Field(i).Name
			if gv.Field(i).IsNil() {
				t.Errorf("%s dropped %s", name, fn)
				continue
			}
			if want := v.Field(i).Elem().Interface(); gv.Field(i).Elem().Interface() != want {
				t.Errorf("%s changed %s: %v, want %v", name, fn, gv.Field(i).Elem().Interface(), want)
			}
		}
	}
	check("Clone", full.Clone())

	b, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var back Quota
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	check("the JSON round trip", back)
	if !strings.Contains(string(b), `"max_job_duration":"`) {
		t.Errorf("max_job_duration is not written as a duration string: %s", b)
	}
}
