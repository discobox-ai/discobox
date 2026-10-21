# Harness Configs Review Notes

- **Every input a config's snapshot is composed from must move the digest a
  sandbox pins.** The snapshot is the platform layer and the harness bundle's
  labels (`resolveHarness`), but seeding's freshness check, the re-pin of
  stopped sandboxes and a container's rebuild all key on `ImageDigest`. An
  input that can change without moving the runnable image's digest is a
  manifest edit that reaches no existing sandbox — the bundle's labels and the
  platform layer moved off the runnable image once, and nothing failed.
- **Tell a bundle from a runnable image by `ReclaimLabel`, never by a
  manifest label.** The runnable images carry the manifest labels too (the
  note above), so a check that a bundle carries `10-platform` or
  `io.discobox.image.v1` admits the sandbox agent image or a twin as one. Only
  a runnable image inherits `ReclaimLabel` from `discobox-sandbox-base`; a bundle is
  `FROM scratch` and carries none. The shared label has misled a check
  twice.
