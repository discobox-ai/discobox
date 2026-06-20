# Sessions API Design

`api` owns the Unix-socket OpenAPI contracts for the sessions daemon and
per-session supervisors. The daemon and CLI client use generated transport
scaffolding from `api/gen`; the daemon and supervisor use generated transport
scaffolding from `api/supervisorgen`. Stable code-facing DTOs remain in the
root `sessions` package.

## Responsibilities

- Maintain the canonical sessions daemon OpenAPI contract at
  `openapi/sessions.yaml`.
- Maintain the canonical per-session supervisor OpenAPI contract at
  `openapi/supervisor.yaml`.
- Generate contract-first client/server scaffolding and schema DTOs into `gen`
  and `supervisorgen` with ogen.
- Keep process supervision, persistence, terminal control, and stream proxying
  out of this package.

`POST /sessions/{sessionId}/attach` and supervisor `POST /attach` are
represented in their contracts as upgrade operations, but the daemon and
supervisor intercept them before generated routing so they can preserve
bidirectional framed stream behavior.
