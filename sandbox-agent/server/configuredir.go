package server

import (
	"fmt"
	"os"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
)

// ensureConfigureDir creates harness.ConfigureDir for the sandbox user, so a
// configure command that does not run as root has somewhere to write its
// result and the control plane has somewhere to seed the previous
// configuration.
//
// It is the sandbox's own layout to provision, not the control plane's: the
// control plane writes to a documented path and cannot chown anything, and
// /run/discobox is root-owned for good reason -- it holds the resolved secrets
// file, the proxy's CA bundles and trust env, and the control-plane and
// buildkit sockets. Widening that directory so the configure command could
// write in it would hand the sandbox user the ability to replace any of them,
// so the writable surface is this one 0700 subdirectory instead.
//
// This runs here rather than in the PID-1 boot flow for the same reason
// boot.WireSecrets does: systemd mounts its own tmpfs over /run after boot has
// exec'd into it, so a directory created during provisioning would be
// shadowed. This process is a systemd unit, ordered after that tmpfs is up.
//
// A nil user means the manifest named nobody, so the configure command inherits
// this process's own identity (ADR 0025 §5) -- root, which already owns the
// directory this creates.
func ensureConfigureDir(user *execs.User) error {
	return ensureConfigureDirAt(harness.ConfigureDir, user)
}

func ensureConfigureDirAt(dir string, user *execs.User) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, and the mode is the
	// half of this that keeps the contents private.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	if user == nil || user.UID == nil || user.GID == nil {
		return nil
	}
	if err := os.Chown(dir, int(*user.UID), int(*user.GID)); err != nil {
		return fmt.Errorf("chown %s: %w", dir, err)
	}
	return nil
}
