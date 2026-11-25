package boot

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/fsdurable"
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
		return newBooter().provisionMachine(logger, machineManifestPath, wiringAtBoot)
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
	if _, err := b.provision(logger, id, &bootstrap, wireContainer); err != nil {
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
	return newBooter().provisionMachine(logger, path, wiringAtAgentStart)
}

// WireInPlace is a machine's wiring on the running machine, which
// discobox-wire-in-place.service runs when `discobox-provision apply` starts
// it (ADR 26-11-23-458 §2): where this start has no mounts yet -- a first
// placement, whose machine booted before its bootstrap was there -- it
// provisions and mounts as a boot's wiring does, moving what the machine has
// at each path in first; where it has them, it mounts only the paths and
// source targets the placed bootstrap declares that this start's mounts do
// not have, or that now come from another directory (wireMachineBoot). The
// mounts unchanged, it does nothing.
//
// It runs in PID 1's mount namespace, as a unit, so its binds are the
// machine's whatever namespace the pool's exec ran apply in. No service is
// stopped or restarted around it: since container-runtime state left the
// declared volumes (ADR 26-11-23-993), Docker and containerd hold no path it
// mounts. A failure fails the unit, and so apply, and the pool stops the
// machine; its next boot mounts at boot.
func WireInPlace(logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if dirExists(configMountPath) || runtime.GOOS != "linux" {
		return nil
	}
	return newBooter().provisionMachine(logger, machineManifestPath, wiringInPlace)
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
	skipped, err := b.ensureAdditionalGroups(id, bootstrap.SandboxGroups())
	// The trees are in place, so a group still missing is not coming.
	logSkippedGroups(logger, id.name, skipped, true)
	return err
}

// pinsPath is agentlayer.PinsFile. A variable so a test does not read the
// machine's own.
var pinsPath = agentlayer.PinsFile

// bundlesFollow reports whether the agent fetches the box's bundle trees after
// boot, and so calls JoinFeatureGroups: its pool placed pins it can apply
// (provisionBox). Pins it cannot read fail that fetch, so nothing follows.
func bundlesFollow() bool {
	_, ok, _ := agentlayer.ReadPins(pinsPath)
	return ok
}

// logSkippedGroups says which declared groups the sandbox user was not added
// to because the OS has no such group, so a user missing one is explained in
// the boot log (#1471). final is whether nothing will try again: a warning
// then, since the image or a feature names a group nothing created; info
// while a feature fetched later may still make it.
func logSkippedGroups(logger *slog.Logger, user string, skipped []string, final bool) {
	if len(skipped) == 0 {
		return
	}
	if final {
		logger.Warn("sandbox user not added to groups the OS does not have", "user", user, "groups", skipped)
		return
	}
	logger.Info("sandbox user not added to groups not present yet; joined once the box's features are fetched", "user", user, "groups", skipped)
}

