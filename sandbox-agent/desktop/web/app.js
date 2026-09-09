// The desktop viewer.
//
// Three things happen here, and only the first is noVNC's:
//
//  1. Connect to the VNC websocket this page's own origin proxies through to
//     websockify, and draw the framebuffer into the frame.
//  2. Keep the remote screen the shape of the frame. This page sends the frame
//     it has, in CSS pixels, and sizes that frame from the framebuffer it gets
//     back; it does no sizing arithmetic of its own. The scale that turns one
//     into the other is a separate, sticky property on /api/scale, reported
//     once from devicePixelRatio and then left alone — everything that reads a
//     scale reads it when a program launches, so a scale that moved with the
//     window would leave every program drawing at whatever it started at.
//  3. Let a person draw a box on the desktop, say what is wrong inside it, and
//     leave that where an agent working in this sandbox will find it.

import RFB from '/novnc/core/rfb.js';
import { crop, screenWithRegion } from '/capture.js';

const $ = (id) => document.getElementById(id);

const dom = {
  stage: $('stage'),
  screenWrap: $('screen-wrap'),
  screen: $('screen'),
  overlay: $('overlay'),
  marquee: $('marquee'),
  curtain: $('curtain'),
  curtainText: $('curtain-text'),
  status: $('status'),
  statusMain: $('status-main'),
  statusText: $('status-text'),
  chipGeometry: $('chip-geometry'),
  chipDensity: $('chip-density'),
  chipName: $('chip-name'),
  btnAnnotate: $('btn-annotate'),
  btnNotes: $('btn-notes'),
  btnFullscreen: $('btn-fullscreen'),
  btnReconnect: $('btn-reconnect'),
  btnCopy: $('btn-copy'),
  scale: $('scale'),
  annotateHint: $('annotate-hint'),
  composer: $('composer'),
  composerText: $('composer-text'),
  composerRegion: $('composer-region'),
  composerCancel: $('composer-cancel'),
  notes: $('notes'),
  notesClose: $('notes-close'),
  notesList: $('notes-list'),
  notesEmpty: $('notes-empty'),
  notesCount: $('notes-count'),
  handoffPrompt: $('handoff-prompt'),
  handoffPath: $('handoff-path'),
  toast: $('toast'),
  noteDialog: $('note-dialog'),
  noteDialogShot: $('note-dialog-shot'),
  noteDialogNoShot: $('note-dialog-noshot'),
  noteDialogSwitch: $('note-dialog-switch'),
  noteDialogTabScreen: $('note-dialog-tab-screen'),
  noteDialogTabRegion: $('note-dialog-tab-region'),
  noteDialogID: $('note-dialog-id'),
  noteDialogWhen: $('note-dialog-when'),
  noteDialogRegion: $('note-dialog-region'),
  noteDialogText: $('note-dialog-text'),
  noteDialogReplies: $('note-dialog-replies'),
  noteDialogDone: $('note-dialog-done'),
  noteDialogDelete: $('note-dialog-delete'),
  noteDialogSave: $('note-dialog-save'),
};

const state = {
  limits: null,
  geometry: null, // what the display was last set to
  rfb: null,
  connected: false,
  retries: 0,
  annotating: false,
  drawing: null,
  pending: null,
  items: [],
  // open is the note the dialog is showing, null when it is closed.
  open: null,
};

const RESIZE_SETTLE_MS = 220;
const MAX_RETRIES = 6;

// ------------------------------------------------------------------ startup

boot();

async function boot() {
  wireControls();
  try {
    const session = await getJSON('/api/session');
    state.limits = session.limits;
    dom.handoffPrompt.textContent = session.prompt;
    dom.handoffPath.textContent = session.feedbackPath;
  } catch (err) {
    setStatus('error', 'Could not reach the desktop service');
    dom.curtainText.textContent = String(err.message || err);
    return;
  }

  // Scale first, then size: the framebuffer is the frame times the scale, so
  // asking for a size before the display knows what screen it is being watched
  // on would size it once and immediately again.
  await reportScale();
  await resizeDisplay({ force: true });
  connect();
  refreshNotes();

  const observer = new ResizeObserver(() => scheduleResize());
  observer.observe(dom.stage);
  window.addEventListener('resize', scheduleResize);
}

