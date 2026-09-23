# 0146 — A hook keeps the name its harness used, and gains a canonical one

- **Status**: Accepted
- **Date**: 2026-09-23
- **Relates to**: [ADR 0137](0137-a-terminal-can-be-read-typed-into-and-waited-on-without-attaching.md),
  the wait that matches hook events; [ADR 0127](0127-the-opencode-harness-runs-opencode-1.md),
  the opencode harness; [ADR 0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md),
  an image Discobox did not build.

## Context

A harness hook is recorded as `(provider, event, payload)` and read back by
`discobox admin audit hooks`. [ADR 0137](0137-a-terminal-can-be-read-typed-into-and-waited-on-without-attaching.md)
gave it a second reader: a wait names the events that end it, and
`FirstHarnessHookSince` matches them **in the query**, so a turn's end is found
behind any number of tool-call hooks.

That made the event name a contract rather than a label, and nothing owns it.
The CLI says so in as many words — the terminal routes are raw, and "what a
caller builds from them … needs to know which hooks end a turn for which
harness, and belongs to a tool above this one." That tool has nowhere to look
the answer up.

Today the answer happens to be the same for both harnesses that publish hooks:

- **Claude Code 2.1.278** defines 33 hook events.
- **Codex 0.155.1** defines 12, and adopted Claude Code's hook vocabulary
  wholesale: 11 of them are spelled identically, down to the JSON shape of a
  hook's config entry.

ADR 0137 relied on that agreement — "Claude Code and Codex both emit `Stop`
when a turn ends" — and it has held. But it is a fact about how Codex was
built, not a contract either vendor owes Discobox, and it has already started
to give:

- **`Interrupt`** is Codex's, and Claude Code has no counterpart to it.
- **opencode 1** ([ADR 0127](0127-the-opencode-harness-runs-opencode-1.md))
  publishes no command hooks at all. Its lifecycle surface is JS plugin hooks
  and an event bus named in `dot.lower.case`, where a turn ends at
  `session.idle` and a tool call is `tool.execute.before` / `.after`. Of its 28
  documented events, roughly six have an unarguable Claude Code counterpart and
  perhaps ten if judgment calls are allowed; **the rest have no Claude Code word
  at all** — `todo.updated`, `session.diff`, `permission.replied`,
  `lsp.updated`, `server.connected`, the three `tui.*`, and more.
- A **third-party harness image** may publish anything. The image contract
  ([ADR 0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md))
  admits images Discobox did not build, and the generic publisher takes
  whatever `--event` it is given.

So the shared vocabulary cannot be the only name a hook has. Most events a
future harness emits will have no counterpart in it.

## Decision

**A hook keeps `event`, the name its harness emitted, exactly as it emitted it.
It gains `canonicalEvent`, the Claude Code name for the same thing, which is
empty when Claude Code has no name for it.**

### 1. Claude Code's vocabulary is the canonical form

Not a neutral one invented for the purpose. Two of the three harnesses already
speak it, it is documented and versioned by someone other than us, and every
name in it is a name some CLI actually emits.

The cost is recorded plainly: it ties the canonical form to one vendor's
naming, and an event Claude Code has no word for has no canonical name. §3 is
what keeps that from becoming a lie.

### 2. `event` is unchanged, and is what the harness said

Always populated, never mapped, never rewritten. It means exactly what it means
today, so nothing already reading it changes behaviour, and
`discobox admin audit hooks` goes on reporting what the harness did rather than
what Discobox made of it.

### 3. `canonicalEvent` is empty when there is no canonical name

Not a copy of `event`, not a sentinel, not a guess.

Copying the harness's own name in would be the tempting move and is the one
real trap here. It would mean `canonicalEvent` no longer holds "a Claude Code
name" but "a Claude Code name, or whatever some vendor called it," with nothing
in the row saying which. Stored hooks would then squat Claude Code's namespace
on its behalf: when Claude Code ships an event for a concept Codex or opencode
named first, either it picks a different spelling and one concept now has two
canonical names, or it picks the same spelling with different semantics and old
rows silently mean something else. Neither is recoverable from stored data.

Emptiness costs nothing, because `event` always carries a name. It is the
honest statement that Claude Code has no word for this yet, and the day it does,
one mapping entry starts filling the field with no stored row renamed.

### 4. The mapping is applied when the hook is recorded

When the row is written, in the store, and nowhere else — not in the
publisher, not in the collector that receives the hook, and not at read time.
ADR 0137's wait matches events in SQL, so the canonical name has to be a
column: a mapping applied when a hook is read either cannot be matched in the
query at all, or forces the wait to load every hook for a terminal and filter
it in Go.

