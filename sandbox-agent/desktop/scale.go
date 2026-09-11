package desktop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// X11 has no compositor-level scaling the way Wayland does, so making a desktop
// HiDPI is two things that have to agree, and this file owns the second.
//
//  1. The framebuffer is asked for at device resolution — the viewer's CSS box
//     times the scale — and the browser draws it back down. That is display.go.
//  2. Every toolkit is told to draw that many times larger, so the desktop is
//     the same size to look at and simply denser. That is here.
//
// There is no single switch for (2). Each toolkit has its own, they are read
// **once when a program launches**, and the ones that read nothing cannot be
// helped at all. So this is delivered as environment, written where a shell and
// the window manager both pick it up, and it reaches the programs started after
// it — never the ones already running.
//
// What each variable is for:
//
//	GDK_SCALE       GTK 3/4 window scale. Scales widgets, icons and text alike.
//	GDK_DPI_SCALE   Divides GTK's font scaling back out. Without it GTK text
//	                takes the scale twice — once from GDK_SCALE and once from
//	                the raised Xft.dpi — and comes out at scale².
//	QT_AUTO_SCREEN_SCALE_FACTOR
//	                Qt 5/6 reads the X server's DPI and scales itself. Preferred
//	                over a hard QT_SCALE_FACTOR so Qt keeps its own rounding.
//	XCURSOR_SIZE    Held at the base size, deliberately, and see below.
//
// Chromium, Electron and Firefox need no variable: they read Xft.dpi, which
// display.go sets on the server. Nothing is set for Java — `_JAVA_OPTIONS` is
// the only lever and it prints a banner to stderr that breaks scripts parsing
// a JVM's output.
const (
	// ScaleEnvDir and ScaleEnvName are the file both readers agree on: a login
	// shell sources it from profile.d, and xfce4-session@.service takes it as an
	// EnvironmentFile, through the drop-in the boot flow writes — the file lives
	// in the sandbox user's home, and the unit cannot name that itself. Both are
	// exported for that drop-in, which has to name the path this package writes.
	ScaleEnvDir  = ".discobox/desktop"
	ScaleEnvName = "scale.env"

	// The two xfconf properties that have to be written instead of merged.
	// xfsettingsd owns the density and the cursor size on an Xfce desktop: it
	// holds each value in xfconf, publishes it over XSETTINGS, and writes the
	// matching X resource itself — so `xrdb -merge`ing either one is undone at
	// its next refresh. Everything else in xresources is a resource xfsettingsd
	// does not touch, which is why the merge is still what carries them.
	dpiProperty        = "/Xft/DPI"
	cursorSizeProperty = "/Gtk/CursorThemeSize"

	// decorationThemeProperty is on the xfwm4 channel rather than xsettings,
	// and is the window manager's half of a scale change. See decorationTheme.
	decorationThemeProperty = "/general/theme"

	// baseCursorSize is the cursor size at every scale, and the one thing here
	// that is deliberately *not* multiplied by it.
	//
	// The cursor is the only part of the desktop the browser does not draw from
	// the framebuffer. noVNC takes the remote cursor shape and hands it to CSS
	// as `cursor: url(...)`, and a CSS cursor image is laid out at its intrinsic
	// size in *CSS* pixels — the scale transform that shrinks the framebuffer
	// into the frame does not touch it. So a cursor of C device pixels appears
	// at C CSS pixels whatever the scale, while everything around it appears at
	// its own size over the scale.
	//
	// Scaling the cursor with the desktop therefore does the opposite of what
	// it does to everything else: at 2x a 48px cursor is drawn 48 CSS pixels
	// wide next to content drawn at half size, which is a pointer twice the
	// size it should be. Holding it at the base size is what makes it land at
	// the same apparent size at every scale.
	//
	// The cost is sharpness: at 2x the pointer is a 24px bitmap the browser
	// upscales, while everything behind it is pixel-for-pixel. Fixing that
	// needs noVNC to scale the cursor, which it has no support for, or x11vnc
	// to composite the cursor into the framebuffer (-nocursorshape) — which
	// makes the pointer move at the framebuffer's latency instead of the
	// browser's, and a laggy pointer is worse than a soft one.
	baseCursorSize = 24

	// LauncherDir is where the image keeps the desktop launchers the boot flow
	// seeds into a new home's ~/Desktop, and DesktopDir is where they land.
	// Exported for that flow, which has to name both.
	LauncherDir = "/usr/local/share/discobox/desktop/launchers"
	DesktopDir  = "Desktop"

	// scaleKey records the scale in the environment file itself, so the file is
	// readable back as well as sourceable. See RememberedScale.
	scaleKey = "DISCOBOX_DESKTOP_SCALE"
)

