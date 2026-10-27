# Stub harness

A test-only harness whose configure flow succeeds instantly without any real
credential. It exists to exercise and troubleshoot the harness configure
pipeline end to end — the only other way to drive that pipeline is a real
harness (codex, claude-code), which stops at an interactive
credential prompt.

One run of `configure` against this harness proves the whole chain:

- the ephemeral `harnessMode=config` sandbox starts and defers its primary
  terminal
- `configure/attach` seeds `/run/discobox/configure/harness-previous-config.json`
  **before** the configure command runs (the script echoes what it found)
- attaching the virtual `primary` exec launches the configure command
- the command writes `/run/discobox/configure/harness-configure.json` and exits 0
- `configure/commit` reads the real exit status, applies the declared secret
  (`STUB_TOKEN` → `stub-token`, with its harness-config binding) and file
  (`stub.json`), marks the harness configured, and deletes the
  sandbox

Running configure a second time additionally proves the reconfigure path. The
echoed previous configuration must now list the `STUB_TOKEN` secret and the file
from the first run, and must contain **no secret value** — the value is offered
as `$PREV_STUB_TOKEN`, a sentinel the proxy swaps only while a live binding
covers it. Afterwards exactly one `stub-token` secret must exist (reconfigure replaces
the previous generation instead of leaking it).

Set `STUB_CONFIGURE_KEEP=1` in the bundle's env to exercise the other half of
that path: the command returns `usePrevious` instead of a value, and the
secret from the first run must survive with its ID and binding intact.
It only keeps when there is something to keep — claiming `usePrevious` on a
first run is a commit error, and a real harness makes the same check before
offering to keep a credential.

`test/bats/harness_configure.bats` automates all of the above; run it with
`go tool task test:docker:bats BATS_SUITE=test/bats/harness_configure.bats`.
The keep case runs against a derived bundle, `discobox-harness-stub-keep:local`,
built `FROM` the stub's with only its label changed and registered beside the
stub under its own slug.

The toggle has to reach the configure command as harness env, and harness env
comes from the bundle's `io.discobox.image.v1` label: its `env` is merged into
the sandbox's manifest, and the sandbox-agent applies the manifest's env when it
starts the command. A Dockerfile `ENV` does not work — that belongs to the
container, whose PID 1 is systemd, and systemd does not pass its own
environment to the services it starts.

## Usage

```bash
go tool task build:harness-stub-bundle
discobox admin harness create --bundle discobox-harness-stub:local
discobox admin harness configure stub        # non-interactive; exits on its own
discobox admin harness deconfigure stub      # removes the secret + file, marks unconfigured
discobox admin harness delete stub
```

To exercise the failure path (commit must record `configureError` and leave the
harness unconfigured), add `STUB_CONFIGURE_EXIT` with a non-zero value to
`image.json`'s `env` and rebuild the bundle, so it reaches the label.

## Notes

- `Dockerfile` is the bundle alone, as every harness's is: `FROM scratch`,
  holding the stub's feature, `/features/stub/`, whose `provision.yaml`
  installs `configure.sh` as `/usr/local/libexec/discobox/configure-stub`,
  the config-mode command `image.json` declares; its label is `image.json`
  (the required `HARNESS_METADATA` build arg). A box gets the feature from
  the bundle (ADR 26-10-16-423 §7); no harness is an image (§10).
  `create` takes the slug `stub` from the harness id.
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
- This directory is deliberately under `test/`, not `harness/`: it is a
  fixture, not a shipped harness, and the image watcher and server seeding do
  not know about it.
