# 0109 — The origin is the client, and its key names where the source came from

- **Status**: Proposed
- **Date**: 2026-09-11
- **Supersedes**: [0001](0001-sandbox-origin-and-remote-source-push.md) §1's
  `Origin.ProjectPath` and its derivation of `OriginKey`. The origin records
  the client and nothing about a directory, and the key becomes the host and
  where the primary source came from, or the host alone. 0001's host identity
  (§2), `ls` filtering on the key, and every decision that reads
  `Origin.HostID` (§§3–4) stand.

## Context

`discobox ls` and the sandbox picker list the discoboxes whose origin key is
`originkey.Of(hostID, projectPath)` for `-C` (`listProjectSandboxes`). The
launcher's header filter compares `Origin.ProjectPath` alone
(`sandboxList.rows`), so two machines holding the same path share one entry.

What the key is for is "the discoboxes whose work comes back here": the ones
whose source was delivered from this place, on this machine, and so the ones
`apply` writes back into it. For a local source, the origin gives exactly that,
by coincidence. `-C` resolves to the same repository root (or, outside a
repository, the same directory) as `Origin.ProjectPath`, as
`GitSource.LocalDirectory`, and as `GitSource.Root()` — the directory
`resolveApplyHostDir` writes into. Where there is no local source, the origin
files the discobox under a directory it has nothing to do with:

```
~/scratch$ discobox -C https://github.com/acme/api -p '…'
```

lists under `~/scratch`, because `ResolveOrigin` falls back to the working
directory — a directory the sandbox took nothing from and nothing will be
applied into. The launcher's header then reads `~/scratch`, and `ls` in a
checkout of `acme/api` does not list it. A discobox with no source at all is
filed the same way, under whichever directory it happened to be started in.

Nothing functional reads `ProjectPath`. Bind-or-push (`sourceNeedsPush`),
`apply`'s gate, and the source-data key all read `Origin.HostID`. Its readers
are the key and four labels — `ls --all`'s `FOLDER` column, the picker's
"started in" line, the launcher's rows, and the launcher's own directory —
and for a local source every one of them reads a copy of `LocalDirectory`.

## Decision

### 1. The origin key is the host and where the primary source came from

| Primary source | The key hashes | Compared with today |
| --- | --- | --- |
| Local — a repository, one with no commits (0083), or a copied directory (0045) | the host and `LocalDirectory` | the same value |
| Remote URL | the host and the URL | was the host and the working directory |
| None (0077, `--no-source`) | the host alone | was the host and the working directory |

With a source, the key is `originkey.Of(Origin.HostID, GitSource.Root())` —
the primary source's source-data key (`sourceDataKey`). One derivation now
answers both "which pool storage is this machine's copy of this source" and
"which discoboxes are this machine's copy of this source".

Without a source, nothing was delivered from anywhere, so the discobox belongs
to no place. It was still created on this machine, and the host is what makes
it this machine's. The host-only key is a derivation of its own in
`internal/originkey`, which no host-and-path pair can produce. `originkey.Of`
goes on refusing an empty path, so an incomplete origin still cannot pass for a
key.

The host is in every key: `ls` lists this machine's discoboxes and never
another's.

The derivation moves out of `Origin.Key()` into the create path, which is the
only place holding both the origin and the source. The URL is hashed as the
create request carries it, which is the form `Root()` returns. The client
derives the key from the source it would send, not from the argument as typed,
so both sides hash the same string.

### 2. The origin records the client, and nothing about a directory

`Origin` is `hostId`, `hostname`, and `user`. `projectPath` leaves the model,
the API schema and its `required` list, `OriginToModel`, and what the CLI's
`origin.Resolve` sends. Its readers move to the source:

- `ls --all` shows a `SOURCE` column — the local path or the URL — where it
  showed `FOLDER`.
- The picker's line for a discobox names its source, and the machine when that
  is not this one.
- The launcher's rows group and label by source (§3).
- The launcher's own directory — the per-folder prompt draft, and the default
  source for a new discobox — is the CLI's own resolution of `-C`, as it
  always was underneath. It never needed the field.

The CLI's repository-root resolution (`origin.ProjectPath`) stays: it is how
`-C <dir>` becomes the path the key hashes. Only the field goes.

