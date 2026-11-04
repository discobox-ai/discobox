# Stub harness

A test-only harness whose terminal is a plain `sh`. It installs no CLI and
needs no credential, so a box on it is ready as soon as its platform bundle is
applied. The end-to-end suites that need *a* harness-backed box but test
nothing about harnesses — `sandbox_upgrade.bats`, `sandbox_transfer.bats`,
`ssh.bats` — register it under a slug of their own and create boxes on it.

Every registered harness runs: there is no configure step and no enable flag
(ADR 26-11-02-152 §1), so registering the stub is all a suite has to do before
`admin box create --harness <slug>`.

## Usage

```bash
go tool task build:harness-stub-bundle
discobox admin harness create --bundle discobox-harness-stub:local
discobox admin box create --harness stub --wait
discobox admin harness delete stub
```

## Notes

- `Dockerfile` is the bundle alone, as every harness's is: `FROM scratch`,
  holding only its label, `image.json` (the required `HARNESS_METADATA` build
  arg). Like `shell`'s, it carries no feature: its terminal is the `sh` the
  platform already ships. `create` takes the slug `stub` from the harness id.
- The server must be able to serve the bundle to a pool: from a registry, or,
  on a development server, from the development bundles its watcher records
  (ADR 26-10-16-423 §13). One only a local daemon holds registers, and boxes
  on it fail to fetch it.
- `image.json` is the authoring source for the bundle's `io.discobox.image.v1`
  label and nothing else — there is no file inside the bundle (ADR 0012 §6).
  It declares only what is this stub's, plus the `platformAPIVersion` every
  harness bundle must name: env, volumes and groups come from the platform
  bundle's layer, which the server composes under the bundle's own
  (ADR 26-10-16-423 §2).
- `sandbox_upgrade.bats` derives a bundle `FROM` this one with only its label
  changed, to move the digest a box pins.
- This directory is deliberately under `test/`, not `harness/`: it is a
  fixture, not a shipped harness, and the image watcher and server seeding do
  not know about it.
