# Sandbox Agent Design

This module owns the sandbox runtime environment and future sandbox REST API
agent implementation.

Today it contains the sandbox-agent image context. The Go implementation has not
been added yet; when it is, it should implement the root `api/openapi/sandbox.yaml`
contract and depend on root generated API/client types rather than server or
provider internals.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `Dockerfile` | Debian/systemd-based sandbox runtime image with developer tools. |

## Boundary Rules

- Implement the sandbox REST API from the root OpenAPI contract.
- Depend on root contracts and generated API types only for cross-module data.
- Do not import server internals or provider implementation packages.
- Keep worker registration and control-plane bootstrapping in the `worker-agent`
  module unless a shared contract belongs in the root module.
