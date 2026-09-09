package desktop

import (
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// What a note is saved with, drawn by a real browser and checked as bytes.
//
// crop and screenWithRegion are the only part of the viewer that produces
// something durable, and the only part no other test could reach: they are
// canvas drawing, so a Go test cannot run them and jsdom has no canvas. What
// they produce is what an agent reads back weeks later, and "the box is drawn
// somewhere near the region" is not good enough — a box that covers the thing
// it points at hides the evidence.
//
// So this drives the real thing: headless Chromium imports /capture.js from the
// real server, draws a framebuffer with a known pattern, calls the real
// functions, and posts the result through the real POST /api/feedback. Nothing
// is stubbed. Go then decodes the PNG the store wrote and asserts pixels, which
// closes the loop end to end — the browser's drawing, the data-URL encoding, the
// handler's decode, and the file on disk are all in the path being checked.

const (
	// The framebuffer the harness paints, and the region marked on it. Chosen
	// so the border lands on whole pixels and every assertion below is exact.
	frameW, frameH = 320, 200
	regionX        = 100
	regionY        = 60
	regionW        = 80
	regionH        = 50

	// How long to wait for the browser to report. Both tests here finish in
	// well under a second once the browser is up, so this only has to cover a
	// cold start on a loaded machine -- and it is also how long a genuine
	// failure takes to surface, which is the reason not to make it generous.
	browserBudget = 45 * time.Second
)

var (
	// The pattern painted into the fake framebuffer.
	inside  = rgb{0, 255, 0}
	outside = rgb{0, 0, 255}

	// marked is the outline screenWithRegion strokes, from web/capture.js.
	marked = rgb{0xf4, 0x5c, 0xff}

	// dimmed is `outside` under the rgba(11, 9, 14, 0.55) wash capture.js lays
	// over everything the region does not cover, composited source-over.
	dimmed = outside.under(rgb{11, 9, 14}, 0.55)
)

type rgb struct{ r, g, b uint8 }

func (c rgb) String() string { return fmt.Sprintf("rgb(%d, %d, %d)", c.r, c.g, c.b) }

// under composites an opaque color beneath a wash of alpha a, the way a canvas
// source-over fill does.
func (c rgb) under(wash rgb, a float64) rgb {
	mix := func(dst, src uint8) uint8 {
		return uint8(float64(src)*a + float64(dst)*(1-a))
	}
	return rgb{mix(c.r, wash.r), mix(c.g, wash.g), mix(c.b, wash.b)}
}

func TestTheCapturesABrowserDrawsAreTheBytesTheStoreKeeps(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real browser")
	}
	browser := findBrowser(t)

	server, dir := newTestServer(t)
	reports := make(chan harnessReport, 1)

	// The harness is served from the server's own origin so the page can import
	// /capture.js as a module and post to /api/feedback without CORS in the way
	// — the same two things the real page does.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__harness/{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, harnessPage, frameW, frameH, regionX, regionY, regionW, regionH)
	})
	mux.HandleFunc("POST /__harness/report", func(w http.ResponseWriter, r *http.Request) {
		var report harnessReport
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			report.Error = "decode the harness report: " + err.Error()
		}
		select {
		case reports <- report:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", server)

	origin := httptest.NewServer(mux)
	defer origin.Close()

	runBrowser(t, browser, origin.URL+"/__harness/")

	var report harnessReport
	select {
	case report = <-reports:
	case <-time.After(browserBudget):
		t.Fatal("the harness page never reported; the browser did not run it")
	}
	t.Logf("the harness reported %+v", report)
	if report.Error != "" {
		t.Fatalf("the harness failed: %s", report.Error)
	}
	if report.Status != http.StatusCreated {
		t.Fatalf("POST /api/feedback = %d, want %d", report.Status, http.StatusCreated)
	}
	if report.Shot == "" || report.Screen == "" {
		t.Fatalf("the note kept shot=%q screen=%q, want both", report.Shot, report.Screen)
	}

	// The crop is the marked region and nothing else: same size, and every
	// sample is the untouched color that was under it.
	shot := readShot(t, dir, report.Shot)
	if got := shot.Bounds().Dx(); got != regionW {
		t.Errorf("crop width = %d, want %d", got, regionW)
	}
	if got := shot.Bounds().Dy(); got != regionH {
		t.Errorf("crop height = %d, want %d", got, regionH)
	}
	for _, at := range []image.Point{{X: 0, Y: 0}, {X: regionW / 2, Y: regionH / 2}, {X: regionW - 1, Y: regionH - 1}} {
		assertPixel(t, shot, at, inside, 0, "the crop is the region, undimmed and unmarked")
	}

	screen := readShot(t, dir, report.Screen)
	if got, want := screen.Bounds().Dx(), frameW; got != want {
		t.Errorf("screen width = %d, want the whole framebuffer, %d", got, want)
	}
	if got, want := screen.Bounds().Dy(), frameH; got != want {
		t.Errorf("screen height = %d, want the whole framebuffer, %d", got, want)
	}

	// Everything outside the region is dimmed...
	for _, at := range []image.Point{{X: 20, Y: 20}, {X: frameW - 20, Y: frameH - 20}, {X: frameW / 2, Y: 10}} {
		assertPixel(t, screen, at, dimmed, 3, "outside the region is dimmed")
	}

	// ...and the region itself is not. This is the property worth having: the
	// point of the picture is to show what was wrong, so nothing may be painted
	// over the thing being pointed at.
	for _, at := range []image.Point{
		{X: regionX, Y: regionY},
		{X: regionX + regionW/2, Y: regionY + regionH/2},
		{X: regionX + regionW - 1, Y: regionY + regionH - 1},
	} {
		assertPixel(t, screen, at, inside, 0, "the marked region is left exactly as it was")
	}

	// The outline frames the region from outside it. lineWidth is 2 at this
	// width, and the rect is inset by half of it, so the stroke covers the two
	// columns and rows just beyond each edge and none of the region.
	for _, at := range []image.Point{
		{X: regionX - 1, Y: regionY + regionH/2},       // left
		{X: regionX + regionW, Y: regionY + regionH/2}, // right
		{X: regionX + regionW/2, Y: regionY - 1},       // top
		{X: regionX + regionW/2, Y: regionY + regionH}, // bottom
	} {
		assertPixel(t, screen, at, marked, 2, "the outline is drawn just outside the region")
	}
}

type harnessReport struct {
	Error  string `json:"error"`
	Status int    `json:"status"`
	Shot   string `json:"shot"`
	Screen string `json:"screen"`
}

func assertPixel(t *testing.T, img image.Image, at image.Point, want rgb, tolerance int, why string) {
	t.Helper()
	at = at.Add(img.Bounds().Min)
	r16, g16, b16, a16 := img.At(at.X, at.Y).RGBA()
	got := rgb{uint8(r16 >> 8), uint8(g16 >> 8), uint8(b16 >> 8)}
	if a16>>8 != 255 {
		t.Errorf("(%d,%d) alpha = %d, want an opaque picture", at.X, at.Y, a16>>8)
	}
	off := func(a, b uint8) int {
		if a > b {
			return int(a - b)
		}
		return int(b - a)
	}
	if off(got.r, want.r) > tolerance || off(got.g, want.g) > tolerance || off(got.b, want.b) > tolerance {
		t.Errorf("(%d,%d) = %s, want %s: %s", at.X, at.Y, got, want, why)
	}
}

func readShot(t *testing.T, storeDir, rel string) image.Image {
	t.Helper()
	path := filepath.Join(storeDir, filepath.FromSlash(rel))
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the stored capture: %v", err)
	}
	defer file.Close()
	img, err := png.Decode(file)
	if err != nil {
		t.Fatalf("decode %s: %v", rel, err)
	}
	return img
}

