package boot

import (
	"fmt"
	"log/slog"
	"os"

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
	id, err := resolveIdentity()
	if err != nil {
		logger.Error("resolve sandbox identity", "error", err)
		return 1
	}
	if err := b.provision(logger, id); err != nil {
		logger.Error("provision sandbox", "error", err)
		return 1
	}
	argv, env := execPlan(id, args)
	if isInitTarget(argv[0]) {
		if err := writeDesktopDropins(id); err != nil {
			logger.Error("write desktop drop-ins", "error", err)
			return 1
		}
	}
	if err := execInit(argv, env); err != nil {
		logger.Error("exec init", "argv", argv, "error", err)
		return 1
	}
	return 0
}

// provision does the user setup and, when the worker has mounted the primary
// volumes, wires the declarative volumes and sources into place.
func (b *booter) provision(logger *slog.Logger, id identity) error {
	if err := b.ensureUser(id); err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}
	worker := dirExists(configMountPath)
	var effective sandboxconfig.Config
	if worker {
		// Both volumes and sources come from this one pre-bind read now that
		// image.json is gone (ADR 0012 §6) — previously this required two
		// separate reads (image.json for volumes, the manifest for sources).
		var err error
		if effective, err = loadEffectiveConfig(); err != nil {
			return fmt.Errorf("load sandbox config: %w", err)
		}
		if err := b.ensureAdditionalGroups(id, effective.SandboxGroups()); err != nil {
			return fmt.Errorf("ensure additional groups: %w", err)
		}
		if err := b.wireConfig(); err != nil {
			return fmt.Errorf("wire config: %w", err)
		}
		volumes, err := loadResolvedVolumes(id, effective.Volumes)
		if err != nil {
			return fmt.Errorf("resolve volumes: %w", err)
		}
		if err := b.wireVolumes(volumes); err != nil {
			return err
		}
		logger.Info("wired sandbox volumes", "count", len(volumes))
	}
	// Seed the home directory after the home volume (if any) is mounted, so the
	// skeleton lands on the persistent volume rather than the image layer.
	if err := b.seedHome(id); err != nil {
		return fmt.Errorf("seed home: %w", err)
	}
	if worker {
		// After seedHome: it chowns the home tree recursively, so a .gitconfig
		// written before it would be owned correctly only by coincidence.
		if err := b.seedGitConfig(id, effective.Git); err != nil {
			return fmt.Errorf("seed git config: %w", err)
		}
		// After seedHome for the same reason, and before wireSources only
		// because it reads the manifest's targets rather than the trees: it
		// writes into home, which the recursive chown has already passed over.
		if err := b.seedDirenvConfig(id, effective.Sources); err != nil {
			return fmt.Errorf("seed direnv config: %w", err)
		}
		if err := b.wireSources(effective.Sources, id); err != nil {
			return err
		}
		if len(effective.Sources) > 0 {
			logger.Info("wired sandbox sources", "count", len(effective.Sources))
		}
	}
	return nil
}

// execPlan mirrors the retired entrypoint.sh tail: systemd/init and root run
// directly with the sandbox env; a non-root, non-init command is dropped to the
// sandbox user via runuser.
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
