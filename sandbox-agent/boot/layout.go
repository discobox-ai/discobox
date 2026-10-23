package boot

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/fsdurable"
)

// volumeLayout is how a data volume is wired onto its target: a plain bind of
// the volume directory, or an overlay whose upper and work directories live in
// it (overlayDirs).
type volumeLayout string

const (
	layoutBind    volumeLayout = "bind"
	layoutOverlay volumeLayout = "overlay"
)

// layoutsDir holds one layout record per data volume, at the data volume's
// root. It is dot-prefixed for the reason cacheUsersDir is: its siblings are
// mirrored target paths, and no image declares a data path at "/.layouts".
const layoutsDir = ".layouts"

// layoutRecordPath is where the layout a data volume was first wired with is
// recorded. The record sits beside the volume directories rather than in one:
// a bound volume's directory is the target itself -- the user's home, for
// home's volume -- and a nested volume's directory is inside its parent's, so
// a marker in or next to either would show up in the sandbox.
func layoutRecordPath(v harness.ResolvedVolume) string {
	return path.Join(dataMountPath, layoutsDir, url.PathEscape(path.Clean(v.Path)))
}

// decideLayout returns the layout a volume is wired with on this boot, and
// records it for the next one when nothing was recorded yet.
//
// The choice is made once per volume and kept. useOverlay judges it from what
// is at the target before the mount, which is whatever the current image --
// or anything else that ran before boot -- put there, so judging it again on
// every boot changes it under a volume that already holds the user's files:
// a bound home re-pinned onto an image that ships content there would come up
// as an overlay with an empty upper, hiding everything written at the volume
// root, and an overlayed home whose image no longer ships content there would
// come up as a bind of the volume root, showing the user's home as ~/upper and
// ~/work. Neither layout is ever converted into the other: an overlay's upper
// holds only what the sandbox changed, plus whiteouts and opaque markers, and
// the files it left alone exist only in the image. An overlay whose lower is
// now an empty directory is still a correct view of everything it holds.
//
// A cache volume is always a bind (useOverlay) and records nothing.
func decideLayout(kind harness.VolumeKind, volDir, record string, targetNonEmpty bool) (volumeLayout, error) {
	if kind != harness.VolumeData {
		return layoutBind, nil
	}
	recorded, ok, err := readLayout(record)
	if err != nil || ok {
		return recorded, err
	}
	layout, ok, err := inferLayout(volDir)
	if err != nil {
		return "", err
	}
	if !ok {
		layout = layoutBind
		if useOverlay(kind, targetNonEmpty) {
			layout = layoutOverlay
		}
	}
	if err := writeLayout(record, layout); err != nil {
		return "", err
	}
	return layout, nil
}

// inferLayout reads the layout of a volume wired before layouts were
// recorded from what the wiring left in its directory. An overlay leaves upper
// and work side by side, and the kernel makes work/work inside the workdir on
// the first mount -- root-owned, mode 0 -- which a user's own directories
// named upper and work do not have. A boot that made upper and work and then
// failed to mount leaves the two of them empty and alone, and that was an
// overlay too: wired as a bind, it would show the user ~/upper and ~/work.
// Any other directory with something in it was bound. An absent or empty
// directory has nothing to keep, and ok is false.
func inferLayout(volDir string) (layout volumeLayout, ok bool, err error) {
	upper, work := overlayDirs(volDir)
	if isDir(upper) && isDir(work) {
		if _, err := os.Lstat(path.Join(work, "work")); err == nil {
			return layoutOverlay, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
		unmounted, err := onlyEmptyOverlayDirs(volDir)
		if err != nil {
			return "", false, err
		}
		if unmounted {
			return layoutOverlay, true, nil
		}
	}
	nonEmpty, err := dirNonEmpty(volDir)
	if err != nil || !nonEmpty {
		return "", false, err
	}
	return layoutBind, true, nil
}

// onlyEmptyOverlayDirs reports whether volDir holds an empty upper and an
// empty work and nothing else.
func onlyEmptyOverlayDirs(volDir string) (bool, error) {
	entries, err := os.ReadDir(volDir)
	if err != nil || len(entries) != 2 {
		return false, err
	}
	upper, work := overlayDirs(volDir)
	for _, d := range []string{upper, work} {
		empty, err := dirEmpty(d)
		if err != nil || !empty {
			return false, err
		}
	}
	return true, nil
}

func isDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

func readLayout(record string) (layout volumeLayout, ok bool, err error) {
	b, err := os.ReadFile(record)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	switch l := volumeLayout(strings.TrimSpace(string(b))); l {
	case layoutBind, layoutOverlay:
		return l, true, nil
	default:
		// Guessing would risk exactly the flip the record exists to prevent.
		return "", false, fmt.Errorf("layout record %s: unknown layout %q", record, l)
	}
}

// writeLayout records layout durably: a temporary file synced, renamed over
// the record, and the directory synced, so a boot cut short -- or the host
// losing power -- leaves no record or a whole one, never an empty one that
// would fail every later boot. The temporary name starts with a dot, and a
// record's name always starts with "%2F", so it cannot be another volume's
// record.
func writeLayout(record string, layout volumeLayout) error {
	dir := path.Dir(record)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+path.Base(record)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(string(layout) + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), record); err != nil {
		return err
	}
	return fsdurable.SyncDir(dir)
}
