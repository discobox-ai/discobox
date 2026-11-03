# 26-11-03-113 — A `discobox://` URI names a box, `discobox:///<box>` names it on the reader's server, and the console opens one clicked in a pane

- **Status**: Accepted
- **Date**: 2026-11-03

  Accepted by the owner by merging #500, 2026-11-03.

- **Extends**: [0116](0116-a-discobox-address-names-a-server-and-a-discobox.md)
  §1, whose address gains an empty-authority form; the rest of 0116, and its
  `<server>` table, stand.
- **Issue**: #499

## Context

A discobox has an address, `discobox://<server>/<discobox>` (ADR 0116 §1,
`endpoint.ParseSandboxAddress` in `pkg/endpoint/sandboxaddr.go`). A server sets
one on every box it creates while it listens on iroh, in `DISCOBOX_ADDRESS`
(`endpoint.EnvSandboxAddress`,
`server/internal/resources/sandboxes/reconciler.go`), with the server's peer ID
as `<server>`. Every box now signs what it posts on GitHub with it. Every
command that takes a discobox takes one (`selectSandbox`, `cli/DESIGN.md`
"Many Servers").

Nothing *opens* one. Two gaps:

- **No form means "the server I am on".** A tool inside a box that lists other
  boxes on the same server (the manager's Claude Code mod lists its `sbx_…`
  workers) would have to know its server's peer ID to write an address, and an
  address with one names that server whoever reads it. `ParseSandboxAddress`
  refuses an empty authority (`u.Host == ""`: "names no server").
- **The console's terminal pane can't follow one.** `termpane` links plain
  text only when it is `http(s)://` and the host's rewriter moves it
  (`urlPattern`, `rewriteLinks`, `termpane/links.go`). An OSC 8 link a program
  emits is passed on outward, so the only gesture that follows it is the outer
  terminal's Ctrl-click, which hands `discobox://` to an OS that has no
  handler for it. The console answers a plain click itself only on its *own*
  links: the header's ports, issue and PR (`hitURL`, `hitIssue`, `hitPull`,
  `cli/internal/tui/mouse.go`). A plain click in a pane is a selection, or the
  program's when it asked for the mouse.

## Decision

### 1. The URI is `discobox://<server>/<discobox>` or `discobox:///<discobox>`

| Written | Names |
| --- | --- |
| `discobox://<server>/<discobox>` | that discobox on that server: ADR 0116 §1, unchanged |
| `discobox:///<discobox>` | that discobox on **the reader's server** (§2) |

- `<server>` is what ADR 0116 §1's table says, in the same order: a peer ID
  (`d1-…` or its compact form, `?addr=` still accepted), an IP with or without
  a port, a DNS name with or without one, or the transport named outright as
  `discobox+http(s)://<host>[:<port>]`. `DISCOBOX_ADDRESS` keeps the peer-ID
  form. A registered server's name is still not a `<server>` (0116 §1), and an
  empty authority is now the one form that names no server.
- `<discobox>` is a discobox ID, full or a unique prefix (0116 §1). One path
  segment. Anything after it (a second segment, a query on the empty form, a
  fragment) is refused by name, as a second segment is today, so an extension
  later (see "Deferred") has nothing to break.
- The empty form has only the bare scheme. `discobox+http:///<id>` is refused:
  a transport with no host to apply it to means nothing.
- `ParseSandboxAddress` returns the empty form with an empty `Server`, and
  `String` writes it back as `discobox:///<id>`. Its callers have to choose
  the reader's server (§2) before they dial. An empty `Server` handed to
  `--server` or a dial is a programming error, not the local default.

### 2. The reader's server is the one the reader would name a bare ID on

`discobox:///<id>` is resolved on exactly one server. There is no fallback
search across servers. That is what makes it an address and not a bare ID:

| Read by | The reader's server |
| --- | --- |
| a CLI command (`discobox attach discobox:///sbx_…`), on a host or in a box | the command's primary: `--server`, `DISCOBOX_SERVER`, the recorded primary, the local default (ADR 0116 §3, 0135). In a box that is the box's own server, reached through its pool (ADR 0140). |
| the console, from a pane (§3) | the server of the box that pane belongs to, which is the server the program that wrote the URI runs on |
| the console, anywhere else it takes an address later | the primary |

The pane rule is the one the use case needs. The manager's mod writes
`discobox:///<worker>` from inside a box, meaning "a box on my server". A
console showing that box from another machine, with that server registered
under any address, opens the worker on it, and never on whatever server is
the console's primary.

An empty-form address never registers a server (ADR 0116 §6): there is no
server named in it to register.

### 3. A click on a box URI in a pane focuses that box

In the console, a `discobox://` URI in a pane's output is a link the console
answers. That covers bare text or the target of an OSC 8 link, in either form.

- **Detection is the host's, and `termpane` stays disco-agnostic.** `termpane`
  gains a way for its host to claim link targets: the schemes it also detects
  in plain text, and asking which link (emitted or detected) is under a cell.
  It knows nothing about `discobox://`. The API's shape is the
  implementation's. Detection stays scheme-led and stops at the row, as for
  `http(s)` today.
- **A plain left click opens it, and so does a Ctrl-click.** Box links work
  like the header's links (`hitURL`): lit under the pointer, opened on a
  press, opened once on a double click. Unlike those, the console takes the
  Ctrl-click too, and **draws a box link outward without its OSC 8**. There is
  nothing outside the console that can follow one. An OSC 8 left on it would
  make the outer terminal's Ctrl-click hand `discobox://` to the OS, which
  fails or opens nothing, while the console stays silent.
