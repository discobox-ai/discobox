# ahphost — review notes

- **A call whose work outlives it runs under a context that outlives it.** An
  attach (`OpenExec`, `NewShell`, `NewTerminal`) lives exactly as long as the
  context it was opened with, and a `Boxes` listing may leave a slow server's
  request running past the call. A request's context, or a timeout cancelled
  when the call returns, ends that work with it — `discobox admin ahp`'s
  screens died at once that way, and its listing dropped every slow server.
  Open under the host's context and bound only the open (`openContext`). A
  test fake that ignores its context cannot catch this; make it end with the
  context.
- **What a client's disconnect cleans up is that client's own.** The host
  forgets drafts and exited shells when the client that made them goes
  (`dropConn`, checking `owner`). Sweeping everything nobody watches at that
  moment removed another, still-connected client's draft, and its first
  message then went nowhere. Anything new that a disconnect forgets carries
  its owner and is checked against it.
- **What only a host in the discobox can do keys on `Config.Workspace`**, and
  git and inotify stay behind that interface, in the sandbox agent
  (`ahplocal`): this package is in the root module and the CLI's build.
- **A file operation never starts a discobox.** `filePath` refuses one that is
  not running: VS Code lists and stats the folders of every session it shows,
  and the files API's read starts a stopped discobox (ADR 26-10-22-499 §3).
- **A host in a discobox creates no discoboxes.** `createSession` is refused
  there before a first message — the prompt — could be lost to a create that
  cannot succeed.
- **A session's folder is a path in the discobox, never the client's.**
  `Box.Workdir` is where the working tree is inside the discobox; `Box.Source`
  is what it was cut from on a client, which names and groups the session but
  is no path the discobox has. The folder once was the client's path, and a
  listing of it found nothing — or the wrong directory. A test that resolves
  files reaches them through the folder a session advertises.
- **A link lists as what it points at.** The files API reports a symlink as
  itself; `followLink` resolves it one level, so a linked folder is a folder
  in VS Code's picker and explorer.
