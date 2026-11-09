package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type fixtureKind string

type fixtureBody struct {
	Name string `json:"name"`
}

type fixture struct {
	Kind    fixtureKind       `json:"kind"`
	Body    *fixtureBody      `json:"body,omitempty"`
	Inline  fixtureBody       `json:"inline,omitempty"`
	Tags    []string          `json:"tags"`
	Note    string            `json:"note,omitempty"`
	Size    uint16            `json:"size"`
	Answers map[string]string `json:"answers"`
	Data    []byte            `json:"data"`
}

// The fixture's package, as its source would be read: the test's own types
// are in no file the generator reads.
func fixtureSources() *sources {
	path := reflect.TypeFor[fixture]().PkgPath()
	return &sources{packages: map[string]*packageDocs{path: {
		types:  map[string]string{"fixture": "fixture is a test message."},
		fields: map[string]string{"fixture.Note": "Note is a remark."},
		consts: map[string][]enumValue{"fixtureKind": {{Value: "plain", Doc: "A plain one."}, {Value: "fieldErrors"}}, "unreadKind": {{Value: "a"}}},
		unread: map[string]string{"unreadKind": "unreadB"},
	}}}
}

func buildSchema(t *testing.T, v reflect.Type) (*builder, error) {
	t.Helper()
	b := &builder{src: fixtureSources(), names: map[string]reflect.Type{}, building: map[reflect.Type]bool{}}
	_, err := b.typeSchema(v)
	return b, err
}

func def(b *builder, name string) *Schema {
	for _, e := range b.defs {
		if e.Key == name {
			return e.Value
		}
	}
	return nil
}

// A field is required unless encoding/json leaves it out when empty, and a
// struct it always writes; a pointer is a reference that may be absent.
func TestObject(t *testing.T) {
	b, err := buildSchema(t, reflect.TypeFor[fixture]())
	if err != nil {
		t.Fatal(err)
	}
	f := def(b, "fixture")
	if got, want := f.Required, []string{"kind", "inline", "tags", "size", "answers", "data"}; !slices.Equal(got, want) {
		t.Errorf("required %v, want %v", got, want)
	}
	var keys []string
	for _, p := range f.Properties {
		keys = append(keys, p.Key)
	}
	if want := []string{"kind", "body", "inline", "tags", "note", "size", "answers", "data"}; !slices.Equal(keys, want) {
		t.Errorf("properties %v in Go's order %v", keys, want)
	}
	if f.Description != "fixture is a test message." || f.Properties[4].Value.Description != "Note is a remark." {
		t.Errorf("docs: %q, %q", f.Description, f.Properties[4].Value.Description)
	}
	if body := f.Properties[1].Value; body.Ref != "#/$defs/fixtureBody" {
		t.Errorf("body %+v", body)
	}
	if size := f.Properties[5].Value; size.Type != "integer" || size.Format != "uint16" {
		t.Errorf("size %+v", size)
	}
	if data := f.Properties[7].Value; data.Type != "string" || data.ContentEncoding != "base64" {
		t.Errorf("data %+v", data)
	}
	kind := def(b, "fixtureKind")
	if kind == nil || len(kind.OneOf) != 2 || kind.OneOf[0].Const != "plain" || kind.OneOf[0].Description != "A plain one." {
		t.Errorf("the enum %+v", kind)
	}
}

type nullPointer struct {
	Body *fixtureBody `json:"body"`
}

type undeclared string

type withUndeclared struct {
	Kind undeclared `json:"kind"`
}

type withAny struct {
	Value any `json:"value"`
}

type scalarPointer struct {
	Flag *bool `json:"flag,omitempty"`
}

type recursive struct {
	Parent *recursive `json:"parent,omitempty"`
}

type withTime struct {
	At marshaling `json:"at"`
}

type marshaling struct{ s string }

func (m marshaling) MarshalText() ([]byte, error) { return []byte(m.s), nil }

type unreadKind string

type withUnread struct {
	Kind unreadKind `json:"kind"`
}

