package main

import (
	"context"
	"fmt"
	"os"

	"github.com/discobox-ai/discobox/cli/internal/cli"
)

func main() {
	if err := cli.Execute(context.Background()); err != nil {
		if code, ok := cli.ExitCode(err); ok {
			os.Exit(code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
