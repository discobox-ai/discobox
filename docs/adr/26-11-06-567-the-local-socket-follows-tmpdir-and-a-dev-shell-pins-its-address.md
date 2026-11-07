# 26-11-06-567 — The local socket follows TMPDIR, and a development shell pins its address

- **Status**: Accepted (supersedes [26-11-04-074](26-11-04-074-local-server-discovery-is-independent-of-shell-temporary-directories.md) §1 and [26-11-05-240](26-11-05-240-runtime-directory-permission-repair-requires-human-consent.md); 26-11-04-074 §§2–4 remain)
- **Date**: 2026-11-06

## Context

ADR 26-11-04-074 §1 moved the default Unix socket from
`$TMPDIR/discobox-<uid>/…` to a fixed `/tmp/discobox-<uid>/…`, so that a
server started inside a Nix development shell — whose devShell replaces
`TMPDIR` with a fresh, short-lived `/tmp/nix-shell.XXXXXX` — could be found
from any other shell. Moving into shared `/tmp` then needed ownership, mode
and ACL validation on both bind and dial, and ADR 26-11-05-240 added a
consent-gated repair of directories a validated path rejected.

That traded every macOS user's upgrade for a development-environment problem.
On macOS, `TMPDIR` is launchd's private per-user directory
(`/var/folders/…/T/`), so every released server before the change listens
there. A 0.19 CLI dials `/tmp`, cannot reach the older server to replace it,
launches another, and fails on the data-directory lock. The automatic upgrade
in `pkg/endpoint/autolaunch.go` only works for an incumbent it can reach.

The cause was never that discobox respected `TMPDIR`: an environment rewrote
the variable the rendezvous point is derived from. That environment is ours.

## Decision

### 1. The default socket follows TMPDIR again

`XDG_RUNTIME_DIR/discobox/server.sock`, otherwise
`os.TempDir()/discobox-<uid>/discobox/server.sock` — the default before
26-11-04-074. On macOS that is the private launchd directory; nothing is
validated or repaired, because nothing is moved into a shared directory. The
privacy validation and consent-gated repair of 26-11-04-074 §1 and
26-11-05-240 are removed with the path that needed them.

### 2. `DISCOBOX_SERVER_ADDRESS` names the local endpoint outright

`endpoint.DefaultEndpoint` returns `DISCOBOX_SERVER_ADDRESS` when it is set,
bypassing the platform default. Both ends read it — the server binds it and
the CLI dials it — and the CLI forwards it to a server it autolaunches as a
systemd user unit, whose environment is otherwise a chosen list. It replaces
only the local default: `--server`, `DISCOBOX_SERVER` and a primary remote
still take precedence on the client, and `DISCOBOX_SERVER_LISTEN` on the
server.

### 3. The repository's flake pins it

The devShell's `shellHook` sets `DISCOBOX_SERVER_ADDRESS`, unless already set,
to the address a plain shell resolves: `XDG_RUNTIME_DIR`, else on macOS
`getconf DARWIN_USER_TEMP_DIR` (the directory launchd sets `TMPDIR` to), else
`/tmp`. A server started inside `nix develop` is the one a plain shell finds,
and the reverse.

## Alternatives rejected

- **Keep `/tmp` and probe the old `TMPDIR` path during upgrade.** A
  compatibility shim that only finds the old server when the CLI's `TMPDIR`
  matches the server's, and keeps the validation 26-11-04-074 needed.
- **Derive the macOS default from `DARWIN_USER_TEMP_DIR` rather than
  `TMPDIR`.** Fixes Nix without an environment variable, but Go exposes no
  `confstr`; it needs cgo in the CLI or an exec of `getconf` on every
  invocation. The development shell is the one environment that rewrites
  `TMPDIR`, so it is where the fix belongs.
- **Pin `DISCOBOX_SERVER` in the shell.** Fixes only the client; the server
  derives its socket from the same default (the reason the earlier `.envrc`
  workaround restored `TMPDIR` instead).

## Consequences

Upgrading from a release that listened under `TMPDIR` replaces the older server
automatically again. A Nix-flake project other than this repository, run in a
shell that rewrites `TMPDIR`, must set `DISCOBOX_SERVER_ADDRESS` itself; the
unreachable-holder error of 26-11-04-074 §3 names the holder's recorded address
when that is missed. On Linux without `XDG_RUNTIME_DIR` or `TMPDIR`, the socket
is in shared `/tmp` without validation, as it was before 26-11-04-074.
