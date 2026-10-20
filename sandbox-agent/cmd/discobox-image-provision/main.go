// Command discobox-image-provision provisions the platform's features onto an
// image as sandbox-agent/Dockerfile builds it, as `discobox-sandbox-agent
// provision` does. It is a binary of its own, mounted into each of the
// build's provisioning steps and never installed, because it changes only
// when provisioning does: the agent changes with nearly every edit to this
// module, and a step's cache follows what it mounts, so provisioning with the
// agent would rebuild every feature for each of them.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/discobox-ai/discobox/sandbox-agent/provision"
)

func main() {
	flags := flag.NewFlagSet("discobox-image-provision", flag.ContinueOnError)
	records := flags.String("records", provision.RecordsDir, "directory of the image's step records")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: discobox-image-provision [--records DIR] TREE...")
		flags.PrintDefaults()
	}
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if flags.NArg() == 0 {
		flags.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Not the agent: no command on the box is this binary's personality.
	if err := provision.Trees(ctx, slog.Default(), *records, "", flags.Args()...); err != nil {
		slog.Error("provision", "error", err)
		stop()
		os.Exit(1)
	}
}
