// Command genschema writes the JSON Schema for the server configuration file
// from the tags on config.Config (ADR 0096 §2, configuration file).
//
// The struct is the source of truth. This emits the artifact an operator's
// editor reads, and `task verify` fails when the two have drifted, which is
// what keeps "all valid configuration is in the file" true rather than
// aspirational.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/discobox-ai/discobox/server/internal/config"
)

func main() {
	out := flag.String("out", "", "path to write the JSON Schema to")
	example := flag.String("example", "", "path to write the commented reference file to")
	flag.Parse()
	if *out == "" && *example == "" {
		fmt.Fprintln(os.Stderr, "genschema: -out or -example is required")
		os.Exit(1)
	}
	if *out != "" {
		schema, err := config.Schema()
		if err != nil {
			fail(err)
		}
		encoded, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			fail(err)
		}
		write(*out, append(encoded, '\n'))
	}
	if *example != "" {
		body, err := config.ExampleYAML()
		if err != nil {
			fail(err)
		}
		write(*example, body)
	}
}

func write(path string, body []byte) {
	if err := os.WriteFile(path, body, 0o644); err != nil { //nolint:gosec // G306: both artifacts are public documentation.
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "genschema: %v\n", err)
	os.Exit(1)
}
