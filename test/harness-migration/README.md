# Harness migration fixture

During upgrade validation, run this target from the repository on a Linux
amd64 host where v0.19.5 defaults to Docker. Pass the published release
candidate's tag.

```sh
go tool task release:validate-harness-migration CANDIDATE=v0.20.0-rc.2
```

The runner downloads both release manifests and CLI and server binaries
from the release asset site. It verifies the binaries against their
manifest digests and pins the v0.19.5 checksums. The server uses each
release's manifest for its base image, pool agent, sandbox agent, platform,
and harness bundles. The run uses a disposable server and keeps its
SQLite database and Docker home volumes in place across the upgrade.
Diagnostic logs are written under `build/ux-smoke/`, with the run directory
printed at cleanup.

The release validation result should record the candidate tag and the
migration verdict. Regular PR CI runs the focused sentinel-alias regression.

The account and settings inputs are in [homes.json](homes.json). The child
inherits Codex's bundle, saves a separate account, and becomes the project
default. The shell case exercises personal files and has no account or
portable settings. Native stores are populated with the vendor import
commands.

The assertions cover bundle refresh, restart, account handoff, save and
reset. The child leaves a settings capture pending on the release agent for
the upgrade to recover. The Codex parent retains an earlier sentinel in its
home across a controlled remint and refresh, then saves a settings edit while
that old alias is present. The runtime suite covers retry after an interrupted
OpenCode native handoff.
