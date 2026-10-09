package discovm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// shimSubcommand is the hidden subcommand that is one disco-vm shim: the
// process that boots a local machine and holds it for its whole life, serving
// the engine's control API on loopback.
//
// It is this binary re-executed, as the libkrun launcher is, so the server
// ships no disco-vm binary beside itself (ADR 26-10-09-106 §2). The engine
// appends the shim's own arguments to the ones shimCommand gives it.
const shimSubcommand = "__discovm-shim"

func shimCommand(exe, root, driver string) []string {
	return []string{exe, shimSubcommand, "--root", root, "--driver", driver}
}

// RunShimIfInvoked runs this process as a disco-vm shim and exits, or returns
// immediately when it was started for anything else. Like the libkrun
// launcher, it must be recognized before anything a server does.
func RunShimIfInvoked() {
	if len(os.Args) < 2 || os.Args[1] != shimSubcommand {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := runShim(ctx, os.Args[2:])
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "discobox disco-vm shim: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runShim opens the engine the server opened, on the same root and driver, and
// runs the shim the engine asked for. Canceling ctx shuts its guest down in
// order.
func runShim(ctx context.Context, args []string) error {
	if len(args) < 4 || args[0] != "--root" || args[2] != "--driver" {
		return errors.New("usage: --root <state dir> --driver <driver> <shim arguments>")
	}
	e, err := openEngine(args[1], args[3])
	if err != nil {
		return err
	}
	return e.ShimMain(ctx, args[4:])
}
