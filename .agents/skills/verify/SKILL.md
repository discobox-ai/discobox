---
name: verify
description: How to drive a running `task dev` instance to verify sandbox/pool-agent/sandbox-agent changes end to end.
---

# Verifying against `task dev`

`task dev` runs two long-lived pieces: `dev:server` (discobox-server under
watchnbuild hot reload) and `dev:docker-image-watch` (rebuilds
pool/sandbox/harness images on source changes, writes the resulting digest to
`.env`).

## Finding the real dev server

Multiple `discobox-server` processes may be running (leftover from other
sessions/background jobs). Find the one actually owned by `task dev`'s
watchnbuild process, not just any listener:

```bash
ps aux | grep -E "watchnbuild|task dev"   # find the `go tool watchnbuild` PID
pstree -p <wnb-pid>                       # its child discobox-server is the real one
ss -ltnp | grep discobox-server           # match PID -> port
```

The CLI binary is at `./build/disco`. Drive it against that port:

```bash
./build/disco --server "http://127.0.0.1:<port>" --project default box sandbox create \
  --name verify-x --harness claude-code --wait --wait-timeout 150s
docker ps --format '{{.ID}}\t{{.Image}}\t{{.Names}}' | grep <sandbox-id>
```

`--image <tag>` on `box sandbox create` does **not** reliably override the
image when a default harness config auto-applies — the harness config's own
registered image wins. Don't rely on it; rebuild the harness's `:local` tag
instead (see below) and create a sandbox with the matching `--harness`.

## Forcing a rebuild without waiting on the watcher

`dev:docker-image-watch` polls file hashes on a 1s ticker and *should* rebuild
automatically, but in practice it can sit for minutes without visibly
rebuilding (its stdout goes to the `task dev` terminal's pty, which isn't
capturable from a separate shell/session — no log file exists to tail).
Don't loop waiting on `.env`'s digest to change. Instead, replicate its build
command directly for a fast, verifiable rebuild:

```bash
# sandbox-agent base image:
docker build -f sandbox-agent/Dockerfile -t discobox-sandbox-agent:local .

# a harness image (layers on the sandbox-agent base you just built):
docker build -f harness/claude-code/Dockerfile \
  --build-arg SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local \
  --build-arg HARNESS_METADATA= \
  -t discobox-harness-claude-code:local harness/claude-code
```
Both build fast from cache when only a Go source file or image/systemd file
changed (only the affected layers rebuild). Then create a sandbox with
`--harness claude-code` — pool-agent resolves the harness's already-registered
image, so a fresh `box sandbox create` picks up the freshly built `:local`
tag's content automatically as long as the harness config was registered
against that base (it usually is, since `:local` is a stable tag name reused
across builds).

## Exec'ing into a running sandbox

```bash
docker exec <container-id> systemctl is-active <unit>
docker exec <container-id> journalctl -u <unit> --no-pager -n 30
docker exec <container-id> docker version   # triggers docker.socket's lazy activation chain
```

To test nested-Docker behavior without needing real network access (sandboxes
may not have outbound internet in this dev environment — DNS to Docker Hub
can fail with "server misbehaving"), load a locally-cached host image into the
sandbox's nested dockerd instead of pulling:

```bash
docker save alpine:latest | docker exec -i <container-id> docker load
docker exec <container-id> docker run --rm alpine ...
```

## Nested Docker needs its data root on a real filesystem

`docker run`/`docker build` **inside a sandbox** fails at container creation with:

```
failed to mount /tmp/containerd-mountNNNN: mount source: "overlay", ...
err: invalid argument
```

*only* when that dockerd's data root sits on the container's own overlay
rootfs. overlayfs cannot use another overlayfs as its `upperdir`, so
overlay-on-overlay is rejected by the kernel.

This is **not** a limitation of the host or of nesting depth. A real sandbox
already avoids it: `sandbox.json` declares `/var/lib/docker` and
`/var/lib/containerd` on the `data` volume, so they are ext4 rather than
overlay, and nested `docker run`/`docker build` work normally (verified: a
container from the sandbox image with `-v <ext4 dir>:/var/lib/docker` pulls and
runs images fine, while the same image with `--data-root` on its overlay rootfs
fails as above).

If you hit this error in an ad-hoc probe container, mount a real filesystem at
the data root rather than concluding the environment cannot nest.

## Cleanup

```bash
./build/disco --server "http://127.0.0.1:<port>" --project default box sandbox delete <id> [<id>...]
docker rm -f <ad-hoc-test-containers>
```
