// The desktop's stream, through voxl (ADR 26-11-06-851).
//
// voxl is two halves that have to match: a server the viewer starts behind
// /api/stream, and a browser client. Both are built from one pinned source
// (sandbox-agent/Dockerfile), and the client arrives here as two files the
// desktop feature serves as /voxl/: the decode worker, which owns the
// WebSocket and decodes H.264 with WebCodecs, and client.js, which exports
// voxl's own keyboard, pointer, touch and cursor handling. This module is the
// rest of a client, and it is ours rather than voxl's because each part of it
// decides something the desktop decides:
//
//   - **Presentation.** Decoded frames are drawn onto a canvas on this page,
//     not into a <video> or a canvas handed to the worker, so the frame the
//     person is looking at can be copied whole when they annotate it
//     (snapshot), in every browser alike.
//   - **Size.** The desktop's geometry is /api/display's, set by the page;
//     voxl's server has its own resizing turned off, and this only follows
//     the size each hello announces.
//   - **Connection.** The worker reconnects on its own every second, forever.
//     Here that is bounded: after a few closes in a row with no desktop in
//     between, the worker is stopped and the page offers a reconnect.
//   - **Clipboard.** voxl's own client writes every remote copy into the
//     local clipboard and reads it on focus. This page does neither: remote
//     text is held here, the local clipboard is written only after a copy
//     made on the desktop through this page or from the clipboard panel, and
//     read only by a paste the person makes (§4).
//
// Nothing here starts X or the stream by itself: openStream does, and the page
// calls it only once it has asked the display for a size.
import { Bin, cmd, createCursor, setupKeyboard, setupPointer } from './voxl/client.js';

// The codec voxl's worker configures when a keyframe's own parameters cannot
// be read: H.264 High at level 5.2, which covers the largest desktop the
// display serves. A decoder that takes it takes what x264 sends.
const STREAM_CODEC = 'avc1.640034';

// MAX_CLIPBOARD_BYTES bounds text sent to, or taken from, the desktop's
// clipboard. A clipboard larger than this is not text a person is moving by
// hand, and the stream carries input in the same socket.
export const MAX_CLIPBOARD_BYTES = 1 << 20;

// MAX_FAILED_CONNECTS is how many closes in a row, with no desktop in between,
// end the attempt. The worker waits a second between them, and the viewer
// retries a stream that will not start itself, so this is a few seconds of
// trying before the page says so.
const MAX_FAILED_CONNECTS = 5;

// FIRST_FRAME_MS is how long a connected stream may go without one decoded
// frame before the page says the browser is not decoding it. voxl sends a
// keyframe to every viewer as it joins.
const FIRST_FRAME_MS = 15000;

// COPY_WINDOW_MS is how long after a copy shortcut typed into the desktop the
// desktop's new clipboard is taken as that copy's result.
const COPY_WINDOW_MS = 2000;

const encoder = new TextEncoder();

// supported says whether this browser can show the stream, and if not, why:
// 'insecure' (WebCodecs is offered only to HTTPS pages and localhost),
// 'webcodecs' (no decoder, worker or offscreen canvas at all), or 'h264' (a
// decoder, but not for this stream).
//
// The decoder is asked about the stream's own configuration rather than taken
// on its presence alone: a browser can offer VideoDecoder and still decode no
// H.264. What no question can settle — a decoder that accepts the
// configuration and then fails the frames — is caught by openStream, which
// reports it as 'decode' once it happens.
export async function supported() {
  if (!window.isSecureContext) return { ok: false, reason: 'insecure' };
  if (typeof Worker === 'undefined' || typeof OffscreenCanvas === 'undefined'
      || typeof VideoDecoder === 'undefined' || typeof EncodedVideoChunk === 'undefined') {
    return { ok: false, reason: 'webcodecs' };
  }
  try {
    const result = await VideoDecoder.isConfigSupported({
      codec: STREAM_CODEC,
      optimizeForLatency: true,
      hardwareAcceleration: 'no-preference',
    });
    if (!result.supported) return { ok: false, reason: 'h264' };
  } catch {
    return { ok: false, reason: 'h264' };
  }
  return { ok: true };
}

