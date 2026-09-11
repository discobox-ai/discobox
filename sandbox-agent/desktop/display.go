package desktop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Display owns the size and the density of the sandbox's X11 screen. They are
// two separate things here, and keeping them separate is the whole design.
//
// **Size follows the window, continuously.** The viewer resizes the desktop by
// driving this rather than by reconnecting with the `?x=&y=` query the
// websockify proxy on 6080 understands. Both reach xrandr in the end, but only
// this one can do it on a live connection: x11vnc runs with `-xrandr resize`
// and pushes the new geometry to an attached client as a desktop-size update,
// so the desktop follows the browser window instead of dropping the session and
// rebuilding it on every drag of the frame.
//
// **Scale does not.** It is a sticky session property — an integer, adopted
// once from the viewer's device pixel ratio and then changed only when a person
// asks. It cannot track the window, because everything that reads it reads it
// **once, when a program launches**: a scale recomputed on every resize leaves
// two programs started at two window sizes drawing at two different sizes, with
// nothing able to reconcile them afterwards.
//
// The scale is what makes the desktop HiDPI, and it is one number driving three
// things that must agree:
//
//   - the framebuffer, asked for at the viewer's CSS box times the scale, so
//     the browser draws it back down onto physical pixels 1:1;
//   - Xft.dpi, so text and the programs that read the server's density (
//     Chromium, Electron, Firefox, Qt) draw that many times larger;
//   - the toolkit environment in scale.go, for GTK and Qt and the cursor, which
//     read variables rather than the server.
//
// Integer only. A fractional scale means a fractional downscale in the browser
// — soft, which is the opposite of the point — and GDK_SCALE takes integers
// anyway, so 1.5 would scale text and not widgets.
type Display struct {
	// Name is the X display to act on.
	Name string

	// EnvDir is where the toolkit environment file is written. Empty writes
	// none, which is what the tests run with.
	EnvDir string
	// Log receives what goes wrong in the parts of a scale change that must not
	// fail the change. Optional.
	Log *slog.Logger

	mu    sync.Mutex
	modes map[string]bool
	// modeOrder is the same keys in the order they were added, so the oldest
	// can be evicted when the table is full.
	modeOrder []string
	// scale is the sticky device scale, re-asserted after every mode change
	// because xrandr recomputes the screen's physical size from the mode and
	// would otherwise quietly drop the density back to the default.
	scale int
	// chosen records that somebody decided the scale, so the viewer's
	// auto-detection stops overriding it. A second browser on a normal screen
	// must not undo what a person set from a HiDPI one.
	chosen bool
	// applied records that the scale has been written out at least once this
	// run. The environment file outlives the process and the in-memory scale
	// does not, so "unchanged" is not the same as "already on disk" until this
	// is set.
	applied bool
	// up records that this process has successfully talked to the X server, so
	// a read-only caller can tell "no display yet" from "a display of unknown
	// size". Set by the calls that legitimately start one.
	//
	// Atomic rather than guarded by mu: run() is the one place that sets it,
	// and it is reached from both sides of the lock -- GeometryIfUp reads this
	// before taking it, the resize and scale paths call run() holding it.
	up atomic.Bool
	// released records that the desktop session has been let go. Before that,
	// a scale change is just the value the session will start with; after it,
	// the session is already running at the old one and has to be restarted to
	// pick the new one up.
	released bool
}

