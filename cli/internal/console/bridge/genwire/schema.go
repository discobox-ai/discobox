package main

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/cli/internal/console/bridge"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

// Document is the wire's JSON Schema (draft 2020-12). Its root is a Message;
// every type the wire carries is in Defs, in the order it is first reached.
// The x- keywords carry what a schema has no word for: the protocol and spec
// versions, the largest frame either side accepts, and the frame kinds the
// bridge carries.
type Document struct {
	Schema      string             `json:"$schema"`
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Ref         string             `json:"$ref"`
	Protocol    int                `json:"x-protocol"`
	SpecVersion int                `json:"x-specVersion"`
	MaxFrame    int                `json:"x-maxFrame"`
	FrameKinds  Ordered[FrameKind] `json:"x-frameKinds"`
	Defs        Ordered[*Schema]   `json:"$defs"`
}

// FrameKind is one kind of frame the bridge carries.
type FrameKind struct {
	Kind        int    `json:"kind"`
	Description string `json:"description"`
}

// Schema is the subset of JSON Schema the wire needs: an object of named
// properties, an array, a map, a string enum (oneOf consts), a $ref, or a
// scalar with the format that names its width.
type Schema struct {
	Ref                  string           `json:"$ref,omitempty"`
	Description          string           `json:"description,omitempty"`
	Type                 string           `json:"type,omitempty"`
	Format               string           `json:"format,omitempty"`
	ContentEncoding      string           `json:"contentEncoding,omitempty"`
	Const                string           `json:"const,omitempty"`
	OneOf                []*Schema        `json:"oneOf,omitempty"`
	Properties           Ordered[*Schema] `json:"properties,omitempty"`
	Required             []string         `json:"required,omitempty"`
	Items                *Schema          `json:"items,omitempty"`
	AdditionalProperties *Schema          `json:"additionalProperties,omitempty"`
}

// The frame kinds the bridge carries (289 §1): its own JSON kind, and the
// execstream frame types a terminal stream uses each way.
var frameKinds = []struct {
	name string
	kind byte
	doc  string
}{
	{"json", bridge.KindJSON, "A JSON message (Message), on stream 0."},
	{"input", frame.Input, "Bytes typed into a stream's terminal (app)."},
	{"stdout", frame.Stdout, "A stream's terminal output (CLI)."},
	{"stderr", frame.Stderr, "A stream's error output (CLI)."},
	{"resize", frame.Resize, "A stream's terminal size, a ResizePayload (app)."},
	{"exit", frame.Exit, "A stream's end; its payload is why, empty when it ended cleanly (CLI)."},
	{"repaint", frame.Repaint, "Asks for a stream's screen again (app); no payload."},
}

const description = "What crosses the pipe between `discobox desktop` and its app: the JSON messages, " +
	"the payloads of the terminal frames, and the frame kinds. Generated from the Go types in " +
	"cli/internal/console/bridge by its genwire command; no field is ever null."

func wireSchema(src *sources) (*Document, error) {
	b := &builder{src: src, names: map[string]reflect.Type{}, building: map[reflect.Type]bool{}}
	for _, root := range []reflect.Type{reflect.TypeFor[bridge.Message](), reflect.TypeFor[frame.ResizePayload]()} {
		if _, err := b.typeSchema(root); err != nil {
			return nil, err
		}
	}
	d := &Document{
		Schema:      "https://json-schema.org/draft/2020-12/schema",
		Title:       "Discobox desktop bridge",
		Description: description,
		Ref:         "#/$defs/Message",
		Protocol:    bridge.Protocol,
		SpecVersion: console.SpecVersion,
		MaxFrame:    bridge.MaxFrame,
		Defs:        b.defs,
	}
	for _, k := range frameKinds {
		d.FrameKinds = append(d.FrameKinds, Entry[FrameKind]{Key: k.name, Value: FrameKind{Kind: int(k.kind), Description: k.doc}})
	}
	return d, nil
}

type builder struct {
	src      *sources
	defs     Ordered[*Schema]
	names    map[string]reflect.Type
	building map[reflect.Type]bool
}

// The names the generated Rust uses itself, which no wire type may take.
// Box and Result may: nothing generated names them, and the app imports them
// under its own names.
var reserved = []string{"Option", "String", "Vec", "HashMap", "Default", "Serialize", "Deserialize",
	"Clone", "Copy", "Debug", "PartialEq", "Eq", "Hash", "Some", "None", "T"}

var rustIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var (
	timeType      = reflect.TypeFor[time.Time]()
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

func (b *builder) typeSchema(t reflect.Type) (*Schema, error) {
	if t != timeType && (t.Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(jsonMarshaler) ||
		t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler)) {
		return nil, fmt.Errorf("%s writes itself, so its fields are not its wire form", t)
	}
	switch {
	case t == timeType:
		return &Schema{Type: "string", Format: "date-time"}, nil
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		return &Schema{Type: "string", ContentEncoding: "base64"}, nil
	}
	switch t.Kind() {
	case reflect.Bool:
		return &Schema{Type: "boolean"}, nil
	case reflect.String:
		if t.PkgPath() == "" {
			return &Schema{Type: "string"}, nil
		}
		return b.define(t, b.enum)
	case reflect.Int, reflect.Int64:
		return &Schema{Type: "integer", Format: "int64"}, nil
	case reflect.Int32:
		return &Schema{Type: "integer", Format: "int32"}, nil
	case reflect.Uint16:
		return &Schema{Type: "integer", Format: "uint16"}, nil
	case reflect.Uint32:
		return &Schema{Type: "integer", Format: "uint32"}, nil
	case reflect.Uint, reflect.Uint64:
		return &Schema{Type: "integer", Format: "uint64"}, nil
	case reflect.Float64:
		return &Schema{Type: "number", Format: "double"}, nil
	case reflect.Slice:
		items, err := b.typeSchema(t.Elem())
		if err != nil {
			return nil, err
		}
		return &Schema{Type: "array", Items: items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("%s: a map's keys must be strings", t)
		}
		values, err := b.typeSchema(t.Elem())
		if err != nil {
			return nil, err
		}
		return &Schema{Type: "object", AdditionalProperties: values}, nil
	case reflect.Struct:
		return b.define(t, b.object)
	default:
		return nil, fmt.Errorf("%s: no wire form for a %s", t, t.Kind())
	}
}

