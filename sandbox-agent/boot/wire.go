package boot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/sandboxconfig"
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

// wireVolumes wires every image-declared data/cache path from its backing
// primary volume onto its target.
func (b *booter) wireVolumes(volumes []harness.ResolvedVolume) error {
	sortVolumesByDepth(volumes)
	for _, v := range volumes {
		if err := b.wireVolume(v); err != nil {
			return fmt.Errorf("wire volume %s: %w", v.Path, err)
		}
	}
	return nil
}

func (b *booter) wireVolume(v harness.ResolvedVolume) error {
	dir := volumeDir(v.Kind, v.Path)
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

// wireSources bind-mounts each worker-materialized source from
// /.discobox/sources/<slug> onto its manifest target as the sandbox user.
func (b *booter) wireSources(sources []sandboxconfig.Source) error {
	for _, s := range sources {
		src := filepath.Join(sourcesMountPath, s.Slug)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				// The worker parked an empty push-delivered source; nothing to bind yet.
				continue
			}
			return err
		}
		if err := os.MkdirAll(s.Target, 0o755); err != nil {
			return err
		}
		if err := bindMount(src, s.Target, false); err != nil {
			return fmt.Errorf("wire source %s: %w", s.Slug, err)
		}
		if err := os.Chown(s.Target, int(s.UID), int(s.GID)); err != nil {
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
