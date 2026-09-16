# 0123 — A discobox is exported as its spec and its durable tree, and a transfer is two of those

- **Status**: Accepted
- **Date**: 2026-09-16

## Context

A discobox lives on one server, on one pool, on one machine. There is no way to
take it anywhere: a laptop pool is replaced, a developer moves to a bigger host,
a box built on a local Docker provider needs to finish its work on a remote
server — and today the only answer is to create a new one and redo the work.

The pieces of an answer already exist and are already load-bearing:

- **Archive** (ADR 0022 §6) tears a sandbox's runtime down and keeps its data.
  What it keeps is the per-sandbox subtrees named by `layout`; what it drops —
  the container, the proxy material, the sentinels — is rebuilt by the next
  create.
- **Create against an existing tree** *is* unarchive. `prepareSandboxVolumes`
  clears the archive marker and reuses the tree as it stands, and
  `materializeGitSource` returns early for a workspace that has already been
  materialized, so nothing re-clones over work done inside the sandbox.
- **A sandbox's spec is one struct.** `SandboxManifest` is the complete answer
  to "would changing this rebuild the container?", and its `Fingerprint` is what
  the pool host compares a container against.

So the durable half of a sandbox is already a well-defined tree of bytes with a
single owner, and the control-plane half is already a single struct. What is
missing is a way to carry them off one server and onto another.

## Decision

### 1. An export is the spec and the durable tree, and nothing else

`discobox admin box export` produces a tar archive — extension `.dbox` — whose
first member is `manifest.json` and whose remaining members are the sandbox's
durable tree under `tree/`:

```
manifest.json      the spec, the image pin, the harness by name,
                   secret bindings by name, and where it came from
tree/data/         the sandbox user's home
tree/sources/      the workspace, git objects and all
tree/origins/      the bare repositories of push-delivered sources
SHA256SUMS         the SHA-256 of every file above it (§8)
```

**The line is what survives a container rebuild.** Everything in the export is
something an upgrade, a repair, or an unarchive already preserves; everything
left out is something one of those already rebuilds. That is not a coincidence
to be tidied up later — it is the whole reason the export is sound. An import is
a create against a restored tree, which is a code path that runs thousands of
times a day under a different name, rather than a second way to bring a sandbox
into being.

Four things are therefore deliberately **not** in the archive:

- **The container's writable layer.** A package installed with `apt` outside the
  mounted trees does not survive `discobox admin box upgrade` today, and it does
  not survive an export either. Rejected: `docker commit` the container and ship
  the layer. It would make the archive carry a whole image, require the
  destination to push an ad-hoc image into its pool registry before the sandbox
  could be built, and — worse — make the imported box the *only* kind of box
  whose contents do not come from its harness image. The reproducible thing to
  do with a needed package is to put it in the harness image, and an export that
  quietly rewarded not doing so would hide that.
- **The `config` and `secrets` subtrees.** Both are written in full by the create
  that follows a restore — `writeSandboxHarnessConfig` rewrites the sandbox
  document and `refreshSourcesReady` its sibling, `writeSandboxSecrets` the
  secrets one — so carrying either would move only material the destination
  regenerates. Both are also *this pool's*: sentinels minted here, and a harness
  document naming this pool's proxy. Nothing under either is written by the
  sandbox user; the config tree is owned by root and mounted read-only.
- **Proxy material and sentinels.** Per-pool trust: a CA and a client
  certificate from another pool are not merely useless on the destination, they
  are material from a different trust domain (`layout.ProxyCerts`).
- **The source's durable pool-local data** (`layout.SourceData`, mounted at
  `/.discobox/data-per-source/<slug>`). This one is the exception to the rule
  above and has to be argued rather than derived: an upgrade, a repair and an
  unarchive all preserve it, and nothing rebuilds its contents. It stays behind
  because it is **not this discobox's to move**. It is a sibling of
  `sandboxes/`, shared by every discobox in the pool that uses the same source,
  which is why deleting one never touches it; carrying it into a per-discobox
  archive would export another discobox's data, and restoring it would overwrite
  data on the far side belonging to discoboxes the archive has never heard of.
  What the imported discobox gets is a fresh, empty one under the same key —
  exactly what the first discobox for a source on any pool gets. The *key* does
  travel, which is the part that has to: it is derived from the origin, so
  `Origin` rides in the manifest (§5).

