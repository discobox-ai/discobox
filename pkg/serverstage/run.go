package serverstage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runDirName is the directory, beside a platform's versioned sets, that a
// staged server is run from where its path has to stay the same across
// upgrades (ADR 26-11-24-372 §3). Windows keys a Winsock LSP exemption by the
// executable's path, so a server run from its versioned directory would lose
// the exemption with every upgrade.
//
// It is never a version (Validate refuses one by this name), holds no
// manifest.json, so migrateLegacyLayout never takes it for a legacy set, and
// carries neither .staging- nor .replaced-, so sweepAbandoned leaves it be.
const runDirName = "run"

// linkingMarker names a file Install is putting into the run directory, before
// it is renamed into place. Install removes any it finds that a killed refresh
// left behind.
const linkingMarker = ".linking-"

// asideMarker names a file Install moved out of an asset's way. Windows will
// not replace or delete an executable or DLL a process has loaded, but it will
// rename one: an older server keeps its image for as long as it drains, after
// its listener has closed. The renamed file goes when nothing has it loaded any
// more, on this Install or a later one.
const asideMarker = ".replaced-"

// RunDir is the fixed directory a goos/goarch server staged under root is run
// from. Nothing about it changes from one version to the next.
func RunDir(root, goos, goarch string) string {
	return filepath.Join(root, goos+"-"+goarch, runDirName)
}

// RunDir is the fixed directory m's platform is run from under root.
func (m Manifest) RunDir(root string) string {
	return RunDir(root, m.OS, m.Arch)
}

// RunCommand is the path m's command is run from under root: the path a
// Windows LSP exemption is made for, the same for every version.
func (m Manifest) RunCommand(root string) string {
	return filepath.Join(m.RunDir(root), m.Command)
}

// Install brings m's run directory to hold m's staged set, and returns the path
// of the command there.
//
// The versioned directory stays the record: it is what Stage verified and what
// a rollback finds. The run directory holds the same files under another name
// — hard links, since both are under one root and so on one volume, or copies
// where a link fails — and nothing else: a file an older version had and this
// one does not is removed, because on Windows a DLL beside the executable is
// one it may load.
//
// An asset already there as the same file (a hard link), or as a file of the
// same size and modification time (a copy, which Install stamps with its
// source's time), is left alone, so a run directory that already holds this
// version costs a stat per asset and changes nothing. That check is also what
// catches a refresh killed halfway: each asset is moved into place on its own,
// so one that never was still differs. It is not a re-hash; like the staged
// set, the run directory is under the user's own state root.
//
// A file a process has loaded cannot be replaced or deleted on Windows, and an
// older server keeps its image loaded while it drains, after it has stopped
// answering. So an asset that has to change is first renamed aside (see
// asideMarker), which Windows allows, and the new one moved into its name.
// The caller still runs Install only once an older server has stopped
// answering, so nothing launches from the directory while it changes.
func Install(root string, m Manifest) (string, error) {
	dir, ok := Staged(root, m)
	if !ok {
		return "", fmt.Errorf("server %s for %s is not staged", m.Version, m.Platform())
	}
	run := m.RunDir(root)
	if err := os.MkdirAll(run, 0o700); err != nil {
		return "", err
	}
	wanted := make(map[string]bool, len(m.Assets))
	for _, asset := range m.Assets {
		wanted[asset.Name] = true
		if err := installAsset(filepath.Join(dir, asset.Name), filepath.Join(run, asset.Name)); err != nil {
			return "", fmt.Errorf("put server %s in %s: %w", m.Version, run, err)
		}
	}
	entries, err := os.ReadDir(run)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		name := entry.Name()
		if wanted[name] {
			continue
		}
		path := filepath.Join(run, name)
		if strings.Contains(name, asideMarker) || strings.Contains(name, linkingMarker) {
			// Still loaded by a server that has not exited yet: the next
			// Install takes it.
			_ = os.RemoveAll(path)
			continue
		}
		// A file this version does not have must not stay under its name: a
		// DLL beside the executable is one it may load.
		if err := moveAside(path); err != nil {
			return "", fmt.Errorf("put server %s in %s: move %s, which it does not have, out of the way: %w", m.Version, run, name, err)
		}
	}
	return m.RunCommand(root), nil
}

// moveAside renames path out of its name and removes it if it can. A file a
// running process has loaded can be renamed but not removed on Windows; it is
// left under its aside name for a later Install.
func moveAside(path string) error {
	aside := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+asideMarker+fmt.Sprint(time.Now().UnixNano()))
	if err := os.Rename(path, aside); err != nil {
		return err
	}
	_ = os.RemoveAll(aside)
	return nil
}

// installAsset makes dst the same file as src, unless it already is.
func installAsset(src, dst string) error {
	source, err := os.Stat(src)
	if err != nil {
		return err
	}
	current, err := os.Stat(dst)
	switch {
	case err == nil && sameAsset(source, current):
		return nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	exists := err == nil
	// Into a temporary name first, so what is moved into dst is always a
	// whole file, never a partial copy.
	temp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+linkingMarker+fmt.Sprint(time.Now().UnixNano()))
	if err := os.Link(src, temp); err != nil {
		if err := copyAsset(src, temp, source); err != nil {
			_ = os.Remove(temp)
			return err
		}
	}
	// The old file is moved aside rather than renamed over: a server that is
	// still draining has it loaded, and Windows refuses to replace that.
	if exists {
		if err := moveAside(dst); err != nil {
			_ = os.Remove(temp)
			return err
		}
	}
	if err := os.Rename(temp, dst); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// sameAsset reports whether current already is source: the same file, or a
// copy of it as copyAsset leaves one.
func sameAsset(source, current os.FileInfo) bool {
	if os.SameFile(source, current) {
		return true
	}
	return current.Mode().IsRegular() &&
		current.Size() == source.Size() &&
		current.ModTime().Equal(source.ModTime())
}

// copyAsset copies src to dst with src's permissions and modification time,
// which is what lets sameAsset recognize the copy the next time.
func copyAsset(src, dst string, info os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// isRunDir reports whether a version would name the run directory.
func isRunDir(version string) bool {
	return strings.EqualFold(version, runDirName)
}
