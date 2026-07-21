# Stub harness image

A test-only harness whose configure flow succeeds instantly without any real
credential. It exists to exercise and troubleshoot the harness configure
pipeline end to end — the only other way to drive that pipeline is a real
harness (codex, claude-code, opencode), which stops at an interactive
credential prompt.

One run of `configure` against this image proves the whole chain:

- the ephemeral `harnessMode=config` sandbox starts and defers its primary
  terminal
- `configure/attach` seeds `/run/discobox/harness-previous-config.json`
  **before** the configure command runs (the script echoes what it found)
- attaching the virtual `primary` exec launches the configure command
- the command writes `/run/discobox/harness-configure.json` and exits 0
- `configure/commit` reads the real exit status, applies the declared secret
  (`STUB_TOKEN` → `stub-token`, with its binding and `harnessConfig`-scoped
  grant) and file (`stub.json`), marks the harness configured, and deletes the
  sandbox

Running configure a second time additionally proves the reconfigure path. The
echoed previous configuration must now list the `STUB_TOKEN` secret and the file
from the first run, and must contain **no secret value** — the value is offered
as `$PREV_STUB_TOKEN`, a sentinel the proxy swaps only while a live grant covers
it. Afterwards exactly one `stub-token` secret must exist (reconfigure replaces
the previous generation instead of leaking it).

Set `STUB_CONFIGURE_KEEP=1` in the configure sandbox to exercise the other half
of that path: the command returns `usePrevious` instead of a value, and the
secret from the first run must survive with its ID, binding, and grant intact.

## Usage

```bash
go tool task build:harness-stub-image   # uses the dev sandbox-agent image from .env when present
disco box harness create --image discobox-harness-stub:local
disco box harness configure stub        # non-interactive; exits on its own
disco box harness deconfigure stub      # removes the secret + file, marks unconfigured
disco box harness delete stub
```

To exercise the failure path (commit must record `configureError` and leave the
harness unconfigured), bake or override `STUB_CONFIGURE_EXIT` to a non-zero
value in the configure sandbox's environment.

## Notes

- The image must be rebuilt when the sandbox-agent base changes; the build task
  resolves the base from `.env` (`DISCOBOX_DEFAULT_SANDBOX_IMAGE`) so it tracks
  the image watcher's dev builds, falling back to
  `discobox-sandbox-agent:local`.
- `image.json` and the `io.discobox.harness.v1` label carry the same harness
  object, like the real harness images: the label is what the control plane
  validates at registration, and `/usr/share/discobox/image.json` is what the
  sandbox-agent reads at runtime. An image with only the label registers fine
  but fails at attach with "exec command is required".
- This directory is deliberately under `test/`, not `harness/`: it is a
  fixture, not a shipped harness, and the image watcher and server seeding do
  not know about it.
