package boot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/sandbox-agent/desktop"
)

// desktopUserUnits are the desktop units whose work belongs to the person using
// the sandbox rather than to root: the VNC server, the session bus and the
// desktop that shares it, and the viewer, whose annotation record lives under
// that user's home and is read back by an agent running as the same user.
//
// The Xfce session is not here. It takes the user as its instance name instead,
// because xvfb.service has to name the instance it wants started.
var desktopUserUnits = []string{
	"x11vnc@.service",
	"discobox-desktop-bus.service",
	"discobox-desktop.service",
}

// writeDesktopDropins binds the socket-activated desktop units to the resolved
// sandbox user, reproducing what the retired entrypoint.sh generated. Each
// drop-in is written only when its base unit is present in the image, so an
// image built without the desktop gets none of them.
//
// Two kinds are written: the user a unit runs as, and the companions that hang
// off xvfb.service so that whatever starts X — a program talking to :0, or a
// browser that has loaded the viewer and asked it for a framebuffer size —
// brings up the desktop session and the viewer with it.
func writeDesktopDropins(id identity) error {
	if fileExists("/etc/systemd/system/xvfb.service") {
		var companions []string
		if fileExists("/etc/systemd/system/xfce4-session@.service") {
			companions = append(companions, "xfce4-session@"+id.name+".service")
		}
		if fileExists("/etc/systemd/system/discobox-desktop.service") {
			companions = append(companions, "discobox-desktop.service")
		}
		if len(companions) > 0 {
			if err := installFile("/etc/systemd/system/xvfb.service.d/discobox-desktop-user.conf", 0o644,
				"[Unit]\nWants="+strings.Join(companions, " ")+"\n"); err != nil {
				return err
			}
		}
	}
	for _, unit := range desktopUserUnits {
		if !fileExists("/etc/systemd/system/" + unit) {
			continue
		}
		if err := installFile("/etc/systemd/system/"+unit+".d/discobox-desktop-user.conf", 0o644,
			fmt.Sprintf("[Service]\nUser=%s\n", id.name)); err != nil {
			return err
		}
	}
	// The Xfce session also reads the toolkit environment the viewer writes when
	// the desktop scale changes, so the window manager and the panel are drawn
	// at the same scale as everything launched from a shell. It is optional
	// (`-`): nothing has written it until somebody has changed the scale once.
	if fileExists("/etc/systemd/system/xfce4-session@.service") {
		if err := installFile("/etc/systemd/system/xfce4-session@.service.d/discobox-desktop-user.conf", 0o644,
			fmt.Sprintf("[Service]\nEnvironmentFile=-%s\n",
				filepath.Join(id.home, desktop.ScaleEnvDir, desktop.ScaleEnvName))); err != nil {
			return err
		}
	}
	if fileExists("/etc/systemd/system/websockify@.service") && fileExists("/etc/systemd/system/websockify-proxy.service") {
		if err := installFile("/etc/systemd/system/websockify-proxy.service.d/discobox-desktop-user.conf", 0o644,
			fmt.Sprintf("[Unit]\nWants=websockify@%s.service\nAfter=websockify@%s.service\n", id.name, id.name)); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
