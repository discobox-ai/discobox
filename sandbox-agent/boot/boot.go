package boot

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/desktop"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
)

// Wire is a sandbox's boot: discobox-wire.service runs it from the box's own
// systemd, before local-fs.target and so before every unit that uses a wired
// path (ADR 26-10-10-767). In a pool's container it provisions the sandbox user
// from the bootstrap's own `user`, binds the config volume onto /etc/discobox
// and the secrets volume onto /run/discobox/secrets, wires the image-declared
// volumes and the sources from the primary volumes the pool agent mounted (ADR
// 0007), writes the units bound to the sandbox user, and reloads the manager so
// they are read. On a machine, which has no config volume, the unit runs where
// its pool placed the bootstrap (its machine drop-in), and mounts the
// machine's volumes and sources from its own data and cache roots
// (provisionMachine, ADR 26-11-02-023).
//
// It runs on every boot: a container's mounts do not survive a restart, nor
// its account a rebuild, and a machine's mounts do not survive a restart
// either. A machine's account and seeding do, and are made once per sandbox
// (provisionMachine keeps its record on the disk).
//
// An error fails the unit, and the unit's failure ends the container, or
// powers the machine off: a sandbox whose wiring failed would write its state
// into the image layer, or onto the bare paths of a machine's disk.
func Wire(logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if !dirExists(configMountPath) {
		// A machine: its mounts are made here, before anything uses them, at
		// every boot. A failure fails the unit, which powers the machine off
		// (ADR 26-11-02-023 §3).
		if runtime.GOOS != "linux" {
			return nil
		}
		return newBooter().provisionMachine(logger, machineManifestPath, true)
	}
	// Both volumes and sources come from this one read of the config volume's
	// copy, before it is bound onto /etc/discobox (ADR 0012 §6).
	bootstrap, err := loadEffectiveConfig(manifestPath)
	if err != nil {
		return err
	}
	id, err := bootstrapIdentity(bootstrap)
	if err != nil {
		return err
	}
	b := newBooter()
	if err := b.provision(logger, id, &bootstrap, wireContainer); err != nil {
		return err
	}
	if err := wireSecrets(); err != nil {
		return fmt.Errorf("wire secrets: %w", err)
	}
	return b.reloadUserUnits(logger, id)
}

// Provision is the agent's start on a machine, which has no config volume: a
// VM guest boots its own init, and the agent is one of its services, started
// once the bootstrap at path has been placed through the running guest (ADR
// 26-10-09-143 §1). Wire has normally provisioned the machine at this boot
// already (provisionMachine), and then this does nothing; it provisions where
// the unit did not run -- an agent layer placed by a pool that places no
// machine drop-in: an older pool agent, which a newer pinned agent can run
// under -- from the agent's start, moving nothing (wireMachineStarted).
//
// Where Wire ran, it has already done all of this, and Provision does nothing.
// The config volume is what says so: only a pool's container has one, and Wire
// is what binds it onto /etc/discobox.
//
// Only a Linux guest has accounts to provision. A macOS or Windows sandbox has
// one account, the image's own (ADR 0145 §5), and nothing here applies to it.
func Provision(logger *slog.Logger, path string) error {
	if logger == nil {
		logger = slog.Default()
	}
	if dirExists(configMountPath) || runtime.GOOS != "linux" {
		return nil
	}
	return newBooter().provisionMachine(logger, path, false)
}

