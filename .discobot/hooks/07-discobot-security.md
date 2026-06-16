---
name: Security reviewer
type: file
engine: ai
phase: review
pattern: "{cmd,internal,gormdb,orchestration,.discobot}/**"
description: Review security-sensitive backend, orchestration, database, and workspace automation changes
---

Review changed files in this repository for security issues. This project is a Go
HTTP service with database access, durable orchestration helpers, sandbox and
worker orchestration, jobs, project events, and Discobot workspace automation.
Apply the closest applicable `AGENTS.md`, `DESIGN.md`, and `REVIEW.md` guidance
for the changed files before evaluating security impact.

Focus only on real security risks introduced by the current changes. Do not
report style, architecture, or preference issues unless they create a security
problem.

Check for:

- secrets, tokens, credentials, private keys, or sensitive values exposed in
  logs, fixtures, generated output, API responses, events, job payloads, hook
  output, service definitions, or test data
- API handlers, service methods, job executors, or reconciliation paths that
  trust client-controlled headers, path params, query params, forms, JSON
  payloads, project IDs, session IDs, thread IDs, generations, or resource IDs
  without server-side validation
- missing authentication, authorization, ownership, project, session, thread,
  sandbox, workspace, container, resource, or generation checks before reading
  or mutating state
- unsafe file operations, path traversal, symlink escapes, invalid parent/child
  moves, unsafe archive extraction, or destructive actions without appropriate
  validation
- SSRF, open redirect, proxy abuse, or unsafe outbound requests from user- or
  workspace-controlled URLs
- command injection, shell injection, argument injection, unsafe environment
  propagation, or untrusted input passed to subprocesses, hooks, services,
  sandboxes, workers, or containers
- container or sandbox escape risks, unsafe mounts, host path exposure, Docker
  socket access, credential leakage across sessions, or weakened isolation
- broad CORS, cache, cookie, CSRF, or static-file changes that expose private
  state or allow credentialed cross-origin access
- logs or error messages that disclose secrets, sensitive local paths, request
  bodies containing credentials, OAuth data, API keys, or internal tokens
- cryptography, token, OAuth, credential storage, or encryption changes that use
  weak randomness, weak validation, unsafe persistence, or overly broad scopes
- database access that bypasses project/resource scoping, permits SQL injection,
  or fails to keep intent changes, events, and durable reconcile jobs atomic

When evaluating a finding, identify the exploit path and the security impact. If
there is no plausible exploit path, do not report it.
