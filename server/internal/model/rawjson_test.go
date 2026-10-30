package model_test

import (
	"encoding/json"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// A document scans from the string Postgres's driver hands over and the bytes
// SQLite's does, stores as text, and encodes as itself, not base64, so the
// API's JSON round-trip carries it unchanged.
func TestRawJSONScansEitherFormAndEncodesAsItself(t *testing.T) {
	for _, src := range []any{`{"a":1}`, []byte(`{"a":1}`)} {
		var doc model.RawJSON
		if err := doc.Scan(src); err != nil || string(doc) != `{"a":1}` {
			t.Fatalf("scan %T = %q, %v", src, doc, err)
		}
	}
	var none model.RawJSON
	if err := none.Scan(nil); err != nil || none != nil {
		t.Fatalf("scan nil = %q, %v", none, err)
	}
	if value, err := none.Value(); err != nil || value != nil {
		t.Fatalf("value of none = %v, %v; want NULL", value, err)
	}
	if value, err := model.RawJSON(`{"a":1}`).Value(); err != nil || value != `{"a":1}` {
		t.Fatalf("value = %v, %v; want text", value, err)
	}
	encoded, err := json.Marshal(struct {
		Doc   model.RawJSON `json:"doc"`
		Empty model.RawJSON `json:"empty,omitempty"`
	}{Doc: model.RawJSON(`{"a":1}`)})
	if err != nil || string(encoded) != `{"doc":{"a":1}}` {
		t.Fatalf("encoded = %s, %v", encoded, err)
	}
	var decoded struct {
		Doc model.RawJSON `json:"doc"`
	}
	if err := json.Unmarshal([]byte(`{"doc": {"b": [1, 2]}}`), &decoded); err != nil || string(decoded.Doc) != `{"b": [1, 2]}` {
		t.Fatalf("decoded = %s, %v", decoded.Doc, err)
	}
}