// The bounds a requested size is held to. The floor keeps a window nobody has
// dragged open yet usable; the ceiling is what a 2x viewport on a large HiDPI
// screen reaches, and bounds how much framebuffer a browser tab can ask this
// sandbox to allocate and x11vnc to encode.
//
// All four are multiples of SizeBucket, so every reachable size is one this
// display can be asked for exactly — a ceiling off the bucket would be a size
// NormalizeSize could never return, and the viewer would size its frame for a
// screen it cannot get.
const (
	MinWidth  = 640  // 10 buckets
	MinHeight = 384  // 6
	MaxWidth  = 3840 // 60
	MaxHeight = 2432 // 38

	// SizeBucket is what widths and heights are rounded *down* to. Every
	// distinct size becomes a modeline that stays on the output for the life of
	// the X server, so a continuous drag would otherwise leave one behind per
	// frame. Down rather than up because the viewer sizes its frame to the
	// framebuffer it expects: rounding up would hand back a screen slightly
	// larger than the window that has to hold it.
	SizeBucket = 64

	// MaxDynamicModes bounds that set even so, for a client that resizes
	// across the whole range rather than settling. Reaching it evicts the
	// oldest mode rather than refusing the resize: the reachable width x height
	// grid is far larger than this, so a long session finds new sizes as a
	// matter of course, and failing outright would wedge the viewer at whatever
	// size it happened to be for the rest of the process's life.
	MaxDynamicModes = 48

	// BaseDPI is the density this X server actually reports — the dummy driver
	// logs `DPI set to (96, 96)` and sets a 338x270mm screen for 1280x1024,
	// which is 96 to within a rounding error. It is the native value, not a
	// convention borrowed from elsewhere, and the default the desktop runs at.
	BaseDPI = 96

	// MinScale and MaxScale bound the device scale. 1 is a normal screen; 2 is
	// every current HiDPI laptop; 3 exists because some phones report it.
	MinScale = 1
	MaxScale = 3

	refreshRate    = 60
	commandTimeout = 5 * time.Second
)

// Geometry is the screen as it now stands.
type Geometry struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	DPI    int    `json:"dpi"`
	Scale  int    `json:"scale"`
	Chosen bool   `json:"chosen"`
	Output string `json:"output"`
}

var (
	screenPattern  = regexp.MustCompile(`current\s+(\d+)\s+x\s+(\d+)`)
	modelinePrefix = "Modeline "
)

// NewDisplay targets name, defaulting to :0.
func NewDisplay(name string) *Display {
	if strings.TrimSpace(name) == "" {
		name = ":0"
	}
	return &Display{Name: name, modes: map[string]bool{}, scale: 1}
}

// NormalizeSize holds a requested framebuffer size to the range this display
// serves and rounds it to a mode boundary. Clamped on both sides of the
// rounding, since flooring a value already at the floor would go under it.
func NormalizeSize(width, height int) (int, int) {
	return clamp(bucket(clamp(width, MinWidth, MaxWidth)), MinWidth, MaxWidth),
		clamp(bucket(clamp(height, MinHeight, MaxHeight)), MinHeight, MaxHeight)
}

// NormalizeScale rounds a requested device scale to a supported integer. A
// browser may report any ratio at all — 1.25 on a scaled Windows desktop, 1.5
// on a fractional-scaling Linux one — and this is where that becomes a scale
// the desktop can actually be set to.
func NormalizeScale(scale float64) int {
	if scale <= 0 {
		return 1
	}
	return clamp(int(scale+0.5), MinScale, MaxScale)
}

// Resize sets the screen to hold a viewer frame of cssWidth x cssHeight CSS
// pixels at the standing scale — so the framebuffer is that box in *device*
// pixels, which is what the browser draws back down onto physical pixels.
//
// It takes the CSS box rather than a framebuffer size so that this is the only
// place the arithmetic lives. The page does not compute the framebuffer to size
// its frame before asking; it sizes the frame from what comes back, which costs
// one loopback round trip and avoids a pair of roundings that would have to
// agree forever.
//
// It does not change the scale: the size tracks the browser window and the
// scale must not, so the standing value is re-asserted here rather than
// recomputed.
func (d *Display) Resize(ctx context.Context, cssWidth, cssHeight int) (Geometry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	width, height := NormalizeSize(cssWidth*d.scale, cssHeight*d.scale)

	output, err := d.connectedOutput(ctx)
	if err != nil {
		return Geometry{}, err
	}
	mode := fmt.Sprintf("%dx%d_%d.00", width, height, refreshRate)
	if err := d.ensureMode(ctx, output, mode, width, height); err != nil {
		return Geometry{}, err
	}
	// --dpi rides along with the mode change rather than following it: xrandr
	// derives the screen's physical size from the mode, so a bare --mode would
	// drop the density back to the default for a moment, and any program that
	// happened to launch in that moment would latch the wrong one.
	dpi := d.dpiLocked()
	if _, err := d.run(ctx, "--output", output, "--mode", mode, "--dpi", strconv.Itoa(dpi)); err != nil {
		return Geometry{}, err
	}
	return d.geometryOf(width, height, output), nil
}

