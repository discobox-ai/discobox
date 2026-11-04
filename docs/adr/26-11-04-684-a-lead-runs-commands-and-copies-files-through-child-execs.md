# 26-11-04-684 — A lead runs commands and copies files through child execs

- **Status**: Proposed (would supersede 0140 §4's refusal and deferral of execs for a child; 26-10-01-397 §1's refusal of waiting acquire and §2's exclusion of create, start, delete and attach; extends 26-11-03-970 §§2–3's unchanged route set and adoption disclosure; narrows 26-10-22-499 §4's scp-over-SSH transport to scp/SFTP over API execs for sandbox callers)
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
file is also a write, including a download. There is no read-only shell or copy
exception for a child with wider egress. Commands, login shells, PTYs, non-PTY
binary streams and exit status use the existing exec protocol. Preserve the
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

### 2. Copy from a sandbox uses exec, without an SSH identity

For a sandbox caller, `discobox cp` and its `scp` alias keep scp's transfer
engine and operand/option semantics, but supply a CLI-owned SSH-command helper
(the `scp -S` transport). That helper translates scp's subsystem invocation to
a non-PTY API exec of the sandbox's `sftp-server`, and legacy scp remote commands
(`-O`) to the existing shell-command exec. It pumps stdin/stdout as bytes,
stderr separately, EOF and exit status through exec attach. Relative remote
paths start in the sandbox user's home, matching today's SSH behavior. Paths
and arguments must preserve their boundaries; no locally evaluated shell
command is assembled from a remote path.

Each remote endpoint is resolved and authorized separately. Remote-to-remote
copies are relayed through the caller (scp's local-transfer mode), so both
children must pass the checks and neither receives an identity to contact the
other. A target helper receives a resolved sandbox/project/server, never a
project SSH key. It must not reinterpret an scp hostname as an arbitrary API
endpoint or forward a caller credential to an operand-selected foreign host.
Existing explicit server selection uses that server's configured authentication.
Transport-changing options that conflict with the managed helper must fail
with an explanation rather than silently fall back to SSH enrollment.

Select the transport from the authenticated caller identity supplied by the
server, not solely from being inside a box or the presence of an environment
variable. A sandbox principal never falls back to enrolling a key after a 403.
People retain the current SSH-backed path. No new project key, SSH certificate,
file API, or archive extraction protocol is introduced. The existing mTLS
certificate continues to identify the caller through the pool's API gate;
this proposal changes its allowed operations, not how that identity is minted.
The existing files API is not widened to the sandbox role by this decision.
Raw SSH ingress and TCP/UDP tunnels remain outside the sandbox role. This grants the CLI's copy
commands, not arbitrary external SSH clients a new sandbox identity.

### 3. Parenthood and authority remain live during a stream

Check the live API flag, direct parent and egress coverage at create, start,
attach and delete, not just when the CLI resolves a name. A suspended exec is
not a transferable capability to start it later after release or reparenting.

For sandbox-authenticated exec attaches, release, reparenting, deletion of the
caller, disabling its API access, or loss of egress coverage invalidates the
stream. Close it and reject further input. Use authoritative change
notification plus revalidation, including a recheck after registering a stream,
so a concurrent release cannot be missed; server restart or loss of the
validation channel fails closed. This must work across server instances, not
only in one process. The implementation chooses the notification mechanism and
records the concurrency contract in the owning design docs.

Closing an attach revokes further access; it does not undo commands, retract
files, or promise to kill detached processes already launched. A person moving
a child between leads must consider what the previous lead already ran. No
blanket process kill or deletion of the child's work accompanies release.

### 4. Created and adopted permissions are the same

The following is the proposed policy, not a claim that new routes are already
implemented. `W` means live parenthood plus `withinCaller`; `R` means live
parenthood without the egress coverage check. The API flag and same-project
boundary apply throughout.

| Operation | Created, still led | Adopted, still led |
| --- | --- | --- |
| Get child; terminal records, screen, logs, events and wait | R | R |
| Command, login shell, exec lifecycle and stream input | W | W |
| Copy/scp upload and download through exec | W | W |
| Source delivery; tag, name and description | W | W |
| Start, stop, restart, repair and upgrade | W | W |
| Service reads/logs; service start/stop/restart | R; W | R; W |
| HTTP to child ports/URLs, including GET and upgrades | W | W |
| Archive (`rm`) | W | W |
| Release | R | R |
| Answer child's credential requests | Within caller's delegation | Within caller's delegation |
| Direct child secret bind/unbind, binding list or secret value retrieval | Denied | Denied |
| Direct `/files/*` API calls | Denied | Denied |
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
ending execs, including existing sessions. A wider child remains readable only
through existing read routes, without shell or copy. Existing parent relations
receive the expanded fixed role; there is no migration or new approval record.
That broadening must be called out when the implementation ships.

### 5. Refusals explain the failed check

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
- **Use the existing files API for `cp`.** No new file endpoint is needed,
  and it would keep content out of exec input logs. But it would require new
  sandbox-role file permissions and reimplementing scp's recursive, metadata,
  symlink and option behavior in the CLI. Preserve the decision in
  [26-10-22-499 §4](26-10-22-499-a-discobox-has-a-files-api-that-acts-as-its-user.md)
  to let scp perform transfers; narrow only its SSH ingress transport for a
  sandbox caller. This tradeoff deliberately keeps transfer input in the
  existing exec logging path. Do not claim that the files API is absent.
- **Replace scp with tar over exec.** Changes established option, symlink and
  path behavior and adds an extraction surface. Keep scp and SFTP doing the
  filesystem work.
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

Implementation must demonstrate command exit status, a login shell, binary and
recursive copy both ways, remote-to-remote relay and legacy scp, for created and
approved adopted children. Exercise the real CLI/scp against running boxes,
without SSH-key listing or enrollment by a sandbox caller. Exercise the full
pool API gate, not a direct server URL: prove an interactive attach lasts past
the ordinary gate timeout, binary bytes and EOF cross it, spoofed forwarding
identity is discarded, and revoked attaches close. Authorizer and
service tests must cover direct invocation, self/unrelated/cross-project
refusals, wider-child downloads as well as uploads, API disable, exec target
mismatch, release/reparent between create and start, and revocation of an
already open stream (including the registration race). Assert actionable 403s
without leaking unrelated targets. Update the live design docs and permission
help with the implementation; this ADR alone changes no runtime behavior.

Helper command spelling, caller-identity discovery's contract, and the shared
revocation mechanism remain implementation design work. They may not relax the
transport, authorization, compatibility or revocation requirements above.
