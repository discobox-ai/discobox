package boot

import "fmt"

// writeSessionBusDropins binds the sandbox's session bus to the resolved
// sandbox user: the socket is created owned by that user, and the daemon runs
// as them. Every sandbox terminal is handed the socket's path as
// DBUS_SESSION_BUS_ADDRESS, so the bus is the user's whether or not the desktop
// ever starts. Each drop-in is written only when its unit is present in the
// image.
func writeSessionBusDropins(id identity) error {
	dropins := []struct{ unit, content string }{
		{"discobox-session-bus.socket", fmt.Sprintf("[Socket]\nSocketUser=%s\n", id.name)},
		{"discobox-session-bus.service", fmt.Sprintf("[Service]\nUser=%s\n", id.name)},
	}
	for _, d := range dropins {
		if !fileExists("/etc/systemd/system/" + d.unit) {
			continue
		}
		if err := installFile("/etc/systemd/system/"+d.unit+".d/discobox-sandbox-user.conf", 0o644, d.content); err != nil {
			return err
		}
	}
	return nil
}
