package boot

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/discobox-ai/discobox/sandbox-agent/desktop"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// Init is the container entrypoint run as PID 1. It resolves the sandbox user,
// wires the primary volumes and sources into place, then execs the container's
// real init (systemd) so it keeps PID 1. See ADR 0007.
func Init(logger *slog.Logger, args []string) int {
	if logger == nil {
		logger = slog.Default()
	}
	b := newBooter()
	manifest, err := manifestUser()
	if err != nil {
		logger.Error("read the manifest's user", "error", err)
		return 1
	}
	id, err := resolveIdentity(manifest)
	if err != nil {
		logger.Error("resolve sandbox identity", "error", err)
		return 1
	}
	// The pool agent mounted the primary volumes, so this is a pool's sandbox:
	// its bootstrap is in the config volume, and there is something to wire.
	// Without them (a bare `docker run ... bash` debug session) only the user
	// and its home are set up.
	var bootstrap *sandboxconfig.Config
	if dirExists(configMountPath) {
		// Both volumes and sources come from this one pre-bind read; there is
		// no separate image-baked volume file (ADR 0012 §6).
		effective, err := loadEffectiveConfig(manifestPath)
		if err != nil {
			logger.Error("load sandbox config", "error", err)
			return 1
		}
		bootstrap = &effective
	}
	if err := b.provision(logger, id, bootstrap, bootstrap != nil); err != nil {
		logger.Error("provision sandbox", "error", err)
		return 1
	}
	argv, env := execPlan(id, args)
	if isInitTarget(argv[0]) {
		if err := writeUnitDropins(logger, id); err != nil {
			logger.Error("write unit drop-ins", "error", err)
			return 1
		}
	}
	if err := execInit(argv, env); err != nil {
		logger.Error("exec init", "argv", argv, "error", err)
		return 1
	}
	return 0
}

// Provision is the init flow's provisioning for a sandbox that has no PID-1
// flow: a VM guest boots its own init, and the agent is one of its services,
// started once the bootstrap at path has been placed through the running guest
// (ADR 26-10-09-143 §1). It sets up what applies outside a container -- the
// sandbox user and its groups, the working root, the home skeleton,
// ~/.gitconfig, direnv config and the units bound to the sandbox user -- and
// wires nothing: there are no declared volumes (ADR 0126 §2), and sources
// arrive through the intake.
//
// Where a PID-1 flow did run, it has already done all of this, and Provision
// does nothing. The config volume is what says so: only a container has one,
// and Init is what binds it onto /etc/discobox.
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
	return newBooter().provisionWithoutInit(logger, path)
}

// provisionWithoutInit is Provision past its two guards.
//
// It runs once per sandbox, not on every start. The agent's start is not a
// boot: a restart after a crash finds the sandbox user's processes running,
// and seedHome's walk of a home with no volumes mounted under it -- module
// caches, checkouts -- costs seconds and takes back files the user gave to
// somebody else. provisionedPath records whose sandbox this disk was
// provisioned for, on the disk itself, so a stopped and restarted machine is
// not provisioned again either.
func (b *booter) provisionWithoutInit(logger *slog.Logger, path string) error {
	bootstrap, err := loadEffectiveConfig(path)
	if err != nil {
		return err
	}
	if bootstrap.SandboxID == "" {
		return fmt.Errorf("bootstrap %s names no sandbox", path)
	}
	if provisionedFor(provisionedPath) == bootstrap.SandboxID {
		return nil
	}
	id, err := bootstrapIdentity(bootstrap)
	if err != nil {
		return err
	}
	if err := b.provision(logger, id, &bootstrap, false); err != nil {
		return err
	}
	// systemd is already running here, unlike under Init, so what the drop-ins
	// change has to be reloaded, and the session bus's socket -- listening
	// since boot, owned by root -- recreated for the sandbox user. Nothing has
	// connected to it yet: every client is a process the agent starts. The
	// proxy-trust environment the daemons read is rendered from the bootstrap,
	// so it was skipped at this boot and is rendered now. Every later boot
	// finds the drop-ins and the bootstrap on disk and needs none of this.
	if dirExists("/run/systemd/system") {
		if err := writeUnitDropins(logger, id); err != nil {
			return fmt.Errorf("write unit drop-ins: %w", err)
		}
		if err := b.run("systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := b.run("systemctl", "try-restart", "discobox-session-bus.socket"); err != nil {
			return err
		}
		if fileExists("/etc/systemd/system/discobox-render-proxy-env.service") {
			if err := b.run("systemctl", "start", "discobox-render-proxy-env.service"); err != nil {
				return err
			}
		}
	}
	return markProvisioned(provisionedPath, bootstrap.SandboxID)
}

// bootstrapIdentity is who the sandbox runs as, from the bootstrap's own user:
// where no pool agent started the sandbox, nothing injected DISCOBOX_USER_*,
// and sandbox.json's `user` is the same manifest layer that environment is
// rendered from.
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

// provisionedPath records the sandbox this disk was provisioned for. A
// variable so a test does not write the machine's own.
var provisionedPath = "/var/lib/discobox/provisioned"