// What would put null on the wire, or has no form the Rust can read, is
// refused rather than generated.
func TestRefused(t *testing.T) {
	for _, c := range []struct {
		typ  reflect.Type
		want string
	}{
		{reflect.TypeFor[nullPointer](), "written as null"},
		{reflect.TypeFor[withUndeclared](), "declare its values"},
		{reflect.TypeFor[withAny](), "no wire form"},
		{reflect.TypeFor[recursive](), "contains itself"},
		{reflect.TypeFor[scalarPointer](), "cannot be told from its zero value"},
		{reflect.TypeFor[withTime](), "writes itself"},
		{reflect.TypeFor[withUnread](), "not a string literal"},
	} {
		if _, err := buildSchema(t, c.typ); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.typ, err, c.want)
		}
	}
}

// The Rust is the schema's: required fields read strictly, optional ones
// defaulted and left out when empty, enums by their values.
func TestRust(t *testing.T) {
	b, err := buildSchema(t, reflect.TypeFor[fixture]())
	if err != nil {
		t.Fatal(err)
	}
	d := &Document{Title: "Fixture", Protocol: 7, SpecVersion: 1, Defs: b.defs,
		FrameKinds: Ordered[FrameKind]{{Key: "json", Value: FrameKind{Kind: 255, Description: "JSON."}}}}
	encoded, err := encodeSchema(d)
	if err != nil {
		t.Fatal(err)
	}
	var read Document
	if err := json.Unmarshal(encoded, &read); err != nil {
		t.Fatal(err)
	}
	rust, err := generateRust(&read)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"pub const PROTOCOL: i64 = 7;",
		"    pub const JSON: u8 = 255;",
		"pub struct fixture {\n    pub kind: fixtureKind,\n",
		"    #[serde(default, skip_serializing_if = \"Option::is_none\")]\n    pub body: Option<fixtureBody>,\n",
		"    pub inline: fixtureBody,\n",
		"    /// Note is a remark.\n    #[serde(default, skip_serializing_if = \"is_default\")]\n    pub note: String,\n",
		"    pub size: u16,\n",
		"    pub answers: HashMap<String, String>,\n",
		"    #[default]\n    #[serde(rename = \"plain\")]\n    Plain,\n",
		"    #[serde(rename = \"fieldErrors\")]\n    FieldErrors,\n",
		"            fixtureKind::FieldErrors => \"fieldErrors\",\n",
		"fn is_default<T: Default + PartialEq>",
	} {
		if !strings.Contains(string(rust), want) {
			t.Errorf("the Rust lacks %q:\n%s", want, rust)
		}
	}
}

// Every message type has its body in the envelope, under its own name, and
// may be absent: the envelope carries one.
func TestEveryTypeHasItsBody(t *testing.T) {
	d, err := wireSchema(newSources())
	if err != nil {
		t.Fatal(err)
	}
	var message, types *Schema
	for _, e := range d.Defs {
		switch e.Key {
		case "Message":
			message = e.Value
		case "Type":
			types = e.Value
		}
	}
	for _, v := range types.OneOf {
		if !slices.ContainsFunc(message.Properties, func(p Entry[*Schema]) bool { return p.Key == v.Const }) {
			t.Errorf("%q has no body in Message", v.Const)
		}
		if slices.Contains(message.Required, v.Const) {
			t.Errorf("%q's body is required", v.Const)
		}
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{"secretId": "secret_id", "cpuVcpus": "cpu_vcpus", "type": "type", "URLPath": "url_path", "memoryBytes": "memory_bytes"} {
		if got := snake(in); got != want {
			t.Errorf("snake(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"fieldErrors": "FieldErrors", "api-access": "ApiAccess", "done": "Done", "9lives": "", "self": "Self"} {
		if got := pascal(in); got != want {
			t.Errorf("pascal(%q) = %q, want %q", in, got, want)
		}
	}
	if got, _ := ident("box"); got != "r#box" {
		t.Errorf("box is %q", got)
	}
	for _, bad := range []string{"self", "agent-msg", "x.y", ""} {
		if _, err := ident(bad); err == nil {
			t.Errorf("%q was taken as a field", bad)
		}
	}
}

// A value whose variant would be Rust's own Self is refused, not generated
// into an enum that does not compile.
func TestSelfIsNoVariant(t *testing.T) {
	d := &Document{Defs: Ordered[*Schema]{{Key: "Scope", Value: &Schema{Type: "string", OneOf: []*Schema{{Const: "self"}}}}}}
	if _, err := generateRust(d); err == nil || !strings.Contains(err.Error(), "no variant name") {
		t.Fatalf("self as a variant: %v", err)
	}
}
