# Architecture Decision Records

An ADR records a decision, the alternatives rejected, and why — at the time it
was made.

## When to write one

Only when either is true:

- A plausible alternative was rejected for a non-obvious reason.
- Something was deferred with a specific condition for revisiting it.

Otherwise skip the ADR and update the relevant `DESIGN.md`. Most changes do not
need one.

## Rules

- **Immutable.** Never edit an accepted ADR. Superseded by a new one that links
  back; mark the old one `Superseded by <id>`. An ADR's ID is as immutable as
  its text — never renumber one.
- **Outside the drill-down hierarchy.** ADRs live here, never next to code.
  `DESIGN.md`/`REVIEW.md` are read root-down by agents on every task; ADRs are
  not, and must not be, because they are history rather than current state.
- **Not a substitute for `DESIGN.md`.** When the work lands, update the live
  design docs to describe what now exists. The ADR keeps the "why we didn't";
  `DESIGN.md` keeps the "what is".

## Status lifecycle

`Proposed` → `Accepted` → (`Superseded by <id>`)

Use `Rejected` for decisions considered and declined; keep the file.

## Identifiers

An ADR's ID is `YY-MM-DD-RRR` — the date it was written, plus three random
decimal digits ([ADR 26-09-24-458](26-09-24-458-adr-ids-are-a-date-and-a-random-suffix.md)).
It is generated, never allocated from a sequence, so two branches in flight
cannot claim the same one.

```bash
python3 -c "import secrets; print(f'{secrets.randbelow(1000):03d}')"
```

- Filename `YY-MM-DD-RRR-<slug>.md`, heading `# YY-MM-DD-RRR — Title`, cited in
  prose and code comments as `ADR 26-09-24-458` or `ADR 26-09-24-458 §3`.
- Under the heading, before the first `##` section, a `- **Status**: …` line
  (continued on indented lines) saying where the ADR stands and what it
  supersedes, amends or narrows. The index is read from that heading and that
  line, and `go tool task adr:check` (run by CI) fails an ADR without them.
- The date is fixed when the file is created. Where it and the `Date` header
  disagree — an ADR written before a long review — the header is authoritative.
- If two branches somehow draw the same ID, **re-roll the suffix** on one of
  them. Nothing outside that branch cites it yet, and no value is the "correct"
  one to fight over.
- ADRs `0001`–`0149` predate this scheme and keep their four-digit numbers,
  which are cited across the tree. Both forms are permanent; the old ones sort
  first.

## Workflow

Nygard-style ADRs (see adr.github.io) combined with current-state design docs:

1. **Decide first.** Draft the ADR as `Proposed` and land it on its own,
   before implementation. Flipping it to `Accepted` is the decision gate —
   with review, the ADR's PR is where the review happens, and merge means
   accepted. The status lives in the ADR's own `Status` line, and nowhere
   else.
2. **The accepted ADR is the spec** while implementation is in flight.
   `DESIGN.md` never describes in-progress or planned work — only what exists.
3. **DESIGN.md rides the code.** Each change that alters the architecture
   updates the affected `DESIGN.md` files in the same change, not as a
   follow-up pass. When the ADR's work fully lands, the live design docs
   describe the new state and the ADR keeps only the why.
4. **Plans are not documents.** Sequencing, checklists, and rollout order live
   in the task or branch driving the work; they are meant to go stale.
5. **Wrong decisions:** an `Accepted` ADR may still be amended while nothing
   has shipped against it. Once implementation has landed, supersede instead.

## Index

The index is printed from the ADR files, never kept in a file:

```bash
go tool task adr:index                       # every ADR: ID, title, status
go tool task adr:index -- -status proposed   # only those whose status starts so
```

It lists both ID forms in order, the four-digit numbers (up to `0149`, assigned
before the date scheme) first, and takes each title from the ADR's heading and
each status from its `Status` line. So an ADR's PR adds only its own file, and
accepting, superseding or amending one edits only the ADR files concerned. Never
add an index table or list of ADRs to this README.

A committed index, edited by hand or generated, is a line every ADR PR adds to
one file, so each ADR PR that merges leaves every other open one conflicting.
Git's `merge=union` attribute would not prevent that: GitHub's mergeability
check and its merge queue apply no merge drivers.
