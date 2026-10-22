// Command gensettingsschema writes the JSON Schema for a repository's
// .discobox/settings.json from the tags on sandboxconfig.Settings (ADR
// 26-10-16-423 §11), with the generator the server's and the CLI's
// configuration files use (ADR 0096). It lives here rather than beside
// Settings so the agents that read the file do not build the generator in.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"

	"github.com/discobox-ai/discobox/pkg/configfile"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

func main() {
	out := flag.String("out", "", "path to write the JSON Schema to")
	flag.Parse()
	if *out == "" {
		fail(fmt.Errorf("-out is required"))
	}
	encoded, err := schemaJSON()
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*out, encoded, 0o644); err != nil { //nolint:gosec // G306: the schema is public documentation.
		fail(err)
	}
}

// schema builds the schema from the tags on sandboxconfig.Settings.
func schema() (map[string]any, error) {
	return configfile.Schema(reflect.TypeOf(sandboxconfig.Settings{}), sandboxconfig.SettingsSchemaID,
		"Discobox repository settings",
		"A repository's .discobox/settings.json: the sources it is worked on with, the features its boxes turn off, and its project layer.")
}

// schemaJSON is the schema as it is committed.
func schemaJSON() ([]byte, error) {
	built, err := schema()
	if err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(built, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "gensettingsschema: %v\n", err)
	os.Exit(1)
}
