// The parts of voxl's own browser client the desktop page drives voxl with:
// keyboard (keysyms, modifiers, IME, paste), mouse, wheel and touch, the remote
// cursor, and the wire protocol's commands. Built into /voxl/client.js from
// voxl's source at the revision sandbox-agent/Dockerfile pins, beside the
// decode worker (/voxl/worker.js) built from the same tree, so the page and
// the stream it opens speak one protocol (ADR 26-11-06-851 §5).
//
// It is copied into voxl's tree at web/src/ before esbuild runs, so these
// paths are voxl's. Only the input and cursor modules are taken: the page,
// presentation, resizing, clipboard and toolbar are the desktop page's own
// (sandbox-agent/desktop/web/stream.js), because voxl's would decide things
// Discobox decides — the desktop's size, and when the local clipboard is read
// or written.
export { Bin, cmd } from './protocol';
export { createCursor } from './client/cursor';
export { setupKeyboard } from './client/keyboard';
export { setupPointer } from './client/pointer';
