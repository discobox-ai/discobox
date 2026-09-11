package desktop

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The launcher hook forces the desktop's scale on a browser with a window on :0
// and on nothing else, and anywhere else drops the desktop's GDK_SCALE too,
// because Chromium multiplies it into its own scale. scale.env exists in every
// sandbox from its first boot, so a headless Chromium, or one on another X
// server, would otherwise come back with twice the pixels it asked for.
func TestChromiumForcesTheScaleOnlyOnTheDesktop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher hook is POSIX shell")
	}
	hook, err := filepath.Abs("../image/desktop/chromium.d/discobox-scale")
	if err != nil {
		t.Fatalf("resolve hook: %v", err)
	}
	home := t.TempDir()
	if _, err := WriteScaleEnv(filepath.Join(home, ScaleEnvDir), 2); err != nil {
		t.Fatalf("WriteScaleEnv: %v", err)
	}
	const forced = "--force-device-scale-factor=2"
	cases := []struct {
		name    string
		display string
		args    []string
		// exported is a login shell's environment, which already carries the
		// scale whether or not the hook sources the file.
		exported bool
		// want is whether the factor is forced; keepsGDK whether an exported
		// GDK_SCALE reaches the browser.
		want     bool
		keepsGDK bool
	}{
		{name: "headed on the desktop", display: ":0", want: true},
		{name: "headed on a screen of :0", display: ":0.0", args: []string{"https://example.com"}, want: true},
		{name: "headed on the desktop from a login shell", display: ":0", exported: true, want: true, keepsGDK: true},
		{name: "headless", display: ":0", args: []string{"--headless"}},
		{name: "new headless", display: ":0", args: []string{"--headless=new", "--screenshot"}},
		{name: "headless from a login shell", display: ":0", args: []string{"--headless"}, exported: true},
		{name: "another X server", display: ":99", exported: true},
		{name: "no display", display: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The launcher sources the hook with its own arguments in "$@"; sh -c
			// with the hook as $0 reproduces that.
			script := `CHROMIUM_FLAGS=""; . "$0"; printf '%s|%s' "$CHROMIUM_FLAGS" "${GDK_SCALE-unset}"`
			//nolint:gosec // The hook path and its arguments are this test's own fixtures.
			cmd := exec.CommandContext(t.Context(), "sh", append([]string{"-c", script, hook}, c.args...)...)
			cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "DISPLAY=" + c.display}
			if c.exported {
				cmd.Env = append(cmd.Env, "DISCOBOX_DESKTOP_SCALE=2", "GDK_SCALE=2", "GDK_DPI_SCALE=0.5")
			}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("run hook: %v", err)
			}
			flags, gdk, _ := strings.Cut(string(out), "|")
			if got := strings.Contains(flags, forced); got != c.want {
				t.Fatalf("CHROMIUM_FLAGS = %q; forced scale = %v, want %v", flags, got, c.want)
			}
			if c.exported {
				if kept := gdk == "2"; kept != c.keepsGDK {
					t.Fatalf("GDK_SCALE after the hook = %q; kept = %v, want %v", gdk, kept, c.keepsGDK)
				}
			}
		})
	}
}
