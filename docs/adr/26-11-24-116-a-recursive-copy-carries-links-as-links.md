# 26-11-24-116 — A recursive copy carries links as links

- **Status**: Proposed (would supersede [26-11-04-684](26-11-04-684-a-lead-runs-child-commands-and-copies-files-through-the-files-api.md) §3's "the copy archive profile carries only regular files and directories" and its dereference of descendant source links, §4's link and special-file bullet and its "no hard-link topology", and §3's refusal of every existing destination entry symlink for a file member)
- **Date**: 2026-11-24
- **Issue**: [#1732](https://github.com/discobox-ai/discobox/issues/1732)
- **Relates to**: [26-11-04-684](26-11-04-684-a-lead-runs-child-commands-and-copies-files-through-the-files-api.md) (the files API copy and its archive), [26-10-22-499](26-10-22-499-a-discobox-has-a-files-api-that-acts-as-its-user.md) (the files API), [26-11-10-203](26-11-10-203-the-console-shows-a-boxs-files-beside-its-workspace-and-moves-them-by-drag-drop-and-key.md) (the console's file panel, which reuses this copy), [0123](0123-a-discobox-is-exported-as-its-spec-and-its-durable-tree.md) (`sandboxtree`, the format `run -i` delivers through)

## Context

Checked against `e6f5ea446` on main.

`discobox cp -r` and the console's file panel move a directory through
`pkg/copyarchive`, on both ends: the CLI (`cli/internal/cli/cp_transfer.go`
`copyDirectory`, and `tui_files.go`) and the sandbox agent's files helper
(`sandbox-agent/files/ops.go` `OpImport`/`OpExport`). ADR 26-11-04-684 §3
limited that archive to regular files and directories:

- `Export` (`pkg/copyarchive/archive.go`) calls `root.Stat`, which follows a
  link. `os.Root` refuses one that leaves the selected root, and a dangling one
  has nothing to stat, so `Export` fails with `statat <name>`. An in-tree link
  arrives as a second copy of its target. Hard links arrive as separate files.
- `Import` refuses any member that is neither `TypeReg` nor `TypeDir`, or that
  carries a `Linkname`.
- A failure part way leaves the completed entries behind (684 §4: "a recursive
  copy is not a tree transaction").
- The OpenAPI text for `GET`/`PUT …/files/archive` says links are dereferenced
  and that link members are refused (`api/openapi/sandbox.yaml`,
  `api/openapi/server.yaml`). `discobox cp --help` says the same
  (`cli/internal/cli/cp.go`).

Every Python virtualenv has a link that leaves its tree
(`.venv/bin/python3 -> /usr/bin/python3`). So `cp -r` on a typical project
aborts after a few files. The same happens to anything with a dangling link,
or a socket left by a running tool. `discobox run -i` delivers that tree
intact through `pkg/sandboxtree` (`sandboxtree.go` `writeEntry`), which
writes a link as a tar symlink, keeps hard links (`links_unix.go`) and skips
sockets, FIFOs and devices silently. The files API's own in-box copy
(`files/copy`, `ops.go` `copyTree`) also copies a link as a link. `tar`,
`rsync -a` and `cp -a` do the same. Only `cp -r` disagrees.

684 chose dereferencing to keep a batch copy rooted. The rooting that
matters, though, is that the copy never *reads or writes* outside the
selected trees. A link carried as its text is neither: the exporter reads
the link itself, and the importer writes the link itself. Neither follows it.

The misleading error (the exporter's failure reported as a connection
failure, with `discobox admin server status` advice) is a separate bug, fixed
in `copyDirectory` without this decision.

## Decision

### 1. Links travel as links, wherever they point

The copy archive carries a symbolic link as a tar symlink member
(`TypeSymlink`, `Linkname` = the link's text, exactly as `readlink` returns
it). This applies to every link below the selected root: in-tree, leaving the
tree, absolute and dangling. None of them is resolved, and none fails the
copy. A link inside the tree is no longer dereferenced into a second copy.

The explicitly named source operand is still resolved first, as it is today
(`cp -r link-to-dir box:` copies the directory it names, as `cp -rH` does).
Only descendants travel as links. A single-file copy (`cp link box:`) is
unchanged.

### 2. The exporter never follows a descendant

`Export` walks with `Lstat`/`Readlink` through the pinned `os.Root`. It opens
a regular file without following a final link and checks that the open file
is the one it saw (`os.SameFile`), as it does today. A link swapped in
between the two is a changed source: the copy fails, and it never reads
through the link. Since no link is followed, a directory cycle cannot form
through one. The ancestry cycle check goes; the depth, entry-count and
metadata bounds stay.

### 3. The importer creates links and never traverses one

`Import` accepts a symlink member under these rules. Together they keep 684's
confinement: no write and no metadata change outside the destination root.

- **Target text is data.** It must be non-empty, at most 4096 bytes, valid
  UTF-8 and free of NUL. Nothing else is checked: absolute, `..` and dangling
  targets are created as they are, as `tar` does. The importer never resolves
  a target.
- **No member lives below a link.** A member whose parent path is a symlink
  member of the same archive is refused when decoded, before any filesystem
  operation. Each destination directory is still opened through
  `openChild`, which refuses a link. An existing destination link therefore
  never becomes a directory a later member is written into.
- **Creation is rooted and atomic.** The link is created at a temporary
  sibling through the pinned directory (`os.Root.Symlink`) and renamed over the
  name, so a failed member leaves what was there.
- **What a member may replace.** A link member replaces an existing regular
  file or link at its name. An existing directory is a type conflict and
  fails, as 684 §4 says for a file. A regular-file member may now also replace
  an existing link at its name. The rename replaces the link itself and never
  writes through it, so copying a tree onto an earlier copy of itself works.
  A directory member onto an existing link stays a type conflict.
- **Metadata.** `-p` applies no mode or times to a link. Link modes mean
  nothing on Linux and macOS, and a link's times are not worth a second
  platform path.

### 4. Hard links are kept

On an exporter whose platform reports a link count, the second and later
names of one inode are written as `TypeLink` members naming the first
member's path. Only files with a count above one are indexed, as
`sandboxtree` does. The importer accepts a `TypeLink` only when its
`Linkname` is an earlier regular-file member of the same archive. It creates
the link through the pinned root (`os.Root.Link`), at a temporary sibling,
and renames it into place. It never links to a destination file the archive
did not write. On a receiver that cannot make a hard link, the importer
writes a copy of the earlier member's content instead, read through the
pinned root.

### 5. Special files are skipped and named, not fatal

Sockets, FIFOs and devices are left out of the archive, as `sandboxtree`
does, instead of failing the copy. Unlike `sandboxtree`, the skip is not
silent. The command names each skipped path on stderr and still exits zero.
This holds wherever the exporter runs, so on a download the skipped names
must reach the CLI. How the archive carries them is left to implementation
(see Open). The importer still refuses a special member.

Every other source failure, such as an unreadable file, a name the receiver
cannot represent, or the bounds, fails the copy as 684 §4 says. The failure
names the refused path and is attributed to the source, never to the
connection.

### 6. A peer that predates this refuses before writing

A receiver on an older version refuses link members part way through an
import. So:

- **Upload** (CLI or relaying server exporting to a sandbox): the exporter
  sends link members only to a receiver known to accept them. A receiver that
  does not is refused before any entry is written, with an upgrade
  explanation, as 684 §3 says for a missing route. The copy never falls back
  silently to dereferencing.
- **Download** (an older CLI reading a newer sandbox's export): the export
  request says whether its reader accepts links. For a reader that does not,
  the exporter emits the same archive as before for a tree without links, and
  fails at the first link with an explanation to upgrade the CLI. It keeps no
  dereferencing path.

How the reader declares this and how the CLI learns a receiver's support are
left to implementation (see Open). The rule above is the constraint on both.

### 7. Windows

A Windows CLI exporting a link carries its target text with `\` converted to
`/`. A reparse point that is not a symbolic link, such as a junction, is
treated as a special file (§5). A receiver that cannot create a symbolic
link, such as Windows without the privilege or a filesystem without links,
skips that member and names it as §5 does, never substituting a copy of the
target.

### Open, for implementation

- How the export request declares that its reader accepts links and
  skipped-name reports, and how the CLI and a relaying server learn that a
  receiving agent accepts them before sending (§6). Candidates: a query
  parameter or media type on `GET …/files/archive`, a field the receiver
  reports in its stat, or a first archive member an older importer refuses
  before it writes anything.
- How the skipped names of §5 travel in the archive. One option is a member
  outside `files/`, covered by the `SHA256SUMS` manifest.
- Whether the inode index is shared with `sandboxtree` or owned by
  `copyarchive`. There must be one implementation, not two copies.

## Alternatives considered

- **Keep dereferencing, refuse escaping links (684 as is).** This breaks on
  every virtualenv and every `node_modules/.bin`, and it duplicates content.
  It also disagrees with `-i`, the in-box `files/copy`, `tar` and `rsync -a`.
  The confinement it protected is kept by §§2–3 without it.
- **Skip escaping and dangling links with a warning, keep dereferencing
  in-tree ones.** This stops the abort, but the tree still arrives broken:
  the virtualenv has no interpreter, and links become copies. It also keeps
  the cycle-checking dereference path.
- **Rewrite absolute targets to stay in the tree, or refuse them.** That would
  change what the person copied. A `.venv` link to `/usr/bin/python3` is
  correct in a box with Python there. A target is text the importer never
  follows, so refusing it protects nothing.
- **Make the copy transactional (stage, then publish the whole tree).** It
  would double disk use and need a rename of a whole tree over a merge
  destination, which 684 §3's merge semantics rule out. With links carried and
  specials skipped, the common causes of a part-way abort are gone.
- **A whole-tree pre-scan before the first write.** This is unnecessary once
  links and specials no longer fail. It also cannot catch what fails only
  during the read (permissions that change, files that change), and it doubles
  the walk.
- **Drop hard links (split them into copies).** `pnpm` stores and git
  alternates link at scale, and `-i` keeps them. Since the format already has
  to carry link members, keeping hard links costs only an index and one
  importer case.
- **Use `sandboxtree` for `cp -r`.** That is the durable restore format. It
  carries ownership, fixed subtrees and no overwrite-merge rules, and 684 §3
  keeps the two apart for those reasons. This ADR aligns `copyarchive`'s link
  behaviour with it but does not merge the formats.

## Consequences

- `cp -r` and the console's directory copy deliver a virtualenv,
  `node_modules/.bin` or any linked tree intact, in both directions and
  between boxes. A copy can now *create* a link that points outside the
  destination tree, as `tar`, `rsync -a` and `-i` do. The copy itself never
  follows one. Following a link later is the job of the programs that read the
  tree, and a later `discobox cp` into that tree still refuses to traverse a
  destination link.
- A download from a box can place a link the box chose in the person's local
  destination tree, and can replace a local file or link of the same name
  there. That is the same reach a download already has over a file's content.
- Older CLIs downloading from newer boxes lose in-tree link dereferencing.
  They fail at the first link with an explanation to upgrade instead of
  receiving a copy.
- The OpenAPI descriptions of both archive routes, `cp --help` and
  `docs/public/cli.md`, `pkg/copyarchive`'s `DESIGN.md`/`REVIEW.md` and 684's
  status line change with the implementation.

Acceptance evidence: the triage test `TestSymlinksLeavingTheTreeCopyAsSymlinks`
passes, extended with an in-tree link, a hard link pair, a socket (skipped and
named) and a re-copy onto an earlier copy. There are hostile-archive tests: a
member below a link member, a `TypeLink` naming a later or a non-archive path,
a link target with NUL or over 4096 bytes, and a destination link swapped in
during import. In each, a sentinel outside the root is neither read nor
changed. Skew tests cover a new CLI against an agent that refuses links and
an old reader against a new exporter, with no entry written. A `cp -r` of a
real `python3 -m venv` tree into a dev box ends with `.venv/bin/python` able
to run.
