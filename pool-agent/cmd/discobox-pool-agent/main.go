package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/gitcache"
	"github.com/discobox-ai/discobox/pool-agent/handover"
	"github.com/discobox-ai/discobox/pool-agent/poolunits"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := poolagent.ExecSystemdChildIfRequested(); err != nil {
		logger.Error("exec systemd child failed", "error", err)
		os.Exit(1)
	}

	// A Git cache's pre-receive hook is this program (gitcache.RunPreReceive):
	// git runs it for a sandbox's push, with what arrived still in quarantine.
	if len(os.Args) > 1 && os.Args[1] == gitcache.PreReceiveCommand {
		os.Exit(gitcache.RunPreReceive(os.Stdin, os.Stderr))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) > 1 && os.Args[1] == poolagent.InMachineFenceCommand {
		if err := poolagent.RunInMachineFence(ctx); err != nil {
			logger.Error("in-machine fence failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == poolagent.UnitsCommand {
		if err := poolagent.RunUnits(ctx, logger, os.Args[2:]); err != nil {
			// Before the drain: a stalled takeover also refuses to move a
			// draining slot, and that is the failure to report.
			if errors.Is(err, poolunits.ErrNotTakenOver) {
				logger.Error("pool units: the new agent has not taken over", "error", err)
				os.Exit(bundle.UnitsTakeOverStalledExitCode)
			}
			if errors.Is(err, poolunits.ErrSlotDraining) {
				logger.Info("pool units moved nothing", "reason", err)
				os.Exit(bundle.UnitsDrainingExitCode)
			}
			logger.Error("pool units failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "buildkit-mediator" {
		if err := poolagent.RunBuildkitMediator(ctx, logger); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("buildkit mediator failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "proxy" {
		err := poolagent.RunProxy(ctx, logger)
		exitDrained(err)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("pool proxy failed", "error", err)
			os.Exit(1)
		}
		return
	}

	err := poolagent.RunAgent(ctx, logger)
	exitDrained(err)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("pool agent failed", "error", err)
		os.Exit(1)
	}
}

// exitDrained ends a process that drained for the one that took over with
// the status its unit does not restart.
func exitDrained(err error) {
	if errors.Is(err, handover.ErrDrained) {
		os.Exit(handover.DrainedExitCode)
	}
}