The removal is a clean cut. `Origin` is strict (`additionalProperties: false`)
and `projectPath` is required, so a CLI and a server on either side of this
change fail to create or list against each other. Compatibility across versions
is not maintained (0110 records that stance and when it changes), and a client
that breaks is the signal to upgrade. The CLI replaces an older local server it
starts, so the local pairing moves together.

### 3. Every listing filters on the key

- `ListSandboxes`'s `originKey` is repeatable, and matches any of the keys
  given.
- `discobox ls` and `selectSandbox`'s candidates send two keys: the one `-C`
  names, and the host's own. The first is the URL for `-C <url>`, or the
  repository root (the directory outside one) for `-C <dir>`, which is
  unchanged. The second lists this machine's sourceless discoboxes from any
  directory on it.
- The launcher's header filters by the origin key, not by path, so it agrees
  with `ls` and stops merging two machines' copies of one path. It opens on the
  same two keys. `Sandbox` exposes `originKey` read-only, so the launcher reads
  the stored key rather than deriving it again. An entry is labeled with its
  path or URL, a sourceless one as having no source, and `discobox -C <url>`
  opens the window on that URL. Discoboxes from other machines stay under
  "all folders", marked `from <host>` as now.

### 4. Existing rows are re-keyed, and keep what they recorded

A migration after `AutoMigrate` recomputes `origin_key` by §1 for every row with
an origin; it reads only the host ID and the source. Remote-sourced rows move
from the directory they were created in to their URL. Sourceless rows move to
the host key.

The stored `origin` of an existing row keeps its `projectPath` key. The column
is JSON, the model no longer declares the field, and decoding ignores it. It is
left rather than stripped, because deleting what a row recorded is not needed
for anything this decides.

## Alternatives rejected

**A new source key beside the origin key.** It would be two keys for one
question, which the launcher and `ls` already answer differently. A separate
key was only worth having if a URL grouped across machines. `ls` stays
per-machine, so it would be the origin key under another name.

**Keep `ProjectPath` as provenance.** After §1 its only readers are labels, and
the source answers each of them better: it names where the work goes back to,
not where a command was typed. What the field alone knows — the directory a
remote-sourced or sourceless create happened to run in — is read by nothing.

**Remove it in two releases**, optional first and absent later, so mismatched
CLIs and servers keep working. That is compatibility work for a guarantee not
yet offered (0110).

**Key a sourceless discobox by the directory it was started in**, as today.
Nothing was delivered from that directory and nothing will be applied into it,
so filing the discobox there is the same accident as the remote case.

**Filter on `source_root`.** It is a path with no machine, which on a shared
server merges two users' `~/src/api` — 0001's reason for moving `ls` off it.

**Resolve a local checkout to its `origin` remote.** That groups different
places — two checkouts, or a checkout and a clone, holding different unpushed
commits — and `apply` goes into exactly one of them.

**Normalize the URL in the key**, so `https://` and `git@` spellings of one
repository list together. The origin key and the source-data key would then
differ in exactly the case this changes. Two spellings already get two copies
of pool storage, and listing them apart is the same answer.

## Consequences

- Nothing moves for a local source. `ls` in a checkout lists what it listed,
  plus this machine's sourceless discoboxes.
- A remote-sourced discobox lists under its URL from any directory on the
  machine that created it. `ls` in the directory it was created in no longer
  lists it.
- A sourceless discobox lists in every `ls` on the machine that created it, and
  in no `ls` on any other.
- The origin no longer says where a create was run. For a local source the
  source's directory says it; for a remote-sourced or sourceless discobox,
  nothing does.
- A CLI and a server from either side of this change do not work together.
- The key outlives its old name: it is no longer derived from `Origin` alone.
  It keeps the name, because it still says which client a discobox belongs to,
  and the column, the query parameter, and every reader already use it.
- The help for `-C` describes the key for a URL as well as for a directory.
  `cli/DESIGN.md` (Origin and Source Delivery), the launcher's folder control
  in `cli/internal/tui/DESIGN.md`, and `resources/sandboxes/DESIGN.md` — where
  the primary's source-data key "is the same identity as the sandbox's origin
  key" becomes true of every source — change with the code.
