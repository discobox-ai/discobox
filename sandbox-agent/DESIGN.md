# Sandbox Agent Design

This module owns the sandbox runtime environment and future in-sandbox agent REST
API implementation.

Today it contains the sandbox-agent image context. The Go implementation has not
been added yet; when it is, it should implement the root
`api/openapi/sandbox.yaml` in-sandbox agent API seed and depend on root generated
API/client types rather than server or provider internals.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `Dockerfile` | `codex-universal`-based systemd sandbox runtime image with Docker, desktop, and Nix tooling. |

## Boundary Rules

- Implement the future in-sandbox agent API from `api/openapi/sandbox.yaml`.
- Depend on root contracts and generated API types only for cross-module data.
- Do not import server internals or provider implementation packages.
- Keep worker registration and control-plane bootstrapping in the `worker-agent`
  module unless a shared contract belongs in the root module.
