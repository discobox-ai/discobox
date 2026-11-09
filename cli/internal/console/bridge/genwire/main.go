// Command genwire writes the desktop bridge's wire schema and the desktop
// app's Rust types (ADR 26-11-07-289 §3). The schema is generated from the Go
// structs in cli/internal/console/bridge, which are the wire; the Rust types
// are generated from the schema, never from the Go, so what the app reads is
// exactly what the schema says. `go tool task generate` runs it, and `task
// verify` fails when either file is stale.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

func main() {
	schemaPath := flag.String("schema", "", "path to write the JSON Schema to")
	rustPath := flag.String("rust", "", "path to write the Rust types to, generated from that schema")
	flag.Parse()
	if *schemaPath == "" || *rustPath == "" {
		fail(fmt.Errorf("-schema and -rust are required"))
	}
	schema, err := wireSchema(newSources())
	if err != nil {
		fail(err)
	}
	encoded, err := encodeSchema(schema)
	if err != nil {
		fail(err)
	}
	write(*schemaPath, encoded)

	// The Rust is generated from the schema as written, read back: nothing
	// reaches it that the schema does not carry.
	var read Document
	if err := json.Unmarshal(encoded, &read); err != nil {
		fail(fmt.Errorf("reading the schema back: %w", err))
	}
	rust, err := generateRust(&read)
	if err != nil {
		fail(err)
	}
	write(*rustPath, rust)
}

func encodeSchema(d *Document) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func write(path string, body []byte) {
	if err := os.WriteFile(path, body, 0o644); err != nil { //nolint:gosec // G306: both are checked-in source.
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "genwire: %v\n", err)
	os.Exit(1)
}