function wireControls() {
  dom.btnAnnotate.addEventListener('click', () => setAnnotating(!state.annotating));
  dom.btnNotes.addEventListener('click', () => toggleNotes(dom.notes.hidden));
  dom.notesClose.addEventListener('click', () => toggleNotes(false));
  const reconnect = () => {
    state.retries = 0;
    connect();
  };
  dom.btnReconnect.addEventListener('click', reconnect);
  dom.statusMain.addEventListener('click', reconnect);
  dom.btnFullscreen.addEventListener('click', toggleFullscreen);
  dom.scale.addEventListener('change', chooseScale);
  dom.btnCopy.addEventListener('click', copyPrompt);
  dom.noteDialogTabScreen.addEventListener('click', () => {
    if (state.open) showCapture(state.open, 'screen');
  });
  dom.noteDialogTabRegion.addEventListener('click', () => {
    if (state.open) showCapture(state.open, 'region');
  });
  dom.noteDialogDone.addEventListener('click', () => {
    if (state.open) patchNote(state.open, { done: !state.open.done });
  });
  dom.noteDialogDelete.addEventListener('click', () => {
    if (state.open) deleteNote(state.open);
  });
  dom.noteDialogSave.addEventListener('click', () => {
    if (!state.open) return;
    const comment = dom.noteDialogText.value.trim();
    if (!comment || comment === state.open.comment) {
      dom.noteDialog.close();
      return;
    }
    patchNote(state.open, { comment });
  });

  dom.composer.addEventListener('submit', saveNote);
  dom.composerCancel.addEventListener('click', cancelComposer);

  dom.overlay.addEventListener('pointerdown', onPointerDown);
  dom.overlay.addEventListener('pointermove', onPointerMove);
  dom.overlay.addEventListener('pointerup', onPointerUp);
  dom.overlay.addEventListener('pointercancel', cancelDraw);

  document.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape') return;
    if (!dom.composer.hidden) {
      cancelComposer();
    } else if (state.annotating) {
      setAnnotating(false);
    }
  });
}

// ------------------------------------------------------------------- sizing

// availableCSS is the box the desktop gets, which is now the whole stage:
// clientWidth/Height already exclude the border, and there is no padding and no
// frame left to subtract.
function availableCSS() {
  return {
    w: Math.max(120, dom.stage.clientWidth),
    h: Math.max(120, dom.stage.clientHeight),
  };
}

let resizeTimer = null;

function scheduleResize() {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => resizeDisplay({ force: false }), RESIZE_SETTLE_MS);
}

let lastRequested = null;

async function resizeDisplay({ force }) {
  const avail = availableCSS();
  if (!force && lastRequested
      && lastRequested.w === avail.w && lastRequested.h === avail.h) {
    return;
  }
  lastRequested = avail;
  try {
    const geometry = await postJSON('/api/display', { cssWidth: avail.w, cssHeight: avail.h });
    applyGeometry(geometry);
  } catch (err) {
    // Forget the request, or this size is on record as asked for and the
    // debounce above suppresses every later attempt at it. One failure --- a
    // cold X server taking longer than the display's command timeout, say ---
    // would otherwise leave the desktop the wrong shape for the rest of the
    // session, with nothing to retry it but the user dragging the window to
    // some other size.
    lastRequested = null;
    setStatus('error', 'Could not resize the desktop');
    toast(String(err.message || err), 'error');
  }
}

// applyGeometry sizes the frame to the framebuffer that just arrived, divided
// by the scale it was rendered at. That division is why there is never a
// letterbox, and why a 2x desktop lands on physical pixels 1:1.
function applyGeometry(geometry) {
  state.geometry = geometry;
  dom.screenWrap.style.width = `${Math.round(geometry.width / geometry.scale)}px`;
  dom.screenWrap.style.height = `${Math.round(geometry.height / geometry.scale)}px`;
  showGeometry(geometry);
  rescaleViewport();
}

