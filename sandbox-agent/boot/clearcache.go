package boot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/discobox-ai/discobox/harness"
)

// ClearCaches empties every cache path the bootstrap at path declares, in
// place: what is in each goes, and the directory stays, since a bind or a
// process may hold it. It is how a machine pool clears its caches, which are on
// each sandbox's own disk (ADR 26-10-23-551 §3). It returns the paths it
// emptied.
//
// A cache path is the declared path itself, never the volume behind it, so it
// is the same path whether the box wired it or not.
func ClearCaches(path string) ([]string, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	bootstrap, err := loadEffectiveConfig(path)
	if err != nil {
		return nil, err
	}
	id, err := bootstrapIdentity(bootstrap)
	if err != nil {
		return nil, err
	}
	volumes, err := loadResolvedVolumes(id, bootstrap.Volumes)
	if err != nil {
		return nil, fmt.Errorf("resolve volumes: %w", err)
	}
	// A data volume may be declared inside a cache (/nix is one, its
	// profiles and gcroots are data): it is the sandbox's own, and stays.
	var keep []string
	for _, v := range volumes {
		if v.Kind != harness.VolumeCache {
			keep = append(keep, v.Path)
		}
	}
	var cleared []string
	var errs []error
	for _, v := range volumes {
		if v.Kind != harness.VolumeCache {
			continue
		}
		if err := emptyDir(v.Path, keep); err != nil {
			errs = append(errs, fmt.Errorf("empty cache %s: %w", v.Path, err))
			continue
		}
		cleared = append(cleared, v.Path)
	}
	return cleared, errors.Join(errs...)
}

// emptyDir removes everything in dir and keeps dir, and every path in keep
// with whatever leads to it.
func emptyDir(dir string, keep []string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		switch {
		case slices.Contains(keep, path):
		case entry.IsDir() && slices.ContainsFunc(keep, func(k string) bool { return within(path, k) }):
			if err := emptyDir(path, keep); err != nil {
				errs = append(errs, err)
			}
		default:
			if err := os.RemoveAll(path); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
