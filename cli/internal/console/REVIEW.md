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