// scaleEnv is the environment for a given integer scale. Scale 1 is written out
// in full rather than left empty: the file is the whole truth about the current
// scale, so going back to 1 has to actively clear a 2 that is already exported
// in some shell's parent, not just stop mentioning it.
func scaleEnv(scale int) map[string]string {
	return map[string]string{
		"GDK_SCALE":                   strconv.Itoa(scale),
		"GDK_DPI_SCALE":               trimFloat(1 / float64(scale)),
		"QT_AUTO_SCREEN_SCALE_FACTOR": "1",
		"XCURSOR_SIZE":                strconv.Itoa(baseCursorSize),
	}
}

// cursorThemeSize is the XSETTINGS cursor size that leaves GDK drawing a
// baseCursorSize cursor after it applies the window scale factor. Floored at 1
// so an implausible scale cannot ask for a zero-pixel pointer.
func cursorThemeSize(scale int) int {
	if size := baseCursorSize / scale; size > 0 {
		return size
	}
	return 1
}

func trimFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// writeScaleEnv writes the environment file for scale.
//
// The format is plain KEY=VALUE, which is systemd's EnvironmentFile format and
// is also what `set -a; . file; set +a` exports from a shell — so one file
// serves the window manager's unit and every login shell, with no second
// representation to keep in step.
func writeScaleEnv(dir string, scale int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("desktop: create scale env directory: %w", err)
	}
	env := scaleEnv(scale)
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("# Written by discobox-desktop. Do not edit; it is rewritten\n")
	b.WriteString("# whenever the desktop scale changes.\n")
	fmt.Fprintf(&b, "%s=%d\n", scaleKey, scale)
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%s\n", key, env[key])
	}

	path := filepath.Join(dir, ScaleEnvName)
	// Written whole and moved into place: a shell sourcing this file while it
	// is half-written would export a truncated value, and `set -a` makes that
	// a silently wrong environment rather than an error.
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("desktop: write scale env: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return "", fmt.Errorf("desktop: install scale env: %w", err)
	}
	return path, nil
}

// RememberedScale reads back the scale the environment file records.
//
// It is what makes a restarted sandbox come up at the scale it was left at, and
// it matters more than it looks: %HOME% is a data volume, so this file outlives
// the process that wrote it while Display.scale resets to 1 in memory. Without
// reading it back, a sandbox that was 2x last time starts its desktop session
// from a file saying GDK_SCALE=2 while the server is still at 96 DPI — widgets
// at 2x around text at 1x, and no way out but changing the scale twice.
func RememberedScale(dir string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dir, ScaleEnvName))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || name != scaleKey {
			continue
		}
		scale, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || scale < MinScale || scale > MaxScale {
			return 0, false
		}
		return scale, true
	}
	return 0, false
}

// xresources is what goes into the X resource database for a scale.
//
// Only what xfsettingsd does not own. It holds the density and the cursor size
// in xfconf and rewrites both resources at every refresh, so merging those here
// is undone behind our back, which is why the cursor size is not in this list.
// xterm's font is not one of xfsettingsd's, which is why it is.
func xresources(scale int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Xft.dpi: %d\n", BaseDPI*scale)
	// xterm defaults to a bitmap font, which scales with nothing: at 2x it is
	// drawn at native pixels and then shrunk by the browser to half size. That
	// is not a DPI problem and no DPI setting fixes it. Naming an Xft font
	// instead is what makes xterm scale at all, and `monospace` is a fontconfig
	// alias that always resolves to something.
	b.WriteString("XTerm*faceName: monospace\n")
	b.WriteString("XTerm*faceSize: 10\n")
	b.WriteString("UXTerm*faceName: monospace\n")
	b.WriteString("UXTerm*faceSize: 10\n")
	return b.String()
}

