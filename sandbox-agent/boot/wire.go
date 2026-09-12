package boot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// wireConfig rebinds the config volume onto /etc/discobox so the running
// sandbox-agent, proxy bridge, and manifest live at their documented paths.
// The bind is recursive so the nested proxy material rides along, and the top
// is read-only to protect the manifest.
func (b *booter) wireConfig() error {
	if !dirExists(configMountPath) {
		// No config volume (e.g. a bare `docker run ... bash` debug session).
		return nil
	}
	if err := os.MkdirAll(etcDiscobox, 0o755); err != nil {
		return err
	}
	return recursiveBindMount(configMountPath, etcDiscobox, true)
}

// WireSecrets rebinds the secrets volume onto /run/discobox/secrets so the
// running sandbox-agent finds the resolved-secrets file at its documented
// path. Unlike the rest of this package, it is called from the long-running
// sandbox-agent server process itself (cmd/discobox-sandbox-agent), not from
// the PID-1 boot flow: systemd (PID 1) mounts its own tmpfs over /run during
// its own startup, after the boot flow has already exec'd into it, so a bind
// mount placed at /run/discobox/secrets during PID-1 provisioning — the way
// wireConfig places one at /etc/discobox — would be silently shadowed. The
// server process starts as a systemd-managed unit, ordered after that tmpfs
// is already in place, so it is the first point at which this bind mount can
// actually survive. The bind is read-only: nothing inside the container
// writes this file, only pool-agent, from the host side.
func WireSecrets() error {
	if !dirExists(secretsMountPath) {
		// No secrets volume (e.g. a bare `docker run ... bash` debug session).
		return nil
	}
	if err := os.MkdirAll(runSecrets, 0o755); err != nil {
		return err
	}
	return recursiveBindMount(secretsMountPath, runSecrets, true)
}

// wireVolumes wires every image-declared data/cache path from its backing
// primary volume onto its target.
//
// id is the identity this sandbox runs as, which decides the cache partition of
// every path that did not declare itself shared (ADR 0094, cache partition).
// It is the resolved
// uid rather than the volume's declared owner: what makes sharing a cache
// directory safe is who ends up writing in it, which is a claim the image makes
// in its scope rather than something derivable from who owns the mountpoint.
func (b *booter) wireVolumes(volumes []harness.ResolvedVolume, id identity) error {
	sortVolumesByDepth(volumes)
	for _, v := range volumes {
		if err := b.wireVolume(v, id); err != nil {
			return fmt.Errorf("wire volume %s: %w", v.Path, err)
		}
	}
	return nil
}

func (b *booter) wireVolume(v harness.ResolvedVolume, id identity) error {
	dir := volumeDir(v, id.uid)
	if err := os.MkdirAll(v.Path, 0o755); err != nil {
		return err
	}
	nonEmpty, err := dirNonEmpty(v.Path)
	if err != nil {
		return err
	}
	if useOverlay(v.Kind, nonEmpty) {
		upper, work := overlayDirs(dir)
		for _, d := range []string{upper, work} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
		}
		// ADR 0107 §3. overlayfs reports the *upperdir's* ownership and mode
		// for the merged root, not the lower's. A freshly created upper is
		// root:root 0755, so without this every overlayed path would present
		// as root:root 0755 however the image built it -- and applyOwnership
		// below only speaks when the declaration states uid/gid/mode, which a
		// path that is already correct in the image has no reason to do.
		//
		// The visible half is writes at the top level of the path. Anything
		// below it inherits the lower directory's own attributes on copy-up and
		// is unaffected, which is what makes this fail in a confusing way: a
		// group-writable tree accepts writes everywhere except its own root.
		if err := adoptDirIdentity(v.Path, upper); err != nil {
			return err
		}
		if err := overlayMount(v.Path, v.Path, upper, work); err != nil {
			return err
		}
		return applyOwnership(v.Path, v)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := bindMount(dir, v.Path, false); err != nil {
		return err
	}
	return applyOwnership(v.Path, v)
}

// adoptDirIdentity gives dst the ownership and permission bits of src, so an
// overlay's upperdir presents the merged root the way the image's own directory
// did. Permission bits include setuid/setgid/sticky: a setgid directory is
// exactly how an image hands a tree to a group rather than to a uid it cannot
// know yet, and dropping that bit would defeat it.
func adoptDirIdentity(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	uid, gid, ok := fileOwner(fi)
	if !ok {
		// No ownership to copy on this platform; the mode still carries.
		return os.Chmod(dst, dirPermissions(fi))
	}
	if err := os.Chown(dst, uid, gid); err != nil {
		return fmt.Errorf("chown overlay upperdir %s: %w", dst, err)
	}
	// Chmod last: chown(2) clears setuid/setgid on some filesystems.
	if err := os.Chmod(dst, dirPermissions(fi)); err != nil {
		return fmt.Errorf("chmod overlay upperdir %s: %w", dst, err)
	}
	return nil
}

// dirPermissions is fi's permission bits plus the setuid/setgid/sticky bits,
// in the form os.Chmod understands.
func dirPermissions(fi os.FileInfo) os.FileMode {
	return fi.Mode().Perm() | (fi.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky))
}

func applyOwnership(target string, v harness.ResolvedVolume) error {
	if v.Mode != nil {
		if err := os.Chmod(target, *v.Mode); err != nil {
			return fmt.Errorf("chmod %s: %w", target, err)
		}
	}
	if v.UID != nil || v.GID != nil {
		uid, gid := -1, -1
		if v.UID != nil {
			uid = *v.UID
		}
		if v.GID != nil {
			gid = *v.GID
		}
		if err := os.Chown(target, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", target, err)
		}
	}
	return nil
}

