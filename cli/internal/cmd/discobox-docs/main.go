// discobox-docs exports the CLI reference without running a CLI command.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/discobox-ai/discobox/cli/internal/cli"
)

func main() {
	output := flag.String("output", "docs/public/cli.md", "reference file to write or check")
	check := flag.Bool("check", false, "fail if the reference differs from the file")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "discobox-docs takes no positional arguments")
		os.Exit(2)
	}
	// The exporter owns this process. Never let a developer's credentials,
	// endpoint, or channel executable name become part of the public manual.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "DISCOBOX_") {
			_ = os.Unsetenv(key)
		}
	}
	os.Args[0] = "discobox"
	var rendered bytes.Buffer
	if err := cli.WriteReference(&rendered); err != nil {
		fail(err)
	}
	if *check {
		old, err := os.ReadFile(*output)
		if err != nil {
			fail(err)
		}
		if !bytes.Equal(old, rendered.Bytes()) {
			fail(fmt.Errorf("%s is stale; run go tool task docs:generate", *output))
		}
	} else if err := os.WriteFile(*output, rendered.Bytes(), 0o600); err != nil {
		fail(err)
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
