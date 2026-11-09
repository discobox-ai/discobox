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
