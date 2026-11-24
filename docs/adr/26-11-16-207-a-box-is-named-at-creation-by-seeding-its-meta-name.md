# 26-11-16-207 — A box is named at creation by seeding its meta name

- **Status**: Accepted
- **Date**: 2026-11-16
  Accepted by the owner by merging #1246, 2026-11-16
- **Amends**:
  [26-10-12-026](26-10-12-026-a-sandboxs-name-is-meta-and-renaming-it-is-a-meta-write.md)
  §4, which made a rename the only way to set the meta name from outside: a
  create may now set it too. Its §3 is unchanged: `config.name` stays the
  generated handle.
- **Amended by**: [26-11-19-859](26-11-19-859-sandbox-create-supplies-an-account-and-a-handle.md) — §1 for a create with no handle: the server draws it.

## Context

A sandbox has two names (ADR 26-10-12-026). `config.name` is the handle: the
CLI generates it at create (`randomname.Generate()`,
`cli/internal/sandboxcreate/prompt.go`), the server requires it and keeps it
unique in the project (`idx_sandbox_project_name`,
`server/internal/resources/sandboxes/service.go`) because `discobox admin
ssh-config` emits it as an ssh `Host` alias and scripts resolve a box by it.
The meta name is `name` in `~/.discobox/meta.json`: display text, not unique,
the system of record inside the box, cached on the row as `meta_name`, and the
first choice of `displayName` (ADR 26-10-31-618 §1).

Since #700, `discobox run` seeds the meta file's description and tags at
creation (`--description`, `--tag`, and `"description"`/`"tags"` in
`--from-json`). The create body carries them as flat
`config.description`/`config.tags`; the server checks them as a meta change,
records them unobserved on the row, and the reconciler hands the row's
`Description` and `Tags` to the pool (`reconciler.go`), through
`sandboxconfig.Runtime` and the pool's create body (create protocol revision
2), to the sandbox agent, whose `meta.File.Seed` writes them only when there
is no meta file. `Seed` says outright that the name is not seeded: "the name
is a person's or the agent's to give". So a box can only be named after it
exists, with `discobox rename`, which boots it if it is stopped.

The owner wants `discobox run --name` (issue #706, from #700): it sets the
meta name, which overrides everything else in the listing, and it does not
set `config.name`, because the generated handle cannot be moved into
`meta.json`.

## Decision

### 1. `--name` sets the meta name, never `config.name`

`discobox run --name NAME` names the new box the way `discobox rename` would
afterwards. The CLI still generates `config.name`, and nothing about the
handle changes: it is unique, required, and resolvable, and `--name` neither
replaces it nor is checked against it. `discobox admin box update --name`
stays the only edit of `config.name`.

### 2. The create body carries it as a flat `config.metaName`

`SandboxCreateConfig` gains an optional `metaName`, a string, beside
`description` and `tags`. It is not a `config.meta {name, description, tags}`
object: `description` and `tags` are already flat fields that clients send,
and moving them under an object would break those clients to tidy a shape.
`metaName` is the name the sandbox row already reports the cached meta name
under (`model.Sandbox.MetaName`, `json:"metaName"`), so one word means one
thing on the read and the write side, and `config.name`, the API's own
`name`, stays the handle.

An empty or absent `metaName` sets no name.

### 3. The server checks it and records it before anything is created

`CreateSandbox` checks the name with the description and tags, as one
`sandboxmeta.Meta{Name, Description, Tags}.Validate()`, and refuses a bad one
with 400 before anything is created. It records it on the row's `meta_name`
unobserved, as the tags are, so the first listing's `displayName` carries it,
and the sandbox's first report of its meta file replaces it under the
newer-wins rule (ADR 0136 §§2, 4). The created event does not carry it: an
event's `name` is the handle, and an event carries no meta name, as it
carries no description (ADR 26-10-29-316 §1).

Uniqueness is not checked: two boxes may carry the same meta name, and a
command given it refuses it as ambiguous, as ADR 26-10-12-026 already says
of a rename.

### 4. The name is seeded on the description's terms

The seed carries the name to the box as it carries the description: the
reconciler passes the row's `MetaName` with its `Description` and `Tags`;
`sandboxconfig.Runtime` and the pool's create body gain `metaName`; the
sandbox agent passes it to `meta.File.Seed`.

`Seed` writes name, description and tags **only when there is no meta file**,
and only when one of them is set. This reverses `Seed`'s "the name is not
seeded" rule, and keeps the rest of it: once the file exists, it is the name,
whatever the box was created with. A rename, or the agent writing the file,
survives a restart and a re-create, and a name cleared later is not put back,
because the row the seed is read from has followed the file.

### 5. The pool's create protocol advances to revision 3

The pool's create body gains a field a strict older pool agent refuses, so
`createprotocol.Revision` goes from 2 to 3, as #700's did, and a start waits
for a compatible pool (ADR 26-11-05-143 §1).

### 6. `--from-json`'s `"name"` is the meta name

In `discobox run --from-json`, `"name"` means `--name`: the meta name. It is
not the API's `config.name`, and the request has no way to set the handle.
The JSON request is the CLI's flags as an object, not the API body, and a
person writing `"name"` means what `--name` means. `run --help` and
`docs/public/cli.md` say so in the field's description, since the API's
`name` is the handle.

## Alternatives rejected

**`--name` sets `config.name`.** It is the obvious reading of the flag, and
the owner rejected it. The handle is required and unique per project, so a
person's name for a box would collide with another box's, be refused, or have
to be uniqued; it is the ssh alias and the name scripts resolve, so it must
not be display text; and the handle the CLI generates cannot be moved into
`meta.json` afterwards, so the box would have one name nobody can rename the
way every other box is renamed. ADR 26-10-12-026 rejected a rename that edits
`config.name` for the same reasons.

**Set both: `--name` as the handle and the meta name.** Rejected: it ties a
unique, ASCII-ish identifier to free display text of up to 128 bytes, and a
later rename moves one and not the other.

**A `config.meta {name, description, tags}` object.** Rejected (§2): it moves
two fields clients already send, for symmetry alone.

**Deliver the name with a meta write after the create.** The CLI could create
the box and then `PATCH .../meta`, which already takes a name. Rejected: it
is two requests where one would do, a box in the listing without its name
between them, a create that half-succeeds when the second fails, and a write
that waits on the box being up to answer (ADR 0136 §3). #700 seeded the description and
tags for the same reasons.

**Seed the name every start, not only when there is no file.** Rejected: it
would undo a rename on every restart, and make the create a standing claim
over a file the box owns (ADR 0136).

## Consequences

- `discobox run --name "Fix login flake"` lists as `Fix login flake` from the
  first listing, while `config.name` stays a generated handle that `ssh`,
  scripts and `admin box update` keep using.
- The same field reaches every create path: the flags, `--from-json`, and the
  window's create, which builds its body through the same `PromptOptions`.
  Whether the console's create form gets a name field of its own is left
  open; it is not needed for the flag.
- A sandbox (a lead creating a worker) may name it at creation with the
  route it already uses to create it; nothing new is granted.
- A server from before this change refuses a create body with `metaName`
  (`additionalProperties: false`), as one from before #700 refused `tags`.
  The CLI sends it only when a name was given.
- A pool agent from before revision 3 is not sent a create it would refuse;
  the start waits for the pool's upgrade (§5).
- `server/internal/resources/sandboxes/DESIGN.md`, `sandbox-agent/DESIGN.md`
  and `pkg/sandboxconfig/DESIGN.md` describe the seed as name, description
  and tags once this lands.
