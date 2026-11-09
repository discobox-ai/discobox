package libkrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/providers/libkrun/internal/krunvm"
)

// launcherCommand is the hidden subcommand that is a pool VM.
//
// It replaces the separate discobox-krun binary (ADR 0062 §9). A dedicated
// *process* is still required, because krun_start_enter consumes the one that
// calls it; a dedicated *artifact* never was. Re-executing this binary removes
// the second thing to build, ship, find on PATH, and keep in step with the
// server that spawns it.
//
// The name is prefixed rather than hidden by a flag on a cobra command because
// this has to be recognized before any argument parsing, in the binary running
// the server. That is `discobox-server` alone: the CLI downloads and starts it
// as a separate program (ADR 0099), so the server process owns the VM.
const launcherCommand = "__pool-vm-launcher"

// watchdogFD is the read end of a pipe the server holds open. It is the third
// descriptor because os/exec numbers ExtraFiles from 3.
const watchdogFD = 3

// Startup liveness travels separately from diagnostics so a heartbeat cannot
// bury the last native startup stage in the log tail.
const startupLivenessFD = 4

// RunLauncherIfInvoked runs this process as a pool VM and never returns, or
// returns immediately when the process was started for anything else.
//
// It is called from main before any other work: the launcher must not open a
// database, bind a listener, or read the server's configuration, and being the
// first thing that happens is what guarantees it does none of them.
func RunLauncherIfInvoked() {
	if len(os.Args) < 2 || os.Args[1] != launcherCommand {
		return
	}
	if err := runLauncher(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "discobox pool VM launcher: %v\n", err)
		os.Exit(1)
	}
	// krunvm.Run only returns on failure, so this is unreachable in practice.
	os.Exit(0)
}

// CheckHost probes the configured native software in a disposable launcher.
// First-start selection also requires KVM; an explicitly configured provider
// checks KVM only when booting, so configuring it need not open the device.
func CheckHost(ctx context.Context, cfg Config, requireKVM bool) error {
	if err := krunvm.Supported(); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mode := "--check"
	if requireKVM {
		mode = "--check-host"
	}
	cmd := exec.CommandContext(ctx, executable, launcherCommand, mode, strings.TrimSpace(cfg.LibkrunPath), strings.TrimSpace(cfg.PasstPath))
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("libkrun host prerequisites: %w", ctx.Err())
	}
	if err != nil {
		// The reason becomes a one-line fallback warning (and the provider's
		// error), so whatever the child printed is folded onto one line: a
		// line break there would be escaped to a literal \n.
		return fmt.Errorf("libkrun host prerequisites: %w: %s", err, strings.Join(strings.Fields(string(output)), " "))
	}
	return nil
}

func runLauncher(args []string) error {
	if len(args) == 3 && args[0] == "--check" {
		return krunvm.CheckDependencies(args[1], args[2])
	}

	if len(args) == 3 && args[0] == "--check-host" {
		return krunvm.CheckHost(args[1], args[2])
	}
	if len(args) != 2 || args[0] != "--config" {
		return errors.New("usage: --config <manifest>")
	}
	path := args[1]
	log.Printf("discobox pool VM launcher: reading manifest %s", path)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("manifest path %s must be absolute", path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // The path is this process's only argument and is checked above.
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", path, err)
	}
	var cfg krunvm.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("decode manifest %s: %w", path, err)
	}
	watchServerProcess()
	stop := reportStartupLiveness()
	defer stop()
	return krunvm.Run(cfg)
}

// reportStartupLiveness confirms that the launcher can still schedule work
// while libkrun boots on its locked OS thread. The driver closes the reader
// once sockets are ready, which stops this reporter for the rest of VM life.
// A manually invoked launcher has no liveness pipe and needs no reporter.
func reportStartupLiveness() func() {
	pipe := os.NewFile(startupLivenessFD, "discobox-startup-liveness")
	info, err := pipe.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		_ = pipe.Close()
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer func() { _ = pipe.Close() }()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			if _, err := pipe.Write([]byte{1}); err != nil {
				return
			}
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	return func() { close(done); _ = pipe.Close() }
}

// watchServerProcess ends this VM when the server that started it goes away.
//
// PR_SET_PDEATHSIG is the driver's half and covers the ordinary case. It cannot
// cover the window between fork and prctl, so the server also passes the read
// end of a pipe it holds the write end of: the descriptor is closed by the
// kernel when the last writer exits, whether or not this process ever got as
// far as arming a signal. Between them, no VM outlives the server that owns it
// — which is the lifetime rule vz and wslc get for free by keeping their VM in
// the server process (ADR 0062 §9).
//
// A launcher started without the pipe — by hand, to debug one — gets no
// watchdog rather than dying instantly. That has to be checked rather than
// assumed: os.NewFile returns nil only for a negative descriptor, so an unset
// fd 3 produces a File whose first Read fails at once, and a watchdog that
// treated that as the server exiting would kill the VM it was asked to start.
// Only a pipe arms this; anything else fd 3 might be is not the server's.
func watchServerProcess() {
	pipe := os.NewFile(watchdogFD, "discobox-server-watchdog")
	info, err := pipe.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		_ = pipe.Close()
		fmt.Fprintln(os.Stderr, "discobox pool VM launcher: no watchdog pipe on fd 3; this VM ends with its parent's death signal alone")
		return
	}
	go func() {
		// Nothing is ever written, so this blocks until EOF.
		var scratch [1]byte
		_, err := pipe.Read(scratch[:])
		fmt.Fprintf(os.Stderr, "discobox pool VM launcher: the server exited (%v); stopping the VM\n", err)
		os.Exit(1)
	}()
}
