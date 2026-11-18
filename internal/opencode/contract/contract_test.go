package contract

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jimutt/spov/internal/config"
)

func load(t *testing.T) *Document {
	t.Helper()
	d, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// @decision(DL-035)
func TestEveryUsedOperationIsInTheDocument(t *testing.T) {
	d := load(t)
	for _, op := range Used {
		if !d.Has(op) {
			t.Errorf("OpenCode %s has no %s", Version(), op)
		}
	}
}

// A bump that does not refresh the document fails here.
//
// @decision(DL-035)
func TestTheDocumentIsThePinnedBuilds(t *testing.T) {
	if Version() != config.DefaultOpenCodeVersion {
		t.Errorf("the committed document is OpenCode %s and the pin is %s: run scripts/opencode-contract.sh on the pinned binary",
			Version(), config.DefaultOpenCodeVersion)
	}
}

// The validator checks the keywords it knows and reads past the ones it
// ignores. A keyword it has never seen would be read past silently, so
// a document that uses one fails until the validator learns it. Every
// pattern must compile as Go reads it, or it fails on the first request
// or frame it meets rather than at the bump.
//
// @decision(DL-035) @decision(DL-145)
func TestTheValidatorKnowsEveryKeywordTheDocumentUses(t *testing.T) {
	unknown, patterns := documentKeywords(t, document)
	for _, k := range unknown {
		t.Errorf("the document uses the schema keyword %q, which the validator neither checks nor ignores", k)
	}
	for _, p := range patterns {
		t.Errorf("the document's pattern %s does not compile in Go", p)
	}
}

// The keyword sweep itself: an ignored keyword passes, an unknown one
// is named wherever it sits, a boolean subschema has no keywords, and a
// pattern Go cannot compile is named, as a value or a property pattern.
//
// @decision(DL-145)
func TestTheKeywordSweepFindsWhatTheValidatorWouldMiss(t *testing.T) {
	unknown, patterns := documentKeywords(t, []byte(`{
	  "paths": {"/a": {"post": {
	    "parameters": [{"schema": {"type": "string", "pattern": "^(?!x)"}}],
	    "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"n": {"not": {"oneOf": []}}}}}}}
	  }}},
	  "components": {"schemas": {
	    "A": {"type": "string", "contentMediaType": "application/json", "contentSchema": {"if": {}}},
	    "B": {"type": "object", "additionalProperties": false, "properties": {"t": true, "f": false},
	      "patternProperties": {"\\1": {"type": "string"}}},
	    "C": {"type": "array", "items": {"const": 1}, "prefixItems": [true, {"type": "integer", "multipleOf": 2}]}
	  }}
	}`))
	slices.Sort(unknown)
	if want := []string{"const", "multipleOf", "oneOf"}; !slices.Equal(unknown, want) {
		t.Errorf("unknown keywords %q, want %q", unknown, want)
	}
	slices.Sort(patterns)
	if want := []string{`"\\1"`, `"^(?!x)"`}; !slices.Equal(patterns, want) {
		t.Errorf("uncompilable patterns %s, want %s", patterns, want)
	}
}

// documentKeywords walks every schema of a document and returns the
// keywords the validator neither checks nor ignores, and the patterns
// Go's regexp cannot compile.
func documentKeywords(t *testing.T, raw []byte) (unknown, badPatterns []string) {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Schema any `json:"schema"`
			} `json:"parameters"`
			RequestBody *struct {
				Content map[string]struct {
					Schema any `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	compiles := func(p string) {
		if _, err := regexp.Compile(p); err != nil && !seen["pattern:"+p] {
			seen["pattern:"+p] = true
			badPatterns = append(badPatterns, fmt.Sprintf("%q", p))
		}
	}
	var walk func(s any)
	walk = func(s any) {
		m, ok := s.(map[string]any)
		if !ok {
			return
		}
		for k, v := range m {
			if !slices.Contains(Keywords, k) && !slices.Contains(Ignored, k) && !seen[k] {
				seen[k] = true
				unknown = append(unknown, k)
			}
			switch k {
			case "pattern":
				compiles(v.(string))
			case "properties", "patternProperties":
				for name, p := range v.(map[string]any) {
					if k == "patternProperties" {
						compiles(name)
					}
					walk(p)
				}
			case "anyOf", "allOf", "prefixItems":
				for _, b := range v.([]any) {
					walk(b)
				}
			case "items", "additionalProperties", "not":
				walk(v)
			}
		}
	}
	for _, s := range doc.Components.Schemas {
		walk(s)
	}
	for _, methods := range doc.Paths {
		for _, op := range methods {
			for _, p := range op.Parameters {
				walk(p.Schema)
			}
			if op.RequestBody != nil {
				for _, c := range op.RequestBody.Content {
					walk(c.Schema)
				}
			}
		}
	}
	return unknown, badPatterns
}

func check1(d *Document, method, target, body string) error {
	u, err := url.Parse(target)
	if err != nil {
		return err
	}
	_, err = d.Check(method, u, []byte(body))
	return err
}

// The requests spov sends pass against the pinned build's document, and
// each kind of drift fails with a message that names it.
//
// @decision(DL-035)
func TestRequestsAreCheckedAgainstThePinnedDocument(t *testing.T) {
	d := load(t)
	for _, c := range []struct {
		name, method, target, body string
		wantErr                    string
	}{
		{"identity", "GET", "/api/info", "", ""},
		{"events", "GET", "/api/event", "", ""},
		{"a session with a location", "POST", "/api/session", `{"location":{"directory":"/workspace"}}`, ""},
		{"a model object", "POST", "/api/session/ses_1/model", `{"model":{"id":"m","providerID":"p","variant":"high"}}`, ""},
		{"a model string", "POST", "/api/session/ses_1/model", `{"model":"p/m"}`, `body.model: string is not of type object`},
		{"the interrupt with resume", "POST", "/api/session/ses_1/interrupt?resume=true", "", ""},
		{"an unknown query parameter", "POST", "/api/session/ses_1/interrupt?carry=true", "", `unknown query parameter "carry"`},
		{"a value outside an enum", "POST", "/api/session/ses_1/interrupt?resume=yes", "", `"yes" is not one of`},
		{"a steer", "POST", "/api/session/ses_1/prompt", `{"id":"msg_1","text":"hi","resume":true,"delivery":"steer"}`, ""},
		{"a message ID of the wrong shape", "POST", "/api/session/ses_1/prompt", `{"id":"m1","text":"hi"}`, `body.id`},
		{"an unknown body field", "POST", "/api/session/ses_1/synthetic", `{"text":"t","carry":true}`, `unknown field "carry"`},
		{"a missing required field", "POST", "/api/session/ses_1/synthetic", `{"resume":true}`, `missing required field "text"`},
		{"a required body left out", "POST", "/api/session/ses_1/command", "", "requires one"},
		{"a body to a GET", "GET", "/api/session", `{}`, "does not take"},
		{"a file read", "GET", "/api/fs/read/out/report.md?location%5Bdirectory%5D=%2Fworkspace", "", ""},
		{"an unknown deep-object field", "GET", "/api/fs/read/x?location%5Bplace%5D=%2Fw", "", `no property "place"`},
		{"a deep object on a plain parameter", "GET", "/api/session?limit%5Bx%5D=1", "", "not a deep object"},
		{"a read with no path", "GET", "/api/fs/read/", "", "no operation"},
		{"a path the build does not have", "GET", "/api/nothing", "", "no operation GET /api/nothing"},
		{"an operation spov does not use", "GET", "/api/config", "", "not in contract.Used"},
		{"a conversation ID OpenCode would not mint", "GET", "/api/session/conv_1/message", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := check1(d, c.method, c.target, c.body)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.wantErr != "" && err == nil:
				t.Errorf("accepted, want an error containing %q", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("error %q, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

// The schema rules the pinned document happens not to exercise on
// spov's requests, against a small document of its own.
//
// @decision(DL-035)
func TestTheValidatorsRules(t *testing.T) {
	d, err := Parse([]byte(`{
	  "paths": {
	    "/a/{id}": {"post": {"requestBody": {"required": false, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Body"}}}}}},
	    "/a/fixed": {"post": {"parameters": [{"name": "q", "in": "query", "required": true, "schema": {"type": "string"}}]}}
	  },
	  "components": {"schemas": {
	    "Body": {"type": "object", "additionalProperties": {"type": "integer", "minimum": 1, "maximum": 9}, "properties": {
	      "n": {"type": "number", "exclusiveMinimum": 0},
	      "pair": {"type": "array", "prefixItems": [{"type": "string"}, {"type": "boolean"}], "minItems": 2},
	      "list": {"type": "array", "items": {"type": ["string", "null"]}},
	      "loop": {"$ref": "#/components/schemas/Loop"},
	      "bad": {"$ref": "#/elsewhere"},
	      "re": {"type": "string", "pattern": "("},
	      "name": {"type": "string", "minLength": 2},
	      "notnull": {"not": {"type": "null"}},
	      "both": {"allOf": [{"type": "object", "additionalProperties": {"type": "string"}}], "type": "object", "properties": {"n": {"type": "integer"}}},
	      "byname": {"type": "object", "additionalProperties": false, "properties": {"ses_fixed": {"type": "string"}},
	        "patternProperties": {"^ses_": {"type": "string", "minLength": 2}, "^id_": {"type": "integer"}, "_9$": {"minimum": 5}}},
	      "loose": {"type": "object", "additionalProperties": false, "patternProperties": {"ses": {"type": "string"}}},
	      "badname": {"type": "object", "patternProperties": {"(": {}}},
	      "anything": true,
	      "never": false,
	      "empty": {"type": "array", "items": false},
	      "nothing": {"not": true},
	      "odd": 3,
	      "either": {"anyOf": [false, {"type": "string"}]},
	      "free": {"type": "object", "additionalProperties": true, "patternProperties": {"^n_": false}}
	    }},
	    "Loop": {"$ref": "#/components/schemas/Loop"}
	  }}
	}`), []Operation{{"POST", "/a/{id}"}, {"POST", "/a/fixed"}})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		target, body, wantErr string
	}{
		{"/a/1", ``, ""},
		{"/a/1", `{"n":0.5,"pair":["a",true],"list":["x",null],"extra":3}`, ""},
		{"/a/1", `{"n":0}`, "is not above"},
		{"/a/1", `{"extra":0}`, "below"},
		{"/a/1", `{"extra":10}`, "above"},
		{"/a/1", `{"extra":1.5}`, "not of type integer"},
		{"/a/1", `{"pair":["a"]}`, "want at least 2"},
		{"/a/1", `{"pair":["a","b"]}`, "pair[1]"},
		{"/a/1", `{"list":[1]}`, "list[0]"},
		{"/a/1", `{"loop":1}`, "too deep"},
		{"/a/1", `{"bad":1}`, "unsupported $ref"},
		{"/a/1", `{"re":"x"}`, "pattern"},
		{"/a/1", `{"name":"åä"}`, ""},
		{"/a/1", `{"name":"å"}`, "shorter than 2"},
		{"/a/1", `{"notnull":1}`, ""},
		{"/a/1", `{"notnull":null}`, "must not"},
		{"/a/1", `{"both":{"s":"x"}}`, ""},
		{"/a/1", `{"both":{"s":1}}`, "both.s"},
		{"/a/1", `{"byname":{"ses_a":"xy","id_1":3}}`, ""},
		{"/a/1", `{"byname":{"id_1":"x"}}`, "byname.id_1: string is not of type integer"},
		{"/a/1", `{"byname":{"other":1}}`, `unknown field "other"`},
		{"/a/1", `{"byname":{"ses_fixed":"x"}}`, "shorter than 2"},
		{"/a/1", `{"byname":{"ses_fixed":1}}`, "byname.ses_fixed: integer is not of type string"},
		{"/a/1", `{"byname":{"id_9":7}}`, ""},
		{"/a/1", `{"byname":{"id_9":3}}`, "byname.id_9: 3 is below 5"},
		{"/a/1", `{"byname":{"id_9":"x"}}`, "byname.id_9: string is not of type integer"},
		{"/a/1", `{"loose":{"x_ses_y":"a"}}`, ""},
		{"/a/1", `{"loose":{"x_ses_y":1}}`, "loose.x_ses_y: integer is not of type string"},
		{"/a/1", `{"loose":{"x_se":"a"}}`, `unknown field "x_se"`},
		{"/a/1", `{"badname":{"k":1}}`, "pattern"},
		{"/a/1", `{"anything":{"a":[1]}}`, ""},
		{"/a/1", `{"never":1}`, "never: 1 is where the schema allows no value"},
		{"/a/1", `{"empty":[]}`, ""},
		{"/a/1", `{"empty":[1]}`, "empty[0]"},
		{"/a/1", `{"nothing":1}`, "must not"},
		{"/a/1", `{"odd":1}`, "odd: a schema of type float64"},
		{"/a/1", `{"either":"x"}`, ""},
		{"/a/1", `{"either":1}`, "body.either: integer is not of type string"},
		{"/a/1", `{"free":{"k":1}}`, ""},
		{"/a/1", `{"free":{"n_k":1}}`, "free.n_k: 1 is where"},
		{"/a/1", `[1]`, "not of type object"},
		{"/a/1", `{`, "not JSON"},
		{"/a/1", `{} {}`, "after its JSON value"},
		{"/a/fixed", ``, `missing required query parameter "q"`},
		{"/a/fixed?q=1", ``, ""},
	} {
		if err := check1(d, "POST", c.target, c.body); (err == nil) != (c.wantErr == "") || (err != nil && !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s %s: error %v, want %q", c.target, c.body, err, c.wantErr)
		}
	}
	other, err := Parse([]byte(`{"paths": {"/x": {"post": {"requestBody": {"content": {"text/plain": {}}}}}}}`), []Operation{{"POST", "/x"}})
	if err != nil {
		t.Fatalf("a document with a body that is not JSON was refused: %v", err)
	}
	if err := check1(other, "POST", "/x", "text"); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("a request to an operation whose body is not JSON was accepted: %v", err)
	}
	if _, err := Parse([]byte(`{`), nil); err == nil {
		t.Error("a broken document was accepted")
	}
}

// A fake server wrapped in Checked reports a request that drifts from
// the document, and passes every request on to the fake.
//
// @decision(DL-035)
func TestAFakeSandboxReportsDrift(t *testing.T) {
	rec := &recorder{TB: t}
	var reached atomic.Int32
	srv := httptest.NewServer(Checked(rec, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) })))
	defer srv.Close()
	for _, target := range []string{"/api/info", "/connect/sb-1/0/api/session/ses_1/interrupt?carry=true"} {
		resp, err := http.Post(srv.URL+target, "application/json", strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if reached.Load() != 2 {
		t.Errorf("the fake saw %d requests, want 2", reached.Load())
	}
	// POST /api/info is not an operation, and carry is not a parameter
	// once the fake connect prefix is dropped.
	if len(rec.errors) != 2 || !strings.Contains(rec.errors[1], `unknown query parameter "carry"`) {
		t.Errorf("reported %q", rec.errors)
	}

	rec.errors = nil
	CheckRequest(rec, "GET", "/api/session/ses_1/message", nil)
	CheckRequest(rec, "GET", "/api/session/ses_1/messages", nil)
	CheckRequest(rec, "GET", "%zz", nil)
	if len(rec.errors) != 2 {
		t.Errorf("reported %q, want the misspelled route and the bad URL", rec.errors)
	}
}

// recorder collects what a check reports instead of failing the test.
// The fake server's handler reports from its own goroutine.
type recorder struct {
	testing.TB
	mu     sync.Mutex
	errors []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// @decision(DL-035)
func TestCheckSchema(t *testing.T) {
	d := load(t)
	if err := d.CheckSchema("Model.Ref", []byte(`{"id":"m","providerID":"p"}`)); err != nil {
		t.Errorf("a valid model reference was refused: %v", err)
	}
	if err := d.CheckSchema("Model.Ref", []byte(`{"id":"m"}`)); err == nil {
		t.Error("a model reference with no provider was accepted")
	}
	if err := d.CheckSchema("No.Such.Schema", []byte(`{}`)); err == nil {
		t.Error("an unknown schema was accepted")
	}
	if err := d.CheckSchema("Model.Ref", []byte(`{`)); err == nil {
		t.Error("a broken document was accepted")
	}
}
