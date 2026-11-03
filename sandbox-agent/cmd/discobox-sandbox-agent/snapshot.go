package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pkg/runcca"
	"github.com/discobox-ai/discobox/sandbox-agent/bundlestore"
	"github.com/discobox-ai/discobox/sandbox-agent/intake"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
	"github.com/discobox-ai/discobox/sandbox-agent/proxyenv"
)

// runSnapshot provisions a box as a snapshot is taken (ADR 26-10-16-423 §8,
// ADR 26-11-03-986 §1; see agentlayer.SnapshotPlan), run by the taker as one
// blocking exec in a box it started: it applies the plan's proxy material,
// fetches the bundles its pins name, provisions them with every step blocking,
// cleans the box of what is any one machine's, empties agentlayer.SnapshotDir,
// and seals the box (agentlayer.SealedFile). Its exit status is the result,
// and what went wrong is on its stderr. It leaves the box running: the taker
// stops it and commits it.
func runSnapshot(args []string) int {
	flags := flag.NewFlagSet("discobox-sandbox-agent snapshot", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := takeSnapshot(ctx, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-sandbox-agent snapshot:", err)
		return 1
	}
	return 0
}

// snapshotPlanLimit bounds the plan read from standard input.
const snapshotPlanLimit = 1 << 20

// takeSnapshot provisions the box from the plan on planInput.
func takeSnapshot(ctx context.Context, planInput io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(planInput, snapshotPlanLimit+1))
	if err != nil {
		return fmt.Errorf("read the snapshot plan: %w", err)
	}
	if len(data) > snapshotPlanLimit {
		return errors.New("the snapshot plan is too large")
	}
	plan, err := agentlayer.ParseSnapshotPlan(data)
	if err != nil {
		return err
	}
	// The plan's proxy material is laid under SnapshotDir: never onto the
	// disk the snapshot keeps.
	if err := ephemeralSnapshotDir(ctx); err != nil {
		return err
	}
	if err := awaitSystemd(ctx); err != nil {
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
	trees, err := bundlestore.Trees("/")
	if err != nil {
		return err
	}
	tracker := provision.NewTracker(provisionspec.StatusPath)
	if err := provision.Snapshot(ctx, slog.Default(), tracker, trees, plan.Features); err != nil {
		return err
	}
	// Nothing is sealed that is not provisioned whole: every feature done, as
	// `discobox-provision wait` reads it, and none failed.
	if err := requireProvisioned(provisionspec.StatusPath); err != nil {
		return err
	}
	if err := cleanMachine(ctx, filepath.Join(layout.ProxyDir, "mitm-ca.crt")); err != nil {
		return err
	}
	// The plan's material and the intake's record are the build's.
	if err := emptyDir(agentlayer.SnapshotDir); err != nil {
		return fmt.Errorf("empty %s: %w", agentlayer.SnapshotDir, err)
	}
	if err := disarmAgentStart(); err != nil {
		return err
	}
	return seal()
}

// disarmAgentStart removes the link that has systemd watch for a bootstrap
// and start the agent, so a box made from the snapshot starts its agent as a
// fresh one does: once whoever made it has placed its bootstrap and started
// it — a VM's maker by `discobox-provision apply`, which reloads systemd
// first, and a container's by placing the link again before its start. A box
// whose agent started by itself the moment its bootstrap landed would
// re-execute systemd for its identity (takeIdentity) under the maker's own
// systemctl calls.
func disarmAgentStart() error {
	for _, unit := range agentlayer.Units {
		if unit.Name != agentlayer.AgentStartUnit {
			continue
		}
		link, ok := unit.Link()
		if !ok {
			return nil
		}
		if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("disarm the agent's start: %w", err)
		}
	}
	return nil
}

// requireProvisioned reads the box's provisioning status at path and refuses
// one with any feature not done, or failed: a snapshot of it would hand every
// box made from it a step that did not run.
func requireProvisioned(path string) error {
	status, ok, err := provisionspec.ReadStatus(path)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the box reports no provisioning status")
	}
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(status.Features)) {
		if !status.Features[name].State.Done() {
			problems = append(problems, fmt.Errorf("the %s feature is still %s", name, status.Features[name].State))
			continue
		}
		if status.Features[name].State == provisionspec.StateFailed {
			problems = append(problems, provision.Problem(status, name))
		}
	}
	if status.State == provisionspec.StateFailed && len(problems) == 0 {
		problems = append(problems, fmt.Errorf("provisioning failed: %s", status.Error))
	}
	return errors.Join(problems...)
}

