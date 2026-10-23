# 26-10-20-596 — The console keeps one memory per machine, and the last source leads

- **Status**: Accepted (§3's `-C` half superseded by [26-10-23-600](26-10-23-600-the-cli-is-spelled-one-way-run-takes-its-source-and-only-commits-go-in.md): the console takes no source; the `--server` half stands)
- **Date**: 2026-10-20
- **Supersedes**: [26-09-30-188](26-09-30-188-the-consoles-filters-open-on-everything-and-are-kept-per-directory.md)
  §2 (filters kept per launch directory), §3's "a saved view outranks
  both", §4's "the answer narrows the folder — which is then what that
  directory opens on", and its rejection of one global view and of the
  flags always winning. Its §1 (a view never narrowed is everything) and
  §4's question (a create asks where to cut from) stand. Narrows
  [0111](0111-the-origin-is-the-client-and-its-key-names-where-the-source-came-from.md)
  §2's "the launcher's own directory — the per-folder prompt draft, and the
  default source for a new discobox": the draft is no longer per folder, and
  the default source is the last create's.

## Context

The console kept its unsent prompt and its header filters per directory the
window was opened in, and remembered the create card's last answer only for
as long as the window was open. In practice the console is one place a person
keeps an eye on everything they run, opened from wherever the terminal
happens to be: the per-directory memory brought back a different prompt and
view depending on the `cd`, and every new window asked again where to cut
from, opening on whatever directory it ran in rather than on the source just
used.

## Decision

1. **One memory per machine.** The unsent prompt, the header's filters and the
   last create's source are kept once on this machine, in the CLI's state
   directory (`console.json`), whatever directory a window is opened in.
2. **The last source leads.** The source a create card was last answered with
   is saved on every answer. The next window — and the next create in this
   one — offers it first on the Source row and the card, and opens the card
   on it whatever folder the header is on, until the header or the row is
   moved to name another place. `discobox new` without `-C` still opens its
   card on the directory it ran in.
3. **The command line wins.** `-C` and `--server` narrow the view this window
   opens on, and `-C` is where its card opens, over what was saved. They are
   not saved unless the filters then move. With one view for every directory,
   a saved folder is no answer to a `-C` typed now.

## Alternatives rejected

- **Keep the filters and the draft per directory (26-09-30-188 §2).** That ADR
  rejected one global view as bleed between checkouts; in use, the bleed is the
  point: the console is one window onto everything, and the directory it was
  opened from is incidental.
- **Key the memory per project.** Closer to where sources come from, but a
  person switching projects is still the same person in the middle of the
  same thought; nothing asked for per-project memory.
- **Open the card on the header's folder when it is on one.** Every window opens
  on the saved folder, so that would put the card on the header nearly every
  time and the last answer almost never.

## Consequences

- `prompt-drafts.json` and `console-views.json` are no longer read; what they
  held is not carried over, as convenience state that a first window rebuilds.
  They are left where they are, private to the user like the rest of the
  state directory, rather than deleted by code that would have to stay in
  the CLI for good; `discobox admin uninstall` removes them with the directory.
- Two windows open at once share one memory: the last to write a field wins
  it. A window closing with a prompt in its composer writes it again, so the
  prompt in the window closed last is the one that comes back.
- A source saved as the last answer stays offered even once nothing in the
  listing was cut from it, and a directory that has since gone fails the
  create that names it, as a path typed by hand would.
