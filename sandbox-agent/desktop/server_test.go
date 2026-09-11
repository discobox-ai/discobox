package desktop

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// newTestServer is a viewer with nothing real behind it.
//
// Display and ScaleEnvDir are named explicitly and must stay that way:
// applyDefaults resolves them to ":0" and ~/.discobox/desktop, so a test that
// leaves them out drives whatever X server the developer is sitting in front of
// and writes their own scale.env. Inside a sandbox that is the desktop being
// worked on. :91 is a display number nothing here uses.
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	server, err := New(slog.New(slog.DiscardHandler), Config{
		FeedbackDir: dir,
		Display:     ":91",
		ScaleEnvDir: filepath.Join(t.TempDir(), "scale-env"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server, dir
}

func do(t *testing.T, server *Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = strings.NewReader(string(encoded))
	}
	request := httptest.NewRequestWithContext(t.Context(), method, target, reader)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return value
}

// The limits the page is served are the ones it has to agree with, so they are
// asserted here rather than left to drift against the constants.
func TestSessionCarriesTheLimitsThePageQuantizesWith(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := do(t, server, http.MethodGet, "/api/session", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	session := decode[sessionResponse](t, recorder)
	if session.Limits.Bucket != SizeBucket ||
		session.Limits.MinWidth != MinWidth || session.Limits.MaxWidth != MaxWidth ||
		session.Limits.MinHeight != MinHeight || session.Limits.MaxHeight != MaxHeight ||
		session.Limits.BaseDPI != BaseDPI ||
		session.Limits.MinScale != MinScale || session.Limits.MaxScale != MaxScale {
		t.Fatalf("limits do not match the display's own: %+v", session.Limits)
	}
	if !strings.Contains(session.Prompt, session.FeedbackPath) {
		t.Fatalf("the prompt must name the file it points at: %q", session.Prompt)
	}
	// The display the server was configured with, not a constant: the page
	// shows this, and a viewer that reported some other display would be
	// describing a desktop nobody is looking at.
	if session.Display != server.display.Name {
		t.Fatalf("display = %q, want the configured %q", session.Display, server.display.Name)
	}
}

// A session must still be servable with no X server behind it: the page needs
// this payload to render at all, and it asks for a size of its own regardless.
func TestSessionIsServedWithNoDisplayAttached(t *testing.T) {
	server, _ := newTestServer(t)
	server.display = NewDisplay(":91") // nothing is listening here
	recorder := do(t, server, http.MethodGet, "/api/session", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}
	if got := decode[sessionResponse](t, recorder).Geometry.Width; got != 0 {
		t.Fatalf("expected an unknown geometry, got width %d", got)
	}
}

func TestCaptureWritesTheNoteAndItsScreenshot(t *testing.T) {
	server, _ := newTestServer(t)
	png := []byte("\x89PNG\r\n\x1a\n fake")
	created := do(t, server, http.MethodPost, "/api/feedback", createRequest{
		Comment:    "The toolbar icons sit two pixels low.",
		Region:     testRegion(),
		Screenshot: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", created.Code, created.Body)
	}
	item := decode[Item](t, created)
	if item.ID != "df-0001" {
		t.Fatalf("unexpected id %q", item.ID)
	}

	listed := decode[feedbackResponse](t, do(t, server, http.MethodGet, "/api/feedback", nil))
	if len(listed.Items) != 1 || listed.Items[0].Comment != "The toolbar icons sit two pixels low." {
		t.Fatalf("unexpected listing: %+v", listed.Items)
	}

	shot := do(t, server, http.MethodGet, "/shots/df-0001.png", nil)
	if shot.Code != http.StatusOK {
		t.Fatalf("screenshot status = %d", shot.Code)
	}
	if shot.Body.String() != string(png) {
		t.Fatalf("screenshot was not served back verbatim")
	}
}

// An empty listing has to be an empty array, not null: the page iterates it.
func TestFeedbackListIsAnArrayWhenEmpty(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := do(t, server, http.MethodGet, "/api/feedback", nil)
	if !strings.Contains(recorder.Body.String(), `"items":[]`) {
		t.Fatalf("expected an empty array, got %s", recorder.Body)
	}
}

func TestCaptureRejectsBadInput(t *testing.T) {
	server, _ := newTestServer(t)
	for _, test := range []struct {
		name    string
		request createRequest
	}{
		{"no comment", createRequest{Region: testRegion()}},
		{"screenshot is not a PNG data URL", createRequest{
			Comment:    "something",
			Region:     testRegion(),
			Screenshot: "data:image/jpeg;base64,AAAA",
		}},
		{"screenshot is not base64", createRequest{
			Comment:    "something",
			Region:     testRegion(),
			Screenshot: "data:image/png;base64,!!!!",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := do(t, server, http.MethodPost, "/api/feedback", test.request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
			}
		})
	}
}

// A note taken without a screenshot is still a note.
func TestCaptureWithoutAScreenshot(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := do(t, server, http.MethodPost, "/api/feedback", createRequest{
		Comment: "the framebuffer could not be read",
		Region:  testRegion(),
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}
	if shot := decode[Item](t, recorder).Shot; shot != "" {
		t.Fatalf("expected no screenshot path, got %q", shot)
	}
}

// The three things the notes panel can do to a note it already saved.
// A capture carries two pictures of the same moment, and they travel and are
// served independently.
func TestACaptureCarriesBothPictures(t *testing.T) {
	server, dir := newTestServer(t)
	crop := []byte("\x89PNG\r\n\x1a\n crop")
	screen := []byte("\x89PNG\r\n\x1a\n screen")
	created := do(t, server, http.MethodPost, "/api/feedback", createRequest{
		Comment:    "the icons sit low",
		Region:     testRegion(),
		Screenshot: "data:image/png;base64," + base64.StdEncoding.EncodeToString(crop),
		Screen:     "data:image/png;base64," + base64.StdEncoding.EncodeToString(screen),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", created.Code, created.Body)
	}
	item := decode[Item](t, created)
	if item.Shot == "" || item.Screen == "" {
		t.Fatalf("a capture lost a picture: %+v", item)
	}
	for name, want := range map[string][]byte{
		"/shots/df-0001.png":        crop,
		"/shots/df-0001-screen.png": screen,
	} {
		recorder := do(t, server, http.MethodGet, name, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", name, recorder.Code)
		}
		if recorder.Body.String() != string(want) {
			t.Fatalf("GET %s served the wrong bytes", name)
		}
	}
	// The whole-desktop capture is a separate file, not a second copy of the
	// crop under another name.
	if string(crop) == string(screen) {
		t.Fatalf("the test fixture cannot tell the two apart")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "shots"))
	if err != nil {
		t.Fatalf("read shots: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected two files in shots/, got %d", len(entries))
	}
}

// A page that could not read the framebuffer sends neither, and that is still a
// note.
func TestACaptureWithNeitherPictureIsStillANote(t *testing.T) {
	server, _ := newTestServer(t)
	recorder := do(t, server, http.MethodPost, "/api/feedback", createRequest{
		Comment: "no framebuffer here", Region: testRegion(),
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}
	item := decode[Item](t, recorder)
	if item.Shot != "" || item.Screen != "" {
		t.Fatalf("pictures appeared from nowhere: %+v", item)
	}
}

func TestANoteCanBeEditedTickedAndDeleted(t *testing.T) {
	server, _ := newTestServer(t)
	created := do(t, server, http.MethodPost, "/api/feedback", createRequest{
		Comment: "the icons sit low", Region: testRegion(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	id := decode[Item](t, created).ID

	edited := do(t, server, http.MethodPatch, "/api/feedback/"+id, updateRequest{Comment: "the labels sit low"})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", edited.Code, edited.Body)
	}
	if items := decode[feedbackResponse](t, edited).Items; len(items) != 1 || items[0].Comment != "the labels sit low" {
		t.Fatalf("the edit did not take: %+v", items)
	}

	done := true
	ticked := do(t, server, http.MethodPatch, "/api/feedback/"+id, updateRequest{Done: &done})
	if ticked.Code != http.StatusOK {
		t.Fatalf("tick: %d %s", ticked.Code, ticked.Body)
	}
	items := decode[feedbackResponse](t, ticked).Items
	if len(items) != 1 || !items[0].Done {
		t.Fatalf("the tick did not take: %+v", items)
	}
	// Ticking must not disturb the words, and editing must not untick.
	if items[0].Comment != "the labels sit low" {
		t.Fatalf("ticking rewrote the comment: %+v", items[0])
	}

	deleted := do(t, server, http.MethodDelete, "/api/feedback/"+id, nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body)
	}
	if items := decode[feedbackResponse](t, deleted).Items; len(items) != 0 {
		t.Fatalf("the note survived the delete: %+v", items)
	}
}

func TestEditingAnUnknownNoteIsRefused(t *testing.T) {
	server, _ := newTestServer(t)
	for _, test := range []struct {
		name     string
		method   string
		body     any
		expected int
	}{
		{"edit", http.MethodPatch, updateRequest{Comment: "nope"}, http.StatusBadRequest},
		{"delete", http.MethodDelete, nil, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := do(t, server, test.method, "/api/feedback/df-0099", test.body)
			if recorder.Code != test.expected {
				t.Fatalf("status = %d, want %d", recorder.Code, test.expected)
			}
		})
	}
	// A request that changes nothing is a mistake, not a no-op success.
	created := do(t, server, http.MethodPost, "/api/feedback", createRequest{Comment: "x", Region: testRegion()})
	id := decode[Item](t, created).ID
	if recorder := do(t, server, http.MethodPatch, "/api/feedback/"+id, updateRequest{}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("an empty edit returned %d", recorder.Code)
	}
}

func TestPageAndScriptAreServed(t *testing.T) {
	server, _ := newTestServer(t)
	for path, want := range map[string]string{
		"/":        "<title>Discobox desktop</title>",
		"/app.js":  "import RFB from",
		"/app.css": "--mark: #f45cff",
	} {
		recorder := do(t, server, http.MethodGet, path, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("GET %s did not contain %q", path, want)
		}
	}
}

// The page's quantize() mirrors this arithmetic; if it changes here, it changes
// there. Both floor to the bucket so the framebuffer never exceeds the frame
// the viewer sized for it.
func TestNormalizeSizeFloorsToTheBucketWithinBounds(t *testing.T) {
	for _, test := range []struct{ w, h, wantW, wantH int }{
		{1920, 1080, 1920, 1024},
		{1919, 1079, 1856, 1024},
		{1, 1, MinWidth, MinHeight},
		{99999, 99999, MaxWidth, MaxHeight},
		{MaxWidth, MaxHeight, MaxWidth, MaxHeight},
		{MinWidth, MinHeight, MinWidth, MinHeight},
	} {
		gotW, gotH := NormalizeSize(test.w, test.h)
		if gotW != test.wantW || gotH != test.wantH {
			t.Fatalf("NormalizeSize(%d, %d) = %d, %d; want %d, %d",
				test.w, test.h, gotW, gotH, test.wantW, test.wantH)
		}
		if gotW < MinWidth || gotW > MaxWidth || gotH < MinHeight || gotH > MaxHeight {
			t.Fatalf("NormalizeSize(%d, %d) left the bounds: %d, %d", test.w, test.h, gotW, gotH)
		}
	}
}

// Every bound has to sit on a bucket boundary, or NormalizeSize has values it
// can never return and the viewer sizes its frame for a screen it cannot get.
func TestSizeBoundsSitOnBucketBoundaries(t *testing.T) {
	for name, bound := range map[string]int{
		"MinWidth": MinWidth, "MinHeight": MinHeight,
		"MaxWidth": MaxWidth, "MaxHeight": MaxHeight,
	} {
		if bound%SizeBucket != 0 {
			t.Fatalf("%s (%d) is not a multiple of SizeBucket (%d)", name, bound, SizeBucket)
		}
	}
}

// A browser may report any ratio at all; the desktop can only be one of a few
// integers. Fractional scales are what produced the 1.88x that started this.
func TestNormalizeScaleRoundsToASupportedInteger(t *testing.T) {
	for _, test := range []struct {
		in   float64
		want int
	}{
		{0, 1}, {-2, 1}, {1, 1}, {1.25, 1}, {1.5, 2}, {1.88, 2}, {2, 2}, {2.75, 3}, {9, MaxScale},
	} {
		if got := NormalizeScale(test.in); got != test.want {
			t.Fatalf("NormalizeScale(%v) = %v, want %v", test.in, got, test.want)
		}
	}
}

// The density is native until somebody asks for something else. A desktop
// nobody has configured must look like a plain 96 DPI desktop.
func TestANewDisplayIsNative(t *testing.T) {
	display := NewDisplay(":0")
	if display.scale != 1 {
		t.Fatalf("a new display starts at scale %v, want 1", display.scale)
	}
	if got := display.dpiLocked(); got != BaseDPI {
		t.Fatalf("a new display reports %d dpi, want the native %d", got, BaseDPI)
	}
}

// The bug this replaced: the density was computed from the browser's device
// pixel ratio and the framebuffer ceiling, so a large window on a HiDPI screen
// landed on 1.88x and 180 dpi, and every resize moved it again. Xft.dpi is
// latched by each program at launch, so a density derived from the window can
// never be made consistent. Resizing must not touch it.
func TestResizeRequestCarriesNoDensity(t *testing.T) {
	encoded, err := json.Marshal(resizeRequest{CSSWidth: 1920, CSSHeight: 1024})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"scale", "dpi", "devicePixelRatio"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("a resize request carries %q: %s", forbidden, encoded)
		}
	}
}

// %HOME% is a data volume, so the environment file outlives the process that
// wrote it while the in-memory scale resets to 1. Reading it back is what stops
// a restarted sandbox from starting its session at one scale and its server at
// another.
func TestTheScaleIsRememberedAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	if _, ok := RememberedScale(dir); ok {
		t.Fatalf("an empty directory remembered a scale")
	}
	if _, err := WriteScaleEnv(dir, 2); err != nil {
		t.Fatalf("WriteScaleEnv: %v", err)
	}
	scale, ok := RememberedScale(dir)
	if !ok || scale != 2 {
		t.Fatalf("RememberedScale = %d, %v; want 2, true", scale, ok)
	}
}

// Nothing remembered starts at the default, and a remembered scale starts at
// itself -- whichever of the boot flow, the viewer or the session is asking.
func TestTheStartingScaleIsTheRememberedOneOrTheDefault(t *testing.T) {
	dir := t.TempDir()
	if scale, remembered := StartingScale(dir); scale != DefaultScale || remembered {
		t.Fatalf("StartingScale with nothing written = %d, %v; want %d, false", scale, remembered, DefaultScale)
	}
	if _, err := WriteScaleEnv(dir, 1); err != nil {
		t.Fatalf("WriteScaleEnv: %v", err)
	}
	if scale, remembered := StartingScale(dir); scale != 1 || !remembered {
		t.Fatalf("StartingScale after writing 1 = %d, %v; want 1, true", scale, remembered)
	}
}

// A scale equal to the one in memory still has to be written the first time:
// "unchanged" says nothing about whether the file on disk agrees, and the file
// is what the desktop session reads.
//
// The same call also pins the other half: with no X server the live half fails,
// and nothing may be committed when it does. A display that recorded the scale
// up front would short-circuit every later call at that value and could never
// be repaired.
func TestAnUnchangedScaleIsStillWrittenOnce(t *testing.T) {
	dir := t.TempDir()
	display := NewDisplay(":91") // no X server, so only the durable half runs
	display.EnvDir = dir
	if display.Applied() {
		t.Fatalf("a new display claims to have applied a scale")
	}
	if _, _, err := display.SetScale(t.Context(), 1, true); err == nil {
		t.Fatalf("expected the call to fail with no X server behind it")
	}
	scale, ok := RememberedScale(dir)
	if !ok || scale != 1 {
		t.Fatalf("RememberedScale = %d, %v; want 1, true: an unchanged scale is still written the first time", scale, ok)
	}
	if display.Applied() {
		t.Fatalf("a call that failed to reach the display marked the scale applied, so no retry can repair it")
	}
}

// The environment file is what a login shell sources and what the session unit
// takes as an EnvironmentFile, so its format has to stay sourceable.
func TestScaleEnvIsPlainKeyValue(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteScaleEnv(dir, 2)
	if err != nil {
		t.Fatalf("WriteScaleEnv: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := map[string]string{
		"DISCOBOX_DESKTOP_SCALE": "2",
		"GDK_SCALE":              "2",
		// Divides GTK's own doubling back out of the font size, which the
		// raised Xft.dpi would otherwise apply a second time.
		"GDK_DPI_SCALE": "0.5",
		// Not 48. The cursor is composited by the browser outside the scaled
		// framebuffer, so scaling it makes the pointer twice the size it
		// should be at 2x. See baseCursorSize.
		"XCURSOR_SIZE": "24",
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("line %q is not KEY=VALUE and would break `set -a; . file`", line)
		}
		got[name] = value
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s = %q, want %q (whole file:\n%s)", name, got[name], value, data)
		}
	}
}

// The scale is set on its own endpoint, by the viewer reporting its screen or
// by a person overriding it.
// The pointer has to be the same size over every window. GDK multiplies the
// XSETTINGS cursor size by the window scale factor and libXcursor does not
// multiply XCURSOR_SIZE at all, so the two settings have to differ by the scale
// to end up at the same number of device pixels. They agreeing on a number is
// the bug, not the fix: it is what made the pointer double as it crossed from
// the desktop onto an application window.
func TestCursorSizesAgreeInDevicePixels(t *testing.T) {
	for scale := MinScale; scale <= MaxScale; scale++ {
		fromEnv, err := strconv.Atoi(scaleEnv(scale)["XCURSOR_SIZE"])
		if err != nil {
			t.Fatalf("XCURSOR_SIZE at scale %d: %v", scale, err)
		}
		fromGDK := cursorThemeSize(scale) * scale
		if fromEnv != baseCursorSize {
			t.Fatalf("XCURSOR_SIZE at scale %d = %d device px, want %d", scale, fromEnv, baseCursorSize)
		}
		if fromGDK != baseCursorSize {
			t.Fatalf("GDK cursor at scale %d = %d device px (%d x %d), want %d",
				scale, fromGDK, cursorThemeSize(scale), scale, baseCursorSize)
		}
	}
}

func TestScaleIsSetOnItsOwnEndpoint(t *testing.T) {
	dir := t.TempDir()
	server, _ := newTestServer(t)
	server.display = NewDisplay(":91") // no X server behind it
	server.display.EnvDir = dir
	recorder := do(t, server, http.MethodPost, "/api/scale", scaleRequest{Scale: 2})
	// The call cannot succeed with no display, but it must reach the display
	// rather than be rejected as a bad request.
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}
	// What the endpoint reaching the display looks like from outside: the
	// durable record moved. The in-memory scale deliberately did not, because
	// the live half never ran.
	scale, ok := RememberedScale(dir)
	if !ok || scale != 2 {
		t.Fatalf("RememberedScale = %d, %v; want 2, true: the request never reached the display", scale, ok)
	}
}