func findBrowser(t *testing.T) string {
	t.Helper()
	if named := os.Getenv("DISCOBOX_TEST_BROWSER"); named != "" {
		return named
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Skip("no Chromium on PATH; set DISCOBOX_TEST_BROWSER to run this")
	return ""
}

func runBrowser(t *testing.T, browser, url string) {
	t.Helper()
	// Not t.TempDir: the browser goes on writing into its profile while it
	// shuts down, so the framework's cleanup races it and fails the test on a
	// directory it could not empty. This one is removed after the kill, and a
	// leftover in the OS temp dir is not worth failing a passing test over.
	profile, err := os.MkdirTemp("", "discobox-capture-browser-")
	if err != nil {
		t.Fatalf("browser profile: %v", err)
	}
	//nolint:gosec // G204: the browser is one this test found on PATH or the developer named, and the URL is this test's own server.
	cmd := exec.CommandContext(t.Context(), browser,
		"--headless",
		// The test may run in a container without user namespaces, and the page
		// is one this test wrote and serves to itself.
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--no-first-run",
		"--user-data-dir="+profile,
		url,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", browser, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(profile)
	})
}

// harnessPage paints a framebuffer the way noVNC would, runs the real capture
// functions over it, and posts the result the way the real page does. It asserts
// nothing itself: the pixels are checked in Go, against the file that was
// written, so the encode, the transport and the store are inside the test too.
const harnessPage = `<!doctype html>
<meta charset="utf-8">
<title>capture harness</title>
<script type="module">
import { crop, screenWithRegion } from '/capture.js';

const report = (body) => navigator.sendBeacon('/__harness/report', new Blob(
  [JSON.stringify(body)], { type: 'application/json' },
));

try {
  const region = { x: %[3]d, y: %[4]d, w: %[5]d, h: %[6]d, frameWidth: %[1]d, frameHeight: %[2]d };

  const frame = document.createElement('canvas');
  frame.width = %[1]d;
  frame.height = %[2]d;
  const ctx = frame.getContext('2d');
  ctx.fillStyle = 'rgb(0, 0, 255)';
  ctx.fillRect(0, 0, frame.width, frame.height);
  ctx.fillStyle = 'rgb(0, 255, 0)';
  ctx.fillRect(region.x, region.y, region.w, region.h);

  const screenshot = crop(frame, region);
  const screen = screenWithRegion(frame, region);
  if (!screenshot || !screen) throw new Error('the capture functions returned nothing');

  const response = await fetch('/api/feedback', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ comment: 'drawn by the capture harness', region, screenshot, screen }),
  });
  const item = response.ok ? await response.json() : {};
  report({ status: response.status, shot: item.shot || '', screen: item.screen || '' });
} catch (err) {
  report({ error: String(err && err.stack || err) });
}
</script>
`

