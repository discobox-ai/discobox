# 26-10-27-172 — A harness config attached to a discobox binds it for use

- **Status**: Accepted (narrows [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md) §2's three levels)
- **Date**: 2026-10-27

## Context

A discobox runs one harness config, and ADR 26-10-27-880 §2 binds it at three
levels: its own bindings, its harness config's, and its project's. A
discobox can also run a second harness for one invocation
(`POST /sandboxes/{id}/harness-secrets`): that harness needs its harness
config's credentials in the discobox, and before bindings it got them as
sentinels that still needed a grant.

With grants gone, the second harness's credentials have to reach the discobox
through a binding of some level.

## Decision

### 1. An attached harness config is a fourth level, for use

Running a second harness in a discobox attaches its harness config to the
discobox, recorded on the sandbox (`AttachedHarnessConfigIDs`). Per variable,
the narrowest binding still wins, in this order: the discobox's own, its
harness config's, its attached harness configs', the project's.

An attached harness config gives the discobox its **use** bindings only. Its
delegate bindings do not cover the discobox: attaching runs a harness, and
running a harness is not the power to hand its credentials on (880 §3).

Attaching is a person's act; a discobox may run a harness already attached to
it. An attachment lasts as long as the discobox, or until its harness config
is deleted.

### 2. Changes to the harness config reach it

Because the binding is the harness config's, read live, binding, narrowing or
removing it at the harness config reaches the discobox as it reaches every
discobox running that config, and the same delivery marks it.

## Alternatives rejected

- **Copy the harness config's bindings onto the discobox.** It was the first
  build. A copy is the discobox's own binding: removing the binding at the
  harness config leaves the copy standing, and nothing ties the two.
- **Attached configs carry their delegate bindings too.** A discobox with the
  discobox API could then hand on a harness's credentials to the discoboxes it
  creates because a person once ran that harness in it.

## Consequences

Resolution, marking and the resolve check read four levels instead of three.
The sandbox carries the list of harness configs attached to it, written by
its own setter and never by a whole-row save.
