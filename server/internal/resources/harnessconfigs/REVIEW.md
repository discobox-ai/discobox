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
