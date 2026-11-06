// Package contract checks the requests spov sends to an OpenCode server
// against the OpenAPI document the pinned build publishes. Only tests
// import it; scripts/opencode-contract.sh refreshes the document.
//
// @decision(DL-035)
package contract

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

//go:embed openapi.json
var document []byte

//go:embed version
var versionFile string

// Version is the OpenCode build the committed document came from.
func Version() string { return strings.TrimSpace(versionFile) }

// Operation is one request spov sends to an OpenCode server: a method
// and the document's path template.
type Operation struct {
	Method string
	Path   string
}

func (o Operation) String() string { return o.Method + " " + o.Path }

// Used is every OpenCode operation spov sends. A request a test's fake
// sandbox sees must be one of these, and each must be in the document.
//
// @decision(DL-035)
var Used = []Operation{
	{"GET", "/api/info"},
	{"GET", "/api/event"},
	{"GET", "/api/session"},
	{"POST", "/api/session"},
	{"GET", "/api/session/{sessionID}/message"},
	{"POST", "/api/session/{sessionID}/synthetic"},
	{"POST", "/api/session/{sessionID}/prompt"},
	{"POST", "/api/session/{sessionID}/interrupt"},
	// @decision(DL-019)
	{"DELETE", "/api/session/{sessionID}/inbox/{inboxID}"},
	// @decision(DL-020)
	{"POST", "/api/session/{sessionID}/permission/{requestID}/reply"},
	{"POST", "/api/session/{sessionID}/form/{formID}/reply"},
	{"DELETE", "/api/session/{sessionID}/form/{formID}"},
	{"POST", "/api/session/{sessionID}/model"},
	{"POST", "/api/session/{sessionID}/command"},
	{"GET", "/api/fs/read/*"},
	{"GET", "/api/fs/list"},
	// @decision(DL-089)
	{"POST", "/api/experimental/session/import"},
	{"GET", "/api/experimental/session/{sessionID}/log"},
}

// Document is the parsed OpenAPI document, with the operations a
// request may match.
type Document struct {
	ops     []operation
	schemas map[string]any
	used    []Operation
}

type operation struct {
	Operation
	segments []string
	params   []parameter
	// body is the request body's JSON schema, nil when it takes none.
	body         map[string]any
	bodyRequired bool
	// otherBody is a body in another media type, which is not checked.
	otherBody bool
}

type parameter struct {
	Name     string         `json:"name"`
	In       string         `json:"in"`
	Required bool           `json:"required"`
	Style    string         `json:"style"`
	Schema   map[string]any `json:"schema"`
}

var (
	loadOnce sync.Once
	loaded   *Document
	loadErr  error
)

// Load parses the committed document once, against Used.
func Load() (*Document, error) {
	loadOnce.Do(func() { loaded, loadErr = Parse(document, Used) })
	return loaded, loadErr
}

