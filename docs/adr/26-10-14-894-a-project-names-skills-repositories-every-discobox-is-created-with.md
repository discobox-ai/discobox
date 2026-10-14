# 26-10-14-894 — A project names skills repositories every discobox is created with

- **Status**: Accepted
- **Date**: 2026-10-14
- **Relates to**: [26-10-14-294](26-10-14-294-a-skills-repository-is-a-source-its-skills-are-linked-from.md),
  [26-10-09-395](26-10-09-395-a-sandbox-may-be-created-with-skills.md),
  [26-10-09-389](26-10-09-389-the-cli-reads-one-schema-checked-configuration-file.md),
  [26-10-10-545](26-10-10-545-git-reaches-a-pool-only-from-a-client-and-is-cached-there-per-remote.md),
  [26-09-24-630](26-09-24-630-a-discobox-delivers-the-source-of-the-discoboxes-it-creates.md).

## Context

`--skills URL[@REF][//PATH]` brings a skills repository into one discobox
(ADR 26-10-14-294), and `client.yaml`'s `new.skills` makes that the default on
one machine (ADR 26-10-09-389). A team that keeps its skills in a repository
wants every discobox of a project to carry them, whoever creates it and from
whichever machine, without each person configuring their own client. Nothing
on the server holds that today, and nothing manages `new.skills` short of
editing the file by hand.

A skills source is a Git source. Its checkout is pinned to a commit by the
client that creates the discobox, and that client delivers it: the server
never runs Git (ADR 26-10-10-545 §1). A discobox that creates discoboxes is
that client for them (ADR 26-09-24-630).

## Decision

### 1. A project holds an ordered list of skills repositories

A project gains skills sources: `/projects/{projectId}/skills-sources`, listed,
added to and removed from, one row per entry, kept in the order they were
added. Each entry is a remote Git `url`, an optional `ref`, and the
`directory` inside the repository that holds the skills (`.`, the root, when
none is named): the three parts of a `--skills URL[@REF][//PATH]`.

The server refuses an entry whose `url` is not a remote Git URL, whose
`directory` `sandboxconfig.ValidateSkillsDirectory` refuses, that repeats an
entry already there, or that names a repository the list already names at a
different ref: a discobox checks out one ref of a repository (ADR 26-10-14-294
§1), so such a list would fail every create.

An entry is written by one member and run by every other member's client, so
the `url` carries no credential — every client and every discobox of the
project reads it, and an http(s) URL takes no user, since a token is as often
the user as the password — and neither it nor the `ref` may start with `-`,
which Git would read as an option. The `ref` is a name Git takes for a ref.
These rules are `sandboxconfig.ValidateSkillsRepository`, and a client that
copies an entry into a create holds it to them again before Git runs on it,
since what it reads is only what the server answered.

Only repositories. A directory is on someone's machine, and is not the
project's to name.

A discobox may list its own project's skills sources, so a discobox creating
discoboxes reads them as a person's client does. Adding and removing stay a
member's.

### 2. The creating client copies them into the request, first

`discobox new`, the console's create, and `admin box create` read the
project's skills sources when they build a create request, and add each one
as a `--skills` value would be added, ahead of the `--skills` values the
command takes from its flags or from `new.skills`. A later value wins on a
skill's name (ADR 26-10-14-294 §4), so a person's own skills win over the
project's.

What the request carries is a copy, made at create. The discobox's skills
sources are its manifest's, as for any `--skills URL`; adding or removing a
project entry later changes no discobox that exists.

`--no-project-skills`, and the console create card's Project skills row,
leave the project's out of one create. `--skills` replaces `new.skills` as
before, and never the project's. A `--skills` naming a repository the project
names at another ref is refused, as two `--skills` doing so are, and the
refusal names `--no-project-skills`.

A server that predates this answers the list with 404, which the client reads
as an empty list.

### 3. `discobox skills` manages both lists, by what each value is

`discobox skills` lists the project's skills sources and the client's
`new.skills`, each marked with where it lives. `discobox skills add VALUE`
adds a Git URL to the project and a directory to `new.skills`, reading the
value as `--skills` reads it. `discobox skills rm` removes either. The
console gains a skills screen, beside the secrets screen, that does the same.

Writing `new.skills` is the first time the CLI writes `client.yaml`. It edits
the file in place, keeping what else is in it and its comments, and creates
it at the default path when there is none. A directory is written absolute,
since a relative one in the file is relative to the file (ADR 26-10-09-389
§2). With `DISCOBOX_CLIENT_CONFIG_FILE` set empty, which reads no file, there
is nowhere to write, and adding a directory is refused.

## Alternatives rejected

- **The server merges the project's skills sources into each create.** It
  would reach every client, including an API client that never reads the
  list. But a source's checkout is a commit the client pins, and the source is
  delivered by the client that created the discobox; the server would be
  adding a source it cannot pin, for a client that did not ask to deliver it.
- **Keep the list on the project row, as one JSON column.** It holds the order
  for free, but every add and remove becomes a read-modify-write of the whole
  list, and two people adding at once lose one entry. A row per entry is how
  secrets and SSH keys are kept.
- **The discobox reads the project's list when it starts.** A list read live
  would need a delivery path for a source added after create, and would make
  a discobox's skills change under it. A discobox's skills are fixed at create
  (ADR 26-10-09-395 §2).
- **Project entries may be directories, read by each creating client.** A path
  means something on one machine; on another it names nothing or something
  else.
- **`--skills` replaces the project's list, as it replaces `new.skills`.** A
  person naming one extra skill would silently drop the project's, which is
  the default the project exists to set. Leaving them out is an explicit
  `--no-project-skills`.

## Consequences

- The server gains a `project_skills_sources` table and the three routes, and
  the sandbox role reads the list.
- Every create that reads the list makes one more request before it resolves
  its sources.
- `client.yaml` is written by the CLI; a hand-edited file keeps its comments.

## Deferred

- **Reordering the project's list.** Entries are kept in the order added; to
  move one, remove and add it. Revisit when a project has enough entries that
  order between them matters often.
