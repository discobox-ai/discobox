package main

import (
	"context"
	"fmt"
	"os"

	"github.com/obot-platform/discobox/server"
)

func main() {
	if err := server.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
