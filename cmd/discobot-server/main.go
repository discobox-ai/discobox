package main

import (
	"context"
	"fmt"
	"os"

	"github.com/obot-platform/disco2/internal/server"
)

func main() {
	if err := server.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
