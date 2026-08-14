package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	poolagent "github.com/obot-platform/discobox/pool-agent"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := poolagent.ExecSystemdChildIfRequested(); err != nil {
		logger.Error("exec systemd child failed", "error", err)
		os.Exit(1)
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
