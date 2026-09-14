// Command discobox-release-manifest combines platform build metadata for release publication.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/discobox-ai/discobox/releasemanifest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	output := flag.String("out", "", "output release manifest")
	flag.Parse()
	if *output == "" {
		return fmt.Errorf("-out is required")
	}
	var parts []releasemanifest.Manifest
	for _, path := range flag.Args() {
		part, err := releasemanifest.Read(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		parts = append(parts, part)
	}
	manifest, err := releasemanifest.Merge(parts)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*output, append(data, '\n'), 0o600)
}
