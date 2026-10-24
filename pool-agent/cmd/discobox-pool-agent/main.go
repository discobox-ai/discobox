package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/gitcache"
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

	if len(os.Args) > 1 && os.Args[1] == "buildkit-mediator" {
		if err := poolagent.RunBuildkitMediator(ctx, logger); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("buildkit mediator failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "proxy" {
		if err := poolagent.RunProxy(ctx, logger); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("pool proxy failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := poolagent.RunAgent(ctx, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("pool agent failed", "error", err)
		os.Exit(1)
	}
}
