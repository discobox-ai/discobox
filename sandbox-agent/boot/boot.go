package boot

import (
	"fmt"
	"log/slog"
	"os"
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
	if worker {
		if err := b.wireConfig(); err != nil {
			return fmt.Errorf("wire config: %w", err)
		}
		volumes, err := loadImageVolumes(id)
		if err != nil {
			return fmt.Errorf("load image volumes: %w", err)
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
		manifest, err := loadManifest()
		if err != nil {
			return fmt.Errorf("load manifest: %w", err)
		}
		if err := b.wireSources(manifest.Sources); err != nil {
			return err
		}
		if len(manifest.Sources) > 0 {
			logger.Info("wired sandbox sources", "count", len(manifest.Sources))
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
	if id.uid == 0 {
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