// provisionedFor is the sandbox ID provisionedPath records, or "" when it
// records none.
func provisionedFor(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func markProvisioned(path, sandboxID string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(sandboxID+"\n"), 0o600)
}

// writeUnitDropins binds the image's per-user units -- the session bus and the
// desktop -- to the sandbox user, and seeds the desktop's starting scale.
func writeUnitDropins(logger *slog.Logger, id identity) error {
	if err := writeDesktopDropins(id); err != nil {
		return fmt.Errorf("desktop: %w", err)
	}
	if err := writeSessionBusDropins(id); err != nil {
		return fmt.Errorf("session bus: %w", err)
	}
	if fileExists("/etc/systemd/system/xfce4-session@.service") {
		// Not fatal. Everything that reads the file tolerates its absence,
		// and a sandbox must not fail to start over a HiDPI hint.
		if err := seedDesktopScale(id); err != nil {
			logger.Warn("seed desktop scale", "error", err)
		}
	}
	return nil
}

// provision does the user setup and, when there is a bootstrap, what it
// declares for the user: groups, the working root, ~/.gitconfig, direnv config
// and desktop launchers. wire says the pool agent mounted the primary volumes
// (Init in a pool's container), and only then are the config volume, the
// declared volumes and the sources wired from them.
func (b *booter) provision(logger *slog.Logger, id identity, bootstrap *sandboxconfig.Config, wire bool) error {
	if err := b.ensureUser(id); err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}
	if bootstrap != nil {
		if err := b.ensureAdditionalGroups(id, bootstrap.SandboxGroups()); err != nil {
			return fmt.Errorf("ensure additional groups: %w", err)
		}
		if wire {
			if err := b.wireConfig(); err != nil {
				return fmt.Errorf("wire config: %w", err)
			}
		}
		// Before the volumes and the sources, per seedWorkingRoot: anything
		// wired onto the working root itself carries its own ownership.
		if err := b.seedWorkingRoot(bootstrap.WorkingRoot(sandboxPaths), id); err != nil {
			return fmt.Errorf("seed working root: %w", err)
		}
		if wire {
			volumes, err := loadResolvedVolumes(id, bootstrap.Volumes)
			if err != nil {
				return fmt.Errorf("resolve volumes: %w", err)
			}
			if err := b.wireVolumes(volumes, id); err != nil {
				return err
			}
			logger.Info("wired sandbox volumes", "count", len(volumes))
		}
	}
	// Seed the home directory after the home volume (if any) is mounted, so the
	// skeleton lands on the persistent volume rather than the image layer.
	if err := b.seedHome(id); err != nil {
		return fmt.Errorf("seed home: %w", err)
	}
	if bootstrap == nil {
		return nil
	}
	// After seedHome: it chowns the home tree recursively, so a .gitconfig
	// written before it would be owned correctly only by coincidence.
	if err := b.seedGitConfig(id, bootstrap.Git); err != nil {
		return fmt.Errorf("seed git config: %w", err)
	}
	// After seedHome for the same reason, and before wireSources only
	// because it reads the manifest's targets rather than the trees: it
	// writes into home, which the recursive chown has already passed over.
	if err := b.seedDirenvConfig(id, bootstrap.Sources); err != nil {
		return fmt.Errorf("seed direnv config: %w", err)
	}
	// After seedHome for the same reason: it chowns the home tree, and
	// these are written straight to their final owner.
	if err := b.seedDesktopLaunchers(id, desktop.LauncherDir); err != nil {
		return fmt.Errorf("seed desktop launchers: %w", err)
	}
	if !wire {
		return nil
	}
	if err := b.wireSources(bootstrap.Sources, id); err != nil {
		return err
	}
	if len(bootstrap.Sources) > 0 {
		logger.Info("wired sandbox sources", "count", len(bootstrap.Sources))
	}
	return nil
}

// execPlan decides what PID 1 execs: systemd/init and root run directly with
// the sandbox env; a non-root, non-init command is dropped to the sandbox user
// via runuser.
func execPlan(id identity, args []string) (argv, env []string) {
	if len(args) == 0 {
		args = []string{"sleep", "infinity"}
	}
	if args[0] == "bash" || args[0] == "/bin/bash" {
		args = append([]string{"bash", "--login"}, args[1:]...)
	}
	if isInitTarget(args[0]) {
		return args, os.Environ()
	}
	userEnv := userEnviron(id)
	// An unconfigured identity means the image's own user already applies
	// (ADR 0025 §5); there is nobody to drop to.
	if !id.configured || id.uid == 0 {
		return args, userEnv
	}
	runuser := append([]string{"runuser", "-u", id.name, "--", "env",
		"HOME=" + id.home, "USER=" + id.name, "LOGNAME=" + id.name}, args...)
	return runuser, os.Environ()
}

func userEnviron(id identity) []string {
	env := os.Environ()
	env = append(env, "HOME="+id.home, "USER="+id.name, "LOGNAME="+id.name)
	return env
}

func isInitTarget(name string) bool {
	switch name {
	case "/sbin/init", "/lib/systemd/systemd", "systemd":
		return true
	}
	return false
}
