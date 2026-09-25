package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// A sandbox terminal has DISPLAY=:0, and a D-Bus client with a DISPLAY and no
// bus address autolaunches a bus through dbus-launch, which connects to :0 and
// so starts X and the whole desktop -- gh reading its keyring did that at every
// harness start (ADR 26-09-25-146). Terminals are therefore handed the session
// bus's socket, which has to be the path the socket unit listens on, and the
// bus it activates must not bring the display up behind it.
func TestTerminalsAreGivenTheSessionBusWithoutTheDisplay(t *testing.T) {
	body, err := os.ReadFile("../image.json")
	if err != nil {
		t.Fatalf("read image.json: %v", err)
	}
	var manifest struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("parse image.json: %v", err)
	}
	if manifest.Env["DISPLAY"] == "" {
		t.Fatal("image.json sets no DISPLAY; this test guards the bus that has to go with it")
	}

	listen := directive(readUnit(t, "discobox-session-bus.socket"), "ListenStream")
	if len(listen) != 1 || !strings.HasPrefix(listen[0], "/") {
		t.Fatalf("discobox-session-bus.socket ListenStream = %q, want exactly one path", listen)
	}
	want := "unix:path=" + listen[0]
	if got := manifest.Env["DBUS_SESSION_BUS_ADDRESS"]; got != want {
		t.Errorf("image.json DBUS_SESSION_BUS_ADDRESS = %q, want %q: without it a terminal's D-Bus client autolaunches through :0", got, want)
	}

	for _, unit := range []string{"xfce4-session@.service", "discobox-desktop.service"} {
		found := false
		for _, env := range directive(readUnit(t, unit), "Environment") {
			if value, ok := strings.CutPrefix(env, "DBUS_SESSION_BUS_ADDRESS="); ok {
				found = true
				if value != want {
					t.Errorf("%s DBUS_SESSION_BUS_ADDRESS = %q, want the terminals' %q", unit, value, want)
				}
			}
		}
		if !found {
			t.Errorf("%s sets no DBUS_SESSION_BUS_ADDRESS, want %q", unit, want)
		}
	}

	bus := readUnit(t, "discobox-session-bus.service")
	// A service the bus activates inherits the daemon's environment, so a GUI
	// one a terminal asks for -- Thunar -- needs the terminals' display, or it
	// exits with "cannot open display".
	if got := directive(bus, "Environment"); !slices.Contains(got, "DISPLAY="+manifest.Env["DISPLAY"]) {
		t.Errorf("discobox-session-bus.service Environment = %q, want DISPLAY=%s for the GUI services it activates", got, manifest.Env["DISPLAY"])
	}
	for _, key := range []string{"Requires", "Requisite", "Wants", "BindsTo", "PartOf", "Upholds"} {
		for _, value := range directive(bus, key) {
			for _, unit := range strings.Fields(value) {
				if unit == "xvfb.service" || strings.HasPrefix(unit, "xfce4-session@") || unit == "x11-display.socket" {
					t.Errorf("discobox-session-bus.service has %s=%s; a terminal reaching its bus would start a desktop", key, unit)
				}
			}
		}
	}
	// The socket unit owns the path. A runtime directory is removed when the
	// daemon stops, and would take the listening socket with it.
	if dirs := directive(bus, "RuntimeDirectory"); len(dirs) > 0 {
		t.Errorf("discobox-session-bus.service RuntimeDirectory = %q; stopping the daemon would unlink its activation socket", dirs)
	}
}