The other three follow the rebuild rule; this one does not, and the export is
the poorer for it. It is the one place where "move a discobox" is not quite
"rebuild it elsewhere".

The manifest names its harness by **name** and its secrets by **name**. Name
resolution is the only thing that can work across servers — an ID means nothing
on the destination. A harness the destination does not have is a refusal with
the name in it, not a silent substitution.

**The image is the destination harness config's, not the archive's.** The
archive records the image the box ran, but the import re-pins, exactly as a
create does. It is tempting to keep the exported digest so the box runs the
identical image, and it is wrong: the image is only half of what a harness
contributes to a container, and the other half — `RunCommand`, `Files`,
`Volumes`, `Env` — is read off the destination's harness config row when the
container is built. Pinning those two to different harnesses produces a
container that starts and a harness that does not. Most visible under
`--harness`, which names a different harness outright, and the same shear in
miniature whenever the destination's own pin has moved since the export. So the
harness config wins both halves or neither, and it wins both.

**No secret values ever enter the archive.** A binding records the env name and
the secret's name; the destination binds to its own secret of that name and
reports the bindings it could not satisfy. An export is a file people mail to
each other, and a file that reconstitutes credentials on extraction is a
credential store with no lock on it.

### 2. Export refuses a running discobox

A tar of a live tree can capture a torn git index, a half-written sqlite
database, or a lock file whose owner does not exist on the other side. An
archive that is subtly broken and only found to be so after the source has been
archived is worse than a refusal, so export answers 409 for a running box and
`--stop` stops it first, leaving it stopped.

Rejected: warn and export anyway. The warning is printed at the moment the user
can least act on it — they wanted the archive, they now have one, and whether it
is any good is not knowable until it is imported somewhere.

### 3. Import restores the tree before the row exists

The tree has to be on the pool before the reconciler builds a container, or the
create materializes an empty workspace and the restore arrives too late. The
ordering is therefore:

1. resolve the project, the pool, the harness and every secret binding, and
   allocate the sandbox ID — everything that can refuse the import;
2. stream `tree/` to that pool's agent, which unpacks it under
   `layout.Sandbox(projectID, poolID, sandboxID)`;
3. create the sandbox row with that ID and the sources recorded as already
   delivered.

Only after (3) does anything wake the reconciler, and what it then finds is the
unarchive case: a tree in place, a container to build against it.

**Everything that can say no happens in (1).** A destination whose harness
declares a required secret nobody there has bound is an ordinary condition, not
a corrupt archive, and answering it in (3) would charge the user a whole
workspace upload for a `400` — and in a transfer the source is already stopped,
so the retry is the entire transfer again. The one refusal that cannot move is
the name index, which closes a race between two concurrent imports of one name
that the check in (1) cannot.

Rejected: **park the sandbox in a new state while the tree is delivered**, the
way a push-delivered source parks at `awaiting_source`. It is the obvious shape
and it is more moving parts than the problem has — a new lifecycle state, a new
completion call, a new timeout, and a new way for a sandbox to be stuck. The
tree does not need the row to exist, because the pool agent addresses a tree by
`(project, pool, sandbox)` and not by anything the control plane holds.

The cost is that an import which dies between (2) and (3) leaves a tree on the
pool with no row. That is a state the system already has and already handles:
the pool agent's volume reaper collects a tree with no live counterpart after
its retention window, exactly as it does for a sandbox whose container was lost
out of band. Failing the other way round — a row with no tree — would instead
produce a live, empty sandbox that looks like a successful import.

### 4. An imported source keeps the delivery it was exported with

`resolveSourceDelivery` refuses a client that asks for push delivery, because
whether a bind is possible is the server's to know. An import is not that
client: the delivery mode is already written into the bytes being restored — a
push-delivered source has a bare repository in `tree/origins/` and a `clone`
one does not — so re-deciding it would contradict the tree. An import therefore
carries the exported delivery through and stamps `SourceDeliveredAt`, which is
what keeps the restored sandbox from parking to wait for a push that nobody is
going to make.

