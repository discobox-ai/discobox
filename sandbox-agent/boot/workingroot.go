package boot

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/fsdurable"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// workingRootDurable reports whether something durable already backs the
// working root: a source whose target is it or one of its parents, or a
// declared volume whose path is (a working root inside home is home's). Only
// then is it left as it is; otherwise it is a data volume of the sandbox's
// own (wireWorkingRoot, ADR 26-11-14-302 §1).
func workingRootDurable(root string, volumes []harness.ResolvedVolume, sources []sandboxconfig.Source) bool {
	for _, s := range sources {
		if sandboxPaths.Within(s.Target, root) {
			return true
		}
	}
	for _, v := range volumes {
		if sandboxPaths.Within(v.Path, root) {
			return true
		}
	}
	return false
}

// workingRootDirName, at the data root, keeps the working root's contents.
// Not the working root's own path under the data root, as a declared data path
// is kept (volumeDir): a machine keeps a source at its target's path there
// (machineSourceDir), so a source nested in the working root
// (/workspace/<slug>) already has its tree at /.discobox/data/workspace/<slug>,
// and the working root's mountpoint for it, moved in, would collide with it.
// Dot-prefixed for the reason layoutsDir is: no image declares a data path at
// "/.working-root". The data subtree an export carries holds it (ADR 0129).
const workingRootDirName = ".working-root"

func workingRootDir() string { return path.Join(dataMountPath, workingRootDirName) }

// workingRootRecordPath is where the data root records that the working root
// was moved in and bound, beside the declared volumes' layouts and apart from
// them, as a machine's source is (sourceRecordPath): a record under the
// path's own name would be read as an image-declared volume's.
func workingRootRecordPath(root string) string {
	return path.Join(dataMountPath, layoutsDir, url.PathEscape("working-root:"+path.Clean(root)))
}

// wireWorkingRoot makes the working root durable when no source or declared
// volume backs it (workingRootDurable): the harness starts there, so it is
// where a box with no source keeps its work, and the container's own
// filesystem -- or a machine's root, which a new root replaces -- is dropped
// by every rebuild (ADR 26-11-14-302 §1).
//
// It is moved in and bound, in a container as on a machine (moveIn), never
// overlaid: what is at the target is the user's work, from a box made before
// this or carried over from its previous container by the pool (§3), and an
// overlay would keep it in the lower layer the next rebuild drops (§2).
//
// It runs before the declared volumes are wired. Nothing declared is its
// parent (that would back it), so it is the outermost of them, and a
// declared path nested under it is then bound onto its directory.
//
// Whatever a boot finds at the target is moved in, even once the working root
// is recorded: what the pool carried from a removed container into a new
// one's layer arrives there whenever that container first boots, and a bind
// over it would hide it under the volume for good. A name the volume already
// has is not refused, as a declared path's is (moveIn): the target's entry is
// set aside under a name of its own beside it (setAsideClashes), so neither
// is lost and the box still boots.
//
// It reports whether the working root is durable once it returns: what a
// machine's agent says on its data volume, so the pool gives the machine a
// new root only once its working root is kept there (§4). At an agent's start
// on a machine (wireMachineStarted) nothing is moved under running services,
// as moveIn is never run there: a working root with entries stays where it
// is, unreported, for the next boot to move in, and an empty one is recorded
// and bound as it stands. Roots on a machine's disk mount nothing, and report
// nothing either: that machine's disk is all it has.
func (b *booter) wireWorkingRoot(root string, volumes []harness.ResolvedVolume, sources []sandboxconfig.Source, id identity, mode wireMode) (bool, error) {
	if workingRootDurable(root, volumes, sources) {
		return true, nil
	}
	if mode == wireMachineStarted && !machineDataVolume() {
		return false, nil
	}
	if err := mkdirAllOwned(root, id.home, id.uid, id.gid); err != nil {
		return false, err
	}
	record := workingRootRecordPath(root)
	nonEmpty, err := dirNonEmpty(root)
	if err != nil {
		return false, err
	}
	dir := workingRootDir()
	switch {
	case mode == wireMachineStarted && nonEmpty:
		return false, nil
	case mode == wireMachineStarted:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, err
		}
		if _, recorded, err := readLayout(record); err != nil {
			return false, err
		} else if !recorded {
			if err := writeLayout(record, layoutBind); err != nil {
				return false, err
			}
		}
	default:
		if nonEmpty {
			if err := setAsideClashes(root, dir); err != nil {
				return false, fmt.Errorf("working root %s: %w", root, err)
			}
			// moveIn moves only what was never recorded; this is moved in
			// all the same, and recorded again once it has been.
			if err := fsdurable.Remove(record); err != nil {
				return false, err
			}
		}
		layout, err := b.moveIn(root, dir, record)
		if err != nil {
			return false, fmt.Errorf("working root %s: %w", root, err)
		}
		if layout != layoutBind {
			return false, fmt.Errorf("working root %s: recorded as %s, and only a bind keeps it", root, layout)
		}
	}
	if err := b.bind(dir, root); err != nil {
		return false, fmt.Errorf("wire working root %s: %w", root, err)
	}
	// Onto the root of the tree now mounted over it, as wireSources does.
	if err := os.Chown(root, id.uid, id.gid); err != nil {
		return false, fmt.Errorf("chown working root %s: %w", root, err)
	}
	return true, nil
}

// setAsideClashes renames each entry at target whose name dir already has to
// a name of its own beside it, "<name>.carried", then "<name>.carried-2" and
// on, that neither has, so moveIn can move every entry in. An entry a cut
// short copy left (movingPrefix) is moveIn's to finish, and is left alone.
func setAsideClashes(target, dir string) error {
	entries, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, movingPrefix) || !exists(filepath.Join(dir, name)) {
			continue
		}
		aside := name + ".carried"
		for n := 2; exists(filepath.Join(dir, aside)) || exists(filepath.Join(target, aside)); n++ {
			aside = fmt.Sprintf("%s.carried-%d", name, n)
		}
		if err := os.Rename(filepath.Join(target, name), filepath.Join(target, aside)); err != nil {
			return err
		}
	}
	return nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
