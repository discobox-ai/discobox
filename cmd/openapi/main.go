package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/discobox/internal/server"
	"github.com/obot-platform/discobox/internal/workeragent/sandboxruntime"
	workerserver "github.com/obot-platform/discobox/internal/workeragent/server"
)

func main() {
	output := flag.String("output", "openapi.json", "path to write the generated OpenAPI document")
	downgrade := flag.Bool("downgrade", false, "write OpenAPI 3.0.3 for tools that do not support OpenAPI 3.1")
	omitUnsupportedClientOperations := flag.Bool("omit-unsupported-client-operations", false, "omit operations unsupported by the generated REST client")
	apiName := flag.String("api", "public", "API document to generate: public or worker")
	flag.Parse()

	api, err := openAPI(*apiName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	doc := api.OpenAPI()
	if *omitUnsupportedClientOperations {
		delete(doc.Paths, "/projects/{projectId}/stream/sse")
	}
	var data []byte
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

func openAPI(name string) (huma.API, error) {
	switch name {
	case "public":
		_, api := server.NewStubbedRouter()
		return api, nil
	case "worker":
		_, api := workerserver.NewRouter(workerserver.Config{
			Identity: workerserver.Identity{ProjectID: "project", WorkerID: "worker"},
			Runtime:  sandboxruntime.NewMemorySandboxRuntime(),
		})
		return api, nil
	default:
		return nil, fmt.Errorf("unknown API %q: expected public or worker", name)
	}
}