// A person's choice outranks the viewer's detection, so opening the desktop in
// a second browser on an ordinary screen cannot undo a HiDPI session.
//
// Asserted on the environment file rather than on the in-memory scale, because
// the file is the part that outlives the process and the part the session
// reads — and with no X server the in-memory scale is deliberately not
// committed at all.
func TestAutoDetectionDoesNotOverrideAChoice(t *testing.T) {
	dir := t.TempDir()
	display := NewDisplay(":91")
	display.EnvDir = dir
	if _, _, err := display.SetScale(t.Context(), 2, false); err == nil {
		t.Fatalf("expected the call to fail with no X server behind it")
	}
	if !display.Chosen() {
		t.Fatalf("an explicit scale did not record that somebody chose it")
	}
	if scale, ok := RememberedScale(dir); !ok || scale != 2 {
		t.Fatalf("RememberedScale = %d, %v; want 2, true", scale, ok)
	}
	// The error is incidental — there is no X server to read a geometry back
	// from. What matters is that the detection did not rewrite the choice.
	_, _, _ = display.SetScale(t.Context(), 1, true)
	if scale, ok := RememberedScale(dir); !ok || scale != 2 {
		t.Fatalf("auto detection overrode a chosen scale: RememberedScale = %d, %v", scale, ok)
	}
}

