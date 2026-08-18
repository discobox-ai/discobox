# 0047 — Local base images resolve through a per-sandbox registry namespace

- **Status**: Accepted
- **Date**: 2026-08-16
- **Extends**: [0044](0044-builds-run-on-a-pool-shared-buildkit.md)

## Context

0044 moved builds off the sandbox's own BuildKit onto a pool-shared builder, on
the stated goal that a sandbox user should not be able to tell. One case tells
immediately:

```
FROM discobox-sandbox-agent:local
ERROR: pull access denied, repository does not exist or may require
authorization: docker.io/library/discobox-sandbox-agent:local
```

A local `docker build` resolves an unqualified base against the daemon's own
image store, because with the `docker` driver BuildKit runs inside dockerd and
that store is right there. The pool builder is a different daemon on a different
host with no such store, so the name normalises to `docker.io/library/...` and
goes to Hub. Nothing about `:local` is special; the image simply exists in one
place and the build runs in another.

This is not an edge case. It is how the repository builds its own images —
`build:harness-images` builds the sandbox base, then builds each harness
`FROM discobox-sandbox-agent:local` — and it is the ordinary shape of a
multi-image project. `task build:images` and the `dockerfile-test-builds` hook
both fail on it today.

Three things are already true and shape the answer:

- **The mediator cannot see a build's base images.** 0044 records this: for a
  dockerfile build the gateway solve's `Definition` is nil, because the frontend
  generates LLB inside the daemon. The mediator sets policy but cannot enumerate
  sources.
- **BuildKit resolves named contexts before it resolves images.**
  `dockerfile2llb` checks `context:<name>` frontend opts against the stage's
  base name *as written* and only falls through to a registry pull if there is
  no match. buildx surfaces this as `--build-context`.
- **The pool registry already holds everything the pool built.** Layers are
  content-addressed and cross-repository mounting is a registry primitive:
  re-publishing a pool-built 7.1GB image under a second name measured **0.377s**,
  every layer reported `Mounted from discobox-build/...`.

## Decision

A build's local base images are published into a **per-sandbox namespace in the
pool registry** and redirected there with `--build-context`, by the shim.

1. **The shim decides, not the mediator.** It is the only component holding both
   the Dockerfile and the local image list. It scans the build's `FROM`
   instructions, and for each reference that names no registry host and exists
   in the local daemon, publishes it and adds
   `--build-context <ref-as-written>=docker-image://<namespace ref>`. A `FROM`
   that resolves nowhere locally is left alone and pulled as before.

2. **The namespace is keyed on a secret, not on the sandbox ID.** pool-agent
   mints an unguessable token per sandbox and stages it with the rest of that
   sandbox's proxy material; the repository path is `<token>/<image>`.

3. **Publishing is a push to the pool registry**, not a stream over the session.
   For anything the pool built, its layers are already there and the push is
   metadata.

## Alternatives rejected

**Map `docker.io/library` to a per-sandbox namespace in buildkitd's registry
configuration.** The obvious reading of "make local names resolve here". It
cannot be per-sandbox: buildkitd's registry configuration is per-daemon, and one
daemon serves every sandbox in the pool. It would also capture every unqualified
public image, making the pool registry a mandatory mirror for Hub.

**Name the namespace after the sandbox ID.** Reads better and needs no secret.
Rejected because the pool registry has no authentication — it is plaintext and
reachable by every sandbox in the pool — so a path is a capability. Today's
build output is protected only by `discobox-build/<random hex>` being
unguessable; a namespace a peer can *derive* from a sandbox ID it already knows
is strictly weaker than what it replaces. A secret keeps the property the random
ref has. This is a stopgap for the absent authentication, not a substitute for
it: when the registry authenticates, the namespace should become the sandbox ID
and the secret should go.

**Send the base image over the session as an `oci-layout` build context.** Works
without a registry, and is what `--load` did in reverse. Rejected for the reason
0044 moved image transport to the registry at all: it reships bytes the far end
already has on every build.

**Detect the failure and retry.** Build, and on "pull access denied" for a name
that exists locally, publish it and build again. No Dockerfile parsing. Rejected
because it makes every genuinely-missing image pay a full failed build first,
and it reads the daemon's error text to decide.

**Keep an index of tags the shim has applied and redirect those.** Cheaper than
parsing — the shim knows what it built. Rejected because it only covers images
this shim produced: an image that arrived by `docker load`, `docker pull` and
`docker tag`, or a build that predates the index still fails, and the failure
depends on invisible local state rather than on the Dockerfile.

## Consequences

- The shim gains a Dockerfile scanner. It must track stage names so `FROM
  builder` is not treated as an image, expand `ARG` in a base name, and honour
  line continuations — `FROM ${SANDBOX_AGENT_IMAGE}` is exactly the shape this
  repository's own harness images use. A missed `FROM` degrades to today's
  behaviour rather than to a wrong build.
- An unqualified base that exists *both* locally and on Hub now resolves to the
  local copy. That is what a local `docker build` does, so it is the faithful
  behaviour, but it means a stale local `node:22` shadows the registry's for
  pool builds as it already does for local ones.
- Publishing a base the pool did not build (one pulled from Hub) is a real
  upload the first time. It happens once per sandbox namespace per image.
- The pool registry accumulates a repository per sandbox that has built, and
  nothing removes them. Content lifetime is deferred to its own decision,
  alongside the equivalent question for per-sandbox proxy material.
- pool-agent gains one more per-sandbox artefact to stage. It is written
  world-readable inside the sandbox: the boundary it defends is other sandboxes,
  and the sandbox's own user is the tenant it belongs to.
