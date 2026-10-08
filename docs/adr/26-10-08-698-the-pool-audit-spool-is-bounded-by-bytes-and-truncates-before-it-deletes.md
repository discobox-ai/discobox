# 26-10-08-698 — The pool audit spool is bounded by bytes, and truncates before it deletes

- **Status**: Accepted
- **Date**: 2026-10-08
- **Relates to**: the age retention in [proxy/DESIGN.md](../../proxy/DESIGN.md#retention)
  (`Recording.Retention`, `DISCOBOX_PROXY_AUDIT_RETENTION`), which this keeps
  and adds a second bound beside.

## Context

The pool proxy writes every HTTP request and response body, and every upgraded
stream, to spool files under the pool's `proxy-bodies` and `proxy-streams`
trees. The only bound on those trees is age: 48h by default. Age bounds how long
a byte is kept, not how many there are, so a pool's disk use is its traffic
rate times two days. A busy pool fills its data disk well inside the window, and
spool data is the largest thing on it.

Most of those bytes are copies. A registry blob is cached by the proxy's
content-addressed response cache and, on the same pass, spooled in full as a
response body. Each later cache hit spools it in full again. A pool that pulls
one 2 GiB image ten times in the window holds 2 GiB in the cache and 20 GiB in
the body spool, all of it the same bytes.

## Decision

### 1. A body served through the cache is a reference to the cache entry

A response the cache stores (a miss that commits) or serves (a hit) writes no
body spool. Its row records no `response_body_file`. The `cache_key` it already
carries, with `cache_stored` or `cache_hit`, is the reference. `OpenBody`
resolves such a row through the cache, reading the entry without touching its
LRU position so an audit read never decides what the cache keeps.

The cache stays bounded by its own byte ceiling and evicts by LRU, so the
reference can outlive the entry. Such a body then reads as reclaimed (§6). This
is acceptable: the bytes are content-addressed, the row's URL names the digest,
and the registry can still serve them.

A miss whose cache store aborts (write error, digest mismatch) has already
streamed to the client with nothing spooled. Its row records why in
`response_body_error`, and the body is not retained. A digest mismatch is the
one body worth keeping. It is rare, and keeping it would mean spooling every
cacheable body in case its store fails, which brings the duplication back.

### 2. The spool trees share one byte budget

The body and stream trees together are bounded by a budget that is the lesser
of two terms:

- `Recording.MaxSpoolBytes`, an absolute ceiling.
- `Recording.MaxSpoolPercent`, a share of the total size of the filesystem
  holding the spool trees. The total comes from `statfs` on each pass, so a
  disk that is resized moves the budget with it.

A pool VM's spool sits on its data disk, which defaults to 100 GiB, while a
Docker pool's spool sits on the host's filesystem, which may be several
terabytes. A fixed size fits only one of them. The percentage scales with the
disk, and the ceiling stops a large host from reserving hundreds of gigabytes
for audit data. The term is a share of the total size, not of the free space. A
free-space budget would shrink as sandboxes fill the disk, swinging between
reclaiming nothing and reclaiming everything as their use changes.

Setting a term to zero drops it from the minimum, and setting both to zero
disables the budget, for an embedder that manages disk itself. The
recorder keeps a running total: a walk sets it at startup, each spool write adds
to it, and each pass subtracts what it reclaims and corrects drift from its own
walk. A write that takes the total past the budget wakes the sweeper without
blocking. A pass also runs on the age sweeper's own tick.

A pass reclaims down to a low watermark, 90% of the budget, so a pool at its
budget does not sweep on every write.

The response cache is outside the budget. It has its own ceiling and is not
audit data. The SQLite database is outside it too: rows are small and the age
sweep removes them, and the database file does not shrink without a `VACUUM`.

### 3. Over budget, a pass truncates tails first, largest file first

A file larger than `Recording.BodyHeadBytes` (default 64 KiB) has a tail: the
bytes past its head. While the total is above the watermark, a pass truncates
tails, largest file first, ties going to the older file. Each truncated file
keeps its head.

Only once no file has a tail left does a pass delete whole files, oldest
modification time first, each one now at most a head.

The result is that one large file can never cost a small file its existence:
every large file's tail goes before any whole file does. Largest first reaches
the outlier that caused the overflow in one step, instead of trimming every
other large file before it. Deletion order matters only among files of at most
head size, where age is the fair order.

Truncation keeps three properties the age sweep relies on:

- **Modification time is restored** after a truncate (`Chtimes`). The age sweep
  pairs files with rows by mtime, and truncating must not make an old file look
  new.
- **An open spool is never truncated or deleted**, the same rule as the age
  sweep. A spool still being written has no row yet, and its writer holds an
  offset that a truncate would turn into a sparse hole.
- **A stream spool is cut on a frame boundary**: at the last whole frame that
  ends at or before the head, never inside the stream header. The file stays
  parseable and loses only its trailing frames and the summary frame. A body
  spool is raw bytes and is cut at the head exactly.

All file operations go through the existing `os.Root` over each spool tree, so a
planted symlink cannot point a truncate outside it.

### 4. Rows outlive their files

The budget never deletes a row. A row is the record that an exchange happened,
and it is small. The age sweep still removes it at the retention cutoff. Under
the budget, a file may be truncated or gone while its row remains.

### 5. Configured per pool, enabled by default

The proxy's defaults are a budget of min(100 GiB, 5% of the filesystem) and a
64 KiB head. On the default 100 GiB VM data disk that budget is 5 GiB. It
reaches the 100 GiB ceiling on a host filesystem of 2 TB or more.

These settings reach a pool the way retention does. They are `PoolPolicy`
fields `proxyAuditMaxSize`, `proxyAuditMaxPercent` and `proxyAuditBodyHead`.
They are rendered into the pool container's environment as
`DISCOBOX_PROXY_AUDIT_MAX_SIZE`, `DISCOBOX_PROXY_AUDIT_MAX_PERCENT` and
`DISCOBOX_PROXY_AUDIT_BODY_HEAD`, and read by `proxyagent.RunProxy`.

Unset fields serialize away (the `configRevision` rule in
[server/providers/DESIGN.md](../../server/providers/DESIGN.md)), so existing
pools are not recreated. An explicit `0` drops that term (§2). A percentage
must be within (0, 100]. An unparsable or negative value is an error, as it is
for retention.

### 6. A reader is told what was reclaimed

A row records the full byte counts of what passed through. The body read
compares that count with what the file now holds and reports it. A shorter file
is served with its retained and original sizes. A missing file, or a missing
cache entry, is answered as reclaimed, which is distinct from a body never
recorded. The CLI says which on stderr, so a truncated body is never mistaken
for a complete one.

## Alternatives rejected

- **Delete oldest first under the budget (plain LRU by mtime).** This is what
  the user asked not to have. One recent multi-gigabyte body evicts every older
  small body to make room, and small bodies (API calls, git negotiations, model
  requests) are most of what anyone reads the trail for.
- **Delete largest first, with no truncation.** This protects small files but
  loses the head of every large one. The head (status line, JSON envelope,
  first bytes of an archive) is usually what identifies a body.
- **Truncate oldest first.** This reclaims the outlier's tail last, after
  trimming every other large file. Largest first reclaims the most bytes per
  file touched.
- **Water-filling to a common per-file ceiling.** This is the max-min fair
  allocation, but it changes the cut point every pass, re-truncating the same
  files repeatedly. A fixed head is predictable for a reader.
- **A fixed byte budget alone.** Any single default is wrong at one end:
  10 GiB is a tenth of a VM pool's data disk, but trivially small on a
  multi-terabyte host. A percentage alone has the opposite problem on a large
  host.
- **A budget measured against free space.** This tracks pressure more
  directly, but the budget moves with sandbox disk use. The spool is reclaimed
  hard when a sandbox briefly fills the disk and then grows again when it frees
  space.
- **Cap each spool at write time.** This loses data on a pool that has the
  space. The budget truncates only when space is actually short.
- **Pin cache entries while a row references them.** This couples the cache's
  byte ceiling to audit retention, so the cache could no longer guarantee its
  bound.
- **Spool every cacheable body anyway and dedupe later.** This writes the bytes
  twice and spends a pass finding the duplicate. The cache key already
  identifies the content.

## Deferred

**Offloading spool data to object storage.** The intended end state is to ship
bodies and streams to an object store and keep only heads locally. It is
deferred because no pool has an object-store binding today, and the local budget
is needed regardless as the bound while an upload is pending or the store is
unreachable. Revisit when a pool can be given an object-store credential. The
natural point is to upload before §3's delete step, so local truncation and
deletion become eviction of a copy, not loss of the only one.

## Consequences

- Registry traffic stops being the dominant cost of the body spool. Disk use is
  bounded by the budget, not by traffic times retention.
- A body can now be partial or gone inside the retention window. Readers are
  told which (§6).
- `REVIEW.md` gains a rule: a new spool kind counts toward the budget and must
  define a truncation point that leaves it readable.