// Parse reads an OpenAPI document. A request is checked only if it
// matches one of used.
func Parse(raw []byte, used []Operation) (*Document, error) {
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters  []parameter `json:"parameters"`
			RequestBody *struct {
				Required bool `json:"required"`
				Content  map[string]struct {
					Schema map[string]any `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("contract: %w", err)
	}
	d := &Document{schemas: doc.Components.Schemas, used: used}
	for path, methods := range doc.Paths {
		for method, op := range methods {
			o := operation{
				Operation: Operation{Method: strings.ToUpper(method), Path: path},
				segments:  strings.Split(strings.TrimPrefix(path, "/"), "/"),
				params:    op.Parameters,
			}
			if rb := op.RequestBody; rb != nil {
				if body, ok := rb.Content["application/json"]; ok {
					o.body, o.bodyRequired = body.Schema, rb.Required
				} else {
					o.otherBody = true
				}
			}
			d.ops = append(d.ops, o)
		}
	}
	// Sorted, so a tie between two templates resolves the same way on
	// every run.
	slices.SortFunc(d.ops, func(a, b operation) int { return strings.Compare(a.String(), b.String()) })
	return d, nil
}

// Has reports whether the document has an operation.
func (d *Document) Has(op Operation) bool {
	return slices.ContainsFunc(d.ops, func(o operation) bool { return o.Operation == op })
}

// Check matches a request to one of the document's operations, which
// must be in Used, and checks its query parameters and JSON body
// against it. Path parameters are matched by position only, so a fake's
// conversation ID need not look like OpenCode's.
func (d *Document) Check(method string, u *url.URL, body []byte) (Operation, error) {
	op, ok := d.match(method, u.EscapedPath())
	if !ok {
		return Operation{}, fmt.Errorf("OpenCode %s has no operation %s %s", Version(), method, u.EscapedPath())
	}
	if !slices.Contains(d.used, op.Operation) {
		return op.Operation, fmt.Errorf("%s is not in contract.Used", op.Operation)
	}
	if err := d.checkQuery(op, u.Query()); err != nil {
		return op.Operation, fmt.Errorf("%s: %w", op.Operation, err)
	}
	if err := d.checkBody(op, body); err != nil {
		return op.Operation, fmt.Errorf("%s: %w", op.Operation, err)
	}
	return op.Operation, nil
}

// CheckSchema validates a JSON document against one of the document's
// named schemas, such as the config file's.
func (d *Document) CheckSchema(name string, body []byte) error {
	schema, ok := d.schemas[name].(map[string]any)
	if !ok {
		return fmt.Errorf("OpenCode %s has no schema %q", Version(), name)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("%s: not JSON: %w", name, err)
	}
	return d.validate(v, schema, name)
}

// match finds the operation for a path. Where two templates match, the
// one with more literal segments wins.
func (d *Document) match(method, path string) (operation, bool) {
	got := strings.Split(strings.TrimPrefix(path, "/"), "/")
	var best operation
	bestLiterals := -1
	for _, op := range d.ops {
		if op.Method != method {
			continue
		}
		if n, ok := matchSegments(op.segments, got); ok && n > bestLiterals {
			best, bestLiterals = op, n
		}
	}
	return best, bestLiterals >= 0
}

// matchSegments matches a template's segments: a literal matches
// itself, {name} any one non-empty segment, and a final * the rest.
func matchSegments(template, got []string) (literals int, ok bool) {
	for i, t := range template {
		if t == "*" && i == len(template)-1 {
			return literals, len(got) > i && got[i] != ""
		}
		if i >= len(got) {
			return 0, false
		}
		switch {
		case strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}"):
			if got[i] == "" {
				return 0, false
			}
		case t == got[i]:
			literals++
		default:
			return 0, false
		}
	}
	return literals, len(got) == len(template)
}

func (d *Document) checkQuery(op operation, q url.Values) error {
	seen := map[string]bool{}
	for key, values := range q {
		name, sub, deep := strings.Cut(key, "[")
		p, ok := op.param(name, "query")
		if !ok {
			return fmt.Errorf("unknown query parameter %q", key)
		}
		seen[name] = true
		schema := p.Schema
		if deep {
			if p.Style != "deepObject" || !strings.HasSuffix(sub, "]") {
				return fmt.Errorf("query parameter %q is not a deep object", key)
			}
			prop, err := d.property(schema, strings.TrimSuffix(sub, "]"))
			if err != nil {
				return fmt.Errorf("query parameter %q: %w", key, err)
			}
			schema = prop
		}
		for _, v := range values {
			if err := d.validate(v, schema, key); err != nil {
				return err
			}
		}
	}
	for _, p := range op.params {
		if p.In == "query" && p.Required && !seen[p.Name] {
			return fmt.Errorf("missing required query parameter %q", p.Name)
		}
	}
	return nil
}

func (o operation) param(name, in string) (parameter, bool) {
	for _, p := range o.params {
		if p.Name == name && p.In == in {
			return p, true
		}
	}
	return parameter{}, false
}

// property finds a named property in an object schema, looking through
// $ref and anyOf for the object.
func (d *Document) property(schema map[string]any, name string) (map[string]any, error) {
	schema, err := d.resolve(schema)
	if err != nil {
		return nil, err
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		if p, ok := props[name].(map[string]any); ok {
			return p, nil
		}
	}
	for _, key := range []string{"anyOf", "allOf"} {
		branches, _ := schema[key].([]any)
		for _, b := range branches {
			if bs, ok := b.(map[string]any); ok {
				if p, err := d.property(bs, name); err == nil {
					return p, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no property %q", name)
}

func (d *Document) checkBody(op operation, body []byte) error {
	empty := len(bytes.TrimSpace(body)) == 0
	switch {
	case op.otherBody:
		return errors.New("takes a body that is not JSON, which the contract cannot check")
	case op.body == nil && !empty:
		return errors.New("sends a body the operation does not take")
	case op.body == nil:
		return nil
	case empty && op.bodyRequired:
		return errors.New("sends no body, and the operation requires one")
	case empty:
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("body is not JSON: %w", err)
	}
	if dec.More() {
		return errors.New("body has data after its JSON value")
	}
	return d.validate(v, op.body, "body")
}

// Keywords are the schema keywords validate checks, and Ignored the
// ones it reads past. A document using any other keyword fails
// TestTheValidatorKnowsEveryKeywordTheDocumentUses.
var (
	Keywords = []string{"$ref", "type", "enum", "pattern", "properties", "required", "additionalProperties",
		"items", "prefixItems", "minItems", "anyOf", "allOf", "not", "minimum", "maximum", "exclusiveMinimum", "minLength"}
	Ignored = []string{"description", "format", "contentEncoding", "contentMediaType", "title", "default", "examples"}
)

var (
	patternsMu sync.Mutex
	patterns   = map[string]*regexp.Regexp{}
)

func (d *Document) resolve(schema map[string]any) (map[string]any, error) {
	for range 32 {
		ref, ok := schema["$ref"].(string)
		if !ok {
			return schema, nil
		}
		name, ok := strings.CutPrefix(ref, "#/components/schemas/")
		if !ok {
			return nil, fmt.Errorf("unsupported $ref %q", ref)
		}
		next, ok := d.schemas[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unknown schema %q", name)
		}
		schema = next
	}
	return nil, errors.New("$ref chain too deep")
}

// validate checks a JSON value (decoded with UseNumber) or a query
// parameter's string against a schema.
func (d *Document) validate(v any, schema map[string]any, at string) error {
	schema, err := d.resolve(schema)
	if err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	if branches, ok := schema["anyOf"].([]any); ok {
		var first error
		for _, b := range branches {
			bs, _ := b.(map[string]any)
			err := d.validate(v, bs, at)
			if err == nil {
				first = nil
				break
			}
			// Report the branch of the value's own type, which says
			// more than "matched nothing".
			if first == nil || typeMatches(v, bs) {
				first = err
			}
		}
		if first != nil {
			return first
		}
	}
	// Each allOf branch is checked on its own, as JSON Schema does: a
	// branch's additionalProperties does not see the parent's properties.
	if all, ok := schema["allOf"].([]any); ok {
		for _, b := range all {
			bs, _ := b.(map[string]any)
			if err := d.validate(v, bs, at); err != nil {
				return err
			}
		}
	}
	if not, ok := schema["not"].(map[string]any); ok && d.validate(v, not, at) == nil {
		return fmt.Errorf("%s: %s matches a schema it must not", at, literal(v))
	}
	if _, ok := schema["type"]; ok && !typeMatches(v, schema) {
		return fmt.Errorf("%s: %s is not of type %v", at, jsonType(v), schema["type"])
	}
	if enum, ok := schema["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return equal(e, v) }) {
		return fmt.Errorf("%s: %s is not one of %s", at, literal(v), literal(enum))
	}
	if p, ok := schema["pattern"].(string); ok {
		if s, ok := v.(string); ok {
			re, err := compiled(p)
			if err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
			if !re.MatchString(s) {
				return fmt.Errorf("%s: %q does not match %s", at, s, p)
			}
		}
	}
	if min, ok := number(schema["minLength"]); ok {
		// JSON Schema counts a string's length in code points.
		if s, ok := v.(string); ok && float64(utf8.RuneCountInString(s)) < min {
			return fmt.Errorf("%s: %q is shorter than %v", at, s, min)
		}
	}
	if err := checkBounds(v, schema, at); err != nil {
		return err
	}
	switch v := v.(type) {
	case map[string]any:
		return d.validateObject(v, schema, at)
	case []any:
		return d.validateArray(v, schema, at)
	}
	return nil
}

func (d *Document) validateObject(v map[string]any, schema map[string]any, at string) error {
	props, _ := schema["properties"].(map[string]any)
	for _, r := range asStrings(schema["required"]) {
		if _, ok := v[r]; !ok {
			return fmt.Errorf("%s: missing required field %q", at, r)
		}
	}
	for _, k := range sortedKeys(v) {
		if ps, ok := props[k].(map[string]any); ok {
			if err := d.validate(v[k], ps, at+"."+k); err != nil {
				return err
			}
			continue
		}
		switch extra := schema["additionalProperties"].(type) {
		case bool:
			if !extra {
				return fmt.Errorf("%s: unknown field %q", at, k)
			}
		case map[string]any:
			if err := d.validate(v[k], extra, at+"."+k); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Document) validateArray(v []any, schema map[string]any, at string) error {
	if n, ok := number(schema["minItems"]); ok && float64(len(v)) < n {
		return fmt.Errorf("%s: %d items, want at least %v", at, len(v), n)
	}
	prefix, _ := schema["prefixItems"].([]any)
	for i, item := range v {
		var s map[string]any
		if i < len(prefix) {
			s, _ = prefix[i].(map[string]any)
		} else {
			s, _ = schema["items"].(map[string]any)
		}
		if s == nil {
			continue
		}
		if err := d.validate(item, s, fmt.Sprintf("%s[%d]", at, i)); err != nil {
			return err
		}
	}
	return nil
}

func checkBounds(v any, schema map[string]any, at string) error {
	n, isNumber := number(v)
	if !isNumber {
		return nil
	}
	if min, ok := number(schema["minimum"]); ok && n < min {
		return fmt.Errorf("%s: %v is below %v", at, n, min)
	}
	if max, ok := number(schema["maximum"]); ok && n > max {
		return fmt.Errorf("%s: %v is above %v", at, n, max)
	}
	if min, ok := number(schema["exclusiveMinimum"]); ok && n <= min {
		return fmt.Errorf("%s: %v is not above %v", at, n, min)
	}
	return nil
}

func compiled(p string) (*regexp.Regexp, error) {
	patternsMu.Lock()
	defer patternsMu.Unlock()
	if re, ok := patterns[p]; ok {
		return re, nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", p, err)
	}
	patterns[p] = re
	return re, nil
}

// typeMatches reports whether v has the schema's type. A schema with no
// type matches anything.
func typeMatches(v any, schema map[string]any) bool {
	var types []string
	switch t := schema["type"].(type) {
	case string:
		types = []string{t}
	case []any:
		types = asStrings(t)
	default:
		return true
	}
	got := jsonType(v)
	for _, t := range types {
		if t == got || (t == "number" && got == "integer") {
			return true
		}
	}
	return false
}

func jsonType(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if _, err := v.Int64(); err == nil {
			return "integer"
		}
		return "number"
	case float64:
		if v == float64(int64(v)) {
			return "integer"
		}
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return fmt.Sprintf("%T", v)
}

func literal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	}
	return 0, false
}

func equal(a, b any) bool {
	if x, ok := number(a); ok {
		y, ok := number(b)
		return ok && x == y
	}
	return reflect.DeepEqual(a, b)
}

func asStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
