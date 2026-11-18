# Harness migration fixture

Run `go tool task ci:harness-migration` on Linux amd64 with Docker. The CI job
uses the same target.

The runner verifies the published v0.19.5 CLI and server digests, then uses
those binaries to generate a SQLite database and Docker homes. The checked-in
home inputs contain synthetic credentials. The release CLI saves them through
the normal capture flow. The runner keeps the database and home volumes in
place when it starts the checkout's server and provisions the checkout's
bundles. Each run creates its own disposable server identity, pool and boxes.

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
