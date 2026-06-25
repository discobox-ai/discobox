package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/server"
	"github.com/obot-platform/discobox/sandbox-agent/terminal/shim"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && args[0] == "shim" {
		return runShim(args[1:])
	}
	var configPath string
	flags := flag.NewFlagSet("discobox-sandbox-agent", flag.ContinueOnError)
	flags.StringVar(&configPath, "config", "", "path to sandbox-agent config")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("load config", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx, slog.Default(), server.ConfigFromAgentConfig(cfg)); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("serve sandbox agent", "error", err)
		return 1
	}
	return 0
}

func runShim(args []string) int {
	var cfg shim.Config
	var commandBase64 string
	var rows, cols int
	flags := flag.NewFlagSet("discobox-sandbox-agent shim", flag.ContinueOnError)
	flags.StringVar(&cfg.TerminalID, "terminal-id", "", "agent terminal id")
	flags.StringVar(&cfg.AgentID, "agent-id", "", "agent id")
	flags.StringVar(&cfg.Workdir, "workdir", "", "terminal working directory")
	flags.StringVar(&cfg.SocketPath, "socket", "", "shim unix socket path")
	flags.StringVar(&cfg.RuntimePath, "runtime", "", "shim runtime status path")
	flags.IntVar(&rows, "rows", 0, "initial PTY rows")
	flags.IntVar(&cols, "cols", 0, "initial PTY cols")
	flags.StringVar(&commandBase64, "command", "", "base64 encoded JSON command argv")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if commandBase64 == "" {
		slog.Error("shim command is required")
		return 2
	}
	commandJSON, err := base64.StdEncoding.DecodeString(commandBase64)
	if err != nil {
		slog.Error("decode shim command", "error", err)
		return 2
	}
	if err := json.Unmarshal(commandJSON, &cfg.Command); err != nil {
		slog.Error("parse shim command", "error", err)
		return 2
	}
	cfg.Rows = uint16Dimension(rows)
	cfg.Cols = uint16Dimension(cols)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := shim.Run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("run terminal shim", "terminalID", cfg.TerminalID, "error", err)
		return 1
	}
	return 0
}

func uint16Dimension(value int) uint16 {
	if value <= 0 {
		return 0
	}
	if value > int(^uint16(0)) {
		return ^uint16(0)
	}
	return uint16(value)
}
