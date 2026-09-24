# Driving the `task dev` loop inside a discobox

Shared reference for anything that exercises this checkout end to end:
reproducing an issue (`triage-issue` §3) and QA-verifying a fix (`test-fix`
phase 2). Linux discobox only; Windows and macOS have no `task dev` here.

## The loop is already running — do not start it

The repository declares `task dev` as a discobox service
(`.discobox/services/10-discobox-api.sh`), and the box starts it at boot,
before any harness session begins. It is running before you read this. **Do not
run `task dev`, `task dev:server`, or `discobox-server` yourself** — a second
copy is refused by `discobox-dev-lock`, or, run bare, it would bind other
directories and endpoints than the service does and fight it for the socket.

Confirm it is up rather than assuming:

```bash
pgrep -af 'discobox-dev-lock (server|docker-image-watch)'   # both halves
curl -fsS http://127.0.0.1:8080/openapi.yaml >/dev/null && echo up
```

A service that exits stays exited; nothing restarts it and there is no in-box
command to. Only when both checks fail, read `go tool task check:dev-build`
for why it stopped, then start it the way the box does — the service script,
in the background, which sets the port and `.tmp/discobox` paths bare `task
dev` would not:

```bash
.discobox/services/10-discobox-api.sh   # run_in_background; never bare `task dev`
```

The loop watches the whole tree and rebuilds on any change — including a `git
switch` — so checking out a commit *is* building it:

- `build/discobox` and `build/discobox-server` are rebuilt; the server is
  restarted on `http://127.0.0.1:8080` with its data under `.tmp/discobox`.
- The pool, sandbox, and harness images are rebuilt by the image watcher.
- **Always pass `--server http://127.0.0.1:8080`.** The box sets
  `DISCOBOX_SERVER=https://api.discobox.internal`. The flake's dev shell unsets
  it, but a shell that has not loaded the dev shell (an agent's tool shell,
  usually) still has it, and a CLI there without the flag talks to the *outer*
  discobox API — the one running this box — not the build under test. A shell function keeps it honest:

  ```bash
  d() { ./build/discobox --server http://127.0.0.1:8080 "$@"; }
  ```

Never start a second server or run `discobox-server` directly; use the one the
loop runs.

## Waiting for a rebuild

A rebuild is done when `d --version` reports a server version starting with
the checkout's `git rev-parse --short=12 HEAD`, and `go tool task
check:dev-build` passes (a `wnb-*-failed.txt` in the repo root means it did not
build or would not start). Not binary mtimes: `go build` leaves an identical
output untouched. Wait on that with Monitor, not a sleep loop.

A tree with uncommitted changes reports `<sha>+dirty`, which says *that* the
build has edits but not *which* — every edit to a dirty tree reports the same
string. For an uncommitted change, wait until `check:dev-build` passes and the
server process started after your last edit (`ps -o lstart= -p "$(pgrep -x
discobox-server)"`), and say in the report that the build under test is
HEAD plus the working tree.

## Images

Images are built reproducibly (created time is always 1980) and tagged by
content, `dev-<digest>`; the watcher writes the new tags into
`.tmp/discobox-dev-images.json` and `.env`, and the `.env` change restarts the
server onto them. Never `docker build` an image by hand to get ahead of the
watcher — the server would not use it.

When the change lives in an image, check whether its inputs changed — its own
and every image it is built `FROM`. There are two chains, both from
base-image: base-image → sandbox-agent → harness, and base-image → pool-agent;
each agent image also bakes in binaries from its own module and the root
packages. `git diff --stat <from> <to> --` over the chain the image is on. If
none changed, the image is identical and there is nothing to wait for. If any
did — between a release tag and `main`, nearly always — save
`.tmp/discobox-dev-images.json` before the change or switch and wait until
that image's `reference` in it changes.

A box created before an image rebuild keeps the old image; create a fresh box
(or `d admin box upgrade <id>`) to test the new one. `d admin box ls` shows
an `UPGRADE` column when a box is behind.

## Making a box to test in

The `shell` harness needs no credentials and starts in seconds:

```bash
id=$(d -H shell -d --no-source -o json | jq -r .id)   # returns while still starting
d admin box get "$id" -o json | jq -r .runtime.state   # pending → ready
d shell "$id" -- sh -c 'id; pwd; systemctl is-system-running'
```

- `-C <dir>` cuts the box from a source directory instead of `--no-source`;
  `--include-dirty=false` keeps it from asking about uncommitted work.
- `-H claude-code` / `-H codex` need a configured harness (`d admin harnesses
  ls`, CONFIGURED column); unconfigured, that is **blocked**, not failed.
- `docker ps --format '{{.ID}}\t{{.Image}}\t{{.Names}}' | grep "$id"` finds
  the container; it is named `discobox-sandbox-<pool>-<sandbox-id>`.
- Inside it: `docker exec <container> systemctl is-active <unit>`,
  `docker exec <container> journalctl -u <unit> --no-pager -n 30`.
- The server's own view: `d admin box get`, `d admin job`, `d admin audit`,
  `d admin pool ls`.

## Nested Docker needs its data root on a real filesystem

`docker run` inside a sandbox fails with `failed to mount
/tmp/containerd-mountNNNN: ... invalid argument` only when that dockerd's data
root is on the container's own overlay rootfs — overlayfs cannot stack on
overlayfs. A real sandbox avoids it: `sandbox.json` puts `/var/lib/docker` and
`/var/lib/containerd` on the ext4 `data` volume. In an ad-hoc probe container,
mount a real filesystem at the data root rather than concluding nesting is
broken. Sandboxes may have no outbound DNS; `docker save <img> | docker exec -i
<container> docker load` instead of pulling.

## Bats

The suites in `test/bats` drive this same stack and own only what they create.
Inside a box they need the port and the server pointed at the loop, or they
skip — and a skipped suite reports `ok`:

```bash
PORT=8080 DISCOBOX_SERVER=http://127.0.0.1:8080 \
  go tool task test:docker:bats BATS_SUITE=test/bats/<file>.bats
```

Read the output for `# skip`; a run where everything skipped tested nothing.
Some suites fail on `main` for reasons unrelated to any one change — compare
against the base commit before calling a failure new.

## Cleanup

`d admin box purge <id>` destroys a box and waits until its data is gone;
`d rm` / `d admin box delete` only archive. Purge every box you created, by
the ID you recorded — never by filter or name pattern, which would reap boxes
someone else made.