The consequence worth stating: a `clone`-delivered source that named a local
directory keeps naming it, and on a destination where that path does not exist
the sandbox's `origin` remote points at nothing. The workspace is intact —
nothing re-clones — so this costs a push target, not the work. It is the same
position a sandbox is in today when the directory it was created from is
deleted.

### 5. The origin travels, because it is not a fact about the server

`Origin` says which client machine the discobox was created from, and a move
does not change that: the workspace still belongs to the same checkout on the
same laptop. Two things read it, and both break without it — `OriginKey` is
re-derived from it, which is what makes `discobox ls` run inside that repository
list the discobox where it now lives; and the pool runtime derives each source's
data key from it, so an import without one comes up with
`/.discobox/data-per-source/<slug>` **absent** rather than empty.

It is deliberately not re-derived from whoever ran the import. The person moving
a discobox between servers is usually on the machine it came from, but not
always, and a box that silently re-homed itself to whichever laptop happened to
run the transfer would disappear from the listing of the repository it is
actually for.

### 6. A transfer is a client-side pipe, not a server-to-server copy

`discobox admin box transfer BOX --to SERVER` stops the box, opens the export on
the source server and the import on the destination, and streams one into the
other. The bytes flow through the client and are never written to disk.

Rejected: **have the source server push directly to the destination.** It reads
as the efficient option and it is the expensive one. The source server would
need a credential for the destination — which the user has and the server does
not, and which would have to be delegated to it — and it would need to be able
to reach the destination at all, which is exactly what the client is for: a
laptop can reach a work server and a home server that cannot reach each other.
Two servers that *can* reach each other still leaves the trust problem, and
solving it buys one hop.

The client already holds both halves. `discobox` addresses any registered server
(ADR 0119) and already aims one invocation at another server to act on a
discobox there, so a transfer is composition of things that exist rather than a
new capability.

### 7. A transfer archives the source, and `--keep` makes it a copy

A move moves. On the destination's confirmation the source box is **archived** —
data kept, no container, recoverable, and collected by the project's ordinary
archive retention. `--keep` leaves the source stopped and intact, which makes
the same command a copy.

Rejected: purge the source. Purge is irreversible and the destination has been
confirmed for all of a few milliseconds; archive costs the same disk for a day
and is undone with `discobox admin box unarchive`. Rejected too: leave the
source running. Two boxes running the same work on two servers, both believing
they own it, is the one outcome nobody asks for.

### 8. An archive ends with its SHA256SUMS, and a reader refuses one that does not

A tar cannot say it is finished. Go's tar reader ends cleanly on a stream cut
between two members, whether or not the end-of-archive blocks are there, and an
HTTP handler that returns after its copy failed has `net/http` end a chunked
body cleanly too. Together they turned a walk that failed part way into a
shorter archive: `export` reported it written, `import` restored it as a
workspace with files missing, and a transfer then archived the source.

So both archives — the `.dbox`, and the tree a pool agent sends and receives —
end with a `SHA256SUMS` member listing every regular file before it, in the
format GNU `sha256sum` writes. `tar xf box.dbox && sha256sum -c SHA256SUMS`
checks an export with nothing of ours installed.

**The member's own framing covers its truncation.** Its tar header states its
length, so a stream cut inside it is an unexpected EOF, and a stream cut before
it leaves it missing, which every reader refuses. Nothing may follow it: a
member after the sums is one they do not cover. What the digests add on top is
the bytes that did arrive and are wrong.

**Each hop verifies before it writes its own.** The pool agent writes the
tree's sums. The server verifies them while composing the `.dbox`, and writes
the `.dbox`'s only once they matched; on import it verifies the `.dbox`'s and
writes the tree's only once they matched; the pool agent verifies those and
removes a tree that fails. No hop ever checksums bytes that already arrived
wrong, so a failure anywhere reaches the end of the chain as a missing
`SHA256SUMS`.

