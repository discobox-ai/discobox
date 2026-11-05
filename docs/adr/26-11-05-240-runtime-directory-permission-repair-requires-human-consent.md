# 26-11-05-240 — Runtime-directory permission repair requires human consent

- **Status**: Accepted (narrows ADR 26-11-04-074 §1's prohibition on repairing unsafe runtime directories; its private-directory requirement remains)
- **Date**: 2026-11-05

Accepted by the owner by merging #680, 2026-11-05
- **Issue**: [#677](https://github.com/discobox-ai/discobox/issues/677)

## Context

The release-validation upgrade from v0.18.1 to v0.19.0-rc.4 failed before the
new CLI could contact the running server. The previous CLI created its runtime
directories with mode 0755; the new CLI requires private directories and rejects
them. Both running and stopped boxes still exist, but normal commands cannot
reach the server to perform the upgrade handoff.

At tag v0.18.1, `endpoint/default_unix.go` chooses `XDG_RUNTIME_DIR`, or
`os.TempDir()/discobox-<uid>`, and appends `discobox/server.sock`.
`endpoint/autolaunch_unix.go` creates the startup-lock directory with
`MkdirAll(..., 0755)` before starting the server. The server's subsequent
`MkdirAll(..., 0700)` in `endpoint/listener_unix.go` does not change existing
permissions. With both runtime and temporary-directory overrides unset on
Linux, this leaves `/tmp/discobox-<uid>/discobox` and its per-user parent at 0755.

Today `pkg/endpoint/privacy_unix.go` validates the default socket's directories
before connecting, binding, or acquiring the launch lock. It rejects public
permissions, foreign ownership, user symlinks and unsafe ACLs. The `public`
case in `pkg/endpoint/privacy_unix_test.go` explicitly requires rejection.
ADR 26-11-04-074 §1 and root `REVIEW.md` prohibit repair as well as trusting an
unsafe listener. That prohibition makes the legacy upgrade require manual
permission changes outside Discobox.

Read and traverse permission is different from permission to replace a socket,
but mode 0755 is not an adequate portable substitute for a private directory:
socket access enforcement differs across Unix platforms. Tightening permissions
also cannot establish who created a listener or undo prior access. A person
must decide whether the existing environment is trustworthy.

## Decision

### 1. Detect, explain, ask, repair

Keep creating default runtime directories privately, with mode 0700, and keep
validating them before use. When validation finds incorrect mode bits on an
otherwise eligible user-owned runtime directory, the interactive CLI offers to
repair the permissions instead of immediately failing.

Before asking, show the exact directories, their observed modes, and the
proposed change to 0700. Explain that the existing environment could be insecure,
that restricting permissions does not undo previous exposure or prove the old
server trustworthy, and that the person should proceed only if they trust the
environment. Ask for an explicit affirmative answer; the default is No.

No, cancellation, EOF or the absence of an interactive terminal stops the
command with a nonzero exit and leaves permissions unchanged. Noninteractive
callers receive the paths and an actionable instruction to rerun interactively
or inspect and repair them manually. Do not infer consent from an upgrade,
installation, a general assume-yes setting, or a previous invocation. No bypass
flag or saved approval is introduced by this decision.

Approval authorizes only the displayed permission changes for this attempt.
After successful repair and revalidation, continue the original command,
including the existing server upgrade flow, without requiring it to be rerun.

### 2. Consent is not permission to take over an arbitrary path

The repair applies only to the directories protecting the selected default Unix
socket: the per-user fallback base and its `discobox` child, or the configured
`XDG_RUNTIME_DIR` and its `discobox` child. Explain explicitly when the runtime
base is shared with other applications. Never recursively chmod their contents,
change ownership, modify ancestors outside this runtime tree, or delete or move
the directory. Explicit custom endpoints retain their existing trust policy;
Windows named pipes are unchanged.

Foreign ownership, user-controlled symlinks, unexpected file types, unsafe
ancestor ownership or replacement permissions, and unsafe or unreadable ACLs
remain errors without an offer to repair. Preserve the system-owned macOS
`/tmp` and `/var` alias handling. Consent does not override these refusals.

Inspect through directory descriptors without following user symlinks. Recheck
ownership, identity, mode and ACLs after the prompt; if the inspected objects or
conditions changed, stop and report that the path changed. Apply changes to the
validated directory objects, then revalidate the whole path using the ordinary
privacy checks before opening the launch lock or contacting any listener.
Never chmod an object reached through an unchecked pathname. A repair error
stops the command and reports any changes already made; do not roll back by
making a successfully protected directory public again.

This repairs mode bits only. Removing or rewriting ACLs, adopting foreign-owned
paths and recovering attacker-writable ancestors are outside this decision.
An eligible directory may have overly broad mode bits, including write bits;
the warning and affirmative human decision are required precisely because its
past integrity cannot be established automatically.

### 3. Resume the existing handoff under the existing trust boundary

Do not connect to the old listener before repair and full revalidation. After
approval, trust the resulting private endpoint under the existing same-user
local IPC model. Consent acknowledges that filesystem checks cannot authenticate
the historical environment; neither a version response nor the directory's
current mode proves the listener's origin. This proposal adds no independent
peer-identity authentication protocol.

Keep the normal version comparison, shutdown request, readiness handling and
provider-drain wait in `pkg/endpoint/autolaunch.go`. ADR 26-11-04-074 §§2–4 and
ADR 0019's data-directory kernel-lock authority remain unchanged: no PID kills,
no forced lock release, no binding over an incumbent before it releases the
lock, and no database recreation. Missing holder metadata on an older server
is not a reason to skip the normal shutdown handoff.

The CLI owns the interaction; shared endpoint code reports the detected
condition and performs validated filesystem operations without prompting from
a transport callback. Server startup and other noninteractive consumers fail
closed with recovery information. The CLI must handle the condition before its
first default-endpoint connection, including commands that normally only probe
status. Never interpret a privacy error as proof that no server is running.

## Alternatives rejected

- **Silently accept public directories.** This abandons the private IPC boundary
  and makes protection depend on platform-specific socket permissions.
- **Automatically tighten eligible directories.** This makes a trust decision
  for the person and hides the possibility of prior exposure or tampering.
- **Require only an external manual chmod.** It is safe to refuse, but the CLI
  can explain the problem and perform the same bounded operation after consent.
- **Move to a new private socket.** This leaves the running legacy server at
  the old endpoint and still needs a trusted handoff; changing the address
  cannot establish trust in that server or release its data-directory lock.

## Consequences and validation

The upgrade intentionally requires human intervention when legacy permissions
are unsafe. Issue #677's original successful-first-command criterion is narrowed:
`start` succeeds after an affirmative repair answer, not unattended. Declining
must fail without changing permissions or contacting the unsafe listener.

Implementation must exercise the release-site upgrade from v0.18.1 with a fresh
Linux account, empty HOME and unset `XDG_RUNTIME_DIR` and `TMPDIR`, seeding
running and stopped Docker boxes. Install the target release with the old server
running; make `start` the first command, inspect the warning, approve, and verify
the target CLI and server versions, private directories and preserved seeded
state. Repeat with No and without a terminal to prove refusal without mutation.

Filesystem coverage must include both base and child modes, partial repair and
retry, concurrent invocations, path changes during the prompt, foreign owners,
symlinks, writable ancestors, Linux ACL masks and Darwin ACL grants. Existing
hostile-path refusal coverage remains; the public-mode test must distinguish
strict validation from the explicitly approved repair flow. No claim that the
upgrade is fixed is made until implementation and this validation land.

The exact error representation, prompt integration and descriptor operations
remain implementation details subject to these constraints. Implementation
must update the root design and review guidance together with the runtime code;
this proposal does not describe the repair as already available.
