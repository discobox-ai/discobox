# 0112 — The top level is for people, not for a transport diagnosis

- **Status**: Accepted
- **Date**: 2026-09-12
- **Relates to**: [ADR 0100](0100-the-prompt-is-a-flag-and-the-root-takes-no-words.md),
  which settled what the root command's words are for.

## Context

`discobox status` was a top-level command, and the reason was written into its
test: it is what somebody reaches for when nothing else works, and hunting
through `admin` for it is one step too many at that moment.

What it prints is a transport diagnosis. One row per layer, in the order a
connection passes through them — address, this machine's identity, the socket,
the relay, the connection to the peer, whether the server admits this machine,
whether it is ready, the route traffic ends up taking — then the server's own
account of its iroh listener, then the layer to fix. It is the right output for
the problem, and it is an operator's output: relays, admission, direct
addresses, round trips.

The top level is the surface a user meets. `run`, `ls`, `attach`, `shell`,
`apply`, `push`, `proxy`, `cp`, `tools`, `secret`, `configure` — each is a thing
somebody does with a discobox. A layer-by-layer transport report standing among
them is the one word in that list that is not about the user's work.

The cheap half of the question that sent people there — is a server reachable,
and which versions are these two ends running — is now answered by
`discobox version` and `discobox --version`, which print the client's version
and the server's, say `unavailable` when nothing answers, and never start a
server to find out.

## Decision

### 1. The diagnosis moves to `discobox admin server status`

The top level holds commands chosen for how well they serve somebody using
discobox. Everything that exists for operating the system lives under `admin`,
and a report about reaching the server belongs with the rest of the server's
commands.

### 2. The retired word gets no alias and no pointer

`discobox status` is an unknown command, answered the way any other unknown
command is. It is not aliased, and the error is not special-cased to name the
new path.

## Alternatives rejected

**Keep it at the top level.** The previous decision, and the discoverability
argument behind it is real: a broken connection is exactly when a user does not
want to go looking. It loses to what the top level is for. The command is
reached from documentation and from error hints that name it in full, not by
browsing a command list, so the placement costs discoverability that was
largely theoretical — and `discobox version` now covers the common case without
the diagnosis.

**A hidden top-level alias.** Cheap, and nothing would break. Rejected because
the command would then live at two paths indefinitely, and the technical word
would stay in the namespace this decision is clearing.

**Special-case the error to point at the new path.** Rejected: every command
that ever moves would earn the same treatment, and cobra's suggestions exist for
misspellings rather than for relocations.

**`discobox admin status` rather than under `server`.** Rejected because the
report is about reaching the server, which is where `stage`, `manifest`,
`shutdown`, and `logs` already are.

## Consequences

- `discobox status` fails with `unknown command "status"`. Scripts and habits
  that used it move to `discobox admin server status`, which keeps its `-o
  json` report and its non-zero exit for an unreachable server.
- Hints and docs that named the old spelling name the new one.
- `discobox version` and `--version` answer "is it up, and what are we
  running" without the diagnosis, so the full report is what somebody reaches
  for second rather than first.
