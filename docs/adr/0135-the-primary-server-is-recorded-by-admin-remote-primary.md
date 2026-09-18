# 0135 — The primary server is recorded by `admin remote primary`

- **Status**: Accepted
- **Date**: 2026-09-18
- **Supersedes**: [0116](0116-a-discobox-address-names-a-server-and-a-discobox.md)
  §3's "the primary is `--server`, `DISCOBOX_SERVER`, or the local default",
  and §3's rejection of a kubectl-style current context as far as that
  rejection covers a recorded default. What §3 rejected it for stands: every
  listing still spans every server, and switching the primary changes where a
  discobox is created and what a command without an address talks to, never
  what is seen.

## Context

ADR 0116 §3 left the primary to `--server`, `DISCOBOX_SERVER`, or the local
server. The primary is where a discobox is created and what every command
without an address talks to. A person whose work lives on a remote server has
to type `--server` on every command, or set an environment variable per shell,
to make that server the default. A registered server can be listed but never
made the default.

## Decision

1. `discobox admin remote primary NAME` makes a registered server the primary.
   `servers.json` records it as `primary`, the server's address. `--server` and
   `DISCOBOX_SERVER` still win for the command they are given to, and the local
   server is the primary when nothing is recorded. Choosing the local server
   records nothing.
2. Recording a primary registers nothing unless `--register-current` asks.
   Without it, a primary being replaced that nothing registered is no longer
   listed until `admin remote add` registers it. That can be a server only
   `--server` named, or the local server. With it, that primary is registered
   in the same write under the name it offers, as `add` would. As with `add`,
   it has to answer first. It is not started to find out, and a primary that
   does not answer fails the command before anything is written.
3. `admin remote rm` refuses the recorded primary. Something has to be the
   primary, and which server replaces it is the person's to say.
   `admin remote primary` with no name prints the primary.

A recorded primary is a current context in the narrow sense 0116 rejected:
one server that commands default to, switched between. 0116 rejected it
because `--server` already chose a server per command and seeing several at
once was the point. The second reason still holds. The first did not survive
use: `--server` on every command, or an environment variable set in every
shell, is the cost of working mainly on a server other than the local one.

### Rejected

- **Recording the primary by name.** Every rename would have to update it.
  The address is what the primary is resolved to either way, and `serverKey`
  already matches it to its registration.
- **Clearing the primary when its entry is removed.** The next command would
  quietly talk to the local server, and would start it.
- **Always registering the primary being replaced.** It would keep every
  server that had been the primary in the listings. But a server that is down
  when the primary moves (usually the local server nobody has started) would
  be reported as not answering by every listing, since a registered server is
  never started. Registering it only when it answers would make what gets
  listed depend on whether a server happened to be up. So it is opt-in, and
  when asked for it behaves as `add` does.
- **A flag on `admin remote add`.** It only covers a server being added, and
  switching between registered servers is the common case.

## Consequences

- Every command without `--server` reads `servers.json`, so a file that does
  not parse fails those commands rather than only the listings. `version` and
  `admin uninstall` still skip that failure.
- `admin remote primary` says so when `--server` or `DISCOBOX_SERVER` is
  given, since the recorded primary does not apply there.
- The root's `--server` default in help is still the local endpoint. The
  recorded primary replaces it after flags are parsed.
