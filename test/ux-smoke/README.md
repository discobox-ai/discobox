# Docker UX smoke

`nix develop -c go tool task ci:ux-smoke` is the required Linux CI lane for
send → send-key → wait → capture (and timeout exit 124), reviewed harness
candidate save and default reset, and sandbox-requested adoption with person
approval/denial and release. Missing prerequisites and failed cases fail the
job; there are no opt-in gates or skipped cases. It needs a Docker daemon.

The runner builds the CLI, server, sandbox base and OCI bundles from this
checkout. Each run has its own image tag, database, directories,
port and socket under `build/ux-smoke/<run>`. It never joins `task dev` or
uses the enclosing box's API identity. OCI builds use the host builder with
the containerd image store, or a run-owned fallback builder on classic Docker.
The selected builder is bootstrapped before concurrent builds connect, so a
new fallback container must have a ready BuildKit socket first. On failure its
container state and daemon logs are retained before the builder is removed.
The synthetic shell harness exports a
JSON settings file and a fake token scoped to `ux-smoke.invalid`; it has no
login or external credential requirement. Save and reset assert creation and
removal of the synthetic shared binding as well as settings.

Bootstrap runs at most three builds concurrently, with both host binaries built
in one Go invocation. It keeps the existing Go/BuildKit caches and performs a
fresh bootstrap on every run. The terminal box also serves as the adoption
requester; three boxes retain every flow and the distinct creator assertion.
Build, flow and cleanup timings are printed in CI, and command durations remain
in the logs. Concurrent builds have separate logs and are joined before cleanup.

Every command and readiness wait is bounded; Taskfile bounds the whole run to
25 minutes. Before teardown it records screens, container logs and system journals. The runner
purges its boxes, unsets its default pool, deletes its pools and removes its
builder and base image tag. Its fallback uses only pool IDs from its private
database, including a pool seeded during failed bootstrap. Cleanup errors fail
an otherwise successful run. CI retains command/server/pool logs, candidate
previews and screen captures on failure for seven days; build archives and
binaries are removed rather than uploaded.
