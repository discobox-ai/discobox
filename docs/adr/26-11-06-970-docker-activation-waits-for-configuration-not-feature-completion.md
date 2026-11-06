# 26-11-06-970 — Docker activation waits for configuration, not feature completion

- **Status**: Proposed
- **Date**: 2026-11-06
- **Issue**: [#724](https://github.com/discobox-ai/discobox/issues/724)
- **Extends**: [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md) §5's readiness-on-use rule with a daemon activation boundary; preserves background provisioning and feature opt-outs.
- **Related**: [0020](0020-nested-docker-trust-is-injected-by-a-runc-wrapper.md)'s daemon proxy and nested runtime trust; [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)'s agent placement before bundle provisioning.

## Context

A fresh proxy-only sandbox can admit an exec whose first `docker pull` starts
Docker without its proxy environment. A later pull succeeds after the Docker
feature finishes and restarts the daemon. The issue reports this at runtime;
its triage reproduces the ordering defect with intercepted system commands.

At `00541f9c` on `main`:

- `server/providers/dockerworker/sandbox-image/Dockerfile` enables
  `docker.socket` in the base image and disables eager daemon startup.
- `sandbox-agent/features/docker/provision.yaml` is async. Its socket and
  personality steps precede daemon JSON, Docker/containerd runtime drop-ins,
  proxy drop-ins and bridge units. It declares no command placeholders.
- `sandbox-agent/provision/runner.go` releases general readiness before async
  steps. `runSteps` collects restarts and synchronously calls `try-restart`
  before the feature can finish.
- `sandbox-agent/dockercache/run.go` passes non-build commands straight to the
  real client, without a feature wait. A client using `/usr/bin/docker` or the
  socket API bypasses the personality altogether.
- `pkg/agentlayer/units/discobox-render-proxy-env.service` orders proxy rendering
  before Docker, but does not install the feature's daemon drop-ins. Rendering
  an environment file does not make an unconfigured daemon read it.

Waiting for the whole Docker feature inside daemon startup would create a
cycle: feature completion waits for a restart that waits for feature completion.
Moving only the feature's socket step cannot close the base-image window.
Pool outbound bootstrap (#679/#683/#721) and the WSL pre-daemon hook (#720)
are separate concerns.

## Decision

### 1. Gate daemon activation on a configuration-only prerequisite

Managed Linux sandboxes with the Docker feature enabled get a systemd oneshot
prerequisite, required and ordered before `docker.service` and
`containerd.service`. The agent layer installs the dependency before the box
boots, independently of the async feature. The base image must not expose an
activation path before that dependency exists. The same protection applies to
provider images with Docker already installed and sockets already enabled.

The prerequisite applies the Docker configuration needed before a daemon can
serve work: merged daemon JSON preserving provider keys, both runtime PATH
drop-ins and wrapper links, Docker's proxy environment drop-in, and bridge
units and dependency drop-ins. It requires successful proxy/trust preparation
when the sandbox declares those inputs, then reloads systemd before releasing
daemon startup. A sandbox declaring no proxy remains supported.

It uses the provisioning implementation and the selected bundle's configuration
sources, with shared step records and a process-safe lock. It does not copy
Docker configuration into a second independent installer. This is an explicit
configuration entry point: it never starts, restarts or waits for Docker,
containerd, the bridge, or the Docker feature's completion. Its dependency
closure must not require the agent's general readiness or a running daemon.

The ordinary feature ensures that this prerequisite has succeeded before its
remaining lifecycle work. It waits for the prerequisite outside the lock. Any
restart runs only after configuration is committed and the lock is released.
The prerequisite cannot hold a lock while waiting on a unit that needs it.
Changes for an already running daemon still use the existing restart behavior;
the gate protects startup, rather than replacing convergence.

### 2. Keep socket activation and expose readiness on use

Docker remains async unless required by a blocking feature. General exec
readiness does not wait for every optional feature. The managed `docker`
personality waits for Docker feature readiness before executing a command,
using existing provisioning diagnostics for pending, failed and off features.
Install that waiting entry point during pre-readiness wiring so an early command
cannot resolve the native CLI before the personality exists. Preserve direct
stdio, signals and exit status after the wait, and the pool builder rewrite.

The daemon prerequisite also protects direct native clients and socket API
consumers. A socket may queue a request while configuration completes, but the
daemon must not process it with partial configuration. Correctness does not
rely on a PATH wrapper, retries or a later restart. A queued request must finish
within the supported client startup deadline; implementation must test that
bound and may delay socket listening until configuration is ready to meet it.

### 3. Configuration success is distinct from feature readiness

The prerequisite succeeds only after all required configuration and systemd
reloads succeed. It fails closed, naming the failing operation and its log;
failed configuration must not start an unconfigured daemon. The command wait
reports the feature failure rather than falling through to native Docker.
Do not use a successful unit skip or an absent status document as proof that an
enabled managed Docker feature is configured.

Success is scoped to the current boot and selected configuration. Persistent
step digests can avoid unchanged writes, but a snapshot or an older bundle's
success cannot release a new boot without validating its configuration.
Concurrent daemon activation and background provisioning share one application
of the same inputs. Interrupted application is retriable and cannot publish
partial success. A changed bundle invalidates the relevant success before its
configuration is applied.

### 4. Preserve existing lifecycle and opt-out semantics

An off or absent Docker feature does not configure or uninstall the image's
Docker, and this gate does not take ownership of it. Resolve the feature plan
from the delivered bootstrap and bundle selection before deciding whether the
gate applies; do not infer opt-out from an agent that has not yet reported.
Retain the existing managed-command off diagnostic.

Install the gate before first activation on fresh containers, VM images and
snapshot boots. For an existing sandbox, install and reload its dependency
before admitting new managed work on the upgraded boot; converge any inherited
running daemon before declaring Docker ready. No destructive database, Docker
state or volume reset is involved. Reapplying configuration must not restart an
unchanged active daemon, stop workloads just to establish a gate, or eagerly
start an idle daemon. Existing configuration changes keep their current restart
semantics.

## Alternatives rejected

- **Make Docker, or all optional features, block exec readiness.** This delays
  unrelated work and still leaves socket activation before agent readiness
  unprotected. Configuration is the prerequisite, not general sandbox readiness.
- **Wait for `discobox-provision wait docker` from daemon startup.** It creates
  the restart/completion cycle described above. A timeout only bounds the
  deadlock; it does not make the dependency correct.
- **Only reorder socket setup or add a Docker placeholder.** Neither protects
  the already-enabled base socket and direct socket clients. The placeholder
  remains useful for diagnostics, alongside the activation gate.
- **Keep the final restart and retry pulls.** The first registry request still
  escapes with incomplete settings. It fails the first-request requirement.
- **Bake all settings into the base image.** Settings depend on delivered
  sandbox configuration and the selected bundle; provider images and snapshots
  must work too. Only the early activation dependency belongs to agent placement.

## Consequences and implementation questions

There are two milestones: configuration safe for daemon startup, and the whole
Docker feature ready for managed commands. Neither changes general readiness.
The implementation must update the affected provisioning, agent-layer and
sandbox design docs with the code; this proposal changes no current-state docs.

Open to implementation: the prerequisite's command and unit names, how the
configuration subset is represented in the provision specification, and its
failure/status plumbing. These are not permission to duplicate configuration
sources or introduce a full-feature startup wait. A reusable phase is justified
only if it retains the explicit side-effect and dependency limits above; a
second daemon consumer is not required for this fix.

Delivery must demonstrate an immediate first pull in a fresh proxy-only sandbox,
after CLI execution is admitted, with no retry and evidence of the daemon's
proxy/trust configuration before its first registry request. Exercise native
socket access during delayed configuration, configuration failure, concurrent
activation, an already socket-enabled image, changed configuration on an
existing box, restart without a completion cycle, snapshot reboot, and off/no
proxy cases. Confirm idle Docker remains socket-activated and unchanged
reapplication leaves running workloads alone. Deterministic ordering tests
complement the real pull; they do not substitute for it.
