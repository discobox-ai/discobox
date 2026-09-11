package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The desktop is socket-activated from two directions -- a browser reaching the
// viewer on 6900, and an X client reaching :0 -- and the rule for both is that
// only deliberate use starts it. The Go half of that rule is tested against a
// live server in server_test.go; these are the parts of it that live in the
// image's unit files, where nothing else would catch a regression.

const unitDir = "../image/systemd"

func readUnit(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(unitDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// directive is every value given for a key, across the whole unit: systemd
// treats a repeated key as a list, so reading only the first would miss one.
func directive(unit, key string) []string {
	var values []string
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(name) == key {
			values = append(values, strings.TrimSpace(value))
		}
	}
	return values
}

// The display's activation socket is a path on disk, and the X server that
// serves it must not delete that path. It did once: an ExecStartPre swept
// /tmp/.X11-unix/X0 away as stale state, which unlinked the socket file
// x11-display.socket had already bound. The listening unit still reported
// itself active, the first client through -- the one whose connection started
// the server -- still worked, and every later DISPLAY=:0 got "Can't open
// display", until something restarted the socket unit. Xorg runs -nolisten
// local and never creates that path, so it has no business removing it either.
func TestTheXServerDoesNotDeleteItsActivationSocket(t *testing.T) {
	listen := directive(readUnit(t, "x11-display.socket"), "ListenStream")
	if len(listen) != 1 {
		t.Fatalf("ListenStream = %q, want exactly one path", listen)
	}
	socketPath := listen[0]
	if !strings.HasPrefix(socketPath, "/") {
		t.Fatalf("ListenStream = %q, want a filesystem path", socketPath)
	}

	xvfb := readUnit(t, "xvfb.service")
	for _, key := range []string{"ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost"} {
		for _, command := range directive(xvfb, key) {
			for _, field := range strings.Fields(command) {
				if strings.TrimSuffix(field, "/") == socketPath {
					t.Errorf("xvfb.service %s names %s, which x11-display.socket is listening on;\n"+
						"removing it makes :0 reachable exactly once per start", key, socketPath)
				}
			}
		}
	}
}

// Reaching the viewer's port is not a request for a desktop. A port probe, a
// health check, or a preview link someone opens and closes all land on 6900 and
// activate this service, so it must not drag the X server up behind it -- the
// browser asking for a framebuffer size is what does that.
func TestTheViewerDoesNotPullUpTheDisplay(t *testing.T) {
	viewer := readUnit(t, "discobox-desktop.service")
	for _, key := range []string{"Requires", "Requisite", "Wants", "BindsTo", "PartOf", "Upholds"} {
		for _, value := range directive(viewer, key) {
			for _, unit := range strings.Fields(value) {
				if unit == "xvfb.service" || strings.HasPrefix(unit, "xfce4-session@") {
					t.Errorf("discobox-desktop.service has %s=%s; reaching port 6900 would start a desktop", key, unit)
				}
			}
		}
	}
}

// The session brings the server to its scale before it starts anything.
// scale.env carries only the launch-time half of a scale, and the viewer that
// applies the rest is not running when a program talking to :0 is what started
// X -- so without this, such a session starts GDK_SCALE=2 programs against a
// 96 DPI server: 2x widgets around 1x text.
func TestTheSessionPreparesTheServerScaleBeforeStarting(t *testing.T) {
	session := readUnit(t, "xfce4-session@.service")
	pre := directive(session, "ExecStartPre")
	for _, command := range pre {
		if strings.HasSuffix(strings.TrimPrefix(command, "-"), "discobox-sandbox-agent desktop prepare-session") {
			return
		}
	}
	t.Fatalf("xfce4-session@.service ExecStartPre = %q, want `discobox-sandbox-agent desktop prepare-session`", pre)
}