// provisionMachine provisions a Linux machine for the bootstrap at manifest, or,
// on a disk already provisioned for it, makes this start's mounts. when says
// which call it is: Wire's, from discobox-wire.service before sysinit.target;
// WireInPlace's, on the running machine; or the agent's start.
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
func (b *booter) provisionMachine(logger *slog.Logger, manifest string, when machineWiring) error {
	bootstrap, err := loadEffectiveConfig(manifest)
	if err != nil {
		return err
	}
	if bootstrap.SandboxID == "" {
		return fmt.Errorf("bootstrap %s names no sandbox", manifest)
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
	// At the agent's start no unit ran at this boot, and the machine's
	// services are running on the paths: nothing is moved under them
	// (wireMachineStarted).
	mode := wireMachine
	if when == wiringAtAgentStart {
		mode = wireMachineStarted
	}
	if records.Sandbox() == bootstrap.SandboxID {
		return b.wireMachineBoot(logger, bootstrap, id, mode, when == wiringInPlace)
	}
	if err := ensureMachineRoots(); err != nil {
		return err
	}
	if mode == wireMachine {
		b.mounted = machineMounts{}
	}
	workingRoot, err := b.provision(logger, id, &bootstrap, mode)
	if err != nil {
		return err
	}
	if err := markMachineWired(bootstrap.SandboxID, mode, workingRoot, b.mounted); err != nil {
		return err
	}
	if dirExists("/run/systemd/system") {
		if err := b.reloadUserUnits(logger, id); err != nil {
			return err
		}
		// On the running machine -- in place, or at the agent's start --
		// long after boot, the session bus's socket has been listening since
		// then, owned by root, so it is recreated for the sandbox user;
		// nothing has connected to it yet: every client is a process the
		// agent starts. The proxy-trust environment the daemons read is
		// rendered from the bootstrap, so it was skipped at this boot and is
		// rendered now. At boot, before sysinit.target, both start after this
		// and need neither, and starting a unit ordered after it from here
		// would wait on itself. Both units belong to features a box may not
		// have (the session bus is the desktop's), so each is touched only
		// where its file is installed: systemctl fails on a unit it cannot
		// find.
		if when != wiringAtBoot {
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
//
// It reports whether the working root is durable once it returns
// (wireWorkingRoot), which a machine says on its data volume.
func (b *booter) provision(logger *slog.Logger, id identity, bootstrap *sandboxconfig.Config, mode wireMode) (bool, error) {
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
		return false, fmt.Errorf("ensure user: %w", err)
	}
	skipped, err := b.ensureAdditionalGroups(id, bootstrap.SandboxGroups())
	if err != nil {
		return false, fmt.Errorf("ensure additional groups: %w", err)
	}
	// A box its pool placed pins on fetches its trees after this, and
	// JoinFeatureGroups joins the groups only a feature makes (`brew`'s)
	// then; on any other box this is the last chance.
	logSkippedGroups(logger, id.name, skipped, !bundlesFollow())
	if mode == wireContainer {
		if err := b.wireConfig(); err != nil {
			return false, fmt.Errorf("wire config: %w", err)
		}
	}
	// Before the volumes and the sources, per seedWorkingRoot: anything wired
	// onto the working root itself carries its own ownership.
	if err := b.seedWorkingRoot(bootstrap.WorkingRoot(sandboxPaths), id); err != nil {
		return false, fmt.Errorf("seed working root: %w", err)
	}
	volumes, err := loadResolvedVolumes(id, bootstrap.Volumes)
	if err != nil {
		return false, fmt.Errorf("resolve volumes: %w", err)
	}
	if mode != wireContainer {
		volumes = machineVolumes(logger, volumes)
	}
	if mode == wireMachine {
		if err := adoptLegacyLayouts(bootstrap.SandboxID, volumes, bootstrap.Sources, id.uid); err != nil {
			return false, fmt.Errorf("adopt the data volume's layouts: %w", err)
		}
	}
	// Before the declared volumes: nothing declared is its parent, or it
	// would not be wired, and a declared path nested under it is bound onto
	// its directory (ADR 26-11-14-302 §1).
	workingRoot, err := b.wireWorkingRoot(bootstrap.WorkingRoot(sandboxPaths), volumes, bootstrap.Sources, id, mode)
	if err != nil {
		return false, err
	}
	if err := b.wireVolumes(volumes, id, mode); err != nil {
		return false, err
	}
	logger.Info("wired sandbox volumes", "count", len(volumes))
	// Seed the home directory after the home volume (if any) is mounted, so the
	// skeleton lands on the persistent volume rather than the image layer.
	if err := b.seedHome(id); err != nil {
		return false, fmt.Errorf("seed home: %w", err)
	}
	// After seedHome: it fills only an empty home from /etc/skel, so a
	// .gitconfig written before it would leave the sandbox with no skeleton.
	if err := b.seedGitConfig(id, bootstrap.Git); err != nil {
		return false, fmt.Errorf("seed git config: %w", err)
	}
	// After seedHome for the same reason, and before wireSources only
	// because it reads the manifest's targets rather than the trees. It
	// writes straight to its final owner: nothing chowns home afterwards.
	if !b.off(featureDirenv) {
		if err := b.seedDirenvConfig(id, bootstrap.Sources); err != nil {
			return false, fmt.Errorf("seed direnv config: %w", err)
		}
	}
	// After seedHome for the same reason, and written straight to their
	// final owner.
	if !b.off(featureDesktop) {
		if err := b.seedDesktopLaunchers(id, desktop.LauncherDir); err != nil {
			return false, fmt.Errorf("seed desktop launchers: %w", err)
		}
	}
	if mode != wireContainer {
		if err := b.wireMachineSources(bootstrap.Sources, id, mode); err != nil {
			return false, err
		}
	} else if err := b.wireSources(bootstrap.Sources, id); err != nil {
		return false, err
	}
	if len(bootstrap.Sources) > 0 {
		logger.Info("wired sandbox sources", "count", len(bootstrap.Sources))
	}
	return workingRoot, nil
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

// machineWiring is which call provisions a machine (provisionMachine).
type machineWiring int

const (
	// wiringAtBoot is Wire's, from discobox-wire.service at a boot, before
	// anything uses the paths.
	wiringAtBoot machineWiring = iota
	// wiringInPlace is WireInPlace's, on the running machine, from the unit
	// `discobox-provision apply` starts (ADR 26-11-23-458 §2). It moves in
	// as a boot does (wireMachine), and on a start already mounted mounts
	// only what this start's mounts lack.
	wiringInPlace
	// wiringAtAgentStart is the agent's own start, where no unit wired this
	// start (wireMachineStarted).
	wiringAtAgentStart
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
//
// workingRoot is that the working root is durable this start
// (wireWorkingRoot): the line then says so as well
// (sandboxconfig.MachineWorkingRootWired), and only such a line lets the pool
// give the machine a new root, which would otherwise take a working root still
// on it (ADR 26-11-14-302 §4).
//
// mounts is what this start mounted, as the mounts were made
// (booter.mounted), which a wiring in place reads to mount only what they
// lack (machineMountsPath); nil, from the agent's start, which moves nothing
// in, records none, and a wiring in place then mounts nothing more this
// start.
func markMachineWired(sandboxID string, mode wireMode, workingRoot bool, mounts machineMounts) error {
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
	if err := writeDataWiredLine(sandboxID, token, workingRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(machineWiredPath), 0o755); err != nil {
		return err
	}
	if mounts != nil {
		if err := mounts.write(); err != nil {
			return err
		}
	}
	return os.WriteFile(machineWiredPath, []byte(token+"\n"), 0o644) //nolint:gosec // G306: the pool reads it; a token of this start
}

// writeDataWiredLine writes, where the data root is a data volume, the line
// on it naming sandboxID and this start's token
// (sandboxconfig.MachineDataWiredFile).
func writeDataWiredLine(sandboxID, token string, workingRoot bool) error {
	if !machineDataVolume() {
		return nil
	}
	return os.WriteFile(machineDataWiredFile, []byte(sandboxconfig.MachineDataWiredLine(sandboxID, token, workingRoot)+"\n"), 0o644) //nolint:gosec // G306: the pool reads it; it holds only the sandbox's ID and a token
}

// machineMountsPath is where a machine's wiring records what this start
// mounted (machineMounts), beside its token in /run, a new tmpfs at every
// start. A variable so a test does not write the machine's own.
var machineMountsPath = "/run/discobox/machine-mounts.json"

// machineMounts is what a start mounted from the machine's data and cache
// roots: each target path -- the working root, a declared volume's path, a
// source's target -- and the directory on a root it was mounted from. The
// mount sites record it as they mount (booter.mounted).
type machineMounts map[string]string

// add records that target was mounted from dir; a nil m records nothing.
func (m machineMounts) add(target, dir string) {
	if m != nil {
		m[path.Clean(target)] = dir
	}
}

// machineVolumes is volumes without the container runtimes' state
// directories (harness.ContainerRuntimeStatePaths), which a machine never
// mounts: they are no declared volume (ADR 26-11-23-993 §§1, 5), and a box
// made before that ADR still names them in the bootstrap its pool keeps. On a
// running machine Docker and containerd run on them, and mounting under them
// is what ADR 26-11-02-023 §3 rules out; the wiring in place stops no
// service, on the premise that they hold no declared path (ADR 26-11-23-458).
func machineVolumes(logger *slog.Logger, volumes []harness.ResolvedVolume) []harness.ResolvedVolume {
	return slices.DeleteFunc(slices.Clone(volumes), func(v harness.ResolvedVolume) bool {
		if !slices.Contains(harness.ContainerRuntimeStatePaths, path.Clean(v.Path)) {
			return false
		}
		logger.Info("a container runtime's state is no declared volume; leaving it on the machine's root", "path", v.Path)
		return true
	})
}

// wantedMachineMounts is what a wiring that moves in (wireMachine) would
// mount for bootstrap, its home already expanded, by the directories the
// mount sites mount from (workingRootDir, volumeDir, machineSourceDir): the
// working root where nothing durable backs it (workingRootDurable), every
// declared volume a machine mounts (machineVolumes), and every source.
func wantedMachineMounts(volumes []harness.ResolvedVolume, bootstrap sandboxconfig.Config, uid int) machineMounts {
	mounts := machineMounts{}
	root := bootstrap.WorkingRoot(sandboxPaths)
	if !workingRootDurable(root, volumes, bootstrap.Sources) {
		mounts.add(root, workingRootDir())
	}
	for _, v := range volumes {
		mounts.add(v.Path, volumeDir(v, uid))
	}
	for _, s := range bootstrap.Sources {
		mounts.add(s.Target, machineSourceDir(s.Target))
	}
	return mounts
}

// readMachineMounts is what this start recorded it mounted; not ok where it
// recorded nothing: no wiring that moves in has run this start.
func readMachineMounts() (machineMounts, bool, error) {
	raw, err := os.ReadFile(machineMountsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var mounts machineMounts
	if err := json.Unmarshal(raw, &mounts); err != nil {
		return nil, false, fmt.Errorf("read this start's mounts %s: %w", machineMountsPath, err)
	}
	return mounts, true, nil
}

// write records the mounts, whole, as a rename.
func (m machineMounts) write() error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(machineMountsPath), 0o755); err != nil {
		return err
	}
	return fsdurable.WriteFile(machineMountsPath, raw, 0o644)
}

// lacking is what of want m does not have: a target m never mounted, or one
// it mounted from another directory.
func (m machineMounts) lacking(want machineMounts) machineMounts {
	lack := machineMounts{}
	for target, dir := range want {
		if m[target] != dir {
			lack[target] = dir
		}
	}
	return lack
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
// (sandboxconfig.MachineWiredFile) is left alone, unless this is a wiring in
// place (inPlace): that mounts what the placed bootstrap declares and this
// start's mounts lack (wireMachineLacking).
func (b *booter) wireMachineBoot(logger *slog.Logger, bootstrap sandboxconfig.Config, id identity, mode wireMode, inPlace bool) error {
	if fileExists(machineWiredPath) {
		if !inPlace {
			return nil
		}
		return b.wireMachineLacking(logger, bootstrap, id)
	}
	if err := ensureMachineRoots(); err != nil {
		return err
	}
	expanded := bootstrap.ExpandHome(sandboxPaths, id.home)
	volumes, err := loadResolvedVolumes(id, expanded.Volumes)
	if err != nil {
		return fmt.Errorf("resolve volumes: %w", err)
	}
	volumes = machineVolumes(logger, volumes)
	if mode == wireMachine {
		if err := adoptLegacyLayouts(bootstrap.SandboxID, volumes, expanded.Sources, id.uid); err != nil {
			return fmt.Errorf("adopt the data volume's layouts: %w", err)
		}
		b.mounted = machineMounts{}
	}
	workingRoot, err := b.wireWorkingRoot(expanded.WorkingRoot(sandboxPaths), volumes, expanded.Sources, id, mode)
	if err != nil {
		return err
	}
	if err := b.wireVolumes(volumes, id, mode); err != nil {
		return err
	}
	if err := b.wireMachineSources(expanded.Sources, id, mode); err != nil {
		return err
	}
	logger.Info("mounted a machine's volumes", "volumes", len(volumes), "sources", len(expanded.Sources))
	return markMachineWired(bootstrap.SandboxID, mode, workingRoot, b.mounted)
}

// wireMachineLacking mounts, on a running machine already mounted this start,
// the paths and source targets the placed bootstrap declares that this
// start's mounts lack, or that now come from another directory, moving what
// the machine has at each in first, as a boot does (ADR 26-11-23-458 §2).
// The mounts unchanged, it does nothing. A path the bootstrap no longer
// declares stays bound until the next boot.
//
// A lacking path that holds one this start mounted is left to the next boot
// too: moving it in would rename the inner mount point, which the kernel
// refuses, and binding over it would hide the inner mount. A boot mounts
// parents first.
//
// A start whose mounts were not recorded -- made at the agent's start, which
// moves nothing in, or by an agent from before the record -- mounts nothing
// more: what it mounted is not known, and a second mount of a path would
// stack on the first. The next boot mounts what the bootstrap declares.
//
// The record is written after each mount, as the mount sites record it
// (booter.mounted), so an apply cut short is finished by the next: the moves
// are per entry (moveIn), and only what is still lacking is mounted again.
func (b *booter) wireMachineLacking(logger *slog.Logger, bootstrap sandboxconfig.Config, id identity) error {
	mounted, ok, err := readMachineMounts()
	if err != nil {
		return err
	}
	if !ok {
		logger.Info("this start's mounts were not recorded; what the bootstrap newly declares is mounted at the next boot")
		return nil
	}
	expanded := bootstrap.ExpandHome(sandboxPaths, id.home)
	volumes, err := loadResolvedVolumes(id, expanded.Volumes)
	if err != nil {
		return fmt.Errorf("resolve volumes: %w", err)
	}
	volumes = machineVolumes(logger, volumes)
	lacking := mounted.lacking(wantedMachineMounts(volumes, expanded, id.uid))
	for target := range lacking {
		for inner := range mounted {
			if inner != target && sandboxPaths.Within(target, inner) {
				logger.Info("a newly declared path holds one mounted this start; it is mounted at the next boot", "path", target, "mounted", inner)
				delete(lacking, target)
				break
			}
		}
	}
	if len(lacking) == 0 {
		return nil
	}
	b.mounted = mounted
	root := expanded.WorkingRoot(sandboxPaths)
	if _, ok := lacking[path.Clean(root)]; ok {
		if _, err := b.wireWorkingRoot(root, volumes, expanded.Sources, id, wireMachine); err != nil {
			return err
		}
		if err := mounted.write(); err != nil {
			return err
		}
		// The working root is durable now: the data volume's line says so,
		// as a boot's would (ADR 26-11-14-302 §4).
		token, err := os.ReadFile(machineWiredPath)
		if err != nil {
			return err
		}
		if err := writeDataWiredLine(bootstrap.SandboxID, strings.TrimSpace(string(token)), true); err != nil {
			return err
		}
	}
	sortVolumesByDepth(volumes)
	var nv int
	for _, v := range volumes {
		if _, ok := lacking[path.Clean(v.Path)]; !ok {
			continue
		}
		if err := b.wireVolume(v, id, wireMachine); err != nil {
			return fmt.Errorf("wire volume %s: %w", v.Path, err)
		}
		if err := mounted.write(); err != nil {
			return err
		}
		nv++
	}
	var sources []sandboxconfig.Source
	for _, s := range expanded.Sources {
		if _, ok := lacking[path.Clean(s.Target)]; ok {
			sources = append(sources, s)
		}
	}
	if err := b.wireMachineSources(sources, id, wireMachine); err != nil {
		return err
	}
	if err := mounted.write(); err != nil {
		return err
	}
	logger.Info("mounted in place what a machine's placed bootstrap newly declares", "volumes", nv, "sources", len(sources))
	return nil
}
