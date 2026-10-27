package discovm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// shimSubcommand is the hidden subcommand that is one disco-vm shim: the
// process that boots a local machine and holds it for its whole life, serving
// its control API on loopback.
//
// It is this binary re-executed, as the libkrun launcher is, so the server
// ships no disco-vm binary beside itself (ADR 26-10-09-106 §2). It is the
// local driver's machine.Config.ShimCommand (driverConfig), to which the
// driver appends the shim's own arguments.
const shimSubcommand = "__vm-shim"

func shimCommand(exe, dir, driver, project string) []string {
	return []string{exe, shimSubcommand, "--dir", dir, "--driver", driver, "--project", project}
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
		fmt.Fprintf(os.Stderr, "discobox vm shim: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runShim makes the driver the server made, from the same configuration
// (driverConfig) on the same directory, driver and project, and runs the shim
// it asked for. Canceling ctx shuts its guest down in order.
func runShim(ctx context.Context, args []string) error {
	if len(args) < 6 || args[0] != "--dir" || args[2] != "--driver" || args[4] != "--project" {
		return errors.New("usage: --dir <state dir> --driver <driver> --project <project> <shim arguments>")
	}
	dir, name, project := args[1], args[3], args[5]
	cfg, err := driverConfig(name, project, dir, nil)
	if err != nil {
		return err
	}
	driver, err := machine.New(name, cfg)
	if err != nil {
		return err
	}
	return local.ShimMain(ctx, driver, args[6:])
}
