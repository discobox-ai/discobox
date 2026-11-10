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
- **A tool runs through the core** (`OpenTool`, `RunHostTool`, `EditTool`):
  each reads the box's tools again and refuses what the picker would, and
  `OpenTool` attaches a running tool's live session (`RunningTools`) rather
  than make a second. The TUI's first move onto the core kept calling
  `NewTool` on the catalog it had read, refusing nothing, while its docs said
  every console went through the core; and the desktop, opening a tool from
  the list with no workspace up, started a second session of one running.
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
- **What the secrets page says and does is `secrets.go`'s.** A row's
  standing, what reading a secret, a binding or a decision says, what a
  delete or an unbind asks, and what an edit says it changed were the TUI's
  alone; the bridge calls the same functions. An action reads its secret (and
  binding) again inside itself, never as a step a caller must remember: the
  first version had the bridge read first and the TUI not at all, while the
  docs said both did.
- **Narrowing anything to "this server" is the core's** — requests,
  refusals, boxes, harnesses (`RequestsFor`, `RejectionsFor`,
  `RefusedSecrets`, `BindableBoxes`). The secrets page's first version
  filtered refusals by server in the bridge and not in the TUI, so the two
  could disagree on which rows said "refused".
- **A run option's rule is `RunOptions`', never a renderer's.** Which rows a
  create offers, what each leads with, the sources, the request and the
  command were the TUI's panel alone, and the desktop would have needed a
  second copy. A renderer moves a cursor over the rows and draws them; the
  bridge holds the window's `RunOptions` and fills `CreateForm`/`OptionsForm`.
- **Where a link in a pane goes is `links.go`'s, never a renderer's.** The
  forward rewrite and which box an address names were the TUI's alone; the
  desktop asks the bridge (`follow`) rather than keeping a copy in Rust.
  Finding the link in the screen's text is the renderer's emulator.
- **A form's answer names a choice by its value, never its place.** The
  run options' answer was first read back into positions and applied to rows
  the bridge holds, which another command can rebuild while the sheet is up
  (another server's harnesses): a place then names another harness.
  `RunAnswer.Chosen` is values, found again as the rows are taken.
- **What a create asks, and in what order, is `Create`'s alone.** The order
  was once written twice (the TUI's `Model` and the bridge's `NewBox`), and
  the desktop's copy question had no size. A console calls `Create` and
  answers its questions through the narration; it never asks one of them
  itself, decides what an answer leads to, or reads a question's keys for
  meaning (`Choice.Means` says which choice is a yes or a no).
- **What the inbox says and does is `requests.go`'s, `approval.go`'s and
  `notices.go`'s.** A request's cards, where a part's answer goes next, the
  review of what an approval replaces, which notice a workspace shows and what
  a dismissal covers were the TUI's alone (its `approval` struct, its band);
  the desktop would have needed a second copy. A renderer sequences the cards
  and draws them; it never words a card or decides a step. That includes a
  card's actions (`CardActions`: their keys, labels, which declines), what a
  renewal that came back says and does (`Renewals.Finish`), which notice
  moves (`Notice.Urgent`), and the order of a page's requests (`RequestsOn`):
  each was first written in both the TUI and the bridge.
- **When the explainer may come up is the core's rule
  (`ExplainerScreen.Free`), not a renderer's.** Each console reports what has
  its screen; the desktop, which cannot call the core, writes the same rule
  over the same facts with the spec's `PromptQuiet`. The explainer's
  free-screen check was written short twice: the TUI's first guarded the
  terminal only, and the desktop's first had no quiet window, so the Enter
  that sent a prompt could answer it.
