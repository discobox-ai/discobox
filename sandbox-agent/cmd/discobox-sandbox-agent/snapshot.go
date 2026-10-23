package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pkg/runcca"
	"github.com/discobox-ai/discobox/sandbox-agent/intake"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
	"github.com/discobox-ai/discobox/sandbox-agent/proxyenv"
)

// runSnapshot provisions a box as a snapshot is taken (ADR 26-10-16-423 §8;
// see agentlayer.SnapshotPlan), as discobox-snapshot.service: it applies the
// plan's proxy material, fetches the bundles its pins name, provisions them
// with every step blocking, cleans the box of what is any one machine's, and
// writes how that went to agentlayer.SnapshotResultFile. Whatever the
// outcome, it then powers the box off, which is what its taker waits for.
func runSnapshot(args []string) int {
	flags := flag.NewFlagSet("discobox-sandbox-agent snapshot", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var result agentlayer.SnapshotResult
	if err := takeSnapshot(ctx); err != nil {
		slog.Error("provision a snapshot", "error", err)
		result.Error = err.Error()
	}
	status := 0
	if err := writeSnapshotResult(result); err != nil {
		slog.Error("write the snapshot's result", "error", err)
		status = 1
	}
	// --no-block: this unit is one of what the poweroff stops. Not ctx: a
	// stop signal is no reason to leave the box up.
	poweroff, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := exec.CommandContext(poweroff, "systemctl", "--no-block", "poweroff").Run(); err != nil {
		slog.Error("power the snapshot off", "error", err)
		status = 1
	}
	return status
}

func takeSnapshot(ctx context.Context) error {
	data, err := os.ReadFile(agentlayer.SnapshotPlanFile)
	if err != nil {
		return err
	}
	plan, err := agentlayer.ParseSnapshotPlan(data)
	if err != nil {
		return err
	}
	// What is provisioned reaches the network through the pool, as a box's
	// boot does (exportProxyEnv), and so do the units systemd starts.
	vars := proxyenv.Resolve(plan.Env, slices.Sorted(maps.Keys(plan.Env)))
	for name, value := range vars {
		if err := os.Setenv(name, value); err != nil {
			return err
		}
	}
	if err := proxyenv.WriteVars(vars, proxyenv.DefaultOutputPath); err != nil {
		return fmt.Errorf("render the proxy environment: %w", err)
	}
	// The plan's material is applied as a delivery would be, bringing up the
	// bridge every fetch and download goes through. Its record is kept beside
	// the plan, off the filesystem the snapshot is taken of.
	layout := intake.DefaultLayout()
	layout.StatePath = agentlayer.SnapshotStateFile
	in, err := intake.Open(ctx, intake.Config{
		Layout:   layout,
		Owner:    intake.Owner{ProjectID: plan.ProjectID, SandboxID: plan.ID, PoolID: plan.PoolID},
		Pool:     &plan.Pool,
		Activate: intake.UnitActivator(layout.ProxyDir, slog.Default()),
	})
	if err != nil {
		return fmt.Errorf("open the snapshot's runtime config: %w", err)
	}
	if _, err := in.Apply(ctx, plan.RuntimeConfig); err != nil {
		return fmt.Errorf("apply the snapshot's runtime config: %w", err)
	}
	fetched, err := fetchBundles(ctx, in)
	if err != nil {
		return err
	}
	if !fetched {
		return fmt.Errorf("the box has no pins (%s) to provision a snapshot of", agentlayer.PinsFile)
	}
	tracker := provision.NewTracker(provisionspec.StatusPath)
	if err := provision.Snapshot(ctx, slog.Default(), tracker, plan.Features); err != nil {
		return err
	}
	return cleanMachine(ctx, filepath.Join(layout.ProxyDir, "mitm-ca.crt"))
}

// cleanMachine removes what is one machine's, so no two boxes made from a
// snapshot share it: its machine ID, which is left empty for systemd to
// fill at each box's first boot; its SSH host keys, which a package that
// installed sshd generated; and the trust of the build's own proxy CA, at
// buildCA, which discobox-trust-ca.service anchored for the build and each
// box anchors its own of at its boot.
func cleanMachine(ctx context.Context, buildCA string) error {
	if err := untrustCA(buildCA); err != nil {
		return err
	}
	// systemd mounts a transient ID over an empty /etc/machine-id; the file
	// under it is what the snapshot keeps.
	if exec.CommandContext(ctx, "mountpoint", "-q", "/etc/machine-id").Run() == nil {
		if out, err := exec.CommandContext(ctx, "umount", "/etc/machine-id").CombinedOutput(); err != nil {
			return fmt.Errorf("unmount the transient machine ID: %w: %s", err, out)
		}
	}
	//nolint:gosec // G306: the machine ID is every user's to read, as systemd makes it.
	if err := os.WriteFile("/etc/machine-id", nil, 0o444); err != nil {
		return fmt.Errorf("empty the machine ID: %w", err)
	}
	// D-Bus keeps its own copy where it is not a link to /etc/machine-id.
	if info, err := os.Lstat("/var/lib/dbus/machine-id"); err == nil && info.Mode().IsRegular() {
		if err := os.Remove("/var/lib/dbus/machine-id"); err != nil {
			return err
		}
	}
	keys, err := filepath.Glob("/etc/ssh/ssh_host_*")
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := os.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove SSH host key %s: %w", key, err)
		}
	}
	return nil
}

// The trust store discobox-trust-ca.service anchors a box's proxy CA into.
const (
	trustAnchorDir = "/usr/local/share/ca-certificates"
	trustStoreDir  = "/etc/ssl/certs"
)

// untrustCA takes the CA at path back out of the trust store it was anchored
// in (runcca.UntrustAnchor): the anchoring only ever adds to the store.
func untrustCA(path string) error {
	ca, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := runcca.UntrustAnchor(trustStoreDir, []string{trustAnchorDir}, ca); err != nil {
		return fmt.Errorf("untrust the build's proxy CA: %w", err)
	}
	return nil
}

func writeSnapshotResult(result agentlayer.SnapshotResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tmp := agentlayer.SnapshotResultFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, agentlayer.SnapshotResultFile)
}
