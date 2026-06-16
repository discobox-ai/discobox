package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/obot-platform/discobox/worker-agent/workeragent"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := workeragent.ExecSystemdChildIfRequested(); err != nil {
		logger.Error("exec systemd child failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := workeragent.RunAgent(ctx, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("worker agent failed", "error", err)
		os.Exit(1)
	}
}