// The guarantee: starting the viewer must not start the desktop.
//
// It used to. settleScale called a readiness loop (since deleted) whose xrandr
// connects to
// /tmp/.X11-unix/X0 — which socket-activates the X server, and xvfb.service
// pulls the Xfce session up behind it. So anything that opened a TCP connection
// to 6900 and went away brought up an X server, a window manager, a panel and a
// VNC server, in a sandbox where nobody had asked to see a desktop.
//
// :91 has no X server, so any attempt to reach one fails and is visible as a
// non-nil error. What this asserts is that no attempt is made at all.
func TestSettlingTheScaleNeverTouchesTheDisplay(t *testing.T) {
	server, dir := newTestServer(t)
	server.display = NewDisplay(":91")
	server.display.EnvDir = filepath.Join(dir, "env")

	server.settleScale()

	// The session's environment is written, which is the whole job.
	scale, ok := RememberedScale(server.display.EnvDir)
	if !ok || scale != DefaultScale {
		t.Fatalf("settleScale did not write the default scale: %d, %v", scale, ok)
	}
	// And nothing has talked to X, so a read-only caller still gets "no
	// display" rather than a geometry conjured by asking for one.
	if _, err := server.display.GeometryIfUp(t.Context()); err == nil {
		t.Fatalf("settleScale brought a display up")
	}
}

