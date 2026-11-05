package cli

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jimutt/spov/internal/config"
)

// sampleOnlyCommented are the keys the reference sample leaves commented
// out, since setting either would make it invalid.
var sampleOnlyCommented = map[string]bool{"sandbox.image": true, "sandbox.gpu": true}

// TestTheConfigurationSampleIsComplete keeps docs/configuration.md, the
// schema and `spov config show` in step: the sample loads and validates,
// sets every key config.Config has, and config show prints each one back.
func TestTheConfigurationSampleIsComplete(t *testing.T) {
	raw, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "## Every key")
	open := strings.Index(doc[start:], "```yaml\n")
	if start < 0 || open < 0 {
		t.Fatal("docs/configuration.md has no sample under \"## Every key\"")
	}
	body := doc[start+open+len("```yaml\n"):]
	sample := body[:strings.Index(body, "```")]

	var file map[string]any
	if err := yaml.Unmarshal([]byte(sample), &file); err != nil {
		t.Fatalf("the sample is not YAML: %v", err)
	}
	wd := t.TempDir()
	environ := userConfigEnv(t, sample)
	out, _, err := run(t, Env{Workdir: wd, Environ: environ}, "config", "show", "--format", "json")
	if err != nil {
		t.Fatalf("the sample does not load: %v", err)
	}
	var shown map[string]any
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatal(err)
	}

	for _, key := range configKeys(reflect.TypeOf(config.Config{}), "") {
		if sampleOnlyCommented[key] {
			if !strings.Contains(sample, "# "+key[strings.LastIndex(key, ".")+1:]+":") {
				t.Errorf("the sample does not show %s, even commented out", key)
			}
			continue
		}
		v, ok := lookup(file, key)
		if !ok {
			t.Errorf("the sample does not set %s", key)
			continue
		}
		if _, ok := lookup(shown, key); !ok && !reflect.ValueOf(v).IsZero() {
			t.Errorf("config show does not print %s", key)
		}
	}
}

// configKeys lists every key path config.Config reads, from its koanf
// tags. A list of structs contributes its elements' keys.
func configKeys(t reflect.Type, prefix string) []string {
	var keys []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("koanf")
		if tag == "" {
			continue
		}
		path := prefix + tag
		ft := f.Type
		if ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Duration(0)) {
			keys = append(keys, configKeys(ft, path+".")...)
			continue
		}
		keys = append(keys, path)
	}
	return keys
}

// lookup finds a dotted key in decoded YAML or JSON. In a list it looks
// through every element.
func lookup(cur any, key string) (any, bool) {
	if key == "" {
		return cur, true
	}
	if list, ok := cur.([]any); ok {
		for _, e := range list {
			if v, ok := lookup(e, key); ok {
				return v, true
			}
		}
		return nil, false
	}
	obj, ok := cur.(map[string]any)
	if !ok {
		return nil, false
	}
	part, rest, _ := strings.Cut(key, ".")
	v, ok := obj[part]
	if !ok {
		return nil, false
	}
	return lookup(v, rest)
}
