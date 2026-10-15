# 26-10-15-629 — The VM provider and its image kind are spelled `vm`

- **Status**: Accepted
- **Date**: 2026-10-15
- **Supersedes**, in part:
  [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md)
  §1's provider type `discovm` and §4's image kind spelling
  `discovm/<driver>`. Nothing else in it changes.

## Context

ADR 26-10-09-106 named the pool runtime provider `discovm` and spelled the
kind of a machine image `discovm/<driver>`, after disco-vm, the engine the
server embeds. Both are strings a person reads and writes: the provider type
in the catalog, in `discobox admin provider` and in a provider instance's
record; the kind in the API, in a manifest file's `imageKind`, and in the
refusal a sandbox gets when its image and its pool's kinds differ.

disco-vm is how the server runs those machines, not what a person is choosing.
What they choose is a VM: a provider whose sandboxes are machines, and an
image built as one for a driver.

The image kind shipped in v0.14.0. The provider has not shipped.

## Decision

### 1. The provider type is `vm`

The pool runtime provider of 26-10-09-106 §1 is registered as `vm`, and its
catalog name is `vm`. No user-facing string — its description, its fields',
its errors, its CLI help — names disco-vm or `discovm`; they say VM. The Go
package and the developer docs still call the engine disco-vm, which is what
it is.

### 2. The image kind is spelled `vm/<driver>`

26-10-09-106 §4's kind of a machine image is `vm/<driver>` — `vm/boxd`,
`vm/vz` — and its descriptions say "a VM image for the boxd driver". `oci` is
unchanged.

The kinds a release has stored are rewritten when the server starts: every
pool's and harness's `discovm/<driver>` becomes `vm/<driver>`. Only one
spelling is read afterwards. A `discovm/<driver>` that arrives later is
refused rather than read as an alias that a stored or declared kind could
silently change into: from a manifest file written for an earlier release, with
a message that names `vm/<driver>`; from a pool agent's report, by the API
schema's pattern, as any other unreadable kind is.

The kind a server returns on a harness or a pool carries no pattern in the API
schema, so a client validates none: a client then reads a kind spelled by a
server of another release, and only the server checks a kind it is sent.

## Alternatives rejected

- **Keep `discovm`.** It names the implementation, and every person who
  configures a provider or writes a manifest file has to learn it.
- **Read `discovm/<driver>` as an alias for good.** No release had a provider
  that ran such an image, so a harness declaring one could never be placed;
  what an alias would keep working is nothing. It would also be the second
  spelling of a placement key that is compared as a string.

## Consequences

- A migration rewrites `image_kind` on pools and harness configs, on every
  start; after the first it matches nothing.
- A manifest file that declares `discovm/<driver>` is refused on refresh until
  it says `vm/<driver>`.
- A CLI from v0.14.0 to v0.16.0 validates the kinds a server returns against
  its own `discovm/<driver>` pattern, so against a server from this release it
  fails to list harnesses, or pools, once one has a VM kind. Only a manifest-file
  harness has one — a macOS template — and no released provider runs a VM
  pool; such a CLI is upgraded with the server.
- The provider's state root on a host is under `discobox/vm/<driver>`.