// openStream shows the desktop in surface and sends it what the person does
// there. kbd is a textarea kept out of sight that takes keyboard, IME and paste
// input; surface gets the pointer.
//
// on receives what happens:
//   connecting(attempt)   the worker is (re)connecting; attempt counts from 1
//   hello({width,height}) the desktop's size, on every (re)connect and resize
//   frame()               the first decoded frame of a connection
//   closed()              the stream closed; the worker is retrying
//   failed(reason)        given up: 'unreachable' or 'decode'
//   clipboard(text)       the desktop's clipboard changed (text, or null when
//                         it is gone with the connection or too large)
//   copied(text)          a copy made on the desktop through this page
//   notice(message)       something worth telling the person
//
// It returns the controls the page needs; see the object at the end.
export function openStream({ surface, kbd, on }) {
  const canvas = document.createElement('canvas');
  canvas.className = 'stream';
  surface.replaceChildren(canvas);
  const ctx = canvas.getContext('2d', { alpha: false });

  let worker = null;
  let failures = 0;
  let framed = false;
  let firstFrameTimer = null;
  let decodeErrors = 0;
  let remoteText = null;
  let copyArmedUntil = 0;

  // app is the shape voxl's own modules are written against: its main.ts
  // builds the same object around its own page.
  const app = {
    worker: null,
    post(message, transfer = []) {
      if (worker) worker.postMessage(message, transfer);
    },
    send(command) {
      // Before a desktop is there, only what keeps a stream alive is sent;
      // anything else would act on a desktop the person cannot see.
      if (!app.desk.w) return;
      if (command.startsWith('c,')) {
        const text = command.slice(2);
        if (encoder.encode(text).length > MAX_CLIPBOARD_BYTES) {
          // A paste sends the text and then the paste keystroke, which voxl's
          // keyboard sends whatever happens here. The desktop's clipboard is
          // emptied instead, so that keystroke pastes nothing rather than
          // whatever the desktop held before — in a terminal, an old command.
          on.notice('That text is too large to paste into the desktop.');
          app.post({ t: 'send', d: cmd.clipboard('') });
          tookClipboard('');
          return;
        }
        app.post({ t: 'send', d: command });
        tookClipboard(text);
        return;
      }
      app.post({ t: 'send', d: command });
    },
    // Binary messages from a viewer are images pasted into the clipboard and
    // the microphone, which this desktop does not take.
    sendBin(type) {
      if (type === Bin.ClipImageUp) on.notice('Only text can be pasted into the desktop.');
    },
    surface,
    kbd,
    tools: document.createElement('div'),
    isMac: /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent),
    remoteMac: false,
    readOnly: false,
    inIframe: false,
    desk: { w: 0, h: 0 },
    view: { scale: 1 },
  };

  const cursor = createCursor(app);
  // voxl's pinch zoom magnifies the surface with a transform, which would move
  // the desktop out from under the annotation overlay; this page has no zoom,
  // so a pinch does nothing and a two-finger drag scrolls.
  const zoom = {
    begin() {},
    pinch() {},
    pan(dx, dy) { return [dx, dy]; },
    fit() {},
  };
  setupPointer(app, zoom, () => {});
  // The keyboard's clipboard is told what was pasted locally, which voxl's own
  // client uses to avoid echoing it back. Here the echo is harmless, since
  // nothing is written locally without a copy, so it only remembers it.
  setupKeyboard(app, { noteLocal() {} }, () => {});

  // A copy shortcut typed into the desktop is the person copying something
  // there to take it here, so the desktop's clipboard changing just after is
  // written to the local one. Nothing else writes it.
  kbd.addEventListener('keydown', (event) => {
    const accel = app.isMac ? event.metaKey : event.ctrlKey;
    const key = event.key.toLowerCase();
    if ((accel && key === 'c') || (event.ctrlKey && key === 'insert')) {
      copyArmedUntil = performance.now() + COPY_WINDOW_MS;
    }
  }, true);

  // tookClipboard records text this page put on the desktop's clipboard as what
  // the desktop's clipboard now holds: voxl does not report back a clipboard it
  // set itself, so without this the page would go on showing the one before.
  function tookClipboard(text) {
    remoteText = text;
    on.clipboard(text);
  }

  function layout() {
    if (!app.desk.w) return;
    const width = surface.clientWidth;
    if (width) app.view.scale = width / app.desk.w;
    cursor.apply();
  }

  function present(parts) {
    for (const part of parts) {
      ctx.drawImage(part.frame, 0, part.y);
      part.frame.close();
    }
    if (!framed) {
      framed = true;
      clearTimeout(firstFrameTimer);
      on.frame();
    }
  }

  function disconnected() {
    app.desk.w = 0;
    framed = false;
    clearTimeout(firstFrameTimer);
    // Held keys and buttons are released, and what the desktop's clipboard
    // held is forgotten with the connection that brought it.
    if (remoteText !== null) {
      remoteText = null;
      on.clipboard(null);
    }
  }

  function fail(reason) {
    stop();
    on.failed(reason);
  }

  function onMessage(event) {
    const message = event.data;
    switch (message.t) {
      case 'frames':
        present(message.parts);
        break;
      case 'hello':
        failures = 0;
        decodeErrors = 0;
        app.desk.w = message.w;
        app.desk.h = message.h;
        if (canvas.width !== message.w || canvas.height !== message.h) {
          canvas.width = message.w;
          canvas.height = message.h;
        }
        layout();
        on.hello({ width: message.w, height: message.h });
        clearTimeout(firstFrameTimer);
        if (!framed) {
          firstFrameTimer = setTimeout(() => {
            if (!framed && !document.hidden) fail('decode');
          }, FIRST_FRAME_MS);
        }
        break;
      case 'status':
        if (message.s === 'open') {
          updateVisibility(true);
          break;
        }
        disconnected();
        failures += 1;
        if (failures >= MAX_FAILED_CONNECTS) {
          fail('unreachable');
          break;
        }
        on.closed();
        on.connecting(failures + 1);
        break;
      case 'cursor':
        cursor.set(message.buf);
        break;
      case 'clip': {
        const text = typeof message.d === 'string' ? message.d : '';
        if (encoder.encode(text).length > MAX_CLIPBOARD_BYTES) {
          remoteText = null;
          on.clipboard(null);
          on.notice('The desktop’s clipboard is too large to bring here.');
          break;
        }
        remoteText = text;
        on.clipboard(text);
        if (performance.now() < copyArmedUntil) {
          copyArmedUntil = 0;
          on.copied(text);
        }
        break;
      }
      case 'log':
        console.warn('[voxl]', message.s);
        // A decoder that accepted the configuration and then fails the frames
        // is a browser that cannot show this stream, however it reported
        // itself; the worker retries with a keyframe, so a few in a row with
        // nothing shown is the answer.
        if (!framed && /decoder error|configure failed|decode threw/.test(message.s)) {
          decodeErrors += 1;
          if (decodeErrors >= 3) fail('decode');
        }
        break;
      default:
        // stats, images on the clipboard, the microphone: not taken here.
        break;
    }
  }

  // Video is paused while nobody can see it, and held input released.
  let videoOn = null;
  function updateVisibility(force = false) {
    const visible = !document.hidden;
    if (visible !== videoOn || force) {
      videoOn = visible;
      app.post({ t: 'video', on: visible });
    }
  }
  function onVisibility() {
    updateVisibility();
    if (document.hidden) app.send(cmd.releaseAll);
  }
  document.addEventListener('visibilitychange', onVisibility);

  function start() {
    stop();
    failures = 0;
    decodeErrors = 0;
    const url = new URL('api/stream', document.baseURI);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    worker = new Worker(new URL('./voxl/worker.js', import.meta.url));
    app.worker = worker;
    worker.addEventListener('message', onMessage);
    // The worker paints into the canvas it is given unless it is told to hand
    // frames back; it is given one it will never show, and frames come here.
    const offscreen = new OffscreenCanvas(1, 1);
    worker.postMessage({ t: 'init', canvas: offscreen, url: url.href, presentToMain: true }, [offscreen]);
    on.connecting(1);
  }

  function stop() {
    if (worker) {
      worker.removeEventListener('message', onMessage);
      worker.terminate();
      worker = null;
      app.worker = null;
    }
    videoOn = null;
    disconnected();
  }

  start();

  return {
    // reconnect drops this connection and opens a new one.
    reconnect: start,
    // layout follows a change to the size the desktop is shown at.
    layout,
    // canvas is where the desktop is drawn, for placing things over it.
    canvas,
    // snapshot is a copy of the frame on screen now, at the desktop's size,
    // or null before the first one: the picture an annotation is cut from,
    // taken once so a frame arriving mid-capture cannot mix two moments.
    snapshot() {
      if (!framed || !app.desk.w) return null;
      const copy = document.createElement('canvas');
      copy.width = canvas.width;
      copy.height = canvas.height;
      copy.getContext('2d').drawImage(canvas, 0, 0);
      return copy;
    },
    // remoteClipboard is the desktop's clipboard text, if this connection has
    // seen any.
    remoteClipboard() {
      return remoteText;
    },
    // sendClipboard puts text on the desktop's clipboard, as a clipboard and
    // never as keystrokes. It says whether it could: 'sent', 'too-large' or
    // 'disconnected'.
    sendClipboard(text) {
      if (!app.desk.w) return 'disconnected';
      if (encoder.encode(text).length > MAX_CLIPBOARD_BYTES) return 'too-large';
      app.post({ t: 'send', d: cmd.clipboard(text) });
      tookClipboard(text);
      return 'sent';
    },
    // focus sends the keyboard to the desktop.
    focus() {
      kbd.focus({ preventScroll: true });
    },
    // releaseAll lets go of every key and button the desktop thinks is held.
    releaseAll() {
      app.send(cmd.releaseAll);
    },
  };
}
