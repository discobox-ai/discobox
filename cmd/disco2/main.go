package main

import (
	"context"
	"fmt"
	"os"

	"github.com/obot-platform/disco2/internal/cli"
)

func main() {
	if err := cli.Execute(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
