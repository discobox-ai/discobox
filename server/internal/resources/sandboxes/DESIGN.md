# Sandboxes Design

`internal/resources/sandboxes` owns sandbox API behavior, sandbox lifecycle
reconciliation, sandbox provider catalog access, and sandbox runtime trust
integration.

## Boundaries

```mermaid
flowchart LR
    api[internal/handlers] --> service[Service]
    service --> store[internal/store]
    service --> jobs[internal/resources/jobs.Manager]
    dispatcher[orchestration.Dispatcher] --> executor[SandboxReconcileExecutor]
    executor --> store
    executor --> providers[sandbox.ProviderManager]
    executor --> auth[internal/auth/sandbox]
```

- `Service` exposes sandbox API use cases and may call store directly for simple
  reads or non-orchestrated updates.
- Lifecycle intent must go through durable job submission and generation guards.
- `SandboxReconcileExecutor` owns payload decode, generation assertions, and
  sandbox lifecycle reconciliation.
- Provider runtime operations belong in reconciliation, not in handlers or raw
  stores.

## Worker-observed runtime loss

Worker-backed providers report an out-of-band container removal through the
authenticated worker control-plane route. The sandbox service verifies the
worker assignment, atomically records stopped intent (generation bump plus stop
operation), and marks the sandbox dirty. Stop reconciliation treats a missing
runtime as drift: it recreates the sandbox from persisted intent and state, then
stops the retained runtime so observed and desired state converge on `stopped`.
Duplicate, stale-worker, and already-deleting reports are no-ops.

## Image-backed harnesses

A sandbox selects a persisted image-backed `HarnessConfig`. The selected image
overrides a caller-supplied generic sandbox image. Providers receive only the
harness identity and project-configured non-secret file overlay; run, relaunch,
config, and static file metadata stay inside the image.

`harnessMode` is persisted sandbox intent. Normal/omitted `run` mode applies the
harness secret requirement gate before scheduling. `config` mode skips that
gate so the image-owned interactive command can collect required credentials.
