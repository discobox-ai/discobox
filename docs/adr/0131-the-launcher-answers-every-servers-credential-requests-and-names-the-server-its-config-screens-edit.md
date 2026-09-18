# 0131 — The launcher answers every server's credential requests, and names the server its config screens edit

- **Status**: Accepted
- **Supersedes**: [0116](0116-a-discobox-address-names-a-server-and-a-discobox.md) §4's "the launcher's credential inbox, and its harness and secret screens, are the primary's". The rest of §4 stands.
- **Date**: 2026-09-18

## Context

[ADR 0116](0116-a-discobox-address-names-a-server-and-a-discobox.md) §4 made the
launcher list every server and route each row's verbs to its server, but kept
the credential inbox and the harnesses (F3) and secrets (F4) screens on the
primary. Following each request to its server was deferred until "a registered
server's discobox waiting on a credential is a thing somebody reports missing".
That has now been reported, along with three more problems that come from the
same rule:

- **A request is invisible unless it is on the primary.** `CredentialRequests`
  asks the primary only. The list's `!` mark (`Sandbox.PendingRequests`) and the
  workspace's credential banner both read what it returns, so a discobox on a
  registered server that is waiting on a credential shows no `!`. When it is
  opened in the workspace, the banner never appears. The agent inside waits
  until it times out, and the window never mentions it.
- **F3 and F4 do not say which server they edit.** The header opens on `all
  servers`, and the screens hide the server control (`viewHeaderLeft`) so that
  they do not appear to be narrowed. Nothing on screen says that a secret added
  there goes to the primary.
- **F3 and F4 ignore the server the header is narrowed to.** With the list
  narrowed to `beta`, and the prompt creating on `beta`, F4 still edits the
  primary's secrets. Someone adding the secret that `beta`'s discobox needs puts
  it on the wrong server, and nothing on screen shows that.
- **Harnesses and secrets are per server.** Each server has its own harness
  configs, secrets and grants, and a request can only be answered with a secret
  on its own server. So "the primary's screen" is not a simplification over the
  others. It is simply the wrong server whenever the work is somewhere else.

`discobox attach` from a shell is not affected: it opens the window on the App
aimed at the discobox's server, which is then that window's primary. The gap is
a registered server's discobox opened from inside a window whose primary is
another server.

## Decision

### 1. The inbox is every server's, and each request is answered on its own server

Pending credential requests are read from every server the window lists, with
the same polling approach as the listing ([ADR 0122](0122-a-window-that-polls-lists-the-servers-that-answer.md)).
A slow server cannot hold back the others, and a server that did not answer
contributes nothing and is already reported as `not answering`. Each request
carries the server it came from.

Everything that answers a request goes to that server: the secrets offered for
it, a secret typed into the approval dialog, the approval, and the denial. So
the `!` mark and the workspace banner appear for any discobox the window lists,
and answering one works the same wherever the discobox is.

### 2. F3 and F4 are always one server's, and the header names it

The harnesses and secrets screens show one server. They open on the header's
server, or on the primary when the header is on `all servers`. The header's
server control stays drawn over them and names that server. It offers the
servers but not `all servers`, which is not a server anything can be added to.

It is the same control as the list's server filter, not a second one, in line
with "the two filters are one filter" in the tui DESIGN. Choosing `beta` on F4
means the list is narrowed to `beta` after Esc, and the prompt creates there.
Opening F3 or F4 from `all servers` and leaving without choosing another server
leaves the list on `all servers`.

The secrets screen's own inbox lists only its server's requests: those are the
only ones its secrets can answer.

### 3. The data source is told the server rather than guessing it

Every `DataSource` method that reads or changes server-scoped configuration —
harnesses, harness secrets, harness verbs and configure, secrets, grants, and
approving and denying requests — takes the server by the name the window lists
it under, as a parameter. It is not inferred from an ID the way sandbox verbs
are (`apiDataSource.at`). A secret ID says which server it is on only after that
server has been listed, and the call that lists secrets is the one that needs
to know which server to ask.

### Rejected

- **Merging the servers into F3 and F4, with a section per server as the list
  has.** A new secret, a new grant or enabling a harness still has to go to one
  server. That would bring back the question of which server, now asked in every
  dialog on the screen instead of answered once in the header. Two servers'
  `claude` harnesses side by side also read as a duplicate, not as two servers.
- **Keeping the screens on the primary but naming it in the header.** It fixes
  the missing label and leaves the other problem in place. With the list
  narrowed to `beta`, the screen would correctly say it is editing the primary,
  which is still not what the user wants.
- **A separate server selector on F3/F4, independent of the list's.** Two
  controls that both say "server" and can disagree, in the same header. The
  folder and server filters became one control for this reason.
- **Routing secret and grant calls by ID, as sandbox calls are.** It only works
  for IDs the window has already listed from a server, and listing is the call
  that needs the server first. A parameter says it once, at the point where the
  window knows it.
- **Answering from `discobox --server <name> tui`**, which is what §4 left in
  place. The request is not visible in the window the person is using, so they
  do not know to open another one.

## Consequences

- `CredentialRequest` gains `Server`. `DataSource`'s harness, secret, grant and
  request-answering methods take a server name. Every caller and the test fake
  change with them, and no method keeps its old primary-only form.
- The CLI's `apiDataSource` resolves a server name to that server's data source
  (`sourceFor`), so each method's body is written against one server, as the
  sandbox methods already are.
- The credential poll goes to every server on the listing's terms: one request
  outstanding per server, bounded by `pollTimeout`, with the window drawing
  whatever has arrived. A window with one server makes the same single call it
  does today.
- The header's server control is drawn over F3 and F4 whenever there is more
  than one server, and the tui DESIGN's "Only the list narrows" becomes
  "everything in the header follows one server".
- The server F3 and F4 show is always the server the next create goes to, so
  one list of harnesses serves F3, the run options and the questions a run
  asks before it creates, and it follows the header. The run options offer the
  target server's harnesses as a result, not as a goal: §5's rule that a
  harness is sent by slug and resolved by the server it is sent to stands, and
  servers are still expected to agree on their slugs.
