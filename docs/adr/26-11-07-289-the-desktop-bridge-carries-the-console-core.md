# 26-11-07-289 — The desktop bridge carries the console core

- **Status**: Accepted
- **Date**: 2026-11-07
- Accepted by the owner by merging #844, 2026-11-07
- **Issue**: [#786](https://github.com/discobox-ai/discobox/issues/786) (epic), [#787](https://github.com/discobox-ai/discobox/issues/787)

## Context

`discobox desktop` serves its app over stdio
([26-11-07-983](26-11-07-983-discobox-desktop-downloads-its-app-and-serves-it-over-stdio.md)).
The app renders the console core
([26-11-07-876](26-11-07-876-the-console-is-one-core-with-two-renderers.md)).
This ADR decides what crosses between them.

Checked at `00541f9c90fefeabe1b61134a737fab287861803`:

- The prototype bridge (`cli/internal/cli/desktop.go`) framed each message
  as a kind byte, a big-endian u32 stream and a u32 length. Kind 255 carried
  JSON on stream 0. Every other kind was an execstream frame type
  (`pkg/execstream/frame`) for one terminal.
  - It served nine `DataSource` calls plus a listing pushed every 2 s.
  - Its Rust types were written by hand. One mismatch, a nil Go slice encoded
    as `null`, stopped the app at its handshake.
  - A resize sent before a stream was registered was silently dropped.
- `DataSource` calls that report as they go take callbacks. These are `Run`'s
  `report`, `warn` and `accepted`, `CredentialUI`'s `Confirm` and `Run`, and
  `DeliverSource`'s `report`. `FollowAudit`, `WatchProvisioning` and
  `Forward` return streams or channels. `EditHarnessFile` and `EditToolFile`
  hand the terminal to `$EDITOR` through `tea.Exec`.

## Decision

### 1. One framed pipe each way

The prototype's framing stays: kind (1 byte), stream (u32), length (u32),
payload, each way.

- **Kind 255** is a JSON message on stream 0.
- **Every other kind** is an execstream frame type on a non-zero stream:
  - Stdout, Stderr and Exit from the CLI;
  - Input, Resize, Repaint and CloseInput from the app.

A stream is allocated by the app before it is opened. The CLI buffers its
input, resize and repaint frames until the stream is attached, so nothing sent
early is lost.

### 2. Messages address the core

| Message | From | Purpose |
| --- | --- | --- |
| `hello` / `welcome` | app / CLI | Protocol version, the core's spec (commands, bindings, glyphs, tokens), the session |
| `subscribe` / `unsubscribe` | app | Listing, resources, credential and approval requests, provisioning, audit follow, forwarded ports |
| `snapshot` | CLI | The current value of a subscription, sent whole on subscribe and on every change |
| `command` | app | A command ID, its explicit targets and arguments |
| `result` | CLI | A command finished, failed with a reason, or needs a form |
| `form` / `submit` / `fieldErrors` | CLI / app / CLI | A form described by the core, filled by the app, validated by the core |
| `question` / `answer` | CLI / app | A question a running command asks, such as a credential confirm or purge's confirmation |
| `progress` | CLI | A running command's report, warning or acceptance |
| `edit` / `saved` | CLI / app | A file to edit: a local path the app opens in the user's editor, and the app's word that it was saved |
| `upload` | app | A pasted or dropped image, for `PutImage` |

Commands run in the core with the core's rules. The app never evaluates
eligibility, ordering or placement itself.

### 3. Versions and generated types

- **The wire schema.** The message types are Go structs in
  `cli/internal/console/bridge`. Their JSON schema is generated from them, and
  the Rust types are generated from the schema. Both generated files are
  checked by `task verify`.
- **Versions.** `hello` carries a protocol version. Since the CLI pins the
  exact app it runs (983 §2), a mismatch means a development override. The
  app says which side is older and exits.
- **Empty values.** No field is ever `null`: an empty list is `[]`, and an
  absent value is omitted.

### 4. Contract tests

A contract suite runs the real Rust client against a Go fake of the core over
a pipe. It covers every message kind, early stream frames, a CLI that exits
mid-command, and a question asked while another command is in flight.

## Alternatives rejected

- **Expose `DataSource` as is.** That is what the prototype did. The app then
  needs the core's rules to use the answers (876's rejected alternative).
  Callbacks such as `CredentialUI` also have no wire form unless they are
  reshaped into questions anyway.
- **gRPC or Cap'n Proto over the pipe.** These are heavier toolchains on both
  sides for one pipe between two programs that always ship together. The
  execstream frames, which terminals already use end to end, would need
  re-wrapping.
- **Hand-written Rust types.** The prototype's `null` failure is the kind of
  drift generated types prevent.

## Consequences

- Each wave of the desktop port adds core commands and subscriptions, not
  bridge-specific calls.
- `CredentialUI` and the other callbacks become core questions and progress.
  The TUI answers them with its existing dialogs, and the desktop with sheets.
