package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/obot-platform/disco2/internal/app"
)

func main() {
	output := flag.String("output", "openapi.json", "path to write the generated OpenAPI document")
	flag.Parse()

	_, api := app.NewStubbedRouter()
	data, err := json.MarshalIndent(api.OpenAPI(), "", "  ")
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
