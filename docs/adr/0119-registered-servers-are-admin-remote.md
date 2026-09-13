# 0119 — Registered servers are `discobox admin remote`

- **Status**: Accepted
- **Supersedes**: [0116](0116-a-discobox-address-names-a-server-and-a-discobox.md) §3's command spelling — `discobox servers` becomes `discobox admin remote`. What §3 decides about the primary, registered servers, `servers.json`, and peer IDs stands.
- **Date**: 2026-09-13

## Context

[ADR 0116](0116-a-discobox-address-names-a-server-and-a-discobox.md) §3 put the
registry of servers a client lists discoboxes from at the root, as
`discobox servers` with `ls`, `add`, `rename` and `rm`. The top level holds
what somebody does with a discobox, and operating the system lives under
`discobox admin` ([ADR 0112](0112-the-top-level-is-for-people-not-a-transport-diagnosis.md)).
Adding and renaming servers is configuration a person does once per machine,
not work with a discobox.

Moving it under `admin` meets `discobox admin server`, which stages and runs
the API server process. Two commands one letter apart, one running a server and
one listing servers, is a spelling a person gets wrong and a completion that
offers the wrong one.

## Decision

The registry is `discobox admin remote`, with alias `remotes`. `add`, `rename`
and `rm` change it, and running it bare or as `ls` lists it. `discobox servers`
is removed from the root with no alias left behind.

`remote` is git's name for the same thing — other places this checkout's work
lives, added and renamed and removed by name — so the subcommands need no
learning. It is exact for every registered server; the primary it also lists
may be local, which the listing's PRIMARY column already says.

Go identifiers keep the server vocabulary (`serverRegistry`,
`newServersCommand`): what is registered is a server, and only the command is
spelled for the person, as `admin box` is for `newSandboxCommand`.

### Rejected

- **`admin servers` beside `admin server`.** Cobra accepts both, but the pair
  differs by a plural, and the registry's `server` alias could no longer exist.
- **Renaming the process command to `admin serve`, freeing `admin servers`.**
  It moves a command people already run to make room for the one that moved,
  and `admin serve logs` / `admin serve stage` read as verbs taking objects.
- **`admin registry`.** Matches the Go type, but "registry" already means the
  image registry the base, pool-agent, sandbox-agent and guest images are pulled
  from, which is the more common meaning in this repository.

## Consequences

- `discobox servers` is an unknown command. Scripts that used it change to
  `discobox admin remote`; `servers.json` and its contents are untouched.
- Help text, errors and design docs that named `discobox servers` name
  `discobox admin remote`. ADRs 0116 and 0117 still say `discobox servers`; read
  it as this command.