// define adds a named type to the definitions, once, and is a reference to
// it. Two types of one name in different packages are refused: the Rust
// types share one namespace.
func (b *builder) define(t reflect.Type, build func(reflect.Type, *packageDocs) (*Schema, error)) (*Schema, error) {
	if !rustIdent.MatchString(t.Name()) || slices.Contains(reserved, t.Name()) {
		return nil, fmt.Errorf("%s: %q cannot name a Rust type beside the ones the generated code uses", t, t.Name())
	}
	ref := &Schema{Ref: "#/$defs/" + t.Name()}
	if seen, ok := b.names[t.Name()]; ok {
		if seen != t {
			return nil, fmt.Errorf("%s and %s are both named %s on the wire", seen, t, t.Name())
		}
		if b.building[t] {
			return nil, fmt.Errorf("%s contains itself, which the generator does not carry: a pointer to itself has no Rust size, and a list of itself is not supported yet", t)
		}
		return ref, nil
	}
	b.building[t] = true
	defer delete(b.building, t)
	b.names[t.Name()] = t
	// Reserved before it is built, so the definitions are in the order each
	// type is first reached, its own before its fields'.
	i := len(b.defs)
	b.defs = append(b.defs, Entry[*Schema]{Key: t.Name()})
	docs, err := b.src.pkg(t.PkgPath())
	if err != nil {
		return nil, err
	}
	def, err := build(t, docs)
	if err != nil {
		return nil, err
	}
	def.Description = docs.types[t.Name()]
	b.defs[i].Value = def
	return ref, nil
}

// enum is a named string type as the values its constants declare.
func (b *builder) enum(t reflect.Type, docs *packageDocs) (*Schema, error) {
	if name, ok := docs.unread[t.Name()]; ok {
		return nil, fmt.Errorf("%s: the constant %s is not a string literal, so its value cannot be read into the enum", t, name)
	}
	values := docs.consts[t.Name()]
	if len(values) == 0 {
		return nil, fmt.Errorf("%s: a named string on the wire must declare its values as constants", t)
	}
	s := &Schema{Type: "string"}
	for _, v := range values {
		if v.Value == "" {
			return nil, fmt.Errorf("%s: an empty value cannot be told from an absent one", t)
		}
		s.OneOf = append(s.OneOf, &Schema{Const: v.Value, Description: v.Doc})
	}
	return s, nil
}

// object is a struct as encoding/json writes it. A field is required unless
// it is omitted when empty; a pointer must be, since it would otherwise be
// written as null.
func (b *builder) object(t reflect.Type, docs *packageDocs) (*Schema, error) {
	s := &Schema{Type: "object", Required: []string{}}
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			return nil, fmt.Errorf("%s.%s: an embedded field has no wire form here", t, f.Name)
		}
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		options := strings.Split(opts, ",")
		if slices.Contains(options, "string") || slices.Contains(options, "omitzero") {
			return nil, fmt.Errorf("%s.%s: the json option %q is not one the wire uses", t, f.Name, opts)
		}
		omitempty := slices.Contains(options, "omitempty")
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			if !omitempty {
				return nil, fmt.Errorf("%s.%s: a pointer that is not omitted when empty is written as null", t, f.Name)
			}
			ft = ft.Elem()
			// Only a struct's absence reads apart from its zero value here:
			// a pointer to a scalar would lose nil against false, "" or 0.
			if ft.Kind() != reflect.Struct || ft == timeType {
				return nil, fmt.Errorf("%s.%s: a pointer to a %s cannot be told from its zero value in the app; make it a value", t, f.Name, ft)
			}
		}
		// encoding/json writes a struct even when it is empty.
		optional := omitempty && (f.Type.Kind() == reflect.Pointer || ft.Kind() != reflect.Struct)
		prop, err := b.typeSchema(ft)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t, f.Name, err)
		}
		prop.Description = docs.fields[t.Name()+"."+f.Name]
		s.Properties = append(s.Properties, Entry[*Schema]{Key: name, Value: prop})
		if !optional {
			s.Required = append(s.Required, name)
		}
	}
	return s, nil
}

// Entry is one key of an Ordered object.
type Entry[T any] struct {
	Key   string
	Value T
}

// Ordered is a JSON object whose keys keep their order, so the schema and
// the Rust read in the order the Go declares.
type Ordered[T any] []Entry[T]

// MarshalJSON writes the object in order.
func (o Ordered[T]) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	buf.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := enc.Encode(e.Key); err != nil {
			return nil, err
		}
		buf.WriteByte(':')
		if err := enc.Encode(e.Value); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON reads the object in order.
func (o *Ordered[T]) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return fmt.Errorf("an ordered object must be an object")
	}
	*o = nil
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		var v T
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*o = append(*o, Entry[T]{Key: key, Value: v})
	}
	_, err := dec.Token()
	return err
}