// A sandbox that ran a desktop before comes back at the scale it was left at,
// still without starting one.
func TestARememberedScaleIsAdoptedWithoutADisplay(t *testing.T) {
	server, dir := newTestServer(t)
	server.display = NewDisplay(":91")
	server.display.EnvDir = filepath.Join(dir, "env")
	if _, err := WriteScaleEnv(server.display.EnvDir, 2); err != nil {
		t.Fatalf("WriteScaleEnv: %v", err)
	}

	server.settleScale()

	if got := server.display.Scale(); got != 2 {
		t.Fatalf("scale = %d, want the remembered 2", got)
	}
	if _, err := server.display.GeometryIfUp(t.Context()); err == nil {
		t.Fatalf("adopting a remembered scale brought a display up")
	}
}

// Describing the display must not create one either: a client fetching the
// session is not yet somebody looking at a desktop.
func TestTheSessionEndpointDoesNotStartADisplay(t *testing.T) {
	server, _ := newTestServer(t)
	server.display = NewDisplay(":91")

	recorder := do(t, server, http.MethodGet, "/api/session", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := decode[sessionResponse](t, recorder).Geometry.Width; got != 0 {
		t.Fatalf("geometry = %d, want none reported for a display nobody has started", got)
	}
	if _, err := server.display.GeometryIfUp(t.Context()); err == nil {
		t.Fatalf("serving the session brought a display up")
	}
}

// Splitting a module out of app.js is only half the job: the browser fetches
// what it imports, and a specifier with no route behind it is a page that fails
// to start with nothing in the server log. Routes are listed one by one rather
// than served as a tree, so this is easy to forget.
//
// The proxied trees are left out: /novnc/ and /brand/ are served from
// directories on the image, not from the embedded assets, so a test server has
// nothing behind them.
func TestEveryModuleThePageImportsIsServed(t *testing.T) {
	server, _ := newTestServer(t)

	source, err := fs.ReadFile(server.static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	specifiers := regexp.MustCompile(`from\s+'(/[^']+)'`).FindAllStringSubmatch(string(source), -1)
	if len(specifiers) == 0 {
		t.Fatal("app.js imports nothing; this test is no longer reading it correctly")
	}

	checked := 0
	for _, match := range specifiers {
		path := match[1]
		if strings.HasPrefix(path, "/novnc/") || strings.HasPrefix(path, "/brand/") {
			continue
		}
		checked++
		recorder := do(t, server, http.MethodGet, path, nil)
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: app.js imports it, so the page will not start without a route", path, recorder.Code)
		}
	}
	if checked == 0 {
		t.Fatal("no same-origin module imports were checked")
	}
}

// The environment file and the in-memory scale are one fact, and a failure in
// the live half must not split them.
//
// They are read by different consumers: the file becomes GDK_SCALE for the
// desktop session, while d.scale is what Resize multiplies the framebuffer by
// and what the DPI is asserted from. A scale change whose X half fails — a cold
// display taking longer than the command timeout — used to leave the file at the
// new value and the process at the old one, which is 2x widgets around 1x text
// once the session starts.
func TestAFailedScaleChangeDoesNotSplitTheFileFromTheDisplay(t *testing.T) {
	dir := t.TempDir()
	display := NewDisplay(":91") // no X server, so the live half always fails
	display.EnvDir = dir

	if _, _, err := display.SetScale(t.Context(), 2, false); err == nil {
		t.Fatalf("expected the call to fail with no X server behind it")
	}
	remembered, ok := RememberedScale(dir)
	if !ok {
		t.Fatalf("the scale was not written")
	}
	if got := display.Scale(); got != remembered {
		t.Fatalf("the display says %d and the session will be handed %d", got, remembered)
	}
	// Not applied, so the live half is retried rather than short-circuited.
	if display.Applied() {
		t.Fatalf("a half-finished scale change marked itself applied")
	}
}

// And the retry that repairs it needs no user action. An auto report is what
// every page load sends, so it must be able to finish a chosen scale whose live
// half failed — the choice outranks the reported *value*, not the work.
func TestAnAutoReportStillFinishesAnUnappliedChoice(t *testing.T) {
	dir := t.TempDir()
	display := NewDisplay(":91")
	display.EnvDir = dir

	_, _, _ = display.SetScale(t.Context(), 2, false)
	if !display.Chosen() || display.Applied() {
		t.Fatalf("expected a chosen, unapplied scale: chosen=%v applied=%v", display.Chosen(), display.Applied())
	}

	// Removed so the next call's work is observable: only a call that gets past
	// the guard and runs the durable half again puts it back.
	if err := os.Remove(filepath.Join(dir, ScaleEnvName)); err != nil {
		t.Fatalf("remove the environment file: %v", err)
	}

	// A 1x browser reporting itself must not move the scale...
	_, _, _ = display.SetScale(t.Context(), 1, true)
	if got := display.Scale(); got != 2 {
		t.Fatalf("an auto report overrode a chosen scale: %d", got)
	}
	// ...and must still have carried on, which is what repairs a half-finished
	// change once there is an X server to finish it against.
	scale, ok := RememberedScale(dir)
	if !ok {
		t.Fatalf("the auto report returned early instead of finishing the unapplied choice")
	}
	if scale != 2 {
		t.Fatalf("the auto report wrote %d; a choice outranks the reported value", scale)
	}
}