// The page boots in a real browser.
//
// app.js runs at import and fetches /api/session before it does anything else,
// so that request arriving is proof the whole module graph resolved and ran: the
// document parsed, every import found a route, and nothing threw on the way in.
// A missing route or a syntax error anywhere in it means the request never
// comes — which is exactly how the page fails in production, silently, with an
// empty screen and nothing in the server log.
//
// Only noVNC is stubbed, because it is a third-party library that ships in the
// image rather than in this repository, so a test has nothing real to serve.
// index.html, app.js and capture.js are the real ones.
func TestThePageBootsInABrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real browser")
	}
	browser := findBrowser(t)

	novnc := filepath.Join(t.TempDir(), "novnc")
	if err := os.MkdirAll(filepath.Join(novnc, "core"), 0o755); err != nil {
		t.Fatalf("stub noVNC: %v", err)
	}
	stub := "export default class RFB { constructor() {} addEventListener() {} disconnect() {} }\n"
	if err := os.WriteFile(filepath.Join(novnc, "core", "rfb.js"), []byte(stub), 0o644); err != nil {
		t.Fatalf("stub noVNC: %v", err)
	}

	// Display and ScaleEnvDir are the load-bearing ones: this test loads the
	// real app.js, whose boot() posts to /api/scale and /api/display before it
	// does anything else. Left to applyDefaults those resolve to ":0" and
	// ~/.discobox/desktop — so the test would set the density of, and resize,
	// whatever desktop the developer is actually looking at, and rewrite their
	// own scale.env. :91 is a display number nothing here uses.
	server, err := New(slog.New(slog.DiscardHandler), Config{
		FeedbackDir: t.TempDir(),
		NoVNCDir:    novnc,
		Display:     ":91",
		ScaleEnvDir: filepath.Join(t.TempDir(), "scale-env"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	booted := make(chan struct{}, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session" {
			select {
			case booted <- struct{}{}:
			default:
			}
		}
		server.ServeHTTP(w, r)
	}))
	defer origin.Close()

	runBrowser(t, browser, origin.URL+"/")

	select {
	case <-booted:
	case <-time.After(browserBudget):
		t.Fatal("the page never asked for /api/session; its modules did not load or it threw on the way up")
	}
}
