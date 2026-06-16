# Worker Agent Design

This module owns the in-guest worker agent process and local worker-agent image
watcher.

The worker agent is not the sandbox REST API implementation. It reads root
`workerbootstrap` metadata, authenticates to the control plane, reports worker
health/capacity, and runs the local worker runtime plumbing needed by provider
backends.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `cmd/discobox-worker-agent` | Worker agent binary entrypoint. |
| `cmd/discobox-worker-agent-watch` | Local development watcher that rebuilds the worker-agent image and updates the repo `.env`. |
| `workeragent` | Worker-agent runtime implementation and HTTP/systemd helpers. |

## Boundary Rules

- Depend on root contracts such as `workerbootstrap`; do not define cross-module
  boot metadata locally.
- Build the worker-agent image from the repository root with
  `docker build -f worker-agent/Dockerfile ... .` so the Dockerfile can copy root
  contracts without vendoring them.
- Do not import server internals or provider implementation packages.
- Keep sandbox REST API implementation code in the `sandbox-agent` module.
