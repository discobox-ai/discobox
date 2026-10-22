package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
)

// runProvisionCommand is the discobox-provision personality: how anything in
// the box waits for a feature still being provisioned (ADR 26-10-16-423 §5),
// and what a placeholder command runs.
//
//	discobox-provision wait FEATURE...           block until each is ready
//	discobox-provision run FEATURE COMMAND ARG... a placeholder: wait, then exec
func runProvisionCommand(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: discobox-provision wait FEATURE...\n       discobox-provision run FEATURE COMMAND [ARG...]")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "wait":
		flags := flag.NewFlagSet("discobox-provision wait", flag.ContinueOnError)
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() == 0 {
			return usage()
		}
		return provisionWait(ctx, flags.Args())
	case "run":
		if len(args) < 3 {
			return usage()
		}
		return provisionRun(ctx, args[1], args[2], args[3:])
	}
	return usage()
}

func provisionWait(ctx context.Context, features []string) int {
	status, err := provision.Wait(ctx, provisionspec.StatusPath, features, func(status provisionspec.Status, pending []string) {
		fmt.Fprintf(os.Stderr, "discobox-provision: waiting for %s\n", provision.Describe(status, pending))
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox-provision:", err)
		return 1
	}
	code := 0
	for _, name := range features {
		if err := provision.Problem(status, name); err != nil {
			fmt.Fprintln(os.Stderr, "discobox-provision:", err)
			code = 1
		}
	}
	return code
}

// provisionRun is a placeholder's: say the feature is still provisioning,
// wait for it, and run the real command in this process's place.
func provisionRun(ctx context.Context, feature, command string, args []string) int {
	status, err := provision.Wait(ctx, provisionspec.StatusPath, []string{feature}, func(status provisionspec.Status, pending []string) {
		fmt.Fprintf(os.Stderr, "discobox: %s is not installed yet: the %s feature is still provisioning. Waiting for it: %s\n", command, feature, provision.Describe(status, pending))
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox:", err)
		return 127
	}
	if err := provision.Problem(status, feature); err != nil {
		fmt.Fprintf(os.Stderr, "discobox: %s is not installed: %v\n", command, err)
		return 127
	}
	path, err := provision.LookPath(command, os.Getenv("PATH"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "discobox: the %s feature is ready, but %v\n", feature, err)
		return 127
	}
	argv := append([]string{command}, args...)
	if err := syscall.Exec(path, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "discobox: run %s: %v\n", path, err)
		return 126
	}
	return 0
}
