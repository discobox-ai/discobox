package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/obot-platform/disco2/internal/workeragent"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := workeragent.RunCommand(ctx, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("worker agent failed", "error", err)
		os.Exit(1)
	}
}
