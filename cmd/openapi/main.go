package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/obot-platform/discobox/internal/server"
)

func main() {
	output := flag.String("output", "openapi.json", "path to write the generated OpenAPI document")
	downgrade := flag.Bool("downgrade", false, "write OpenAPI 3.0.3 for tools that do not support OpenAPI 3.1")
	omitUnsupportedClientOperations := flag.Bool("omit-unsupported-client-operations", false, "omit operations unsupported by the generated REST client")
	flag.Parse()

	_, api := server.NewStubbedRouter()
	doc := api.OpenAPI()
	if *omitUnsupportedClientOperations {
		delete(doc.Paths, "/projects/{projectId}/stream/sse")
	}
	var data []byte
	var err error
	if *downgrade {
		data, err = doc.Downgrade()
		if err == nil {
			var buf bytes.Buffer
			err = json.Indent(&buf, data, "", "  ")
			data = buf.Bytes()
		}
	} else {
		data, err = json.MarshalIndent(doc, "", "  ")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal openapi: %v\n", err)
		os.Exit(1)
	}
	data = append(data, '\n')

	if err := os.WriteFile(*output, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *output, err)
		os.Exit(1)
	}
}