// SetScale makes the desktop HiDPI at the given integer scale, setting the
// server's density and writing the toolkit environment together so nothing sees
// half of a change.
//
// auto marks the call as the viewer's detection rather than a person's choice:
// it is ignored once anybody has chosen, so opening the desktop in a second
// browser on a normal screen does not undo what was set from a HiDPI one.
//
// It reaches only programs launched afterwards. Everything involved — Xft.dpi,
// GDK_SCALE, Qt's screen factor — is read once at startup, so a running program
// keeps the scale it began with. That is a property of X11, not of this code.
// A session that is already running is therefore restarted, and the second
// return value says whether that happened: it does not for a scale that did not
// move, nor before the session has been released, and the viewer has to tell a
// person which of those they are getting before it warns them about losing
// their windows.
func (d *Display) SetScale(ctx context.Context, scale int, auto bool) (Geometry, bool, error) {
	scale = clamp(scale, MinScale, MaxScale)

	d.mu.Lock()
	defer d.mu.Unlock()
	// A choice outranks the viewer's detection on the *value*, and that is all
	// it does: the reported scale is discarded and the chosen one kept, then
	// the call carries on. Returning here instead would mean an auto call could
	// never finish a chosen scale whose live half had failed, and an auto call
	// is the only thing that happens on its own -- every page load makes one.
	if auto && d.chosen {
		scale = d.scale
	}
	if !auto {
		d.chosen = true
	}
	// Not `scale == d.scale`: an unchanged scale still has to be written out
	// the first time, because the environment file is what the desktop session
	// reads and this process cannot know whether the file on disk agrees with
	// it. A sandbox restarted after a 2x session comes back with d.scale at 1
	// and a file still saying 2.
	if scale == d.scale && d.applied {
		geometry, err := d.geometryLocked(ctx)
		return geometry, false, err
	}
	changed := scale != d.scale

	// The durable half first, and before anything that needs an X server. The
	// environment file is what the session and every login shell read when they
	// start; AdoptScale writes it the same way with no display at all, and a
	// scale asked for while X is down should still be the one X comes up at.
	//
	// d.scale is committed with it, not after the X work, because the two are
	// one fact and different consumers read each: the file becomes GDK_SCALE
	// for the session, while d.scale is what Resize multiplies the framebuffer
	// by and what the DPI is asserted from. Letting them disagree is the
	// mixed-channel state REVIEW.md forbids -- 2x widgets around 1x text, from a
	// scale change whose X half timed out against a cold display.
	if d.EnvDir != "" {
		if _, err := writeScaleEnv(d.EnvDir, scale); err != nil {
			return Geometry{}, false, err
		}
	}
	d.scale = scale
	output, err := d.connectedOutput(ctx)
	if err != nil {
		return Geometry{}, false, err
	}
	if _, err := d.run(ctx, "--output", output, "--dpi", strconv.Itoa(BaseDPI*scale)); err != nil {
		return Geometry{}, false, err
	}
	if err := d.applyLiveScale(ctx, scale); err != nil {
		return Geometry{}, false, err
	}

	// d.applied is what waits for all of it, and it is the flag that makes a
	// failure repairable: the short-circuit above needs it, so a call that got
	// halfway leaves the next one -- an auto report from the next page load,
	// with no user action at all -- to run the live half again.
	d.applied = true
	// A session already running started its panel, desktop and window manager
	// at the old scale and cannot be told the new one any other way. Before the
	// session is released this is free: it has not started yet, and it will
	// read the file we just wrote.
	restarted := false
	if changed && d.released {
		if err := d.restartSession(ctx); err != nil {
			// Not fatal. The scale is set, everything launched from here on is
			// correct, and the desktop that is already up is merely stale --
			// which is better than refusing the change outright.
			d.log("restart the desktop session after a scale change", err)
		} else {
			restarted = true
		}
	}
	geometry, err := d.geometryLocked(ctx)
	return geometry, restarted, err
}