// reportScale tells the display what kind of screen it is being watched on.
// Sent once, on connect, as a suggestion rather than an instruction: the
// display ignores it if somebody has chosen a scale, so opening the desktop in
// a second browser on an ordinary screen does not undo a HiDPI session.
async function reportScale() {
  try {
    applyGeometry(await postJSON('/api/scale', {
      scale: window.devicePixelRatio || 1,
      auto: true,
    }));
  } catch (err) {
    console.warn('could not report the screen scale', err);
  }
}

async function chooseScale() {
  const scale = parseInt(dom.scale.value, 10);
  try {
    const geometry = await postJSON('/api/scale', { scale, auto: false });
    // The framebuffer has to be re-asked for at the new scale; the display
    // holds the scale but the frame is still the size it was.
    await resizeDisplay({ force: true });
    // Only said when it happened. A running session has to restart to pick a
    // new scale up — GDK_SCALE is read once per process, so the panel and the
    // window manager cannot be told any other way, and open windows do not
    // survive it — but re-selecting the density already in effect changes
    // nothing and restarts nothing, and neither does a change made before the
    // session has started. The server says which of those this was.
    const density = geometry.scale > 1 ? `HiDPI ${geometry.scale}×` : 'Standard density';
    toast(geometry.restarted
      ? `${density} — restarting the desktop to apply it`
      : `${density} — programs started from now on use it`);
  } catch (err) {
    // Put the control back to the scale the desktop is actually at. Leaving it
    // showing the value that failed means picking that value again fires no
    // change event, so the obvious way to retry does nothing at all.
    if (state.geometry) dom.scale.value = String(state.geometry.scale);
    toast(String(err.message || err), 'error');
  }
}

function showGeometry(geometry) {
  if (geometry.width) {
    dom.chipGeometry.textContent = `${geometry.width}×${geometry.height}`;
    dom.chipGeometry.hidden = false;
  }
  dom.chipDensity.textContent = geometry.scale > 1
    ? `${geometry.scale}× HiDPI · ${geometry.dpi} dpi`
    : `${geometry.dpi} dpi`;
  dom.chipDensity.hidden = false;
  dom.scale.value = String(geometry.scale);
}

// noVNC recomputes its scale when the container changes, but the container was
// resized in the same frame as the request that changed the framebuffer, so
// nudge it once the new size is known.
function rescaleViewport() {
  if (!state.rfb) return;
  state.rfb.scaleViewport = false;
  state.rfb.scaleViewport = true;
}

// --------------------------------------------------------------- connection

function connect() {
  if (state.rfb) {
    try { state.rfb.disconnect(); } catch { /* already gone */ }
    state.rfb = null;
  }
  dom.curtain.hidden = false;
  dom.curtain.classList.remove('fading');
  dom.curtainText.textContent = 'Waking the desktop…';
  setStatus('connecting', 'Connecting to the display…');

  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const rfb = new RFB(dom.screen, `${scheme}//${location.host}/websockify`, { shared: true });
  rfb.scaleViewport = true;
  rfb.clipViewport = false;
  // The size is this page's decision, made through /api/display; letting noVNC
  // negotiate one too would give the display two owners that disagree.
  rfb.resizeSession = false;
  rfb.background = '#000';
  rfb.showDotCursor = true;

  // Every handler below checks that it is still the current connection first.
  // disconnect() does not detach listeners and noVNC dispatches 'disconnect'
  // asynchronously from the socket close, so the *previous* connection's
  // handler runs after this one is already in state.rfb -- painting
  // "Disconnected" over a desktop that is connecting or connected. A
  // user-initiated disconnect carries clean === true, which falls past the
  // retry branch straight to the error state, so pressing Reconnect on a
  // healthy session was the way to see it.
  const current = () => state.rfb === rfb;

  rfb.addEventListener('connect', () => {
    if (!current()) return;
    state.connected = true;
    state.retries = 0;
    setStatus('connected', 'Connected');
    dom.curtain.classList.add('fading');
    setTimeout(() => { dom.curtain.hidden = true; }, 280);
    rescaleViewport();
  });

  rfb.addEventListener('disconnect', (event) => {
    if (!current()) return;
    state.connected = false;
    dom.curtain.hidden = false;
    dom.curtain.classList.remove('fading');
    if (event.detail && event.detail.clean === false && state.retries < MAX_RETRIES) {
      state.retries += 1;
      const wait = Math.min(400 * state.retries, 2500);
      setStatus('connecting', `Reconnecting… (${state.retries}/${MAX_RETRIES})`);
      dom.curtainText.textContent = 'Reconnecting to the desktop…';
      setTimeout(connect, wait);
      return;
    }
    setStatus('error', 'Disconnected');
    dom.curtainText.textContent = 'The desktop connection closed. Click the status at the top to reconnect.';
  });

  rfb.addEventListener('desktopname', (event) => {
    if (!current()) return;
    const name = event.detail.name;
    dom.chipName.textContent = name || '';
    dom.chipName.hidden = !name;
  });

  state.rfb = rfb;
}

