package boot

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/sandbox-agent/desktop"
)

// seedDesktopLaunchers puts the image's desktop launchers on a new desktop, so
// a person opening the viewer finds a terminal and a browser rather than an
// empty backdrop. The Home icon beside them is not a file at all — xfdesktop
// draws it from `show-home` in image/desktop/xfconf/xfce4-desktop.xml.
//
// Keyed on ~/Desktop being absent, not on each file being absent. The directory
// belongs to whoever is using the sandbox: somebody who deletes the browser
// icon has said something, and a per-file check would put it back on the next
// boot. Seeding the directory once says the same thing about the initial set
// without overriding what happens to it afterwards.
//
// It cannot ride on /etc/skel, which seedHome copies: that fires only when home
// is empty, and home is a data volume, so every sandbox that already exists
// would never get these.
func (b *booter) seedDesktopLaunchers(id identity, from string) error {
	entries, err := os.ReadDir(from)
	if os.IsNotExist(err) {
		// An image built without the desktop. Nothing to seed, not an error.
		return nil
	}
	if err != nil {
		return err
	}
	dir := filepath.Join(id.home, desktop.DesktopDir)
	switch _, err := os.Stat(dir); {
	case err == nil:
		return nil
	case !os.IsNotExist(err):
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chown(dir, id.uid, id.gid); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".desktop") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(from, entry.Name()))
		if err != nil {
			return err
		}
		path := filepath.Join(dir, entry.Name())
		// 0755, and it is load-bearing: xfdesktop draws a launcher as a
		// launcher only if the file is executable, and shows an unexecutable
		// one as a text file with a "trust this?" prompt behind it.
		//nolint:gosec // 0755 is the point: xfdesktop draws a launcher as a launcher only when the file is executable.
		if err := os.WriteFile(path, content, 0o755); err != nil {
			return err
		}
		if err := os.Chown(path, id.uid, id.gid); err != nil {
			return err
		}
	}
	return nil
}