// Release marks the desktop session as started. Everything before it is setup;
// everything after it is a change to a running desktop.
func (d *Display) Release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.released = true
}

// Applied reports whether a scale has been written out this run.
func (d *Display) Applied() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.applied
}

// Chosen reports that somebody set the scale deliberately, so the viewer's
// auto-detection has stopped overriding it. Recorded even when applying the
// scale then failed: the person decided either way.
func (d *Display) Chosen() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.chosen
}

func (d *Display) log(what string, err error) {
	if d.Log != nil {
		d.Log.Warn("desktop: "+what, "error", err)
	}
}

// AdoptScale records the scale the desktop will start at and writes the
// environment the session reads, **without touching X**.
//
// It is the boot-time half of SetScale. Everything SetScale does to the running
// display — the server's DPI, xfconf, the window decoration theme — needs an X
// server, and asking for one is what starts the whole desktop. None of it is
// needed yet: at this point nothing is drawing, and the only consumer is the
// session's own environment file, which is a file.
//
// The X-side half happens when something first needs pixels, through SetScale
// or Resize.
func (d *Display) AdoptScale(scale int) error {
	scale = clamp(scale, MinScale, MaxScale)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scale = scale
	d.applied = true
	if d.EnvDir == "" {
		return nil
	}
	_, err := writeScaleEnv(d.EnvDir, scale)
	return err
}

// Scale reports the standing scale.
func (d *Display) Scale() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.scale
}

func (d *Display) dpiLocked() int {
	return BaseDPI * d.scale
}

func (d *Display) geometryOf(width, height int, output string) Geometry {
	return Geometry{
		Width:  width,
		Height: height,
		DPI:    d.dpiLocked(),
		Scale:  d.scale,
		Chosen: d.chosen,
		Output: output,
	}
}

// GeometryIfUp reports the screen only when this process has already
// established that there is one, and otherwise reports nothing and starts
// nothing.
//
// The distinction matters because every route to the geometry runs xrandr, and
// xrandr on a cold display is not a read — it is a socket connection to
// /tmp/.X11-unix/X0, which starts the X server and the desktop session behind
// it. Callers that merely want to *describe* the display have to be able to ask
// without conjuring one.
func (d *Display) GeometryIfUp(ctx context.Context) (Geometry, error) {
	if !d.up.Load() {
		return Geometry{}, errNotUp
	}
	return d.Geometry(ctx)
}

// errNotUp is returned rather than an empty geometry so a caller cannot mistake
// "there is no display yet" for "the display is 0x0".
var errNotUp = errors.New("desktop: the display has not been started")

// Geometry reports the screen as it stands.
func (d *Display) Geometry(ctx context.Context) (Geometry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.geometryLocked(ctx)
}

func (d *Display) geometryLocked(ctx context.Context) (Geometry, error) {
	out, err := d.run(ctx, "--query")
	if err != nil {
		return Geometry{}, err
	}
	match := screenPattern.FindStringSubmatch(out)
	if match == nil {
		return Geometry{}, errors.New("desktop: could not read the current screen size from xrandr")
	}
	width, _ := strconv.Atoi(match[1])
	height, _ := strconv.Atoi(match[2])
	output, err := d.connectedOutput(ctx)
	if err != nil {
		return Geometry{}, err
	}
	return d.geometryOf(width, height, output), nil
}

func (d *Display) connectedOutput(ctx context.Context) (string, error) {
	out, err := d.run(ctx, "--query")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "connected" {
			return fields[0], nil
		}
	}
	return "", errors.New("desktop: no connected XRandR output")
}

// ensureMode creates the modeline for a size the server has not been asked for
// before and attaches it to the output. Modes are remembered so a viewer that
// settles between two sizes stops shelling out entirely.
func (d *Display) ensureMode(ctx context.Context, output, mode string, width, height int) error {
	if d.modes[output+" "+mode] {
		return nil
	}
	known, err := d.run(ctx, "--query")
	if err != nil {
		return err
	}
	if !strings.Contains(known, mode) {
		for len(d.modes) >= MaxDynamicModes {
			d.evictOldestModeLocked(ctx)
		}
		modeline, err := d.modeline(ctx, width, height)
		if err != nil {
			return err
		}
		if err := d.addTolerant(ctx, append([]string{"--newmode", mode}, modeline...)); err != nil {
			return err
		}
	}
	if err := d.addTolerant(ctx, []string{"--addmode", output, mode}); err != nil {
		return err
	}
	key := output + " " + mode
	if !d.modes[key] {
		d.modeOrder = append(d.modeOrder, key)
	}
	d.modes[key] = true
	return nil
}

