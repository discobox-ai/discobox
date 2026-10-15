# 26-10-15-417 — A sandbox container has no working directory, and a source's paths are checked where a host tool gets them

- **Status**: Accepted
- **Date**: 2026-10-15

## Context

The pool gave every sandbox container a Docker `WorkingDir`: the primary
source's `destination.workingDirectory`, the subdirectory `discobox new` was
pointed at, or any path `discobox admin box create --working-directory`
names. Nothing honoured it. PID 1 is systemd, which runs from `/`. Nothing
`docker exec`s a sandbox container. Every exec is the sandbox agent's own.
Where the harness starts is now delivered through `sandbox.json`
(ADR 26-10-15-664 §3).

What `WorkingDir` did do was make Docker create the path, root-owned, in the
container's own layer before boot. For a checkout kept at its host path under
`$HOME`, that left the directories between home and the source root-owned.
A recursive chown of home had been handing them over, and #234 removes that
walk because it cost 20–35s of every boot on a Docker pool. It was also the
only thing that made such a home an overlay.

The only check on the field was the pool's refusal of a `WorkingDir` that
Docker would reject. The client hands a source's recorded paths to host
tools as a folder on the far side of an SSH host: an editor's
`ssh-remote+<host><folder>` URL, and `{workdir}` in a tool's command. There a
value such as `@attacker.example/x` names another host. The record is not the
client's alone: a box can create boxes (ADR 26-10-14-320), and an archive is
anyone's.

## Decision

1. The pool sets no `WorkingDir` on a sandbox container. The start directory
   reaches the sandbox only as `sandbox.json`'s, under 664 §3.
2. The server refuses a primary source's working directory that is not an
   absolute path, by the sandbox's platform, at or below the directory the
   same request names. It does so on create and on import. It checks the
   record against itself; where the source lands stays the pool's to decide.
   Paths are compared cleaned, so a repository path with a space passes.
3. The client refuses to hand a host tool any folder that is not an absolute
   path in the sandbox, is a Windows share, or holds a control character
   (`sandboxSSHFolder`). That covers the working directory, the directory,
   every reference, and every record however old. The path a template joins
   to the host (`{workdir.urlpath}`) always begins with `/`, a drive path
   written `/C:/src/app`, so no absolute path on either platform runs on into
   the host.

## Alternatives rejected

- **Keep `WorkingDir` and chown the path to it at boot.** Boot cannot see it,
  because systemd runs from `/`, and it is a repair for a value nothing reads:
  the kind of walk #234 removes.
- **Check only where the value is stored.** The server check covers one
  field going forward. The same sink takes `destination.directory`, every
  reference's paths, and records stored before any check existed.
- **Check only at the client's sink.** It covers every field and record, but
  leaves the server accepting values no client produces. The two checks cost
  little, and each covers what the other cannot.

## Consequences

- No path under home exists before boot. Boot's `mkdirAllOwned` creates the
  path to a source target, owned by the sandbox user.
- A home holding only a checkout's path is no longer an overlay.
- A container built before this change keeps its `WorkingDir` until it is
  rebuilt.
- A command printed for the user to paste (`apply`'s `git -C <dir>` hint) is
  not covered by either check yet. It is recorded in `cli/REVIEW.md` and left
  for a follow-up.
