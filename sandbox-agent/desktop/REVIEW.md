# Desktop Review Notes

- Read browser clipboard text through a paste event. The box owns the
  viewer's origin and another box can reuse the same local port.
- Write desktop text to the browser clipboard on an explicit action and keep
  the viewer out of frames. A page embedding the viewer could disguise the
  copy control.
- Serve screenshot bytes as PNG with MIME sniffing disabled and a sandbox
  CSP. Screenshot files are writable from inside the box.
