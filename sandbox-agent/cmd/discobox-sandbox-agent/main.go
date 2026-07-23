package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/obot-platform/discobox/proxy/bridge"
	"github.com/obot-platform/discobox/sandbox-agent/boot"
	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
	harnesshooks "github.com/obot-platform/discobox/sandbox-agent/hooks"
	"github.com/obot-platform/discobox/sandbox-agent/server"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if filepath.Base(os.Args[0]) == "discobox-hook-publish" {
		return runHookPublish(args)
	}
	if len(args) > 0 && args[0] == "hook-publish" {
		return runHookPublish(args[1:])
	}
	if len(args) > 0 && args[0] == "exec-shim" {
		return runExecShim(args[1:])
	}
	if len(args) > 0 && args[0] == "proxy-bridge" {
		return runProxyBridge(args[1:])
	}
	if len(args) > 0 && args[0] == "init" {
		return boot.Init(slog.Default(), args[1:])
	}
	var configPath string
	flags := flag.NewFlagSet("discobox-sandbox-agent", flag.ContinueOnError)
	flags.StringVar(&configPath, "config", "", "path to sandbox manifest")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	// Rebind the secrets volume here, not in the PID-1 boot flow: systemd
	// mounts its own tmpfs over /run during its own startup, which happens
	// after boot exec's into it, so a bind placed during PID-1 provisioning
	// would be shadowed. This process starts as a systemd-managed unit,
	// ordered after that tmpfs is already up.
	if err := boot.WireSecrets(); err != nil {
		slog.Error("wire secrets volume", "error", err)
		return 1
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("load config", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx, slog.Default(), server.ConfigFromHarnessConfig(cfg)); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("serve sandbox agent", "error", err)
		return 1
	}
	return 0
}

func runHookPublish(args []string) int {
	var provider, event string
	flags := flag.NewFlagSet("discobox-hook-publish", flag.ContinueOnError)
	flags.StringVar(&provider, "provider", "", "harness hook provider")
	flags.StringVar(&event, "event", "", "harness hook event")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		slog.Error("read hook payload", "error", err)
		return 1
	}
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if !json.Valid(payload) {
		wrapped, err := json.Marshal(map[string]string{"raw": string(payload)})
		if err != nil {
			slog.Error("wrap hook payload", "error", err)
			return 1
		}
		payload = wrapped
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := harnesshooks.Publish(ctx, os.Getenv(harnesshooks.SocketEnv), harnesshooks.Message{
		TerminalID: os.Getenv(harnesshooks.TerminalIDEnv),
		Provider:   provider,
		Event:      event,
		Payload:    payload,
	}); err != nil {
		slog.Error("publish hook", "error", err)
		return 0
	}
	return 0
}

// bridgeConfig mirrors the on-disk config written by the worker harness into the
// sandbox proxy material directory.
type bridgeConfig struct {
	ListenAddress  string `json:"listenAddress"`
	WorkerProxyURL string `json:"workerProxyUrl"`
	MTLSCAPath     string `json:"mtlsCaPath"`
	ClientCertPath string `json:"clientCertPath"`
	ClientKeyPath  string `json:"clientKeyPath"`
}

// runProxyBridge runs the sandbox-local forwarder that routes sandbox proxy
// traffic to the worker proxy over mTLS.
func runProxyBridge(args []string) int {
	var configPath string
	flags := flag.NewFlagSet("discobox-sandbox-agent proxy-bridge", flag.ContinueOnError)
	flags.StringVar(&configPath, "config", "/etc/discobox/proxy/bridge.json", "path to proxy bridge config")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		slog.Error("read proxy bridge config", "error", err)
		return 1
	}
	var cfg bridgeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Error("parse proxy bridge config", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	forwarder, err := bridge.New(ctx, bridge.Config{
		ListenAddress:  cfg.ListenAddress,
		WorkerProxyURL: cfg.WorkerProxyURL,
		MTLSCAPath:     cfg.MTLSCAPath,
		ClientCertPath: cfg.ClientCertPath,
		ClientKeyPath:  cfg.ClientKeyPath,
	})
	if err != nil {
		slog.Error("create proxy bridge", "error", err)
		return 1
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("sandbox proxy bridge serving", "addr", cfg.ListenAddress, "worker", cfg.WorkerProxyURL)
		errCh <- forwarder.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		_ = forwarder.Close()
		return 0
	case err := <-errCh:
		_ = forwarder.Close()
		if err != nil {
			slog.Error("proxy bridge failed", "error", err)
			return 1
		}
		return 0
	}
}

func runExecShim(args []string) int {
	var cfg execs.ShimConfig
	var commandBase64, envBase64, userBase64, metadataBase64 string
	var rows, cols int
	flags := flag.NewFlagSet("discobox-sandbox-agent exec-shim", flag.ContinueOnError)
	flags.StringVar(&cfg.ExecID, "exec-id", "", "sandbox exec id")
	flags.StringVar(&cfg.Unit, "unit", "", "systemd unit name")
	flags.StringVar(&cfg.Workdir, "workdir", "", "exec working directory")
	flags.StringVar(&cfg.SocketPath, "socket", "", "exec shim unix socket path")
	flags.StringVar(&cfg.RuntimePath, "runtime", "", "exec runtime status path")
	flags.StringVar(&cfg.LogDir, "logs", "", "exec log directory")
	flags.IntVar(&rows, "rows", 0, "initial PTY rows")
	flags.IntVar(&cols, "cols", 0, "initial PTY cols")
	flags.BoolVar(&cfg.TTY, "tty", false, "allocate a PTY")
	flags.StringVar(&commandBase64, "command", "", "base64 encoded JSON command argv")
	flags.StringVar(&envBase64, "env", "", "base64 encoded JSON environment")
	flags.StringVar(&userBase64, "user", "", "base64 encoded JSON exec user")
	flags.StringVar(&metadataBase64, "metadata", "", "base64 encoded JSON exec metadata")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if commandBase64 == "" {
		slog.Error("exec shim command is required")
		return 2
	}
	commandJSON, err := base64.StdEncoding.DecodeString(commandBase64)
	if err != nil {
		slog.Error("decode exec shim command", "error", err)
		return 2
	}
	if err := json.Unmarshal(commandJSON, &cfg.Command); err != nil {
		slog.Error("parse exec shim command", "error", err)
		return 2
	}
	if envBase64 != "" {
		envJSON, err := base64.StdEncoding.DecodeString(envBase64)
		if err != nil {
			slog.Error("decode exec shim env", "error", err)
			return 2
		}
		if err := json.Unmarshal(envJSON, &cfg.Env); err != nil {
			slog.Error("parse exec shim env", "error", err)
			return 2
		}
	}
	if userBase64 != "" {
		userJSON, err := base64.StdEncoding.DecodeString(userBase64)
		if err != nil {
			slog.Error("decode exec shim user", "error", err)
			return 2
		}
		if err := json.Unmarshal(userJSON, &cfg.User); err != nil {
			slog.Error("parse exec shim user", "error", err)
			return 2
		}
	}
	if metadataBase64 != "" {
		metadataJSON, err := base64.StdEncoding.DecodeString(metadataBase64)
		if err != nil {
			slog.Error("decode exec shim metadata", "error", err)
			return 2
		}
		if err := json.Unmarshal(metadataJSON, &cfg.Metadata); err != nil {
			slog.Error("parse exec shim metadata", "error", err)
			return 2
		}
	}
	cfg.Rows = uint16Dimension(rows)
	cfg.Cols = uint16Dimension(cols)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := execs.RunShim(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("run exec shim", "execID", cfg.ExecID, "error", err)
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