// decorationTheme is the xfwm4 theme that draws the window decorations at
// scale.
//
// The window manager needs naming here because it is the one part of the
// desktop that cannot scale itself. xfwm4 draws its decorations from fixed-size
// pixmaps, and its only HiDPI accommodation is a hardcoded substitution of
// Default-xhdpi for Default at scale 2 — which reaches no other theme, so a
// custom one stays at 1x however the desktop is scaled. The image therefore
// ships the art pre-scaled, one variant per scale, and selecting between them
// is this function. See image/desktop/brand-theme, which generates them, and
// image/desktop/xfconf/xfwm4.xml, which names the scale-1 theme.
//
// A theme change goes straight into xfconf, which xfwm4 watches, so unlike
// GDK_SCALE the decorations follow a scale change on the windows already open.
func decorationTheme(scale int) string {
	if scale <= 1 {
		return "Discobox"
	}
	return fmt.Sprintf("Discobox-%dx", scale)
}

// restartSession ends the Xfce session so systemd's Restart=always brings it
// back reading the new environment file.
//
// This is the only way GDK_SCALE can reach a desktop that is already running.
// It is an environment variable, fixed when a process starts, and the panel,
// the desktop and the window manager are all processes that started before
// anybody said what screen this desktop was going to be watched on. Changing
// the density underneath them without this leaves 2x text inside 1x widgets.
//
// It goes through xfce4-session-logout rather than systemctl deliberately: the
// viewer runs as the sandbox user with no privileges and an empty capability
// set, and cannot restart a system unit. It can ask the session to end over the
// session bus it already shares with it, which is the same thing the Log Out
// menu entry does — and xfce4-session@.service restarts on any exit that is not
// a systemd-initiated stop, which is exactly what that comes back from.
func (d *Display) restartSession(ctx context.Context) error {
	if _, err := d.command(ctx, "xfce4-session-logout", "--logout", "--fast"); err != nil {
		return fmt.Errorf("desktop: restart the desktop session: %w", err)
	}
	return nil
}

// applyLiveScale hands the density to the running session: it merges the X
// resources, tells the settings daemon, and points the window manager at
// decorations drawn for this scale.
//
// The environment file is deliberately not written here. That is the durable
// half — read at launch by every process that starts afterwards — and it needs
// no X server, so SetScale writes it before any of this and AdoptScale writes
// it on its own.
func (d *Display) applyLiveScale(ctx context.Context, scale int) error {
	if err := d.mergeResources(ctx, xresources(scale)); err != nil {
		return err
	}
	if _, err := d.command(ctx, "xfconf-query", "-c", "xfwm4",
		"-p", decorationThemeProperty, "-s", decorationTheme(scale)); err != nil {
		return fmt.Errorf("desktop: set the window decoration theme: %w", err)
	}
	settings := [...]struct {
		property string
		value    int
	}{
		{dpiProperty, BaseDPI * scale},
		// Divided, not multiplied, and not left alone either. GDK takes this
		// value as *logical* pixels and multiplies it by the window scale
		// factor, while libXcursor takes XCURSOR_SIZE as device pixels and
		// multiplies it by nothing. Setting both to the same number is what
		// made the pointer change size as it crossed from the desktop onto an
		// application window: 24 over the root, 24x2 over anything GTK.
		//
		// Both have to land on baseCursorSize device pixels, so the one that
		// gets multiplied is divided first.
		{cursorSizeProperty, cursorThemeSize(scale)},
	}
	for _, setting := range settings {
		if _, err := d.command(ctx, "xfconf-query", "-c", "xsettings",
			"-p", setting.property, "-s", strconv.Itoa(setting.value)); err != nil {
			return fmt.Errorf("desktop: set the desktop's %s: %w", setting.property, err)
		}
	}
	return nil
}
