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
