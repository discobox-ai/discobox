# Harness Configs Review Notes

- **Every input a config's snapshot is composed from must move a digest a
  sandbox pins.** The snapshot is the platform layer and the harness bundle's
  labels (`resolveHarness`), and seeding's freshness check, the re-pin of
  stopped sandboxes and a container's rebuild key on `BundleDigest` and
  `ManifestDigest`. A platform-layer edit moves no bundle digest: only the
  composed manifest's digest carries it. An input that can change without
  moving either is a manifest edit that reaches no existing sandbox — the
  platform layer moved off the runnable image's digest once, and nothing
  failed. The same holds for every consumer that decides by a pin, not only
  the sandbox fingerprint: a consumer once matched on `BundleDigest` alone and
  kept a box composed on the old platform layer, the second time this mistake
  was made.
- **Tell a bundle from an image by `ReclaimLabel`, never by a manifest
  label.** A release from before bundles shipped runnable images carrying the
  manifest labels too, so a check that a bundle carries `10-platform` or
  `io.discobox.image.v1` admits one of them as a bundle. Every image built
  `FROM` `discobox-sandbox-base` inherits `ReclaimLabel`; a bundle is `FROM
  scratch` and carries none. The shared label has misled a check twice.

- Preview reads are observations: use `SandboxObserve`, never `SandboxAction`.
  The console polls them automatically, and a stale running listing can race
  a stop; an action would wake the box again.
- Compare settings without converting JSON numbers to float64. Adjacent large
  integers can collapse to one float, hiding a real change from the save offer.
- Refuse inheritance from a parent whose `FileOverrides` is nil until it has
  refreshed. Its legacy `Files` may still mix personal account files with the
  bundle baseline; copying them would leak personal state into another config.
- A writer that changes a binding on someone else's behalf (`removeStrandedBindingsTx` taking a stranded route) reads that binding with `store.LockSecretBinding` in its transaction and takes only what it means to from the row as it now stands. Why: `UpdateSecretBinding` writes every saved column, so writing back a copy listed earlier would undo a person's concurrent edit (delegate turned off, raw taken away, a shorter expiry), recorded as the harness's change (#1105 review).
