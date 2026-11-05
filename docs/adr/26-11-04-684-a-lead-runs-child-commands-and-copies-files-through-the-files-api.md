# 26-11-04-684 — A lead runs child commands and copies files through the files API

- **Status**: Proposed (would supersede 0140 §4's refusal and deferral of execs for a child; 26-10-01-397 §1's refusal of waiting acquire and §2's exclusion of create, start, delete and attach; extends 26-11-03-970 §§2–3's unchanged route set and adoption disclosure; supersedes 26-10-22-499 §4's keeping of cp on scp/SFTP and its Trees deferral for CLI recursive copy)
- **Date**: 2026-11-04
- **Issue**: [#609](https://github.com/discobox-ai/discobox/issues/609)
- **Relates to**: [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md), [26-10-01-397](26-10-01-397-a-discobox-reads-and-types-into-the-terminals-of-the-discoboxes-it-creates.md), [26-11-03-970](26-11-03-970-a-discobox-adopts-another-as-its-child-when-a-person-approves.md), [26-10-01-032](26-10-01-032-a-sandboxs-egress-is-denied-when-any-of-its-levels-says-so.md) §4, and [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md) §9.

## Context

Checked against `b302561eb51039c19a4d651cf620017563a57755` on main:

- `server/internal/auth/authorizers.go` allows exec reads and input, but not
  create, start, delete or attach. `authorizeChildDiscobox` reads
  `ParentSandboxID`, so created and adopted children already have identical
  authority. The refusal is not an adoption regression.
- `server/internal/resources/sandboxes/service.go` independently refuses
  `SandboxUse.Attach` for sandbox principals, even though exec read/write
  scopes are allowed. Widening the route table alone cannot enable a shell.
- `cli/internal/cli/cp.go` delegates transfers to scp via
  `startSSHClientSession`; `cli/internal/cli/ssh_config.go` lists and may enroll
  project SSH keys. Those keys authenticate as their enrolling user with
  project-wide exec/TCP scopes (`server/internal/sshd/DESIGN.md`). Allowing key
  enrollment would turn child management into project-wide authority.
- `server/internal/server/sandbox_agent_terminals_proxy.go` already carries
  create, start, delete and attach through the exec primitive. SSH's SFTP
  subsystem is itself an exec of `/usr/lib/openssh/sftp-server` in
  `server/internal/sshd/session.go`.

A lead needs direct commands and file transfer to inspect session history and
install worker instructions. Terminal input already permits arbitrary commands;
forcing a lead to type them through the harness adds fragility. Adoption can
expose a child's existing files and credential sentinels: that trust decision
belongs in the person's approval, not in a promise that only a screen is read.

The files API already exists on main, implemented by `sandbox-agent/files`,
contracted under `sandboxes/{id}/files/*` in `api/openapi/server.yaml`, and
forwarded by `sandbox_agent_terminals_proxy.go`. It supplies stat, list,
streamed read/write, mkdir, remove, move and copy within a sandbox as its
default user. `cli/internal/cli/sandbox_files.go` already uses it. Open
[PR #334](https://github.com/discobox-ai/discobox/pull/334) adds AHP resource
access over that API; the API itself is not waiting for that PR. Today the
sandbox role omits these routes and its service scope allow-list omits
`files:read` and `files:write`.

The sandbox also already has certificate-backed identity for API calls.
`proxy.EnsureClientCertificate` puts its ID in the pool-issued client
certificate's Common Name; `generateSignedCert` currently generates an RSA-2048
key, not an elliptic-curve key. The proxy verifies the mTLS chain and derives
the caller from that certificate (`proxy/http.go`, `proxy/server.go`). The
pool's API gate replaces caller-supplied identity headers with that ID and its
own assertion (`pool-agent/proxyagent/gate.go`); `SandboxForwardAuthenticator`
verifies the assertion and checks that the pool hosts the named sandbox.
Thus the API already derives a sandbox principal from the existing certificate,
without registering a new public key.

## Decision

### 1. The current parent gets the whole exec lifecycle

With `DiscoboxAPI` enabled, a sandbox may create, start, attach to and delete
execs in a direct child in its own project. Add these routes to the fixed role:

| Method | Suffix below `sandboxes/{id}` | Guard |
| --- | --- | --- |
| POST | `/execs` | `withinCaller` |
| POST | `/execs/{execId}/start` | `withinCaller` |
| GET and POST | `/execs/{execId}/attach` | `withinCaller` |
| DELETE | `/execs/{execId}` | `withinCaller` |

These accompany existing exec reads and input. An attach carries input, signals
and resize even when opened with GET; it is a write. Creating an exec to read a
file is also a write. There is no read-only shell exception for a child with
wider egress; files API reads have their own permission below. Commands, login
shells, PTYs, non-PTY binary streams and exit status use the existing exec protocol. Preserve the
create-suspended, attach-before-start ordering.

The complete API path must support the attach stream. Today the pool's API
gate strips `Connection`/`Upgrade` and uses a bounded ordinary HTTP request
(`pool-agent/proxyagent/gate.go`), so adding the server routes and acquire
permission alone is insufficient. Extend that same authenticated gate to carry
exec attach upgrades and bidirectional bytes, EOF and close, retaining the
pool-derived caller identity and fail-closed revocation below. Do not open an
unauthenticated tunnel or let a sandbox set the forwarding identity. Ordinary
bounded API calls retain their existing timeout; an authorized interactive
attach must not inherit that two-minute ordinary-request cutoff.

Authorize waiting acquire for these exec operations at the service boundary,
with the same current-parent, project, API flag and egress checks; do not remove
the guard for unrelated scopes or grant a sandbox user scopes. Direct callers
of the service must not bypass those checks. An exec ID is resolved only inside
the authorized target sandbox, including aliases such as `primary`.

The grant includes existing child terminals, not only execs this caller made.
A lead can already input into them and stop or archive the entire child. An
exec creator restriction would create a second ownership model without a useful
boundary. Authority is direct parenthood, never ancestry or immutable creator.

### 2. The role gains the files API, and cp uses it for every caller

Add the existing files routes, scoped to the caller's direct child:

| Method | Suffix below `sandboxes/{id}` | Guard |
| --- | --- | --- |
| GET | `/files/stat`, `/files/list`, `/files/content` | `childDiscobox` (R) |
| PUT | `/files/content` | `withinCaller` (W) |
| POST | `/files/mkdir`, `/files/remove`, `/files/move`, `/files/copy` | `withinCaller` (W) |
| POST | `/files/metadata` (extension below) | `withinCaller` (W) |

Add `files:read` and `files:write` to the sandbox service's supported scopes,
with the corresponding live parent, project, API flag and egress checks at the
service boundary, including direct service callers. Keep the files API's
existing default-user execution; no request selects root. Reads retain their
existing `read` use (start a stopped box, repair nothing); writes are actions.
A lead may read/download from a wider child, just as it may read its logs.
Uploading, removing or changing a file in that child remains refused.

`discobox cp` and its `scp` alias use this API for people and sandbox callers
alike. Remove the scp subprocess, SSH-command helper, key enrollment and caller
identity-based transport choice from copy. Retain current operand resolution:
IDs, names, bare `:`, explicit addresses, local colon/Windows-drive rules,
remote paths relative to the box user's home, and one server per invocation.
A box-to-box copy streams through the CLI with each endpoint authorized
separately; neither child receives the other's credential. `/files/copy`
operates within one box, not between boxes or between a box and the CLI.

The CLI copies files with stat/read/write, and recursively walks directories
with list/mkdir. Content stays out of exec input logs; no shell, SFTP server,
archive format or new identity is involved. Use bounded buffers and stream
bodies rather than loading a file or tree into memory. Large transfers must
work through the real pool API gate: its current two-minute ordinary-request
timeout must not truncate an otherwise progressing files content transfer.
Keep bounded setup/stall/cancellation behavior; do not remove timeouts from
unrelated calls. Exec/shell still needs the upgrade support in §1, but copy
does not depend on that work.

### 3. Copy has a defined contract, not arbitrary scp passthrough

Keep the core copy cases and test them explicitly:

- One or more sources to a destination; multiple sources require an existing
  directory. A directory source requires `-r`. Existing directories receive
  the source basename; an absent destination takes that source's name/content.
  A trailing slash requires a directory and is not an implicit contents-only
  switch. Include empty directories and dotfiles. Never remove destination
  entries that were not in the source.
- Remote paths are literal, including spaces, colons, Unicode and shell
  metacharacters. Local shell expansion happens before argv as today; do not
  add remote shell/glob expansion. `--` ends options. Explain this behavior in
  help rather than treating a filename as a command or pattern.
- Match ordinary scp's dereferencing of source symlinks, including directory
  links during recursion. Resolve link chains relative to their containing
  directory with the source platform's path rules, detect ancestry cycles and
  bound link depth. Dangling links and special files (FIFO, socket, device)
  fail clearly; do not block by opening a FIFO. Read permission remains the
  source user's, never the agent's root.
- Existing regular destination files are overwritten; file/directory type
  conflicts fail. Reject copying a tree into itself and a file onto itself
  where both operands name the same filesystem. Reject traversal through a
  destination symlink during recursive download: a child must not choose
  writes outside the caller-selected destination tree. Directory listing names
  are untrusted: require one child component, disallow absolute/dot/parent
  components and destination-platform separators, and reject names/collisions
  that cannot be represented on the receiving platform. Confine local writes
  with filesystem operations that resist symlink replacement races, not just
  a string-prefix or preflight check.
- A cancelled/failed content write leaves the previous destination file
  intact, as the current atomic write does. Local downloads must do the same
  with a temporary sibling and atomic replacement. A recursive copy is not a
  tree transaction: completed files remain, the command reports partial
  failure and returns nonzero, and temporary files are cleaned up. Cancellation
  reaches every open body. Do not retry a consumed upload stream silently.
- Retain `-r`, `-p` and `-q` and the `scp` alias. Without `-p`, preserve the
  existing destination mode on overwrite and use source ordinary permission
  bits subject to the destination umask for newly created files/directories;
  do not propagate setuid/setgid bits. With `-p`, preserve ordinary permission
  bits and source access/modification times, including on existing files and
  after directory children are written. This preserves no owner, ACL, extended
  attributes, hard-link topology or sparse allocation layout. If the receiving
  platform cannot apply requested preservation, report the limitation as a
  failure, never silently claim success.

The current API exposes mode and modification time, but not access time or a
metadata setter; its write preserves an existing file's mode, and `mode` is
only a create option. Extend `SandboxFileStat` with access time and add
`POST /files/metadata` with path, ordinary permission bits and access/modified
times, all optional except path, acting as the same default user. Specify and
generate it in the canonical OpenAPI contract. Use it for `-p` and final
recursive directory modes; capture source times before reading content.
Destination-platform behavior and symlink handling belong in the files package,
not a privileged CLI chmod shell command. Extend its operations structurally.

Stop promising that every scp option is passed through. SSH/transport options
(`-O`, `-S`, `-i`, `-J`, `-P`, `-F`, `-o`, cipher/compression/protocol flags)
and other unsupported scp flags fail before transfer with a specific explanation
and supported syntax; there is no fallback to scp, SSH or exec. Document this
compatibility change for all callers. Full OpenSSH CLI emulation is not a
prerequisite for implementing file and recursive directory copy correctly.

### 4. Parenthood and authority remain live during a stream

Check the live API flag and direct parent at every file request and exec
create/start/attach/delete, not just when the CLI resolves a name. Check egress
coverage for writes and execs; files reads do not require coverage. A suspended exec is
not a transferable capability to start it later after release or reparenting.

For sandbox-authenticated exec attaches, release, reparenting, deletion of the
caller, disabling its API access, or loss of egress coverage invalidates the
stream. Close it and reject further input. Use authoritative change
notification plus revalidation, including a recheck after registering a stream,
so a concurrent release cannot be missed; server restart or loss of the
validation channel fails closed. This must work across server instances, not
only in one process. The implementation chooses the notification mechanism and
records the concurrency contract in the owning design docs.

Apply the same parent/API invalidation to in-flight file content transfers;
loss of egress coverage invalidates file writes, not file reads. Revalidate on
each request in a recursive copy and cancel invalidated bodies so interrupted
writes cannot publish partial content. This is revocation of ongoing access,
not rollback: an operation accepted before the change may already have committed,
and completed files remain. Tests must cover release/reparent/API disable during
upload/download as well as between files.

Closing an attach revokes further access; it does not undo commands, retract
files, or promise to kill detached processes already launched. A person moving
a child between leads must consider what the previous lead already ran. No
blanket process kill or deletion of the child's work accompanies release.

### 5. Created and adopted permissions are the same

The following is the proposed policy, not a claim that new routes are already
implemented. `W` means live parenthood plus `withinCaller`; `R` means live
parenthood without the egress coverage check. The API flag and same-project
boundary apply throughout.

| Operation | Created, still led | Adopted, still led |
| --- | --- | --- |
| Get child; terminal records, screen, logs, events and wait | R | R |
| Command, login shell, exec lifecycle and stream input | W | W |
| Files stat/list/read; cp download | R | R |
| Files write/mkdir/remove/move/copy/metadata; cp upload | W | W |
| Source delivery; tag, name and description | W | W |
| Start, stop, restart, repair and upgrade | W | W |
| Service reads/logs; service start/stop/restart | R; W | R; W |
| HTTP to child ports/URLs, including GET and upgrades | W | W |
| Archive (`rm`) | W | W |
| Release | R | R |
| Answer child's credential requests | Within caller's delegation | Within caller's delegation |
| Direct child secret bind/unbind, binding list or secret value retrieval | Denied | Denied |
| Raw SSH/key enrollment; TCP/UDP tunnels | Denied | Denied |
| Purge, unarchive, general spec changes, adoption approval | Denied | Denied |
| Separate suspend API | No new permission | No new permission |

The existing stop operation is the supported way to stop a child; this does
not invent a suspend route. Project sandbox listing and sandbox events keep
their existing project-wide visibility. Creation still has its own bounds and
may assign only delegated bindings. Passthrough updates keep their current
special checks. Creator-based passthrough semantics do not change on adoption.

Files visible through a shell may contain credential sentinels. Reading those
files is part of command access; this does not expose the underlying secret
values or grant binding management. Adoption changes no binding, and approval
of requests remains limited to the lead's delegation.

The adoption dialog and in-box permission documentation must say explicitly
that approval permits commands, login shells, reading and writing files, and
ending execs, including existing sessions. A wider child remains readable
through file and existing read routes, including cp downloads; shell and file writes remain refused. Existing parent relations
receive the expanded fixed role; there is no migration or new approval record.
That broadening must be called out when the implementation ships.

### 6. Refusals explain the failed check

For a target verified to be a current child, a 403 identifies the operation and
missing authority: API access disabled, operation outside the sandbox role, or
child egress not covered. Include whether the caller created this child or
leads it through adoption; make clear that this distinction does not change
permissions. For example: `cannot attach exec: you lead this adopted box, but
its egress policy is wider than yours`.

Do not report adoption as the missing permission after approval. For unrelated,
self, missing and cross-project targets, preserve the existing visibility rules
and do not add parent, creator or policy details. Global route refusals such as
SSH-key management identify the missing role permission without looking up an
unrelated target. Stream revocation reports that authority changed when the
protocol can carry a reason; the CLI must surface failure rather than success.

## Alternatives rejected

- **Enroll an SSH key for the lead.** Today's key authenticates as a person
  across a project, escaping parent, API-flag and egress checks.
- **Use the existing sandbox certificate/key at SSH ingress.** The identity
  can be derived, and RSA versus elliptic curve is not the authorization
  boundary. But today's SSH callback matches raw key fingerprints against
  authorized keys or project SSH-key rows, then creates a user principal
  (`server/internal/sshd/server.go`); it does not verify pool-issued X.509
  client certificates or map them to sandbox principals. A raw SSH key proof
  carries neither the X.509 issuer nor its sandbox Common Name. Enrolling that
  key in today's project table would grant the same project-wide user access.
  Reusing the certificate safely would need an authenticated certificate or
  pool assertion path into SSH, a sandbox principal, live child/egress/API
  checks on channels, and revocation. That is possible without minting a new
  key, but a new SSH authentication mode is not needed for API exec access.
  Revisit if sandbox principals need general external SSH tools beyond copy.
- **Keep scp/SFTP, with an exec-backed SSH-command helper.** This preserves
  OpenSSH option behavior but adds a second protocol stack and subprocess
  translation to file transfer. The files API already supplies structured
  operations, identity and atomic writes. The owner prefers a direct client
  of it with an explicit copy contract; corner cases are testable behavior,
  not a reason to retain the helper. This supersedes
  [26-10-22-499 §4](26-10-22-499-a-discobox-has-a-files-api-that-acts-as-its-user.md)'s
  choice to keep cp on scp and its Trees deferral for CLI recursive copy.
- **Keep SSH for people but use files API for sandbox callers.** Two copy
  implementations would give the same command different flag, path and failure
  behavior depending on caller identity. Use one files implementation.
- **Tar over exec, or a new bulk archive API.** Adds an extraction protocol
  and exec dependency or server surface while the files API already streams
  content. Tree archives remain deferred; revisit only if measured per-file
  overhead makes recursive copies impractical.
- **Allow only adopted children, or only execs the lead created.** Parenthood
  already governs both kinds of child and all their terminals; extra provenance
  gates make the model inconsistent.
- **Authorize only at attach time.** An old lead could keep a shell and upload
  files after a person deliberately moved the child to someone else.
- **Continue typing commands through the harness.** Already permits arbitrary
  execution while making binary file transfers and history inspection fragile.

## Consequences and acceptance evidence

This deliberately replaces 397's exclusion of long-lived exec streams. There
is no judge now (880); the boundary is current parenthood and egress, not
inspection of each command. Direct execution makes the trust already conveyed
by terminal input explicit, especially for a child with pre-existing bindings.

Implementation must demonstrate commands and a login shell through the full
pool API gate, and files-API copy of binary files, empty files and recursive
trees both ways and between children, for created and approved adopted boxes.
Prove the copy path invokes neither scp/SSH nor an exec and never enrolls a key.
Exercise overwrite/type conflicts, multiple sources, destination directory and
trailing-slash rules, literal names, source links/cycles, special files, `-p`,
local download confinement, foreign path separators/collisions, same-file and
copy-into-itself refusals, large/slow transfers, partial trees and cancellation.
Include a malicious listing and a symlink swap targeting a file outside the
chosen local download root. Verify unsupported options fail before any writes.

Authorizer and service tests must cover all files operations and exec lifecycle
calls, direct service invocation, self/unrelated/cross-project refusals, API
disable, and exec target mismatch. A wider child's file reads/downloads succeed
but uploads/metadata/other writes and execs fail. Test release/reparent during
active streams and between requests, spoofed identity at the gate, and actionable
403s without unrelated-target details. Update adoption disclosure, CLI help,
permission docs and live design docs with implementation. This Proposed ADR
alone changes no runtime behavior; implementation follows acceptance.

Metadata platform details, the shared revocation mechanism and concrete CLI
copy decomposition remain implementation work. They may not relax the copy,
authorization or confinement contract above. No filesystem migration is needed;
existing boxes receive the expanded fixed role. Older agents missing the
metadata extension fail `-p` with an upgrade explanation, without an exec fallback.
