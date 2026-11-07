# 26-11-07-303 — A command Discobox puts on PATH is linked in /usr/local/bin, and what the box had there is moved aside

- **Status**: Proposed
- **Date**: 2026-11-07
- **Amends**:
  [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §4's `adopted` row ("Installs nothing; wires ours on top") and §9's
  "never displaces what a provider owns", for the names on `/usr/local/bin`.
- **Relates to**: #775, #776.

## Context

ADR 26-10-16-423 §4 has a feature install into "a prefix Discobox owns"
when the box's own copy is not usable, and adopt the box's copy when it is.
It does not say how the chosen copy is the one a user, a script or a unit
runs. A box's image may have put anything at a command's name on the system
PATH. A boxd machine links `/usr/local/bin/{node,npm,claude}` into
`/home/boxd/.nvm`, which is mode 0750, so no other user can enter it. Its
`corepack` link points there too, beside a Node 25 that ships none.
`/usr/local/bin` comes before `/usr/bin` on every PATH, so whatever the box
has there shadows a copy Discobox installed or adopted elsewhere:

- `npm install -g`, as root, ran boxd's npm and put the harness CLI into
  `/home/boxd`, where the sandbox user cannot run it.
- A bare `node` in a launcher or capture script ran a Node the adopt check
  had refused.

The features already fixed (#776) install into
`/usr/local/lib/discobox/<tool>` and link each command into `/usr/local/bin`
with core's `link-command`. That replaces what the box had at the name. This
ADR records that choice and how a box's entry is kept.

## Decision

### 1. Every name a feature puts on PATH is a link in `/usr/local/bin`

- **Installed tools.** Each command a feature installs, into its prefix under
  `/usr/local/lib/discobox/`, is linked into `/usr/local/bin` by core's
  `link-command`.
- **The chosen Node, adopted or installed.** Its `node`, `npm`, `npx` and
  `corepack` are linked the same way.
- **Discobox's own commands.** `discobox`, `discobox-review`, nix's shims and
  the `docker` personality are linked the same way too.
- **The one exception.** Core's own `link-agent.sh` runs before
  `link-command` exists, and links with `ln`.

Adopting a copy still installs nothing. Linking it into `/usr/local/bin` is
part of the "wires ours on top" that §4 already gives an adopted feature.

### 2. What the box had at the name is moved aside, never deleted

A name Discobox links is Discobox's. Whatever the box had there is moved to
`/usr/local/lib/discobox/displaced/`, under its own path
(`…/displaced/usr/local/bin/node`, a later one `.1`, `.2`). That covers a
file, or a link anywhere, dangling or not. The move is written to the
feature's log. A link already into `/usr/local/lib/discobox/` was laid by an
earlier step, and is replaced in place.

To put a box's own copy back, move it back from `displaced/`. A name that
goes with a refused copy and has no counterpart in the chosen one is moved
aside with nothing put in its place (`link-command --aside`): boxd's
`corepack` beside a Node 25 is one.

§9's "never displaces what a provider owns" stays as written for what a
provider *declares* it owns: DNS, the network, sshd, the trust store. A
provider's entries in `/usr/local/bin` are not among them. They are moved,
so nothing is lost.

### 3. Each linking step checks its links at every boot

Each step that links has a `check` (`link-command --check`). The check fails
when a link is gone, points elsewhere, or is shadowed again, so the step
runs again at the next boot. A box provisioned before this decision is
repaired the same way. The adopt check passes over a link to the same
command in a later system PATH directory, so linking the chosen copy does
not change what the next boot adopts.

## Alternatives rejected

- **A Discobox bin directory ahead of `/usr/local/bin` on PATH**, leaving
  the box's files as they are. Discobox sets PATH for login shells (core's
  `profile.d`) and for a non-login exec (`image.json`), but not for
  everything that runs a command by name:
  - systemd gives its units its own default PATH;
  - sudo uses its `secure_path`;
  - cron, `env -i` and a script that sets PATH outright each use their own.

  Every one of those starts with `/usr/local/bin`, so on boxd each would
  still run the refused nvm copy. Covering them would mean editing sudoers,
  systemd's manager configuration and every unit, which changes more of the
  box than moving a few entries in one directory.
- **Replace the box's entries with `ln -sf`, as several features did.** It
  deletes what the box had there, with no record, and it is not reversible.
  A provider's link there may be the only way back to its own copy.
- **Replace only entries that are dangling or closed to other users, and
  leave any working command in place.** A box's working command can still
  be the wrong one: an older version the adopt check refused, or a copy
  that is not the chosen Node's own `npm`. A pair split that way, one
  user's `node` with Discobox's `npm`, fails in ways no check sees.

## Deferred

- **The browser's `chromium` wrapper**, which a `file` step writes to
  `/usr/local/bin/chromium` over whatever the box had there. Every other
  `file` step into `/usr/local/bin` writes a `discobox-*` name, which no
  image ships. Revisit when a provider's image is found with its own
  `chromium` there, or when the browser feature is next changed: it would
  install the wrapper under `/usr/local/lib/discobox/browser` and link it.

## Consequences

- On boxd, `/usr/local/bin/{node,npm,npx,corepack,claude}` are Discobox's
  links, and the box's own are in
  `/usr/local/lib/discobox/displaced/usr/local/bin/`.
- A box's Docker CLI at `/usr/local/bin/docker`, if it had one, is moved
  aside for the `docker` personality. Before, `ln -sf` deleted it.
- Running the same steps again changes nothing, and a check puts back
  whatever drifted.