// setStatus paints the state, and makes it the way back when the way back is
// what a person wants.
//
// A dead connection is the one moment the status has something to offer, and
// the reconnect button in the far corner is not where anyone is looking when
// the desktop has just gone: they are looking at the thing that says it is
// gone. So that becomes the button — and only then, because a control that is
// live but inert the rest of the time teaches people not to press it.
function setStatus(kind, text) {
  dom.status.dataset.state = kind;
  const offersRetry = kind === 'error';
  dom.statusText.textContent = offersRetry ? `${text} — click to reconnect` : text;
  dom.statusMain.disabled = !offersRetry;
  dom.statusMain.title = offersRetry ? 'Reconnect to the desktop' : '';
}

// -------------------------------------------------------------- annotating

function setAnnotating(on) {
  state.annotating = on;
  dom.btnAnnotate.setAttribute('aria-pressed', String(on));
  dom.overlay.classList.toggle('armed', on);
  dom.annotateHint.hidden = !on;
  if (!on) cancelDraw();
}

// canvasBox is where the framebuffer actually sits inside the overlay. noVNC
// centers the canvas when the aspect ratios differ, so this is read from the
// canvas rather than assumed to be the whole overlay.
function canvasBox() {
  const canvas = dom.screen.querySelector('canvas');
  if (!canvas || !canvas.width || !canvas.height) return null;
  const rect = canvas.getBoundingClientRect();
  const origin = dom.overlay.getBoundingClientRect();
  if (!rect.width || !rect.height) return null;
  return {
    canvas,
    left: rect.left - origin.left,
    top: rect.top - origin.top,
    width: rect.width,
    height: rect.height,
    scaleX: canvas.width / rect.width,
    scaleY: canvas.height / rect.height,
  };
}

function onPointerDown(event) {
  if (!state.annotating || event.button !== 0) return;
  const box = canvasBox();
  if (!box) return;
  dom.overlay.setPointerCapture(event.pointerId);
  const origin = dom.overlay.getBoundingClientRect();
  state.drawing = {
    x0: event.clientX - origin.left,
    y0: event.clientY - origin.top,
    x1: event.clientX - origin.left,
    y1: event.clientY - origin.top,
  };
  drawMarquee();
  event.preventDefault();
}

function onPointerMove(event) {
  if (!state.drawing) return;
  const origin = dom.overlay.getBoundingClientRect();
  state.drawing.x1 = event.clientX - origin.left;
  state.drawing.y1 = event.clientY - origin.top;
  drawMarquee();
}

function onPointerUp(event) {
  if (!state.drawing) return;
  onPointerMove(event);
  const rect = marqueeRect();
  const box = canvasBox();
  state.drawing = null;
  if (!box || rect.w < 8 || rect.h < 8) {
    cancelDraw();
    return;
  }
  // Framebuffer pixels: the marked region is the drag intersected with the
  // desktop, so a drag that began or ended in the letterbox beside it still
  // marks a region that exists on it.
  //
  // Both edges are clamped, not just the origin. Clamping x and then taking the
  // full drawn width would keep the part of the drag that was never over the
  // desktop and slide it inward, marking a region the person did not draw.
  // Each far edge is held one pixel past its near one so the region is never
  // empty and never a zero-sized canvas to crop from.
  const left = clamp(Math.round((rect.x - box.left) * box.scaleX), 0, box.canvas.width - 1);
  const top = clamp(Math.round((rect.y - box.top) * box.scaleY), 0, box.canvas.height - 1);
  const right = clamp(Math.round((rect.x + rect.w - box.left) * box.scaleX), left + 1, box.canvas.width);
  const bottom = clamp(Math.round((rect.y + rect.h - box.top) * box.scaleY), top + 1, box.canvas.height);
  const region = {
    x: left,
    y: top,
    w: right - left,
    h: bottom - top,
    frameWidth: box.canvas.width,
    frameHeight: box.canvas.height,
  };

  state.pending = {
    region,
    screenshot: crop(box.canvas, region),
    screen: screenWithRegion(box.canvas, region),
  };
  openComposer(rect);
}

