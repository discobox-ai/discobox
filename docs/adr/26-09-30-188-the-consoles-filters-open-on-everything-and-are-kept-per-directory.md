# 26-09-30-188 — The console's filters open on everything, and are kept per directory

- **Status**: Accepted (§1's sectioning by server superseded by [ADR 26-10-14-585](26-10-14-585-a-console-folder-is-a-github-repository-and-servers-are-not-sections.md))
- **Date**: 2026-09-30
- **Supersedes**: [0111](0111-the-origin-is-the-client-and-its-key-names-where-the-source-came-from.md)
  §3's folder the launcher's header opens on — the window's own, and
  `discobox -C <url>` opening on that URL unconditionally. The header still
  filters by the origin key, labels entries as 0111 says, and `ls` is
  unchanged.

## Context

ADR 0111 §3 has the console's header open on the window's own folder: the
same two keys `discobox ls` sends from here. With several servers registered
([ADR 0116](0116-a-discobox-address-names-a-server-and-a-discobox.md) §4) and discoboxes filed under many folders,
that opening view hides most of what the person runs. Seeing the rest was
two presses away every time the window opened, and the window forgot the
choice on the way out.

## Decision

1. **A directory never narrowed opens on everything**: every server, every
   folder, every tag. On `all folders` the list is sectioned by folder, and by
   folder and server (`<folder> on <server>`) while it shows every server; a
   filter narrowed to one value drops out of the sections.
2. **The three filters are kept per launch directory**, on this machine, in the
   CLI's state directory beside the prompt drafts. The next window opened in
   that directory opens on them; any other directory still opens on
   everything. Returning to everything forgets the entry.
3. **`--server` and `-C` narrow only the default.** In a directory with nothing
   saved, an explicitly typed `--server` opens on that server, and `-C` on the
   folder it names (resolved to its absolute project root). Neither is saved
   until a filter is changed, and a saved view outranks both.
   `DISCOBOX_SERVER` does not count: it is set inside every discobox.
4. **A create on `all folders` still asks where to cut from**, as before. The
   first prompt in a fresh directory therefore asks, and the answer narrows the
   folder — which is then what that directory opens on.

## Alternatives rejected

- **Keep opening on the window's own folder (0111 §3).** It agrees with `ls`,
  but `ls` is one command's answer and the console is where a person keeps an
  eye on everything they run; the narrow view was the one they kept undoing.
- **Remember one view globally.** A choice made in one checkout would reach
  every other, which is the bleed the per-directory prompt draft already
  avoids.
- **Let `--server`/`-C` always win over a saved view.** A flag typed out of
  habit would then quietly discard what the directory was left on; the flags
  answer "what should a directory with no history show", nothing more.
- **Cut from the launch directory on `all folders` without asking.** Declined
  by the user: the question stays, and answering it is what narrows the view.

## Consequences

- The console no longer opens on what `discobox ls` lists in the same
  directory, unless it was left there or `-C` was typed.
- `console-views.json` is new best-effort state: a missing or corrupt file
  opens on everything.
