# 26-11-01-122 — A harness is only a bundle, and a sandbox takes its kind and platform from its pool

Status: Proposed
Date: 2026-11-01

Supersedes, if accepted:
[0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md)
§3's manifest shipped "as one of the overlay's own files" — the manifest-file
harness, named by a `file://` reference into the server's overlay directory —
and its Consequence that "the harness catalog gains a platform per entry, so a
client offers only what a pool can run";
[26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md)
§4's "a harness's image declares its kind" and "a sandbox is placed only where
both match", and its Consequence that "a harness's image and a pool declare an
image kind, and placement matches it with the platform".

Narrows: 0145 §1's "a sandbox's template declares the platform it runs" —
nothing a sandbox runs declares one, and its platform is its pool's; §1's
placement on a pool of the sandbox's own platform stands. 0145 §3's rule that
what a platform lacks is declared rather than probed stands as a refusal and a
contract: `harness.ImageMetadata.ValidateFor` keeps the non-Linux rules of 0145
§§3, 5, by which a non-Linux bundle would be judged, but every manifest
registered now is a bundle's, judged as Linux, so a label naming an account or
a shell is refused.

Completes [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
§2. Issue [#426](https://github.com/discobox-ai/discobox/issues/426).

## Context

26-10-16-423 §2 made a harness a bundle and gave the image kind to the
provider: a bundle names no kind, and places on any provider whose platform
its steps select. The implementation stopped halfway. Registration stamped
`oci` on every bundle, and create, a harness change, and placement compared
that kind with the pool's. Every bundle harness, `shell` included, ran only on
a Docker pool:

```
$ discobox run -H shell --pool boxd --no-source
409 Conflict: harness "shell" cannot run on pool "boxd": its image is an OCI
image, and the pool runs VM images for the boxd driver
```

The stamp survived because one kind of harness still needed a kind to compare:
the manifest-file harness, an `Image` of `file://<path>` naming the JSON
manifest of a non-Linux template with no image (0145 §3), which declared one
platform and a `vm/<driver>` kind. Nothing makes one any more. Its producer was
0145 §2's pool-assembled template, which 26-10-09-106 superseded; nothing in
the repository writes a manifest file to the overlay directory; and no provider
or pool runtime reads the image pin a sandbox on one carries. A VM pool
provisions bundles like every other box (26-10-16-423, 26-10-23-551,
26-10-24-953).

## Decision

### 1. A harness is a bundle and nothing else

A harness config records its bundle, its digest, and the manifest composed from
it. It has no image, image digest, image kind or platforms; the create body has
no field for one, and a request that sends `image` is refused as a bad request.
`overlayDir` is retired: nothing is staged there.

### 2. A sandbox's kind and platform are its pool's

The same harness runs in a Docker pool's container and on a boxd pool's
machine. Create, a harness change, an upgrade and placement compare no harness
kind or platform: a sandbox takes its pool's platform, as it already did, and
is whatever kind of box its pool runs. A bundle label naming a platform or a
kind is refused at registration, as before.

### 3. Import is refused by the pool's kind

An archive holds a container discobox's tree, and machine export and import
stay deferred (26-10-09-106). Import is refused onto a pool that runs anything
but OCI images, for the pool's kind, never a harness's; the refusal names the
pool's kind alone.

### 4. Existing databases keep their rows

The four columns are dropped in place. Every row stays: one that recorded a
manifest file, or a runnable image from before bundles, has no bundle, which
create and repair already refuse with 409 naming
`admin harness update --bundle`. What each recorded is logged before the drop.
A sandbox's own image pin stays, since a container from before bundles is
fingerprinted by it until that sandbox is pinned to its bundles.

## Alternatives rejected

- **Keep the manifest-file harness and skip the kind check for bundles.** It
  keeps a kind on the harness that only a dead path sets, a migration that
  repairs it, an overlay directory the server reads from on a member's say-so,
  and a second pin a sandbox carries, all for a registration nothing produces.
  A non-Linux template, when one exists again, is a bundle a VM pool provisions
  or a decision of its own.
- **Default the kind to the pool's at create.** It records on the harness a fact
  that is the pool's, which is wrong as soon as the harness runs on a second
  kind of pool.

## Consequences

- `discobox run -H shell --pool boxd` creates a box that applies the `shell`
  bundle.
- The CLI offers every harness: it no longer filters by pool platform or kind,
  and `admin harness create` has no `--manifest-file`.
- The manifest a bundle composes to no longer carries `imageKind: oci`, so its
  `ManifestDigest` moves once and boxes on it report an upgrade, as a release
  that edits the platform layer does.
