package desktop

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// stubVoxlClient stands in for the client.js the desktop feature builds from
// voxl's source: the names stream.js imports, doing nothing. The input modules
// are voxl's own and are not what these tests are about.
const stubVoxlClient = `export const Bin = { ClipImageUp: 17 };
export const cmd = { releaseAll: 'r', clipboard: (text) => 'c,' + text };
export function createCursor() { return { set() {}, apply() {} }; }
export function setupKeyboard() { return { releaseAll() {} }; }
export function setupPointer() {}
`

// stubVoxlWorker stands in for voxl's decode worker, speaking the same
// messages to the page. What it does is the harness's choice, read from
// /__stream/mode when it starts:
//
//   - "ok": the stream opens, says hello at 320x200, presents one green frame,
//     and answers every clipboard it is sent with the desktop's clipboard
//     changing to it, as voxl does when the desktop takes a paste.
//   - "silent": clipboards it is sent are not answered, as voxl does not answer
//     one the page set itself.
//   - "refuse": every connection closes at once, as voxl's worker reports a
//     stream that will not start, and it keeps retrying, as voxl's does.
const stubVoxlWorker = `self.addEventListener('message', async (event) => {
  const message = event.data;
  if (message.t === 'init') {
    const mode = await (await fetch('/__stream/mode')).text();
    if (mode === 'refuse') {
      setInterval(() => self.postMessage({ t: 'status', s: 'closed' }), 30);
      return;
    }
    self.postMessage({ t: 'status', s: 'open' });
    self.postMessage({ t: 'hello', w: 320, h: 200 });
    const canvas = new OffscreenCanvas(320, 200);
    const ctx = canvas.getContext('2d');
    ctx.fillStyle = 'rgb(0, 255, 0)';
    ctx.fillRect(0, 0, 320, 200);
    const frame = new VideoFrame(canvas, { timestamp: 0 });
    self.postMessage({ t: 'frames', parts: [{ i: 0, y: 0, frame }] }, [frame]);
    return;
  }
  if (message.t === 'send' && message.d.startsWith('c,')) {
    if ((await (await fetch('/__stream/mode')).text()) === 'silent') return;
    self.postMessage({ t: 'clip', d: message.d.slice(2) });
  }
});
`

// streamHarnessPage drives the real stream.js, with the stubs above behind it,
// and reports what it saw. It asserts nothing itself; Go does.
const streamHarnessPage = `<!doctype html>
<meta charset="utf-8">
<title>stream harness</title>
<div id="surface" style="width:320px;height:200px"></div>
<textarea id="kbd"></textarea>
<script type="module">
import { openStream } from '/stream.js';

const step = (what) => console.log('stream harness: ' + what);
const report = (body) => fetch('/__stream/report', { method: 'POST', body: JSON.stringify(body) });
const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const until = async (check, what) => {
  for (let i = 0; i < 200; i++) {
    if (check()) return;
    await wait(25);
  }
  throw new Error('timed out waiting for ' + what);
};

// The local clipboard, watched: every write this page makes lands here.
const writes = [];
navigator.clipboard.writeText = async (text) => { writes.push(text); };
navigator.clipboard.readText = async () => { throw new Error('read the local clipboard'); };

try {
  const seen = { frames: 0, clipboard: [], copied: [], failed: [], connecting: [] };
  const stream = openStream({
    surface: document.getElementById('surface'),
    kbd: document.getElementById('kbd'),
    on: {
      connecting: (attempt) => seen.connecting.push(attempt),
      hello: () => {},
      frame: () => { seen.frames++; },
      closed: () => {},
      failed: (reason) => seen.failed.push(reason),
      clipboard: (text) => seen.clipboard.push(text),
      copied: (text) => { seen.copied.push(text); navigator.clipboard.writeText(text); },
      notice: () => {},
    },
  });
  await until(() => seen.frames > 0, 'the first frame');
  step('framed');

  // The frame is on the page's canvas, and the snapshot is a copy of it.
  const shot = stream.snapshot();
  const pixel = Array.from(shot.getContext('2d').getImageData(160, 100, 1, 1).data);

  // A clipboard sent to the desktop is what the desktop's clipboard holds,
  // whether or not the desktop says so: voxl does not.
  await fetch('/__stream/mode', { method: 'POST', body: 'silent' });
  const sentSilently = stream.sendClipboard('sent without an echo');
  const remoteAfterSilentSend = stream.remoteClipboard();
  await fetch('/__stream/mode', { method: 'POST', body: 'ok' });

  // A clipboard sent to the desktop changes the desktop's clipboard, which is
  // held here and not written to this computer.
  const sent = stream.sendClipboard('to the desktop');
  await until(() => seen.clipboard.includes('to the desktop'), 'the desktop clipboard');
  await wait(200);
  const writesAfterRemoteChange = writes.slice();
  const remoteBeforeCopy = stream.remoteClipboard();

  // A copy typed into the desktop brings the next change back.
  document.getElementById('kbd').dispatchEvent(new KeyboardEvent('keydown', { key: 'c', ctrlKey: true, bubbles: true }));
  stream.sendClipboard('copied on the desktop');
  await until(() => seen.copied.length > 0, 'the copy');
  await wait(100);
  const writesAfterCopy = writes.slice();

  // Text over the bound is refused before it is sent.
  const tooLarge = stream.sendClipboard('x'.repeat((1 << 20) + 1));

  // A stream that will not open is retried a bounded number of times, and
  // reconnecting forgets the desktop's clipboard.
  await fetch('/__stream/mode', { method: 'POST', body: 'refuse' });
  stream.reconnect();
  const remoteAfterReconnect = stream.remoteClipboard();
  await until(() => seen.failed.length > 0, 'giving up');
  await wait(300);

  report({
    pixel, width: shot.width, height: shot.height, sent, tooLarge, sentSilently, remoteAfterSilentSend,
    remoteBeforeCopy, remoteAfterReconnect,
    writesAfterRemoteChange, writesAfterCopy,
    failed: seen.failed, connecting: seen.connecting,
  });
} catch (err) {
  report({ error: String(err && err.stack || err) });
}
</script>
`