The store rather than the collector because it is the one choke point every
writer passes through, and because the backfill in §7 needs the same mapping —
putting it anywhere else leaves two copies of one derivation, and a row whose
two names disagree becomes expressible. What a caller supplies in that field is
overwritten rather than trusted: it is an answer the store gives, not an input
it takes.

Recording it also means a hook keeps the reading that was in force when it was
observed, which is the property an audit log wants.

### 5. The table lives in the root `harness` package

Provider strings are harness driver IDs, so the mapping is harness knowledge,
and `sandbox-agent` already imports `harness`.

Not in the harness images. A per-image mapping would be version-coupled to each
CLI, duplicated across three Dockerfiles, and unavailable to an image Discobox
did not build — and images are the last place we want a table that changes when
*our* canonical form changes.

### 6. A reader matches on either name

- `HarnessHookLog` gains `canonicalEvent`, omitted when empty.
- A wait's `hookEvents` and `audit hooks --event` match **either** column, so
  `Stop` finds a turn's end on every harness that has one, and `Interrupt`
  finds Codex's on the harness that does. One flag, not two: a canonical name
  and a harness's own name cannot realistically collide on different concepts,
  and a caller that has to know which column its name lives in is back to
  knowing which harness it is talking to.

### 7. Existing rows are filled, not rewritten

`AutoMigrate` adds `canonical_event` empty, and a backfill applies the mapping
to rows already stored. Purely additive: `event` is not touched, so no stored
row changes meaning and nothing reading it today is affected. The backfill
exists so a query by canonical name finds hooks recorded before the column did.

## Consequences

- **On landing, the mapping is an identity map with one hole.** All 11 events
  Codex shares with Claude Code are spelled identically, so `canonicalEvent`
  equals `event` for them; Codex's `Interrupt` is the only recorded event that
  gets an empty one. Nothing is renamed anywhere.
- Its value is what lets opencode be wired up without teaching every caller a
  second vocabulary, and what keeps a third-party image's events readable and
  waitable. ADR 0147 lands the first of those beside this, so the table ships
  with real translations — `session.idle` to `Stop` among them — and not only
  the identity entries Codex's spelling happens to give it.
- **Revisit if opencode hook capture is abandoned**: `canonicalEvent` would
  then be an identity copy of `event` with no durable owner, and should be
  collapsed back out.
- The audit trail is stronger than before this ADR, not weaker: `event` is now
  documented as never rewritten, which was previously only true by accident.

## Alternatives rejected

1. **A neutral vocabulary** — `agent.turn.ended` and friends, owned by
   Discobox. Rejected: it is a third spelling that no CLI emits, so every
   reader learns a name they will never see in a harness's own documentation,
   and every mapping gains a hop. The only thing gained is vendor-neutrality in
   the name itself, which is cosmetic — the semantics are Claude Code's either
   way, because that is the vocabulary the mapping was built against.
2. **One field, rewritten in place.** Rejected: it destroys the audit trail.
   `discobox admin audit hooks` exists to report what the harness did, and a
   row reading `Stop` when the CLI emitted `session.idle` is a false statement
   about an observation, not a normalization.
3. **`event` holds the canonical name, and the harness's name moves to a new
   field.** Rejected on three counts: it changes the meaning of the one field
   everything already reads, it forces every stored row to be rewritten rather
   than filled, and — because most events have no canonical name — it leaves
   the field that looks primary empty most of the time. Emptiness belongs in
   the derived field, not the observed one.
4. **Namespacing un-canonical names into one field**, `codex-cli:Interrupt`.
   Rejected: it is collision-proof, but it loses the harness's own name
   whenever a mapping *does* apply — opencode's `session.idle` would be stored
   as `Stop` and the fact that the CLI said `session.idle` would be gone.
5. **Mapping at read time, in the server or the CLI.** Rejected by ADR 0137's
   wait, which matches in SQL; see §4.
6. **Mapping in each harness image**, with `hooks.json` publishing both names.
   Rejected: it triples the table, version-couples it to each CLI, and leaves
   third-party images with no way to participate.
7. **Waiting until opencode needs it.** Considered, and in the event not taken:
   opencode hook capture was wanted immediately and landed alongside this, as
   [ADR 0147](0147-opencode-publishes-its-lifecycle-through-an-image-owned-plugin.md).
   The sequencing worry — a database migration and a new image-owned plugin in
   one change — proved small enough to hold together: the column is additive
   and stamped, the backfill is covered by tests, and the plugin touches
   neither. What that costs is the clean split, and what it buys is that the
   mapping table lands with real translations in it rather than as an identity
   map nothing exercises.