function drawMarquee() {
  const rect = marqueeRect();
  Object.assign(dom.marquee.style, {
    left: `${rect.x}px`,
    top: `${rect.y}px`,
    width: `${rect.w}px`,
    height: `${rect.h}px`,
  });
  dom.marquee.hidden = false;
}

function marqueeRect() {
  const d = state.drawing || { x0: 0, y0: 0, x1: 0, y1: 0 };
  return {
    x: Math.min(d.x0, d.x1),
    y: Math.min(d.y0, d.y1),
    w: Math.abs(d.x1 - d.x0),
    h: Math.abs(d.y1 - d.y0),
  };
}

function cancelDraw() {
  state.drawing = null;
  dom.marquee.hidden = true;
}


// --------------------------------------------------------------- composer

function openComposer(rect) {
  const region = state.pending.region;
  dom.composerRegion.textContent = `${region.w}×${region.h} at ${region.x},${region.y}`;
  dom.composerText.value = '';
  dom.composer.hidden = false;

  // Anchor below the box, flipping above it when there is no room, and held
  // inside the window on both axes.
  const origin = dom.overlay.getBoundingClientRect();
  const width = dom.composer.offsetWidth;
  const height = dom.composer.offsetHeight;
  const left = clamp(origin.left + rect.x, 10, window.innerWidth - width - 10);
  const below = origin.top + rect.y + rect.h + 10;
  const top = below + height > window.innerHeight - 10
    ? Math.max(10, origin.top + rect.y - height - 10)
    : below;
  dom.composer.style.left = `${left}px`;
  dom.composer.style.top = `${top}px`;
  dom.composerText.focus();
}

function cancelComposer() {
  dom.composer.hidden = true;
  state.pending = null;
  cancelDraw();
}

async function saveNote(event) {
  event.preventDefault();
  if (!state.pending) return;
  const comment = dom.composerText.value.trim();
  if (!comment) {
    dom.composerText.focus();
    return;
  }
  const payload = {
    comment,
    region: state.pending.region,
    screenshot: state.pending.screenshot,
    screen: state.pending.screen,
  };
  dom.composer.hidden = true;
  try {
    const item = await postJSON('/api/feedback', payload);
    state.pending = null;
    cancelDraw();
    setAnnotating(false);
    await refreshNotes();
    toast(`Saved DF-${item.number} — open Notes to copy the prompt`);
  } catch (err) {
    toast(String(err.message || err), 'error');
    dom.composer.hidden = false;
  }
}

// ------------------------------------------------------------------- notes

async function refreshNotes() {
  try {
    adoptNotes(await getJSON('/api/feedback'));
  } catch (err) {
    console.warn('could not read notes', err);
  }
}

function renderNotes() {
  dom.notesList.replaceChildren();
  dom.notesEmpty.hidden = state.items.length > 0;
  for (const item of state.items) {
    dom.notesList.append(noteRow(item));
  }
}

// noteRow is one saved note, with the three things that can be done to it.
//
// Ticking is here, on the person's side, and deliberately not something the
// agent does: an agent that closes its own note has kept a checklist rather
// than had a review. The agent makes the change; saying it is right is this
// button.
function noteRow(item) {
  const li = document.createElement('li');
  li.className = item.done ? 'note done' : 'note';
  li.title = 'Open this note';

  if (item.shot) {
    const img = document.createElement('img');
    img.className = 'note-shot';
    img.loading = 'lazy';
    img.alt = '';
    img.src = shotURL(item.shot);
    li.append(img);
  }

  const body = document.createElement('div');
  body.className = 'note-body';

  const head = document.createElement('div');
  head.className = 'note-head';
  const id = document.createElement('span');
  id.className = 'note-id';
  id.textContent = item.done ? `✓ DF-${item.number}` : `DF-${item.number}`;
  const when = document.createElement('span');
  when.className = 'note-when';
  when.textContent = relativeTime(item.createdAt);
  head.append(id, when);

  const text = document.createElement('p');
  text.className = 'note-text';
  text.textContent = item.comment;

  const region = document.createElement('div');
  region.className = 'note-region';
  region.textContent = `${item.region.w}×${item.region.h} at ${item.region.x},${item.region.y}`;
  if (item.replies && item.replies.length) {
    region.textContent += ` · ${item.replies.length} ${item.replies.length === 1 ? 'reply' : 'replies'}`;
  }

  body.append(head, text, region);
  li.append(body);
  li.addEventListener('click', () => openNote(item));
  return li;
}

