package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// RawJSON is a JSON document kept verbatim in a text column: an opaque
// payload a writer stores as it came and a reader hands on as it was stored.
//
// It is json.RawMessage for encoding — it marshals as the document itself,
// never as base64 — with the scanning json.RawMessage lacks. Postgres's
// driver hands a text column over as a string, and database/sql scans a
// string into no []byte type but []byte itself, so a json.RawMessage field
// failed every read of its row there while SQLite, which hands over bytes,
// passed.
type RawJSON []byte

// MarshalJSON writes the document as it is, or null when there is none.
func (r RawJSON) MarshalJSON() ([]byte, error) {
	return json.RawMessage(r).MarshalJSON()
}

// UnmarshalJSON keeps a copy of the document.
func (r *RawJSON) UnmarshalJSON(data []byte) error {
	return (*json.RawMessage)(r).UnmarshalJSON(data)
}

// Value stores the document as text, and no document as NULL.
func (r RawJSON) Value() (driver.Value, error) {
	if r == nil {
		return nil, nil
	}
	return string(r), nil
}

// Scan reads a stored document, from whichever form the driver hands over.
func (r *RawJSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*r = nil
	case string:
		*r = RawJSON(v)
	case []byte:
		*r = append(RawJSON(nil), v...)
	default:
		return fmt.Errorf("cannot scan %T into a JSON document", src)
	}
	return nil
}