// systemdReadyTimeout bounds the wait for the box's boot, which the exec may
// start ahead of.
const systemdReadyTimeout = 5 * time.Minute

// awaitSystemd waits for a box whose init is systemd to finish booting, then
// has systemd read the units placed in it: a VM's maker places them in a box
// that is already running (ADR 26-10-24-953 §2), and the intake starts the
// proxy's units by name. A box whose init is not systemd has nothing to wait
// for.
func awaitSystemd(ctx context.Context) error {
	if !systemdIsInit() {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, systemdReadyTimeout)
	defer cancel()
	// Its directory under /run appears before systemd answers on its bus:
	// wait for systemctl to reach it and say a state, which it does not
	// while it cannot ("offline" is a manager it cannot talk to yet).
	for {
		out, _ := exec.CommandContext(waitCtx, "systemctl", "is-system-running").Output()
		if state := strings.TrimSpace(string(out)); state != "" && state != "offline" {
			break
		}
		select {
		case <-waitCtx.Done():
			return errors.New("systemd did not come up in the box")
		case <-time.After(250 * time.Millisecond):
		}
	}
	// A boot that ends degraded, or does not settle in time, is still one
	// to provision: its status is not the build's, and a step that needs a
	// unit says so itself.
	_ = exec.CommandContext(waitCtx, "systemctl", "is-system-running", "--wait").Run()
	reloadCtx, cancelReload := context.WithTimeout(ctx, time.Minute)
	defer cancelReload()
	if out, err := exec.CommandContext(reloadCtx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, out)
	}
	return nil
}

// ephemeralSnapshotDir puts agentlayer.SnapshotDir on a tmpfs, unless it is
// a mount already: a container's is its taker's bind, off the layer it
// commits, and a VM's is on the disk it commits, where a key written and
// deleted stays in free blocks. What the box's base shipped there is hidden,
// not copied.
func ephemeralSnapshotDir(ctx context.Context) error {
	if err := os.MkdirAll(agentlayer.SnapshotDir, 0o755); err != nil {
		return err
	}
	//nolint:gosec // G204: a fixed command on a constant path.
	if exec.CommandContext(ctx, "mountpoint", "-q", agentlayer.SnapshotDir).Run() == nil {
		return nil
	}
	//nolint:gosec // G204: a fixed command on a constant path.
	if out, err := exec.CommandContext(ctx, "mount", "-t", "tmpfs", "-o", "mode=0755,nosuid,nodev", "tmpfs", agentlayer.SnapshotDir).CombinedOutput(); err != nil {
		return fmt.Errorf("mount a tmpfs at %s: %w: %s", agentlayer.SnapshotDir, err, out)
	}
	return nil
}

// emptyDir removes everything in dir and keeps dir, which may be a mount
// point.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// seal writes agentlayer.SealedFile, last, so a box made from this one takes
// an identity of its own when its bootstrap arrives (takeIdentity).
func seal() error {
	if err := os.MkdirAll(filepath.Dir(agentlayer.SealedFile), 0o755); err != nil {
		return err
	}
	//nolint:gosec // G306: a marker with nothing in it, which every user may see.
	if err := os.WriteFile(agentlayer.SealedFile, nil, 0o644); err != nil {
		return fmt.Errorf("seal the box: %w", err)
	}
	return nil
}

// takeIdentity gives a box made from a snapshot an identity of its own, once,
// before its agent does anything else (ADR 26-11-03-986 §3): a new machine ID,
// which systemd and the journal take up at once, and new SSH host keys where
// sshd is installed. The snapshot was cleaned of both, but a boot nobody here
// ran — its provider's own, as it captured or copied the disk — may have
// written them again, and every box made from it would share them. A box with
// no seal, made fresh from a base, is left as it is.
func takeIdentity(ctx context.Context) error {
	if _, err := os.Stat(agentlayer.SealedFile); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := newMachineID(ctx); err != nil {
		return err
	}
	if err := newSSHHostKeys(ctx); err != nil {
		return err
	}
	return os.Remove(agentlayer.SealedFile)
}

