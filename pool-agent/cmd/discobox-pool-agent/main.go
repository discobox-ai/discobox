package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
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

	// The pool-agent bundle's one command (ADR 26-10-23-551 §1), run by a
	// pool machine's provider from the tree it placed.
	if len(os.Args) > 1 && os.Args[1] == "install" {
		if err := runInstall(ctx); err != nil {
			logger.Error("install the pool agent", "error", err)
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

// runInstall installs the bundle tree this agent runs from on this machine.
func runInstall(ctx context.Context) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return bundle.Install(ctx, bundle.Config{
		Executable: executable,
		Run: func(ctx context.Context, env []string, argv ...string) error {
			//nolint:gosec // G204: install's commands are systemctl and apt-get with fixed arguments.
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Env = append(os.Environ(), env...)
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
			}
			return nil
		},
		LookPath: exec.LookPath,
		Log:      os.Stderr,
	})
}
