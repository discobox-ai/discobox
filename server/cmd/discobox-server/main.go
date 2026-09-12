package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/discobox-ai/discobox/server"
	"github.com/discobox-ai/discobox/version"
)

func main() {
	// The one thing this binary answers without starting anything. The server
	// is configured by a file and the environment and takes no flags (ADR
	// 0096), but it is now downloaded rather than built alongside whatever
	// starts it (ADR 0099), so "what is this binary" has to be a question a
	// staged file can be asked.
	if len(os.Args) > 1 && slices.Contains([]string{"--version", "-version", "-v"}, os.Args[1]) {
		fmt.Println(version.String())
		return
	}

	// And the images a first run here will want, which the CLI that staged this
	// binary asks for before starting it, so it can download them where the
	// user is watching (ADR 0113 §1). It reads the configuration and starts
	// nothing.
	if len(os.Args) > 1 && os.Args[1] == "images" {
		if err := server.PrintImages(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	// Then this: the same binary is also a pool VM, re-executed into a hidden
	// subcommand, and a launcher must do nothing a server does — no database,
	// no listener, no configuration read.
	server.RunVMLauncherIfInvoked()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