async function patchNote(item, change) {
  try {
    adoptNotes(await requestJSON('PATCH', `/api/feedback/${item.id}`, change));
    // The dialog stays open on an edit and follows what was saved, so ticking
    // and rewording read as changes to the thing being looked at.
    const updated = state.items.find((note) => note.id === item.id);
    if (state.open && state.open.id === item.id && updated) {
      state.open = updated;
      dom.noteDialogText.value = updated.comment;
      showDoneButton(updated.done);
    }
  } catch (err) {
    toast(String(err.message || err), 'error');
  }
}

async function deleteNote(item) {
  try {
    adoptNotes(await requestJSON('DELETE', `/api/feedback/${item.id}`));
    if (state.open && state.open.id === item.id) {
      state.open = null;
      dom.noteDialog.close();
    }
    toast(`Deleted DF-${item.number}`);
  } catch (err) {
    toast(String(err.message || err), 'error');
  }
}

// adoptNotes takes the listing the write returned, so the panel never shows a
// note the file no longer has — the edit endpoints answer with the whole list
// for exactly this reason.
function adoptNotes(data) {
  state.items = data.items || [];
  dom.handoffPrompt.textContent = data.prompt;
  dom.handoffPath.textContent = data.path;
  const open = state.items.filter((note) => !note.done).length;
  dom.notesCount.textContent = String(open);
  dom.notesCount.dataset.open = String(open > 0);
  renderNotes();
}

function toggleNotes(open) {
  dom.notes.hidden = !open;
  dom.btnNotes.setAttribute('aria-pressed', String(open));
  if (open) refreshNotes();
}

async function copyPrompt() {
  const text = dom.handoffPrompt.textContent;
  try {
    await navigator.clipboard.writeText(text);
    toast('Prompt copied');
  } catch {
    // Clipboard access can be refused; select it so the copy is one keystroke.
    const range = document.createRange();
    range.selectNodeContents(dom.handoffPrompt);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    toast('Press ⌘/Ctrl+C to copy the selected prompt', 'error');
  }
}

// --------------------------------------------------------------- note dialog

// openNote shows one note as what it is: a picture of the screen at the moment
// it was taken, with the words about it and everything that can be done to it.
//
// It is a dialog rather than a box drawn back onto the desktop. The desktop has
// moved on — a window has been dragged, a page has scrolled, the resolution has
// changed — so a rectangle in those coordinates points at whatever happens to
// be there now, which is usually not what the note is about. The screenshot is
// the record; the live desktop is not.
function openNote(item) {
  state.open = item;

  // The whole desktop first, when there is one: opening a note weeks later,
  // "where was this" is the thing you have lost, and the box on the full screen
  // answers it before the crop can.
  showCapture(item, item.screen ? 'screen' : 'region');

  dom.noteDialogID.textContent = `DF-${item.number}`;
  dom.noteDialogWhen.textContent = relativeTime(item.createdAt);
  // The framebuffer it was captured against is named, because that is what
  // makes the coordinates mean anything — and what makes it clear they are not
  // coordinates on the desktop as it stands.
  dom.noteDialogRegion.textContent =
    `${item.region.w}×${item.region.h} at ${item.region.x},${item.region.y}` +
    (item.region.frameWidth ? ` · captured on a ${item.region.frameWidth}×${item.region.frameHeight} desktop` : '');
  dom.noteDialogText.value = item.comment;
  renderReplies(item.replies || []);
  showDoneButton(item.done);

  dom.noteDialog.showModal();
}