// seedWorkingRoot creates the directory the sandbox works in and gives it to
// the sandbox user.
//
// Nothing else here reaches it. The image ships it (`mkdir -p /workspace` in a
// Dockerfile) root-owned, because the sandbox user does not exist until
// ensureUser above creates it, and wireSources chowns only the targets it
// binds -- which are directories *under* the working root, or somewhere else
// entirely when a source keeps its host path. So a sandbox created with
// --no-source, which has no source to bind at all, worked in a root-owned
// directory it could not write in.
//
// Only the directory itself, not a walk of it: everything under it is either a
// directory wireSources created and gave away with it (mkdirAllOwned), a source
// with the ownership wireSources gave it, or work the sandbox user made and
// already owns. Recursing into a checkout is the cost chownTreeOnOwnFilesystem
// exists to avoid.
//
// Before wireVolumes and wireSources, so that a declared volume or a source
// bound onto the working root itself -- which is the primary source's default
// target -- states the ownership and this does not overwrite it.
func (b *booter) seedWorkingRoot(root string, id identity) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return os.Chown(root, id.uid, id.gid)
}

// wireSources bind-mounts each pool-agent-materialized source from
// /.discobox/sources/<slug> onto its manifest target, owned by the sandbox user.
//
// Ownership comes from the manifest only when the pool agent actually knew it;
// otherwise it comes from id, the identity this flow just resolved. The pool
// agent cannot resolve a sandbox's account (ADR 0025 §4), so when the manifest
// named no user there is nothing for it to publish -- which is why those
// fields are optional rather than plain ints. Absent arriving as 0 would make
// this chown hand the primary source tree to root, in precisely the case where
// the sandbox is least likely to be running as root (ADR 0033 §5).
func (b *booter) wireSources(sources []sandboxconfig.Source, id identity) error {
	for _, s := range sources {
		src := filepath.Join(sourcesMountPath, s.Slug)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				// Nothing to bind. The pool agent prepares a push-delivered
				// source's directory before the container exists even though it
				// parks empty until the push lands, precisely so this is not
				// the parked case: the resume that fills it does not rebuild
				// the container, so a bind skipped here is never made at all.
				continue
			}
			return err
		}
		uid, gid := sourceOwner(s, id)
		if err := mkdirAllOwned(s.Target, uid, gid); err != nil {
			return fmt.Errorf("create source target %s: %w", s.Target, err)
		}
		if err := bindMount(src, s.Target, false); err != nil {
			return fmt.Errorf("wire source %s: %w", s.Slug, err)
		}
		// Not redundant with mkdirAllOwned, which speaks only for a mountpoint
		// it created and only for the inode underneath: this one lands on the
		// root of the tree now mounted over it, which is the source itself.
		if err := os.Chown(s.Target, uid, gid); err != nil {
			return fmt.Errorf("chown source %s: %w", s.Target, err)
		}
	}
	return nil
}

// loadEffectiveConfig reads the sandbox's effective config from the config
// volume. It is read from the /.discobox/config mount because /etc/discobox
// is not populated until wireConfig runs. Both sources and volumes are
// present in this one read (ADR 0012 §6) — there is no separate image-baked
// file to read before the bind, unlike the old image.json.
func loadEffectiveConfig() (sandboxconfig.Config, error) {
	path := filepath.Join(configMountPath, manifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sandboxconfig.Config{}, nil
		}
		return sandboxconfig.Config{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var effective sandboxconfig.Config
	if err := json.Unmarshal(data, &effective); err != nil {
		return sandboxconfig.Config{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return effective, nil
}

func loadResolvedVolumes(id identity, volumes []harness.Volume) ([]harness.ResolvedVolume, error) {
	return harness.ResolveVolumes(volumes, harness.VolumeRuntime{Home: id.home, UID: id.uid, GID: id.gid})
}

// mkdirAllOwned creates dir, giving the components it had to create to
// uid:gid and leaving the ownership of anything that already existed alone.
//
// The distinction is between a directory the image shipped -- whose ownership
// is the image's statement and not boot's to overwrite -- and one boot invented
// on the way to a mountpoint. A source target of /workspace/repos/api names an
// intermediate directory nothing else in the sandbox describes: created as root
// here, it is the failure seedWorkingRoot fixes for the working root, one level
// down, and for a target outside the working root (/srv/code/api) nothing would
// ever chown it. Source destinations are free-form, so this is reachable
// without an unusual image.
func mkdirAllOwned(dir string, uid, gid int) error {
	created, err := missingDirs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, p := range created {
		if err := os.Chown(p, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", p, err)
		}
	}
	return nil
}

// missingDirs is dir and each of its ancestors that does not exist yet, deepest
// first. It is read before the mkdir rather than derived from it because
// os.MkdirAll does not report what it created, and "did this path exist before
// boot ran" is the whole question.
func missingDirs(dir string) ([]string, error) {
	var missing []string
	for p := filepath.Clean(dir); ; {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return missing, nil
}

// sourceOwner decides who owns a wired source: the manifest's ids when the pool
// agent actually knew them, and otherwise the identity boot resolved.
//
// It is separate from wireSources because this is the whole of the decision and
// none of it needs a privileged syscall to exercise. The bug it replaces was
// never in the chown -- it was in an absent id arriving as 0 with no way to
// tell it from a deliberate root, which is a question about values rather than
// about mounting.
func sourceOwner(s sandboxconfig.Source, id identity) (uid, gid int) {
	uid, gid = id.uid, id.gid
	if s.UID != nil {
		uid = int(*s.UID)
	}
	if s.GID != nil {
		gid = int(*s.GID)
	}
	return uid, gid
}