type streamHarnessReport struct {
	Error                   string   `json:"error"`
	Pixel                   []int    `json:"pixel"`
	Width                   int      `json:"width"`
	Height                  int      `json:"height"`
	Sent                    string   `json:"sent"`
	TooLarge                string   `json:"tooLarge"`
	SentSilently            string   `json:"sentSilently"`
	RemoteAfterSilentSend   *string  `json:"remoteAfterSilentSend"`
	RemoteBeforeCopy        *string  `json:"remoteBeforeCopy"`
	RemoteAfterReconnect    *string  `json:"remoteAfterReconnect"`
	WritesAfterRemoteChange []string `json:"writesAfterRemoteChange"`
	WritesAfterCopy         []string `json:"writesAfterCopy"`
	Failed                  []string `json:"failed"`
	Connecting              []int    `json:"connecting"`
}

// The page's half of the stream, in a real browser: what it draws, what it
// does with the desktop's clipboard, and when it gives up.
//
// voxl's worker and client are stubbed (they are built into the image, not
// served by this repository), so this is stream.js's own decisions under test,
// against the messages voxl's worker sends.
func TestTheStreamPresentsFramesAndKeepsTheClipboardToThePerson(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real browser")
	}
	browser := findBrowser(t)

	voxlDir := stubVoxl(t)
	server, err := New(slog.New(slog.DiscardHandler), Config{
		FeedbackDir: t.TempDir(),
		Display:     ":91",
		ScaleEnvDir: filepath.Join(t.TempDir(), "scale-env"),
		VoxlDir:     voxlDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var mu sync.Mutex
	mode := "ok"
	reports := make(chan streamHarnessReport, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__stream/{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, streamHarnessPage)
	})
	mux.HandleFunc("GET /__stream/mode", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = io.WriteString(w, mode)
	})
	mux.HandleFunc("POST /__stream/mode", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		mode = string(body)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__stream/report", func(w http.ResponseWriter, r *http.Request) {
		var report streamHarnessReport
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

	origin := httptest.NewServer(browser.serve(mux))
	defer origin.Close()
	browser.open(t, origin.URL+"/__stream/")

	report := await(t, browser, reports, "the stream harness never reported")
	t.Logf("the stream harness reported %+v", report)
	if report.Error != "" {
		t.Fatalf("the harness failed: %s\n%s", report.Error, browser.report())
	}

	// The decoded frame is what the page drew, at the desktop's size, and what
	// an annotation is cut from.
	if report.Width != 320 || report.Height != 200 {
		t.Errorf("snapshot = %dx%d, want 320x200", report.Width, report.Height)
	}
	if len(report.Pixel) != 4 || report.Pixel[0] > 8 || report.Pixel[1] < 247 || report.Pixel[2] > 8 {
		t.Errorf("snapshot pixel = %v, want the frame's green", report.Pixel)
	}

	if report.Sent != "sent" {
		t.Errorf("sendClipboard = %q, want sent", report.Sent)
	}
	if report.SentSilently != "sent" || report.RemoteAfterSilentSend == nil || *report.RemoteAfterSilentSend != "sent without an echo" {
		t.Errorf("after sending with no echo: sent %q, remoteClipboard %v; want the sent text", report.SentSilently, report.RemoteAfterSilentSend)
	}
	if report.TooLarge != "too-large" {
		t.Errorf("sendClipboard of more than the bound = %q, want too-large", report.TooLarge)
	}
	// The desktop's clipboard changing is held, never written here by itself.
	if report.RemoteBeforeCopy == nil || *report.RemoteBeforeCopy != "to the desktop" {
		t.Errorf("remoteClipboard = %v, want the desktop's text", report.RemoteBeforeCopy)
	}
	if len(report.WritesAfterRemoteChange) != 0 {
		t.Errorf("the local clipboard was written when the desktop's changed: %q", report.WritesAfterRemoteChange)
	}
	// A copy made on the desktop through the page is.
	if len(report.WritesAfterCopy) != 1 || report.WritesAfterCopy[0] != "copied on the desktop" {
		t.Errorf("local clipboard after a copy on the desktop = %q, want the copied text", report.WritesAfterCopy)
	}
	// And what the desktop held goes with the connection.
	if report.RemoteAfterReconnect != nil {
		t.Errorf("remoteClipboard after reconnecting = %q, want none", *report.RemoteAfterReconnect)
	}

	// A stream that will not open is given up on, once, after a bounded number
	// of attempts; voxl's worker alone would retry forever.
	if len(report.Failed) != 1 || report.Failed[0] != "unreachable" {
		t.Errorf("failed = %q, want one unreachable", report.Failed)
	}
	last := report.Connecting[len(report.Connecting)-1]
	if last > streamMaxFailedConnects {
		t.Errorf("connected %d times before giving up, want at most %d", last, streamMaxFailedConnects)
	}
}

// streamMaxFailedConnects is web/stream.js's MAX_FAILED_CONNECTS.
const streamMaxFailedConnects = 5

// stubVoxl is a voxl directory with the stub client and worker as its web/.
func stubVoxl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	web := filepath.Join(dir, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"client.js": stubVoxlClient, "worker.js": stubVoxlWorker} {
		if err := os.WriteFile(filepath.Join(web, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
