# 0136 — A sandbox's meta lives in the sandbox, and the server caches it

- **Status**: Accepted
- **Date**: 2026-09-18

## Context

A sandbox needs a description of what it is for and tags that label it, and
the agent working inside it is often the one best placed to write them: it
knows what it was asked to do and where the work stands. People outside need to
read them too, in `discobox ls` and the launcher, including for a sandbox that
is stopped, and to filter a listing by tag.

Until now a sandbox had one such field, `config.description`, set by the
control plane at create and never seen inside the sandbox.

## Decision

1. **The sandbox is the system of record.** Its description and tags live in
   `~/.discobox/meta.json` under the sandbox user's home, a JSON object with
   exactly two fields, `description` and `tags` (key to value, an empty value a
   plain label), owned by the sandbox user so the agent edits it directly. The
   file's shape and its rules are the root `sandboxmeta` package's, shared by
   the sandbox agent, the server and the CLI.
2. **The status report carries it.** The sandbox agent reads the file on every
   status poll and reports it as `meta`, or reports `metaError` and no `meta`
   when the file does not read. The server records the reported meta on the
   sandbox row — the `description` column, a `tags` column and
   `meta_observed_at` — as a cache for reading a stopped sandbox and filtering
   listings. A report without `meta` leaves the cache alone: absent is "not
   known", not "cleared".
3. **An API write is carried into the sandbox.** `PATCH
   /projects/{p}/sandboxes/{s}/meta` forwards the change to the sandbox agent's
   `PATCH .../meta`, which applies it to the file (read, change, rename over)
   and answers with what the file then holds; the server records that answer
   and returns the sandbox. A sandbox that cannot take the change fails the
   request; nothing is recorded that the sandbox does not hold. The write is
   gated as `exec:write`, which already allows any write into the sandbox.
4. **Newer wins, on the sandbox's clock.** Both paths stamp what they record
   with the sandbox's own time, and the server replaces its copy only with a
   newer observation, so a poll that read the file before a write cannot undo
   the write by arriving after it.
5. **The server-owned description is retired.** `config.description` is no
   longer returned; the create request's `description` is only a seed, carried
   through `sandbox.json` and written into the meta file by the sandbox agent
   when there is no file yet. The existing `description` column becomes the
   cache of the sandbox's description, so descriptions recorded before this
   change read as the seed until the sandbox first reports.

6. **Export carries both.** The meta file is under the home, a data volume,
   so it travels in the archive's `data` tree and is restored with it. The
   server's copy of the description and tags travels in the export spec, so an
   imported sandbox lists and filters by them before it has reported; its
   observation time does not, because it is the source host's clock, and the
   imported copy is left unobserved for the first report to replace. The spec
   fields are optional and the format version is unchanged: an older reader
   loses only the copy.

## Alternatives rejected

**The server as the system of record, pushed into the sandbox.** Rejected: the
agent in the sandbox would need an API and a credential to change what is
about its own box, and an edit made in the file would be overwritten by the
next push. Making the file authoritative lets the agent use the tool it already
has — writing a file — and needs nothing new from the proxy or the credential
model.

**One file per field (`meta/tags.json`, `meta/description`).** Rejected in
favor of one object: one read, one atomic replace, and one validity answer per
status report, at the cost of the description being a JSON string.

**Filtering listings in SQL.** Rejected for now: the tags are a JSON column, and
a listing is one project's sandboxes. The server filters in Go after the query;
revisit if a project's sandbox count makes that read expensive.

## Consequences

- Meta changes reach listings on the status poll's cadence (15 s) when made in
  the sandbox, and immediately when made through the API.
- A stopped sandbox is started to take an API write; a sandbox that is gone or
  unreachable cannot have its meta changed.
- A file the agent writes invalid is reported, not recorded, and blocks API
  writes (409) until it is fixed, rather than being overwritten.
- Deleting the file lets the next boot reseed it with whatever description
  `sandbox.json` carries; writing `{}` is how meta is cleared.
- Newer-wins trusts the sandbox's clock to move forward. A sandbox whose clock
  steps back — a restored VM, a corrected host — has its meta reports ignored
  until it passes the last time recorded; revisit if a provider is found to do
  that routinely.
- A sandbox recreated on a pool agent older than this change gets no seed in
  its `sandbox.json`, and its first report records an empty description. Nothing
  displayed the old server-owned description, so nothing visible is lost; the
  pool agent and sandbox images are released as one set, which keeps the
  window small.
