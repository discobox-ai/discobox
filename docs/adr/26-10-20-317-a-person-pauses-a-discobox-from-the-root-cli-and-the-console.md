# 26-10-20-317 — A person pauses a discobox from the root CLI and the console, and the CLI calls the state `paused`

- **Status**: Proposed
- **Date**: 2026-11-21
- **Amends**: [26-10-16-420](26-10-16-420-a-docker-pool-freezes-a-sleeping-sandbox-and-a-person-can-suspend-one.md)
  §3, where the CLI offered suspend as `discobox admin box suspend` only.
  Everything else in 26-10-16-420 stands, the API's `suspended` included.
- **Relates to**: [26-10-23-600](26-10-23-600-the-cli-is-spelled-one-way-run-takes-its-source-and-only-commits-go-in.md)
  §1, one spelling per meaning and old spellings kept as untaught aliases;
  [26-10-02-478](26-10-02-478-a-discobox-starts-and-stops-the-discoboxes-it-creates.md) §1,
  the power calls the sandbox role allows.

## Context

26-10-16-420 gave a person a way to freeze a running box, with its memory
kept, and kept it out of the everyday commands. Its only spelling is
`admin box suspend DISCOBOX_ID`, in `cli/internal/cli/sandbox.go`, and it
shipped in v0.19.0. The root power commands, `discobox start`, `stop` and
`restart` (`sandboxLifecycles`, `cli/internal/cli/lifecycle.go`), take any
number of discoboxes, each named the way `discobox ls` lists it. The console
offers stop (`t`) and start (`T`) on the selected boxes
(`Model.actions`, `cli/internal/tui/model.go`), and no freeze.

Freezing a box to free its CPU, then coming back to it where it was, is an
everyday thing to do, and `start` or any use already thaws it (26-10-16-420
§4). disco-vm is adding a freeze for machine pools (#1274), so it will stop
being a Docker-only action.

"Suspend" is the API's word for this: the route (`POST
/projects/{p}/sandboxes/{s}/suspend`), the provider contract's `Suspend`, the
pool's `suspend` route, and the observed `runtimeState`/`displayState`
`suspended`. Renaming it all to "pause" means changing the OpenAPI document,
the generated clients, the provider and pool contracts and their
implementations, and the stored runtime state. That is a migration, for a word
only a person reading the CLI sees.

## Decision

**A person pauses a box with `discobox pause` or the console's pause
action. The CLI and the console call the action "pause" and the state
`paused`. The API, the pool, the provider contract and the stored state keep
`suspend` / `suspended` unchanged, and the CLI translates between the two
words.**

### 1. `discobox pause DISCOBOX...` is a root power command

It is a fourth `sandboxLifecycles` entry beside `start`, `stop` and
`restart`, and so it takes and resolves its arguments the way they do. It
posts `POST /projects/{p}/sandboxes/{s}/suspend`, and what happens on the
server is 26-10-16-420 §3's: a box that is not running is refused, one already
suspended is a no-op, and a pool that cannot freeze refuses, with the box left
running. There is no root `suspend`: one spelling per meaning (26-10-23-600
§1).

### 2. `discobox admin box pause DISCOBOX_ID` is the raw form

It replaces `admin box suspend` as the raw form. That name shipped in
v0.19.0, so it stays working as an alias that the help, the reference and the
docs don't teach (26-10-23-600 §1).

### 3. The console has a pause action

The console's action menu gets a **pause** action beside stop and start. It
is enabled when a selected box is running, and pauses each running box
selected. Start's detail stops talking about thawing a suspended box and
talks about resuming a paused one. On a pool that cannot freeze, the action
reports the pool's refusal for that box, as the console reports a failed
stop. Implementation picks the key, from the keys the action menu and the
list don't already use.

### 4. The CLI shows the state as `paused`

Everywhere the CLI or the console writes a box's state for a person to
read, `suspended` reads `paused`: `discobox ls` and `admin box ls`/`get` in
their table forms, shell completion's descriptions, the box picker, and the
console's list and detail. The console keeps its `‖` mark. The word changes where
the CLI turns a `displayState` into text (`sandboxDisplayState`,
`cli/internal/cli/output.go`) and where the console names its own state
(`tui.StateSuspended`, `cli/internal/tui/data.go`, which the list prints).

`-o json`, `-o yaml` and `discobox events` print the API's record, so they
keep `suspended`. They are what scripts read, and the API reference documents
the field with that value.

### 5. The sandbox role still cannot pause

26-10-16-420 §3 stands. The sandbox role keeps start, stop and restart
(`sandboxRole`, `server/internal/auth/authorizers.go`) and gets no suspend
route. A discobox that runs `discobox pause` is refused by the server, and the
CLI prints that refusal for each argument the way `start`, `stop` and
`restart` print a failure.

## Alternatives rejected

**Rename the action and state everywhere to `pause` / `paused`.** This was
the earlier draft of this ADR's scope, from a working tree that changed 84
files. Rejected: it changes the API, both pool and provider contracts and the
stored runtime state, and needs a migration and a period where API clients
accept both words. All of that is for a word that, on the CLI's side, one
translation already changes.

**Keep `suspended` as the CLI's state word, with `pause` as the only new
word.** The earlier draft decided this ("a paused box lists as
`suspended`"). Rejected: a person who typed `pause` and then reads
`suspended` has to learn that the two words mean the same thing.

**Show `paused` in `-o json` too.** Rejected: the JSON output would no longer
be the API's record, and a script written against the API reference would
read a value the reference doesn't list.

**Drop `admin box suspend`.** The earlier draft dropped it because nothing had
shipped against it. Rejected: it shipped in v0.19.0, and 26-10-23-600 §1 keeps
a shipped spelling working as an alias.

**A root `resume` or `unpause` verb.** Rejected for 26-10-16-420's reason: a
paused box comes back the way a stopped one does, by `discobox start` or by
being used. A second verb would also be one that every start on use had to
know about.

## Consequences

- `discobox pause mybox` freezes a box, and `discobox ls` lists it as
  `paused` until `start`, an attach or any other use thaws it.
- A Docker box that froze for its own sleep (26-10-16-420 §2) is in the same
  state, so it lists as `paused` too.
- The CLI and the API use different words for one state. The CLI's `-o json`
  and the API docs say `suspended`, and the CLI's tables and the console say
  `paused`. Error messages the server writes, such as a read refused because
  the box is suspended, keep the server's word.
- On a machine pool, `pause` is refused until that pool's runtime can freeze
  (#1274). The command, the action and the state word need nothing more when
  it can.