// renderReplies shows what has been said back. The agent writes these into the
// Markdown itself — that file is its whole interface here — so this only has to
// display them.
function renderReplies(replies) {
  dom.noteDialogReplies.replaceChildren();
  dom.noteDialogReplies.hidden = replies.length === 0;
  for (const reply of replies) {
    const item = document.createElement('li');
    item.className = 'note-reply';

    const head = document.createElement('div');
    head.className = 'note-reply-head';
    const who = document.createElement('span');
    who.className = 'note-reply-who';
    who.textContent = reply.author || 'reply';
    head.append(who);
    if (reply.at) {
      const when = document.createElement('span');
      when.className = 'note-reply-when';
      when.textContent = relativeTime(reply.at);
      head.append(when);
    }

    const text = document.createElement('p');
    text.className = 'note-reply-text';
    text.textContent = reply.text;

    item.append(head, text);
    dom.noteDialogReplies.append(item);
  }
}

// shotURL turns the path a note recorded into the URL that serves it.
//
// Read from the record rather than rebuilt from the id: the store parses these
// out of the Markdown on every read and resolves whatever path it finds there,
// so a picture that was renamed, moved or hand-edited into the file still
// resolves. Rebuilding the name from the id would hardcode one writer's
// convention and quietly 404 on anything else. /shots/ serves them by name.
function shotURL(rel) {
  if (!rel) return '';
  const name = String(rel).split('/').pop();
  return `/shots/${encodeURIComponent(name)}`;
}

// showCapture switches between the two pictures of the same moment.
function showCapture(item, which) {
  const has = { screen: Boolean(item.screen), region: Boolean(item.shot) };
  if (!has.screen && !has.region) {
    dom.noteDialogShot.removeAttribute('src');
    dom.noteDialogShot.hidden = true;
    dom.noteDialogNoShot.hidden = false;
    dom.noteDialogSwitch.hidden = true;
    return;
  }
  if (!has[which]) which = has.screen ? 'screen' : 'region';

  dom.noteDialogShot.src = shotURL(which === 'screen' ? item.screen : item.shot);
  dom.noteDialogShot.hidden = false;
  dom.noteDialogNoShot.hidden = true;
  // The switch is only worth showing when there is something to switch to.
  dom.noteDialogSwitch.hidden = !(has.screen && has.region);
  dom.noteDialogTabScreen.setAttribute('aria-pressed', String(which === 'screen'));
  dom.noteDialogTabRegion.setAttribute('aria-pressed', String(which === 'region'));
}

function showDoneButton(done) {
  dom.noteDialogDone.textContent = done ? '✓ Done' : 'Mark done';
  dom.noteDialogDone.setAttribute('aria-pressed', String(done));
}

// ------------------------------------------------------------------- misc

function toggleFullscreen() {
  if (document.fullscreenElement) {
    document.exitFullscreen();
  } else {
    document.documentElement.requestFullscreen().catch(() => toast('Fullscreen was refused', 'error'));
  }
}

let toastTimer = null;

function toast(message, kind) {
  dom.toast.textContent = message;
  dom.toast.dataset.kind = kind || 'ok';
  dom.toast.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { dom.toast.hidden = true; }, 3600);
}

function relativeTime(iso) {
  const at = Date.parse(iso);
  if (Number.isNaN(at)) return '';
  const seconds = Math.max(0, Math.round((Date.now() - at) / 1000));
  if (seconds < 60) return 'just now';
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

function clamp(value, min, max) {
  return Math.min(Math.max(value, min), max);
}

async function getJSON(path) {
  return unwrap(await fetch(path, { headers: { Accept: 'application/json' } }));
}

async function postJSON(path, body) {
  return requestJSON('POST', path, body);
}

async function requestJSON(method, path, body) {
  const init = { method, headers: { Accept: 'application/json' } };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  return unwrap(await fetch(path, init));
}

async function unwrap(response) {
  let payload = null;
  try {
    payload = await response.json();
  } catch {
    payload = null;
  }
  if (!response.ok) {
    throw new Error((payload && payload.error) || `${response.status} ${response.statusText}`);
  }
  return payload;
}