- **It is the console's even while the program has the mouse.** The target
  names something only the console can open. A program that asked for the
  mouse loses only presses on box-link text, and `leader m` (mouse seized)
  makes every press a selection, so the link can still be copied as text.
- **Focusing is `landOn`** (`cli/internal/tui/tree.go`,
  [ADR 26-10-31-319](26-10-31-319-a-press-on-a-workspace-tree-row-switches-to-it.md)):
  a live or parked box is switched to. Any other box is pointed at on the
  list with the reason it isn't attached ("enter starts it"), since a click on
  text a program printed shouldn't start a stopped box.

### 4. Which servers a click can reach, and what it says otherwise

A URI that names a server (§1's first form) is matched against the servers
the console lists (primary and registered), **by endpoint and by peer ID**. A
server is the same server however it was registered (`serverKey`, `byServer`,
`cli/DESIGN.md` "Many Servers"). `DISCOBOX_ADDRESS` names a server by peer ID,
while the console usually knows its primary by a local socket or an http
address. So the console learns each server's peer ID (`GET /peer`, which
every current server answers, ADR 0117) and doesn't rely only on the
registration record, which a primary nobody registered doesn't have. How and
when it asks is left to implementation.

| The URI's server is | A click |
| --- | --- |
| one the console lists | focuses the box there (§3) |
| one the console lists, and the box isn't on it | says so on the status line: `no discobox <id> on <server>` |
| one the console lists, not answering | says the server isn't answering, as the listing does |
| none it lists | says so on the status line, naming the command that would reach it: `discobox://… is on a server this console doesn't know — discobox attach <uri> registers it` |

**A click never dials or registers an unknown server.** A typed `discobox
attach <address>` does, under ADR 0116 §6, because the person typed it. The
text in a pane was written by whatever runs in the box, and a click on it
would otherwise make the console reach, and remember, a host a stranger
chose.

### 5. Where the syntax is written down

The URI's grammar (§1) and resolution (§2) are documented once, beside
ADR 0116's address in `cli/DESIGN.md` "Many Servers". The console's handling
(§§3–4) goes in `cli/internal/tui/DESIGN.md`. `termpane`'s claim API goes in
`termpane/DESIGN.md`.

## Alternatives rejected

- **A reserved host, `discobox://local/<box>` or `discobox://localhost/…`.**
  Under ADR 0116 §1 both are already servers: a DNS name without a port, looked
  up at `_discobox.<name>` and otherwise dialed over https. Reserving one takes
  a name away from the table and makes an address mean two things depending on
  whether the reader knows the reservation. An empty authority is RFC 3986's
  own spelling for "no host named, the local one" (`file:///`), and no
  existing address can be written that way, since `ParseSandboxAddress`
  refuses it.
- **A project segment, `discobox:///<project>/<box>`.** A box ID is unique on
  its server, so a project adds nothing to finding it. Project IDs are per
  server (0116 §4), so the segment would mean different projects to different
  readers, and the `default` alias would make that worse. It would also turn
  0116's "more than one segment is refused" into a parse that depends on
  counting segments.
- **The empty form as a bare ID** (primary first, then every registered server,
  as `selectSandbox` does for an ID). That makes the URI's meaning depend on
  which servers the reader has registered, which is exactly what ADR 0116 §1
  refused for registered names. In a pane it would also find a same-prefixed
  box on some other server before reporting "not here".
- **A `discobox://` OS URL handler**, so a click anywhere (the outer terminal,
  a browser, GitHub) reaches a running console. That means a handler
  registered per platform (macOS `LSSetDefaultHandlerForURLScheme`, a Windows
  registry key, an XDG `.desktop` file under WSL and Linux), and a way into a
  console that is already running, which nothing offers today. The issue asks
  for the pane. Deferred, below.
- **Leaving Ctrl-click to the outer terminal**, as for the header's `http`
  links. That works there because the OS can open `http://`. For a box URI it
  is a gesture that silently does nothing.

## Deferred

- **A terminal or pane in the URI** (`discobox:///<box>?terminal=<id>`, or a
  path segment). Revisit when something needs to link to one terminal and not
  only to the box. §1 refuses anything after the box segment, so adding one
  later breaks nothing that parses today.
- **Opening a box URI from outside the console** (the OS handler above).
  Revisit when a box URI on GitHub or in a browser is something people report
  wanting to click. The empty form would then resolve on the opening
  console's primary, by §2's third row.

## Consequences

- Every address that parses today means what it meant. `discobox:///<id>` is
  new and was an error.
- A tool in a box can write a link to another box on its server without
  knowing that server's ID. The link works in every console that has the
  box's server registered, and at the CLI in any box on that server.
- An empty-form URI pasted elsewhere (an issue, a chat) means the reader's
  server, so it is the wrong form for a link meant to leave the server.
  `DISCOBOX_ADDRESS` stays the full form for that reason.
- `termpane` gains a host-claimed-link API. Its only consumer is the
  console.
- A program that tracks the mouse no longer receives presses on box-link
  text in its pane.
- Tests cover parsing both forms and the refusals in `pkg/endpoint`, and the
  click handling (bare and OSC 8, local and full form, known and unknown
  server) in `cli/internal/tui`.
