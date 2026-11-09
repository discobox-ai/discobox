# console/bridge review notes

- **A box is known by its key, server and ID together, and everything about
  it goes through that server.** Two servers can list the same ID: resolve a
  key with `Server.box`/`source` and call `ForServer(box.Server)`, never the
  root source with an ID and never a lookup by ID alone. This mistake was made
  in the console's own data source, and twice in the prototype bridge.
- **The bridge runs the core's rules; it never writes its own.** A command
  goes through `console.Dispatch`; a question, a form, a title or a refusal is
  the core's (`Confirmation`, `UpgradeOffer`, `RenameForm`, `Sandbox.Title`,
  an entry's `Why`). A rule the bridge needs and the core lacks is moved into
  the core, as the TUI would need it too.
- **Nothing on the wire is null.** A list field is made with `nonNil` or
  `make(..., 0, n)`; the prototype's app stopped at its handshake on a nil
  slice encoded as `null`. The schema has no null, and the tests hold every
  message to it.
- **A wire change is regenerated, never mirrored.** Change `wire.go` (or the
  console's `Spec`), run `go tool task generate`, and build the app against
  the new `desktop/src/wire.rs`. A type the app reads written by hand in
  `desktop/` is the drift the generated types exist to prevent. A new named
  string type on the wire declares its values as constants: they are its
  enum.
- **A closed set on the wire is a named type, never a plain string.** A field
  whose values the bridge or the core decides (a box's state, a tab's kind, a
  request's kind, a command's target) is a named string type with its
  constants, so the app matches a generated enum. As a plain `string` the app
  had to compare it with literals it spelled itself, and a value renamed in Go
  regenerated cleanly while the app quietly stopped matching. A set that is
  open on purpose, such as an agent's reported state that the core draws even
  when it does not know it, stays a string, and the app reads it only through
  the spec.
- **A waiting command has a way out.** Every wait on the app — a question, a
  form — selects on the command's context, which ends when the app goes.
- **The registry is held only while it rules.** `Dispatch` runs under
  `regMu`; anything that waits on the app or the server runs in the
  `outcome`, after it is let go. That includes what a rule reads through
  the `Context` (`Waiting`, `Refused`): it is read before the lock (`inbox`),
  never lazily from inside a rule, where one slow server once held up every
  command, a pane switch included.
- **The pipe's reader never blocks on a command, and never drops what the
  app sent one.** A form's submissions are queued in order (`mailbox`); a
  one-slot channel with a dropping send lost the submit that followed a
  draft, and the command waited until the app quit.
- **Undo the view only once the box has acted.** `end` ends the session in
  the box first and closes its stream after: closing first left a pane with
  no stream and a session still running when the end failed.
