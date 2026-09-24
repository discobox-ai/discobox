# 26-09-24-458 — ADR IDs are a date and a random suffix

- **Status**: Accepted
- **Date**: 2026-09-24

## Context

An ADR's identifier was the next free number in a dense global sequence, chosen
when the file was written. Choosing it requires knowing every number already
taken, and a branch only knows the ones on `main` as of the day it started. This
repository writes the ADR and implements it in the same branch — step 1 of the
workflow below, "land the ADR on its own first", is the exception rather than the
rule — so two branches in flight for a week routinely both claim `0100`.

Git does not report that as a conflict. `0100-a.md` and `0100-b.md` are
different paths and merge cleanly. The only textual conflict is the row each
branch appends to the index table, and resolving that row by hand leaves the
duplicate number in place. The failure is therefore silent, and it has happened
four times: `0071`, `0094`, `0095` and `0096` each name two ADRs in `main`,
landed a day to four days apart. 111 files outside `docs/` cite one of those
four numbers with no way to tell which ADR is meant, and two of the eight
ADRs involved never reached the index at all.

Renumbering after the fact is the expensive part, and it is what made a
collision worth avoiding rather than worth fixing:

- The number is the citation key in 681 files outside `docs/`, as `ADR NNNN`
  and `ADR NNNN §N`, alongside 578 links between ADRs.
- A bare four-digit number is not a distinctive string. Rewriting `ADR 0094` to
  `ADR 0150` across the tree cannot be done safely by search and replace; it has
  previously touched 30 unrelated files.
- Both branches want the same value, because with a counter exactly one value is
  correct. Whichever branch yields must renumber its file, its inbound links,
  and every citation already written against it.

The two requirements are in tension only because of *when* the number is picked.
An ID must be immutable after merge, because everything cites it. Nothing
requires it to be unique before merge — and nothing requires it to be dense.

## Decision

**A new ADR's identifier is `YY-MM-DD-RRR`: the date it was written, plus three
random decimal digits.** It is generated, not allocated, so there is nothing for
two branches to agree on and no sequence to race for.

- Filename: `YY-MM-DD-RRR-<slug>.md`, as `26-09-24-458-adr-ids-are-a-date-and-a-random-suffix.md`.
- Heading: `# YY-MM-DD-RRR — Title`.
- Citation: `ADR 26-09-24-458`, with sections as `ADR 26-09-24-458 §3`.

The date is the date of writing and is fixed when the file is created. The
`Date` header bullet stays, and is authoritative where the two disagree — an ADR
written before a long review keeps the ID it was born with.

### Existing IDs are not migrated, but a duplicated one was never an ID

`0001` through `0149` keep their numbers. An ADR's ID is as immutable as its
text, 681 files cite the old form, and a mechanical rewrite of a bare
four-digit number is exactly the unsafe operation named above. The corpus holds
both forms permanently.

A *duplicated* number is the exception, because it was never a valid ID: two
files cannot both be `0094`, and no citation of it resolves. Resolving a
collision is therefore not a migration, and the resolution is to give one of the
pair a new `YY-MM-DD-RRR` ID stamped with its own date. The four existing
collisions were resolved this way, each time moving the less-cited side:

| Was | Now | Citations moved |
| --- | --- | --- |
| `0071` a tool session is an exec the launcher labeled | `26-08-27-302` | 16 |
| `0094` an image declares services… | `26-09-05-409` | 15 |
| `0095` an attached client pushes… | `26-09-05-008` | 24 |
| `0096` a source keeps its host path… | `26-09-09-044` | 12 |

That the new IDs are distinctive strings is what made the rewrite safe, and it
is why the surviving number in each pair was left alone.

### Three digits, because ADRs land in batches

The suffix only has to separate ADRs written on the same day, and this
repository writes them in bursts — git records days with 6, 8 and 14 new ADRs.
Two digits is not enough for that shape:

| same-day ADRs | 2 digits | 3 digits | 4 digits |
| --- | --- | --- | --- |
| 2 | 1.0% | 0.1% | 0.01% |
| 6 | 14.2% | 1.5% | 0.15% |
| 14 | 61.5% | 8.7% | 0.91% |

