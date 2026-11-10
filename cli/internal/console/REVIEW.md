# console review notes

- **The core's text names a command, never a key** (`Sandbox.Remark`'s
  `Command`). Keys are each frontend's binding (ADR 26-11-07-598): a key
  spelled in core text is one the desktop does not bind, and one the TUI's
  registry entry can move away from. The renderer looks the command up and
  spells its own key.
- **A `List` copy only reads, and only its `Filter` changes.** A copy shares
  the listing's slice and maps; a setter that reuses a slice it was handed
  (`append(x[:0], …)`) writes into every copy's — an open filter card
  showing another refresh's servers. Clone what a setter keeps.
- **A workspace rule goes in `workspace.go`, never in a renderer or the
  bridge.** Which sessions open, their column, order, title and number, and
  what an ending does are answered there; a renderer or the bridge that
  re-derives one from `Exec` fields is the copy ADR 26-11-07-876 rejected.
- **What a workspace header says is `NewHeader`, never a renderer's.** Which
  ports link and where, the desktop's link, the work links and which tags are
  said: the TUI's header once held these rules alone, and the desktop app
  would have needed a second copy. A renderer orders, colors and drops the
  fields; it decides none of them.
- **`OneLine` is rune for rune what the TUI's field holds** (bubbles'
  `textinput` sanitizer: `\r`, `\n` and `\t` each a space, U+FFFD and other
  control characters dropped). A form that answers only what changed compares
  every typed answer with what its field opened holding (`opening` of the
  form's own fields, read the same way the answer is — `HostList`, the route
  parser), never with the stored value or a hand-built copy of it; any
  difference makes an untouched field an edit nobody made, and saving another
  setting renamed the box. A picker's key, and a lifetime read back from one,
  are made from the stored value and may be compared with it. This has
  regressed three times (a dropped control character, a folded line break, a
  field compared with its stored value): test a changed rule with CRLF and
  U+FFFD, not only a dropped control character.
  A renderer is a fourth path: the desktop's sheet first sent every field back
  as its own text widget held it. A renderer submits only the fields the person
  changed, and the bridge sends each field's `Value` as the core reads it
  (`Opening`), never the raw stored value.
- **A renderer reads answers through the core.** A form's meaning — what an
  option key stands for, which answers count — is a `Read…` function or the
  form's `Submit` here, never `f.chosen("…") == "…"` in a renderer.
- **A question a command asks before it runs is reached through `Dispatch`.**
  Purge's confirmation was once a renderer's own check and then a free
  function a renderer had to remember; either way the desktop bridge could
  send a purge unasked. It lives in the core (`Confirmation`) and `Dispatch`
  routes the verb to `Frontend.Confirm`; anything sending a verb outside the
  registry refuses one that asks.
- **A `Narration` send that waits has a way out.** `Accept`, `Ask` and
  `Handover` wait for the reader, so each selects on its context: a create
  whose frontend stopped reading must still return, and its deferred cleanup
  (delivery locks) still run, once it is cancelled.
- **Which requests and refused credentials a box is affected by is
  `RequestsFor` and `RejectionsFor`, never a match written by hand.** A
  harness's credential is refused for every box running that harness, and a
  request or refusal is on one server; the bridge's first copy matched by
  sandbox ID alone and refused `rejections` where the TUI opened it.
- **A run option's rule is `RunOptions`', never a renderer's.** Which rows a
  create offers, what each leads with, the sources, the request and the
  command were the TUI's panel alone, and the desktop would have needed a
  second copy. A renderer moves a cursor over the rows and draws them; the
  bridge holds the window's `RunOptions` and fills `CreateForm`/`OptionsForm`.
- **A form's answer names a choice by its value, never its place.** The
  run options' answer was first read back into positions and applied to rows
  the bridge holds, which another command can rebuild while the sheet is up
  (another server's harnesses): a place then names another harness.
  `RunAnswer.Chosen` is values, found again as the rows are taken.
- **A create's question order is still written twice** (the TUI's `Model`
  and the bridge's `NewBox`) until #917 moves it into one core flow both answer
  through `Narration`. Until then a change to it changes both, and its rules
  (`DefaultHarnessQuestion`, `AskAboutWorkspace`, `WorkspaceChecked`) are
  called by both, never restated.