// newMachineID writes the box a machine ID of its own and has systemd and the
// journal take it up: PID 1 reads it again when it re-executes, and journald
// files its journal under it from its restart.
func newMachineID(ctx context.Context) error {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	// A version 4 UUID, as systemd makes one.
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	if err := writeMachineID(ctx, []byte(hex.EncodeToString(id)+"\n")); err != nil {
		return fmt.Errorf("write the machine ID: %w", err)
	}
	if !systemdIsInit() {
		// Nothing has read the old one yet: the box's init reads this one.
		return nil
	}
	for _, command := range [][]string{
		{"systemctl", "daemon-reexec"},
		{"systemctl", "restart", "systemd-journald.service"},
	} {
		//nolint:gosec // G204: the commands are the fixed list above.
		if out, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(command, " "), err, out)
		}
	}
	return nil
}

// systemdIsInit reports whether systemd is the box's init, PID 1, whether
// or not it has finished coming up: the snapshot command may run before it
// has, and both it and takeIdentity ask the same question.
func systemdIsInit() bool {
	comm, err := os.ReadFile("/proc/1/comm")
	return err == nil && strings.TrimSpace(string(comm)) == "systemd"
}

// newSSHHostKeys replaces the box's SSH host keys, where sshd is installed
// (ssh-keygen alone is the client's), and restarts an sshd already up, which
// serves the keys it read at its start.
func newSSHHostKeys(ctx context.Context) error {
	if err := removeSSHHostKeys(); err != nil {
		return err
	}
	if !sshdInstalled() {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "ssh-keygen", "-A").CombinedOutput(); err != nil {
		return fmt.Errorf("make new SSH host keys: %w: %s", err, out)
	}
	if !systemdIsInit() {
		return nil
	}
	// Debian's unit is ssh.service, others' sshd.service; one not active is
	// left so.
	for _, unit := range []string{"ssh.service", "sshd.service"} {
		if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).Run() != nil {
			continue
		}
		if out, err := exec.CommandContext(ctx, "systemctl", "restart", unit).CombinedOutput(); err != nil {
			return fmt.Errorf("restart %s onto its new host keys: %w: %s", unit, err, out)
		}
		return nil
	}
	return nil
}

// sshdInstalled reports whether the box has an SSH server.
func sshdInstalled() bool {
	for _, path := range []string{"/usr/sbin/sshd", "/usr/bin/sshd"} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// cleanMachine removes what is one machine's, so no two boxes made from a
// snapshot share it: its machine ID, which is left empty, and which each box
// is given anew when its bootstrap arrives (takeIdentity); its SSH host keys, which a package that
// installed sshd generated; and the trust of the build's own proxy CA, at
// buildCA, which discobox-trust-ca.service anchored for the build and each
// box anchors its own of at its boot.
func cleanMachine(ctx context.Context, buildCA string) error {
	if err := untrustCA(buildCA); err != nil {
		return err
	}
	if err := writeMachineID(ctx, nil); err != nil {
		return fmt.Errorf("empty the machine ID: %w", err)
	}
	return removeSSHHostKeys()
}

// writeMachineID writes id as the box's machine ID, the file itself: systemd
// mounts a transient ID over an empty /etc/machine-id, which is unmounted
// first, and D-Bus's own copy, where it is not a link to it, goes so D-Bus
// reads this one.
func writeMachineID(ctx context.Context, id []byte) error {
	if exec.CommandContext(ctx, "mountpoint", "-q", "/etc/machine-id").Run() == nil {
		if out, err := exec.CommandContext(ctx, "umount", "/etc/machine-id").CombinedOutput(); err != nil {
			return fmt.Errorf("unmount the transient machine ID: %w: %s", err, out)
		}
	}
	//nolint:gosec // G306: the machine ID is every user's to read, as systemd makes it.
	if err := os.WriteFile("/etc/machine-id", id, 0o444); err != nil {
		return err
	}
	if info, err := os.Lstat("/var/lib/dbus/machine-id"); err == nil && info.Mode().IsRegular() {
		return os.Remove("/var/lib/dbus/machine-id")
	}
	return nil
}

// removeSSHHostKeys removes the box's SSH host keys, which a package that
// installed sshd generated.
func removeSSHHostKeys() error {
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