The true figure is lower, because the count that matters is concurrent
*branches*, not ADRs per day: `0095` and `0096` were added by one commit, written
together and never racing. Three digits puts the realistic case under a percent
for one character more than two.

### A collision is re-rolled, not renumbered

If two branches do draw the same ID, no value is the correct one, so neither
branch owes the other anything: re-roll the suffix on one of them. That is a
rename of a file nothing outside its own branch cites yet, with none of the
cascade a counter collision forces. Generate one with:

```bash
python3 -c "import secrets; print(f'{secrets.randbelow(1000):03d}')"
```

## Alternatives rejected

**Keep the counter and fetch `origin` before numbering.** The previous
mitigation. Two branches that both fetch on Monday both see `0149` as the
highest and both take `0150`; fetching narrows the window without closing it,
and it is what was in place while all four existing duplicates landed. Rejected:
it asks every author to win a race rather than removing the race.

**Allocate the number at merge.** Write the ADR as `draft-<slug>.md`, cite
`ADR <slug>` in code, and run a tool before merging that renames the file to the
next free number and rewrites the slug citations — which is safe, because a slug
is a distinctive string. This does fully close the race, since numbers are then
drawn serially from merged `main`. Rejected: it needs a tool, a mandatory
pre-merge step, a CI check that the step was not skipped, and a window in which
committed code cites an ADR by a name it will not keep — all to preserve a
four-digit citation whose only advantage over `26-09-24-458` is six characters.

**A sequential suffix within the day** (`-01`, `-02`). Rejected: it puts the
race back exactly where the collisions actually happen, since several ADRs on
one day is the common case here, not the rare one.

**A base36 or hash suffix.** 1296 values against 1000 buys nothing at this
rate, and a mixed alphanumeric suffix is harder to read, say aloud, and
transcribe than three digits.

**Drop identifiers; the slug is the ID.** Collision-proof and needs no tooling,
but `ADR a-source-keeps-its-host-path-only-where-a-sandbox-may-hold-it §5` is not
a citation anyone will write in a code comment. The compact key is the thing
being protected.

**Generate the index table instead.** The index row is where the collision
surfaces as a git conflict, so generating the table from each ADR's own header
would remove the conflict — but it would leave the duplicate IDs it currently
exposes, silently. Rejected as a replacement for this decision; see Deferred.

## Consequences

- Two branches can no longer claim the same ID by working from the same view of
  `main`. Nothing needs fetching, reserving, or checking first.
- The index row is still a hand-appended line at the end of one table, so two
  branches still conflict there. The resolution becomes trivial — keep both
  lines — because no renumbering hides behind it.
- The corpus holds two citation shapes permanently: `ADR 0121 §2` for decisions
  made before this ADR, `ADR 26-09-24-458 §3` after. Both are unambiguous; only
  the old one is short.
- New IDs are safely greppable. `26-09-24-458` does not occur in unrelated
  prose, so a reference to one can be found and rewritten mechanically, which a
  bare four-digit number never could be.
- An ADR's age is legible from its ID. Its position in the sequence is not, and
  "the ADR after 0149" stops being a meaningful phrase. A plain directory listing
  no longer reads chronologically once both forms are present; the index table
  is where that order lives.
- A collision remains possible and still merges silently, because two distinct
  filenames never conflict. It is rarer by roughly a thousandfold and cheap to
  fix, but nothing detects it.

**Deferred.** Two follow-ups, neither blocking:

- *A check for duplicate IDs*, an ADR missing from the index, and links to
  ADR files that do not exist. Revisit when the first re-rolled collision is
  missed, or alongside the cleanup below — this is the only thing that would
  have caught the four existing duplicates on the day they landed.
- *Generating the index table*, which would remove the last conflict surface.
  Revisit if appending that row is still annoying now that a conflict there
  costs nothing but keeping both lines.

The four duplicated numbers were resolved as part of this change, per §"Existing
IDs are not migrated". Disambiguating their citations was tractable because the
authors had already been qualifying them in prose — `ADR 0071, resource
accounting` against `ADR 0071 on tool sessions` — which is itself evidence the
collisions had been costing readers something all along.
