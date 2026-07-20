package boot

import (
	"fmt"
	"os"
)

// writeDesktopDropins reproduces the systemd desktop unit drop-ins the retired
// entrypoint.sh generated, binding the socket-activated desktop units to the
// resolved sandbox user. Each drop-in is written only when its base unit is
// present in the image.
func writeDesktopDropins(id identity) error {
	if fileExists("/etc/systemd/system/openbox@.service") && fileExists("/etc/systemd/system/xvfb.service") {
		if err := installFile("/etc/systemd/system/xvfb.service.d/discobox-desktop-user.conf", 0o644,
			fmt.Sprintf("[Unit]\nWants=openbox@%s.service\n", id.name)); err != nil {
			return err
		}
	}
	if fileExists("/etc/systemd/system/x11vnc@.service") {
		if err := installFile("/etc/systemd/system/x11vnc@.service.d/discobox-desktop-user.conf", 0o644,
			fmt.Sprintf("[Service]\nUser=%s\n", id.name)); err != nil {
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
