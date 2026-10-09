// Command genschema writes the JSON Schema and the commented reference for the
// CLI's configuration file from the tags on clientconfig.Config, the way the
// server's genschema does for server.yaml (ADR 0096 §2, configuration file).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/discobox-ai/discobox/cli/internal/clientconfig"
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
		schema, err := clientconfig.Schema()
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
		body, err := clientconfig.ExampleYAML()
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
