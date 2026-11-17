package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Unions of event schemas, by the document's name for them: the event
// stream's frames and the session log's items.
const (
	EventUnion   = "V2Event"
	LogItemUnion = "SessionLogItem"
)

// SpovPrefix begins the type of a frame spov writes itself, which no
// document describes.
const SpovPrefix = "spov."

// eventTypes maps each event type of a union to its schema, and holds
// the schemas whose type is a pattern.
type eventTypes struct {
	byType    map[string]map[string]any
	patterned []patternedType
}

type patternedType struct {
	re     *regexp.Regexp
	schema map[string]any
}

// CheckEvent validates one frame of the event stream against its type's
// schema in the committed document.
//
// @decision(DL-145)
func CheckEvent(frame []byte) error { return checkIn(EventUnion, frame) }

// CheckLogItem validates one item of a session's durable log against
// its type's schema in the committed document.
//
// @decision(DL-145)
func CheckLogItem(frame []byte) error { return checkIn(LogItemUnion, frame) }

func checkIn(union string, frame []byte) error {
	d, err := Load()
	if err != nil {
		return err
	}
	return d.CheckFrame(union, frame)
}

// CheckFrame reads a frame's type, finds that type's schema among the
// union's branches and validates the frame against it. A type the union
// does not list is an error; one that begins spov. is not checked.
//
// @decision(DL-145)
func (d *Document) CheckFrame(union string, frame []byte) error {
	dec := json.NewDecoder(bytes.NewReader(frame))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("frame is not JSON: %w", err)
	}
	if dec.More() {
		return errors.New("frame has data after its JSON value")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("frame is a %s, not an object", jsonType(v))
	}
	typ, ok := obj["type"].(string)
	if !ok {
		return errors.New("frame has no type")
	}
	if strings.HasPrefix(typ, SpovPrefix) {
		return nil
	}
	types, err := d.eventTypes(union)
	if err != nil {
		return err
	}
	schema, ok := types.byType[typ]
	for _, p := range types.patterned {
		if !ok && p.re.MatchString(typ) {
			schema, ok = p.schema, true
		}
	}
	if !ok {
		return fmt.Errorf("OpenCode %s's %s has no event type %q", Version(), union, typ)
	}
	return d.validate(v, schema, typ)
}

// eventTypes flattens a union's branches, through $ref and nested
// anyOf, into the schema of each event type it names.
func (d *Document) eventTypes(union string) (*eventTypes, error) {
	d.unionsMu.Lock()
	defer d.unionsMu.Unlock()
	if types, ok := d.unions[union]; ok {
		return types, nil
	}
	root, ok := d.schemas[union].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("OpenCode %s has no schema %q", Version(), union)
	}
	types := &eventTypes{byType: map[string]map[string]any{}}
	var add func(s map[string]any, depth int) error
	add = func(s map[string]any, depth int) error {
		s, err := d.resolve(s)
		if err != nil {
			return fmt.Errorf("%s: %w", union, err)
		}
		if branches, ok := s["anyOf"].([]any); ok && s["properties"] == nil {
			if depth > 8 {
				return fmt.Errorf("%s: anyOf nested too deep", union)
			}
			for _, b := range branches {
				bs, ok := b.(map[string]any)
				if !ok {
					return fmt.Errorf("%s: a branch that is not a schema", union)
				}
				if err := add(bs, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		props, _ := s["properties"].(map[string]any)
		ts, _ := props["type"].(map[string]any)
		if names := asStrings(ts["enum"]); len(names) > 0 {
			for _, n := range names {
				types.byType[n] = s
			}
			return nil
		}
		if p, ok := ts["pattern"].(string); ok {
			re, err := compiled(p)
			if err != nil {
				return fmt.Errorf("%s: %w", union, err)
			}
			types.patterned = append(types.patterned, patternedType{re: re, schema: s})
			return nil
		}
		return fmt.Errorf("%s: a branch whose type is neither an enum nor a pattern", union)
	}
	if err := add(root, 0); err != nil {
		return nil, err
	}
	if d.unions == nil {
		d.unions = map[string]*eventTypes{}
	}
	d.unions[union] = types
	return types, nil
}

// envelope wraps data in the fields an event of type typ requires
// besides its data: id, created, and durable when the type has one.
func envelope(typ string, seq int64, data string) ([]byte, error) {
	d, err := Load()
	if err != nil {
		return nil, err
	}
	var fields struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal([]byte(data), &fields); err != nil {
		return nil, fmt.Errorf("%s: data is not a JSON object: %w", typ, err)
	}
	types, err := d.eventTypes(EventUnion)
	if err != nil {
		return nil, err
	}
	schema, ok := types.byType[typ]
	if !ok {
		return nil, fmt.Errorf("OpenCode %s has no event type %q", Version(), typ)
	}
	frame := map[string]any{
		"id": fmt.Sprintf("evt_%s_%d", strings.ReplaceAll(typ, ".", "_"), seq), "created": 1794165467000 + seq,
		"type": typ, "data": json.RawMessage(data),
	}
	if slices.Contains(asStrings(schema["required"]), "durable") {
		version, err := d.durableVersion(schema)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", typ, err)
		}
		frame["durable"] = map[string]any{"aggregateID": fields.SessionID, "seq": seq, "version": version}
	}
	return json.Marshal(frame)
}

// durableVersion is the one version a type's durable shape allows.
func (d *Document) durableVersion(schema map[string]any) (any, error) {
	props, _ := schema["properties"].(map[string]any)
	durable, _ := props["durable"].(map[string]any)
	dprops, _ := durable["properties"].(map[string]any)
	version, _ := dprops["version"].(map[string]any)
	version, err := d.resolve(version)
	if err != nil {
		return nil, err
	}
	if enum, _ := version["enum"].([]any); len(enum) == 1 {
		return enum[0], nil
	}
	return nil, errors.New("the durable version is not one value")
}
