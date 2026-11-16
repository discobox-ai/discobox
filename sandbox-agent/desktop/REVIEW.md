# Desktop Review Notes

- **The viewer's origin belongs to the box; never leave the browser holding a
  permission for it.** The sandbox user has passwordless sudo and can serve
  any page on 6900 — stop the socket, replace the viewer — and a forward binds
  the same local port for whichever box asks next, so `localhost:6900` is
  whatever is behind it at the moment. A permission a browser keeps per origin
  (clipboard read, camera, notifications) granted to the desktop would be held
  by every page served there afterwards. So the page never calls
  `navigator.clipboard.readText()`: paste goes through the paste event, which
  needs no permission. A change that asks the browser for a lasting permission
  on this origin is the thing to catch.
- **Nothing the viewer serves under the sandbox user's control may be a page.**
  `/shots/` serves `.png` only, as `image/png` with `nosniff` and a sandboxing
  CSP, so a file dropped into `~/.discobox/desktop-feedback` is never script
  on the viewer's origin. A new route serving such files holds to the same.
