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
// sandbox user. Each drop-in is written only when its base unit is present in the image, so an
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
	// The Xfce session also reads the toolkit environment, so the window
	// manager and the panel are drawn at the same scale as everything launched
	// from a shell. seedDesktopScale writes it before systemd starts, but a
	// failure there is logged rather than fatal, and `-` is what lets the
	// session start without the file.
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

// seedDesktopScale writes the desktop's starting scale into the sandbox user's
// home before systemd starts, and so before anything in the sandbox runs.
//
// The file is not only the desktop's. Every login shell sources it through
// /etc/profile.d, which is how GDK_SCALE reaches a program an agent launches —
// and the harness is a login shell started at boot. The viewer writes the same
// file, but it is socket-activated and starts only when a browser or an X
// client first reaches for the desktop, which is seconds after the harness has
// already read its environment and found nothing. Written here, the file is in
// place for the first shell of the boot.
//
// It is rewritten every boot rather than seeded once: the value is kept, but
// the variables around it are this image's, so a release that changes what
// the scale is delivered as reaches sandboxes that already have the file.
// Kept means any value, not only one somebody settled on -- the file cannot
// tell those from a default an earlier image wrote -- so a sandbox written at
// the old 1x default stays there until a HiDPI viewer opens it. That is a
// decision, not an oversight: existing sandboxes are left where they are.
func seedDesktopScale(id identity) error {
	envDir := filepath.Join(id.home, desktop.ScaleEnvDir)
	// Each component is created here and chowned, rather than by MkdirAll
	// alone: boot writes as root, and the viewer, running as the sandbox user,
	// rewrites this file by renaming into the directory -- and creates
	// siblings of it under ~/.discobox.
	for _, dir := range []string{filepath.Dir(envDir), envDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.Chown(dir, id.uid, id.gid); err != nil {
			return err
		}
	}
	scale, _ := desktop.StartingScale(envDir)
	path, err := desktop.WriteScaleEnv(envDir, scale)
	if err != nil {
		return err
	}
	return os.Chown(path, id.uid, id.gid)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
