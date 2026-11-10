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
- **A service verb goes through `console.RunServiceVerb`**, which both
  consoles call: it reads the service again, refuses what its menu would
  (`Service.Actions`) and words the refusal. The bridge's first version looked
  the service and verb up and wrote its own refusals beside the TUI's bare
  `DoService` — the third rule after `Session.Saves` and `RequestsFor` copied
  into the bridge by hand.
- **Nothing on the wire is null.** A list field is made with `nonNil` or
  `make(..., 0, n)`; the prototype's app stopped at its handshake on a nil
  slice encoded as `null`. The schema has no null, and the tests hold every
  message to it.
- **A rule about when the window does something is the core's, even a small
  one.** Whether a filter is saved (`Session.Saves`), which requests a box is
  affected by (`RequestsFor`): the bridge's first copy of each was written by
  hand from the TUI's and already differed from it. Move the rule into
  `console` and call it from both.
- **A wire change is regenerated, never mirrored.** Change `wire.go` (or the
  console's `Spec`), run `go tool task generate`, and build the app against
  the new `desktop/bridge/src/wire.rs`. A type the app reads written by hand in
  `desktop/` is the drift the generated types exist to prevent. A new named
  string type on the wire declares its values as constants: they are its
  enum.
- **A field added to the spec bumps `SpecVersion`.** Every wire field is
  required in the generated Rust, so an app built for the new spec, run
  against a CLI from before it (`--binary`, `DISCOBOX_DESKTOP_BINARY`, a dev
  build beside an older CLI), passed the version check and stopped on a bare
  missing field instead of the check's "run the app built with this CLI".
  Adding `Dimensions` nearly shipped that way.
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
- **An ended subscription sends nothing.** Every snapshot goes out through
  `follower.send`, which checks the subscription's context under `subMu`,
  the lock `unsubscribe` holds to end it. A check in one follower's loop
  only narrowed the race: a relist wakes a follower as readily as its end,
  and the snapshot after an unsubscribe once went out after the next
  command's result.
- **The contract suite reaches the core through commands.** A case the app
  needs covered is a box in `TestContractCore` whose command does it, never
  a control channel beside the bridge: the app's client sends only what the
  app could.
- **The suite covers every message kind but `edit`** (289 §4), which
  nothing the core runs sends yet. The change that first makes the core send
  one adds its case, with `saved` answering it, to the contract suite.
- **A topic that waits for the next listing never lists, nor reads the
  servers.** The commands and the palette are sent again whenever a listing
  is read (`relisted`), so they rule from what was last read
  (`cachedContext`, `cached`, `lastListing`), never through `Server.box` or
  `inbox`. Their first version built its context the way a command does: a
  workspace box missing from the listing listed again, which woke them
  again, and the bridge listed every server without end.
- **A yes or no dismissed is answered "no", never declined.** The core says
  leaving a yes or no is no (`console.Question`); only a pick of one is
  declined when dismissed. The desktop's first sheet declined both on Esc,
  ending the same question differently from the TUI. The wire's `Answer`
  says so, so the next renderer of a question reads it there.