// evictOldestModeLocked forgets the mode added longest ago and asks the server
// to drop it too.
//
// Forgetting is unconditional and removing is best effort, which is the right
// way round: this table is what has to stay bounded, and the server refuses to
// delete a mode that is currently in use. A mode the server kept is found again
// by the `known` check in ensureMode, which skips creating it and simply
// re-attaches -- so a failed removal costs one xrandr call the next time that
// size comes back, and nothing else.
func (d *Display) evictOldestModeLocked(ctx context.Context) {
	if len(d.modeOrder) == 0 {
		// Nothing of ours to evict: every entry came from a server that already
		// had the mode. Clearing the table is the only way to make room, and
		// costs only the re-attach above.
		d.modes = map[string]bool{}
		return
	}
	oldest := d.modeOrder[0]
	d.modeOrder = d.modeOrder[1:]
	delete(d.modes, oldest)
	output, mode, ok := strings.Cut(oldest, " ")
	if !ok {
		return
	}
	if _, err := d.run(ctx, "--delmode", output, mode); err != nil {
		d.log("detach a display mode that is no longer tracked", err)
		return
	}
	if _, err := d.run(ctx, "--rmmode", mode); err != nil {
		d.log("delete a display mode that is no longer tracked", err)
	}
}

// addTolerant runs an xrandr call whose only expected failure is that the thing
// it creates is already there — which happens whenever this process restarts
// against an X server it configured before, since the modes outlive it.
func (d *Display) addTolerant(ctx context.Context, args []string) error {
	_, err := d.run(ctx, args...)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}
	return nil
}

func (d *Display) modeline(ctx context.Context, width, height int) ([]string, error) {
	out, err := d.command(ctx, "gtf", strconv.Itoa(width), strconv.Itoa(height), strconv.Itoa(refreshRate))
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, modelinePrefix) {
			fields := strings.Fields(line)
			if len(fields) > 2 {
				return fields[2:], nil
			}
		}
	}
	return nil, fmt.Errorf("desktop: could not generate a %dx%d display mode", width, height)
}

// mergeResources loads resources into the X resource database, which is where
// every toolkit that reads a density at all reads it from.
func (d *Display) mergeResources(ctx context.Context, resources string) error {
	cmd := exec.CommandContext(ctx, "xrdb", "-merge")
	cmd.Env = d.env()
	cmd.Stdin = strings.NewReader(resources)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("desktop: merge X resources: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// run is every xrandr call, and the one place that learns the display exists.
// A call that succeeds proves there is an X server; the first such call is also
// usually what started it, which is why nothing calls this speculatively.
func (d *Display) run(ctx context.Context, args ...string) (string, error) {
	out, err := d.command(ctx, "xrandr", args...)
	if err == nil {
		d.up.Store(true)
	}
	return out, err
}

func (d *Display) command(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = d.env()
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("desktop: %s: %w: %s", name, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("desktop: %s: %w", name, err)
	}
	return string(out), nil
}

// env is the whole environment these tools get: the display to act on, a PATH
// that reaches them, and the two variables that decide *which* desktop they act
// on. HOME because a home directory is where a settings daemon looks for the
// user's own configuration, and the session bus address because xfconf is only
// reachable over it — the unit carries the desktop session's, so this service
// and the session it is a viewer for read and write the same settings.
func (d *Display) env() []string {
	env := []string{"DISPLAY=" + d.Name, "PATH=/usr/bin:/bin"}
	for _, name := range []string{"HOME", "DBUS_SESSION_BUS_ADDRESS"} {
		if value := os.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func bucket(value int) int {
	return (value / SizeBucket) * SizeBucket
}