**Verification happens at the end, after the files are written.** Holding a
workspace back until it had been checked would mean holding a workspace; a
restore that fails already removes what it wrote (§3's reasoning about half a
tree).

The streams also fail loudly rather than relying on the sums alone: a handler
whose copy fails aborts the connection (`http.ErrAbortHandler`), and the CLI
writes `<file>.partial` and renames it only when the whole body arrived. The
sums are what a file someone kept is checked against; the abort is what keeps
that file from being written as though it were whole.

Rejected:

- **Rely on tar's end-of-archive blocks.** Go's reader does not require them and
  its API cannot tell their presence from a bare EOF, and they detect nothing
  about corruption.
- **A digest in an HTTP trailer.** It does not survive being saved, and a
  `.dbox` is a file people keep and mail to each other.
- **A digest in `manifest.json`.** The manifest is the first member so a reader
  learns what it holds before the workspace arrives, and the digest is not
  known until the workspace has been sent.
- **One digest over the stream's raw bytes.** The server rewrites every
  member's name on the way through (`tree/` is added on export and removed on
  import), so it could not survive a hop, and no stock tool checks it.
- **An end marker of our own** (a member carrying a count). It catches
  truncation and not corruption, and it is a format nobody else can read; a
  sums file does both in a form people already use.

The costs: only regular files are listed, because they are all `sha256sum` can
check, so a directory's or a symlink's metadata is protected against truncation
by preceding the sums and not against alteration. A writer holds the listing in
memory until the end — one line per file, bounded by the file count and never
by file size. A reader holds nothing: it hashes the listing it expects from the
members it has read and compares one digest, which also means it accepts only
the listing a writer produces, in archive order, and refuses a hand-edited one
rather than interpreting it.

## Consequences

- **A restore is confined by `os.Root`, not by checking entry names.** An
  archive arrives from outside and the entry that escapes is not the one with
  `..` in its name: it is a symlink an earlier entry in the same archive
  created — `data/x -> /etc`, then `data/x/passwd` — whose name is entirely
  inside the tree and whose file is not. The pool agent is root on the pool
  host and an import is reachable by any project member, so a lexical check
  alone is a remote arbitrary-write. Every write in a restore therefore goes
  through an `os.Root` opened on the sandbox tree, which resolves each path
  component beneath it and refuses one that leaves; a symlink is still stored
  verbatim, because it is resolved inside the sandbox where the tree is mounted
  elsewhere, and is safe to write because nothing in the restore follows it.
  The name check stays for its own separate job: keeping an archive from
  carrying a `config/` or a `.discobox-archived` over what the create is about
  to write.
- `sandbox.Provider` gains `ExportTree` and `ImportTree`. Both are told their
  pool rather than reading one out of runtime state: a sandbox whose create
  failed before its agent reported has no such state and still has a tree on
  the pool its row names — and is exactly the sandbox somebody wants to export,
  because it is broken where it is. They are required
  methods on the core interface, not an optional capability: every backend
  stores a sandbox's durable state the same way, through the pool agent, so
  there is no runtime variation for an optional interface to express.
- The pool agent gains `GET`/`PUT .../sandboxes/{id}/tree`, hand-wired beside
  the git routes rather than in the OpenAPI contract, for the reason the git
  routes are: the body is an opaque stream. Neither route auto-starts the
  sandbox — the whole point of both is that nothing is running.
- The archive format is versioned (`formatVersion`) and read by the server, not
  by the CLI, so one implementation decides what a `.dbox` is and an older CLI
  keeps working against a newer server. A reader accepts its own version and
  older, and refuses newer: a `.dbox` is a file people keep, so a version bump
  that orphaned every archive in existence would be a schema change
  invalidating persisted state, which this repository does not do. Refusing a
  newer one is the point of the field — what an unknown addition means is the
  difference between a restored discobox and a subtly different one.
- A root-module package, `tarsums`, is the one implementation of §8, because
  both the pool agent and the server read and write it.
- An export is a full copy of a workspace, including its git objects and
  anything cached in the sandbox user's home. It is not incremental and makes no
  attempt to be; the archive is uncompressed tar and compression is the user's
  to apply.