// JoinFeatureGroups wires the features of the box's bundle trees and joins
// the sandbox user to the groups its bootstrap names that they made. A box
// delivered its platform has no trees when Wire or Provision runs at its
// first boot — the agent fetches them after (ADR 26-10-16-423 §7) — so the
// groups only a feature creates (`brew`'s) were not there for the account to
// join. The agent calls this once its trees are in place and before
// readiness, which every session waits for, so the account has them before
// any session starts (§5).
func JoinFeatureGroups(logger *slog.Logger, path string) error {
	if logger == nil {
		logger = slog.Default()
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	bootstrap, err := loadEffectiveConfig(path)
	if err != nil {
		return err
	}
	id, err := bootstrapIdentity(bootstrap)
	if err != nil {
		return err
	}
	b := newBooter()
	b.wireFeatures(logger, bootstrap.Features)
	return b.ensureAdditionalGroups(id, bootstrap.SandboxGroups())
}

// provisionMachine provisions a Linux machine for the bootstrap at path, or,
// on a disk already provisioned for it, makes this start's mounts. atBoot is
// Wire's call, from discobox-wire.service before sysinit.target; otherwise it
// is the agent's start.
//
// Every machine mounts its declared volumes and its sources from
// /.discobox/data and /.discobox/cache: its driver's volumes where it
// attached them, directories on its own disk where it did not (ADR
// 26-11-02-023 §1). What the machine already has at a path is moved into its
// volume and bound (wireVolume, §2).
//
// The account and the seeding run once per sandbox, not on every start. The
// agent's start is not a boot: a restart after a crash finds the sandbox
// user's processes running, and re-running account changes and re-seeding
// under them is a second boot they did not ask for. The box's provisioning
// records (provision.Records) keep whose sandbox this disk was provisioned
// for, on the disk itself, so a stopped and restarted machine is only mounted
// again (wireMachineBoot).
func (b *booter) provisionMachine(logger *slog.Logger, path string, atBoot bool) error {
	bootstrap, err := loadEffectiveConfig(path)
	if err != nil {
		return err
	}
	if bootstrap.SandboxID == "" {
		return fmt.Errorf("bootstrap %s names no sandbox", path)
	}
	records := provision.Records{Dir: recordsDir}
	if records.Sandbox() != bootstrap.SandboxID && provisionedFor(legacyProvisionedPath) == bootstrap.SandboxID {
		// A disk provisioned before the records were kept says so with the
		// marker that preceded them; it is carried into the records once.
		if err := records.RecordSandbox(bootstrap.SandboxID); err != nil {
			return err
		}
		if err := removeLegacyMarker(legacyProvisionedPath); err != nil {
			return err
		}
	}
	id, err := bootstrapIdentity(bootstrap)
	if err != nil {
		return err
	}
	// At the agent's start the unit did not run at this boot, and the
	// machine's services are running on the paths: nothing is moved under
	// them (wireMachineStarted).
	mode := wireMachine
	if !atBoot {
		mode = wireMachineStarted
	}
	if records.Sandbox() == bootstrap.SandboxID {
		return b.wireMachineBoot(logger, bootstrap, id, mode)
	}
	if err := ensureMachineRoots(); err != nil {
		return err
	}
	if err := b.provision(logger, id, &bootstrap, mode); err != nil {
		return err
	}
	if err := markMachineWired(bootstrap.SandboxID, mode); err != nil {
		return err
	}
	if dirExists("/run/systemd/system") {
		if err := b.reloadUserUnits(logger, id); err != nil {
			return err
		}
		// At the agent's start, long after boot, the session bus's socket has
		// been listening since then, owned by root, so it is recreated for
		// the sandbox user; nothing has connected to it yet: every client is a
		// process the agent starts. The proxy-trust environment the daemons
		// read is rendered from the bootstrap, so it was skipped at this boot
		// and is rendered now. At boot, before sysinit.target, both start
		// after this and need neither, and starting a unit ordered after it
		// from here would wait on itself. Both units belong to features a box
		// may not have (the session bus is the desktop's), so each is touched
		// only where its file is installed: systemctl fails on a unit it
		// cannot find.
		if !atBoot {
			if fileExists("/etc/systemd/system/discobox-session-bus.socket") {
				if err := b.run("systemctl", "try-restart", "discobox-session-bus.socket"); err != nil {
					return err
				}
			}
			if fileExists("/etc/systemd/system/discobox-render-proxy-env.service") {
				if err := b.run("systemctl", "start", "discobox-render-proxy-env.service"); err != nil {
					return err
				}
			}
		}
	}
	if err := records.RecordSandbox(bootstrap.SandboxID); err != nil {
		return err
	}
	return removeLegacyMarker(legacyProvisionedPath)
}

// bootstrapIdentity is who the sandbox runs as, from the bootstrap's own user:
// a unit does not see the container's DISCOBOX_USER_* (Wire), and a VM guest
// is given none (Provision), while sandbox.json's `user` is the same manifest
// layer that environment is rendered from (ADR 26-10-10-767 §3).
func bootstrapIdentity(bootstrap sandboxconfig.Config) (identity, error) {
	if err := bootstrap.User.Validate(); err != nil {
		return identity{}, fmt.Errorf("the manifest's user: %w", err)
	}
	id, err := resolveIdentity(&bootstrap.User)
	if err != nil {
		return identity{}, fmt.Errorf("resolve sandbox identity: %w", err)
	}
	return id, nil
}

// The features whose wiring boot does, skipped when provisioning reports one
// off. A feature the box does not report is its image's, and is wired.
const (
	featureDirenv  = "direnv"
	featureDesktop = "desktop"
)

// recordsDir holds the box's provisioning records, the sandbox phase's among
// them (ADR 26-10-16-423 §3). A variable so a test does not write the
// machine's own.
var recordsDir = provision.RecordsDir

// legacyProvisionedPath is the marker that named the sandbox a disk was
// provisioned for before the records did. A variable so a test does not read
// the machine's own.
var legacyProvisionedPath = "/var/lib/discobox/provisioned"

// provisionedFor is the sandbox ID the legacy marker at path records, or ""
// when it records none.
func provisionedFor(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func removeLegacyMarker(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeUnitDropins binds the desktop feature's per-user units -- the session
// bus and the desktop -- to the sandbox user, installed yet or not, and seeds
// the desktop's starting scale. A desktop turned off gets none of them: the
// session bus is the desktop's (ADR 26-09-25-146).
func writeUnitDropins(logger *slog.Logger, id identity, desktopOff bool) error {
	if desktopOff {
		return nil
	}
	if err := writeDesktopDropins(id); err != nil {
		return fmt.Errorf("desktop: %w", err)
	}
	if err := writeSessionBusDropins(id); err != nil {
		return fmt.Errorf("session bus: %w", err)
	}
	// Not fatal. Everything that reads the file tolerates its absence, and a
	// sandbox must not fail to start over a HiDPI hint.
	if err := seedDesktopScale(id); err != nil {
		logger.Warn("seed desktop scale", "error", err)
	}
	return nil
}

// reloadUserUnits writes the units bound to the sandbox user and has systemd,
// already running wherever this is called, read them.
func (b *booter) reloadUserUnits(logger *slog.Logger, id identity) error {
	if err := writeUnitDropins(logger, id, b.off(featureDesktop)); err != nil {
		return fmt.Errorf("write unit drop-ins: %w", err)
	}
	return b.run("systemctl", "daemon-reload")
}

// provision does the user setup and what the bootstrap declares for the user:
// groups, the working root, ~/.gitconfig, direnv config and desktop
// launchers. mode says what the declared volumes and the sources are wired
// from: the primary volumes the pool agent mounted (a pool's container, which
// also binds its config volume), or a machine's own data and cache roots.
func (b *booter) provision(logger *slog.Logger, id identity, bootstrap *sandboxconfig.Config, mode wireMode) error {
	// A skills source's target may be home-relative (ADR 26-11-01-692 §3):
	// expanded here against the home boot resolved, so the direnv whitelist
	// and the wiring below see the place it is mounted.
	expanded := bootstrap.ExpandHome(sandboxPaths, id.home)
	bootstrap = &expanded
	// The features' groups and env go first, before the account exists, so
	// the account joins groups already there (ADR 26-10-16-423 §5); it also
	// decides which features are off, which the steps below read.
	if b.wireFeatures != nil {
		b.wireFeatures(logger, bootstrap.Features)
	}
	if err := b.ensureUser(id); err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}
	if err := b.ensureAdditionalGroups(id, bootstrap.SandboxGroups()); err != nil {
		return fmt.Errorf("ensure additional groups: %w", err)
	}
	if mode == wireContainer {
		if err := b.wireConfig(); err != nil {
			return fmt.Errorf("wire config: %w", err)
		}
	}
	// Before the volumes and the sources, per seedWorkingRoot: anything wired
	// onto the working root itself carries its own ownership.
	if err := b.seedWorkingRoot(bootstrap.WorkingRoot(sandboxPaths), id); err != nil {
		return fmt.Errorf("seed working root: %w", err)
	}
	volumes, err := loadResolvedVolumes(id, bootstrap.Volumes)
	if err != nil {
		return fmt.Errorf("resolve volumes: %w", err)
	}
	if mode == wireMachine {
		if err := adoptLegacyLayouts(bootstrap.SandboxID, volumes, bootstrap.Sources, id.uid); err != nil {
			return fmt.Errorf("adopt the data volume's layouts: %w", err)
		}
	}
	if err := b.wireVolumes(volumes, id, mode); err != nil {
		return err
	}
	logger.Info("wired sandbox volumes", "count", len(volumes))
	// Seed the home directory after the home volume (if any) is mounted, so the
	// skeleton lands on the persistent volume rather than the image layer.
	if err := b.seedHome(id); err != nil {
		return fmt.Errorf("seed home: %w", err)
	}
	// After seedHome: it fills only an empty home from /etc/skel, so a
	// .gitconfig written before it would leave the sandbox with no skeleton.
	if err := b.seedGitConfig(id, bootstrap.Git); err != nil {
		return fmt.Errorf("seed git config: %w", err)
	}
	// After seedHome for the same reason, and before wireSources only
	// because it reads the manifest's targets rather than the trees. It
	// writes straight to its final owner: nothing chowns home afterwards.
	if !b.off(featureDirenv) {
		if err := b.seedDirenvConfig(id, bootstrap.Sources); err != nil {
			return fmt.Errorf("seed direnv config: %w", err)
		}
	}
	// After seedHome for the same reason, and written straight to their
	// final owner.
	if !b.off(featureDesktop) {
		if err := b.seedDesktopLaunchers(id, desktop.LauncherDir); err != nil {
			return fmt.Errorf("seed desktop launchers: %w", err)
		}
	}
	if mode != wireContainer {
		if err := b.wireMachineSources(bootstrap.Sources, id, mode); err != nil {
			return err
		}
	} else if err := b.wireSources(bootstrap.Sources, id); err != nil {
		return err
	}
	if len(bootstrap.Sources) > 0 {
		logger.Info("wired sandbox sources", "count", len(bootstrap.Sources))
	}
	return nil
}

// wireMode is what provision wires the declared volumes and the sources from.
type wireMode int

const (
	// wireContainer wires from the primary volumes the pool agent mounted
	// into its container (Wire).
	wireContainer wireMode = iota
	// wireMachine wires from the machine's own data and cache roots: its
	// driver's volumes, or directories on its disk (ensureMachineRoots),
	// moving what the machine has at each path in first (moveIn). Only at a
	// boot, from discobox-wire.service, before anything uses the paths.
	wireMachine
	// wireMachineStarted is a machine's wiring from the agent's start, where
	// the unit did not run at this boot (an older pool placed no drop-in):
	// the machine's services are running on its paths, and moving their
	// data out from under them loses what they write meanwhile (ADR
	// 26-11-02-023, Alternatives rejected). So nothing is moved, and the
	// machine is wired as it was before that ADR: a data volume's paths
	// mounted with the layout a container's would take (decideLayout) and
	// its sources bound, and roots on the machine's disk mount nothing, its
	// source targets made where they are for the intake to clone into.
	wireMachineStarted
)

// machineWiredPath marks that a machine's data volume is wired for this start
// (sandboxconfig.MachineWiredFile): /run is a tmpfs, so every start begins
// without it, and a second wiring in one start would stack its mounts. A
// variable so a test does not write the machine's own.
var machineWiredPath = sandboxconfig.MachineWiredFile

// machineDataVolume reports whether this box is a machine whose driver
// mounted a data volume of its own at /.discobox/data (disco-vm ADR
// 26-11-03-782): a pool's container has a config volume, and a machine without
// a data volume has a directory of its disk there (ensureMachineRoots). Only
// a data volume is recorded as keeping the sandbox's data
// (markMachineWired). A variable so a test says which.
var machineDataVolume = func() bool {
	if dirExists(configMountPath) || runtime.GOOS != "linux" {
		return false
	}
	mounts, err := mountPoints(mountInfoPath)
	return err == nil && slices.Contains(mounts, dataMountPath)
}

// markMachineWired records that this start's machine wiring has run: a
// random token in /run (sandboxconfig.MachineWiredFile), which a second
// wiring in the same start reads as done -- written by wireMachineStarted
// too, where roots on the disk mounted nothing -- and, where the data root is a data
// volume, on the volume that the sandbox's data is kept there, naming the
// token (sandboxconfig.MachineDataWiredFile), which the pool reads before it
// gives the machine a new root. Roots on the machine's own disk say nothing
// on it: that machine's disk is all it has, and the pool rebuilds it in place
// (ADR 26-11-02-023 §1). A boot's wiring (wireMachine) on a data volume also
// marks its layouts this agent's (adoptedFile): every path it mounted has
// its record now. The agent's start, which records nothing for a cache path
// or a source (decideLayout), does not.
func markMachineWired(sandboxID string, mode wireMode) error {
	if mode == wireMachine && machineDataVolume() {
		if err := markAdopted(); err != nil {
			return err
		}
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(raw[:])
	if machineDataVolume() {
		if err := os.WriteFile(machineDataWiredFile, []byte(sandboxID+" "+token+"\n"), 0o644); err != nil { //nolint:gosec // G306: the pool reads it; it holds only the sandbox's ID and a token
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(machineWiredPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(machineWiredPath, []byte(token+"\n"), 0o644) //nolint:gosec // G306: the pool reads it; a token of this start
}

// machineDataWiredFile is sandboxconfig.MachineDataWiredFile. A variable so a
// test does not write the machine's own.
var machineDataWiredFile = sandboxconfig.MachineDataWiredFile

// dataWiredFor reports whether the line at path (machineDataWiredFile) says
// an agent has wired this data volume for sandboxID: at an earlier start,
// since this start writes it only once its mounts are made. It is what
// says an older agent's unrecorded paths may be there (adoptLegacyLayouts).
func dataWiredFor(path, sandboxID string) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the agent's own record, at a fixed path
	if err != nil {
		return false
	}
	id, _, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")
	return id != "" && id == sandboxID
}

// ensureMachineRoots makes /.discobox/data and /.discobox/cache on a machine
// whose driver attached no volumes there; on one that did they are the
// volumes' mount points already.
func ensureMachineRoots() error {
	for _, root := range []string{dataMountPath, cacheMountPath} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// wireMachineBoot makes a machine's mounts from its own data and cache roots
// again, once per start: its declared volumes and its sources, as the first
// provisioning of this disk made them. A start that mounted them already
// (sandboxconfig.MachineWiredFile) is left alone.
func (b *booter) wireMachineBoot(logger *slog.Logger, bootstrap sandboxconfig.Config, id identity, mode wireMode) error {
	if fileExists(machineWiredPath) {
		return nil
	}
	if err := ensureMachineRoots(); err != nil {
		return err
	}
	expanded := bootstrap.ExpandHome(sandboxPaths, id.home)
	volumes, err := loadResolvedVolumes(id, expanded.Volumes)
	if err != nil {
		return fmt.Errorf("resolve volumes: %w", err)
	}
	if mode == wireMachine {
		if err := adoptLegacyLayouts(bootstrap.SandboxID, volumes, expanded.Sources, id.uid); err != nil {
			return fmt.Errorf("adopt the data volume's layouts: %w", err)
		}
	}
	if err := b.wireVolumes(volumes, id, mode); err != nil {
		return err
	}
	if err := b.wireMachineSources(expanded.Sources, id, mode); err != nil {
		return err
	}
	logger.Info("mounted a machine's volumes", "volumes", len(volumes), "sources", len(expanded.Sources))
	return markMachineWired(bootstrap.SandboxID, mode)
}
